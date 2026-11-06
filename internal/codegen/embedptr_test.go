package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const embedPtrModel = `package model

import "time"

type Base struct {
	ID        int64     ` + "`xdb:\"id,pk,auto\"`" + `
	CreatedAt time.Time ` + "`xdb:\"created_at\"`" + `
}

type User struct {
	*Base
	Name string ` + "`xdb:\"name\"`" + `
}
`

// 嵌入指针的模型必须能编译：_ &m.Base.ID 需要 Base 非 nil。
func TestEmbeddedPointerCompiles(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeModel(t, dir, embedPtrModel)
	if err := Run(Config{Dir: dir, Out: "xdb_gen.go"}); err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile(filepath.Join(dir, "xdb_gen.go"))
	t.Logf("%s", src)
	gomod := "module gentest\n\ngo 1.25.0\n\nrequire github.com/qiudaozhang/goxdb v0.0.0\n\nreplace github.com/qiudaozhang/goxdb => " + root + "\n"
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644)
	os.WriteFile(filepath.Join(dir, "use.go"), []byte("package model\n\nfunc Use() { _ = U.ID; _ = U.CreatedAt; _ = U.Name }\n"), 0o644)
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("embedded pointer does not compile: %v\n%s", err, out)
	}
}
