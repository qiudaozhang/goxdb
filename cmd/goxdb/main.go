// Command goxdb 为 goxdb 模型生成类型化的表与列描述符。
//
// 用法（在包含模型的包目录下）：
//
//	//go:generate go run github.com/qiudaozhang/goxdb/cmd/goxdb
//
// 或显式指定参数：
//
//	goxdb -dir ./model -pkg model -out xdb_gen.go
//
// 生成的文件包含：
//
//	var User = struct{ xdb.Table; ID xdb.NumberCol[int64]; ... }{...}
//
// 以及 User 的关联描述符。列的类型由 Go 字段类型决定（NumberCol / StringCol /
// TimeCol / Col[T]），因此 `U.Age.Eq("abc")` 会在编译期失败。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/qiudaozhang/goxdb/internal/codegen"
)

func main() {
	dir := flag.String("dir", ".", "包含模型的目录")
	pkg := flag.String("pkg", "", "包名（默认取目录中的包名）")
	out := flag.String("out", "xdb_gen.go", "输出文件名（相对 -dir）")
	flag.Parse()

	if err := codegen.Run(codegen.Config{Dir: *dir, Pkg: *pkg, Out: *out}); err != nil {
		fmt.Fprintln(os.Stderr, "goxdb:", err)
		os.Exit(1)
	}
}
