package operations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
)

// infraSQL 是 operations 维护的两张基础设施表：每库的迁移版本与实例绑定。
// 它们由迁移执行器在应用业务迁移前创建，不属于任何编号迁移。
const infraSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY CHECK (version >= 1),
	owner      TEXT    NOT NULL,
	name       TEXT    NOT NULL,
	checksum   TEXT    NOT NULL,
	applied_at INTEGER NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS instance_binding (
	id          INTEGER PRIMARY KEY CHECK (id = 1),
	instance_id TEXT    NOT NULL,
	db_name     TEXT    NOT NULL,
	bound_at    INTEGER NOT NULL
) STRICT;
`

// DataFormatVersion 是本构建读写的数据格式版本，即实例兼容矩阵的版本号。
// 数据根的标记版本高于它时拒绝启动（不支持降级）；低于它时需要迁移。
const DataFormatVersion = 1

// Matrix 是本构建的实例兼容矩阵：数据格式版本与每个库要求的 schema 版本。
// 全部库与标记都满足同一矩阵后实例才开放服务。
type Matrix struct {
	DataFormatVersion int                        `json:"data_format_version"`
	Databases         map[ownership.Database]int `json:"databases"`
}

// migrationSource 返回某个库的迁移列表；测试可替换为较早的版本集合。
type migrationSource func(ownership.Database) []migrations.Migration

func (src migrationSource) matrix() Matrix {
	m := Matrix{DataFormatVersion: DataFormatVersion, Databases: map[ownership.Database]int{}}
	for _, db := range migrations.Databases {
		m.Databases[db] = len(src(db))
	}
	return m
}

// CurrentMatrix 返回本构建的兼容矩阵。
func CurrentMatrix() Matrix { return migrationSource(migrations.For).matrix() }

// DBStatus 是一个库的迁移与绑定状态。
type DBStatus struct {
	Database ownership.Database `json:"database"`
	Present  bool               `json:"present"`
	// Applied 是已连续应用的最高版本；Latest 是本构建已知的最高版本。
	Applied    int      `json:"applied_version"`
	Latest     int      `json:"latest_version"`
	InstanceID ids.ID   `json:"instance_id,omitempty"`
	Problems   []string `json:"problems,omitempty"`
}

// Pending 报告是否还有未应用的迁移。
func (s DBStatus) Pending() bool { return s.Applied < s.Latest }

func tableNames(ctx context.Context, q commands.DBTX) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'table'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// inspectDB 只读核对一个库：已应用迁移连续且摘要与本构建一致、没有未登记
// 或不属于本库的表、实例绑定。发现的问题写入 Problems，不修改库。
func inspectDB(ctx context.Context, q commands.DBTX, name ownership.Database, src migrationSource) (DBStatus, error) {
	known := src(name)
	st := DBStatus{Database: name, Present: true, Latest: len(known)}
	tables, err := tableNames(ctx, q)
	if err != nil {
		return st, err
	}
	var unregistered []string
	for t := range tables {
		if _, err := ownership.CheckTable(name, t); err != nil {
			unregistered = append(unregistered, t)
		}
	}
	sort.Strings(unregistered)
	for _, t := range unregistered {
		st.Problems = append(st.Problems, fmt.Sprintf("table %s is not registered for %s.db", t, name))
	}
	if tables["schema_migrations"] {
		rows, err := q.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations ORDER BY version`)
		if err != nil {
			return st, err
		}
		expect := 1
		for rows.Next() {
			var v int
			var sum string
			if err := rows.Scan(&v, &sum); err != nil {
				rows.Close()
				return st, err
			}
			switch {
			case v != expect:
				st.Problems = append(st.Problems, fmt.Sprintf("migration versions are not contiguous: found %d, expected %d", v, expect))
			case v > len(known):
				st.Problems = append(st.Problems, fmt.Sprintf("migration %d is newer than this build (latest %d)", v, len(known)))
			case digest.Digest(sum) != known[v-1].Checksum():
				st.Problems = append(st.Problems, fmt.Sprintf("migration %s was applied with different content", known[v-1].ID()))
			}
			if v == expect {
				st.Applied = v
			}
			expect = v + 1
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return st, err
		}
	}
	if tables["instance_binding"] {
		var id string
		err := q.QueryRowContext(ctx, `SELECT instance_id FROM instance_binding WHERE id = 1`).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return st, err
		default:
			st.InstanceID = ids.ID(id)
		}
	}
	return st, nil
}

// migrateDB 在一个库内按版本依次应用未完成的迁移，每个迁移与其版本记录在
// 同一事务中提交；中途失败时已提交的迁移保留，重跑从下一个版本继续。
// 每个迁移新建的表都必须登记在该迁移的所有者名下。
func migrateDB(ctx context.Context, db *sql.DB, name ownership.Database, src migrationSource, clk clock.Clock) ([]int, error) {
	if _, err := db.ExecContext(ctx, infraSQL); err != nil {
		return nil, fmt.Errorf("operations: %s.db: create migration tables: %w", name, err)
	}
	st, err := inspectDB(ctx, db, name, src)
	if err != nil {
		return nil, err
	}
	if len(st.Problems) > 0 {
		return nil, fmt.Errorf("operations: %s.db cannot be migrated: %v", name, st.Problems)
	}
	var applied []int
	for _, m := range src(name)[st.Applied:] {
		if err := applyOne(ctx, db, m, clk); err != nil {
			return applied, err
		}
		applied = append(applied, m.Version)
	}
	return applied, nil
}

func applyOne(ctx context.Context, db *sql.DB, m migrations.Migration, clk clock.Clock) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	before, err := tableNames(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("operations: apply %s: %w", m.ID(), err)
	}
	after, err := tableNames(ctx, tx)
	if err != nil {
		return err
	}
	for t := range after {
		if before[t] {
			continue
		}
		owner, err := ownership.CheckTable(m.DB, t)
		if err != nil {
			return fmt.Errorf("operations: %s: %w", m.ID(), err)
		}
		if owner != m.Owner && owner != "sqlite" {
			return fmt.Errorf("operations: %s creates table %s owned by %s, not %s", m.ID(), t, owner, m.Owner)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, owner, name, checksum, applied_at)
		VALUES (?, ?, ?, ?, ?)`, m.Version, m.Owner, m.Name, string(m.Checksum()), clock.Millis(clk.Now())); err != nil {
		if sqlite.IsUniqueViolation(err) {
			return fmt.Errorf("operations: %s was applied concurrently: %w", m.ID(), err)
		}
		return err
	}
	return tx.Commit()
}

// bindDB 把库绑定到实例；已绑定到其他实例时报错，不覆盖。
func bindDB(ctx context.Context, db *sql.DB, name ownership.Database, instance ids.ID, clk clock.Clock) error {
	if _, err := db.ExecContext(ctx, infraSQL); err != nil {
		return err
	}
	res, err := db.ExecContext(ctx, `INSERT INTO instance_binding (id, instance_id, db_name, bound_at)
		VALUES (1, ?, ?, ?) ON CONFLICT (id) DO NOTHING`, instance, string(name), clock.Millis(clk.Now()))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	var bound string
	if err := db.QueryRowContext(ctx, `SELECT instance_id FROM instance_binding WHERE id = 1`).Scan(&bound); err != nil {
		return err
	}
	if ids.ID(bound) != instance {
		return fmt.Errorf("operations: %s.db belongs to instance %s, not %s", name, bound, instance)
	}
	return nil
}
