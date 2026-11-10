package application

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	ax "github.com/oujinhaoai/lantai/internal/agent_execution"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity/httpauth"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/tasks"
)

// OpenRecoveryDrill opens only a restored, credential-rotated, isolated copy.
// It never completes reconciliation or asserts knowledge after backup capture.
// This is an application restriction, not an OS or network sandbox.
func OpenRecoveryDrill(ctx context.Context, opts Options) (*App, error) {
	opts.Instance.RecoveryDrill = true
	return Open(ctx, opts)
}

// Assemble only local state machines. In particular there is no extension
// manager, process host, job runner, workflow dispatcher or lifecycle scheduler.
func (a *App) assembleDrillCollaboration() error {
	i := a.Instance
	auth := authority{a.Identity, i}
	var err error
	a.Tasks, err = tasks.New(tasks.Deps{DB: i.DB(ownership.Runtime), Gate: i.Gate(), Authority: auth, Policies: a.Identity, Members: a.Identity, Resources: a.Ledger, Discussions: a.Ledger, Clock: i.Clock(), IDs: i.IDs(), InstanceID: i.InstanceID()})
	if err != nil {
		return err
	}
	a.Execution, err = ax.New(ax.Deps{DB: i.DB(ownership.Runtime), Gate: i.Gate(), Authority: auth, Tasks: a.Tasks, Assets: ax.CatalogAssets{Files: a.Storage, Evidence: a.Rights, Catalog: a.Catalog, InstanceID: i.InstanceID()}, Clock: i.Clock(), IDs: i.IDs()})
	if err != nil {
		return err
	}
	a.Tasks.SetExecutions(a.Execution)
	a.Ledger.SetCheckoutGuard(a.Tasks)
	return nil
}

func (a *App) startRecoveryDrill(ctx context.Context) error {
	return a.Instance.Start(ctx,
		operations.Hook{Name: "drill-administrator", Run: func(c context.Context) error {
			admins, err := a.Identity.RestoreAdministrators(c)
			if err != nil {
				return err
			}
			for _, admin := range admins {
				if a.Identity.VerifyRestoreAdministrator(c, a.Instance.Marker().Restore.RunID, admin.ID) == nil {
					return a.Identity.CheckStartup(c)
				}
			}
			return errors.New("application: recovery drill requires a freshly confirmed local recovery administrator")
		}},
		operations.Hook{Name: "drill-integrity", Run: func(c context.Context) error {
			r, err := a.FSCK(c, true)
			if err != nil {
				return err
			}
			return r.Err()
		}},
		operations.Hook{Name: "drill-query", Run: func(c context.Context) error { return a.sync(c, true) }},
	)
}

// This explicit allowlist is applied outside every HTTP surface, including the
// operations listener and merged transfer surface. New routes default to denied.
// Domain authentication, version rights, epochs and CSRF still apply inside it.
type drillHTTPOptions struct {
	instanceID ids.ID
	authority  string
	ready      func() bool
}

func recoveryDrillHandler(next http.Handler, options ...drillHTTPOptions) http.Handler {
	var cfg drillHTTPOptions
	if len(options) > 0 {
		cfg = options[0]
	}
	mux := http.NewServeMux()
	allow := func(pattern string) { mux.Handle(pattern, next) }
	for _, pattern := range []string{
		"POST /api/v1/sessions/login", "POST /api/v1/sessions/exchange", "DELETE /api/v1/sessions/current",
		"GET /api/v1/whoami", "GET /api/v1/projects", "GET /api/v1/projects/{key}",
		"GET /api/v1/types", "GET /api/v1/types/{type}", "GET /api/v1/assets", "GET /api/v1/assets/{asset_id}",
		"GET /api/v1/assets/{asset_id}/versions", "GET /api/v1/assets/{asset_id}/versions/{version_id}",
		"GET /api/v1/assets/{asset_id}/versions/{version_id}/rights", "POST /api/v1/assets/{asset_id}/versions/{version_id}/read-grants",
		"GET /xfer/v1/reads/{grant_id}",
		"GET /api/v1/tasks", "GET /api/v1/tasks/{id}", "GET /api/v1/tasks/{id}/attempts",
		"POST /api/v1/tasks", "POST /api/v1/tasks/claim", "POST /api/v1/tasks/renew", "POST /api/v1/tasks/release", "POST /api/v1/tasks/block", "POST /api/v1/tasks/cancel",
		"GET /api/v1/task-runs", "GET /api/v1/task-runs/{id}", "POST /api/v1/task-runs", "POST /api/v1/task-runs/progress", "POST /api/v1/task-runs/checkpoint", "POST /api/v1/task-runs/cancel",
	} {
		allow(pattern)
	}
	status := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ready := cfg.ready == nil || cfg.ready()
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"api_version": "v1", "instance_id": cfg.instanceID, "capabilities": []string{"sessions", "exact_read", "read_grants", "search", "isolated_manual_checkpoints"}, "ready": ready, "recovery_drill": true, "production_ready": false, "unknown_after_capture": true, "restore_complete": false})
	})
	mux.Handle("GET /api/v1/meta", status)
	mux.Handle("GET /healthz", status)
	mux.Handle("GET /readyz", status)
	deny := func(w http.ResponseWriter) {
		httpauth.WriteError(w, errcode.New(errcode.Forbidden, "recovery drill: operation disabled"), "")
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { deny(w) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Lantai-Recovery-Drill", "true")
		w.Header().Set("Cache-Control", "no-store")
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() || (cfg.authority != "" && r.Host != cfg.authority) {
			deny(w)
			return
		}
		// This server has no TLS listener and no proxy mode. Browser requests
		// must be same-origin with the exact bound local authority.
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			deny(w)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			deny(w)
			return
		}
		for key := range r.Header {
			key = strings.ToLower(key)
			if key == "forwarded" || key == "x-real-ip" || strings.HasPrefix(key, "x-forwarded-") {
				deny(w)
				return
			}
		}
		// Do not allow ServeMux to canonicalize an ambiguous path into an allowed one.
		if strings.Contains(r.URL.Path, "//") || strings.Contains(r.URL.Path, "/./") || strings.Contains(r.URL.Path, "/../") || r.URL.RawPath != "" {
			deny(w)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
