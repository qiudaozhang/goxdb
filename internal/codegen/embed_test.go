package codegen

import (
	"strings"
	"testing"
)

const embedModel = `package model

import "time"

type Base struct {
	ID        int64     ` + "`xdb:\"id,pk,auto\"`" + `
	CreatedAt time.Time ` + "`xdb:\"created_at\"`" + `
	Note      string    ` + "`xdb:\"base_note\"`" + `
	Name      string    ` + "`xdb:\"base_name\"`" + `
}

type Audit struct {
	CreatedAt time.Time ` + "`xdb:\"created_at\"`" + `
	By        string    ` + "`xdb:\"by\"`" + `
}

type Tagged struct {
	By string ` + "`xdb:\"by\"`" + `
}

type User struct {
	Base
	*Audit
	Tagged
	Name string ` + "`xdb:\"name\"`" + `
}

type hidden struct {
	Secret string ` + "`xdb:\"secret\"`" + `
}

type WithHidden struct {
	*hidden
	ID int64 ` + "`xdb:\"id,pk\"`" + `
}
`

// 嵌入字段按 Go 字段提升命名，冲突消解与运行时 schema 一致。
func TestEmbeddedFields(t *testing.T) {
	dir := t.TempDir()
	writeModel(t, dir, embedModel)
	models, _, err := ParseDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ModelInfo{}
	for _, m := range models {
		byName[m.Name] = m
	}

	u := byName["User"]
	var got []string
	for _, c := range u.Columns {
		got = append(got, c.GoName+"="+c.Sel+"->"+c.Column)
	}
	// created_at 在 Base 与 Audit 同层出现 → 歧义，丢弃（schema 同样丢弃）；
	// by 在 Audit 与 Tagged 同层出现 → 同样丢弃；
	// Base.Name 的列名是 base_name，与 User.Name 不冲突，但描述符名冲突 → 改名 BaseName。
	want := []string{
		"ID=Base.ID->id",
		"Note=Base.Note->base_note",
		"BaseName=Base.Name->base_name",
		"Name=Name->name",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("columns\n got: %v\nwant: %v", got, want)
	}
	if !u.HasPK {
		t.Error("pk from embedded Base should count")
	}

	// 未导出的嵌入指针运行时无法分配，生成器跳过它。
	w := byName["WithHidden"]
	if len(w.Columns) != 1 || w.Columns[0].Column != "id" {
		t.Fatalf("unexported embedded pointer should be skipped: %+v", w.Columns)
	}

	src, err := Render("model", models)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"U.ID = ", "U.BaseName = ", "s.dest[0] = &m.Base.ID"} {
		if !strings.Contains(string(src), s) {
			t.Errorf("generated code missing %q", s)
		}
	}
}
