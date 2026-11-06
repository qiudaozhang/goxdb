package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestScannerMatchesRuntime 端到端验证生成器与运行时 schema 的列集一致：
// 对每个模型，Query[T] 选出的列必须被生成的 NewRowScanner 接受。
// 否则零反射扫描会静默回退到反射，生成的描述符也会缺列。
//
// 模型覆盖：无标签导出字段、xdb:"-"、嵌入值、导出嵌入指针、同层冲突、
// 显式列名胜出、未导出嵌入指针。
func TestScannerMatchesRuntime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeModel(t, dir, sampleModel+parityModels)
	if err := Run(Config{Dir: dir, Out: "xdb_gen.go"}); err != nil {
		t.Fatal(err)
	}
	gomod := "module gentest\n\ngo 1.25.0\n\nrequire github.com/qiudaozhang/goxdb v0.0.0\n\nreplace github.com/qiudaozhang/goxdb => " + root + "\n"
	files := map[string]string{"go.mod": gomod, "parity_test.go": parityTest}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated scanner disagrees with runtime: %v\n%s", err, out)
	}
}

const parityModels = `
type PBase struct {
	ID        int64     ` + "`xdb:\"id,pk,auto\"`" + `
	CreatedAt time.Time ` + "`xdb:\"created_at\"`" + `
}

type PAudit struct {
	CreatedAt time.Time ` + "`xdb:\"created_at\"`" + `
	Editor    string
}

type PMeta struct {
	Editor string ` + "`xdb:\"editor\"`" + `
}

type PArticle struct {
	PBase
	*PAudit
	PMeta
	Title string ` + "`xdb:\"title\"`" + `
	Body  string
	Skip  string ` + "`xdb:\"-\"`" + `
}

type pHidden struct {
	Secret string ` + "`xdb:\"secret\"`" + `
}

type PWithHidden struct {
	*pHidden
	ID int64 ` + "`xdb:\"id,pk\"`" + `
}
`

const parityTest = `package model

import (
	"strings"
	"testing"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/dialect/postgres"
)

// cols 从 Query[T] 渲染出的 SELECT 列表中取出列名（运行时 schema 的列集与顺序）。
func cols[T any](t *testing.T) []string {
	t.Helper()
	q, _, err := xdb.Query[T](nil).Unscoped().SQL(postgres.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	list := strings.TrimPrefix(q[:strings.Index(q, " FROM ")], "SELECT ")
	var out []string
	for _, item := range strings.Split(list, ", ") {
		name := item[strings.LastIndex(item, ".")+1:]
		out = append(out, strings.Trim(name, "\""))
	}
	return out
}

func check[T any, P interface {
	*T
	xdb.RowScanner
}](t *testing.T) {
	t.Helper()
	c := cols[T](t)
	sc, err := P(new(T)).NewRowScanner(c)
	if err != nil || sc == nil {
		t.Errorf("%T: generated scanner rejects runtime columns %v", *new(T), c)
	}
}

func TestParity(t *testing.T) {
	check[User](t)
	check[Order](t)
	check[PArticle](t)
	check[PWithHidden](t)
}
`
