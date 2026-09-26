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
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// errNotFound 表示记录不存在；对外按调用场景转换为 NOT_FOUND 或认证失败。
var errNotFound = errors.New("identity: not found")

func nullTime(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return clock.FromMillis(v.Int64)
}

const principalCols = `principal_id, kind, name, display_name, state, auth_epoch, profile, revision, created_at, updated_at`

func scanPrincipal(row interface{ Scan(...any) error }) (Principal, error) {
	var p Principal
	var profile string
	var created, updated int64
	err := row.Scan(&p.ID, &p.Kind, &p.Name, &p.DisplayName, &p.State, &p.AuthEpoch, &profile, &p.Revision, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return p, errNotFound
	}
	if err != nil {
		return p, err
	}
	if err := jsonUnmarshal(profile, &p.Profile); err != nil {
		return p, fmt.Errorf("identity: principal %s profile: %w", p.ID, err)
	}
	p.CreatedAt, p.UpdatedAt = clock.FromMillis(created), clock.FromMillis(updated)
	return p, nil
}

func loadPrincipal(ctx context.Context, q commands.DBTX, id ids.ID) (Principal, error) {
	return scanPrincipal(q.QueryRowContext(ctx, `SELECT `+principalCols+` FROM identity_principals WHERE principal_id = ?`, id))
}

func loadPrincipalByName(ctx context.Context, q commands.DBTX, name string) (Principal, error) {
	return scanPrincipal(q.QueryRowContext(ctx, `SELECT `+principalCols+` FROM identity_principals WHERE name = ?`, name))
}

func hasSystemRole(ctx context.Context, q commands.DBTX, id ids.ID, role SystemRole) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM identity_system_roles WHERE principal_id = ? AND role = ?`, id, role).Scan(&n)
	return n > 0, err
}

func systemRolesOf(ctx context.Context, q commands.DBTX, id ids.ID) ([]SystemRole, error) {
	rows, err := q.QueryContext(ctx, `SELECT role FROM identity_system_roles WHERE principal_id = ? ORDER BY role`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SystemRole
	for rows.Next() {
		var r SystemRole
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func projectRolesOf(ctx context.Context, q commands.DBTX, project, principal ids.ID) ([]Role, error) {
	rows, err := q.QueryContext(ctx, `SELECT role FROM identity_project_roles WHERE project_id = ? AND principal_id = ? ORDER BY role`, project, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Role
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func policyRevision(ctx context.Context, q commands.DBTX) (int64, error) {
	var rev int64
	err := q.QueryRowContext(ctx, `SELECT policy_revision FROM identity_state WHERE id = 1`).Scan(&rev)
	return rev, err
}

// bumpPolicyRevision 推进实例级授权修订：任何改变授权结论的写入都调用它。
func bumpPolicyRevision(ctx context.Context, tx *sql.Tx) (int64, error) {
	var rev int64
	err := tx.QueryRowContext(ctx, `UPDATE identity_state SET policy_revision = policy_revision + 1 WHERE id = 1 RETURNING policy_revision`).Scan(&rev)
	return rev, err
}

// humanAccount 是人的认证因子状态。
type humanAccount struct {
	FactorState      string
	PendingOperation ids.ID
	Revision         int64
}

const (
	factorEnrolled     = "enrolled"
	factorSetupPending = "setup_pending"
	factorResetPending = "reset_pending"
)

func loadAccount(ctx context.Context, q commands.DBTX, id ids.ID) (humanAccount, error) {
	var a humanAccount
	var pending sql.NullString
	err := q.QueryRowContext(ctx, `SELECT factor_state, pending_operation_id, revision FROM identity_human_accounts WHERE principal_id = ?`, id).
		Scan(&a.FactorState, &pending, &a.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return a, errNotFound
	}
	a.PendingOperation = ids.ID(pending.String)
	return a, err
}

// sessionRow 是 runtime.db 中的会话记录。
type sessionRow struct {
	ID                ids.ID
	TokenHash         string
	PrincipalID       ids.ID
	PrincipalKind     authz.PrincipalKind
	Kind              string
	Channel           string
	CredentialID      ids.ID
	ParentID          ids.ID
	DelegatedBy       ids.ID
	DelegateAuthEpoch int64
	Purpose           string
	Model             string
	Scopes            []Scope
	Projects          []ids.ID
	Actions           []authz.Action
	OperationID       ids.ID
	AuthEpoch         int64
	RecoveryEpoch     int64
	CreatedAt         time.Time
	ExpiresAt         time.Time
	EndedAt           time.Time
	EndReason         string
}

const (
	sessionNormal    = "normal"
	sessionRecovery  = "recovery"
	sessionDelegated = "delegated"
)

// Channel 是会话的接入方式。
const (
	ChannelCLI     = "cli"
	ChannelBrowser = "browser"
	ChannelAPI     = "api"
)

const sessionCols = `session_id, token_hash, principal_id, principal_kind, session_kind, channel, credential_id,
	parent_session_id, delegated_by, delegate_auth_epoch, purpose, model, scopes, projects, actions, operation_id,
	auth_epoch, recovery_epoch, created_at, expires_at, ended_at, end_reason`

func scanSession(row interface{ Scan(...any) error }) (sessionRow, error) {
	var r sessionRow
	var cred, parent, deleg, op, endReason sql.NullString
	var delegEpoch, ended sql.NullInt64
	var scopes, projects, actionsJSON string
	var created, expires int64
	err := row.Scan(&r.ID, &r.TokenHash, &r.PrincipalID, &r.PrincipalKind, &r.Kind, &r.Channel, &cred, &parent, &deleg,
		&delegEpoch, &r.Purpose, &r.Model, &scopes, &projects, &actionsJSON, &op, &r.AuthEpoch, &r.RecoveryEpoch,
		&created, &expires, &ended, &endReason)
	if errors.Is(err, sql.ErrNoRows) {
		return r, errNotFound
	}
	if err != nil {
		return r, err
	}
	r.CredentialID, r.ParentID, r.DelegatedBy, r.OperationID = ids.ID(cred.String), ids.ID(parent.String), ids.ID(deleg.String), ids.ID(op.String)
	r.DelegateAuthEpoch = delegEpoch.Int64
	r.CreatedAt, r.ExpiresAt, r.EndedAt = clock.FromMillis(created), clock.FromMillis(expires), nullTime(ended)
	r.EndReason = endReason.String
	if r.Scopes, err = decodeList[Scope](scopes); err != nil {
		return r, err
	}
	if r.Projects, err = decodeList[ids.ID](projects); err != nil {
		return r, err
	}
	if r.Actions, err = decodeList[authz.Action](actionsJSON); err != nil {
		return r, err
	}
	return r, nil
}

func loadSession(ctx context.Context, q commands.DBTX, id ids.ID) (sessionRow, error) {
	return scanSession(q.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM identity_sessions WHERE session_id = ?`, id))
}

func loadSessionByToken(ctx context.Context, q commands.DBTX, hash string) (sessionRow, error) {
	return scanSession(q.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM identity_sessions WHERE token_hash = ?`, hash))
}

func nullable[T ~string](v T) any {
	if v == "" {
		return nil
	}
	return string(v)
}

func insertSession(ctx context.Context, tx *sql.Tx, r sessionRow) error {
	var delegEpoch any
	if r.DelegatedBy != "" {
		delegEpoch = r.DelegateAuthEpoch
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO identity_sessions (`+sessionCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL)`,
		r.ID, r.TokenHash, r.PrincipalID, r.PrincipalKind, r.Kind, r.Channel, nullable(r.CredentialID),
		nullable(r.ParentID), nullable(r.DelegatedBy), delegEpoch, r.Purpose, r.Model,
		jsonList(r.Scopes), jsonList(r.Projects), jsonList(r.Actions), nullable(r.OperationID),
		r.AuthEpoch, r.RecoveryEpoch, clock.Millis(r.CreatedAt), clock.Millis(r.ExpiresAt))
	return err
}

// jsonList 把切片编码为 JSON 数组；nil 编码为 []。
func jsonList[T any](v []T) string {
	if v == nil {
		return "[]"
	}
	return jsonText(v)
}

func jsonUnmarshal(s string, v any) error {
	return jsonDecodeStrict([]byte(s), v)
}
