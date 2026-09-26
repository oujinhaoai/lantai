// Package httpauth 是身份模块在 HTTP 层的服务端防护：从请求中取出凭据、
// 核对来源与 CSRF，并把可信的调用者上下文放进请求上下文。路由、请求编号与
// 其余错误映射归 transport（T07），这里只处理认证与跨站请求防护，保证网关、
// 分面监听与开发合并模式使用同一套判定。
//
// 规则：
//   - CLI、SDK 用 Authorization: Bearer <会话令牌>。浏览器不会自动附带该头，
//     所以 Bearer 请求不做 CSRF 检查，但仍须通过正常授权。长期凭据不能直接
//     调用业务接口。
//   - 网页用 HttpOnly/Secure/SameSite=Strict 的 lantai_session Cookie。非安全
//     方法（除 GET/HEAD/OPTIONS 外）必须带与允许列表精确相同的 Origin、不能是
//     跨站请求，并带与会话相符的 X-CSRF-Token；Origin 缺失一律拒绝。SameSite
//     只是额外的一层，不替代 Origin 与 CSRF 检查。
//   - 同一请求同时带 Bearer 与会话 Cookie 时拒绝，避免两种凭据语义混用。
//   - 安全方法不得有业务副作用；这是接口约定，由各处理器遵守。
package httpauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

// 协议中的名称。
const (
	CookieName = "lantai_session"
	CSRFHeader = "X-CSRF-Token"
)

// Authenticator 由 identity.Service 实现。
type Authenticator interface {
	AuthenticateToken(ctx context.Context, token string) (authz.Context, error)
	AuthenticateBrowser(ctx context.Context, token, csrf string, requireCSRF bool) (authz.Context, error)
}

// Policy 是浏览器来源策略。
type Policy struct {
	// AllowedOrigins 是允许携带会话 Cookie 发起写请求的来源，精确匹配
	// scheme://host[:port]（例如 https://lantai.home.arpa）。
	AllowedOrigins []string
}

// Credential 标记请求使用的凭据类型。
type Credential string

const (
	CredentialBearer Credential = "bearer"
	CredentialCookie Credential = "cookie"
)

// Caller 是认证后的调用者。
type Caller struct {
	Context    authz.Context
	Credential Credential
}

type callerKey struct{}

// FromContext 返回请求上下文中的调用者。
func FromContext(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// Guard 是认证中间件。
type Guard struct {
	auth    Authenticator
	origins map[string]bool
}

// NewGuard 创建中间件；允许的来源须是不带路径的 http(s) 来源。
func NewGuard(a Authenticator, p Policy) (*Guard, error) {
	g := &Guard{auth: a, origins: map[string]bool{}}
	for _, o := range p.AllowedOrigins {
		norm, ok := normalizeOrigin(o)
		if !ok {
			return nil, errors.New("httpauth: allowed origin must be scheme://host[:port]: " + o)
		}
		g.origins[norm] = true
	}
	return g, nil
}

func normalizeOrigin(o string) (string, bool) {
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), true
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func deny(code errcode.Code, reason, msg string) *errcode.Error {
	return errcode.New(code, msg).WithDetails(errcode.Detail{Reason: reason})
}

// Authenticate 从请求中认证调用者，不写响应，供 transport 自行组合。
func (g *Guard) Authenticate(r *http.Request) (Caller, error) {
	header := r.Header.Get("Authorization")
	cookie, cerr := r.Cookie(CookieName)
	hasCookie := cerr == nil && cookie.Value != ""
	switch {
	case header != "" && hasCookie:
		return Caller{}, deny(errcode.Forbidden, "ambiguous_credentials", "send either a bearer token or the session cookie, not both")
	case header != "":
		scheme, token, ok := strings.Cut(header, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
			return Caller{}, deny(errcode.AuthRequired, "malformed_authorization", "use Authorization: Bearer <session token>")
		}
		c, err := g.auth.AuthenticateToken(r.Context(), strings.TrimSpace(token))
		if err != nil {
			return Caller{}, err
		}
		return Caller{Context: c, Credential: CredentialBearer}, nil
	case hasCookie:
		write := !safeMethod(r.Method)
		if write {
			if err := g.checkOrigin(r); err != nil {
				return Caller{}, err
			}
		}
		c, err := g.auth.AuthenticateBrowser(r.Context(), cookie.Value, r.Header.Get(CSRFHeader), write)
		if err != nil {
			return Caller{}, err
		}
		return Caller{Context: c, Credential: CredentialCookie}, nil
	default:
		return Caller{}, deny(errcode.AuthRequired, "credentials_missing", "authentication is required")
	}
}

// checkOrigin 要求携带 Cookie 的写请求来自允许列表中的精确来源。
func (g *Guard) checkOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return deny(errcode.Forbidden, "origin_missing", "cookie-authenticated writes must carry an Origin header")
	}
	norm, ok := normalizeOrigin(origin)
	if !ok || !g.origins[norm] {
		return deny(errcode.Forbidden, "origin_not_allowed", "this origin may not send cookie-authenticated writes")
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
		return deny(errcode.Forbidden, "cross_site_request", "cross-site cookie-authenticated writes are refused")
	}
	return nil
}

// Wrap 返回认证后才调用 next 的处理器；认证失败时写结构化错误。
func (g *Guard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := g.Authenticate(r)
		if err != nil {
			WriteError(w, err, "")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
	})
}

// WriteError 以错误信封写出错误；非结构化错误一律为 INTERNAL，消息不带内部细节。
func WriteError(w http.ResponseWriter, err error, requestID string) {
	e := errcode.From(err)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	if e.HTTPStatus() == http.StatusUnauthorized {
		h.Set("WWW-Authenticate", `Bearer realm="lantai"`)
	}
	if e.RetryAfter > 0 {
		h.Set("Retry-After", itoa(int64((e.RetryAfter+time.Second-1)/time.Second)))
	}
	w.WriteHeader(e.HTTPStatus())
	_ = json.NewEncoder(w).Encode(e.Envelope(requestID))
}

func itoa(n int64) string {
	if n <= 0 {
		return "1"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// SessionCookie 返回写入浏览器会话令牌的 Cookie：HttpOnly、Secure、
// SameSite=Strict、仅限本主机（不设 Domain）、整个站点路径。
func SessionCookie(token string, expires time.Time) *http.Cookie {
	return &http.Cookie{Name: CookieName, Value: token, Path: "/", Expires: expires.UTC(),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode}
}

// ClearCookie 返回让浏览器删除会话 Cookie 的设置。
func ClearCookie() *http.Cookie {
	return &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode}
}
