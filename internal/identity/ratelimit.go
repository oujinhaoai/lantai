package identity

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// 失败类别。
const (
	failLogin    = "login"
	failTOTP     = "totp"
	failRecovery = "recovery"
	failSetup    = "setup"
)

// failureRetention 之后的失败记录会被清理；它必须大于限速窗口。
const failureRetention = 24 * time.Hour

var sourceRE = regexp.MustCompile(`^[0-9A-Za-z.:\[\]%_-]{0,80}$`)

// normalizeSource 只保留形如地址的来源标识，其余一律记为空，避免把任意输入写库。
func normalizeSource(src string) string {
	if !sourceRE.MatchString(src) {
		return ""
	}
	return src
}

// 失败分两组计数。未认证入口（登录、恢复码、设置码）的失败谁都能制造；
// 已认证会话内兑换人类授权与确认新验证器的失败只能由持有该会话的人制造。
// 两组分开，匿名者猜口令只会让登录与恢复被限速，不能借此锁住管理员用自己
// 会话兑换的挑战（包括紧急撤权）。
var (
	bucketSignIn = []string{failLogin, failRecovery, failSetup}
	bucketProof  = []string{failTOTP}
)

// checkRate 在滚动窗口内按主体与来源分别统计某一组失败；任一达到上限返回
// RATE_LIMITED。新建挑战不会重置主体计数。
func (s *Service) checkRate(ctx context.Context, q commands.DBTX, principal ids.ID, source string, bucket []string) error {
	now := s.now()
	since := clock.Millis(now.Add(-s.cfg.FailureWindow))
	kinds := "'" + strings.Join(bucket, "', '") + "'"
	check := func(col, val string) error {
		if val == "" {
			return nil
		}
		var n int
		var oldest sql.NullInt64
		if err := q.QueryRowContext(ctx, `SELECT count(*), min(at) FROM identity_auth_failures WHERE `+col+` = ? AND at > ?
			AND kind IN (`+kinds+`)`, val, since).Scan(&n, &oldest); err != nil {
			return err
		}
		if n < s.cfg.FailureLimit {
			return nil
		}
		wait := clock.FromMillis(oldest.Int64).Add(s.cfg.FailureWindow).Sub(now)
		if wait < time.Second {
			wait = time.Second
		}
		return errcode.New(errcode.RateLimited, "too many failed attempts; wait before trying again").WithRetryAfter(wait)
	}
	if err := check("principal_id", string(principal)); err != nil {
		return err
	}
	return check("source", source)
}

// recordFailure 记下一次失败（主体未知时为空），并顺带清理过期记录。
func (s *Service) recordFailure(ctx context.Context, tx *sql.Tx, principal ids.ID, source, kind string) error {
	now := s.now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO identity_auth_failures (principal_id, source, kind, at) VALUES (?, ?, ?, ?)`,
		principal, source, kind, clock.Millis(now)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM identity_auth_failures WHERE at < ?`, clock.Millis(now.Add(-failureRetention)))
	return err
}

// reserveAttempt 在一个短的主库写事务中核对未认证入口的限速，并预先记下
// 一次失败，返回这条记录的序号；认证成功时调用方在其提交事务中用
// dropAttempt 撤销它。口令校验很慢，若先查限速、校验后再记失败，并发的
// 尝试都能通过检查而超出上限；预先记下并与检查在同一个串行化事务中完成，
// 并发尝试也逐次计数。它自己经实例写入口取锁，调用时不能已持有写入口。
func (s *Service) reserveAttempt(ctx context.Context, principal ids.ID, source, kind string) (int64, error) {
	lctx, release, err := s.write(ctx, false)
	if err != nil {
		return 0, err
	}
	defer release()
	var seq int64
	err = inTx(lctx, s.main, func(tx *sql.Tx) error {
		if err := s.checkRate(lctx, tx, principal, source, bucketSignIn); err != nil {
			return err
		}
		if err := s.recordFailure(lctx, tx, principal, source, kind); err != nil {
			return err
		}
		return tx.QueryRowContext(lctx, `SELECT last_insert_rowid()`).Scan(&seq)
	})
	return seq, err
}

// dropAttempt 在认证成功的事务中撤销 reserveAttempt 预记的失败。
func dropAttempt(ctx context.Context, tx *sql.Tx, seq int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM identity_auth_failures WHERE seq = ?`, seq)
	return err
}

func invalidCredentials() *errcode.Error {
	return errcode.New(errcode.AuthRequired, "the credentials are not valid").
		WithDetails(errcode.Detail{Reason: "invalid_credentials"})
}
