// Package genmodel 存放由 `goxdb gen` 生成代码的模型，用于对比
// 「零反射扫描」与「反射扫描」在真实数据库上的差距。
//
// 生成方式（在仓库根目录执行）：
//
//	go run ./cmd/goxdb -dir ./benchmark/genmodel
package genmodel

import "time"

// User 与 benchmark.BenchUser 指向同一张表，唯一的区别是它由生成器产出扫描器。
type User struct {
	ID        int64     `xdb:"id,pk,auto"`
	Name      string    `xdb:"name"`
	Email     *string   `xdb:"email"`
	Age       int       `xdb:"age"`
	CreatedAt time.Time `xdb:"created_at"`
}

func (User) TableName() string { return "xdb_bench_users" }
