package benchmark

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"
	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/benchmark/genmodel"
	"github.com/qiudaozhang/goxdb/dialect"
	_ "github.com/qiudaozhang/goxdb/dialect/mysql"
	_ "github.com/qiudaozhang/goxdb/dialect/postgres"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/schema"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

const selectCols = "id, name, email, age, created_at"

// castBigint 返回把 SUM(BIGINT) 转回 int64 的 CAST 类型。
// MySQL 的 CAST 不接受 BIGINT（只接受 SIGNED），PostgreSQL 反之。
// goxdb 用 dialect.CastType 自动处理，手写 SQL 的三个框架必须自己分支。
func castBigint(b backend) string {
	if b.name == "mysql" {
		return "SIGNED"
	}
	return "BIGINT"
}

// sumExpr 返回聚合表达式：MySQL 的 SUM(BIGINT) 是 DECIMAL，PG 是 NUMERIC，
// 两边都扫不进 int64，必须显式 CAST。
func sumExpr(b backend) string {
	return "COALESCE(CAST(SUM(o.amount) AS " + castBigint(b) + "), 0)"
}

func want100(fw string, n int) error {
	if n != 100 {
		return fmt.Errorf("%s: got %d rows, want 100", fw, n)
	}
	return nil
}

func wantN(fw string, got, want int, what string) error {
	if got != want {
		return fmt.Errorf("%s: %s = %d, want %d", fw, what, got, want)
	}
	return nil
}

// ---------- xdb ----------

func newXDB(std *sql.DB, name string) impl {
	d, ok := dialect.Lookup(name)
	if !ok {
		panic("xdb dialect not registered: " + name)
	}
	return xdbImpl(xdb.New(std, d))
}

// newXDBGen 与 newXDB 使用同一套构建器与 SQL，唯一区别是结果类型换成了
// `goxdb gen` 生成过扫描器的模型，因此读路径不经过反射。
func newXDBGen(std *sql.DB, name string) impl {
	d, ok := dialect.Lookup(name)
	if !ok {
		panic("xdb dialect not registered: " + name)
	}
	db := xdb.New(std, d)
	u := genmodel.U
	base := xdbImpl(db)
	base.SelectOne = func(ctx context.Context, id int64) error {
		_, err := xdb.Query[genmodel.User](db).Where(u.ID.Eq(id)).One(ctx)
		return err
	}
	base.SelectMany = func(ctx context.Context) error {
		rows, err := xdb.Query[genmodel.User](db).
			Where(u.Age.Ge(18)).OrderBy(u.ID.Asc()).Limit(100).All(ctx)
		if err != nil {
			return err
		}
		return wantN("xdb+gen", len(rows), 100, "rows")
	}
	base.SelectPage = func(ctx context.Context, page, size int64) error {
		rows, err := xdb.Query[genmodel.User](db).OrderBy(u.ID.Asc()).Page(page, size).All(ctx)
		if err != nil {
			return err
		}
		return wantN("xdb+gen", len(rows), int(size), "page rows")
	}
	return base
}

func xdbImpl(db *xdb.DB) impl {
	return impl{
		SelectOne: func(ctx context.Context, id int64) error {
			_, err := xdb.Query[BenchUser](db).Where(B.ID.Eq(id)).One(ctx)
			return err
		},
		SelectMany: func(ctx context.Context) error {
			rows, err := xdb.Query[BenchUser](db).
				Where(B.Age.Ge(18)).OrderBy(B.ID.Asc()).Limit(100).All(ctx)
			if err != nil {
				return err
			}
			return want100("xdb", len(rows))
		},
		SelectPage: func(ctx context.Context, page, size int64) error {
			rows, err := xdb.Query[BenchUser](db).OrderBy(B.ID.Asc()).Page(page, size).All(ctx)
			if err != nil {
				return err
			}
			return wantN("xdb", len(rows), int(size), "page rows")
		},
		Count: func(ctx context.Context) error {
			n, err := xdb.Query[BenchUser](db).Where(B.Age.Ge(40)).Count(ctx)
			if err != nil {
				return err
			}
			if n <= 0 {
				return fmt.Errorf("xdb: count = %d", n)
			}
			return nil
		},
		InsertOne: func(ctx context.Context, u *BenchUser) error {
			_, err := xdb.Insert(db, u).Exec(ctx)
			return err
		},
		InsertBatch: func(ctx context.Context, rows []BenchUser) error {
			_, err := xdb.InsertMany(db, rows).Exec(ctx)
			return err
		},
		UpdateOne: func(ctx context.Context, id int64, name string) error {
			_, err := xdb.Update[BenchUser](db).Set(B.Name.To(name)).Where(B.ID.Eq(id)).Exec(ctx)
			return err
		},
		Upsert: func(ctx context.Context, u *BenchUser) error {
			// 冲突目标与 SET 都显式列出，不做零值推断。
			_, err := xdb.Insert(db, u).OnConflict(B.Name).
				DoUpdateSet(B.Age.ToExpr(xdb.Excluded(B.Age.Col)), B.Email.ToExpr(xdb.Excluded(B.Email.Col))).
				Exec(ctx)
			return err
		},
		Preload: func(ctx context.Context) error {
			rows, err := xdb.Query[BenchUser](db).OrderBy(B.ID.Asc()).Limit(preloadN).
				Preload("Orders").All(ctx)
			if err != nil {
				return err
			}
			if len(rows) != preloadN {
				return fmt.Errorf("xdb: preload rows = %d", len(rows))
			}
			for _, r := range rows {
				if len(r.Orders) != 5 {
					return fmt.Errorf("xdb: user %d has %d orders, want 5", r.ID, len(r.Orders))
				}
			}
			return nil
		},
		JoinAggregate: func(ctx context.Context) error {
			total := xdb.Cast[int64]("BIGINT", xdb.Sum(OA.Amount))
			rows, err := xdb.Select[UserStat](db,
				B.ID.As("user_id"),
				xdb.Coalesce(total, 0).As("total"),
				xdb.CountOf(OA.ID).As("cnt"),
			).From(B).
				LeftJoin(OA, OA.UserID.EqCol(B.ID), OA.State.Eq("paid")).
				GroupBy(B.ID).
				OrderBy(B.ID.Asc()).
				Limit(100).
				All(ctx)
			if err != nil {
				return err
			}
			return want100("xdb", len(rows))
		},
		Tx: func(ctx context.Context, id int64, name string) error {
			return db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
				if _, err := xdb.Query[BenchUser](tx).Where(B.ID.Eq(id)).One(ctx); err != nil {
					return err
				}
				_, err := xdb.Update[BenchUser](tx).Set(B.Name.To(name)).Where(B.ID.Eq(id)).Exec(ctx)
				return err
			})
		},
	}
}

// ---------- sqlx ----------

func newSQLX(std *sql.DB, b backend) impl {
	db := sqlx.NewDb(std, b.driver)
	ph := b.placeholder
	byID := fmt.Sprintf("SELECT %s FROM %s WHERE id = %s", selectCols, tableName, ph(1))
	byAge := fmt.Sprintf("SELECT %s FROM %s WHERE age >= %s ORDER BY id LIMIT 100", selectCols, tableName, ph(1))
	page := fmt.Sprintf("SELECT %s FROM %s ORDER BY id LIMIT %s OFFSET %s", selectCols, tableName, ph(1), ph(2))
	count := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE age >= %s", tableName, ph(1))
	insertOne := fmt.Sprintf("INSERT INTO %s (name, email, age, created_at) VALUES (%s, %s, %s, %s)",
		tableName, ph(1), ph(2), ph(3), ph(4))
	insertReturning := insertOne + " RETURNING id"
	updateOne := fmt.Sprintf("UPDATE %s SET name = %s WHERE id = %s", tableName, ph(1), ph(2))
	// ORDER BY id ASC 之后取前 50 个用户，再批量取它们的订单（两次查询，不做 N+1）。
	preloadUsers := fmt.Sprintf("SELECT %s FROM %s ORDER BY id LIMIT %d", selectCols, tableName, preloadN)
	preloadOrders := fmt.Sprintf("SELECT id, user_id, amount, state, created_at FROM %s WHERE user_id IN (?)", orderTable)
	agg := fmt.Sprintf(`SELECT u.id AS user_id, %s AS total, COUNT(o.id) AS cnt
	FROM %s u LEFT JOIN %s o ON o.user_id = u.id AND o.state = %s
	GROUP BY u.id ORDER BY u.id LIMIT 100`, sumExpr(b), tableName, orderTable, ph(1))

	return impl{
		SelectOne: func(ctx context.Context, id int64) error {
			var u BenchUser
			return db.GetContext(ctx, &u, byID, id)
		},
		SelectMany: func(ctx context.Context) error {
			var rows []BenchUser
			if err := db.SelectContext(ctx, &rows, byAge, 18); err != nil {
				return err
			}
			return want100("sqlx", len(rows))
		},
		SelectPage: func(ctx context.Context, p, size int64) error {
			var rows []BenchUser
			if err := db.SelectContext(ctx, &rows, page, size, (p-1)*size); err != nil {
				return err
			}
			return wantN("sqlx", len(rows), int(size), "page rows")
		},
		Count: func(ctx context.Context) error {
			var n int64
			if err := db.GetContext(ctx, &n, count, 40); err != nil {
				return err
			}
			if n <= 0 {
				return fmt.Errorf("sqlx: count = %d", n)
			}
			return nil
		},
		InsertOne: func(ctx context.Context, u *BenchUser) error {
			args := []any{u.Name, u.Email, u.Age, u.CreatedAt}
			// sqlx 不做主键回填：PostgreSQL 用 RETURNING，MySQL 用 LastInsertId。
			if b.name == "postgres" {
				return db.GetContext(ctx, &u.ID, insertReturning, args...)
			}
			res, err := db.ExecContext(ctx, insertOne, args...)
			if err != nil {
				return err
			}
			u.ID, err = res.LastInsertId()
			return err
		},
		InsertBatch: func(ctx context.Context, rows []BenchUser) error {
			// 手工拼多值 INSERT：sqlx 的 NamedExec 走命名参数，比位置参数慢得多。
			q := fmt.Sprintf("INSERT INTO %s (name, email, age, created_at) VALUES ", tableName)
			args := make([]any, 0, len(rows)*4)
			for i, r := range rows {
				if i > 0 {
					q += ", "
				}
				n := len(args)
				q += fmt.Sprintf("(%s, %s, %s, %s)", ph(n+1), ph(n+2), ph(n+3), ph(n+4))
				args = append(args, r.Name, r.Email, r.Age, r.CreatedAt)
			}
			_, err := db.ExecContext(ctx, q, args...)
			return err
		},
		UpdateOne: func(ctx context.Context, id int64, name string) error {
			_, err := db.ExecContext(ctx, updateOne, name, id)
			return err
		},
		Upsert: func(ctx context.Context, u *BenchUser) error {
			var q string
			if b.name == "postgres" {
				q = fmt.Sprintf(`INSERT INTO %s (name, email, age, created_at) VALUES (%s, %s, %s, %s)
					ON CONFLICT (name) DO UPDATE SET age = EXCLUDED.age, email = EXCLUDED.email`,
					tableName, ph(1), ph(2), ph(3), ph(4))
			} else {
				q = fmt.Sprintf(`INSERT INTO %s (name, email, age, created_at) VALUES (%s, %s, %s, %s) AS new
					ON DUPLICATE KEY UPDATE age = new.age, email = new.email`,
					tableName, ph(1), ph(2), ph(3), ph(4))
			}
			_, err := db.ExecContext(ctx, q, u.Name, u.Email, u.Age, u.CreatedAt)
			return err
		},
		Preload: func(ctx context.Context) error {
			var users []BenchUser
			if err := db.SelectContext(ctx, &users, preloadUsers); err != nil {
				return err
			}
			if len(users) != preloadN {
				return fmt.Errorf("sqlx: preload users = %d", len(users))
			}
			ids := make([]int64, len(users))
			for i, u := range users {
				ids[i] = u.ID
			}
			// 用 sqlx.In 展开切片（这是 sqlx 官方推荐的 IN 写法）。
			q, args, err := sqlx.In(preloadOrders, ids)
			if err != nil {
				return err
			}
			q = db.Rebind(q)
			var orders []BenchOrder
			if err := db.SelectContext(ctx, &orders, q, args...); err != nil {
				return err
			}
			return wantN("sqlx", len(orders), preloadN*5, "orders")
		},
		JoinAggregate: func(ctx context.Context) error {
			var rows []UserStat
			if err := db.SelectContext(ctx, &rows, agg, "paid"); err != nil {
				return err
			}
			return want100("sqlx", len(rows))
		},
		Tx: func(ctx context.Context, id int64, name string) error {
			tx, err := db.BeginTxx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			var u BenchUser
			if err := tx.GetContext(ctx, &u, byID, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, updateOne, name, id); err != nil {
				return err
			}
			return tx.Commit()
		},
	}
}

// ---------- GORM ----------

func newGORM(std *sql.DB, b backend) impl {
	var dialector gorm.Dialector
	if b.name == "mysql" {
		dialector = mysql.New(mysql.Config{Conn: std})
	} else {
		dialector = postgres.New(postgres.Config{Conn: std})
	}
	db, err := gorm.Open(dialector, &gorm.Config{
		// 单条写入不隐式开事务：与 sqlx / bun / xdb 对齐，
		// 否则 GORM 的每次写都会多出 BEGIN + COMMIT 两个往返。
		SkipDefaultTransaction: true,
		Logger:                 gormlogger.Discard,
	})
	if err != nil {
		panic(err)
	}
	upsertClause := clauseOnConflict("name", []string{"age", "email"})
	return impl{
		SelectOne: func(ctx context.Context, id int64) error {
			var u BenchUser
			return db.WithContext(ctx).Where("id = ?", id).Take(&u).Error
		},
		SelectMany: func(ctx context.Context) error {
			var rows []BenchUser
			if err := db.WithContext(ctx).Where("age >= ?", 18).Order("id").Limit(100).Find(&rows).Error; err != nil {
				return err
			}
			return want100("gorm", len(rows))
		},
		SelectPage: func(ctx context.Context, p, size int64) error {
			var rows []BenchUser
			if err := db.WithContext(ctx).Order("id").Offset(int((p - 1) * size)).Limit(int(size)).Find(&rows).Error; err != nil {
				return err
			}
			return wantN("gorm", len(rows), int(size), "page rows")
		},
		Count: func(ctx context.Context) error {
			var n int64
			if err := db.WithContext(ctx).Model(&BenchUser{}).Where("age >= ?", 40).Count(&n).Error; err != nil {
				return err
			}
			if n <= 0 {
				return fmt.Errorf("gorm: count = %d", n)
			}
			return nil
		},
		InsertOne: func(ctx context.Context, u *BenchUser) error {
			return db.WithContext(ctx).Create(u).Error
		},
		InsertBatch: func(ctx context.Context, rows []BenchUser) error {
			return db.WithContext(ctx).Create(&rows).Error
		},
		UpdateOne: func(ctx context.Context, id int64, name string) error {
			return db.WithContext(ctx).Model(&BenchUser{}).Where("id = ?", id).Update("name", name).Error
		},
		Upsert: func(ctx context.Context, u *BenchUser) error {
			return db.WithContext(ctx).Clauses(upsertClause).Create(u).Error
		},
		Preload: func(ctx context.Context) error {
			var rows []BenchUser
			if err := db.WithContext(ctx).Preload("Orders").Order("id").Limit(preloadN).Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) != preloadN {
				return fmt.Errorf("gorm: preload users = %d", len(rows))
			}
			for _, r := range rows {
				if len(r.Orders) != 5 {
					return fmt.Errorf("gorm: user %d has %d orders, want 5", r.ID, len(r.Orders))
				}
			}
			return nil
		},
		JoinAggregate: func(ctx context.Context) error {
			var rows []UserStat
			err := db.WithContext(ctx).
				Model(&BenchUser{}).
				Select(fmt.Sprintf("xdb_bench_users.id AS user_id, %s AS total, COUNT(o.id) AS cnt", sumExpr(b))).
				Joins("LEFT JOIN xdb_bench_orders o ON o.user_id = xdb_bench_users.id AND o.state = ?", "paid").
				Group("xdb_bench_users.id").
				Order("xdb_bench_users.id").
				Limit(100).
				Scan(&rows).Error
			if err != nil {
				return err
			}
			return want100("gorm", len(rows))
		},
		Tx: func(ctx context.Context, id int64, name string) error {
			return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				var u BenchUser
				if err := tx.Where("id = ?", id).Take(&u).Error; err != nil {
					return err
				}
				return tx.Model(&BenchUser{}).Where("id = ?", id).Update("name", name).Error
			})
		},
	}
}

// ---------- bun ----------

func newBun(std *sql.DB, b backend) impl {
	var dial schema.Dialect
	if b.name == "mysql" {
		dial = mysqldialect.New()
	} else {
		dial = pgdialect.New()
	}
	db := bun.NewDB(std, dial)
	return impl{
		SelectOne: func(ctx context.Context, id int64) error {
			var u BenchUser
			return db.NewSelect().Model(&u).Where("id = ?", id).Scan(ctx)
		},
		SelectMany: func(ctx context.Context) error {
			var rows []BenchUser
			if err := db.NewSelect().Model(&rows).
				Where("age >= ?", 18).Order("id").Limit(100).Scan(ctx); err != nil {
				return err
			}
			return want100("bun", len(rows))
		},
		SelectPage: func(ctx context.Context, p, size int64) error {
			var rows []BenchUser
			if err := db.NewSelect().Model(&rows).Order("id").
				Limit(size).Offset((p - 1) * size).Scan(ctx); err != nil {
				return err
			}
			return wantN("bun", len(rows), int(size), "page rows")
		},
		Count: func(ctx context.Context) error {
			n, err := db.NewSelect().Model((*BenchUser)(nil)).Where("age >= ?", 40).Count(ctx)
			if err != nil {
				return err
			}
			if n <= 0 {
				return fmt.Errorf("bun: count = %d", n)
			}
			return nil
		},
		InsertOne: func(ctx context.Context, u *BenchUser) error {
			_, err := db.NewInsert().Model(u).Exec(ctx)
			return err
		},
		InsertBatch: func(ctx context.Context, rows []BenchUser) error {
			_, err := db.NewInsert().Model(&rows).Exec(ctx)
			return err
		},
		UpdateOne: func(ctx context.Context, id int64, name string) error {
			_, err := db.NewUpdate().Model(&BenchUser{}).
				Set("name = ?", name).Where("id = ?", id).Exec(ctx)
			return err
		},
		Upsert: func(ctx context.Context, u *BenchUser) error {
			var q *bun.InsertQuery
			if b.name == "postgres" {
				q = db.NewInsert().Model(u).On("CONFLICT (name) DO UPDATE").
					Set("age = EXCLUDED.age").Set("email = EXCLUDED.email")
			} else {
				q = db.NewInsert().Model(u).On("DUPLICATE KEY UPDATE").
					Set("age = VALUES(age)").Set("email = VALUES(email)")
			}
			_, err := q.Exec(ctx)
			return err
		},
		Preload: func(ctx context.Context) error {
			var rows []BenchUser
			// bun 用 Relation 做批量预加载（两次查询）。
			if err := db.NewSelect().Model(&rows).Order("id").Limit(preloadN).
				Relation("Orders").Scan(ctx); err != nil {
				return err
			}
			if len(rows) != preloadN {
				return fmt.Errorf("bun: preload users = %d", len(rows))
			}
			for _, r := range rows {
				if len(r.Orders) != 5 {
					return fmt.Errorf("bun: user %d has %d orders, want 5", r.ID, len(r.Orders))
				}
			}
			return nil
		},
		JoinAggregate: func(ctx context.Context) error {
			var rows []UserStat
			err := db.NewSelect().
				TableExpr(tableName+" AS u").
				ColumnExpr("u.id AS user_id").
				ColumnExpr(sumExpr(b)+" AS total").
				ColumnExpr("COUNT(o.id) AS cnt").
				Join("LEFT JOIN "+orderTable+" AS o ON o.user_id = u.id AND o.state = ?", "paid").
				Group("u.id").
				Order("u.id").
				Limit(100).
				Scan(ctx, &rows)
			if err != nil {
				return err
			}
			return want100("bun", len(rows))
		},
		Tx: func(ctx context.Context, id int64, name string) error {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			var u BenchUser
			if err := tx.NewSelect().Model(&u).Where("id = ?", id).Scan(ctx); err != nil {
				return err
			}
			if _, err := tx.NewUpdate().Model(&BenchUser{}).
				Set("name = ?", name).Where("id = ?", id).Exec(ctx); err != nil {
				return err
			}
			return tx.Commit()
		},
	}
}

// clauseOnConflict 构造 GORM 的 upsert 子句。
func clauseOnConflict(col string, updates []string) clause.OnConflict {
	return clause.OnConflict{
		Columns:   []clause.Column{{Name: col}},
		DoUpdates: clause.AssignmentColumns(updates),
	}
}

// ---------- 纯渲染（不连数据库） ----------

func renderXDB(name string) func() (string, []any, error) {
	q := xdb.Query[BenchUser](nil).
		Where(B.Age.Ge(18), B.Name.HasPrefix("user-")).
		OrderBy(B.ID.Desc()).
		Limit(100)
	d, ok := dialect.Lookup(name)
	if !ok {
		panic("xdb dialect not registered: " + name)
	}
	return func() (string, []any, error) { return q.SQL(d) }
}

func renderGORM(std *sql.DB, b backend) func() (string, []any, error) {
	var dialector gorm.Dialector
	if b.name == "mysql" {
		dialector = mysql.New(mysql.Config{Conn: std})
	} else {
		dialector = postgres.New(postgres.Config{Conn: std})
	}
	db, err := gorm.Open(dialector, &gorm.Config{
		DryRun:                 true,
		SkipDefaultTransaction: true,
		Logger:                 gormlogger.Discard,
	})
	if err != nil {
		panic(err)
	}
	var rows []BenchUser
	return func() (string, []any, error) {
		tx := db.Session(&gorm.Session{DryRun: true}).Where("age >= ?", 18).
			Order("id desc").Limit(100).Find(&rows)
		return tx.Statement.SQL.String(), tx.Statement.Vars, tx.Error
	}
}

func renderBun(std *sql.DB, b backend) func() (string, []any, error) {
	var dial schema.Dialect
	if b.name == "mysql" {
		dial = mysqldialect.New()
	} else {
		dial = pgdialect.New()
	}
	db := bun.NewDB(std, dial)
	var rows []BenchUser
	return func() (string, []any, error) {
		q := db.NewSelect().Model(&rows).
			Where("age >= ?", 18).Order("id desc").Limit(100)
		out, err := q.AppendQuery(db.QueryGen(), nil)
		if err != nil {
			return "", nil, err
		}
		return string(out), nil, nil
	}
}
