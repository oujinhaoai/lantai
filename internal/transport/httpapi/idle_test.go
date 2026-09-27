package httpapi

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestMergedJSONBodyHasActualReadDeadline(t *testing.T) {
	f := newFixture(t)
	f.h.cfg.APITimeout = 50 * time.Millisecond
	serverConn, conn := net.Pipe()
	defer conn.Close()
	listener := &pipeListener{connections: make(chan net.Conn, 1), closed: make(chan struct{})}
	listener.connections <- serverConn
	s := TransferConfig("", f.h.Merged(http.NotFoundHandler()))
	defer s.Close()
	go func() { _ = s.Serve(listener) }()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, err := fmt.Fprintf(conn, "POST /api/v1/uploads HTTP/1.1\r\nHost: example.test\r\nAuthorization: Bearer lts_test\r\nIdempotency-Key: stalled-json\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 422 {
		t.Fatalf("stalled JSON status %d", response.StatusCode)
	}
	if f.storage.called {
		t.Fatal("incomplete JSON reached domain")
	}
}

func TestTransferIdleReadTimesOutAndProgressResetsIt(t *testing.T) {
	for _, progress := range []bool{false, true} {
		t.Run(fmt.Sprint(progress), func(t *testing.T) {
			results := make(chan error, 1)
			handler := idleTransfer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := io.Copy(io.Discard, r.Body)
				results <- err
				w.WriteHeader(204)
			}), 200*time.Millisecond)
			serverConn, conn := net.Pipe()
			listener := &pipeListener{connections: make(chan net.Conn, 1), closed: make(chan struct{})}
			listener.connections <- serverConn
			s := &http.Server{Handler: handler}
			go func() { _ = s.Serve(listener) }()
			defer s.Close()
			defer conn.Close()
			if _, err := fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: example.test\r\nContent-Length: 8\r\n\r\na"); err != nil {
				t.Fatal(err)
			}
			if progress {
				for i := 0; i < 7; i++ {
					time.Sleep(50 * time.Millisecond)
					if _, err := conn.Write([]byte{'b'}); err != nil {
						t.Fatal(err)
					}
				}
			}
			select {
			case err := <-results:
				if progress && err != nil {
					t.Fatal(err)
				}
				if !progress && err == nil {
					t.Fatal("stalled body was accepted")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("idle read did not finish")
			}
		})
	}
}

type deadlineWriter struct {
	*httptest.ResponseRecorder
	reads, writes []time.Time
	flushed       bool
}

func (w *deadlineWriter) SetReadDeadline(t time.Time) error { w.reads = append(w.reads, t); return nil }
func (w *deadlineWriter) SetWriteDeadline(t time.Time) error {
	w.writes = append(w.writes, t)
	return nil
}
func (w *deadlineWriter) Flush() { w.flushed = true }

func TestTransferWriterDeadlinesAndUnwrap(t *testing.T) {
	recorded := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest("GET", "/", nil)
	const timeout = time.Second
	h := idleTransfer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		controller := http.NewResponseController(w)
		for _, action := range []struct {
			name string
			run  func()
		}{
			{"header", func() { w.WriteHeader(200) }},
			{"first write", func() { _, _ = w.Write([]byte("a")) }},
			{"next write", func() { _, _ = w.Write([]byte("b")) }},
			{"flush", func() {
				if err := controller.Flush(); err != nil {
					t.Fatal(err)
				}
			}},
		} {
			// A controller must reach the connection through Unwrap. Seed an
			// expired deadline so every I/O must actively replace it; merely
			// retaining a deadline from the previous I/O cannot satisfy this test.
			expired := time.Now().Add(-time.Hour)
			if err := controller.SetWriteDeadline(expired); err != nil {
				t.Fatal(err)
			}
			beforeCount := len(recorded.writes)
			if beforeCount == 0 || recorded.writes[beforeCount-1] != expired {
				t.Fatal("controller did not reach the underlying writer")
			}
			earliest := time.Now().Add(timeout)
			action.run()
			latest := time.Now().Add(timeout)
			if len(recorded.writes) != beforeCount+1 {
				t.Fatalf("%s did not refresh exactly one deadline", action.name)
			}
			deadline := recorded.writes[beforeCount]
			// Equal readings are valid on platforms with coarser clocks.
			if deadline.Before(earliest) || deadline.After(latest) {
				t.Fatalf("%s deadline %v is outside [%v, %v]", action.name, deadline, earliest, latest)
			}
		}
	}), timeout)
	h.ServeHTTP(recorded, r)
	if !recorded.flushed || len(recorded.reads) == 0 || recorded.reads[len(recorded.reads)-1].IsZero() || recorded.writes[len(recorded.writes)-1].IsZero() {
		t.Fatalf("deadlines not retained through net/http finalization: %+v", recorded)
	}
}

// net.Pipe exercises net/http connection deadlines without opening a listener.
type pipeListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.connections:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *pipeListener) Addr() net.Addr { return pipeAddr("http-test") }

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }
