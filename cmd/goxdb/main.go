// Command goxdb 提供两种代码生成方向：
//
// 1. model-first（默认）：读 Go 源码中的模型，生成类型化的表与列描述符。
//
//	//go:generate go run github.com/qiudaozhang/goxdb/cmd/goxdb
//
// 2. DB-first（-db 模式）：读数据库 schema，反向生成 Go 模型结构体。
//
//	goxdb -db postgres -dsn "$DSN" -dir ./model -reverse-out models.go
//
// 运行 goxdb -h 查看全部参数。
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiudaozhang/goxdb/dialect"
	"github.com/qiudaozhang/goxdb/internal/codegen"
	"github.com/qiudaozhang/goxdb/internal/reverse"

	mysqld "github.com/qiudaozhang/goxdb/dialect/mysql"
	pgd "github.com/qiudaozhang/goxdb/dialect/postgres"
)

func main() {
	var (
		dir = flag.String("dir", ".", "工作目录（model-first 的输入 / DB-first 的输出目录）")
		pkg = flag.String("pkg", "", "model-first 生成的包名（默认沿用目录中的包名）")
		out = flag.String("out", "xdb_gen.go", "model-first 的输出文件名（相对 -dir）")

		dbName = flag.String("db", "", "DB-first 模式：方言名（mysql / postgres）")
		dsn    = flag.String("dsn", "", "DB-first 模式：连接串")
		tables = flag.String("tables", "", "DB-first 模式：只生成这些表（逗号分隔，默认全部）")
		revOut = flag.String("reverse-out", "models.go", "DB-first 模式的输出文件（相对 -dir）")
		revPkg = flag.String("reverse-pkg", "model", "DB-first 模式生成的包名")
		prefix = flag.String("prefix", "", "DB-first 模式：从表名中去掉的前缀")
		suffix = flag.String("suffix", "", "DB-first 模式：从表名中去掉的后缀")
	)
	flag.Parse()
	if flag.NArg() > 0 {
		// goxdb 只接受标志；多余的位置参数（如误写的子命令 "gen"）会让
		// flag 停止解析，后面的 -dir 等全部被忽略，生成到错误目录。
		fmt.Fprintf(os.Stderr, "goxdb: unexpected argument %q (all options are flags; see -h)\n", flag.Arg(0))
		os.Exit(2)
	}

	err := run(params{
		dbName: *dbName, dsn: *dsn, dir: *dir, out: *out, pkg: *pkg,
		tables: *tables, revOut: *revOut, revPkg: *revPkg,
		prefix: *prefix, suffix: *suffix,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "goxdb:", err)
		os.Exit(1)
	}
}

type params struct {
	dbName, dsn, dir, out, pkg, tables, revOut, revPkg, prefix, suffix string
}

func run(p params) error {
	if p.dbName == "" {
		return codegen.Run(codegen.Config{Dir: p.dir, Pkg: p.pkg, Out: p.out})
	}
	return runReverse(p)
}

func runReverse(p params) error {
	if p.dsn == "" {
		return fmt.Errorf("-db requires -dsn")
	}
	d, ok := dialect.Lookup(p.dbName)
	if !ok {
		return fmt.Errorf("unknown dialect %q (registered: %v)", p.dbName, dialect.Names())
	}
	in, ok := introspectors[d.Name()]
	if !ok {
		return fmt.Errorf("dialect %q does not support DB-first generation", p.dbName)
	}

	sqlDB, err := sql.Open(d.DriverName(), p.dsn)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	cfg := reverse.Config{
		Tables: splitList(p.tables),
		Pkg:    p.revPkg,
		Prefix: p.prefix,
		Suffix: p.suffix,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	src, err := reverse.Generate(ctx, sqlDB, in, cfg)
	if err != nil {
		return err
	}
	path := filepath.Join(p.dir, p.revOut)
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, src, 0o644)
}

// introspectors 把方言名映射到各自的 Introspector 实现。
var introspectors = map[string]dialect.Introspector{
	"mysql":    mysqld.Introspect,
	"postgres": pgd.Introspect,
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
