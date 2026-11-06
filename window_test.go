package xdb_test

import (
	"testing"

	xdb "github.com/qiudaozhang/goxdb"
)

type RankedOrder struct {
	ID         int64  `xdb:"id"`
	CustomerID int64  `xdb:"customer_id"`
	Amount     int64  `xdb:"amount"`
	RN         int64  `xdb:"rn"`
	Running    int64  `xdb:"running"`
	PrevAmount *int64 `xdb:"prev_amount"`
}

func TestWindowRanking(t *testing.T) {
	w := xdb.Win().PartitionBy(O.CustomerID).OrderBy(O.Amount.Desc())
	q := xdb.Select[RankedOrder](nil,
		O.ID, O.CustomerID, O.Amount,
		xdb.RowNumber().Over(w).As("rn"),
	).From(O)

	golden(t, q,
		"SELECT `orders`.`id`, `orders`.`customer_id`, `orders`.`amount`, ROW_NUMBER() OVER (PARTITION BY `orders`.`customer_id` ORDER BY `orders`.`amount` DESC) AS `rn` FROM `orders`",
		`SELECT "orders"."id", "orders"."customer_id", "orders"."amount", ROW_NUMBER() OVER (PARTITION BY "orders"."customer_id" ORDER BY "orders"."amount" DESC) AS "rn" FROM "orders"`)
}

func TestWindowAggregateAndOffset(t *testing.T) {
	// 累计求和需要显式框架
	w := xdb.Win().PartitionBy(O.CustomerID).OrderBy(O.ID.Asc()).
		Frame("ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW")
	golden(t, xdb.Select[RankedOrder](nil,
		xdb.SumOver[int64](O.Amount).Over(w).As("running"),
	).From(O),
		"SELECT SUM(`orders`.`amount`) OVER (PARTITION BY `orders`.`customer_id` ORDER BY `orders`.`id` ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS `running` FROM `orders`",
		`SELECT SUM("orders"."amount") OVER (PARTITION BY "orders"."customer_id" ORDER BY "orders"."id" ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS "running" FROM "orders"`)

	golden(t, xdb.Select[RankedOrder](nil,
		xdb.Lag[int64](O.Amount, 1).Over(xdb.Win().OrderBy(O.ID.Asc())).As("prev_amount"),
	).From(O),
		"SELECT LAG(`orders`.`amount`, 1) OVER (ORDER BY `orders`.`id`) AS `prev_amount` FROM `orders`",
		`SELECT LAG("orders"."amount", 1) OVER (ORDER BY "orders"."id") AS "prev_amount" FROM "orders"`)
}

func TestWindowEmptyFrame(t *testing.T) {
	// 无分区无排序：OVER ()
	golden(t, xdb.Select[RankedOrder](nil, xdb.Rank().Over(xdb.Win()).As("rn")).From(O),
		"SELECT RANK() OVER () AS `rn` FROM `orders`",
		`SELECT RANK() OVER () AS "rn" FROM "orders"`)
}

func TestWindowDenseRankNtile(t *testing.T) {
	w := xdb.Win().OrderBy(O.Amount.Desc())
	golden(t, xdb.Select[RankedOrder](nil,
		xdb.DenseRank().Over(w).As("rn"),
		xdb.Ntile(4).Over(w).As("bucket"),
	).From(O),
		"SELECT DENSE_RANK() OVER (ORDER BY `orders`.`amount` DESC) AS `rn`, NTILE(4) OVER (ORDER BY `orders`.`amount` DESC) AS `bucket` FROM `orders`",
		`SELECT DENSE_RANK() OVER (ORDER BY "orders"."amount" DESC) AS "rn", NTILE(4) OVER (ORDER BY "orders"."amount" DESC) AS "bucket" FROM "orders"`)
}

// 窗口函数在派生表外层过滤——这是 SQL 唯一正确的过滤方式。
func TestWindowFilterViaDerived(t *testing.T) {
	w := xdb.Win().PartitionBy(O.CustomerID).OrderBy(O.Amount.Desc())
	inner := xdb.Select[RankedOrder](nil, O.ID, O.CustomerID, O.Amount,
		xdb.RowNumber().Over(w).As("rn")).From(O)
	d := xdb.Derived("r", inner)
	golden(t, xdb.Select[RankedOrder](nil, d.Star()).From(d).Where(xdb.C[int64](d, "rn").Eq(1)),
		"SELECT `r`.* FROM (SELECT `orders`.`id`, `orders`.`customer_id`, `orders`.`amount`, ROW_NUMBER() OVER (PARTITION BY `orders`.`customer_id` ORDER BY `orders`.`amount` DESC) AS `rn` FROM `orders`) AS `r` WHERE `r`.`rn` = ?",
		`SELECT "r".* FROM (SELECT "orders"."id", "orders"."customer_id", "orders"."amount", ROW_NUMBER() OVER (PARTITION BY "orders"."customer_id" ORDER BY "orders"."amount" DESC) AS "rn" FROM "orders") AS "r" WHERE "r"."rn" = $1`,
		int64(1))
}

// 尚未覆盖的窗口函数逐一渲染。
func TestWindowRemainingFunctions(t *testing.T) {
	w := xdb.Win().PartitionBy(O.CustomerID).OrderBy(O.ID.Asc())
	all := xdb.Win().PartitionBy(O.CustomerID).OrderBy(O.ID.Asc()).
		Frame("ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING")
	cases := []struct {
		n      xdb.Node
		my, pg string
	}{
		{xdb.PercentRank().Over(w), "PERCENT_RANK() OVER (PARTITION BY `orders`.`customer_id` ORDER BY `orders`.`id`)", `PERCENT_RANK() OVER (PARTITION BY "orders"."customer_id" ORDER BY "orders"."id")`},
		{xdb.CumeDist().Over(w), "CUME_DIST() OVER (PARTITION BY `orders`.`customer_id` ORDER BY `orders`.`id`)", `CUME_DIST() OVER (PARTITION BY "orders"."customer_id" ORDER BY "orders"."id")`},
		{xdb.Lead[int64](O.Amount, 0).Over(w), "LEAD(`orders`.`amount`) OVER (PARTITION BY `orders`.`customer_id` ORDER BY `orders`.`id`)", `LEAD("orders"."amount") OVER (PARTITION BY "orders"."customer_id" ORDER BY "orders"."id")`},
		{xdb.Lead[int64](O.Amount, 2).Over(w), "LEAD(`orders`.`amount`, 2) OVER (PARTITION BY `orders`.`customer_id` ORDER BY `orders`.`id`)", `LEAD("orders"."amount", 2) OVER (PARTITION BY "orders"."customer_id" ORDER BY "orders"."id")`},
		{xdb.FirstValue[int64](O.Amount).Over(w), "FIRST_VALUE(`orders`.`amount`) OVER (PARTITION BY `orders`.`customer_id` ORDER BY `orders`.`id`)", `FIRST_VALUE("orders"."amount") OVER (PARTITION BY "orders"."customer_id" ORDER BY "orders"."id")`},
		{xdb.LastValue[int64](O.Amount).Over(all), "LAST_VALUE(`orders`.`amount`) OVER (PARTITION BY `orders`.`customer_id` ORDER BY `orders`.`id` ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)", `LAST_VALUE("orders"."amount") OVER (PARTITION BY "orders"."customer_id" ORDER BY "orders"."id" ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)`},
		{xdb.NthValue[int64](O.Amount, 2).Over(w), "NTH_VALUE(`orders`.`amount`, 2) OVER (PARTITION BY `orders`.`customer_id` ORDER BY `orders`.`id`)", `NTH_VALUE("orders"."amount", 2) OVER (PARTITION BY "orders"."customer_id" ORDER BY "orders"."id")`},
		{xdb.CountOver(O.ID).Over(xdb.Win().PartitionBy(O.CustomerID)), "COUNT(`orders`.`id`) OVER (PARTITION BY `orders`.`customer_id`)", `COUNT("orders"."id") OVER (PARTITION BY "orders"."customer_id")`},
		{xdb.AvgOver[int64](O.Amount).Over(xdb.Win()), "AVG(`orders`.`amount`) OVER ()", `AVG("orders"."amount") OVER ()`},
		{xdb.MinOver[int64](O.Amount).Over(xdb.Win()), "MIN(`orders`.`amount`) OVER ()", `MIN("orders"."amount") OVER ()`},
		{xdb.MaxOver[int64](O.Amount).Over(xdb.Win()), "MAX(`orders`.`amount`) OVER ()", `MAX("orders"."amount") OVER ()`},
	}
	for _, c := range cases {
		golden(t, xdb.Select[RankedOrder](nil, c.n).From(O),
			"SELECT "+c.my+" FROM `orders`",
			`SELECT `+c.pg+` FROM "orders"`)
	}
}

// 导出类型：窗口函数可以存进变量/字段；未调用 Over 时渲染为 OVER ()。
func TestWindowFuncExportedType(t *testing.T) {
	var rank xdb.WindowFunc[int64] = xdb.Rank()
	var nth xdb.WindowFunc[*int64] = xdb.NthValue[int64](O.Amount, 2) // 可能为 NULL
	_ = nth
	golden(t, xdb.Select[RankedOrder](nil, rank).From(O),
		"SELECT RANK() OVER () FROM `orders`",
		`SELECT RANK() OVER () FROM "orders"`)
}

// 非法的整数参数不会渲染出数据库才报错的 SQL，而是在构建期返回错误。
func TestWindowInvalidArgs(t *testing.T) {
	for name, n := range map[string]xdb.Node{
		"ntile0":   xdb.Ntile(0).Over(xdb.Win()),
		"nth0":     xdb.NthValue[int64](O.Amount, 0).Over(xdb.Win()),
		"lagNeg":   xdb.Lag[int64](O.Amount, -1).Over(xdb.Win()),
		"leadNeg":  xdb.Lead[int64](O.Amount, -2).Over(xdb.Win()),
		"noOverNt": xdb.Ntile(-3),
	} {
		if err := renderErr(xdb.Select[RankedOrder](nil, n).From(O)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
