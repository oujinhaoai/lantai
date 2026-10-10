// Package client 是 M1 REST 与文件传输客户端。两个独立连接池隔离普通接口
// 和长时间流式传输；所有业务与授权判定均由服务端完成。
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

const MaxJSONBytes int64 = 8 << 20

type Config struct {
	BaseURL      string
	SessionToken string
	// AllowHTTP 只允许显式选择的 loopback 开发入口，绝不关闭 TLS 校验。
	AllowHTTP bool
	// CAFile selects a PEM CA bundle for this client only, without system trust changes.
	CAFile              string
	TLSConfig           *tls.Config
	APITimeout          time.Duration
	TransferTimeout     time.Duration
	TransferConnections int
}
type Client struct {
	base     *url.URL
	api      *http.Client
	transfer *http.Client
	mu       sync.RWMutex
	token    string
}

func New(c Config) (*Client, error) {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("client: server must be an HTTPS origin without credentials, path or query")
	}
	if u.Scheme != "https" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if u.Scheme != "http" || !c.AllowHTTP || !(host == "localhost" || (ip != nil && ip.IsLoopback())) {
			return nil, errors.New("client: HTTPS is required; explicit HTTP development mode is limited to loopback")
		}
	}
	if c.TLSConfig != nil && c.TLSConfig.InsecureSkipVerify {
		return nil, errors.New("client: TLS certificate verification cannot be disabled")
	}
	if c.CAFile != "" {
		if c.TLSConfig != nil {
			return nil, errors.New("client: CAFile and TLSConfig cannot be combined")
		}
		if u.Scheme != "https" {
			return nil, errors.New("client: CAFile requires HTTPS")
		}
		c.TLSConfig, err = tlsConfigFromCAFile(c.CAFile)
		if err != nil {
			return nil, err
		}
	}
	if err = checkToken(c.SessionToken); err != nil {
		return nil, err
	}
	if c.APITimeout == 0 {
		c.APITimeout = 30 * time.Second
	}
	if c.TransferTimeout == 0 {
		c.TransferTimeout = 24 * time.Hour
	}
	if c.TransferConnections == 0 {
		c.TransferConnections = 4
	}
	if c.APITimeout < 0 || c.TransferTimeout < 0 || c.TransferConnections < 1 || c.TransferConnections > 64 {
		return nil, errors.New("client: invalid timeout or transfer concurrency")
	}
	transport := func(max int) *http.Transport {
		t := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 32, MaxIdleConnsPerHost: max, MaxConnsPerHost: max, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, ExpectContinueTimeout: time.Second, DisableCompression: true}
		if c.TLSConfig != nil {
			t.TLSClientConfig = c.TLSConfig.Clone()
		}
		return t
	}
	redirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: u, token: c.SessionToken, api: &http.Client{Transport: transport(16), Timeout: c.APITimeout, CheckRedirect: redirect}, transfer: &http.Client{Transport: transport(c.TransferConnections), Timeout: c.TransferTimeout, CheckRedirect: redirect}}, nil
}
func checkToken(token string) error {
	if strings.ContainsAny(token, "\r\n\x00") {
		return errors.New("client: invalid bearer token")
	}
	return nil
}
func (c *Client) SetSessionToken(token string) error {
	if err := checkToken(token); err != nil {
		return err
	}
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
	return nil
}
func (c *Client) Origin() string { return c.base.Scheme + "://" + c.base.Host }
func (c *Client) Close()         { c.api.CloseIdleConnections(); c.transfer.CloseIdleConnections() }
func (c *Client) authorize(r *http.Request) {
	c.mu.RLock()
	token := c.token
	c.mu.RUnlock()
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
}

type Options struct {
	IdempotencyKey string
	IfMatch        string
}
type Response struct {
	Status    int
	Body      json.RawMessage
	ETag      string
	RequestID string
}

func (r Response) Decode(v any) error {
	if len(r.Body) == 0 {
		return nil
	}
	return json.Unmarshal(r.Body, v)
}

// APIError 保留服务端登记过的机器错误，不把包含授权 URL 的传输错误放入日志。
type APIError struct {
	Status int
	Body   errcode.Body
	// Protocol means a gateway/server returned an invalid error envelope.
	Protocol bool
}

func (e *APIError) Error() string { return string(e.Body.Code) + ": " + e.Body.Message }

// Do 只访问公共 /api/v1/ 入口。写请求不自动重试；调用方保存幂等键并显式续办。
func (c *Client) Do(ctx context.Context, method, path string, body any, opts Options) (Response, error) {
	rel, err := url.Parse(path)
	if err != nil || rel.IsAbs() || rel.Host != "" || !strings.HasPrefix(rel.Path, "/api/v1/") || rel.Fragment != "" {
		return Response{}, errors.New("client: invalid API path")
	}
	// Path is already percent-decoded, so encoded dot segments cannot bypass
	// this check and be normalized differently by a gateway.
	for _, segment := range strings.Split(rel.Path, "/") {
		if segment == "." || segment == ".." {
			return Response{}, errors.New("client: invalid API path")
		}
	}
	var raw []byte
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return Response{}, err
		}
		if int64(len(raw)) > MaxJSONBytes {
			return Response{}, errors.New("client: JSON request exceeds 8 MiB")
		}
	}
	u := c.base.ResolveReference(rel)
	if origin(u) != origin(c.base) || !strings.HasPrefix(u.Path, "/api/v1/") {
		return Response{}, errors.New("client: invalid API path")
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(raw))
	if err != nil {
		return Response{}, errors.New("client: cannot construct API request")
	}
	// net/http treats Idempotency-Key plus GetBody as permission for an implicit
	// replay after a reused connection fails. Recovery belongs to the caller,
	// which must reconcile operation state before it sends a commit again.
	req.GetBody = nil
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authorize(req)
	if opts.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", opts.IdempotencyKey)
	}
	if opts.IfMatch != "" {
		req.Header.Set("If-Match", opts.IfMatch)
	}
	res, err := c.api.Do(req)
	if err != nil {
		return Response{}, safeNetworkError(ctx, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxJSONBytes+1))
	if err != nil {
		return Response{}, safeNetworkError(ctx, err)
	}
	if int64(len(data)) > MaxJSONBytes {
		return Response{}, errors.New("client: JSON response exceeds 8 MiB")
	}
	out := Response{Status: res.StatusCode, Body: data, ETag: res.Header.Get("ETag"), RequestID: res.Header.Get("X-Request-ID")}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var request any
		_ = json.Unmarshal(raw, &request)
		return out, c.responseError(res.StatusCode, data, requestSecrets(request)...)
	}
	if len(data) > 0 && !json.Valid(data) {
		return Response{}, errors.New("client: server returned invalid JSON")
	}
	return out, nil
}

var sensitiveURL = regexp.MustCompile(`(?i)(?:https?://|otpauth://|/xfer/)[^\s"<>]+`)

func requestSecrets(v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, value := range x {
			switch k {
			case "token", "password", "code", "secret":
				if s, ok := value.(string); ok && s != "" {
					out = append(out, s)
				}
			default:
				out = append(out, requestSecrets(value)...)
			}
		}
	case []any:
		for _, value := range x {
			out = append(out, requestSecrets(value)...)
		}
	}
	return out
}
func (c *Client) responseError(status int, data []byte, secrets ...string) error {
	var e errcode.Envelope
	if json.Unmarshal(data, &e) != nil || errcode.CheckBody(e.Error) != nil {
		return &APIError{Status: status, Protocol: true, Body: errcode.New(errcode.Internal, "server returned an invalid error response").Envelope("").Error}
	}
	c.mu.RLock()
	token := c.token
	c.mu.RUnlock()
	secrets = append(secrets, token)
	var scrub func(any) any
	scrub = func(v any) any {
		switch x := v.(type) {
		case string:
			x = sensitiveURL.ReplaceAllString(x, "[redacted-url]")
			for _, s := range secrets {
				if s != "" {
					x = strings.ReplaceAll(x, s, "[redacted]")
				}
			}
			return x
		case map[string]any:
			for k, v := range x {
				x[k] = scrub(v)
			}
		case []any:
			for i, v := range x {
				x[i] = scrub(v)
			}
		}
		return v
	}
	e.Error.Message = scrub(e.Error.Message).(string)
	e.Error.Hint = scrub(e.Error.Hint).(string)
	for i, ref := range e.Error.Refs {
		e.Error.Refs[i] = scrub(ref).(string)
	}
	for i := range e.Error.Details {
		d := &e.Error.Details[i]
		d.Message = scrub(d.Message).(string)
		d.Ref = scrub(d.Ref).(string)
		if d.Data != nil {
			d.Data = scrub(d.Data).(map[string]any)
		}
	}
	return &APIError{Status: status, Body: e.Error}
}
func safeNetworkError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	// Keep a classified cause for errors.Is while displaying no URL, path, token or peer details.
	return &networkError{cause: err}
}

type networkError struct{ cause error }

func (e *networkError) Error() string {
	return "client: request failed; retain the transfer state and retry the same operation"
}
func (e *networkError) Unwrap() error { return e.cause }

func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme + "://" + net.JoinHostPort(u.Hostname(), port))
}

// authorizedURL accepts only an URL returned by this server's authorization API.
// Even a redirect within the same host is not followed; cross-origin credentials never leave.
func (c *Client) authorizedURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || raw == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("client: invalid authorized transfer URL")
	}
	u = c.base.ResolveReference(u)
	if origin(u) != origin(c.base) || !strings.HasPrefix(u.Path, "/xfer/") {
		return nil, errors.New("client: authorized transfer URL must use the configured gateway origin and /xfer/ path")
	}
	return u, nil
}
func (c *Client) transferRequest(ctx context.Context, method, authorizedURL string, body io.Reader, size int64, headers http.Header) (*http.Response, error) {
	u, err := c.authorizedURL(authorizedURL)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, errors.New("client: invalid transfer request")
	}
	r.ContentLength = size
	if size == 0 && body != nil {
		r.Body = http.NoBody
	}
	c.authorize(r)
	for k, vs := range headers {
		switch k {
		case "Range", "Content-Type", "Lantai-Part-Sha256", "If-Range":
			for _, v := range vs {
				r.Header.Add(k, v)
			}
		default:
			return nil, fmt.Errorf("client: unsupported transfer header %q", k)
		}
	}
	res, err := c.transfer.Do(r)
	if err != nil {
		return nil, safeNetworkError(ctx, err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, MaxJSONBytes))
		return nil, c.responseError(res.StatusCode, raw)
	}
	return res, nil
}
