package benchmark

import (
	"time"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/uptrace/bun"
)

// BenchUser 是四个框架共用的模型。标签各自独立，互不干扰：
//   - xdb   给 goxdb
//   - db    给 sqlx
//   - bun   给 bun（表名与自增）
//   - gorm  给 GORM
type BenchUser struct {
	bun.BaseModel `bun:"table:xdb_bench_users"`
	ID            int64     `xdb:"id,pk,auto" db:"id" bun:"id,pk,autoincrement" gorm:"primaryKey;column:id"`
	Name          string    `xdb:"name" db:"name"`
	Email         *string   `xdb:"email" db:"email"`
	Age           int       `xdb:"age" db:"age"`
	CreatedAt     time.Time `xdb:"created_at" db:"created_at"`

	Orders []BenchOrder `xdb:"rel:has_many,fk:user_id" bun:"rel:has-many,join:id=user_id" gorm:"foreignKey:UserID"`
}

func (BenchUser) TableName() string { return tableName }

// BenchOrder 是子表，用于关联预加载与 JOIN 聚合基准。
type BenchOrder struct {
	bun.BaseModel `bun:"table:xdb_bench_orders"`
	ID            int64     `xdb:"id,pk,auto" db:"id" bun:"id,pk,autoincrement" gorm:"primaryKey;column:id"`
	UserID        int64     `xdb:"user_id" db:"user_id"`
	Amount        int64     `xdb:"amount" db:"amount"`
	State         string    `xdb:"state" db:"state"`
	CreatedAt     time.Time `xdb:"created_at" db:"created_at"`

	User *BenchUser `xdb:"rel:belongs_to,fk:user_id" bun:"rel:belongs-to,join:user_id=id" gorm:"foreignKey:UserID"`
}

func (BenchOrder) TableName() string { return orderTable }

// UserStat 是 JOIN + GROUP BY 聚合的投影。
type UserStat struct {
	UserID int64 `xdb:"user_id" db:"user_id" bun:"user_id" gorm:"column:user_id"`
	Total  int64 `xdb:"total" db:"total" bun:"total" gorm:"column:total"`
	Cnt    int64 `xdb:"cnt" db:"cnt" bun:"cnt" gorm:"column:cnt"`
}

// 类型化列描述符，等价于 `goxdb gen` 生成的代码。
type benchTable struct {
	xdb.Table
	ID        xdb.NumberCol[int64]
	Name      xdb.StringCol
	Email     xdb.StringCol
	Age       xdb.NumberCol[int]
	CreatedAt xdb.TimeCol
}

type benchOrderTable struct {
	xdb.Table
	ID        xdb.NumberCol[int64]
	UserID    xdb.NumberCol[int64]
	Amount    xdb.NumberCol[int64]
	State     xdb.StringCol
	CreatedAt xdb.TimeCol
}

func newBenchTable(t xdb.Table) benchTable {
	return benchTable{
		Table:     t,
		ID:        xdb.CNum[int64](t, "id"),
		Name:      xdb.CStr(t, "name"),
		Email:     xdb.CStr(t, "email"),
		Age:       xdb.CNum[int](t, "age"),
		CreatedAt: xdb.CTime(t, "created_at"),
	}
}

func newBenchOrderTable(t xdb.Table) benchOrderTable {
	return benchOrderTable{
		Table:     t,
		ID:        xdb.CNum[int64](t, "id"),
		UserID:    xdb.CNum[int64](t, "user_id"),
		Amount:    xdb.CNum[int64](t, "amount"),
		State:     xdb.CStr(t, "state"),
		CreatedAt: xdb.CTime(t, "created_at"),
	}
}

func (o benchOrderTable) As(a string) benchOrderTable { return newBenchOrderTable(o.Table.As(a)) }

var (
	B = newBenchTable(xdb.TableOf[BenchUser]())
	O = newBenchOrderTable(xdb.TableOf[BenchOrder]())
	// OA 是 JOIN 中带别名的订单表。
	OA = O.As("o")
)

// 表名带前缀，避免与用户库里的表冲突。
const (
	tableName  = "xdb_bench_users"
	orderTable = "xdb_bench_orders"
)
