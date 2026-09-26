package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
)

// SessionInfo 是会话的可公开描述（不含令牌与令牌摘要）。
type SessionInfo struct {
	SessionID       ids.ID              `json:"session_id"`
	PrincipalID     ids.ID              `json:"principal_id"`
	PrincipalKind   authz.PrincipalKind `json:"principal_kind"`
	Kind            string              `json:"session_kind"`
	Channel         string              `json:"channel"`
	Scopes          []Scope             `json:"scopes"`
	Projects        []ids.ID            `json:"projects,omitempty"`
	Actions         []authz.Action      `json:"actions,omitempty"`
	ParentSessionID ids.ID              `json:"parent_session_id,omitempty"`
	DelegatedBy     ids.ID              `json:"delegated_by,omitempty"`
	CredentialID    ids.ID              `json:"credential_id,omitempty"`
	Purpose         string              `json:"purpose,omitempty"`
	Model           string              `json:"model,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
	ExpiresAt       time.Time           `json:"expires_at"`
}

func (r sessionRow) info() SessionInfo {
	return SessionInfo{
		SessionID: r.ID, PrincipalID: r.PrincipalID, PrincipalKind: r.PrincipalKind, Kind: r.Kind, Channel: r.Channel,
		Scopes: r.Scopes, Projects: r.Projects, Actions: r.Actions, ParentSessionID: r.ParentID, DelegatedBy: r.DelegatedBy,
		CredentialID: r.CredentialID, Purpose: r.Purpose, Model: r.Model, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt,
	}
}

// IssuedSession 是新会话。Token 只在这里返回一次：CLI 放在进程内存或环境
// 变量里，浏览器放在 HttpOnly Cookie 里；任何一方都不得写日志。
type IssuedSession struct {
	Token string
	// CSRFToken 只对浏览器会话给出，写请求放在 X-CSRF-Token 头中。
	CSRFToken string
	Context   authz.Context
	Session   SessionInfo
}

// verified 是一次会话核对的结果；problem 非空表示会话已不可用。
type verified struct {
	sess      sessionRow
	principal Principal
	policyRev int64
	problem   errcode.Code
}

// maxSessionDepth 限制子会话链的深度（会话 → 子会话/委托 → 不再派生）。
const maxSessionDepth = 2

// verifyRow 按当前权威状态核对会话：未结束、未过期、恢复代次与主体
// auth_epoch 仍是当前值、主体仍为同一类别且未停用、换取它的长期凭据未吊销、
// 父会话仍有效、委托方仍有效；恢复会话还要求其恢复操作仍在进行。
func (s *Service) verifyRow(ctx context.Context, r sessionRow, depth int) (verified, error) {
	v := verified{sess: r}
	now := s.now()
	if !r.EndedAt.IsZero() {
		v.problem = errcode.TokenRevoked
		return v, nil
	}
	if !now.Before(r.ExpiresAt) {
		v.problem = errcode.TokenExpired
		return v, nil
	}
	epoch, err := s.recoveryEpoch(ctx)
	if err != nil {
		return v, err
	}
	p, err := loadPrincipal(ctx, s.main, r.PrincipalID)
	switch {
	case errors.Is(err, errNotFound):
		v.problem = errcode.TokenRevoked
		return v, nil
	case err != nil:
		return v, err
	}
	v.principal = p
	if r.RecoveryEpoch != epoch || p.State != StateActive || p.AuthEpoch != r.AuthEpoch || p.Kind != r.PrincipalKind {
		v.problem = errcode.TokenRevoked
		return v, nil
	}
	var revokedSession int
	if err := s.main.QueryRowContext(ctx, `SELECT count(*) FROM identity_session_revocations WHERE session_id = ?`, r.ID).Scan(&revokedSession); err != nil {
		return v, err
	}
	if revokedSession > 0 {
		v.problem = errcode.TokenRevoked
		return v, nil
	}
	if r.CredentialID != "" {
		var revoked sql.NullInt64
		err := s.main.QueryRowContext(ctx, `SELECT revoked_at FROM identity_credentials WHERE credential_id = ?`, r.CredentialID).Scan(&revoked)
		if errors.Is(err, sql.ErrNoRows) || revoked.Valid {
			v.problem = errcode.TokenRevoked
			return v, nil
		}
		if err != nil {
			return v, err
		}
	}
	if r.ParentID != "" {
		if depth >= maxSessionDepth {
			v.problem = errcode.TokenRevoked
			return v, nil
		}
		parent, err := loadSession(ctx, s.runtime, r.ParentID)
		if errors.Is(err, errNotFound) {
			v.problem = errcode.TokenRevoked
			return v, nil
		}
		if err != nil {
			return v, err
		}
		pv, err := s.verifyRow(ctx, parent, depth+1)
		if err != nil {
			return v, err
		}
		if pv.problem != "" || parent.PrincipalID != r.PrincipalID {
			v.problem = errcode.TokenRevoked
			return v, nil
		}
	}
	if r.DelegatedBy != "" {
		d, err := loadPrincipal(ctx, s.main, r.DelegatedBy)
		if err != nil && !errors.Is(err, errNotFound) {
			return v, err
		}
		if err != nil || d.State != StateActive || d.AuthEpoch != r.DelegateAuthEpoch {
			v.problem = errcode.TokenRevoked
			return v, nil
		}
	}
	if r.Kind == sessionRecovery {
		acct, err := loadAccount(ctx, s.main, p.ID)
		if err != nil && !errors.Is(err, errNotFound) {
			return v, err
		}
		if err != nil || acct.FactorState == factorEnrolled || acct.PendingOperation != r.OperationID {
			v.problem = errcode.TokenRevoked
			return v, nil
		}
	}
	if v.policyRev, err = policyRevision(ctx, s.main); err != nil {
		return v, err
	}
	return v, nil
}

// sessionDepth 返回会话之上的父会话层数。
func (s *Service) sessionDepth(ctx context.Context, r sessionRow) (int, error) {
	depth := 0
	for r.ParentID != "" && depth <= maxSessionDepth {
		parent, err := loadSession(ctx, s.runtime, r.ParentID)
		if err != nil {
			return 0, err
		}
		r = parent
		depth++
	}
	return depth, nil
}

// verify 按会话 ID 核对；会话不存在返回 AUTH_REQUIRED。
func (s *Service) verify(ctx context.Context, id ids.ID) (verified, error) {
	if !id.Valid() {
		return verified{problem: errcode.AuthRequired}, nil
	}
	r, err := loadSession(ctx, s.runtime, id)
	if errors.Is(err, errNotFound) {
		return verified{problem: errcode.AuthRequired}, nil
	}
	if err != nil {
		return verified{}, err
	}
	return s.verifyRow(ctx, r, 0)
}

func (s *Service) contextOf(v verified) authz.Context {
	return authz.Context{
		InstanceID: s.instance, PrincipalID: v.principal.ID, PrincipalKind: v.principal.Kind, SessionID: v.sess.ID,
		DelegatedBy: v.sess.DelegatedBy, AuthEpoch: v.principal.AuthEpoch, RecoveryEpoch: v.sess.RecoveryEpoch,
		PolicyRevision: v.policyRev, Projects: slices.Clone(v.sess.Projects), ExpiresAt: v.sess.ExpiresAt,
	}
}

func sessionErr(code errcode.Code) error {
	switch code {
	case errcode.TokenExpired:
		return errcode.New(code, "the session has expired; start a new session")
	case errcode.TokenRevoked:
		return errcode.New(code, "the session is no longer valid; authenticate again")
	default:
		return errcode.New(errcode.AuthRequired, "authentication is required")
	}
}

// VerifySession 实现 authz.SessionVerifier：按当前权威状态核对会话并返回可信
// 上下文。已结束、旧代次、主体停用或凭据吊销返回 TOKEN_REVOKED，过期返回
// TOKEN_EXPIRED，未知返回 AUTH_REQUIRED。
func (s *Service) VerifySession(ctx context.Context, id ids.ID) (authz.Context, error) {
	v, err := s.verify(ctx, id)
	if err != nil {
		return authz.Context{}, err
	}
	if v.problem != "" {
		return authz.Context{}, sessionErr(v.problem)
	}
	return s.contextOf(v), nil
}

// AuthenticateToken 验证 Bearer 会话令牌（CLI、SDK、adapter）。长期凭据不能
// 直接调用业务接口，浏览器会话令牌也不能脱离 Cookie 当作 Bearer 使用。
func (s *Service) AuthenticateToken(ctx context.Context, token string) (authz.Context, error) {
	if strings.HasPrefix(token, CredentialPrefix) {
		return authz.Context{}, errcode.New(errcode.AuthRequired, "long-lived credentials can only be exchanged for a session").
			WithDetails(errcode.Detail{Reason: "long_lived_token_not_accepted"})
	}
	v, err := s.byToken(ctx, token)
	if err != nil {
		return authz.Context{}, err
	}
	if v.sess.Channel == ChannelBrowser {
		return authz.Context{}, errcode.New(errcode.AuthRequired, "browser sessions are only accepted from the session cookie").
			WithDetails(errcode.Detail{Reason: "channel_mismatch"})
	}
	return s.contextOf(v), nil
}

// AuthenticateBrowser 验证浏览器 Cookie 会话；requireCSRF 为 true 时（写请求）
// 还要求 CSRF 令牌与该会话相符。Origin 检查由 HTTP 层先行完成。
func (s *Service) AuthenticateBrowser(ctx context.Context, token, csrf string, requireCSRF bool) (authz.Context, error) {
	v, err := s.byToken(ctx, token)
	if err != nil {
		return authz.Context{}, err
	}
	if v.sess.Channel != ChannelBrowser {
		return authz.Context{}, errcode.New(errcode.AuthRequired, "this session cannot be used from a browser cookie").
			WithDetails(errcode.Detail{Reason: "channel_mismatch"})
	}
	if requireCSRF && !s.csrfValid(v.sess.ID, csrf) {
		return authz.Context{}, errcode.New(errcode.Forbidden, "missing or invalid CSRF token").
			WithDetails(errcode.Detail{Reason: "csrf_invalid"})
	}
	return s.contextOf(v), nil
}

func (s *Service) byToken(ctx context.Context, token string) (verified, error) {
	if !wellFormedToken(token, SessionPrefix) {
		return verified{}, sessionErr(errcode.AuthRequired)
	}
	r, err := loadSessionByToken(ctx, s.runtime, tokenHash(token))
	if errors.Is(err, errNotFound) {
		return verified{}, sessionErr(errcode.AuthRequired)
	}
	if err != nil {
		return verified{}, err
	}
	v, err := s.verifyRow(ctx, r, 0)
	if err != nil {
		return v, err
	}
	if v.problem != "" {
		return v, sessionErr(v.problem)
	}
	return v, nil
}

// issue 写入新会话与 session.started 事件；check 非空时在同一个 runtime.db
// 写事务中先执行（例如同时会话数上限），保证并发签发也不越过它。
func (s *Service) issue(ctx context.Context, r sessionRow, check func(ctx context.Context, tx *sql.Tx) error) (IssuedSession, error) {
	token, hash, err := newToken(SessionPrefix)
	if err != nil {
		return IssuedSession{}, err
	}
	if r.ID, err = s.newID(); err != nil {
		return IssuedSession{}, err
	}
	r.TokenHash, r.CreatedAt = hash, s.now()
	if r.Scopes == nil {
		r.Scopes = []Scope{}
	}
	op, err := s.newID()
	if err != nil {
		return IssuedSession{}, err
	}
	info := r.info()
	ev, err := s.event(evParams{typ: EvSessionStarted, aggType: "session", aggID: r.ID, revision: 1,
		actor: r.PrincipalID, session: r.ID, operation: op, payload: info})
	if err != nil {
		return IssuedSession{}, err
	}
	if err := inTx(ctx, s.runtime, func(tx *sql.Tx) error {
		if check != nil {
			if err := check(ctx, tx); err != nil {
				return err
			}
		}
		if err := insertSession(ctx, tx, r); err != nil {
			return err
		}
		return s.rtStore.AppendEvents(ctx, tx, op, []event.Envelope{ev})
	}); err != nil {
		return IssuedSession{}, err
	}
	out := IssuedSession{Token: token, Session: info}
	v, err := s.verifyRow(ctx, r, 0)
	if err != nil {
		return IssuedSession{}, err
	}
	if v.problem != "" {
		// 签发期间主体被并发撤权：新会话从一开始就无效，按撤权处理。
		return IssuedSession{}, sessionErr(v.problem)
	}
	out.Context = s.contextOf(v)
	if r.Channel == ChannelBrowser {
		out.CSRFToken = s.CSRFToken(r.ID)
	}
	return out, nil
}

// SessionRequest 是换取或收窄会话时的请求。请求中没有“主体类别”或“是否
// 为人”一类字段：会话类别只来自服务端登记的主体。
type SessionRequest struct {
	// Scopes 与 Projects 只能收窄；为空表示沿用上限。
	Scopes   []Scope
	Projects []ids.ID
	// TTL 为空或超过上限时取上限。
	TTL time.Duration
	// Channel 为 cli 或 api。
	Channel string
	Purpose string
	// Model 是客户端自报的模型名，只作记录，不参与任何判定。
	Model string
}

func (r SessionRequest) check() error {
	if r.Channel != ChannelCLI && r.Channel != ChannelAPI {
		return fixRequest("channel must be cli or api")
	}
	for _, v := range []string{r.Purpose, r.Model} {
		if !shortTextRE.MatchString(v) {
			return fixRequest("purpose and model must be short plain text")
		}
	}
	if r.TTL < 0 {
		return fixRequest("ttl must not be negative")
	}
	return nil
}

func (s *Service) ttl(requested, max time.Duration, cap time.Time) (time.Time, error) {
	now := s.now()
	d := s.cfg.SessionTTL
	if max > 0 && max < d {
		d = max
	}
	if requested > 0 && requested < d {
		d = requested
	}
	exp := now.Add(d)
	if !cap.IsZero() && cap.Before(exp) {
		exp = cap
	}
	if !now.Before(exp) {
		return time.Time{}, errcode.New(errcode.TokenExpired, "the parent credential or session has expired")
	}
	return clock.Truncate(exp), nil
}

// sessionQuota 返回在签发事务中执行档案里同时会话数上限的检查。
func (s *Service) sessionQuota(p Principal) func(ctx context.Context, tx *sql.Tx) error {
	if p.Profile.MaxSessions == 0 {
		return nil
	}
	return func(ctx context.Context, tx *sql.Tx) error { return s.checkSessionQuota(ctx, tx, p) }
}

func (s *Service) checkSessionQuota(ctx context.Context, q commands.DBTX, p Principal) error {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM identity_sessions WHERE principal_id = ?
		AND ended_at IS NULL AND expires_at > ? AND auth_epoch = ? AND session_kind = 'normal' AND parent_session_id IS NULL`,
		p.ID, clock.Millis(s.now()), p.AuthEpoch).Scan(&n); err != nil {
		return err
	}
	if n >= p.Profile.MaxSessions {
		return errcode.Newf(errcode.QuotaExceeded, "%s already has %d concurrent sessions", p.Name, n)
	}
	return nil
}

// credentialRow 是长期凭据。
type credentialRow struct {
	ID          ids.ID
	PrincipalID ids.ID
	Prefix      string
	Scopes      []Scope
	Projects    []ids.ID
	Label       string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	RevokedAt   time.Time
}

func scanCredential(row interface{ Scan(...any) error }) (credentialRow, error) {
	var c credentialRow
	var scopes, projects string
	var created, expires int64
	var revoked sql.NullInt64
	err := row.Scan(&c.ID, &c.PrincipalID, &c.Prefix, &scopes, &projects, &c.Label, &created, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return c, errNotFound
	}
	if err != nil {
		return c, err
	}
	c.CreatedAt, c.ExpiresAt, c.RevokedAt = clock.FromMillis(created), clock.FromMillis(expires), nullTime(revoked)
	if c.Scopes, err = decodeList[Scope](scopes); err != nil {
		return c, err
	}
	c.Projects, err = decodeList[ids.ID](projects)
	return c, err
}

const credentialCols = `credential_id, principal_id, prefix, scopes, projects, label, created_at, expires_at, revoked_at`

// ExchangeToken 用 Agent、节点、执行器、运行器或服务的长期凭据换取会话。
// 会话的主体类别取自登记，范围与项目只能在凭据之内收窄，有效期不超过凭据
// 剩余期限与会话上限。长期凭据不能换出人的会话。
func (s *Service) ExchangeToken(ctx context.Context, token string, req SessionRequest) (IssuedSession, error) {
	if err := req.check(); err != nil {
		return IssuedSession{}, err
	}
	if !wellFormedToken(token, CredentialPrefix) {
		return IssuedSession{}, invalidCredentials()
	}
	ctx, release, err := s.write(ctx, false)
	if err != nil {
		return IssuedSession{}, err
	}
	defer release()
	c, err := scanCredential(s.main.QueryRowContext(ctx, `SELECT `+credentialCols+` FROM identity_credentials WHERE secret_hash = ?`, tokenHash(token)))
	if errors.Is(err, errNotFound) {
		return IssuedSession{}, invalidCredentials()
	}
	if err != nil {
		return IssuedSession{}, err
	}
	switch {
	case !c.RevokedAt.IsZero():
		return IssuedSession{}, sessionErr(errcode.TokenRevoked)
	case !s.now().Before(c.ExpiresAt):
		return IssuedSession{}, sessionErr(errcode.TokenExpired)
	}
	p, err := loadPrincipal(ctx, s.main, c.PrincipalID)
	if err != nil {
		return IssuedSession{}, err
	}
	if p.State != StateActive {
		return IssuedSession{}, sessionErr(errcode.TokenRevoked)
	}
	if p.Kind == authz.Human {
		return IssuedSession{}, errcode.New(errcode.Forbidden, "long-lived credentials cannot create human sessions")
	}
	want := req.Scopes
	if len(want) == 0 {
		want = c.Scopes
	}
	scopes, err := normalizeScopes(want, intersect(c.Scopes, kindScopes[p.Kind]))
	if err != nil {
		return IssuedSession{}, errcode.Newf(errcode.Forbidden, "%v", err)
	}
	projects, err := normalizeProjects(req.Projects)
	if err != nil {
		return IssuedSession{}, fixRequest("%v", err)
	}
	if len(projects) == 0 {
		projects = c.Projects
	} else if !subset(projects, c.Projects) {
		return IssuedSession{}, errcode.New(errcode.Forbidden, "the session cannot include projects outside the credential")
	}
	exp, err := s.ttl(req.TTL, 0, c.ExpiresAt)
	if err != nil {
		return IssuedSession{}, err
	}
	epoch, err := s.recoveryEpoch(ctx)
	if err != nil {
		return IssuedSession{}, err
	}
	return s.issue(ctx, sessionRow{PrincipalID: p.ID, PrincipalKind: p.Kind, Kind: sessionNormal, Channel: req.Channel,
		CredentialID: c.ID, Purpose: req.Purpose, Model: req.Model, Scopes: scopes, Projects: projects,
		AuthEpoch: p.AuthEpoch, RecoveryEpoch: epoch, ExpiresAt: exp}, s.sessionQuota(p))
}

func intersect[T comparable](a, b []T) []T {
	var out []T
	for _, v := range a {
		if slices.Contains(b, v) {
			out = append(out, v)
		}
	}
	return out
}

// LoginRequest 是人的登录请求：口令加当前动态码。
type LoginRequest struct {
	Name     string
	Password string
	Code     string
	// Source 是请求来源（例如客户端地址），用于按来源限速。
	Source string
	// Channel 为 browser（得到 Cookie 会话与 CSRF 令牌）或 cli（进程内存会话）。
	Channel  string
	Scopes   []Scope
	Projects []ids.ID
	TTL      time.Duration
}

// Login 用口令与动态码为人开启普通会话。登录不开启任何敏感操作窗口：
// 凭据、策略等变更仍需针对具体动作的 Challenge/HumanGrant。成功的动态码
// 时间步随即作废，不能再用于挑战。失败按主体与来源限速。
func (s *Service) Login(ctx context.Context, req LoginRequest) (IssuedSession, error) {
	if req.Channel != ChannelBrowser && req.Channel != ChannelCLI {
		return IssuedSession{}, fixRequest("channel must be browser or cli")
	}
	want := req.Scopes
	if len(want) == 0 {
		want = kindScopes[authz.Human]
	}
	scopes, err := normalizeScopes(want, kindScopes[authz.Human])
	if err != nil {
		return IssuedSession{}, fixRequest("%v", err)
	}
	projects, err := normalizeProjects(req.Projects)
	if err != nil {
		return IssuedSession{}, fixRequest("%v", err)
	}
	source := normalizeSource(req.Source)
	p, err := loadPrincipalByName(ctx, s.main, req.Name)
	if err != nil && !errors.Is(err, errNotFound) {
		return IssuedSession{}, err
	}
	// 先记下这次尝试（限速检查在同一事务）；只有完全成功才撤销。口令校验
	// 很慢，放在任何锁之外进行。
	seq, err := s.reserveAttempt(ctx, p.ID, source, failLogin)
	if err != nil {
		return IssuedSession{}, err
	}
	p, rec, ok, err := s.verifyPassword(ctx, req.Name, req.Password)
	if err != nil {
		return IssuedSession{}, err
	}
	if !ok {
		return IssuedSession{}, invalidCredentials()
	}
	ctx, release, err := s.write(ctx, false)
	if err != nil {
		return IssuedSession{}, err
	}
	defer release()
	acct, err := loadAccount(ctx, s.main, p.ID)
	if err != nil {
		return IssuedSession{}, err
	}
	if acct.FactorState != factorEnrolled {
		// 口令正确：不算失败，但仍不能登录。
		if err := inTx(ctx, s.main, func(tx *sql.Tx) error { return dropAttempt(ctx, tx, seq) }); err != nil {
			return IssuedSession{}, err
		}
		return IssuedSession{}, errcode.New(errcode.AuthRequired, "the authenticator must be set up before signing in").
			WithDetails(errcode.Detail{Reason: "factor_setup_required"})
	}
	err = inTx(ctx, s.main, func(tx *sql.Tx) error {
		cur, err := loadPassword(ctx, tx, p.ID)
		if err != nil || string(cur.Hash) != string(rec.Hash) {
			return invalidCredentials()
		}
		res, err := s.checkFactorTx(ctx, tx, p.ID, req.Code)
		if err != nil {
			return err
		}
		switch res {
		case totp.Accepted:
			return dropAttempt(ctx, tx, seq)
		case totp.Replayed:
			return errcode.New(errcode.TOTPReplay, "this code has already been used; wait for the next code")
		default:
			return invalidCredentials()
		}
	})
	if err != nil {
		return IssuedSession{}, err
	}
	exp, err := s.ttl(req.TTL, 0, time.Time{})
	if err != nil {
		return IssuedSession{}, err
	}
	epoch, err := s.recoveryEpoch(ctx)
	if err != nil {
		return IssuedSession{}, err
	}
	return s.issue(ctx, sessionRow{PrincipalID: p.ID, PrincipalKind: authz.Human, Kind: sessionNormal, Channel: req.Channel,
		Scopes: scopes, Projects: projects, AuthEpoch: p.AuthEpoch, RecoveryEpoch: epoch, ExpiresAt: exp}, nil)
}

// NarrowSession 为 Agent 等非人主体的会话派生更窄的子会话（例如交给子
// Agent 或子进程）：范围、项目与有效期只能收窄，父会话结束或失效时子会话
// 随之失效，链深度有上限。人的会话不能派生子令牌（要交给工具用委托），
// 浏览器会话、恢复会话与委托会话也不能派生。
func (s *Service) NarrowSession(ctx context.Context, who authz.Context, req SessionRequest) (IssuedSession, error) {
	if err := req.check(); err != nil {
		return IssuedSession{}, err
	}
	ctx, release, err := s.write(ctx, false)
	if err != nil {
		return IssuedSession{}, err
	}
	defer release()
	v, err := s.current(ctx, who)
	if err != nil {
		return IssuedSession{}, err
	}
	if err := s.require(ctx, s.main, v, ActNarrowSession, authz.Resource{}); err != nil {
		return IssuedSession{}, err
	}
	parent := v.sess
	if v.principal.Kind == authz.Human {
		return IssuedSession{}, errcode.New(errcode.Forbidden, "a person's session cannot be narrowed into a child token; delegate to a service principal instead")
	}
	if parent.Channel == ChannelBrowser || parent.Kind != sessionNormal {
		return IssuedSession{}, errcode.New(errcode.Forbidden, "this session cannot derive child sessions")
	}
	depth, err := s.sessionDepth(ctx, parent)
	if err != nil {
		return IssuedSession{}, err
	}
	if depth >= maxSessionDepth {
		return IssuedSession{}, errcode.Newf(errcode.Forbidden, "child sessions can be nested at most %d levels", maxSessionDepth)
	}
	want := req.Scopes
	if len(want) == 0 {
		want = parent.Scopes
	}
	scopes, err := normalizeScopes(want, parent.Scopes)
	if err != nil {
		return IssuedSession{}, errcode.Newf(errcode.Forbidden, "%v", err)
	}
	projects, err := normalizeProjects(req.Projects)
	if err != nil {
		return IssuedSession{}, fixRequest("%v", err)
	}
	if len(projects) == 0 {
		projects = parent.Projects
	} else if !subset(projects, parent.Projects) {
		return IssuedSession{}, errcode.New(errcode.Forbidden, "a child session cannot widen the project scope")
	}
	exp, err := s.ttl(req.TTL, 0, parent.ExpiresAt)
	if err != nil {
		return IssuedSession{}, err
	}
	return s.issue(ctx, sessionRow{PrincipalID: v.principal.ID, PrincipalKind: v.principal.Kind, Kind: sessionNormal,
		Channel: req.Channel, CredentialID: parent.CredentialID, ParentID: parent.ID, Purpose: req.Purpose, Model: req.Model,
		Scopes: scopes, Projects: projects, AuthEpoch: v.principal.AuthEpoch, RecoveryEpoch: parent.RecoveryEpoch, ExpiresAt: exp}, nil)
}

// DelegationRequest 描述交给代为调用者（插件宿主、adapter、服务等 service/runner
// 主体）的短期能力。
type DelegationRequest struct {
	// Delegate 是实际调用者的主体 ID，与被委托主体分别审计。
	Delegate ids.ID
	// Actions 是允许的项目内动作，必须显式列出；Projects 必须显式列出。
	Actions  []authz.Action
	Projects []ids.ID
	TTL      time.Duration
	Purpose  string
}

// IssueDelegated 为 parent 会话签发交给 req.Delegate 的短期能力：主体仍是
// parent 的主体（权限来源），DelegatedBy 记录实际调用者；动作与项目显式限定，
// 且都在 parent 当前权限之内；有效期不超过 DelegationMaxTTL 与 parent 剩余
// 期限。parent 或委托方任一失效，能力随即失效。插件拿到的只是这一短期
// 会话令牌，永远拿不到长期凭据、人类授权或会话 Cookie。
func (s *Service) IssueDelegated(ctx context.Context, parent authz.Context, req DelegationRequest) (IssuedSession, error) {
	if !shortTextRE.MatchString(req.Purpose) {
		return IssuedSession{}, fixRequest("purpose must be short plain text")
	}
	if len(req.Actions) == 0 || len(req.Actions) > 64 {
		return IssuedSession{}, fixRequest("a delegation must list between 1 and 64 actions")
	}
	ctx, release, err := s.write(ctx, false)
	if err != nil {
		return IssuedSession{}, err
	}
	defer release()
	v, err := s.current(ctx, parent)
	if err != nil {
		return IssuedSession{}, err
	}
	if v.sess.Kind != sessionNormal {
		return IssuedSession{}, errcode.New(errcode.Forbidden, "only normal sessions can delegate")
	}
	d, err := loadPrincipal(ctx, s.main, req.Delegate)
	if errors.Is(err, errNotFound) {
		return IssuedSession{}, errcode.New(errcode.NotFound, "delegate not found")
	}
	if err != nil {
		return IssuedSession{}, err
	}
	if d.State != StateActive || (d.Kind != authz.Service && d.Kind != authz.Runner) || d.ID == v.principal.ID {
		return IssuedSession{}, errcode.New(errcode.Forbidden, "the delegate must be an active service or runner principal")
	}
	projects, err := normalizeProjects(req.Projects)
	if err != nil || len(projects) == 0 {
		return IssuedSession{}, fixRequest("a delegation must list its projects")
	}
	if !subset(projects, v.sess.Projects) {
		return IssuedSession{}, errcode.New(errcode.Forbidden, "a delegation cannot widen the project scope")
	}
	acts := slices.Clone(req.Actions)
	slices.Sort(acts)
	acts = slices.Compact(acts)
	scopes := []Scope{ScopeSelf}
	for _, a := range acts {
		spec, ok := Spec(a)
		if !ok || spec.Level != ProjectLevel || spec.HumanOnly || spec.HumanGrant || spec.RecoveryOnly || spec.Scope == ScopeAdmin {
			return IssuedSession{}, errcode.Newf(errcode.Forbidden, "action %s cannot be delegated", a)
		}
		for _, p := range projects {
			code, err := s.decide(ctx, s.main, v, spec, authz.Resource{ProjectID: p})
			if err != nil {
				return IssuedSession{}, err
			}
			if code != "" {
				return IssuedSession{}, errcode.Newf(errcode.Forbidden, "the delegating session is not allowed %s in project %s", a, p)
			}
		}
		if !slices.Contains(scopes, spec.Scope) {
			scopes = append(scopes, spec.Scope)
		}
	}
	slices.Sort(scopes)
	exp, err := s.ttl(req.TTL, s.cfg.DelegationMaxTTL, v.sess.ExpiresAt)
	if err != nil {
		return IssuedSession{}, err
	}
	return s.issue(ctx, sessionRow{PrincipalID: v.principal.ID, PrincipalKind: v.principal.Kind, Kind: sessionDelegated,
		Channel: ChannelAPI, CredentialID: v.sess.CredentialID, ParentID: v.sess.ID, DelegatedBy: d.ID, DelegateAuthEpoch: d.AuthEpoch,
		Purpose: req.Purpose, Scopes: scopes, Projects: projects, Actions: acts, AuthEpoch: v.principal.AuthEpoch,
		RecoveryEpoch: v.sess.RecoveryEpoch, ExpiresAt: exp}, nil)
}

// current 核对调用者上下文对应的会话仍然有效，且上下文中的主体与会话一致。
func (s *Service) current(ctx context.Context, who authz.Context) (verified, error) {
	v, err := s.verify(ctx, who.SessionID)
	if err != nil {
		return v, err
	}
	if v.problem != "" {
		return v, sessionErr(v.problem)
	}
	if v.principal.ID != who.PrincipalID {
		return v, sessionErr(errcode.TokenRevoked)
	}
	return v, nil
}

// EndSession 结束会话：本人可以结束自己的会话及自己派生的子会话与委托；
// 结束他人的会话走需要人类授权的 RevokeSession 命令。对无权的目标返回
// NOT_FOUND。结束会话持 security_guard 写锁，返回后该会话的新请求一律拒绝。
func (s *Service) EndSession(ctx context.Context, who authz.Context, target ids.ID) error {
	ctx, release, err := s.write(ctx, true)
	if err != nil {
		return err
	}
	defer release()
	v, err := s.current(ctx, who)
	if err != nil {
		return err
	}
	if err := s.require(ctx, s.main, v, ActEndSession, authz.Resource{}); err != nil {
		return err
	}
	t, err := loadSession(ctx, s.runtime, target)
	if errors.Is(err, errNotFound) || (err == nil && t.ID != v.sess.ID && t.ParentID != v.sess.ID) {
		return errcode.New(errcode.NotFound, "session not found")
	}
	if err != nil {
		return err
	}
	return s.endSession(ctx, t, "ended_by_owner", v.principal.ID, v.sess.ID)
}

func (s *Service) endSession(ctx context.Context, t sessionRow, reason string, actor, actorSession ids.ID) error {
	if !t.EndedAt.IsZero() {
		return nil
	}
	op, err := s.newID()
	if err != nil {
		return err
	}
	ev, err := s.event(evParams{typ: EvSessionEnded, aggType: "session", aggID: t.ID, revision: 2, actor: actor,
		session: actorSession, operation: op, payload: map[string]any{"session_id": t.ID, "principal_id": t.PrincipalID, "reason": reason}})
	if err != nil {
		return err
	}
	return inTx(ctx, s.runtime, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE identity_sessions SET ended_at = ?, end_reason = ? WHERE session_id = ? AND ended_at IS NULL`,
			clock.Millis(s.now()), reason, t.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return s.rtStore.AppendEvents(ctx, tx, op, []event.Envelope{ev})
	})
}

func mustSpec(a authz.Action) ActionSpec {
	s, ok := Spec(a)
	if !ok {
		panic(fmt.Sprintf("identity: action %s is not registered", a))
	}
	return s
}

// Identity 是 whoami 的结果。
type Identity struct {
	Principal    Principal         `json:"principal"`
	Session      SessionInfo       `json:"session"`
	SystemRoles  []SystemRole      `json:"system_roles,omitempty"`
	ProjectRoles map[ids.ID][]Role `json:"project_roles,omitempty"`
	FactorState  string            `json:"factor_state,omitempty"`
	Context      authz.Context     `json:"-"`
}

// WhoAmI 返回调用者的身份、会话与角色；恢复会话也可以调用。
func (s *Service) WhoAmI(ctx context.Context, who authz.Context) (Identity, error) {
	v, err := s.current(ctx, who)
	if err != nil {
		return Identity{}, err
	}
	if err := s.require(ctx, s.main, v, ActWhoAmI, authz.Resource{}); err != nil {
		return Identity{}, err
	}
	out := Identity{Principal: v.principal, Session: v.sess.info(), Context: s.contextOf(v), ProjectRoles: map[ids.ID][]Role{}}
	if out.SystemRoles, err = systemRolesOf(ctx, s.main, v.principal.ID); err != nil {
		return out, err
	}
	rows, err := s.main.QueryContext(ctx, `SELECT project_id, role FROM identity_project_roles WHERE principal_id = ? ORDER BY project_id, role`, v.principal.ID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var p ids.ID
		var r Role
		if err := rows.Scan(&p, &r); err != nil {
			return out, err
		}
		out.ProjectRoles[p] = append(out.ProjectRoles[p], r)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if v.principal.Kind == authz.Human {
		if acct, err := loadAccount(ctx, s.main, v.principal.ID); err == nil {
			out.FactorState = acct.FactorState
		}
	}
	return out, nil
}
