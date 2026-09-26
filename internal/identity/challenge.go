package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
)

// Target 是敏感命令作用的对象；人类授权按目标集合的摘要与请求摘要绑定。
type Target struct {
	Kind             string `json:"kind"`
	ID               string `json:"id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

// Command 是需要人类授权的敏感命令（凭据与策略管理）。集合封闭在本包内：
// 每个命令自行给出动作、目标、规范化请求体、服务端生成的待批准摘要与执行。
type Command interface {
	action() authz.Action
	project() ids.ID
	targets() []Target
	body() any
	check() error
	describe(ctx context.Context, q commands.DBTX) (string, error)
	apply(ctx context.Context, e *applyEnv) (any, error)
}

// binding 是命令与挑战、人类授权之间的绑定值。
type binding struct {
	action       authz.Action
	project      ids.ID
	targets      []Target
	requestHash  digest.Digest
	targetDigest digest.Digest
}

func bind(c Command) (binding, error) {
	if err := c.check(); err != nil {
		return binding{}, fixRequest("%v", err)
	}
	b := binding{action: c.action(), project: c.project(), targets: append([]Target(nil), c.targets()...)}
	sort.Slice(b.targets, func(i, j int) bool {
		if b.targets[i].Kind != b.targets[j].Kind {
			return b.targets[i].Kind < b.targets[j].Kind
		}
		return b.targets[i].ID < b.targets[j].ID
	})
	tjson, err := canonjson.CanonicalizeValue(b.targets)
	if err != nil {
		return binding{}, err
	}
	b.targetDigest = digest.Of(tjson)
	body, err := canonjson.CanonicalizeValue(c.body())
	if err != nil {
		return binding{}, err
	}
	keys := make([]string, len(b.targets))
	exp := map[string]int64{}
	for i, t := range b.targets {
		keys[i] = t.Kind + ":" + t.ID
		exp[keys[i]] = t.ExpectedRevision
	}
	b.requestHash, err = commands.RequestHash(commands.HashInput{CommandType: string(b.action), ProjectID: b.project,
		Targets: keys, ExpectedRevisions: exp, Body: body})
	return b, err
}

// Challenge 是一次待人验证的挑战：服务端固定动作、目标集合、请求摘要与
// operation_id，界面必须完整展示 Summary 与 Targets 后再请人输入动态码。
type Challenge struct {
	ChallengeID     ids.ID        `json:"challenge_id"`
	OperationID     ids.ID        `json:"operation_id"`
	Action          authz.Action  `json:"action"`
	ProjectID       ids.ID        `json:"project_id,omitempty"`
	Targets         []Target      `json:"targets"`
	Summary         string        `json:"summary"`
	RequestHash     digest.Digest `json:"request_hash"`
	TargetSetDigest digest.Digest `json:"target_set_digest"`
	ExpiresAt       time.Time     `json:"expires_at"`
}

// Grant 是一次性人类授权：只对一个动作、精确的目标集合与请求摘要、一个
// operation_id 与发起挑战的会话有效。
type Grant struct {
	GrantID     ids.ID       `json:"grant_id"`
	ChallengeID ids.ID       `json:"challenge_id"`
	OperationID ids.ID       `json:"operation_id"`
	Action      authz.Action `json:"action"`
	ExpiresAt   time.Time    `json:"expires_at"`
}

// humanSession 确认调用者是人的普通、非委托会话。
func (s *Service) humanSession(ctx context.Context, who authz.Context) (verified, error) {
	v, err := s.current(ctx, who)
	if err != nil {
		return v, err
	}
	if !interactiveHuman(v) {
		return v, errcode.New(errcode.Forbidden, "only a person's own interactive session can request human authorization")
	}
	return v, nil
}

// CreateChallenge 为敏感命令创建挑战（默认 5 分钟有效）。调用者必须有执行该
// 命令的资格；服务端生成 operation_id 与待批准摘要。
func (s *Service) CreateChallenge(ctx context.Context, who authz.Context, cmd Command) (Challenge, error) {
	return s.challenge(ctx, who, cmd, "")
}

// Rechallenge 为同一个尚未提交的 operation 重新申请挑战（例如人类授权已过期）：
// 动作、目标集合与请求摘要必须与该操作原来的挑战完全相同，换目标必须新建
// 操作。已提交的操作不再挑战，按原幂等键重放即可取回原结果。
func (s *Service) Rechallenge(ctx context.Context, who authz.Context, operationID ids.ID, cmd Command) (Challenge, error) {
	if !operationID.Valid() {
		return Challenge{}, fixRequest("operation_id is required")
	}
	return s.challenge(ctx, who, cmd, operationID)
}

func (s *Service) challenge(ctx context.Context, who authz.Context, cmd Command, reuse ids.ID) (Challenge, error) {
	b, err := bind(cmd)
	if err != nil {
		return Challenge{}, err
	}
	spec := mustSpec(b.action)
	if !spec.HumanGrant {
		return Challenge{}, errcode.Newf(errcode.Forbidden, "%s does not use human authorization", b.action)
	}
	ctx, release, err := s.write(ctx, false)
	if err != nil {
		return Challenge{}, err
	}
	defer release()
	v, err := s.humanSession(ctx, who)
	if err != nil {
		return Challenge{}, err
	}
	if err := s.require(ctx, s.main, v, b.action, authz.Resource{ProjectID: b.project}); err != nil {
		return Challenge{}, err
	}
	summary, err := cmd.describe(ctx, s.main)
	if err != nil {
		return Challenge{}, err
	}
	op := reuse
	if reuse != "" {
		var action authz.Action
		var project ids.ID
		var tdig, rhash digest.Digest
		err := s.main.QueryRowContext(ctx, `SELECT action, project_id, target_set_digest, request_hash FROM identity_challenges
			WHERE operation_id = ? AND human_id = ? ORDER BY created_at LIMIT 1`, reuse, v.principal.ID).Scan(&action, &project, &tdig, &rhash)
		if errors.Is(err, sql.ErrNoRows) {
			return Challenge{}, errcode.New(errcode.NotFound, "operation not found")
		}
		if err != nil {
			return Challenge{}, err
		}
		if action != b.action || project != b.project || tdig != b.targetDigest || rhash != b.requestHash {
			return Challenge{}, errcode.New(errcode.HumanGrantMismatch, "a new challenge for this operation must repeat the same action, targets and request")
		}
		if _, err := s.mainStore.ReceiptByOperation(ctx, s.main, reuse); err == nil {
			return Challenge{}, errcode.New(errcode.InvalidStateTransition, "the operation has already been committed; replay it with its idempotency key").
				WithOperation(string(reuse))
		} else if !errors.Is(err, commands.ErrNotFound) {
			return Challenge{}, err
		}
	} else if op, err = s.newID(); err != nil {
		return Challenge{}, err
	}
	id, err := s.newID()
	if err != nil {
		return Challenge{}, err
	}
	now := s.now()
	ch := Challenge{ChallengeID: id, OperationID: op, Action: b.action, ProjectID: b.project, Targets: b.targets,
		Summary: summary, RequestHash: b.requestHash, TargetSetDigest: b.targetDigest, ExpiresAt: clock.Truncate(now.Add(s.cfg.ChallengeTTL))}
	err = inTx(ctx, s.main, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO identity_challenges (challenge_id, human_id, session_id, action, project_id,
			targets, target_set_digest, request_hash, operation_id, summary, created_at, expires_at, state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')`, id, v.principal.ID, v.sess.ID, b.action, b.project,
			jsonText(b.targets), b.targetDigest, b.requestHash, op, summary, clock.Millis(now), clock.Millis(ch.ExpiresAt))
		return err
	})
	return ch, err
}

type challengeRow struct {
	ID, HumanID, SessionID, OperationID, GrantID ids.ID
	Action                                       authz.Action
	Project                                      ids.ID
	TargetDigest, RequestHash                    digest.Digest
	ExpiresAt                                    time.Time
	Attempts                                     int
	State                                        string
}

func loadChallenge(ctx context.Context, q commands.DBTX, id ids.ID) (challengeRow, error) {
	var c challengeRow
	var exp int64
	var grant sql.NullString
	err := q.QueryRowContext(ctx, `SELECT challenge_id, human_id, session_id, operation_id, grant_id, action, project_id,
		target_set_digest, request_hash, expires_at, attempts, state FROM identity_challenges WHERE challenge_id = ?`, id).
		Scan(&c.ID, &c.HumanID, &c.SessionID, &c.OperationID, &grant, &c.Action, &c.Project, &c.TargetDigest, &c.RequestHash,
			&exp, &c.Attempts, &c.State)
	if errors.Is(err, sql.ErrNoRows) {
		return c, errNotFound
	}
	c.GrantID, c.ExpiresAt = ids.ID(grant.String), clock.FromMillis(exp)
	return c, err
}

func proofErr(reason, msg string) error {
	return errcode.New(errcode.HumanProofRequired, msg).WithDetails(errcode.Detail{Reason: reason})
}

// VerifyChallenge 用人在自己终端或浏览器里输入的动态码兑换挑战，得到绑定的
// 人类授权。校验、推进因子的 last_accepted_counter 与写入授权在主库同一事务
// 完成：同一时间步不能换第二个挑战；挑战只能由发起它的会话兑换；每个挑战最多
// 失败 5 次，主体与来源另有滚动窗口限速，新建挑战不会重置限速。
func (s *Service) VerifyChallenge(ctx context.Context, who authz.Context, challengeID ids.ID, code, source string) (Grant, error) {
	ctx, release, err := s.write(ctx, false)
	if err != nil {
		return Grant{}, err
	}
	defer release()
	v, err := s.humanSession(ctx, who)
	if err != nil {
		return Grant{}, err
	}
	source = normalizeSource(source)
	var g Grant
	err = inTxKeep(ctx, s.main, func(tx *sql.Tx) error {
		c, err := loadChallenge(ctx, tx, challengeID)
		if errors.Is(err, errNotFound) || (err == nil && c.HumanID != v.principal.ID) {
			return errcode.New(errcode.NotFound, "challenge not found")
		}
		if err != nil {
			return err
		}
		if c.SessionID != v.sess.ID {
			return errcode.New(errcode.HumanGrantMismatch, "a challenge can only be redeemed by the session that requested it")
		}
		now := s.now()
		switch c.State {
		case "verified":
			return errcode.New(errcode.HumanGrantMismatch, "this challenge has already been redeemed").WithOperation(string(c.OperationID))
		case "locked":
			return proofErr("challenge_locked", "too many failed attempts; request a new challenge")
		case "expired":
			return proofErr("challenge_expired", "the challenge has expired; request a new one")
		}
		if !now.Before(c.ExpiresAt) {
			if _, err := tx.ExecContext(ctx, `UPDATE identity_challenges SET state = 'expired' WHERE challenge_id = ?`, c.ID); err != nil {
				return err
			}
			return errKeep{proofErr("challenge_expired", "the challenge has expired; request a new one")}
		}
		if err := s.checkRate(ctx, tx, v.principal.ID, source, bucketProof); err != nil {
			return err
		}
		res, err := s.checkFactorTx(ctx, tx, v.principal.ID, code)
		if err != nil {
			return err
		}
		if res != totp.Accepted {
			state := "pending"
			if c.Attempts+1 >= s.cfg.ChallengeMaxAttempts {
				state = "locked"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE identity_challenges SET attempts = attempts + 1, state = ? WHERE challenge_id = ?`, state, c.ID); err != nil {
				return err
			}
			if err := s.recordFailure(ctx, tx, v.principal.ID, source, failTOTP); err != nil {
				return err
			}
			if res == totp.Replayed {
				return errKeep{errcode.New(errcode.TOTPReplay, "this code has already been used; wait for the next code")}
			}
			return errKeep{proofErr("code_invalid", "the code is not valid")}
		}
		epoch, err := s.recoveryEpoch(ctx)
		if err != nil {
			return err
		}
		gid, err := s.newID()
		if err != nil {
			return err
		}
		exp := clock.Truncate(now.Add(s.cfg.GrantTTL))
		if _, err := tx.ExecContext(ctx, `INSERT INTO identity_human_grants (grant_id, challenge_id, human_id, session_id, action,
			project_id, target_set_digest, request_hash, operation_id, issued_at, expires_at, auth_epoch, recovery_epoch, state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'bound')`, gid, c.ID, v.principal.ID, v.sess.ID, c.Action, c.Project,
			c.TargetDigest, c.RequestHash, c.OperationID, clock.Millis(now), clock.Millis(exp), v.principal.AuthEpoch, epoch); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE identity_challenges SET state = 'verified', grant_id = ? WHERE challenge_id = ?`, gid, c.ID); err != nil {
			return err
		}
		ev, err := s.event(evParams{typ: EvHumanGrantIssued, aggType: "human_grant", aggID: gid, revision: 1, actor: v.principal.ID,
			session: v.sess.ID, project: c.Project, operation: c.OperationID,
			payload: map[string]any{"grant_id": gid, "challenge_id": c.ID, "action": c.Action, "target_set_digest": c.TargetDigest,
				"request_hash": c.RequestHash, "expires_at": clock.Format(exp)}})
		if err != nil {
			return err
		}
		if err := s.mainStore.AppendEvents(ctx, tx, c.OperationID, []event.Envelope{ev}); err != nil {
			return err
		}
		g = Grant{GrantID: gid, ChallengeID: c.ID, OperationID: c.OperationID, Action: c.Action, ExpiresAt: exp}
		return nil
	})
	return g, err
}

type grantRow struct {
	ID, ChallengeID, HumanID, SessionID, OperationID ids.ID
	Action                                           authz.Action
	Project                                          ids.ID
	TargetDigest, RequestHash                        digest.Digest
	ExpiresAt                                        time.Time
	AuthEpoch, RecoveryEpoch                         int64
	State                                            string
}

func loadGrant(ctx context.Context, q commands.DBTX, id ids.ID) (grantRow, error) {
	var g grantRow
	var exp int64
	err := q.QueryRowContext(ctx, `SELECT grant_id, challenge_id, human_id, session_id, operation_id, action, project_id,
		target_set_digest, request_hash, expires_at, auth_epoch, recovery_epoch, state FROM identity_human_grants WHERE grant_id = ?`, id).
		Scan(&g.ID, &g.ChallengeID, &g.HumanID, &g.SessionID, &g.OperationID, &g.Action, &g.Project, &g.TargetDigest,
			&g.RequestHash, &exp, &g.AuthEpoch, &g.RecoveryEpoch, &g.State)
	if errors.Is(err, sql.ErrNoRows) {
		return g, errNotFound
	}
	g.ExpiresAt = clock.FromMillis(exp)
	return g, err
}

// checkGrant 核对人类授权与本次执行完全一致：同一人、同一会话、同一动作、
// 相同目标集合摘要与请求摘要、未吊销未过期、auth_epoch 与 recovery_epoch 仍是
// 当前值。换会话、换目标、换摘要都不能复用授权。
func (s *Service) checkGrant(ctx context.Context, g grantRow, v verified, b binding) error {
	mismatch := func(reason string) error {
		return errcode.New(errcode.HumanGrantMismatch, "the human authorization does not match this request").
			WithDetails(errcode.Detail{Reason: reason})
	}
	switch {
	case g.HumanID != v.principal.ID:
		return mismatch("human")
	case g.SessionID != v.sess.ID:
		return mismatch("session")
	case g.Action != b.action:
		return mismatch("action")
	case g.Project != b.project:
		return mismatch("project")
	case g.TargetDigest != b.targetDigest:
		return mismatch("target_set")
	case g.RequestHash != b.requestHash:
		return mismatch("request_hash")
	case g.State != "bound":
		return proofErr("grant_revoked", "the human authorization was revoked; request a new challenge")
	case !s.now().Before(g.ExpiresAt):
		return proofErr("grant_expired", "the human authorization has expired; request a new challenge for the same operation")
	case g.AuthEpoch != v.principal.AuthEpoch:
		return proofErr("grant_revoked", "credentials changed after the authorization was granted")
	}
	epoch, err := s.recoveryEpoch(ctx)
	if err != nil {
		return err
	}
	if g.RecoveryEpoch != epoch {
		return proofErr("grant_revoked", "the instance was restored after the authorization was granted")
	}
	return nil
}

// Result 是敏感命令的结果。
type Result struct {
	OperationID ids.ID `json:"operation_id"`
	// Replayed 表示同一幂等键的操作已提交过，这是保存的原结果，没有第二次效果。
	Replayed bool            `json:"replayed"`
	Summary  json.RawMessage `json:"result"`
	// Secret 是只在首次执行时返回一次的明文（新长期凭据或设置码）；回执里不保存，
	// 响应丢失后无法取回，只能吊销后重新签发。
	Secret string `json:"-"`
}

type applyEnv struct {
	s      *Service
	tx     *sql.Tx
	actor  verified
	op     ids.ID
	now    time.Time
	ms     int64
	events []event.Envelope
	secret string
}

func (e *applyEnv) emit(p evParams) error {
	p.actor, p.session, p.operation = e.actor.principal.ID, e.actor.sess.ID, e.op
	ev, err := e.s.event(p)
	if err != nil {
		return err
	}
	e.events = append(e.events, ev)
	return nil
}

// Execute 执行敏感命令：持 security_guard 写锁，在主库一个事务中核对人类授权、
// 当前权限与预期修订，写入效果、命令回执与 outbox。同一 operation 已提交时只
// 按当前权限返回原回执，不再核对已过期的授权、不重复写效果；授权不能被另一个
// 幂等键或另一个请求体复用。
func (s *Service) Execute(ctx context.Context, who authz.Context, grantID ids.ID, idempotencyKey string, cmd Command) (Result, error) {
	b, err := bind(cmd)
	if err != nil {
		return Result{}, err
	}
	spec := mustSpec(b.action)
	if !spec.HumanGrant {
		return Result{}, errcode.Newf(errcode.Forbidden, "%s is not a sensitive command", b.action)
	}
	ctx, release, err := s.write(ctx, true)
	if err != nil {
		return Result{}, err
	}
	defer release()
	v, err := s.humanSession(ctx, who)
	if err != nil {
		return Result{}, err
	}
	g, err := loadGrant(ctx, s.main, grantID)
	if errors.Is(err, errNotFound) || (err == nil && g.HumanID != v.principal.ID) {
		return Result{}, proofErr("grant_missing", "this command needs a human authorization bound to it")
	}
	if err != nil {
		return Result{}, err
	}
	epoch, err := s.recoveryEpoch(ctx)
	if err != nil {
		return Result{}, err
	}
	cctx := commands.Context{OperationID: g.OperationID, IdempotencyKey: idempotencyKey, RequestHash: b.requestHash,
		CommandType: string(b.action), ActorID: v.principal.ID, SessionID: v.sess.ID, ProjectID: b.project,
		PolicyRevision: v.policyRev, RecoveryEpoch: epoch, HumanGrantID: g.ID}
	if err := cctx.Validate(); err != nil {
		return Result{}, fixRequest("%v", err)
	}
	if prior, err := s.mainStore.ReceiptByOperation(ctx, s.main, g.OperationID); err == nil && prior.Key != cctx.Key() {
		return Result{}, errcode.New(errcode.IdempotencyConflict, "this authorization was already used with another idempotency key").
			WithOperation(string(g.OperationID))
	} else if err != nil && !errors.Is(err, commands.ErrNotFound) {
		return Result{}, err
	}
	var secret string
	resp, err := s.mainStore.Execute(ctx, s.main, cctx, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		g, err := loadGrant(ctx, tx, grantID)
		if err != nil {
			return commands.Result{}, err
		}
		if err := s.checkGrant(ctx, g, v, b); err != nil {
			return commands.Result{}, err
		}
		if err := s.require(ctx, tx, v, b.action, authz.Resource{ProjectID: b.project}); err != nil {
			return commands.Result{}, err
		}
		now := s.now()
		env := &applyEnv{s: s, tx: tx, actor: v, op: g.OperationID, now: now, ms: clock.Millis(now)}
		summary, err := cmd.apply(ctx, env)
		if err != nil {
			return commands.Result{}, err
		}
		secret = env.secret
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: summary, Events: env.events}, nil
	})
	if err != nil {
		var e *errcode.Error
		if errors.As(err, &e) {
			return Result{OperationID: g.OperationID}, err
		}
		return Result{}, err
	}
	switch resp.Outcome {
	case commands.OutcomeExecute:
		return Result{OperationID: g.OperationID, Summary: resp.Receipt.ResponseSummary, Secret: secret}, nil
	case commands.OutcomeReplay:
		// 重放也按当前权限复核：已被撤权的人拿不到原结果。
		if err := s.require(ctx, s.main, v, b.action, authz.Resource{ProjectID: b.project}); err != nil {
			return Result{}, err
		}
		return Result{OperationID: resp.Receipt.OperationID, Replayed: true, Summary: resp.Receipt.ResponseSummary}, nil
	default:
		return Result{}, fmt.Errorf("identity: unexpected idempotency outcome %s", resp.Outcome)
	}
}

// RevokeGrant 由授权人本人撤回一个尚未使用的人类授权（例如发现摘要不对）。
func (s *Service) RevokeGrant(ctx context.Context, who authz.Context, grantID ids.ID) error {
	ctx, release, err := s.write(ctx, true)
	if err != nil {
		return err
	}
	defer release()
	v, err := s.humanSession(ctx, who)
	if err != nil {
		return err
	}
	return inTx(ctx, s.main, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE identity_human_grants SET state = 'revoked', revoked_at = ?, revoke_reason = 'withdrawn'
			WHERE grant_id = ? AND human_id = ? AND state = 'bound'`, clock.Millis(s.now()), grantID, v.principal.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errcode.New(errcode.NotFound, "grant not found")
		}
		return nil
	})
}
