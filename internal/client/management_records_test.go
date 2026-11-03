package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/managementrecord"
)

func managementTestClient(t *testing.T) (*Client, *atomic.Int32) {
	t.Helper()
	var mutations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/meta" {
			_, _ = w.Write([]byte(`{"api_version":"v1","instance_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV"}`))
			return
		}
		mutations.Add(1)
		t.Errorf("unexpected mutation %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	c, err := New(Config{BaseURL: server.URL, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, &mutations
}

func TestManagementPreparationRejectsChangedScopeBeforeMutation(t *testing.T) {
	c, mutations := managementTestClient(t)
	spec := ManagementRecordSpec{ProjectID: string(ids.New()), Kind: "migration", Title: "Synthetic managed record", Payload: json.RawMessage(`{"original":{"commit_key":"saved-key","operation_id":"fixed-operation"},"pending":true}`)}
	prepared, err := c.PrepareManagementRecord(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.PrepareManagementRecord(t.Context(), spec)
	if err != nil || !bytes.Equal(mustManagementJSON(t, prepared), mustManagementJSON(t, second)) {
		t.Fatal("unchanged preparation changed identity or digest", err)
	}
	raw, err := managementrecord.Render(prepared.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.SHA256 == "" || prepared.Size != int64(len(raw)) || prepared.Input.Content.Files[0].SHA256 != prepared.SHA256 {
		t.Fatal("prepared byte identity missing")
	}
	for _, change := range []func(*PreparedManagementRecord){
		func(p *PreparedManagementRecord) { p.Spec.Title = "Changed title" },
		func(p *PreparedManagementRecord) { p.Spec.ProjectID = string(ids.New()) },
		func(p *PreparedManagementRecord) { p.Envelope.InstanceID = ids.New() },
		func(p *PreparedManagementRecord) { p.Input.Content.Rights = json.RawMessage(`{"noai":false}`) },
		func(p *PreparedManagementRecord) { p.SHA256 = "changed" },
	} {
		var p PreparedManagementRecord
		_ = json.Unmarshal(mustManagementJSON(t, prepared), &p)
		change(&p)
		directory := t.TempDir()
		state := filepath.Join(t.TempDir(), "original-state.json")
		if _, err := c.SaveManagementRecord(t.Context(), p, directory, state); err == nil {
			t.Fatal("changed preparation accepted")
		}
		if _, err := os.Stat(filepath.Join(directory, "record.json")); !os.IsNotExist(err) {
			t.Fatal("rejected input wrote source bytes", err)
		}
		if _, err := os.Stat(state); !os.IsNotExist(err) {
			t.Fatal("rejected input generated new state", err)
		}
	}
	if mutations.Load() != 0 {
		t.Fatal("rejection reached mutation", mutations.Load())
	}
}

func mustManagementJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestManagementSourceExclusiveAndConflictPreservation(t *testing.T) {
	directory := t.TempDir()
	data := []byte("synthetic immutable record\n")
	if err := writeManagementFile(t.Context(), directory, data); err != nil {
		t.Fatal(err)
	}
	if err := writeManagementFile(t.Context(), directory, data); err != nil {
		t.Fatal("identical preparation did not resume", err)
	}
	if err := writeManagementFile(t.Context(), directory, []byte("different record")); err == nil || errcode.CodeOf(err) != errcode.PathConflict {
		t.Fatal("conflicting source overwritten", err)
	}
	actual, err := os.ReadFile(filepath.Join(directory, "record.json"))
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("original source changed", err)
	}
	if err = checkPrivateRecordFile(filepath.Join(directory, "record.json")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	empty := t.TempDir()
	if err := writeManagementFile(ctx, empty, data); err == nil {
		t.Fatal("cancelled preparation wrote source")
	}
	entries, err := os.ReadDir(empty)
	if err != nil || len(entries) != 0 {
		t.Fatal("cancelled preparation left files", entries, err)
	}
}

func checkPrivateRecordFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return checkPinnedOutputPrivate(f)
}

func TestManagementSaveRejectsDirectoryReplacementDuringRead(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	var replace atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/meta" {
			t.Errorf("unexpected remote mutation %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		if replace.Swap(false) {
			if err := os.Rename(directory, directory+"-original"); err != nil {
				t.Error(err)
			}
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Error(err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_version":"v1","instance_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV"}`))
	}))
	defer remote.Close()
	c, err := New(Config{BaseURL: remote.URL, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p, err := c.PrepareManagementRecord(t.Context(), ManagementRecordSpec{ProjectID: string(ids.New()), Kind: "assistant", Title: "Synthetic pinned record", Payload: json.RawMessage(`{"status":"unknown"}`)})
	if err != nil {
		t.Fatal(err)
	}
	replace.Store(true)
	state := filepath.Join(t.TempDir(), "original-state.json")
	if _, err := c.SaveManagementRecord(t.Context(), p, directory, state); err == nil {
		t.Fatal("replaced directory accepted")
	}
	for _, dir := range []string{directory, directory + "-original"} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatal("replaced directory received bytes", dir, entries, err)
		}
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("replacement generated new operation state", err)
	}
}
