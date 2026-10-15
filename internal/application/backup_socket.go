package application

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// BackupSocketPath is derived from a public instance ID, never a secret. The
// enclosing directory and socket are owner-only; there is no TCP fallback.
// /tmp is fixed on supported Unix systems so service and CLI cannot disagree
// because their TMPDIR values differ, or exceed sockaddr_un path limits.
func BackupSocketPath(id ids.ID) string {
	return filepath.Join("/tmp", "lantai-control-"+strconv.Itoa(os.Geteuid()), string(id)+".sock")
}

func (b *BackupControl) listen() (net.Listener, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return nil, errors.New("online backup requires Linux or macOS Unix socket permissions")
	}
	path := BackupSocketPath(b.app.Instance.InstanceID())
	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0700 || !backupOwner(st) {
		return nil, errors.New("backup socket directory must be private (0700)")
	}
	if st, err = os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 || !backupOwner(st) {
			return nil, errors.New("backup socket path occupied by non-socket")
		}
		// This App already owns the data-root lock, so no earlier core for the same
		// instance can be running. Remove only its abandoned socket, never a file.
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

func (b *BackupControl) Handler() http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, j BackupJob, err error, status int) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			switch {
			case errors.Is(err, ErrBackupBusy):
				status = http.StatusConflict
			case errors.Is(err, os.ErrNotExist):
				status = http.StatusNotFound
			default:
				status = http.StatusBadRequest
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(j)
	}
	mux.HandleFunc("POST /backup", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		var req BackupRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			write(w, BackupJob{}, errors.New("invalid backup request"), 0)
			return
		}
		if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			write(w, BackupJob{}, errors.New("trailing request data"), 0)
			return
		}
		j, err := b.Start(req)
		write(w, j, err, http.StatusAccepted)
	})
	mux.HandleFunc("GET /backup/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, err := b.Status(ids.ID(r.PathValue("id")))
		write(w, j, err, http.StatusOK)
	})
	mux.HandleFunc("POST /backup/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		j, err := b.Cancel(ids.ID(r.PathValue("id")))
		write(w, j, err, http.StatusAccepted)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Browsers and public gateway routes are not callers of this local API.
		if r.Header.Get("Origin") != "" || r.Host != "lantai-backup" || r.Header.Get("X-Lantai-Instance") != string(b.app.Instance.InstanceID()) {
			http.Error(w, "local backup authority required", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// BackupClient has no proxy, redirects, DNS or TCP path. It only connects to the
// same-user private socket belonging to the selected instance.
func BackupClient(id ids.ID) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		path := BackupSocketPath(id)
		dir, err := os.Lstat(filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		socket, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !dir.IsDir() || dir.Mode().Perm() != 0700 || !backupOwner(dir) || socket.Mode()&os.ModeSocket == 0 || socket.Mode().Perm() != 0600 || !backupOwner(socket) {
			return nil, errors.New("backup control socket permissions or owner are unsafe")
		}
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
