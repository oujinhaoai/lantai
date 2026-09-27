package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity/httpauth"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

const DefaultMaxJSONBytes int64 = 8 << 20

type Handler struct {
	deps  Deps
	cfg   Config
	guard *httpauth.Guard
	api   http.Handler
}

func New(deps Deps, cfg Config) (*Handler, error) {
	if deps.Identity == nil || deps.Catalog == nil || deps.Storage == nil || deps.Query == nil || deps.Operations == nil || !cfg.InstanceID.Valid() {
		return nil, errors.New("httpapi: all domain dependencies and a valid instance ID are required")
	}
	if cfg.MaxJSONBytes == 0 {
		cfg.MaxJSONBytes = DefaultMaxJSONBytes
	}
	if cfg.APITimeout == 0 {
		cfg.APITimeout = 30 * time.Second
	}
	if cfg.TransferIdleTimeout == 0 {
		cfg.TransferIdleTimeout = 2 * time.Minute
	}
	if cfg.MaxJSONBytes < 1 || cfg.MaxJSONBytes > 64<<20 || cfg.APITimeout < 0 || cfg.TransferIdleTimeout < 0 {
		return nil, errors.New("httpapi: invalid request limits")
	}
	guard, err := httpauth.NewGuard(deps.Identity, httpauth.Policy{AllowedOrigins: cfg.AllowedOrigins})
	if err != nil {
		return nil, err
	}
	h := &Handler{deps: deps, cfg: cfg, guard: guard}
	mux := http.NewServeMux()
	h.routes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { writeError(w, r, errcode.New(errcode.NotFound, "")) })
	h.api = h.middleware(mux, true)
	return h, nil
}

func (h *Handler) API() http.Handler { return h.api }

// Transfer reuses the identity guard and storage's streaming implementation.
// Client supplied transfer classes never enter the trusted authz.Context.
func (h *Handler) Transfer(s *storage.Service, scheduler *transfer.Scheduler) http.Handler {
	auth := storage.AuthenticatorFunc(func(r *http.Request) (authz.Context, error) { c, err := h.guard.Authenticate(r); return c.Context, err })
	return h.middleware(idleTransfer(storage.NewTransferHandler(s, auth, scheduler), h.cfg.TransferIdleTimeout), false)
}

// Merged is a development routing composition of exactly the split handlers.
func (h *Handler) Merged(xfer http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", h.API())
	mux.Handle("/xfer/", xfer)
	mux.Handle("/", h.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeError(w, r, errcode.New(errcode.NotFound, "")) }), false))
	return mux
}

// APIConfig and TransferConfig create servers without listening. Listener/TLS
// selection, startup and coordinated shutdown belong to the application owner.
func APIConfig(addr string, handler http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 35 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
}

// APIConfig uses this handler's configured domain timeout plus a short response margin.
func (h *Handler) APIConfig(addr string) *http.Server {
	s := APIConfig(addr, h.API())
	s.ReadTimeout = h.cfg.APITimeout + 5*time.Second
	s.WriteTimeout = h.cfg.APITimeout + 10*time.Second
	return s
}
func TransferConfig(addr string, handler http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
}

var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
var idempotencyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func (h *Handler) middleware(next http.Handler, api bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		readLimit, writeLimit := h.cfg.TransferIdleTimeout, h.cfg.TransferIdleTimeout
		if api {
			ctx, cancel := context.WithTimeout(r.Context(), h.cfg.APITimeout)
			defer cancel()
			r = r.WithContext(ctx)
			readLimit, writeLimit = h.cfg.APITimeout, h.cfg.APITimeout+10*time.Second
		}
		// Context cancellation alone does not interrupt Body.Read. Keep these
		// deadlines through net/http's final drain/flush; it resets them for reuse.
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(time.Now().Add(readLimit))
		_ = controller.SetWriteDeadline(time.Now().Add(writeLimit))
		id := r.Header.Get("X-Request-Id")
		bad := id != "" && !requestIDRE.MatchString(id) || len(r.Header.Values("X-Request-Id")) > 1
		if id == "" || bad {
			var b [16]byte
			if _, err := rand.Read(b[:]); err != nil {
				writeError(w, r, err)
				return
			}
			id = hex.EncodeToString(b[:])
		}
		r = r.Clone(r.Context())
		r.Header.Set("X-Request-Id", id)
		w.Header().Set("X-Request-Id", id)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if bad {
			writeError(w, r, invalid("invalid request ID"))
			return
		}
		cookies := 0
		for _, cookie := range r.Cookies() {
			if cookie.Name == httpauth.CookieName {
				cookies++
			}
		}
		if len(r.Header.Values("Authorization")) > 1 || len(r.Header.Values("Origin")) > 1 || len(r.Header.Values(httpauth.CSRFHeader)) > 1 || cookies > 1 {
			writeError(w, r, errcode.New(errcode.Forbidden, "ambiguous authentication headers"))
			return
		}
		if api {
			if r.Header.Get("X-Lantai-Task") != "" || r.Header.Get("X-Lantai-Lease") != "" {
				writeError(w, r, invalid("task execution context is not enabled in M1"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type endpoint func(http.ResponseWriter, *http.Request, authz.Context) error

func (h *Handler) route(mux *http.ServeMux, pattern string, public bool, fn endpoint) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		var who authz.Context
		if !public {
			c, err := h.guard.Authenticate(r)
			if err != nil {
				writeError(w, r, err)
				return
			}
			who = c.Context
		}
		if err := fn(w, r, who); err != nil {
			writeError(w, r, err)
		}
	})
}

func invalid(message string) error { return errcode.New(errcode.SchemaInvalid, message) }
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	httpauth.WriteError(w, err, r.Header.Get("X-Request-Id"))
}
func respond(w http.ResponseWriter, status int, v any) error {
	// Encode before writing headers so a serialization failure has one error envelope.
	data, err := marshalHTTP(v)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A disconnected client must replay/poll as documented; after headers have
	// been written an additional error envelope would corrupt this response.
	_, _ = w.Write(append(data, '\n'))
	return nil
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, v any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return invalid("Content-Type must be application/json")
	}
	if r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity" {
		return invalid("compressed JSON requests are not supported")
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.cfg.MaxJSONBytes))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			return errcode.New(errcode.SchemaInvalid, "JSON request exceeds the configured limit").WithDetails(errcode.Detail{Reason: "request_body_too_large"})
		}
		return invalid("could not read JSON request")
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return invalid("request must be a JSON object")
	}
	if _, err = canonjson.Canonicalize(b); err != nil {
		return invalid("request is not valid unambiguous JSON")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err != nil {
		return invalid("JSON fields do not match the request contract")
	}
	return nil
}

func key(r *http.Request) (string, error) {
	k := r.Header.Get("Idempotency-Key")
	if len(r.Header.Values("Idempotency-Key")) != 1 || !idempotencyRE.MatchString(k) {
		return "", invalid("a valid Idempotency-Key is required")
	}
	return k, nil
}
func pathID(r *http.Request, name string) (ids.ID, error) {
	id := ids.ID(r.PathValue(name))
	if !id.Valid() {
		return "", invalid("invalid resource ID")
	}
	return id, nil
}
func view(r *http.Request) (bool, error) {
	v := r.URL.Query().Get("view")
	if v == "" || v == "brief" {
		return false, nil
	}
	if v == "full" {
		return true, nil
	}
	return false, invalid("view must be brief or full")
}
func limit(r *http.Request) (int, error) {
	s := r.URL.Query().Get("limit")
	if s == "" {
		return 50, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 100 {
		return 0, invalid("limit must be an integer from 1 to 100")
	}
	return n, nil
}
func revision(r *http.Request) (int64, error) {
	s := r.Header.Get("If-Match")
	if s == "" {
		return 0, errcode.New(errcode.PreconditionRequired, "")
	}
	if len(r.Header.Values("If-Match")) != 1 || len(s) < 3 || s[0] != '"' || s[len(s)-1] != '"' {
		return 0, invalid("If-Match must contain a quoted revision")
	}
	v := strings.Trim(s, "\"")
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != v {
		return 0, invalid("If-Match must contain a nonnegative revision")
	}
	return n, nil
}
func etag(w http.ResponseWriter, revision int64) {
	w.Header().Set("ETag", `"`+strconv.FormatInt(revision, 10)+`"`)
}
