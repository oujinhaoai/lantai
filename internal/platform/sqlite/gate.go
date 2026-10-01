package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// ErrWriteQueueTimeout 表示在进程内写入队列中等待超过上限；Classify 归为 ClassBusy。
var ErrWriteQueueTimeout = errors.New("sqlite: timed out waiting for the database write queue")

// writeGate 让同一连接池上的写事务与自动提交写语句按到达顺序逐个开始。
// SQLite 的 busy handler 是定时轮询而非排队，多个写者竞争时个别写者可能
// 反复错过锁直到 busy_timeout；先在进程内排队后，同一时刻只有一个写者
// 向 SQLite 取锁，busy_timeout 只兜底其他进程或绕过本包的连接。
type writeGate struct {
	sem  *semaphore.Weighted // x/sync 的信号量按请求顺序唤醒
	wait time.Duration
}

func newWriteGate(wait time.Duration) *writeGate {
	if wait <= 0 {
		wait = 15 * time.Second
	}
	return &writeGate{sem: semaphore.NewWeighted(1), wait: wait}
}

// acquire 在队列中等待写入资格。等待超过上限返回 ErrWriteQueueTimeout，
// 使嵌套取锁等编程错误仍以可分类的 busy 结束，而不是无限等待。
func (g *writeGate) acquire(ctx context.Context) (func(), error) {
	c, cancel := context.WithTimeout(ctx, g.wait)
	defer cancel()
	if err := g.sem.Acquire(c, 1); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrWriteQueueTimeout
	}
	var once sync.Once
	return func() { once.Do(func() { g.sem.Release(1) }) }, nil
}

// registeredDriver 是以 DriverName 注册的 modernc 驱动；sql.Open 不建立连接。
var registeredDriver = sync.OnceValue(func() driver.Driver {
	db, err := sql.Open(DriverName, "")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	return db.Driver()
})

// innerConn 是写入闸门转发的驱动连接能力；modernc 连接全部实现。
type innerConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

type gatedConnector struct {
	dsn  string
	gate *writeGate
}

func (c *gatedConnector) Connect(context.Context) (driver.Conn, error) {
	raw, err := registeredDriver().Open(c.dsn)
	if err != nil {
		return nil, err
	}
	inner, ok := raw.(innerConn)
	if !ok {
		raw.Close()
		return nil, errors.New("sqlite: driver connection lacks context interfaces")
	}
	return &gatedConn{innerConn: inner, gate: c.gate}, nil
}

func (c *gatedConnector) Driver() driver.Driver { return registeredDriver() }

// gatedConn 由 database/sql 串行使用，不需要自身加锁。
type gatedConn struct {
	innerConn
	gate    *writeGate
	release func() // 非 nil 表示本连接上有进行中的写事务
}

func (c *gatedConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *gatedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if opts.ReadOnly || c.release != nil { // 只读事务用普通 BEGIN，不取写锁
		return c.innerConn.BeginTx(ctx, opts)
	}
	release, err := c.gate.acquire(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := c.innerConn.BeginTx(ctx, opts)
	if err != nil {
		release()
		return nil, err
	}
	c.release = release
	return &gatedTx{Tx: tx, c: c}, nil
}

// ExecContext 让事务外的写语句同样排队。VACUUM INTO 只读取本库，不排队，
// 避免备份复制期间挡住写者。
func (c *gatedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.release != nil || vacuumInto(query) {
		return c.innerConn.ExecContext(ctx, query, args)
	}
	release, err := c.gate.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return c.innerConn.ExecContext(ctx, query, args)
}

// QueryContext 让事务外的写语句（如 INSERT … RETURNING）排队，并持有写入
// 资格直到结果集关闭：SQLite 在语句步进结束前一直持有写锁。读查询不排队。
func (c *gatedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.release != nil || !modifies(query) {
		return c.innerConn.QueryContext(ctx, query, args)
	}
	return queueRows(ctx, c.gate, func() (driver.Rows, error) { return c.innerConn.QueryContext(ctx, query, args) })
}

// PrepareContext 返回的语句在事务外执行时同样排队（规则与连接级调用一致）。
func (c *gatedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	st, err := c.innerConn.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	inner, ok := st.(innerStmt)
	if !ok {
		st.Close()
		return nil, errors.New("sqlite: driver statement lacks context interfaces")
	}
	return &gatedStmt{innerStmt: inner, c: c, query: query}, nil
}

func (c *gatedConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *gatedConn) Close() error {
	c.done()
	return c.innerConn.Close()
}

func (c *gatedConn) done() {
	if c.release != nil {
		c.release()
		c.release = nil
	}
}

func vacuumInto(query string) bool {
	f := strings.Fields(query)
	return len(f) > 1 && strings.EqualFold(f[0], "VACUUM") && strings.EqualFold(f[1], "INTO")
}

// modifies 报告查询是否可能写库：跳过前导空白与注释后，首个关键字为 INSERT、
// UPDATE、DELETE 或 REPLACE，或以 WITH 开头且含这些关键字。误判为写只多排
// 一次队，漏判则退回 busy_timeout。
func modifies(query string) bool {
	q := strings.TrimSpace(query)
	for {
		switch {
		case strings.HasPrefix(q, "--"):
			if i := strings.IndexByte(q, '\n'); i >= 0 {
				q = strings.TrimSpace(q[i+1:])
				continue
			}
			return false
		case strings.HasPrefix(q, "/*"):
			if i := strings.Index(q, "*/"); i >= 0 {
				q = strings.TrimSpace(q[i+2:])
				continue
			}
			return false
		}
		break
	}
	words := strings.FieldsFunc(strings.ToUpper(q), func(r rune) bool { return !(r == '_' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') })
	write := func(w string) bool { return w == "INSERT" || w == "UPDATE" || w == "DELETE" || w == "REPLACE" }
	if len(words) == 0 {
		return false
	}
	if words[0] != "WITH" {
		return write(words[0])
	}
	return slices.ContainsFunc(words, write)
}

// queueRows 在写入队列中取得资格后执行查询，结果集关闭时释放。
func queueRows(ctx context.Context, g *writeGate, query func() (driver.Rows, error)) (driver.Rows, error) {
	release, err := g.acquire(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := query()
	if err != nil {
		release()
		return nil, err
	}
	return &gatedRows{Rows: rows, release: release}, nil
}

type gatedRows struct {
	driver.Rows
	release func()
}

func (r *gatedRows) Close() error {
	defer r.release()
	return r.Rows.Close()
}

// innerStmt 是预编译语句转发的驱动能力；modernc 语句全部实现。
type innerStmt interface {
	driver.Stmt
	driver.StmtExecContext
	driver.StmtQueryContext
}

// gatedStmt 在所属连接没有进行中的写事务时，按连接级调用的规则排队。
type gatedStmt struct {
	innerStmt
	c     *gatedConn
	query string
}

func (s *gatedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if s.c.release != nil || vacuumInto(s.query) {
		return s.innerStmt.ExecContext(ctx, args)
	}
	release, err := s.c.gate.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.innerStmt.ExecContext(ctx, args)
}

func (s *gatedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if s.c.release != nil || !modifies(s.query) {
		return s.innerStmt.QueryContext(ctx, args)
	}
	return queueRows(ctx, s.c.gate, func() (driver.Rows, error) { return s.innerStmt.QueryContext(ctx, args) })
}

// Exec 与 Query 是旧接口；database/sql 优先使用带上下文的版本。
func (s *gatedStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), named(args))
}

func (s *gatedStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), named(args))
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

// gatedTx 在提交或回滚返回后释放写入资格。提交失败时驱动会回滚仍打开的事务；
// 个别情况下连接仍持锁，后续写者由 busy_timeout 兜底。
type gatedTx struct {
	driver.Tx
	c *gatedConn
}

func (t *gatedTx) Commit() error {
	defer t.c.done()
	return t.Tx.Commit()
}

func (t *gatedTx) Rollback() error {
	defer t.c.done()
	return t.Tx.Rollback()
}
