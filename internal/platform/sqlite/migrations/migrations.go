// Package migrations 登记五库的 schema 迁移，按库和表所有者组织。
//
// 每个迁移属于一个库与一个所有者模块，库内版本从 1 起连续递增。SQL 文件放在
// sql/<库>/<版本>.<所有者>.<名称>.sql；共享组件的基础设施表（命令回执、
// operations、outbox）直接引用 commands.SchemaSQL，不另存一份。已发布的迁移
// 不修改，变更一律追加新迁移：执行器按内容摘要核对已应用的迁移，发现改动即
// 拒绝启动。迁移的执行顺序、实例兼容矩阵与维护屏障由 operations（T08）负责，
// 每个迁移在所属库内单独成事务，不跨库。
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
)

//go:embed sql
var files embed.FS

// Migration 是一个库内的一次 schema 变更。
type Migration struct {
	DB      ownership.Database
	Version int
	// Owner 是迁移创建或修改的表所属的模块（或基础设施组件）；执行器核对
	// 新建的每张表都登记在该所有者名下。
	Owner string
	Name  string
	// SQL 已把 CRLF 统一为 LF，摘要不受检出方式影响。
	SQL string
}

// Checksum 返回迁移内容的摘要；已应用迁移的摘要写入 schema_migrations。
func (m Migration) Checksum() digest.Digest { return digest.Of([]byte(m.SQL)) }

// ID 返回 "<库>/<版本>.<所有者>.<名称>"，用于日志与报告。
func (m Migration) ID() string {
	return fmt.Sprintf("%s/%04d.%s.%s", m.DB, m.Version, m.Owner, m.Name)
}

// builtin 是由共享组件以 Go 常量维护 DDL 的迁移。
var builtin = []Migration{
	{DB: ownership.Main, Version: 1, Owner: "commands", Name: "infra", SQL: commands.SchemaSQL},
	{DB: ownership.Ledger, Version: 1, Owner: "commands", Name: "infra", SQL: commands.SchemaSQL},
	{DB: ownership.Runtime, Version: 1, Owner: "commands", Name: "infra", SQL: commands.SchemaSQL},
}

var fileRE = regexp.MustCompile(`^([0-9]{4})\.([a-z][a-z0-9_]*)\.([a-z0-9][a-z0-9_-]*)\.sql$`)

// Databases 是迁移与启动检查的固定顺序。
var Databases = []ownership.Database{ownership.Main, ownership.Ledger, ownership.Runtime, ownership.Events, ownership.Index}

var registry = mustLoad(files)

func mustLoad(fsys fs.FS) map[ownership.Database][]Migration {
	r, err := load(fsys)
	if err != nil {
		panic(err)
	}
	return r
}

func load(fsys fs.FS) (map[ownership.Database][]Migration, error) {
	out := map[ownership.Database][]Migration{}
	for _, m := range builtin {
		m.SQL = normalize(m.SQL)
		out[m.DB] = append(out[m.DB], m)
	}
	err := fs.WalkDir(fsys, "sql", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		dbName := path.Base(path.Dir(p))
		db := ownership.Database(dbName)
		if !slices.Contains(Databases, db) || path.Dir(path.Dir(p)) != "sql" {
			return fmt.Errorf("migrations: %s is not under sql/<database>/", p)
		}
		m := fileRE.FindStringSubmatch(path.Base(p))
		if m == nil {
			return fmt.Errorf("migrations: %s must be named <NNNN>.<owner>.<name>.sql", p)
		}
		version, _ := strconv.Atoi(m[1])
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		out[db] = append(out[db], Migration{DB: db, Version: version, Owner: m[2], Name: m[3], SQL: normalize(string(raw))})
		return nil
	})
	if err != nil {
		return nil, err
	}
	modules := map[string]bool{}
	for _, mod := range ownership.Modules {
		modules[mod.Name] = true
	}
	for db, list := range out {
		sort.Slice(list, func(i, j int) bool { return list[i].Version < list[j].Version })
		for i, m := range list {
			if m.Version != i+1 {
				return nil, fmt.Errorf("migrations: %s versions must be contiguous from 1, found %d at position %d", db, m.Version, i+1)
			}
			if !modules[m.Owner] {
				return nil, fmt.Errorf("migrations: %s: owner %q is not a registered module", m.ID(), m.Owner)
			}
			if strings.TrimSpace(m.SQL) == "" {
				return nil, fmt.Errorf("migrations: %s is empty", m.ID())
			}
		}
		out[db] = list
	}
	return out, nil
}

func normalize(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// For 返回 db 的全部迁移（按版本排序的副本）。
func For(db ownership.Database) []Migration { return slices.Clone(registry[db]) }

// Latest 返回本构建已知的 db 最高迁移版本；没有迁移时为 0。
func Latest(db ownership.Database) int { return len(registry[db]) }
