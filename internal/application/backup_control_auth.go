package application

import "github.com/oujinhaoai/lantai/internal/identity"

// BackupControlScope takes no identity locks and performs no IO. Reauthentication
// is admitted after business admission closes, including the drain phase.
// In-flight identity transactions serialize with the shared proof protocol.
func (a *App) BackupControlScope() (identity.BackupControlScope, bool) {
	b := a.backupControl
	if b == nil {
		return identity.BackupControlScope{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	j, ok := b.jobs[b.active]
	if !b.controlAuthOpen || !ok || (j.State != "draining" && j.State != "backing_up" && j.State != "cancelling") {
		return identity.BackupControlScope{}, false
	}
	a.business.mu.Lock()
	closed := a.business.closed
	a.business.mu.Unlock()
	if !closed {
		return identity.BackupControlScope{}, false
	}
	return identity.BackupControlScope{JobID: j.ID, Attempt: j.Attempt}, true
}
