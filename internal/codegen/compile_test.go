package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGeneratedCodeCompiles 端到端验证：解析模型 → 生成代码 → 真的编译通过。
// 这能发现「生成的源码语法正确但类型不匹配」这类只在编译期暴露的问题。
func TestGeneratedCodeCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go build in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	writeModel(t, dir, sampleModel)
	// 生成代码放在同一个包里，并提供 go.mod 指向本地模块。
	if err := Run(Config{Dir: dir, Out: "xdb_gen.go"}); err != nil {
		t.Fatal(err)
	}
	gomod := "module gentest\n\ngo 1.25.0\n\nrequire github.com/qiudaozhang/goxdb v0.0.0\n\nreplace github.com/qiudaozhang/goxdb => " + root + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	// 引用生成的描述符，确保它确实被类型检查（否则未使用的变量也会报错）。
	use := `package model

func Use() {
	_ = U.ID
	_ = U.Name
	_ = U.Age
	_ = U.CreatedAt
	_ = O.State
}
`
	if err := os.WriteFile(filepath.Join(dir, "use.go"), []byte(use), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated code does not compile: %v\n%s", err, out)
	}
}

// TestTypeSafety 验证生成描述符带来的编译期类型安全：
// U.Age 是 NumberCol[int]，传 string 应当无法编译。
func TestTypeSafetyRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go build in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeModel(t, dir, sampleModel)
	if err := Run(Config{Dir: dir, Out: "xdb_gen.go"}); err != nil {
		t.Fatal(err)
	}
	gomod := "module gentest\n\ngo 1.25.0\n\nrequire github.com/qiudaozhang/goxdb v0.0.0\n\nreplace github.com/qiudaozhang/goxdb => " + root + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := `package model

func Bad() {
	_ = U.Age.Eq("not a number")
}
`
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected compile error for U.Age.Eq(string), but build succeeded:\n%s", out)
	}
	if !containsAny(string(out), "cannot use", "type mismatch", "cannot convert") {
		t.Fatalf("unexpected compile error:\n%s", out)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
