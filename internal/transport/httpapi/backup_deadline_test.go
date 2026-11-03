package httpapi

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestBackupPreparationDoesNotConsumeStreamIdleDeadline(t *testing.T) {
	h := &Handler{cfg: Config{APITimeout: 20 * time.Millisecond, TransferIdleTimeout: 40 * time.Millisecond}}
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			path := "/api/v1/admin/backups/" + string(ids.New()) + "/package"
			if method == "POST" {
				path = "/api/v1/admin/backups/" + string(ids.New()) + "/download-request"
			}
			srv := httptest.NewServer(h.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Decode the approval body before full verification.
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
					return
				}
				// Model full package verification before the first response byte.
				select {
				case <-time.After(160 * time.Millisecond):
				case <-r.Context().Done():
					t.Error("preparation inherited JSON API deadline")
					return
				}
				_, _ = io.WriteString(w, "verified package")
			}), true))
			defer srv.Close()
			req, _ := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader("{}"))
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal("preparation exhausted transfer deadline", err)
			}
			defer res.Body.Close()
			raw, err := io.ReadAll(res.Body)
			if err != nil || string(raw) != "verified package" {
				t.Fatalf("download after verification: %q %v", raw, err)
			}
		})
	}
}

func TestBackupApprovalBodyKeepsAbsoluteReadDeadline(t *testing.T) {
	h := &Handler{cfg: Config{APITimeout: 20 * time.Millisecond, TransferIdleTimeout: 150 * time.Millisecond}}
	read := make(chan error, 1)
	srv := httptest.NewServer(h.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		read <- err
		w.WriteHeader(http.StatusBadRequest)
	}), true))
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err = fmt.Fprintf(conn, "POST /api/v1/admin/backups/%s/download-request HTTP/1.1\r\nHost: synthetic\r\nContent-Length: 12\r\n\r\n", ids.New()); err != nil {
		t.Fatal(err)
	}
	stop, sent := make(chan struct{}), make(chan struct{})
	defer func() {
		close(stop)
		_ = conn.Close()
		<-sent
	}()
	go func() {
		defer close(sent)
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		for range 12 {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			if _, err := conn.Write([]byte("x")); err != nil {
				return
			}
		}
	}()
	select {
	case err := <-read:
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("drip-fed approval escaped its absolute body deadline: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("approval body did not time out")
	}
}

func TestBackupPreparationDisconnectCancelsVerification(t *testing.T) {
	h := &Handler{cfg: Config{APITimeout: 20 * time.Millisecond, TransferIdleTimeout: 100 * time.Millisecond}}
	started, cancelled := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(h.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
			return
		}
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}), true))
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = fmt.Fprintf(conn, "POST /api/v1/admin/backups/%s/download-request HTTP/1.1\r\nHost: synthetic\r\nContent-Length: 2\r\n\r\n{}", ids.New()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("verification did not start")
	}
	_ = conn.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect did not cancel verification")
	}
}
