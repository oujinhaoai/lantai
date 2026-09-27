package httpapi

import (
	"io"
	"net/http"
	"time"
)

// ResponseController reaches the real server connection through Unwrap. The
// stream has no total deadline, so a progressing large transfer can finish.
func idleTransfer(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		controller := http.NewResponseController(w)
		// Keep deadlines through net/http's final body drain and buffered flush.
		// net/http resets them before reusing the connection for its next request.
		_ = controller.SetReadDeadline(time.Now().Add(timeout))
		_ = controller.SetWriteDeadline(time.Now().Add(timeout))
		r.Body = &idleBody{ReadCloser: r.Body, controller: controller, timeout: timeout}
		next.ServeHTTP(&idleWriter{ResponseWriter: w, controller: controller, timeout: timeout}, r)
	})
}

type idleBody struct {
	io.ReadCloser
	controller *http.ResponseController
	timeout    time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	_ = b.controller.SetReadDeadline(time.Now().Add(b.timeout))
	return b.ReadCloser.Read(p)
}

type idleWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	timeout    time.Duration
}

func (w *idleWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *idleWriter) Write(p []byte) (int, error) {
	_ = w.controller.SetWriteDeadline(time.Now().Add(w.timeout))
	return w.ResponseWriter.Write(p)
}
func (w *idleWriter) WriteHeader(status int) {
	_ = w.controller.SetWriteDeadline(time.Now().Add(w.timeout))
	w.ResponseWriter.WriteHeader(status)
}
func (w *idleWriter) Flush() {
	_ = w.controller.SetWriteDeadline(time.Now().Add(w.timeout))
	_ = w.controller.Flush()
}
