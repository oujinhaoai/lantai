package application

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedBuffer) entries(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.String()), "\n") {
		if line == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		out = append(out, e)
	}
	return out
}

var backgroundFields = map[string]bool{"time": true, "event": true, "task": true, "status": true, "stage": true,
	"error_code": true, "error_cause": true, "consecutive_failures": true, "failing_seconds": true}

// 后台同步失败只记白名单字段：错误文本里的路径与对象 ID 不进日志。同一失败
// 每分钟最多一条，阶段或错误码变化立即记录，恢复时记一条。
func TestBackgroundLogIsWhitelistedAndRateLimited(t *testing.T) {
	buf := &lockedBuffer{}
	l := newBackgroundLog(buf)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	at := func(d time.Duration) { now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC).Add(d) }

	l.observe(taskInboxAudit, nil) // 一直正常时不记录
	disk := atStage(stageAuditExport, fmt.Errorf("open /private/data-root/audit/events.jsonl: %w", fs.ErrPermission))
	l.observe(taskInboxAudit, disk)
	at(10 * time.Second)
	l.observe(taskInboxAudit, disk) // 同一失败，不到一分钟
	at(61 * time.Second)
	l.observe(taskInboxAudit, disk) // 超过一分钟，再记一条
	at(62 * time.Second)
	inner := errcode.New(errcode.Forbidden, "asset 01K0SECRETASSETID00000000 is not readable")
	denied := atStage(stageInbox, inner)
	l.observe(taskInboxAudit, denied) // 阶段与错误码变化，立即记录
	l.observe(taskOutboxQuery, atStage(stageRelay+"ledger", disk))
	at(70 * time.Second)
	l.observe(taskInboxAudit, nil)
	l.observe(taskInboxAudit, nil) // 已恢复，不再重复

	got := buf.entries(t)
	type row struct {
		task, status, stage, code, cause string
		failures                         float64
	}
	want := []row{
		{taskInboxAudit, "failing", stageAuditExport, "INTERNAL", "other", 1},
		{taskInboxAudit, "failing", stageAuditExport, "INTERNAL", "other", 3},
		{taskInboxAudit, "failing", stageInbox, "FORBIDDEN", "other", 4},
		{taskOutboxQuery, "failing", "relay_ledger", "INTERNAL", "other", 1},
		{taskInboxAudit, "recovered", "", "", "", 4},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		e := got[i]
		s := func(k string) string { v, _ := e[k].(string); return v }
		if e["event"] != "background_sync" || s("task") != w.task || s("status") != w.status || s("stage") != w.stage || s("error_code") != w.code || s("error_cause") != w.cause || e["consecutive_failures"] != w.failures {
			t.Errorf("entry %d = %v, want %+v", i, e, w)
		}
		for k := range e {
			if !backgroundFields[k] {
				t.Errorf("entry %d has field %q outside the whitelist", i, k)
			}
		}
	}
	if got[4]["failing_seconds"] != 70.0 {
		t.Errorf("recovery should report 70 s of failure: %v", got[4])
	}
	raw := buf.String()
	for _, secret := range []string{"/private/data-root", "events.jsonl", "01K0SECRETASSETID", "not readable"} {
		if strings.Contains(raw, secret) {
			t.Errorf("log leaked %q: %s", secret, raw)
		}
	}
	// 阶段包装不改变错误文本、错误码与 errors.Is 判断。
	if denied.Error() != inner.Error() || errcode.CodeOf(denied) != errcode.Forbidden {
		t.Errorf("stage wrapper changed the error: %q %s", denied.Error(), errcode.CodeOf(denied))
	}
	if !strings.Contains(disk.Error(), "events.jsonl") || !errors.Is(disk, fs.ErrPermission) {
		t.Errorf("stage wrapper hid the cause: %v", disk)
	}
	newBackgroundLog(nil).observe(taskInboxAudit, disk) // 未配置输出时不记录也不出错
	var nilLog *backgroundLog
	nilLog.observe(taskInboxAudit, disk)
}

// 真实实例中审计导出持续失败时，就绪状态变为否并写出带阶段的诊断；修复后
// 下一轮恢复，记一条恢复日志。
func TestBackgroundSyncFailureIsDiagnosedAndRecovers(t *testing.T) {
	buf := &lockedBuffer{}
	a := openTestApp(t, func(o *Options) { o.Diagnostics = buf })
	audit := filepath.Join(a.Instance.Layout().Home, "audit", "events.jsonl")
	aside := audit + ".aside"
	// 持有 tailMu 换文件，避免与正在进行的导出交错（Windows 不能改名打开中的文件）。
	a.tailMu.Lock()
	_, statErr := os.Stat(audit)
	if statErr == nil {
		if err := os.Rename(audit, aside); err != nil {
			a.tailMu.Unlock()
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(audit, 0o700); err != nil { // 同名目录使导出无法打开文件
		a.tailMu.Unlock()
		t.Fatal(err)
	}
	a.tailMu.Unlock()

	waitFor := func(what string, ok func([]map[string]any) bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if ok(buf.entries(t)) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("%s not logged: %s", what, buf.String())
	}
	waitFor("audit export failure", func(es []map[string]any) bool {
		for _, e := range es {
			if e["task"] == taskInboxAudit && e["status"] == "failing" && e["stage"] == stageAuditExport && e["error_code"] == "INTERNAL" {
				return true
			}
		}
		return false
	})
	if a.Ready() {
		t.Fatal("instance stayed ready while the audit export kept failing")
	}

	a.tailMu.Lock()
	err := os.Remove(audit)
	if err == nil && statErr == nil {
		err = os.Rename(aside, audit)
	}
	a.tailMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	waitFor("recovery", func(es []map[string]any) bool {
		for _, e := range es {
			if e["task"] == taskInboxAudit && e["status"] == "recovered" {
				return true
			}
		}
		return false
	})
	deadline := time.Now().Add(10 * time.Second)
	for !a.Ready() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !a.Ready() {
		t.Fatal("instance did not become ready again after the audit export recovered")
	}
	if raw := buf.String(); strings.Contains(raw, a.Instance.Layout().Home) {
		t.Fatalf("log leaked the data root: %s", raw)
	}
}
