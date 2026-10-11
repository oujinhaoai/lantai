package firststart

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/identity"
)

//go:embed setup.html setup.js setup.css
var assets embed.FS

// Handler exposes only setup while the exclusive instance lock is held. The
// claim is required for every mutation; no cookie or normal session is issued.
type Handler struct {
	session   *Session
	origins   []string
	mu        sync.Mutex
	pending   *identity.Bootstrap
	id        string
	expires   time.Time
	attempts  int
	nextBegin time.Time
	terminal  bool
	done      chan error
}

func validateOrigins(origins []string) error {
	if len(origins) == 0 {
		return errors.New("first start requires an explicit HTTPS allowed origin")
	}
	for _, origin := range origins {
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
			return errors.New("first start requires exact HTTPS origins without path, query or credentials")
		}
	}
	return nil
}

func NewHandler(s *Session, origins []string) (*Handler, error) {
	if err := validateOrigins(origins); err != nil {
		return nil, err
	}
	return &Handler{session: s, origins: append([]string(nil), origins...), done: make(chan error, 1)}, nil
}

func (h *Handler) validOrigin(r *http.Request) bool {
	values := r.Header.Values("Origin")
	if len(values) > 1 || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	if r.Method == http.MethodPost && len(values) != 1 {
		return false
	}
	for _, origin := range h.origins {
		u, _ := url.Parse(origin)
		if strings.EqualFold(r.Host, u.Host) && (len(values) == 0 || values[0] == origin) {
			return true
		}
	}
	return false
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	if r.URL.RawQuery != "" || !h.validOrigin(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.terminal {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		name, typ := "", ""
		switch r.URL.Path {
		case "/setup", "/setup/":
			name, typ = "setup.html", "text/html; charset=utf-8"
		case "/setup/setup.js":
			name, typ = "setup.js", "text/javascript; charset=utf-8"
		case "/setup/setup.css":
			name, typ = "setup.css", "text/css; charset=utf-8"
		default:
			http.NotFound(w, r)
			return
		}
		raw, _ := assets.ReadFile(name)
		w.Header().Set("Content-Type", typ)
		_, _ = w.Write(raw)
		return
	}
	if r.Method != http.MethodPost || (r.URL.Path != "/setup/begin" && r.URL.Path != "/setup/confirm") {
		http.NotFound(w, r)
		return
	}
	auth := r.Header.Values("Authorization")
	if len(auth) != 1 || subtle.ConstantTimeCompare([]byte(auth[0]), []byte("Bearer "+h.session.code)) != 1 {
		http.Error(w, "Invalid setup code", http.StatusUnauthorized)
		return
	}
	typ, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || typ != "application/json" {
		http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	if r.URL.Path == "/setup/begin" {
		h.begin(w, r)
	} else {
		h.confirm(w, r)
	}
}

func readJSON(r *http.Request, v any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("extra JSON")
	}
	return nil
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Handler) begin(w http.ResponseWriter, r *http.Request) {
	if time.Now().Before(h.nextBegin) {
		http.Error(w, "Wait before restarting setup", http.StatusTooManyRequests)
		return
	}
	var in struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if readJSON(r, &in) != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	h.nextBegin = time.Now().Add(10 * time.Second)
	h.pending = nil
	h.id = ""
	b, err := h.session.Identity.BeginBootstrap(r.Context(), identity.BootstrapRequest{Name: in.Name, Password: in.Password})
	if err != nil {
		http.Error(w, "Use a valid username and a password of at least 12 characters", http.StatusBadRequest)
		return
	}
	codes, err := b.PrepareRecoveryCodes()
	if err != nil {
		http.Error(w, "Setup unavailable", http.StatusServiceUnavailable)
		return
	}
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		http.Error(w, "Setup unavailable", http.StatusServiceUnavailable)
		return
	}
	h.id = base64.RawURLEncoding.EncodeToString(buf)
	h.pending = b
	h.expires = time.Now().Add(10 * time.Minute)
	h.attempts = 0
	reply(w, map[string]any{"enrollment_id": h.id, "totp_secret": b.Enrollment.Secret, "totp_uri": b.Enrollment.URI, "recovery_codes": codes, "expires_in_seconds": 600})
}

func (h *Handler) confirm(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID    string `json:"enrollment_id"`
		TOTP  string `json:"totp_code"`
		Saved bool   `json:"recovery_codes_saved"`
	}
	if readJSON(r, &in) != nil || !in.Saved {
		http.Error(w, "Save recovery codes before confirming", http.StatusBadRequest)
		return
	}
	if h.pending == nil || !time.Now().Before(h.expires) || h.attempts >= 5 || subtle.ConstantTimeCompare([]byte(in.ID), []byte(h.id)) != 1 {
		http.Error(w, "Enrollment expired; start again", http.StatusConflict)
		return
	}
	h.attempts++
	if _, err := h.pending.Confirm(r.Context(), in.TOTP); err != nil {
		http.Error(w, "Verification failed", http.StatusBadRequest)
		return
	}
	// Commit permanently closes this handler, even if activation or cleanup
	// fails. Restart resumes from the authoritative closed bootstrap state.
	h.terminal = true
	h.pending = nil
	h.id = ""
	err := h.session.finish(r.Context())
	h.done <- err
	if err != nil {
		http.Error(w, "Administrator saved; restart the service to retry activation. Keep your recovery codes.", http.StatusServiceUnavailable)
		return
	}
	reply(w, map[string]bool{"complete": true})
}

func (h *Handler) Serve(ctx context.Context, address string) error {
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	s := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192, ErrorLog: log.New(io.Discard, "", 0), BaseContext: func(net.Listener) context.Context { return ctx }}
	result := make(chan error, 1)
	go func() { result <- s.Serve(ln) }()
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-h.done:
	case err = <-result:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = errors.Join(err, s.Shutdown(shutdown))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
