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
