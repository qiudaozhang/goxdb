package xdb_test

import (
	"context"
	"errors"
	"fmt"
	"log"

	xdb "github.com/qiudaozhang/goxdb"
	_ "github.com/qiudaozhang/goxdb/dialect/mysql"
	"github.com/qiudaozhang/goxdb/dialect/postgres"
)

// 本文件中的示例展示在 pkg.go.dev 上。带 Output 的示例只渲染 SQL、不连接数据库，
// 由 go test 校验输出；其余示例只保证能编译。

// Member 是示例模型（表名按类型名推导为 members）。可空列用指针或 sql.Null[T]。
type Member struct {
	ID    int64   `xdb:"id,pk,auto"`
	Name  string  `xdb:"name"`
	Email *string `xdb:"email"`
	Age   int     `xdb:"age"`
}

// M 是 Member 的类型化描述符。实际项目中由 goxdb 命令生成（go run github.com/qiudaozhang/goxdb/cmd/goxdb -dir ./model），
// 这里手写以便示例自包含。
var M = struct {
	xdb.Table
	ID    xdb.NumberCol[int64]
	Name  xdb.StringCol
	Email xdb.StringCol
	Age   xdb.NumberCol[int]
}{}

func init() {
	M.Table = xdb.TableOf[Member]()
	M.ID = xdb.CNum[int64](M.Table, "id")
	M.Name = xdb.CStr(M.Table, "name")
	M.Email = xdb.CStr(M.Table, "email")
	M.Age = xdb.CNum[int](M.Table, "age")
}

// 类型化查询：列类型在编译期检查，M.Age.Eq("abc") 无法编译。
// 同一个构建器按方言渲染出不同的占位符与引号。
func Example() {
	q := xdb.Query[Member](nil).
		Where(M.Age.Ge(18), M.Name.HasPrefix("a")).
		OrderBy(M.ID.Desc()).
		Limit(20)

	sql, args, err := q.SQL(postgres.Dialect)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(sql)
	fmt.Println(args)
	// Output:
	// SELECT "members"."id", "members"."name", "members"."email", "members"."age" FROM "members" WHERE ("members"."age" >= $1 AND "members"."name" LIKE $2) ORDER BY "members"."id" DESC LIMIT 20
	// [18 a%]
}

// 动态条件：xdb.If 为 false 时该条件整体省略，不必手工拼接 WHERE。
func ExampleIf() {
	search := func(name string, minAge int) string {
		sql, _, _ := xdb.Query[Member](nil).
			Where(
				xdb.If(name != "", M.Name.Eq(name)),
				xdb.If(minAge > 0, M.Age.Ge(minAge)),
			).
			SQL(postgres.Dialect)
		return sql
	}
	fmt.Println(search("alice", 0))
	fmt.Println(search("", 30))
	// Output:
	// SELECT "members"."id", "members"."name", "members"."email", "members"."age" FROM "members" WHERE "members"."name" = $1
	// SELECT "members"."id", "members"."name", "members"."email", "members"."age" FROM "members" WHERE "members"."age" >= $1
}

// 打开数据库并用 Raw SQL 查询。占位符统一写 ?，按方言改写；切片参数自动展开为 IN 列表。
func ExampleOpen() {
	ctx := context.Background()
	db, err := xdb.Open("mysql", "root:secret@tcp(127.0.0.1:3306)/app?parseTime=true",
		xdb.WithPool(xdb.Pool{MaxOpen: 20}))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	users, err := xdb.All[Member](ctx, xdb.Raw(db, "SELECT * FROM members WHERE id IN (?)", []int64{1, 2, 3}))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(users))
}

// 托管事务：闭包返回 nil 提交，返回错误或 panic 回滚。
// TxRetry 在死锁 / 序列化失败时整体重试闭包，因此闭包内不要有不可重复的外部副作用。
func ExampleDB_Tx() {
	ctx := context.Background()
	db, err := xdb.Open("mysql", "root:secret@tcp(127.0.0.1:3306)/app?parseTime=true")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	err = db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
		u := &Member{Name: "alice", Age: 20}
		if _, err := xdb.Insert(tx, u).Exec(ctx); err != nil {
			return err
		}
		// u.ID 已回填自增主键
		_, err := xdb.Update[Member](tx).Set(M.Age.Incr(1)).Where(M.ID.Eq(u.ID)).Exec(ctx)
		return err
	}, xdb.TxRetry(3, nil))
	if err != nil {
		log.Fatal(err)
	}
}

// 错误归一化：不同驱动的错误码统一为哨兵错误，用 errors.Is 判断。
func ExampleErrUniqueViolation() {
	ctx := context.Background()
	db, err := xdb.Open("mysql", "root:secret@tcp(127.0.0.1:3306)/app?parseTime=true")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	_, err = xdb.Insert(db, &Member{Name: "alice"}).Exec(ctx)
	switch {
	case errors.Is(err, xdb.ErrUniqueViolation):
		fmt.Println("name already taken")
	case err != nil:
		log.Fatal(err)
	}

	_, err = xdb.One[Member](ctx, xdb.Raw(db, "SELECT * FROM members WHERE id = ?", 42))
	if errors.Is(err, xdb.ErrNotFound) {
		fmt.Println("no such user")
	}
}

// upsert：冲突时按 VALUES 中的新值更新指定列；MySQL 与 PostgreSQL 语法由方言翻译。
func ExampleConflictQ_DoUpdateSet() {
	ctx := context.Background()
	db, err := xdb.Open("mysql", "root:secret@tcp(127.0.0.1:3306)/app?parseTime=true")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	_, err = xdb.Insert(db, &Member{Name: "alice", Age: 20}).
		OnConflict(M.Name).
		DoUpdateSet(M.Age.ToExpr(xdb.Excluded(M.Age.Col))).
		Exec(ctx)
	if err != nil {
		log.Fatal(err)
	}
}
