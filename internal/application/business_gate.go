package application

import (
	"context"
	"github.com/oujinhaoai/lantai/internal/operations"
	"net/http"
	"sync"
)

// businessGate admits whole HTTP operations, including reads, downloads and
// uploads between domain transactions. Backup closes admission before draining
// them, then takes the domain gate to drain background writes. It is not a
// reader/writer lock: no business operation is admitted during backup.
type businessGate struct {
	mu      sync.Mutex
	closed  bool
	active  int
	drained chan struct{}
}

func (g *businessGate) enter() (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, false
	}
	g.active++
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.active--
		if g.closed && g.active == 0 {
			close(g.drained)
		}
	}, true
}

func (g *businessGate) close() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		g.closed = true
		g.drained = make(chan struct{})
		if g.active == 0 {
			close(g.drained)
		}
	}
	return g.drained
}

func (g *businessGate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = false
}

func (g *businessGate) wait(ctx context.Context) error {
	select {
	case <-g.close():
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *businessGate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release, ok := g.enter()
		if !ok {
			readinessGuard(func() bool { return false }, next).ServeHTTP(w, r)
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

// Register whole background iterations before they acquire application mutexes
// or enter multi-step domain operations. This prevents a background sync from
// holding coreMu/tailMu while waiting behind the backup's exclusive domain gate.
func (a *App) backgroundOperation(run func(context.Context)) func(context.Context) {
	return func(ctx context.Context) {
		release, ok := a.business.enter()
		if !ok {
			return
		}
		defer release()
		run(ctx)
	}
}

// Readiness includes the admission-drain phase before the domain gate closes.
func (a *App) Readiness() operations.Readiness {
	r := a.Instance.Readiness()
	a.business.mu.Lock()
	closed := a.business.closed
	a.business.mu.Unlock()
	if closed {
		r.Ready = false
		if r.State == operations.StateReady {
			r.State = operations.StateMaintenance
		}
		r.Reasons = append(r.Reasons, operations.Reason{Code: "backup_maintenance", Message: "business operations are paused for backup"})
	}
	return r
}
