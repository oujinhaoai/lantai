package application

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// HTTPOption 控制本机 HTTP 观测，不改变领域授权。
type HTTPOption func(*httpOptions)
type httpOptions struct{ accessLog io.Writer }

// WithAccessLog 输出经过白名单筛选的 JSONL。nil 关闭日志；默认 stderr。
func WithAccessLog(w io.Writer) HTTPOption { return func(o *httpOptions) { o.accessLog = w } }

type accessLogger struct {
	mu  sync.Mutex
	out io.Writer
}
type observedWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedWriter) WriteHeader(n int) {
	if n >= 100 && n < 200 && n != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(n)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = n
	w.ResponseWriter.WriteHeader(n)
}
func (w *observedWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}
func (w *observedWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func observationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
func safeMethod(v string) string {
	switch v {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
		return v
	default:
		return "OTHER"
	}
}
func (l *accessLogger) wrap(surface string, next http.Handler) http.Handler {
	if l.out == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ow := &observedWriter{ResponseWriter: w}
		id := observationID()
		clientID := r.Header.Get("X-Request-Id")
		var code errcode.Code
		var cause error
		r = r.WithContext(errcode.WithObserver(r.Context(), func(c errcode.Code, err error) { code, cause = c, err }))
		defer func() {
			panicked := recover()
			status := ow.status
			if status == 0 {
				status = http.StatusOK
				if panicked != nil {
					status = http.StatusInternalServerError
				}
			}
			entry := struct {
				Time      string  `json:"time"`
				Event     string  `json:"event"`
				ID        string  `json:"observation_id"`
				Surface   string  `json:"surface"`
				Method    string  `json:"method"`
				Status    int     `json:"status"`
				Bytes     int64   `json:"bytes"`
				Seconds   float64 `json:"duration_seconds"`
				Cancelled bool    `json:"cancelled"`
				RequestID string  `json:"request_id,omitempty"`
				ClientID  string  `json:"request_id_sha256,omitempty"`
				Code      string  `json:"error_code,omitempty"`
				Cause     string  `json:"error_cause,omitempty"`
			}{Time: start.UTC().Format(time.RFC3339Nano), Event: "http_request", ID: id, Surface: surface, Method: safeMethod(r.Method), Status: status, Bytes: ow.bytes, Seconds: time.Since(start).Seconds(), Cancelled: r.Context().Err() != nil}
			// 服务端生成的 request ID 原样记录；客户端自带的即便语法合法也可能是
			// 凭据，只记 SHA-256，运维对错误信封中的 request_id 求值后定位。
			if rid := ow.Header().Get("X-Request-Id"); rid != "" && rid == clientID {
				sum := sha256.Sum256([]byte(rid))
				entry.ClientID = hex.EncodeToString(sum[:])
			} else {
				entry.RequestID = rid
			}
			// 只记错误码与原因类别，不记错误文本：其中可能有路径或对象 ID。
			entry.Code = string(code)
			if code != "" && status >= 500 {
				entry.Cause = string(sqlite.Classify(cause))
			}
			l.mu.Lock()
			_ = json.NewEncoder(l.out).Encode(entry)
			l.mu.Unlock()
			if panicked != nil {
				panic(panicked)
			}
		}()
		next.ServeHTTP(ow, r)
	})
}

// readinessGuard 是入口安全门；不把可重建索引滞后当成权威读取不可用。
// 在途请求的最终接受仍由领域 Gate、安全修订与恢复代次复验。
func readinessGuard(ready func() bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ready() {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(errcode.New(errcode.MaintenanceMode, "the instance is not accepting business requests").Envelope(""))
	})
}

type metric struct {
	name, labels string
	value        float64
}
type collector struct {
	name    string
	collect func(context.Context) ([]metric, error)
}

func gauge(name string, value float64, labels ...string) metric {
	return metric{"lantai_" + name, strings.Join(labels, ","), value}
}
func boolean(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
func unixTime(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.Unix())
}
func age(now, oldest time.Time) float64 {
	if oldest.IsZero() || oldest.After(now) {
		return 0
	}
	return now.Sub(oldest).Seconds()
}

// 部分源失败仍输出可用组；503 与 available=0 明确区别于正常零值。
func metricsHandler(collectors []collector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		var b bytes.Buffer
		complete := true
		for _, c := range collectors {
			metrics, err := c.collect(ctx)
			fmt.Fprintf(&b, "lantai_collector_available{collector=%q} %g\n", c.name, boolean(err == nil))
			if err != nil {
				complete = false
				continue
			}
			for _, m := range metrics {
				if m.labels == "" {
					fmt.Fprintf(&b, "%s %g\n", m.name, m.value)
				} else {
					fmt.Fprintf(&b, "%s{%s} %g\n", m.name, m.labels, m.value)
				}
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if !complete {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = w.Write(b.Bytes())
	})
}

func walBytes(path string) (int64, error) {
	if info, err := os.Stat(path); err != nil {
		return 0, err
	} else if !info.Mode().IsRegular() {
		return 0, errors.New("database is not a regular file")
	}
	info, err := os.Lstat(path + "-wal")
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("WAL is not a regular file")
	}
	return info.Size(), nil
}

func (a *App) observationCollectors(scheduler *transfer.Scheduler) []collector {
	cs := []collector{
		{"instance", func(context.Context) ([]metric, error) {
			r := a.Instance.Readiness()
			open, _ := a.Instance.Gate().State()
			return []metric{gauge("live", boolean(r.Live)), gauge("ready", boolean(a.Ready())), gauge("business_ready", boolean(r.Ready)), gauge("writes_open", boolean(open))}, nil
		}},
		{"transfer", func(context.Context) ([]metric, error) {
			s := scheduler.Stats()
			return []metric{gauge("transfer_active", float64(s.ActiveInteractive), `class="interactive"`), gauge("transfer_active", float64(s.ActiveBatch), `class="batch"`), gauge("transfer_waiting", float64(s.WaitingInteractive), `class="interactive"`), gauge("transfer_waiting", float64(s.WaitingBatch), `class="batch"`), gauge("transfer_admitted_total", float64(s.AdmittedInteract), `class="interactive"`), gauge("transfer_admitted_total", float64(s.AdmittedBatch), `class="batch"`)}, nil
		}},
		{"locks", func(context.Context) ([]metric, error) {
			s := a.Instance.Gate().Stats()
			return []metric{gauge("lock_waiters", float64(s.Waiters)), gauge("lock_wait_total", float64(s.WaitCount)), gauge("lock_wait_seconds_total", float64(s.WaitNanos)/1e9)}, nil
		}},
		{"events", func(ctx context.Context) ([]metric, error) {
			m, err := a.Events.Metrics(ctx)
			if err != nil {
				return nil, err
			}
			return []metric{gauge("events_high_water", float64(m.HighWater)), gauge("events_pruned_through", float64(m.PrunedThrough)), gauge("events_audit_through", float64(m.AuditThrough)), gauge("events_backup_through", float64(m.BackupThrough)), gauge("events_hot_records", float64(m.HotRecords)), gauge("events_relay_failures", float64(m.RelayFailures)), gauge("events_consumer_lag", float64(max(0, m.ConsumerLag)))}, nil
		}},
		{"query", func(ctx context.Context) ([]metric, error) {
			m, err := a.Query.State(ctx)
			if err != nil {
				return nil, err
			}
			return []metric{gauge("query_generation", float64(m.Generation)), gauge("query_high_water", float64(m.HighWater)), gauge("query_rebuild_start", float64(m.RebuildStart))}, nil
		}},
		{"backups", func(ctx context.Context) ([]metric, error) {
			m, err := a.Instance.BackupStats(ctx)
			if err != nil {
				return nil, err
			}
			return []metric{gauge("backups_complete", float64(m.Complete)), gauge("backups_incomplete", float64(m.Incomplete)), gauge("backup_pins", float64(m.Pins)), gauge("backup_last_complete_timestamp_seconds", unixTime(m.LastComplete)), gauge("backup_last_verified_timestamp_seconds", unixTime(m.LastVerified))}, nil
		}},
		{"storage", func(ctx context.Context) ([]metric, error) {
			m, err := a.Storage.Stats(ctx)
			if err != nil {
				return nil, err
			}
			return []metric{gauge("uploads_open", float64(m.OpenUploads)), gauge("upload_staging_bytes", float64(m.UploadStagingBytes)), gauge("upload_pins", float64(m.ActiveUploadPins)), gauge("upload_retained_bytes", float64(m.UploadRetainedBytes)), gauge("gc_supported", boolean(m.GCSupported))}, nil
		}},
	}
	for _, db := range []ownership.Database{ownership.Main, ownership.Ledger, ownership.Runtime} {
		cs = append(cs, collector{"commands_" + string(db), func(ctx context.Context) ([]metric, error) {
			count, oldest, err := commands.OutboxBacklog(ctx, a.Instance.DB(db))
			if err != nil {
				return nil, err
			}
			s, err := commands.OperationStats(ctx, a.Instance.DB(db))
			if err != nil {
				return nil, err
			}
			label := fmt.Sprintf("database=%q", db)
			out := []metric{gauge("outbox_pending", float64(count), label), gauge("outbox_oldest_age_seconds", age(time.Now(), oldest), label), gauge("operations_pending", float64(s.Pending), label)}
			for _, stage := range []commands.Stage{commands.StageReceiving, commands.StagePrepared, commands.StageInstalled, commands.StageBlocked, commands.StageQuarantined, commands.StageCommitted, commands.StageProjected, commands.StageFailed, commands.StageCancelled} {
				out = append(out, gauge("operations", float64(s.ByStage[stage]), label, fmt.Sprintf("stage=%q", stage)))
			}
			return out, nil
		}})
	}
	for _, db := range []ownership.Database{ownership.Main, ownership.Ledger, ownership.Runtime, ownership.Events, ownership.Index} {
		cs = append(cs, collector{"wal_" + string(db), func(context.Context) ([]metric, error) {
			n, err := walBytes(a.Instance.Layout().DBPath(db))
			return []metric{gauge("sqlite_wal_bytes", float64(n), fmt.Sprintf("database=%q", db))}, err
		}})
	}
	for _, area := range []struct{ name, path string }{{"data", a.Instance.Layout().Home}, {"database", a.Instance.Layout().DBDir()}} {
		cs = append(cs, collector{"disk_" + area.name, func(context.Context) ([]metric, error) {
			n, err := fsutil.FreeBytes(area.path)
			return []metric{gauge("disk_available_bytes", float64(n), fmt.Sprintf("area=%q", area.name)), gauge("disk_min_free_bytes", float64(a.Instance.Config().MinFreeBytes), fmt.Sprintf("area=%q", area.name))}, err
		}})
	}
	return cs
}

func (a *App) operationsHandler(scheduler *transfer.Scheduler) http.Handler {
	return localOperationsHandler(a.Instance.Readiness, a.Ready, a.observationCollectors(scheduler))
}
func localOperationsHandler(readiness func() operations.Readiness, ready func() bool, collectors []collector) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		live := readiness().Live
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if !live {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"live": live})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		r := readiness()
		ok := ready()
		reasons := make([]string, 0, len(r.Reasons)+1)
		for _, reason := range r.Reasons {
			reasons = append(reasons, reason.Code)
		}
		if !ok && r.Ready {
			reasons = append(reasons, "synchronization_pending")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": ok, "business_ready": r.Ready, "state": r.State, "reason_codes": reasons})
	})
	mux.Handle("GET /metrics", metricsHandler(collectors))
	return mux
}
