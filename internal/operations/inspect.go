package operations

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
)

// Report 是数据根的只读诊断报告。
type Report struct {
	Home        string         `json:"home"`
	Initialized bool           `json:"initialized"`
	Marker      *Marker        `json:"marker,omitempty"`
	LockHolder  *fsutil.Holder `json:"last_lock_holder,omitempty"`
	FileSystem  fsutil.FSInfo  `json:"file_system"`
	FreeBytes   uint64         `json:"free_bytes"`
	MinFree     uint64         `json:"min_free_bytes"`
	Databases   []DBStatus     `json:"databases"`
	Matrix      Matrix         `json:"matrix"`
	// Problems 列出阻止启动或开放写入的原因；为空表示可以启动。
	Problems []Reason `json:"problems,omitempty"`
	Notes    []Reason `json:"notes,omitempty"`
}

// Compatible 报告数据根能否由本构建直接启动。
func (r Report) Compatible() bool { return r.Initialized && len(r.Problems) == 0 }

// Inspect 只读诊断数据根：不取数据根锁、不创建库、不修改标记，实例运行中也
// 可调用。它给出与 Open 相同类别的原因，供 lantai doctor 与恢复诊断使用；
// 锁文件中的持有者信息只是最近一次取锁的记录，不代表锁当前一定被持有。
func Inspect(ctx context.Context, home string) (Report, error) {
	l, err := NewLayout(home)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Home: l.Home, Matrix: CurrentMatrix()}
	add := func(code, format string, args ...any) {
		rep.Problems = append(rep.Problems, Reason{Code: code, Message: fmt.Sprintf(format, args...)})
	}
	cfg, err := LoadConfig(l)
	if err != nil {
		return rep, err
	}
	rep.MinFree = cfg.MinFreeBytes
	if h, err := fsutil.ReadHolder(l.LockPath()); err == nil {
		rep.LockHolder = &h
	}
	for _, p := range []string{l.Home, l.DBDir()} {
		if !exists(p) {
			continue
		}
		info, err := fsutil.Inspect(p)
		if err != nil {
			continue
		}
		if p == l.Home {
			rep.FileSystem = info
		}
		if info.Remote {
			add(CodeNetworkFileSystem, "%s is on a network file system (%s)", p, info.Type)
		} else if !info.Known {
			rep.Notes = append(rep.Notes, Reason{CodeFileSystemUnknown, fmt.Sprintf("file system type %q of %s could not be confirmed as local", info.Type, p)})
		}
	}
	if free, err := freeBytes(l); err == nil {
		rep.FreeBytes = free
		if free < cfg.MinFreeBytes {
			add(CodeDiskSpaceLow, "%d bytes free, at least %d required", free, cfg.MinFreeBytes)
		}
	}
	marker, err := ReadMarker(l)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if present := existingDBs(l); len(present) > 0 {
			add(CodeUnmarkedData, "database files exist but the instance marker is missing (%s)", strings.Join(present, ", "))
		} else {
			add(CodeNotInitialized, "%s has not been initialized", l.Home)
		}
		return rep, nil
	case err != nil:
		return rep, err
	}
	rep.Marker = &marker
	rep.Initialized = marker.State == MarkerActive
	if !rep.Initialized {
		add(CodeInitIncomplete, "initialization has not completed; run lantai init to resume")
	}
	switch {
	case marker.DataFormatVersion > DataFormatVersion:
		add(CodeFormatNewer, "data format %d is newer than this build (%d)", marker.DataFormatVersion, DataFormatVersion)
	case marker.DataFormatVersion < DataFormatVersion:
		add(CodeMigrationRequired, "data format %d must be migrated to %d", marker.DataFormatVersion, DataFormatVersion)
	}
	if run := marker.Migration; run != nil {
		add(CodeMigrationIncomplete, "migration run %s has not completed", run.RunID)
	}
	for _, db := range migrations.Databases {
		path := l.DBPath(db)
		if !exists(path) {
			rep.Databases = append(rep.Databases, DBStatus{Database: db, Latest: migrations.Latest(db)})
			if isAuthoritative(db) {
				add(CodeDatabaseMissing, "%s.db is missing; restore from a complete backup", db)
			} else {
				rep.Notes = append(rep.Notes, Reason{CodeIndexRecreated, "index.db is missing and will be recreated empty on start"})
			}
			continue
		}
		conn, err := sqlite.Open(ctx, path, sqlite.Options{ReadOnly: true})
		if err != nil {
			return rep, fmt.Errorf("operations: open %s.db read-only: %w", db, err)
		}
		st, err := inspectDB(ctx, conn, db, migrations.For)
		conn.Close()
		if err != nil {
			return rep, err
		}
		rep.Databases = append(rep.Databases, st)
		switch {
		case len(st.Problems) > 0:
			add(CodeSchemaIncompatible, "%s.db: %s", db, strings.Join(st.Problems, "; "))
		case st.InstanceID == "":
			add(CodeBindingMissing, "%s.db is not bound to any instance", db)
		case st.InstanceID != marker.InstanceID:
			add(CodeInstanceMismatch, "%s.db belongs to instance %s", db, st.InstanceID)
		case st.Pending():
			add(CodeMigrationRequired, "%s.db is at version %d, this build requires %d", db, st.Applied, st.Latest)
		}
	}
	return rep, nil
}
