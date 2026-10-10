package integration

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBusinessRecoveryServeLivesUntilOwnedCleanup(t *testing.T) {
	f := newAppFlow(t)
	if err := f.app.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log, err := os.Create(filepath.Join(dir, "cleanup.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := log.Close(); err != nil {
			t.Error(err)
		}
	})
	b := &businessRecovery{
		reviewRemote: &reviewRemote{f: f, bin: reviewAcceptanceBinary(t), log: log},
		home:         f.inst.Layout().Home,
		evidenceDir:  dir,
	}
	t.Cleanup(b.stop)
	b.startServe()
	t.Cleanup(func() {
		if t.Context().Err() == nil {
			t.Error("testing context must be cancelled before cleanup")
		}
		// A test-context cancellation must not race our explicit process owner.
		select {
		case err := <-b.done:
			b.process = nil // Wait has already reaped the unexpectedly dead child.
			t.Errorf("serve ended before its owner stopped it: %v", err)
			return
		case <-time.After(100 * time.Millisecond):
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.origin+"/api/v1/meta", nil)
		if err != nil {
			t.Fatal(err)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("serve unavailable before owned cleanup: %v", err)
			return
		}
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("serve meta before owned cleanup: %d", r.StatusCode)
		}
		b.stop() // Retains the strict Kill error and actual-exit deadline checks.
	})
}
