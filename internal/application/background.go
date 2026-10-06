package application

import (
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
)

// 后台同步任务与阶段名，只用于诊断日志。
const (
	taskOutboxQuery = "outbox_query"
	taskInboxAudit  = "inbox_audit"

	stageRelay       = "relay_" // 后接来源库名
	stageQuery       = "query_catch_up"
	stageRebuild     = "query_rebuild"
	stageInbox       = "inbox"
	stageAuditExport = "audit_export"
)

// syncStageError 标记后台同步失败所在的阶段；错误文本、错误码与原因分类
// 都沿用被包装的错误。
type syncStageError struct {
	stage string
	err   error
}

func (e *syncStageError) Error() string { return e.err.Error() }
func (e *syncStageError) Unwrap() error { return e.err }

func atStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &syncStageError{stage: stage, err: err}
}

// backgroundRepeat 是同一失败持续时两条日志的最短间隔。
const backgroundRepeat = time.Minute

// backgroundLog 记录后台同步的失败与恢复，原则与访问日志相同：只输出任务、
// 阶段、错误码与原因类别，不记错误文本，其中可能有路径或对象 ID。首次失败、
// 阶段/错误码/原因变化时立即记录，同一失败持续时每分钟最多一条，恢复时记一条。
// 失败照常反映在就绪状态上；日志只帮助定位，不改变重试行为。
type backgroundLog struct {
	mu    sync.Mutex
	out   io.Writer
	now   func() time.Time
	tasks map[string]*backgroundState
}

type backgroundState struct {
	failures int64
	since    time.Time
	logged   time.Time
	key      string
}

type backgroundEntry struct {
	Time     string  `json:"time"`
	Event    string  `json:"event"`
	Task     string  `json:"task"`
	Status   string  `json:"status"`
	Stage    string  `json:"stage,omitempty"`
	Code     string  `json:"error_code,omitempty"`
	Cause    string  `json:"error_cause,omitempty"`
	Failures int64   `json:"consecutive_failures"`
	Seconds  float64 `json:"failing_seconds"`
}

func newBackgroundLog(out io.Writer) *backgroundLog {
	return &backgroundLog{out: out, now: time.Now, tasks: map[string]*backgroundState{}}
}

// observe 接收一轮后台同步的结果。out 为 nil 时不记录。
func (l *backgroundLog) observe(task string, err error) {
	if l == nil || l.out == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st := l.tasks[task]
	if st == nil {
		st = &backgroundState{}
		l.tasks[task] = st
	}
	if err == nil {
		if st.failures > 0 {
			l.write(backgroundEntry{Task: task, Status: "recovered", Failures: st.failures, Seconds: now.Sub(st.since).Seconds()}, now)
		}
		*st = backgroundState{}
		return
	}
	stage := "unknown"
	var se *syncStageError
	if errors.As(err, &se) {
		stage = se.stage
	}
	code, cause := string(errcode.CodeOf(err)), string(sqlite.Classify(err))
	key := stage + "\x00" + code + "\x00" + cause
	if st.failures == 0 {
		st.since = now
	}
	st.failures++
	if st.failures > 1 && key == st.key && now.Sub(st.logged) < backgroundRepeat {
		return
	}
	st.key, st.logged = key, now
	l.write(backgroundEntry{Task: task, Status: "failing", Stage: stage, Code: code, Cause: cause, Failures: st.failures, Seconds: now.Sub(st.since).Seconds()}, now)
}

func (l *backgroundLog) write(e backgroundEntry, now time.Time) {
	e.Time, e.Event = now.UTC().Format(time.RFC3339Nano), "background_sync"
	_ = json.NewEncoder(l.out).Encode(e)
}
