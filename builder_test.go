package xdb_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/dialect"
	"github.com/qiudaozhang/goxdb/dialect/mysql"
	"github.com/qiudaozhang/goxdb/dialect/postgres"
	"github.com/qiudaozhang/goxdb/internal/fakedb"
)

// ---------- 模型与（模拟代码生成的）表描述符 ----------

type Customer struct {
	ID        int64     `xdb:"id,pk,auto"`
	Name      string    `xdb:"name"`
	Email     *string   `xdb:"email"`
	Age       int       `xdb:"age"`
	CreatedAt time.Time `xdb:"created_at"`
}

type Order struct {
	ID         int64  `xdb:"id,pk,auto"`
	CustomerID int64  `xdb:"customer_id"`
	Amount     int64  `xdb:"amount"`
	State      string `xdb:"state"`
}

type customerTable struct {
	xdb.Table
	ID        xdb.NumberCol[int64]
	Name      xdb.StringCol
	Email     xdb.StringCol
	Age       xdb.NumberCol[int]
	CreatedAt xdb.TimeCol
}

func newCustomerTable(t xdb.Table) customerTable {
	return customerTable{
		Table:     t,
		ID:        xdb.CNum[int64](t, "id"),
		Name:      xdb.CStr(t, "name"),
		Email:     xdb.CStr(t, "email"),
		Age:       xdb.CNum[int](t, "age"),
		CreatedAt: xdb.CTime(t, "created_at"),
	}
}

func (c customerTable) As(alias string) customerTable { return newCustomerTable(c.Table.As(alias)) }

type orderTable struct {
	xdb.Table
	ID         xdb.NumberCol[int64]
	CustomerID xdb.NumberCol[int64]
	Amount     xdb.NumberCol[int64]
	State      xdb.StringCol
}

func newOrderTable(t xdb.Table) orderTable {
	return orderTable{
		Table:      t,
		ID:         xdb.CNum[int64](t, "id"),
		CustomerID: xdb.CNum[int64](t, "customer_id"),
		Amount:     xdb.CNum[int64](t, "amount"),
		State:      xdb.CStr(t, "state"),
	}
}

func (o orderTable) As(alias string) orderTable { return newOrderTable(o.Table.As(alias)) }

var (
	U = newCustomerTable(xdb.TableOf[Customer]())
	O = newOrderTable(xdb.TableOf[Order]())
)

// ---------- 断言工具 ----------

type renderer interface {
	SQL(d dialect.Dialect) (string, []any, error)
}

// golden 断言两种方言下的 SQL 与参数。两个方言都必须给出期望值，
// 哪怕它们只差引号与占位符风格，否则很容易漏掉某个方言的验证。
func golden(t *testing.T, r renderer, wantMySQL, wantPG string, wantArgs ...any) {
	t.Helper()
	for _, c := range []struct {
		d    dialect.Dialect
		want string
	}{{mysql.Dialect, wantMySQL}, {postgres.Dialect, wantPG}} {
		if c.want == "" {
			t.Fatalf("[%s] golden expectation is empty; both dialects must be asserted", c.d.Name())
		}
		got, args, err := r.SQL(c.d)
		if err != nil {
			t.Fatalf("[%s] %v", c.d.Name(), err)
		}
		if got != c.want {
			t.Errorf("[%s]\n got: %s\nwant: %s", c.d.Name(), got, c.want)
		}
		if len(args) != len(wantArgs) || (len(args) > 0 && !reflect.DeepEqual(args, wantArgs)) {
			t.Errorf("[%s] args\n got: %#v\nwant: %#v", c.d.Name(), args, wantArgs)
		}
	}
}

func renderErr(r renderer) error {
	_, _, err := r.SQL(postgres.Dialect)
	return err
}

// ---------- SELECT ----------

func TestSelectBasic(t *testing.T) {
	q := xdb.Query[Customer](nil).
		Where(U.Age.Ge(18), U.Name.HasPrefix("t")).
		OrderBy(U.ID.Desc()).
		Limit(20).Offset(40)
	golden(t, q,
		"SELECT `customers`.`id`, `customers`.`name`, `customers`.`email`, `customers`.`age`, `customers`.`created_at` FROM `customers` WHERE (`customers`.`age` >= ? AND `customers`.`name` LIKE ?) ORDER BY `customers`.`id` DESC LIMIT 20 OFFSET 40",
		`SELECT "customers"."id", "customers"."name", "customers"."email", "customers"."age", "customers"."created_at" FROM "customers" WHERE ("customers"."age" >= $1 AND "customers"."name" LIKE $2) ORDER BY "customers"."id" DESC LIMIT 20 OFFSET 40`,
		18, "t%")
}

func TestConditions(t *testing.T) {
	name, minAge := "", 0
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := xdb.Select[Customer](nil).From(U).Where(
		xdb.If(name != "", U.Name.Contains(name)), // 被忽略
		xdb.If(minAge > 0, U.Age.Ge(minAge)),      // 被忽略
		xdb.Or(U.Age.Lt(18), xdb.And(U.Age.Gt(60), U.Name.Contains("50%_off\\"))),
		xdb.Not(U.Email.IsNull()),
		U.ID.In(1, 2, 3),
		U.ID.NotIn(), // 恒真
		U.Age.Between(1, 9),
		U.CreatedAt.After(since),
		U.Name.ILike("TOM%"),
	)
	golden(t, q,
		"SELECT * FROM `customers` WHERE ((`customers`.`age` < ? OR (`customers`.`age` > ? AND `customers`.`name` LIKE ?)) AND NOT (`customers`.`email` IS NULL) AND `customers`.`id` IN (?, ?, ?) AND (1 = 1) AND `customers`.`age` BETWEEN ? AND ? AND `customers`.`created_at` > ? AND LOWER(`customers`.`name`) LIKE LOWER(?))",
		`SELECT * FROM "customers" WHERE (("customers"."age" < $1 OR ("customers"."age" > $2 AND "customers"."name" LIKE $3)) AND NOT ("customers"."email" IS NULL) AND "customers"."id" IN ($4, $5, $6) AND (1 = 1) AND "customers"."age" BETWEEN $7 AND $8 AND "customers"."created_at" > $9 AND "customers"."name" ILIKE $10)`,
		18, 60, `%50\%\_off\\%`, int64(1), int64(2), int64(3), 1, 9, since, "TOM%")

	// 空 IN 生成恒假条件
	golden(t, xdb.Select[Customer](nil).From(U).Where(U.ID.In()),
		"SELECT * FROM `customers` WHERE (1 = 0)",
		`SELECT * FROM "customers" WHERE (1 = 0)`)
}

type CustomerStat struct {
	CustomerID int64  `xdb:"customer_id"`
	Name       string `xdb:"name"`
	Total      int64  `xdb:"total"`
	Cnt        int64  `xdb:"cnt"`
}

func TestJoinGroupHaving(t *testing.T) {
	o := O.As("o")
	q := xdb.Select[CustomerStat](nil,
		U.ID.As("customer_id"),
		U.Name,
		xdb.Coalesce(xdb.Sum(o.Amount), 0).As("total"),
		xdb.CountOf(o.ID).As("cnt"),
	).
		From(U).
		LeftJoin(o, o.CustomerID.EqCol(U.ID), o.State.Eq("paid")).
		GroupBy(U.ID, U.Name).
		Having(xdb.Sum(o.Amount).Gt(100)).
		OrderBy(xdb.C[int64](nil, "total").Desc())
	golden(t, q,
		"SELECT `customers`.`id` AS `customer_id`, `customers`.`name`, COALESCE(SUM(`o`.`amount`), ?) AS `total`, COUNT(`o`.`id`) AS `cnt` FROM `customers` LEFT JOIN `orders` AS `o` ON (`o`.`customer_id` = `customers`.`id` AND `o`.`state` = ?) GROUP BY `customers`.`id`, `customers`.`name` HAVING SUM(`o`.`amount`) > ? ORDER BY `total` DESC",
		`SELECT "customers"."id" AS "customer_id", "customers"."name", COALESCE(SUM("o"."amount"), $1) AS "total", COUNT("o"."id") AS "cnt" FROM "customers" LEFT JOIN "orders" AS "o" ON ("o"."customer_id" = "customers"."id" AND "o"."state" = $2) GROUP BY "customers"."id", "customers"."name" HAVING SUM("o"."amount") > $3 ORDER BY "total" DESC`,
		int64(0), "paid", int64(100))

	if err := renderErr(xdb.Select[Customer](nil).From(U).Join(O)); err == nil {
		t.Fatal("JOIN without ON must fail")
	}
}

func TestSubqueries(t *testing.T) {
	paid := xdb.Select[int64](nil, O.CustomerID).From(O).Where(O.State.Eq("paid"))
	golden(t, xdb.Query[Customer](nil).Columns(U.ID).Where(U.ID.InSub(paid), U.Age.Gt(1)),
		"SELECT `customers`.`id` FROM `customers` WHERE (`customers`.`id` IN (SELECT `orders`.`customer_id` FROM `orders` WHERE `orders`.`state` = ?) AND `customers`.`age` > ?)",
		`SELECT "customers"."id" FROM "customers" WHERE ("customers"."id" IN (SELECT "orders"."customer_id" FROM "orders" WHERE "orders"."state" = $1) AND "customers"."age" > $2)`,
		"paid", 1)

	// 关联子查询 + EXISTS
	golden(t, xdb.Query[Customer](nil).Columns(U.ID).Where(
		xdb.Exists(xdb.Select[int](nil, xdb.V(1)).From(O).Where(O.CustomerID.EqCol(U.ID), O.Amount.Gt(10))),
	),
		"SELECT `customers`.`id` FROM `customers` WHERE EXISTS (SELECT ? FROM `orders` WHERE (`orders`.`customer_id` = `customers`.`id` AND `orders`.`amount` > ?))",
		`SELECT "customers"."id" FROM "customers" WHERE EXISTS (SELECT $1 FROM "orders" WHERE ("orders"."customer_id" = "customers"."id" AND "orders"."amount" > $2))`,
		1, int64(10))

	// 标量子查询作为列
	cnt := xdb.Scalar(xdb.Select[int64](nil, xdb.Count()).From(O).Where(O.CustomerID.EqCol(U.ID)))
	golden(t, xdb.Select[CustomerStat](nil, U.ID.As("customer_id"), cnt.As("cnt")).From(U).Where(cnt.Gt(2)),
		"SELECT `customers`.`id` AS `customer_id`, (SELECT COUNT(*) FROM `orders` WHERE `orders`.`customer_id` = `customers`.`id`) AS `cnt` FROM `customers` WHERE (SELECT COUNT(*) FROM `orders` WHERE `orders`.`customer_id` = `customers`.`id`) > ?",
		`SELECT "customers"."id" AS "customer_id", (SELECT COUNT(*) FROM "orders" WHERE "orders"."customer_id" = "customers"."id") AS "cnt" FROM "customers" WHERE (SELECT COUNT(*) FROM "orders" WHERE "orders"."customer_id" = "customers"."id") > $1`,
		int64(2))

	// 派生表
	stats := xdb.Derived("t", xdb.Select[CustomerStat](nil, O.CustomerID, xdb.Count().As("cnt")).From(O).GroupBy(O.CustomerID))
	golden(t, xdb.Select[CustomerStat](nil, stats.Star()).From(stats).Where(xdb.C[int64](stats, "cnt").Gt(10)),
		"SELECT `t`.* FROM (SELECT `orders`.`customer_id`, COUNT(*) AS `cnt` FROM `orders` GROUP BY `orders`.`customer_id`) AS `t` WHERE `t`.`cnt` > ?",
		`SELECT "t".* FROM (SELECT "orders"."customer_id", COUNT(*) AS "cnt" FROM "orders" GROUP BY "orders"."customer_id") AS "t" WHERE "t"."cnt" > $1`,
		int64(10))
}

type Node struct {
	ID       int64  `xdb:"id"`
	ParentID *int64 `xdb:"parent_id"`
}

func TestCTE(t *testing.T) {
	nodes := xdb.T("nodes")
	tree := xdb.T("tree")
	base := xdb.Select[Node](nil, xdb.C[int64](nodes, "id"), xdb.C[int64](nodes, "parent_id")).
		From(nodes).Where(xdb.C[int64](nodes, "id").Eq(1))
	rec := xdb.Select[Node](nil, xdb.C[int64](nodes, "id"), xdb.C[int64](nodes, "parent_id")).
		From(nodes).Join(tree, xdb.C[int64](nodes, "parent_id").EqCol(xdb.C[int64](tree, "id")))
	cte := xdb.CTE("tree", base.UnionAll(rec)).Columns("id", "parent_id")
	q := xdb.Select[Node](nil).WithRecursive(cte).From(cte).OrderBy(xdb.C[int64](cte, "id").Asc())
	golden(t, q,
		"WITH RECURSIVE `tree` (`id`, `parent_id`) AS (SELECT `nodes`.`id`, `nodes`.`parent_id` FROM `nodes` WHERE `nodes`.`id` = ? UNION ALL SELECT `nodes`.`id`, `nodes`.`parent_id` FROM `nodes` JOIN `tree` ON `nodes`.`parent_id` = `tree`.`id`) SELECT * FROM `tree` ORDER BY `tree`.`id`",
		`WITH RECURSIVE "tree" ("id", "parent_id") AS (SELECT "nodes"."id", "nodes"."parent_id" FROM "nodes" WHERE "nodes"."id" = $1 UNION ALL SELECT "nodes"."id", "nodes"."parent_id" FROM "nodes" JOIN "tree" ON "nodes"."parent_id" = "tree"."id") SELECT * FROM "tree" ORDER BY "tree"."id"`,
		int64(1))

	if err := renderErr(xdb.Select[Node](nil).From(nodes).Union(xdb.Select[Node](nil).From(nodes).Limit(1))); err == nil {
		t.Fatal("union operand with LIMIT must fail")
	}
}

func TestLockAndOffset(t *testing.T) {
	golden(t, xdb.Query[Customer](nil).Columns(U.ID).Where(U.ID.Eq(1)).ForUpdate().SkipLocked(),
		"SELECT `customers`.`id` FROM `customers` WHERE `customers`.`id` = ? FOR UPDATE SKIP LOCKED",
		`SELECT "customers"."id" FROM "customers" WHERE "customers"."id" = $1 FOR UPDATE SKIP LOCKED`, int64(1))
	golden(t, xdb.Query[Customer](nil).Columns(U.ID).Offset(5),
		"SELECT `customers`.`id` FROM `customers` LIMIT 18446744073709551615 OFFSET 5",
		`SELECT "customers"."id" FROM "customers" OFFSET 5`)
	golden(t, xdb.Query[Customer](nil).Columns(U.ID).Page(3, 10),
		"SELECT `customers`.`id` FROM `customers` LIMIT 10 OFFSET 20",
		`SELECT "customers"."id" FROM "customers" LIMIT 10 OFFSET 20`)
	if err := renderErr(xdb.Query[Customer](nil).NoWait()); err == nil {
		t.Fatal("NoWait without lock must fail")
	}
}

func TestClauses(t *testing.T) {
	golden(t, xdb.Select[Customer](nil, U.ID).From(U).Distinct().CrossJoin(O),
		"SELECT DISTINCT `customers`.`id` FROM `customers` CROSS JOIN `orders`",
		`SELECT DISTINCT "customers"."id" FROM "customers" CROSS JOIN "orders"`)

	golden(t, xdb.Select[Customer](nil, U.ID).From(U).RightJoin(O, O.CustomerID.EqCol(U.ID)),
		"SELECT `customers`.`id` FROM `customers` RIGHT JOIN `orders` ON `orders`.`customer_id` = `customers`.`id`",
		`SELECT "customers"."id" FROM "customers" RIGHT JOIN "orders" ON "orders"."customer_id" = "customers"."id"`)

	// 无 ON 的 JOIN 必须报错（CROSS JOIN 除外）
	if err := renderErr(xdb.Select[Customer](nil, U.ID).From(U).LeftJoin(O)); err == nil {
		t.Fatal("LEFT JOIN without ON must fail")
	}

	golden(t, xdb.Query[Customer](nil).Columns(U.ID).ForShare().NoWait(),
		"SELECT `customers`.`id` FROM `customers` FOR SHARE NOWAIT",
		`SELECT "customers"."id" FROM "customers" FOR SHARE NOWAIT`)

	// UNION 的 ORDER BY / LIMIT 作用于整个组合结果
	golden(t, xdb.Select[Customer](nil, U.ID).From(U).
		Union(xdb.Select[Customer](nil, U.ID).From(U)).OrderBy(U.ID.Asc()).Limit(3),
		"SELECT `customers`.`id` FROM `customers` UNION SELECT `customers`.`id` FROM `customers` ORDER BY `customers`.`id` LIMIT 3",
		`SELECT "customers"."id" FROM "customers" UNION SELECT "customers"."id" FROM "customers" ORDER BY "customers"."id" LIMIT 3`)

	golden(t, xdb.Select[CustomerStat](nil,
		xdb.Func[string]("UPPER", U.Name).As("name"),
		xdb.CountDistinct(O.CustomerID).As("cnt"),
	).From(U).GroupBy(U.Name),
		"SELECT UPPER(`customers`.`name`) AS `name`, COUNT(DISTINCT `orders`.`customer_id`) AS `cnt` FROM `customers` GROUP BY `customers`.`name`",
		`SELECT UPPER("customers"."name") AS "name", COUNT(DISTINCT "orders"."customer_id") AS "cnt" FROM "customers" GROUP BY "customers"."name"`)
}

// Cast 让调用方显式选择类型；可移植类型名由方言映射为原生写法（MySQL 的 BIGINT → SIGNED）。
func TestCast(t *testing.T) {
	q := xdb.Select[CustomerStat](nil, xdb.Cast[int64]("BIGINT", xdb.Sum(O.Amount)).As("total")).From(U)
	golden(t, q,
		"SELECT CAST(SUM(`orders`.`amount`) AS SIGNED) AS `total` FROM `customers`",
		`SELECT CAST(SUM("orders"."amount") AS BIGINT) AS "total" FROM "customers"`)

	golden(t, xdb.Select[CustomerStat](nil, xdb.Cast[string]("TEXT", U.Name).As("name")).From(U),
		"SELECT CAST(`customers`.`name` AS CHAR) AS `name` FROM `customers`",
		`SELECT CAST("customers"."name" AS TEXT) AS "name" FROM "customers"`)
}

func TestImmutable(t *testing.T) {
	base := xdb.Query[Customer](nil).Columns(U.ID).Where(U.Age.Gt(1))
	a := base.Where(U.Name.Eq("a"))
	b := base.Where(U.Name.Eq("b"))
	golden(t, base,
		"SELECT `customers`.`id` FROM `customers` WHERE `customers`.`age` > ?",
		`SELECT "customers"."id" FROM "customers" WHERE "customers"."age" > $1`, 1)
	golden(t, a,
		"SELECT `customers`.`id` FROM `customers` WHERE (`customers`.`age` > ? AND `customers`.`name` = ?)",
		`SELECT "customers"."id" FROM "customers" WHERE ("customers"."age" > $1 AND "customers"."name" = $2)`, 1, "a")
	golden(t, b,
		"SELECT `customers`.`id` FROM `customers` WHERE (`customers`.`age` > ? AND `customers`.`name` = ?)",
		`SELECT "customers"."id" FROM "customers" WHERE ("customers"."age" > $1 AND "customers"."name" = $2)`, 1, "b")
}

func TestSQLFragment(t *testing.T) {
	e := xdb.SQL[bool]("? = ANY(?) OR ? IN (?)", U.Name, xdb.AsIs([]string{"x"}), U.Age, []int{1, 2})
	golden(t, xdb.Select[Customer](nil, U.ID).From(U).Where(e),
		"SELECT `customers`.`id` FROM `customers` WHERE `customers`.`name` = ANY(?) OR `customers`.`age` IN (?, ?)",
		`SELECT "customers"."id" FROM "customers" WHERE "customers"."name" = ANY($1) OR "customers"."age" IN ($2, $3)`,
		[]string{"x"}, 1, 2)
}

// ---------- INSERT ----------

func strp(s string) *string { return &s }

func TestInsert(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := &Customer{Name: "tom", Email: strp("t@x.com"), Age: 20, CreatedAt: now}
	golden(t, xdb.Insert(nil, c),
		"INSERT INTO `customers` (`name`, `email`, `age`, `created_at`) VALUES (?, ?, ?, ?)",
		`INSERT INTO "customers" ("name", "email", "age", "created_at") VALUES ($1, $2, $3, $4) RETURNING "id"`,
		"tom", c.Email, 20, now)

	cs := []Customer{{Name: "a"}, {Name: "b"}}
	golden(t, xdb.InsertMany(nil, cs).Columns(U.Name, U.Age),
		"INSERT INTO `customers` (`name`, `age`) VALUES (?, ?), (?, ?)",
		`INSERT INTO "customers" ("name", "age") VALUES ($1, $2), ($3, $4) RETURNING "id"`, "a", 0, "b", 0)
	golden(t, xdb.InsertMany(nil, cs).Omit(U.Email, U.CreatedAt).Columns(U.ID, U.Name),
		"INSERT INTO `customers` (`id`, `name`) VALUES (?, ?), (?, ?)",
		`INSERT INTO "customers" ("id", "name") VALUES ($1, $2), ($3, $4)`, int64(0), "a", int64(0), "b")

	up := xdb.Insert(nil, &Customer{Name: "tom"}).Columns(U.Name, U.Age).OnConflict(U.Name).DoUpdate(U.Age)
	golden(t, up,
		"INSERT INTO `customers` (`name`, `age`) VALUES (?, ?) AS `xdb_new` ON DUPLICATE KEY UPDATE `age` = `xdb_new`.`age`",
		`INSERT INTO "customers" ("name", "age") VALUES ($1, $2) ON CONFLICT ("name") DO UPDATE SET "age" = EXCLUDED."age" RETURNING "id"`,
		"tom", 0)

	nothing := xdb.Insert(nil, &Customer{Name: "tom"}).Columns(U.Name).OnConflict(U.Name).DoNothing()
	golden(t, nothing,
		"INSERT INTO `customers` (`name`) VALUES (?) AS `xdb_new` ON DUPLICATE KEY UPDATE `customers`.`name` = `customers`.`name`",
		`INSERT INTO "customers" ("name") VALUES ($1) ON CONFLICT ("name") DO NOTHING`, "tom")

	set := xdb.Insert(nil, &Customer{Name: "tom", Age: 1}).Columns(U.Name, U.Age).OnConflict(U.Name).
		DoUpdateSet(U.Age.ToExpr(xdb.Add[int](U.Age, xdb.Excluded(U.Age.Col))))
	golden(t, set,
		"INSERT INTO `customers` (`name`, `age`) VALUES (?, ?) AS `xdb_new` ON DUPLICATE KEY UPDATE `age` = (`customers`.`age` + `xdb_new`.`age`)",
		`INSERT INTO "customers" ("name", "age") VALUES ($1, $2) ON CONFLICT ("name") DO UPDATE SET "age" = ("customers"."age" + EXCLUDED."age") RETURNING "id"`,
		"tom", 1)

	if err := renderErr(xdb.Insert(nil, c).OnConflict().DoUpdate(U.Age)); !errors.Is(err, xdb.ErrConflictTarget) {
		t.Fatalf("want ErrConflictTarget, got %v", err)
	}
	if err := renderErr(xdb.Insert(nil, c).Columns(O.State)); !errors.Is(err, xdb.ErrForeignColumn) {
		t.Fatalf("want ErrForeignColumn, got %v", err)
	}
}

func TestInsertBatchAndBackfill(t *testing.T) {
	var stmts []string
	id := int64(100)
	db, _ := newDB(t, func(q string, args []any) (*fakedb.Result, error) {
		stmts = append(stmts, q)
		res := &fakedb.Result{Columns: []string{"id"}}
		for range len(args) / 4 {
			id++
			res.Rows = append(res.Rows, []any{id})
		}
		return res, nil
	})
	cs := make([]Customer, 5)
	n, err := xdb.InsertMany(db, cs).Batch(2).Exec(ctx)
	if err != nil || n != 5 || len(stmts) != 3 {
		t.Fatalf("n=%d err=%v stmts=%d", n, err, len(stmts))
	}
	for i, c := range cs {
		if c.ID != int64(101+i) {
			t.Fatalf("row %d id=%d", i, c.ID)
		}
	}
}

// ---------- UPDATE / DELETE ----------

func TestUpdateDelete(t *testing.T) {
	golden(t, xdb.Update[Customer](nil).Set(U.Name.To("x"), U.Age.Incr(1), U.Email.ToNull()).Where(U.ID.Eq(1)),
		"UPDATE `customers` SET `name` = ?, `age` = (`customers`.`age` + ?), `email` = NULL WHERE `customers`.`id` = ?",
		`UPDATE "customers" SET "name" = $1, "age" = ("customers"."age" + $2), "email" = NULL WHERE "customers"."id" = $3`,
		"x", 1, int64(1))

	if err := renderErr(xdb.Update[Customer](nil).Set(U.Name.To("x"))); !errors.Is(err, xdb.ErrMissingWhere) {
		t.Fatalf("want ErrMissingWhere, got %v", err)
	}
	if err := renderErr(xdb.Update[Customer](nil).Set(U.Name.To("x")).Where(xdb.If(false, U.ID.Eq(1)))); !errors.Is(err, xdb.ErrMissingWhere) {
		t.Fatalf("dynamic empty WHERE must be rejected, got %v", err)
	}
	golden(t, xdb.Update[Customer](nil).Set(U.Age.To(0)).AllRows(),
		"UPDATE `customers` SET `age` = ?",
		`UPDATE "customers" SET "age" = $1`, 0)
	if err := renderErr(xdb.Update[Customer](nil).Set(O.State.To("x")).Where(U.ID.Eq(1))); !errors.Is(err, xdb.ErrForeignColumn) {
		t.Fatalf("want ErrForeignColumn, got %v", err)
	}

	c := &Customer{ID: 7, Name: "n", Age: 3}
	golden(t, xdb.UpdateModel(nil, c, U.Name, U.Age).Where(U.Age.Eq(2)),
		"UPDATE `customers` SET `name` = ?, `age` = ? WHERE (`customers`.`id` = ? AND `customers`.`age` = ?)",
		`UPDATE "customers" SET "name" = $1, "age" = $2 WHERE ("customers"."id" = $3 AND "customers"."age" = $4)`,
		"n", 3, int64(7), 2)

	golden(t, xdb.Delete[Customer](nil).Where(U.ID.In(1, 2)),
		"DELETE FROM `customers` WHERE `customers`.`id` IN (?, ?)",
		`DELETE FROM "customers" WHERE "customers"."id" IN ($1, $2)`, int64(1), int64(2))
	golden(t, xdb.DeleteModel(nil, c),
		"DELETE FROM `customers` WHERE `customers`.`id` = ?",
		`DELETE FROM "customers" WHERE "customers"."id" = $1`, int64(7))
	if err := renderErr(xdb.Delete[Customer](nil)); !errors.Is(err, xdb.ErrMissingWhere) {
		t.Fatalf("want ErrMissingWhere, got %v", err)
	}
}

// ---------- 执行路径 ----------

func TestQueryExecution(t *testing.T) {
	var last string
	db, _ := newDB(t, func(q string, _ []any) (*fakedb.Result, error) {
		last = q
		switch {
		case strings.HasPrefix(q, "SELECT COUNT(*)"):
			return &fakedb.Result{Columns: []string{"count"}, Rows: [][]any{{int64(3)}}}, nil
		case strings.HasPrefix(q, "SELECT EXISTS"):
			return &fakedb.Result{Columns: []string{"exists"}, Rows: [][]any{{true}}}, nil
		}
		return &fakedb.Result{Columns: []string{"id"}, Rows: [][]any{{int64(1)}, {int64(2)}}}, nil
	})
	q := xdb.Query[Customer](db).Where(U.Age.Gt(1)).OrderBy(U.ID.Asc())

	n, err := q.Count(ctx)
	if err != nil || n != 3 || last != `SELECT COUNT(*) FROM "customers" WHERE "customers"."age" > $1` {
		t.Fatalf("count %d %v %s", n, err, last)
	}
	_, _ = q.Limit(5).Count(ctx)
	if !strings.HasPrefix(last, `SELECT COUNT(*) FROM (SELECT`) || !strings.HasSuffix(last, `LIMIT 5) AS "xdb_count"`) {
		t.Fatalf("count with limit must wrap: %s", last)
	}
	ok, err := q.Exists(ctx)
	if err != nil || !ok || last != `SELECT EXISTS (SELECT 1 FROM "customers" WHERE "customers"."age" > $1)` {
		t.Fatalf("exists %v %v %s", ok, err, last)
	}
	ids, err := xdb.Pluck(ctx, q, U.ID)
	if err != nil || fmt.Sprint(ids) != "[1 2]" {
		t.Fatalf("pluck %v %v", ids, err)
	}
	if _, err := q.ForUpdate().All(ctx); !errors.Is(err, xdb.ErrLockOutsideTx) {
		t.Fatalf("want ErrLockOutsideTx, got %v", err)
	}
	if got, err := q.All(ctx); err != nil || len(got) != 2 || got[1].ID != 2 {
		t.Fatal(got, err)
	}
}
