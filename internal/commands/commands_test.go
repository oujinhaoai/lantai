package commands

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
)

type fixture struct {
	db    *sql.DB
	clk   *clock.Fake
	gen   *ids.Generator
	store *Store
	actor ids.ID
	proj  ids.ID
}

func newFixture(t *testing.T, module, file string) *fixture {
	t.Helper()
	db, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), file), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := EnsureSchema(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE `+module+`_items (id TEXT PRIMARY KEY, v TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC))
	gen := &ids.Generator{Clock: clk, Rand: rngReader()}
	st, err := NewStore(module, clk)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{db: db, clk: clk, gen: gen, store: st, actor: gen.MustNew(), proj: gen.MustNew()}
}

// rngReader 提供确定但不重复的熵。
func rngReader() *countingReader { return &countingReader{} }

type countingReader struct{ n uint64 }

func (r *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		r.n++
		p[i] = byte(r.n*2654435761>>13) ^ byte(r.n)
	}
	return len(p), nil
}

func (f *fixture) cmd(t *testing.T, key string, body string) Context {
	t.Helper()
	h, err := RequestHash(HashInput{CommandType: f.store.Module() + ".put_item", ProjectID: f.proj, Body: json.RawMessage(body)})
	if err != nil {
		t.Fatal(err)
	}
	return Context{
		OperationID: f.gen.MustNew(), IdempotencyKey: key, RequestHash: h,
		CommandType: f.store.Module() + ".put_item", ActorID: f.actor, ProjectID: f.proj, RecoveryEpoch: 1,
	}
}

func (f *fixture) putItem(id, v string, events ...event.Envelope) Handler {
	return func(ctx context.Context, tx *sql.Tx) (Result, error) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+f.store.Module()+`_items (id, v) VALUES (?, ?)`, id, v); err != nil {
			return Result{}, err
		}
		return Result{Status: ReceiptSucceeded, ResponseCode: 201, Summary: map[string]string{"id": id},
			ResultRefs: []ResultRef{{Kind: "item", ID: ids.New()}}, Events: events}, nil
	}
}

func (f *fixture) event(t *testing.T, op ids.ID, aggregate ids.ID) event.Envelope {
	t.Helper()
	e, err := event.New(f.gen, f.clk, event.Params{
		EventType: "version.created", SchemaVersion: 1, AggregateType: "item", AggregateID: aggregate,
		AggregateRevision: 1, ActorID: f.actor, ProjectID: f.proj, OperationID: op,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRequestHash(t *testing.T) {
	base := HashInput{CommandType: "ledger.commit_version", Targets: []string{"a"}, ExpectedRevisions: map[string]int64{"a": 3},
		Body: json.RawMessage(`{"b":1,"a":[1,2]}`)}
	h1, err := RequestHash(base)
	if err != nil {
		t.Fatal(err)
	}
	reordered := base
	reordered.Body = json.RawMessage("{ \"a\" : [1.0, 2], \"b\": 1 }")
	if h2, _ := RequestHash(reordered); h2 != h1 {
		t.Fatal("formatting and key order must not change the hash")
	}
	for name, mutate := range map[string]func(*HashInput){
		"body":      func(h *HashInput) { h.Body = json.RawMessage(`{"b":2,"a":[1,2]}`) },
		"targets":   func(h *HashInput) { h.Targets = []string{"b"} },
		"revisions": func(h *HashInput) { h.ExpectedRevisions = map[string]int64{"a": 4} },
		"inputs":    func(h *HashInput) { h.InputVersions = []string{"v"} },
		"command":   func(h *HashInput) { h.CommandType = "ledger.other" },
		"project":   func(h *HashInput) { h.ProjectID = ids.MustParse("01J8Z3K4M5N6P7Q8R9S0T1V2W3") },
		"null body": func(h *HashInput) { h.Body = nil },
	} {
		m := base
		mutate(&m)
		if h, _ := RequestHash(m); h == h1 {
			t.Errorf("%s change did not change the hash", name)
		}
	}
	if _, err := RequestHash(HashInput{CommandType: "Bad", Body: json.RawMessage(`{}`)}); err == nil {
		t.Error("invalid command type accepted")
	}
	if _, err := RequestHash(HashInput{CommandType: "a.b", Body: json.RawMessage(`{"a":1,"a":2}`)}); err == nil {
		t.Error("duplicate keys in body accepted")
	}
	// 非法 UTF-8 不能被 encoding/json 静默替换成 U+FFFD 后与其他请求撞摘要。
	for name, in := range map[string]HashInput{
		"target":   {CommandType: "a.b", Targets: []string{"a\xff"}},
		"revision": {CommandType: "a.b", ExpectedRevisions: map[string]int64{"k\xfe": 1}},
		"input":    {CommandType: "a.b", InputVersions: []string{"\xc0"}},
		"body":     {CommandType: "a.b", Body: json.RawMessage("\"\xff\"")},
	} {
		if _, err := RequestHash(in); err == nil {
			t.Errorf("%s with invalid UTF-8 accepted", name)
		}
	}
	if !h1.Valid() {
		t.Fatal("hash format")
	}
}

func TestContextValidate(t *testing.T) {
	f := newFixture(t, "ledger", "l.db")
	good := f.cmd(t, "k1", `{}`)
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Context){
		"op id":       func(c *Context) { c.OperationID = "x" },
		"key":         func(c *Context) { c.IdempotencyKey = "has space" },
		"hash":        func(c *Context) { c.RequestHash = "sha256:zz" },
		"command":     func(c *Context) { c.CommandType = "Commit" },
		"actor":       func(c *Context) { c.ActorID = "" },
		"epoch":       func(c *Context) { c.RecoveryEpoch = 0 },
		"fence alone": func(c *Context) { c.LeaseFence = 3 },
		"negative":    func(c *Context) { c.ExpectedRevisions = map[string]int64{"a": -1} },
	} {
		c := good
		mutate(&c)
		if err := c.Validate(); !errors.Is(err, ErrInvalidContext) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestStageTransitions(t *testing.T) {
	for _, ok := range [][2]Stage{
		{StageReceiving, StagePrepared}, {StagePrepared, StageInstalled}, {StageInstalled, StageCommitted},
		{StagePrepared, StageCommitted}, {StageInstalled, StageQuarantined}, {StageBlocked, StageCommitted},
		{StageQuarantined, StageFailed},
	} {
		if !CanTransition(ok[0], ok[1]) {
			t.Errorf("%s -> %s should be allowed", ok[0], ok[1])
		}
	}
	for _, bad := range [][2]Stage{
		{StageCommitted, StagePrepared}, {StageCommitted, StageFailed}, {StageFailed, StageCommitted},
		{StageInstalled, StagePrepared}, {StageQuarantined, StageCommitted}, {StagePrepared, StageProjected},
		{StageReceiving, StageInstalled},
	} {
		if CanTransition(bad[0], bad[1]) {
			t.Errorf("%s -> %s should be rejected", bad[0], bad[1])
		}
	}
	if StageProjected.Valid() || !StageBlocked.Valid() {
		t.Fatal("projected is a view-only stage")
	}
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := digest.Of([]byte("a"))
	done := &Receipt{RequestHash: h, Status: ReceiptSucceeded, ResponseExpiresAt: now.Add(time.Hour)}
	cases := []struct {
		r    *Receipt
		hash digest.Digest
		now  time.Time
		want Outcome
	}{
		{nil, h, now, OutcomeExecute},
		{done, h, now, OutcomeReplay},
		{done, digest.Of([]byte("b")), now, OutcomeConflict},
		{done, h, now.Add(time.Hour), OutcomeExpired},
		{&Receipt{RequestHash: h, Status: ReceiptInProgress}, h, now, OutcomeInProgress},
		{&Receipt{RequestHash: h, Status: ReceiptInProgress}, digest.Of([]byte("b")), now, OutcomeConflict},
		{&Receipt{RequestHash: h, Status: ReceiptFailed, ResponseExpiresAt: now.Add(time.Hour)}, h, now, OutcomeReplay},
	}
	for i, c := range cases {
		if got := Decide(c.r, c.hash, c.now); got != c.want {
			t.Errorf("case %d: %s, want %s", i, got, c.want)
		}
	}
	if e := OutcomeConflict.Err(done); e.Code != errcode.IdempotencyConflict {
		t.Fatal(e)
	}
	if OutcomeReplay.Err(done) != nil || OutcomeInProgress.Err(done) != nil {
		t.Fatal("replay and in-progress are not errors")
	}
}

func TestExecuteIdempotency(t *testing.T) {
	f := newFixture(t, "ledger", "ledger.db")
	ctx := t.Context()
	cmd := f.cmd(t, "put-1", `{"id":"a"}`)
	ev := f.event(t, cmd.OperationID, f.gen.MustNew())

	resp, err := f.store.Execute(ctx, f.db, cmd, f.putItem("a", "1", ev))
	if err != nil || resp.Outcome != OutcomeExecute || resp.Receipt.Status != ReceiptSucceeded {
		t.Fatalf("first execute: %+v %v", resp, err)
	}

	// 响应丢失后同键同摘要重试：返回原结果，不再执行。
	retry := cmd
	retry.OperationID = f.gen.MustNew() // 服务端为新到达的请求分配的 ID 不会被使用
	resp2, err := f.store.Execute(ctx, f.db, retry, f.putItem("a", "1"))
	if err != nil || resp2.Outcome != OutcomeReplay || resp2.Receipt.OperationID != cmd.OperationID {
		t.Fatalf("replay: %+v %v", resp2, err)
	}
	if string(resp2.Receipt.ResponseSummary) != `{"id":"a"}` || resp2.Receipt.ResponseCode != 201 {
		t.Fatalf("replayed response: %s %d", resp2.Receipt.ResponseSummary, resp2.Receipt.ResponseCode)
	}

	// 同键不同摘要：冲突且无副作用。
	conflict := f.cmd(t, "put-1", `{"id":"b"}`)
	_, err = f.store.Execute(ctx, f.db, conflict, f.putItem("b", "2"))
	if errcode.CodeOf(err) != errcode.IdempotencyConflict {
		t.Fatalf("conflict: %v", err)
	}

	// 超过响应缓存期：不当新请求执行；错误不附带未经权限过滤的结果引用。
	f.clk.Advance(ResponseTTL)
	resp3, err := f.store.Execute(ctx, f.db, retry, f.putItem("a", "1"))
	if errcode.CodeOf(err) != errcode.IdempotencyResultExpired {
		t.Fatalf("expired: %v", err)
	}
	if e, _ := errcode.As(err); len(e.Refs) != 0 || e.OperationID != string(cmd.OperationID) {
		t.Fatalf("expired error refs=%v op=%s", e.Refs, e.OperationID)
	}
	if resp3.Outcome != OutcomeExpired || len(resp3.Receipt.ResultRefs) != 1 {
		t.Fatalf("caller still gets the receipt to filter by permission: %+v", resp3)
	}

	if n := count(t, f.db, `SELECT count(*) FROM ledger_items`); n != 1 {
		t.Fatalf("items = %d, want exactly one business effect", n)
	}
	if n := count(t, f.db, `SELECT count(*) FROM outbox`); n != 1 {
		t.Fatalf("outbox = %d", n)
	}
	if n := count(t, f.db, `SELECT count(*) FROM command_receipts`); n != 1 {
		t.Fatalf("receipts = %d", n)
	}
}

func TestExecuteFailureSemantics(t *testing.T) {
	f := newFixture(t, "ledger", "ledger.db")
	ctx := t.Context()

	// 无效请求：handler 返回 error，事务回滚，不占键。
	cmd := f.cmd(t, "k", `{"id":"a"}`)
	boom := errcode.New(errcode.SchemaInvalid, "bad")
	if _, err := f.store.Execute(ctx, f.db, cmd, func(ctx context.Context, tx *sql.Tx) (Result, error) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_items VALUES ('ghost', 'x')`); err != nil {
			return Result{}, err
		}
		return Result{}, boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if n := count(t, f.db, `SELECT count(*) FROM ledger_items`); n != 0 {
		t.Fatal("business write survived a rejected request")
	}
	if r, _ := f.store.LookupReceipt(ctx, f.db, cmd.Key()); r != nil {
		t.Fatal("rejected request must not occupy the key")
	}
	// 同键修正后可以执行。
	if _, err := f.store.Execute(ctx, f.db, cmd, f.putItem("a", "1")); err != nil {
		t.Fatal(err)
	}

	// 已接受的业务失败：写终态回执，同键重放同一失败。
	cmd2 := f.cmd(t, "k2", `{"id":"b"}`)
	failing := func(ctx context.Context, tx *sql.Tx) (Result, error) {
		// ResponseCode 为 0 时取错误码登记的 HTTP 状态。
		return Result{Status: ReceiptFailed, FailureCode: errcode.BaseVersionConflict}, nil
	}
	if _, err := f.store.Execute(ctx, f.db, cmd2, failing); err != nil {
		t.Fatal(err)
	}
	resp, err := f.store.Execute(ctx, f.db, cmd2, f.putItem("b", "2"))
	if err != nil || resp.Outcome != OutcomeReplay || resp.Receipt.FailureCode != errcode.BaseVersionConflict || resp.Receipt.ResponseCode != 409 {
		t.Fatalf("failed receipt replay: %+v %v", resp, err)
	}

	// 事件与回执同事务：outbox 写入失败时业务写入也回滚。
	cmd3 := f.cmd(t, "k3", `{"id":"c"}`)
	foreign := f.event(t, f.gen.MustNew(), f.gen.MustNew()) // 属于别的 operation
	if _, err := f.store.Execute(ctx, f.db, cmd3, f.putItem("c", "3", foreign)); err == nil {
		t.Fatal("event of another operation accepted")
	}
	if n := count(t, f.db, `SELECT count(*) FROM ledger_items WHERE id = 'c'`); n != 0 {
		t.Fatal("business write committed without its outbox event")
	}

	// 非本模块命令拒绝。
	other := f.cmd(t, "k4", `{}`)
	other.CommandType = "runtime.put_item"
	if _, err := f.store.Execute(ctx, f.db, other, f.putItem("d", "4")); !errors.Is(err, ErrForeignCommand) {
		t.Fatalf("foreign command: %v", err)
	}
	for _, bad := range []Result{
		{Status: "done", ResponseCode: 200},
		{Status: ReceiptSucceeded, ResponseCode: 99},
		{Status: ReceiptSucceeded, ResponseCode: 409},
		{Status: ReceiptFailed, ResponseCode: 409},
		{Status: ReceiptFailed, ResponseCode: 200, FailureCode: errcode.BaseVersionConflict},
		{Status: ReceiptFailed, ResponseCode: 422, FailureCode: errcode.BaseVersionConflict},
		{Status: ReceiptSucceeded, ResponseCode: 200, FailureCode: errcode.Internal},
		{Status: ReceiptSucceeded, ResponseCode: 200, Summary: strings.Repeat("x", MaxSummaryBytes)},
	} {
		c := f.cmd(t, "bad-"+f.gen.MustNew().String(), `{}`)
		if _, err := f.store.Execute(ctx, f.db, c, func(context.Context, *sql.Tx) (Result, error) { return bad, nil }); err == nil {
			t.Errorf("invalid result accepted: %+v", bad.Status)
		}
	}
}

func TestExecuteConcurrentSameKey(t *testing.T) {
	f := newFixture(t, "ledger", "ledger.db")
	var executed atomic.Int32
	var wg sync.WaitGroup
	cmds := make([]Context, 8)
	for i := range cmds {
		cmds[i] = f.cmd(t, "same", `{"id":"x"}`)
	}
	outcomes := make([]Outcome, len(cmds))
	errs := make([]error, len(cmds))
	for i := range cmds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := f.store.Execute(context.Background(), f.db, cmds[i], func(ctx context.Context, tx *sql.Tx) (Result, error) {
				executed.Add(1)
				return f.putItem("x", "1")(ctx, tx)
			})
			outcomes[i], errs[i] = resp.Outcome, err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if executed.Load() != 1 {
		t.Fatalf("handler ran %d times", executed.Load())
	}
	replays := 0
	for _, o := range outcomes {
		if o == OutcomeReplay {
			replays++
		}
	}
	if replays != len(cmds)-1 {
		t.Fatalf("outcomes %v", outcomes)
	}
}

func TestLongOperationLifecycle(t *testing.T) {
	f := newFixture(t, "ledger", "ledger.db")
	ctx := t.Context()
	cmd := f.cmd(t, "commit-1", `{"manifest":"m"}`)
	reserved := 0
	resp, err := f.store.Accept(ctx, f.db, cmd, StagePrepared, []string{"asset-a"}, func(ctx context.Context, tx *sql.Tx) error {
		reserved++
		return nil
	})
	if err != nil || resp.Outcome != OutcomeExecute || resp.Receipt.Status != ReceiptInProgress {
		t.Fatalf("accept: %+v %v", resp, err)
	}
	// prepared 期间重试：202，同一 operation，不再保留第二个版本号。
	resp, err = f.store.Accept(ctx, f.db, f.withOp(cmd), StagePrepared, nil, func(context.Context, *sql.Tx) error {
		reserved++
		return nil
	})
	if err != nil || resp.Outcome != OutcomeInProgress || resp.Receipt.OperationID != cmd.OperationID || reserved != 1 {
		t.Fatalf("retry while prepared: %+v %v reserved=%d", resp, err, reserved)
	}
	v, err := f.store.View(ctx, f.db, cmd.OperationID)
	if err != nil || v.Stage != StagePrepared || !v.Retryable || v.NextAction != errcode.ActionPollOperation {
		t.Fatalf("prepared view: %+v %v", v, err)
	}

	md := digest.Of([]byte("manifest"))
	if err := f.store.Advance(ctx, f.db, cmd.OperationID, StagePrepared, StageInstalled, Update{ManifestDigest: md, IntendedPaths: []string{"a.png"}}); err != nil {
		t.Fatal(err)
	}
	// 并发推进同一阶段只有一个成功。
	if err := f.store.Advance(ctx, f.db, cmd.OperationID, StagePrepared, StageInstalled, Update{}); !errors.Is(err, ErrStageConflict) {
		t.Fatalf("stale advance: %v", err)
	}
	if err := f.store.Advance(ctx, f.db, cmd.OperationID, StageInstalled, StageCommitted, Update{}); err == nil {
		t.Fatal("Advance must not finish operations")
	}
	other, _ := NewStore("runtime", f.clk)
	if err := other.Advance(ctx, f.db, cmd.OperationID, StageInstalled, StageBlocked, Update{}); !errors.Is(err, ErrStageConflict) {
		t.Fatalf("another module advanced a ledger operation: %v", err)
	}

	versionID := f.gen.MustNew()
	ev := f.event(t, cmd.OperationID, versionID)
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_items VALUES (?, 'committed version')`, versionID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Complete(ctx, tx, cmd.OperationID, StageInstalled, Result{
		Status: ReceiptSucceeded, ResponseCode: 201, ResultRefs: []ResultRef{{Kind: "version", ID: versionID, Revision: 1}}, Events: []event.Envelope{ev},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	op, _ := f.store.GetOperation(ctx, f.db, cmd.OperationID)
	if op.Stage != StageCommitted || op.ManifestDigest != md || len(op.IntendedPaths) != 1 {
		t.Fatalf("operation: %+v", op)
	}
	resp, err = f.store.Accept(ctx, f.db, f.withOp(cmd), StagePrepared, nil, nil)
	if err != nil || resp.Outcome != OutcomeReplay || resp.Receipt.ResultRefs[0].ID != versionID {
		t.Fatalf("replay after commit: %+v %v", resp, err)
	}

	v, _ = f.store.View(ctx, f.db, cmd.OperationID)
	if v.Stage != StageCommitted || v.ProjectionPending == nil || !*v.ProjectionPending {
		t.Fatalf("committed view before relay: %+v", v)
	}
	assertViewValid(t, v)
	recs, err := ReadUndelivered(ctx, f.db, 0, 10)
	if err != nil || len(recs) != 1 || recs[0].EventID != ev.EventID {
		t.Fatalf("outbox: %+v %v", recs, err)
	}
	if _, err := event.Parse(recs[0].Envelope); err != nil {
		t.Fatalf("stored envelope invalid: %v", err)
	}
	if n, _ := MarkDelivered(ctx, f.db, []ids.ID{ev.EventID}, f.clk.Now()); n != 1 {
		t.Fatal("mark delivered")
	}
	if n, _ := MarkDelivered(ctx, f.db, []ids.ID{ev.EventID}, f.clk.Now()); n != 0 {
		t.Fatal("mark delivered must be idempotent")
	}
	v, _ = f.store.View(ctx, f.db, cmd.OperationID)
	if v.Stage != StageProjected || *v.ProjectionPending {
		t.Fatalf("view after relay: %+v", v)
	}
	assertViewValid(t, v)
	if open, _ := f.store.OpenOperations(ctx, f.db); len(open) != 0 {
		t.Fatalf("open operations: %v", open)
	}
}

func (f *fixture) withOp(c Context) Context {
	c.OperationID = f.gen.MustNew()
	return c
}

func assertViewValid(t *testing.T, v *View) {
	t.Helper()
	reg, err := schema.Default()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(v)
	if err := reg.ValidateJSON("lantai.operation/v1", data); err != nil {
		t.Fatalf("view does not satisfy lantai.operation/v1: %v\n%s", err, data)
	}
}

func TestCancelAndBlockedViews(t *testing.T) {
	f := newFixture(t, "ledger", "ledger.db")
	ctx := t.Context()
	cmd := f.cmd(t, "c1", `{}`)
	if _, err := f.store.Accept(ctx, f.db, cmd, StagePrepared, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Advance(ctx, f.db, cmd.OperationID, StagePrepared, StageBlocked, Update{FailureCode: errcode.Forbidden}); err != nil {
		t.Fatal(err)
	}
	v, _ := f.store.View(ctx, f.db, cmd.OperationID)
	if v.Stage != StageBlocked || v.NextAction != errcode.ActionHumanAction || v.Reason == nil || v.Reason.Code != errcode.Forbidden {
		t.Fatalf("blocked view: %+v", v)
	}
	assertViewValid(t, v)
	if open, _ := f.store.OpenOperations(ctx, f.db); len(open) != 1 {
		t.Fatalf("open operations: %v", open)
	}
	tx, _ := f.db.BeginTx(ctx, nil)
	if err := f.store.Cancel(ctx, tx, cmd.OperationID, StageBlocked, ""); err == nil {
		t.Fatal("cancel without a registered reason accepted")
	}
	if err := f.store.Cancel(ctx, tx, cmd.OperationID, StageBlocked, errcode.Forbidden); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	v, _ = f.store.View(ctx, f.db, cmd.OperationID)
	if v.Stage != StageCancelled || v.Retryable {
		t.Fatalf("cancelled view: %+v", v)
	}
	assertViewValid(t, v)
	resp, err := f.store.Accept(ctx, f.db, f.withOp(cmd), StagePrepared, nil, nil)
	if err != nil || resp.Outcome != OutcomeReplay || resp.Receipt.Status != ReceiptFailed ||
		resp.Receipt.ResponseCode != 403 || resp.Receipt.FailureCode != errcode.Forbidden {
		t.Fatalf("replay after cancel: %+v %v", resp, err)
	}
	if _, err := f.store.View(ctx, f.db, f.gen.MustNew()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown operation: %v", err)
	}
	// 短命令只有回执：视图由回执推导。
	short := f.cmd(t, "short", `{"id":"s"}`)
	if _, err := f.store.Execute(ctx, f.db, short, f.putItem("s", "1")); err != nil {
		t.Fatal(err)
	}
	v, _ = f.store.View(ctx, f.db, short.OperationID)
	if v.Stage != StageProjected || v.CommandType != "ledger.put_item" {
		t.Fatalf("short command view: %+v", v)
	}
	assertViewValid(t, v)
}

// 回执随业务所属库：两个库各自有回执，同一幂等键互不影响，没有全局回执库。
func TestReceiptsLiveInOwningDatabase(t *testing.T) {
	ledger := newFixture(t, "ledger", "ledger.db")
	runtime := newFixture(t, "runtime", "runtime.db")
	runtime.actor, runtime.proj = ledger.actor, ledger.proj
	ctx := t.Context()
	if _, err := ledger.store.Execute(ctx, ledger.db, ledger.cmd(t, "shared-key", `{"id":"a"}`), ledger.putItem("a", "1")); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.store.Execute(ctx, runtime.db, runtime.cmd(t, "shared-key", `{"id":"a"}`), runtime.putItem("a", "1")); err != nil {
		t.Fatal(err)
	}
	if count(t, ledger.db, `SELECT count(*) FROM command_receipts WHERE owner_module = 'runtime'`) != 0 ||
		count(t, runtime.db, `SELECT count(*) FROM command_receipts WHERE owner_module = 'ledger'`) != 0 {
		t.Fatal("receipts leaked across databases")
	}
	if _, err := NewStore("Bad Module", nil); err == nil {
		t.Fatal("invalid module accepted")
	}
}

func TestOutboxBacklogAndOrder(t *testing.T) {
	f := newFixture(t, "ledger", "ledger.db")
	ctx := t.Context()
	var evs []ids.ID
	for i := range 3 {
		cmd := f.cmd(t, "o"+string(rune('a'+i)), `{}`)
		ev := f.event(t, cmd.OperationID, f.gen.MustNew())
		evs = append(evs, ev.EventID)
		f.clk.Advance(time.Second)
		if _, err := f.store.Execute(ctx, f.db, cmd, f.putItem("i"+string(rune('a'+i)), "v", ev)); err != nil {
			t.Fatal(err)
		}
	}
	n, oldest, err := OutboxBacklog(ctx, f.db)
	if err != nil || n != 3 || oldest.IsZero() {
		t.Fatalf("backlog %d %v %v", n, oldest, err)
	}
	recs, _ := ReadUndelivered(ctx, f.db, 0, 2)
	if len(recs) != 2 || recs[0].EventID != evs[0] || recs[1].EventID != evs[1] {
		t.Fatalf("order: %+v", recs)
	}
	more, _ := ReadUndelivered(ctx, f.db, recs[1].Seq, 10)
	if len(more) != 1 || more[0].EventID != evs[2] {
		t.Fatalf("paging: %+v", more)
	}
	if _, err := ReadUndelivered(ctx, f.db, 0, 0); err == nil {
		t.Fatal("limit 0 accepted")
	}
}
