// Package identity 实现 T01 身份与安全：主体与项目角色、一次性管理员初始化、
// 口令与 TOTP、恢复码与受限因子恢复、长期凭据换会话、会话收窄与委托、
// 实时授权与最终接受协调，以及凭据/策略管理的 Challenge → HumanGrant。
//
// 权威数据在 main.db（主体、角色、策略、凭据、因子、挑战与人类授权），会话
// 运行记录在 runtime.db；两库之间不联表、不共用事务。会话是否有效在每次验证
// 时对照当前主体状态、auth_epoch、凭据吊销与实例 recovery_epoch 判定，不信
// 客户端自报的身份类别，也不依赖缓存。撤权、角色与策略变更持 security_guard
// 写锁，最终业务接受持读锁（见 Accept），撤权成功返回后开始的接受一定看到
// 新状态。
//
// 本机制防止 Agent 凭普通会话或已登录的浏览器扩大敏感操作；它不宣称能对抗
// 已控制同机高权限账号或进程的攻击者。
package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/identity/password"
)

// Module 是本模块在所有权登记中的名称。
const Module = "identity"

// Config 是身份模块的参数；零值字段取 DefaultConfig 中的值。
type Config struct {
	// SessionTTL 是普通会话的默认与最长有效期。
	SessionTTL time.Duration
	// RecoverySessionTTL 是受限恢复/设置会话的有效期。
	RecoverySessionTTL time.Duration
	// DelegationMaxTTL 是委托给插件、adapter 等代为调用者的短期能力的最长有效期。
	DelegationMaxTTL time.Duration
	// CredentialTTL 是长期凭据的默认有效期，CredentialMaxTTL 是上限。
	CredentialTTL    time.Duration
	CredentialMaxTTL time.Duration
	// ChallengeTTL 与 GrantTTL 是挑战与人类授权的有效期。
	ChallengeTTL time.Duration
	GrantTTL     time.Duration
	// SetupCodeTTL 是管理员发放的一次性设置码有效期。
	SetupCodeTTL time.Duration
	// ChallengeMaxAttempts 是单个挑战的失败上限，超过即锁定。
	ChallengeMaxAttempts int
	// FailureWindow 内同一主体或同一来源失败达到 FailureLimit 次时限速。
	FailureWindow time.Duration
	FailureLimit  int
	// RecoveryCodes 是每批恢复码数量。
	RecoveryCodes int
	// Password 是新口令的 Argon2id 参数；HashConcurrency 限制同时计算数。
	Password        password.Params
	HashConcurrency int
}

// DefaultConfig 返回设计基线中的默认值。
func DefaultConfig() Config {
	return Config{
		SessionTTL:           12 * time.Hour,
		RecoverySessionTTL:   15 * time.Minute,
		DelegationMaxTTL:     time.Hour,
		CredentialTTL:        180 * 24 * time.Hour,
		CredentialMaxTTL:     366 * 24 * time.Hour,
		ChallengeTTL:         5 * time.Minute,
		GrantTTL:             5 * time.Minute,
		SetupCodeTTL:         24 * time.Hour,
		ChallengeMaxAttempts: 5,
		FailureWindow:        10 * time.Minute,
		FailureLimit:         10,
		RecoveryCodes:        10,
		Password:             password.Default,
		HashConcurrency:      2,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	pick := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	pick(&c.SessionTTL, d.SessionTTL)
	pick(&c.RecoverySessionTTL, d.RecoverySessionTTL)
	pick(&c.DelegationMaxTTL, d.DelegationMaxTTL)
	pick(&c.CredentialTTL, d.CredentialTTL)
	pick(&c.CredentialMaxTTL, d.CredentialMaxTTL)
	pick(&c.ChallengeTTL, d.ChallengeTTL)
	pick(&c.GrantTTL, d.GrantTTL)
	pick(&c.SetupCodeTTL, d.SetupCodeTTL)
	pick(&c.FailureWindow, d.FailureWindow)
	if c.ChallengeMaxAttempts <= 0 {
		c.ChallengeMaxAttempts = d.ChallengeMaxAttempts
	}
	if c.FailureLimit <= 0 {
		c.FailureLimit = d.FailureLimit
	}
	if c.RecoveryCodes <= 0 {
		c.RecoveryCodes = d.RecoveryCodes
	}
	if c.Password == (password.Params{}) {
		c.Password = d.Password
	}
	if c.HashConcurrency <= 0 {
		c.HashConcurrency = d.HashConcurrency
	}
	return c
}

// Deps 是身份模块依赖的实例资源，由组装方（operations 实例）提供。
type Deps struct {
	Main    *sql.DB
	Runtime *sql.DB
	// Gate 是实例写入口：全部写入经它进入，维护与停止期间被拒。
	Gate *commands.Gate
	// Epochs 提供实例当前的 recovery_epoch。
	Epochs authz.EpochSource
	Clock  clock.Clock
	IDs    *ids.Generator
	// Key 是实例主密钥，用于加密 TOTP 种子与派生 CSRF 密钥。
	Key *masterkey.Key
	// InstanceID 与 InstanceName 写入授权上下文与 TOTP 签发方。
	InstanceID   ids.ID
	InstanceName string
}

// Service 是身份模块的应用服务。
type Service struct {
	main, runtime *sql.DB
	gate          *commands.Gate
	epochs        authz.EpochSource
	clock         clock.Clock
	ids           *ids.Generator
	key           *masterkey.Key
	instance      ids.ID
	issuer        string
	cfg           Config
	hasher        *password.Hasher
	mainStore     *commands.Store
	rtStore       *commands.Store
	csrfKey       []byte
}

var (
	_ authz.Authorizer      = (*Service)(nil)
	_ authz.SessionVerifier = (*Service)(nil)
)

// New 创建服务；迁移须已应用到 main.db 与 runtime.db。
func New(d Deps, cfg Config) (*Service, error) {
	switch {
	case d.Main == nil || d.Runtime == nil:
		return nil, errors.New("identity: main and runtime databases are required")
	case d.Gate == nil || d.Epochs == nil || d.Key == nil:
		return nil, errors.New("identity: gate, epoch source and master key are required")
	case !d.InstanceID.Valid():
		return nil, errors.New("identity: instance id is required")
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	if d.IDs == nil {
		return nil, errors.New("identity: id generator is required")
	}
	cfg = cfg.withDefaults()
	h, err := password.NewHasher(cfg.Password, cfg.HashConcurrency)
	if err != nil {
		return nil, err
	}
	ms, err := commands.NewStore(Module, d.Clock)
	if err != nil {
		return nil, err
	}
	rs, err := commands.NewStore(Module, d.Clock)
	if err != nil {
		return nil, err
	}
	csrf, err := d.Key.Derive("csrf/v1")
	if err != nil {
		return nil, err
	}
	issuer := d.InstanceName
	if issuer == "" {
		issuer = "lantai"
	}
	return &Service{
		main: d.Main, runtime: d.Runtime, gate: d.Gate, epochs: d.Epochs, clock: d.Clock, ids: d.IDs,
		key: d.Key, instance: d.InstanceID, issuer: issuer, cfg: cfg, hasher: h,
		mainStore: ms, rtStore: rs, csrfKey: csrf,
	}, nil
}

// Config 返回生效的参数。
func (s *Service) Config() Config { return s.cfg }

func (s *Service) now() time.Time { return s.clock.Now() }

func (s *Service) newID() (ids.ID, error) { return s.ids.New() }

func (s *Service) recoveryEpoch(ctx context.Context) (int64, error) {
	e, err := s.epochs.RecoveryEpoch(ctx)
	if err != nil {
		return 0, err
	}
	if e < 1 {
		return 0, fmt.Errorf("identity: invalid recovery epoch %d", e)
	}
	return e, nil
}

// write 经实例写入口取锁；普通接受取得 security_guard 读锁，保持最终身份
// 复验与写入相对撤权原子。security 为 true 时取写锁（撤权、角色与策略变更、
// 因子重置等改变授权结论的写入）。网络与慢口令计算须在调用前完成。
func (s *Service) write(ctx context.Context, security bool) (context.Context, func(), error) {
	req := commands.Request{Security: commands.ModeShared}
	if security {
		req.Security = commands.ModeExclusive
	}
	lctx, h, err := s.gate.Acquire(ctx, req)
	if err != nil {
		return ctx, nil, err
	}
	return lctx, h.Release, nil
}

// inTx 在 db 上执行一个写事务（BEGIN IMMEDIATE）；fn 返回错误时回滚。
func inTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// errKeep 包装一个需要在提交事务后返回给调用方的错误：失败计数、挑战尝试
// 次数等必须持久化，即使本次请求被拒绝。
type errKeep struct{ err error }

func (e errKeep) Error() string { return e.err.Error() }

// inTxKeep 与 inTx 相同，但 fn 返回 errKeep 时先提交再返回其内部错误。
func inTxKeep(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ferr := fn(tx)
	var keep errKeep
	if ferr != nil && !errors.As(ferr, &keep) {
		return ferr
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if ferr != nil {
		return keep.err
	}
	return nil
}

func fixRequest(format string, args ...any) *errcode.Error {
	return errcode.Newf(errcode.SchemaInvalid, format, args...)
}
