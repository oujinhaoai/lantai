package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func preconditionFailed(format string, args ...any) error {
	return errcode.Newf(errcode.PreconditionFailed, format, args...)
}

func notFound(what string) error { return errcode.Newf(errcode.NotFound, "%s not found", what) }

// principalFor 读取目标主体并核对预期修订。
func principalFor(ctx context.Context, q commands.DBTX, id ids.ID, expected int64) (Principal, error) {
	p, err := loadPrincipal(ctx, q, id)
	if errors.Is(err, errNotFound) {
		return p, notFound("principal")
	}
	if err != nil {
		return p, err
	}
	if p.Revision != expected {
		return p, preconditionFailed("principal %s is at revision %d, the request expected %d", p.Name, p.Revision, expected)
	}
	return p, nil
}

func describePrincipal(ctx context.Context, q commands.DBTX, id ids.ID) string {
	p, err := loadPrincipal(ctx, q, id)
	if err != nil {
		return string(id)
	}
	return fmt.Sprintf("%s %s (%s)", p.Kind, p.Name, p.ID)
}

func bumpPrincipalRevision(ctx context.Context, tx *sql.Tx, id ids.ID, ms int64) (int64, error) {
	var rev int64
	err := tx.QueryRowContext(ctx, `UPDATE identity_principals SET revision = revision + 1, updated_at = ? WHERE principal_id = ? RETURNING revision`, ms, id).Scan(&rev)
	return rev, err
}

// RegisterPrincipal 登记新主体。主体类别由管理员登记，客户端不能自报；人
// 登记后处于待设置状态，结果附带一次性设置码（只返回这一次）。
type RegisterPrincipal struct {
	Kind        authz.PrincipalKind `json:"kind"`
	Name        string              `json:"name"`
	DisplayName string              `json:"display_name"`
	Profile     Profile             `json:"profile"`
}

func (c *RegisterPrincipal) action() authz.Action { return ActRegisterPrincipal }
func (c *RegisterPrincipal) project() ids.ID      { return "" }
func (c *RegisterPrincipal) targets() []Target {
	return []Target{{Kind: "principal_name", ID: c.Name}}
}
func (c *RegisterPrincipal) body() any { return c }
func (c *RegisterPrincipal) check() error {
	if !c.Kind.Valid() {
		return fmt.Errorf("kind %q is not a principal kind", c.Kind)
	}
	if !ValidName(c.Kind, c.Name) {
		return fmt.Errorf("name %q does not match the naming rule for %s", c.Name, c.Kind)
	}
	if !shortTextRE.MatchString(c.DisplayName) {
		return fmt.Errorf("display name must be short plain text")
	}
	p, err := c.Profile.normalized()
	if err != nil {
		return err
	}
	if c.Kind == authz.Human && (p.AgentType != "" || p.Node != "" || p.DefaultModel != "" || len(p.Capabilities) > 0 || p.MaxSessions != 0) {
		return fmt.Errorf("a human principal has no agent profile")
	}
	c.Profile = p
	return nil
}
func (c *RegisterPrincipal) describe(context.Context, commands.DBTX) (string, error) {
	return fmt.Sprintf("Register %s principal %q", c.Kind, c.Name), nil
}
func (c *RegisterPrincipal) apply(ctx context.Context, e *applyEnv) (any, error) {
	if _, err := loadPrincipalByName(ctx, e.tx, c.Name); err == nil {
		return nil, preconditionFailed("a principal named %q already exists", c.Name)
	} else if !errors.Is(err, errNotFound) {
		return nil, err
	}
	id, err := e.s.newID()
	if err != nil {
		return nil, err
	}
	if _, err := e.tx.ExecContext(ctx, `INSERT INTO identity_principals (principal_id, kind, name, display_name, state,
		auth_epoch, profile, revision, created_at, created_by, updated_at) VALUES (?, ?, ?, ?, 'active', 1, ?, 1, ?, ?, ?)`,
		id, c.Kind, c.Name, c.DisplayName, jsonText(c.Profile), e.ms, e.actor.principal.ID, e.ms); err != nil {
		return nil, err
	}
	out := map[string]any{"principal_id": id, "kind": c.Kind, "name": c.Name, "revision": 1}
	if c.Kind == authz.Human {
		setup, err := ids.DeriveChild(e.op, "setup")
		if err != nil {
			return nil, err
		}
		if err := setAccount(ctx, e.tx, id, factorSetupPending, setup, e.ms); err != nil {
			return nil, err
		}
		code, err := e.s.issueSetupCode(ctx, e.tx, id, setup, e.actor.principal.ID)
		if err != nil {
			return nil, err
		}
		e.secret = code
		out["setup_code_expires_at"] = clock.Format(e.now.Add(e.s.cfg.SetupCodeTTL))
	}
	return out, e.emit(evParams{typ: EvPrincipalRegistered, aggType: "principal", aggID: id, revision: 1,
		payload: map[string]any{"principal_id": id, "kind": c.Kind, "name": c.Name, "profile": c.Profile}})
}

// UpdatePrincipal 修改显示名与档案（能力标签等）；能力不授予任何权限。
type UpdatePrincipal struct {
	PrincipalID      ids.ID  `json:"principal_id"`
	ExpectedRevision int64   `json:"expected_revision"`
	DisplayName      string  `json:"display_name"`
	Profile          Profile `json:"profile"`
}

func (c *UpdatePrincipal) action() authz.Action { return ActUpdatePrincipal }
func (c *UpdatePrincipal) project() ids.ID      { return "" }
func (c *UpdatePrincipal) targets() []Target {
	return []Target{{Kind: "principal", ID: string(c.PrincipalID), ExpectedRevision: c.ExpectedRevision}}
}
func (c *UpdatePrincipal) body() any { return c }
func (c *UpdatePrincipal) check() error {
	if !c.PrincipalID.Valid() || c.ExpectedRevision < 1 {
		return fmt.Errorf("principal_id and expected_revision are required")
	}
	if !shortTextRE.MatchString(c.DisplayName) {
		return fmt.Errorf("display name must be short plain text")
	}
	p, err := c.Profile.normalized()
	c.Profile = p
	return err
}
func (c *UpdatePrincipal) describe(ctx context.Context, q commands.DBTX) (string, error) {
	return "Update the profile of " + describePrincipal(ctx, q, c.PrincipalID), nil
}
func (c *UpdatePrincipal) apply(ctx context.Context, e *applyEnv) (any, error) {
	p, err := principalFor(ctx, e.tx, c.PrincipalID, c.ExpectedRevision)
	if err != nil {
		return nil, err
	}
	if p.Kind == authz.Human && !reflectEmpty(c.Profile) {
		return nil, fixRequest("a human principal has no agent profile")
	}
	var rev int64
	if err := e.tx.QueryRowContext(ctx, `UPDATE identity_principals SET display_name = ?, profile = ?, revision = revision + 1,
		updated_at = ? WHERE principal_id = ? RETURNING revision`, c.DisplayName, jsonText(c.Profile), e.ms, p.ID).Scan(&rev); err != nil {
		return nil, err
	}
	return map[string]any{"principal_id": p.ID, "revision": rev}, e.emit(evParams{typ: EvPrincipalUpdated, aggType: "principal",
		aggID: p.ID, revision: rev, payload: map[string]any{"principal_id": p.ID, "display_name": c.DisplayName, "profile": c.Profile}})
}

func reflectEmpty(p Profile) bool {
	return p.AgentType == "" && p.Node == "" && p.DefaultModel == "" && len(p.Capabilities) == 0 && p.MaxSessions == 0
}

// activeAdmins 返回仍有效的管理员数，防止停用或降级最后一位管理员。
func activeAdmins(ctx context.Context, q commands.DBTX) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM identity_system_roles r JOIN identity_principals p ON p.principal_id = r.principal_id
		WHERE r.role = 'admin' AND p.state = 'active'`).Scan(&n)
	return n, err
}

// DisablePrincipal 停用主体：推进 auth_epoch，其全部会话、子会话与委托立即
// 失效；人的未使用人类授权一并吊销。主体不删除，历史记录保留可查。
type DisablePrincipal struct {
	PrincipalID      ids.ID `json:"principal_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

func (c *DisablePrincipal) action() authz.Action { return ActDisablePrincipal }
func (c *DisablePrincipal) project() ids.ID      { return "" }
func (c *DisablePrincipal) targets() []Target {
	return []Target{{Kind: "principal", ID: string(c.PrincipalID), ExpectedRevision: c.ExpectedRevision}}
}
func (c *DisablePrincipal) body() any { return c }
func (c *DisablePrincipal) check() error {
	if !c.PrincipalID.Valid() || c.ExpectedRevision < 1 {
		return fmt.Errorf("principal_id and expected_revision are required")
	}
	if c.Reason == "" || !shortTextRE.MatchString(c.Reason) {
		return fmt.Errorf("a short plain-text reason is required")
	}
	return nil
}
func (c *DisablePrincipal) describe(ctx context.Context, q commands.DBTX) (string, error) {
	return "Disable " + describePrincipal(ctx, q, c.PrincipalID) + ": " + c.Reason, nil
}
func (c *DisablePrincipal) apply(ctx context.Context, e *applyEnv) (any, error) {
	p, err := principalFor(ctx, e.tx, c.PrincipalID, c.ExpectedRevision)
	if err != nil {
		return nil, err
	}
	if p.ID == e.actor.principal.ID {
		return nil, errcode.New(errcode.Forbidden, "you cannot disable your own identity")
	}
	if p.State == StateDisabled {
		return nil, preconditionFailed("principal %s is already disabled", p.Name)
	}
	if admin, err := hasSystemRole(ctx, e.tx, p.ID, RoleAdmin); err != nil {
		return nil, err
	} else if admin {
		if n, err := activeAdmins(ctx, e.tx); err != nil {
			return nil, err
		} else if n <= 1 {
			return nil, preconditionFailed("%s is the last active administrator", p.Name)
		}
	}
	var rev, epoch int64
	if err := e.tx.QueryRowContext(ctx, `UPDATE identity_principals SET state = 'disabled', disabled_reason = ?,
		auth_epoch = auth_epoch + 1, revision = revision + 1, updated_at = ? WHERE principal_id = ? RETURNING revision, auth_epoch`,
		c.Reason, e.ms, p.ID).Scan(&rev, &epoch); err != nil {
		return nil, err
	}
	if _, err := e.s.revokeGrants(ctx, e.tx, p.ID, "principal_disabled"); err != nil {
		return nil, err
	}
	if _, err := bumpPolicyRevision(ctx, e.tx); err != nil {
		return nil, err
	}
	return map[string]any{"principal_id": p.ID, "revision": rev, "auth_epoch": epoch}, e.emit(evParams{typ: EvPrincipalDisabled,
		aggType: "principal", aggID: p.ID, revision: rev, payload: map[string]any{"principal_id": p.ID, "reason": c.Reason, "auth_epoch": epoch}})
}

// EnablePrincipal 重新启用主体；停用前的会话不会复活（auth_epoch 再次推进）。
type EnablePrincipal struct {
	PrincipalID      ids.ID `json:"principal_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

func (c *EnablePrincipal) action() authz.Action { return ActEnablePrincipal }
func (c *EnablePrincipal) project() ids.ID      { return "" }
func (c *EnablePrincipal) targets() []Target {
	return []Target{{Kind: "principal", ID: string(c.PrincipalID), ExpectedRevision: c.ExpectedRevision}}
}
func (c *EnablePrincipal) body() any { return c }
func (c *EnablePrincipal) check() error {
	if !c.PrincipalID.Valid() || c.ExpectedRevision < 1 {
		return fmt.Errorf("principal_id and expected_revision are required")
	}
	return nil
}
func (c *EnablePrincipal) describe(ctx context.Context, q commands.DBTX) (string, error) {
	return "Enable " + describePrincipal(ctx, q, c.PrincipalID), nil
}
func (c *EnablePrincipal) apply(ctx context.Context, e *applyEnv) (any, error) {
	p, err := principalFor(ctx, e.tx, c.PrincipalID, c.ExpectedRevision)
	if err != nil {
		return nil, err
	}
	if p.State == StateActive {
		return nil, preconditionFailed("principal %s is already active", p.Name)
	}
	var rev, epoch int64
	if err := e.tx.QueryRowContext(ctx, `UPDATE identity_principals SET state = 'active', disabled_reason = NULL,
		auth_epoch = auth_epoch + 1, revision = revision + 1, updated_at = ? WHERE principal_id = ? RETURNING revision, auth_epoch`,
		e.ms, p.ID).Scan(&rev, &epoch); err != nil {
		return nil, err
	}
	if _, err := bumpPolicyRevision(ctx, e.tx); err != nil {
		return nil, err
	}
	return map[string]any{"principal_id": p.ID, "revision": rev}, e.emit(evParams{typ: EvPrincipalEnabled, aggType: "principal",
		aggID: p.ID, revision: rev, payload: map[string]any{"principal_id": p.ID, "auth_epoch": epoch}})
}

// IssueCredential 为非人主体签发长期凭据。令牌只在结果中返回一次，库里只存
// SHA-256 与明文前缀；它只能用来换会话，不能直接调用业务接口。范围不能超出
// 该类主体的上限，节点、执行器、运行器的范围互不通用。
type IssueCredential struct {
	PrincipalID      ids.ID   `json:"principal_id"`
	ExpectedRevision int64    `json:"expected_revision"`
	Scopes           []Scope  `json:"scopes"`
	Projects         []ids.ID `json:"projects"`
	TTLSeconds       int64    `json:"ttl_seconds"`
	Label            string   `json:"label"`
}

func (c *IssueCredential) action() authz.Action { return ActIssueCredential }
func (c *IssueCredential) project() ids.ID      { return "" }
func (c *IssueCredential) targets() []Target {
	return []Target{{Kind: "principal", ID: string(c.PrincipalID), ExpectedRevision: c.ExpectedRevision}}
}
func (c *IssueCredential) body() any { return c }
func (c *IssueCredential) check() error {
	if !c.PrincipalID.Valid() || c.ExpectedRevision < 1 {
		return fmt.Errorf("principal_id and expected_revision are required")
	}
	if c.TTLSeconds < 0 || !shortTextRE.MatchString(c.Label) || len(c.Scopes) == 0 {
		return fmt.Errorf("scopes are required; ttl_seconds must not be negative; label must be short plain text")
	}
	projects, err := normalizeProjects(c.Projects)
	if err != nil {
		return err
	}
	if projects == nil {
		projects = []ids.ID{}
	}
	c.Projects = projects
	sc := slices.Clone(c.Scopes)
	slices.Sort(sc)
	c.Scopes = slices.Compact(sc)
	return nil
}
func (c *IssueCredential) describe(ctx context.Context, q commands.DBTX) (string, error) {
	return fmt.Sprintf("Issue a long-lived credential for %s with scopes %v and projects %v", describePrincipal(ctx, q, c.PrincipalID), c.Scopes, c.Projects), nil
}
func (c *IssueCredential) apply(ctx context.Context, e *applyEnv) (any, error) {
	p, err := principalFor(ctx, e.tx, c.PrincipalID, c.ExpectedRevision)
	if err != nil {
		return nil, err
	}
	if p.Kind == authz.Human {
		return nil, errcode.New(errcode.Forbidden, "people sign in with a password and authenticator; they do not get long-lived credentials")
	}
	if p.State != StateActive {
		return nil, preconditionFailed("principal %s is disabled", p.Name)
	}
	scopes, err := normalizeScopes(c.Scopes, kindScopes[p.Kind])
	if err != nil {
		return nil, errcode.Newf(errcode.Forbidden, "%v", err)
	}
	ttl := e.s.cfg.CredentialTTL
	if c.TTLSeconds > 0 {
		ttl = time.Duration(c.TTLSeconds) * time.Second
	}
	if ttl > e.s.cfg.CredentialMaxTTL {
		return nil, fixRequest("ttl exceeds the maximum of %s", e.s.cfg.CredentialMaxTTL)
	}
	token, hash, err := newToken(CredentialPrefix)
	if err != nil {
		return nil, err
	}
	id, err := e.s.newID()
	if err != nil {
		return nil, err
	}
	exp := clock.Truncate(e.now.Add(ttl))
	prefix := credentialDisplayPrefix(token)
	if _, err := e.tx.ExecContext(ctx, `INSERT INTO identity_credentials (credential_id, principal_id, prefix, secret_hash, scopes,
		projects, label, created_at, created_by, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, p.ID, prefix, hash,
		jsonList(scopes), jsonList(c.Projects), c.Label, e.ms, e.actor.principal.ID, clock.Millis(exp)); err != nil {
		return nil, err
	}
	rev, err := bumpPrincipalRevision(ctx, e.tx, p.ID, e.ms)
	if err != nil {
		return nil, err
	}
	e.secret = token
	info := map[string]any{"credential_id": id, "principal_id": p.ID, "prefix": prefix, "scopes": scopes,
		"projects": c.Projects, "expires_at": clock.Format(exp), "revision": rev}
	return info, e.emit(evParams{typ: EvCredentialIssued, aggType: "principal", aggID: p.ID, revision: rev, payload: info})
}

// RevokeCredential 吊销长期凭据；由它换出的会话在下一次核对时即失效。
type RevokeCredential struct {
	CredentialID ids.ID `json:"credential_id"`
	Reason       string `json:"reason"`
}

func (c *RevokeCredential) action() authz.Action { return ActRevokeCredential }
func (c *RevokeCredential) project() ids.ID      { return "" }
func (c *RevokeCredential) targets() []Target {
	return []Target{{Kind: "credential", ID: string(c.CredentialID)}}
}
func (c *RevokeCredential) body() any { return c }
func (c *RevokeCredential) check() error {
	if !c.CredentialID.Valid() || c.Reason == "" || !shortTextRE.MatchString(c.Reason) {
		return fmt.Errorf("credential_id and a short plain-text reason are required")
	}
	return nil
}
func (c *RevokeCredential) describe(ctx context.Context, q commands.DBTX) (string, error) {
	cr, err := scanCredential(q.QueryRowContext(ctx, `SELECT `+credentialCols+` FROM identity_credentials WHERE credential_id = ?`, c.CredentialID))
	if err != nil {
		return "", notFound("credential")
	}
	return fmt.Sprintf("Revoke credential %s… of %s: %s", cr.Prefix, describePrincipal(ctx, q, cr.PrincipalID), c.Reason), nil
}
func (c *RevokeCredential) apply(ctx context.Context, e *applyEnv) (any, error) {
	cr, err := scanCredential(e.tx.QueryRowContext(ctx, `SELECT `+credentialCols+` FROM identity_credentials WHERE credential_id = ?`, c.CredentialID))
	if errors.Is(err, errNotFound) {
		return nil, notFound("credential")
	}
	if err != nil {
		return nil, err
	}
	if !cr.RevokedAt.IsZero() {
		return nil, preconditionFailed("credential %s is already revoked", cr.Prefix)
	}
	if _, err := e.tx.ExecContext(ctx, `UPDATE identity_credentials SET revoked_at = ?, revoked_by = ?, revoke_reason = ?
		WHERE credential_id = ?`, e.ms, e.actor.principal.ID, c.Reason, cr.ID); err != nil {
		return nil, err
	}
	rev, err := bumpPrincipalRevision(ctx, e.tx, cr.PrincipalID, e.ms)
	if err != nil {
		return nil, err
	}
	if _, err := bumpPolicyRevision(ctx, e.tx); err != nil {
		return nil, err
	}
	return map[string]any{"credential_id": cr.ID, "principal_id": cr.PrincipalID}, e.emit(evParams{typ: EvCredentialRevoked,
		aggType: "principal", aggID: cr.PrincipalID, revision: rev,
		payload: map[string]any{"credential_id": cr.ID, "prefix": cr.Prefix, "reason": c.Reason}})
}

// SetSystemRole 授予或撤销系统角色（M1 只有 admin，只能给人）。
type SetSystemRole struct {
	PrincipalID      ids.ID     `json:"principal_id"`
	ExpectedRevision int64      `json:"expected_revision"`
	Role             SystemRole `json:"role"`
	Grant            bool       `json:"grant"`
}

func (c *SetSystemRole) action() authz.Action {
	if c.Grant {
		return ActGrantSystemRole
	}
	return ActRevokeSystemRole
}
func (c *SetSystemRole) project() ids.ID { return "" }
func (c *SetSystemRole) targets() []Target {
	return []Target{{Kind: "principal", ID: string(c.PrincipalID), ExpectedRevision: c.ExpectedRevision}}
}
func (c *SetSystemRole) body() any { return c }
func (c *SetSystemRole) check() error {
	if !c.PrincipalID.Valid() || c.ExpectedRevision < 1 || c.Role != RoleAdmin {
		return fmt.Errorf("principal_id, expected_revision and role admin are required")
	}
	return nil
}
func (c *SetSystemRole) describe(ctx context.Context, q commands.DBTX) (string, error) {
	verb := "Revoke"
	if c.Grant {
		verb = "Grant"
	}
	return fmt.Sprintf("%s system role %s for %s", verb, c.Role, describePrincipal(ctx, q, c.PrincipalID)), nil
}
func (c *SetSystemRole) apply(ctx context.Context, e *applyEnv) (any, error) {
	p, err := principalFor(ctx, e.tx, c.PrincipalID, c.ExpectedRevision)
	if err != nil {
		return nil, err
	}
	if p.Kind != authz.Human {
		return nil, errcode.New(errcode.Forbidden, "system roles are only for people")
	}
	has, err := hasSystemRole(ctx, e.tx, p.ID, c.Role)
	if err != nil {
		return nil, err
	}
	evType := EvSystemRoleGranted
	if c.Grant {
		if has {
			return nil, preconditionFailed("%s already has role %s", p.Name, c.Role)
		}
		if p.State != StateActive {
			return nil, preconditionFailed("principal %s is disabled", p.Name)
		}
		if _, err := e.tx.ExecContext(ctx, `INSERT INTO identity_system_roles (principal_id, role, granted_at, granted_by) VALUES (?, ?, ?, ?)`,
			p.ID, c.Role, e.ms, e.actor.principal.ID); err != nil {
			return nil, err
		}
	} else {
		evType = EvSystemRoleRevoked
		if !has {
			return nil, preconditionFailed("%s does not have role %s", p.Name, c.Role)
		}
		if n, err := activeAdmins(ctx, e.tx); err != nil {
			return nil, err
		} else if n <= 1 && p.State == StateActive {
			return nil, preconditionFailed("%s is the last active administrator", p.Name)
		}
		if _, err := e.tx.ExecContext(ctx, `DELETE FROM identity_system_roles WHERE principal_id = ? AND role = ?`, p.ID, c.Role); err != nil {
			return nil, err
		}
		if _, err := e.s.revokeGrants(ctx, e.tx, p.ID, "role_revoked"); err != nil {
			return nil, err
		}
	}
	rev, err := bumpPrincipalRevision(ctx, e.tx, p.ID, e.ms)
	if err != nil {
		return nil, err
	}
	if _, err := bumpPolicyRevision(ctx, e.tx); err != nil {
		return nil, err
	}
	return map[string]any{"principal_id": p.ID, "role": c.Role, "revision": rev}, e.emit(evParams{typ: evType,
		aggType: "principal", aggID: p.ID, revision: rev, payload: map[string]any{"principal_id": p.ID, "role": c.Role}})
}

// projectRevision 返回身份模块记录的项目访问控制修订；从未设置过为 0。
func projectRevision(ctx context.Context, q commands.DBTX, project ids.ID) (int64, error) {
	var rev int64
	err := q.QueryRowContext(ctx, `SELECT revision FROM identity_projects WHERE project_id = ?`, project).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return rev, err
}

func bumpProject(ctx context.Context, tx *sql.Tx, project ids.ID, expected, ms int64) (int64, error) {
	cur, err := projectRevision(ctx, tx, project)
	if err != nil {
		return 0, err
	}
	if cur != expected {
		return 0, preconditionFailed("project %s access control is at revision %d, the request expected %d", project, cur, expected)
	}
	var rev int64
	err = tx.QueryRowContext(ctx, `INSERT INTO identity_projects (project_id, revision, updated_at) VALUES (?, 1, ?)
		ON CONFLICT (project_id) DO UPDATE SET revision = identity_projects.revision + 1, updated_at = excluded.updated_at
		RETURNING revision`, project, ms).Scan(&rev)
	return rev, err
}

// SetProjectRole 授予或撤销项目角色。ExpectedRevision 是项目访问控制修订
// （从未设置过为 0）。角色变更立即生效：之后的最终接受按新角色判定。
type SetProjectRole struct {
	ProjectID        ids.ID `json:"project_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	PrincipalID      ids.ID `json:"principal_id"`
	Role             Role   `json:"role"`
	Grant            bool   `json:"grant"`
}

func (c *SetProjectRole) action() authz.Action {
	if c.Grant {
		return ActGrantProjectRole
	}
	return ActRevokeProjectRole
}
func (c *SetProjectRole) project() ids.ID { return c.ProjectID }
func (c *SetProjectRole) targets() []Target {
	return []Target{{Kind: "project", ID: string(c.ProjectID), ExpectedRevision: c.ExpectedRevision},
		{Kind: "principal", ID: string(c.PrincipalID)}}
}
func (c *SetProjectRole) body() any { return c }
func (c *SetProjectRole) check() error {
	if !c.ProjectID.Valid() || !c.PrincipalID.Valid() || c.ExpectedRevision < 0 || !c.Role.Valid() {
		return fmt.Errorf("project_id, principal_id, expected_revision and a valid role are required")
	}
	return nil
}
func (c *SetProjectRole) describe(ctx context.Context, q commands.DBTX) (string, error) {
	verb := "Revoke"
	if c.Grant {
		verb = "Grant"
	}
	return fmt.Sprintf("%s role %s in project %s for %s", verb, c.Role, c.ProjectID, describePrincipal(ctx, q, c.PrincipalID)), nil
}
func (c *SetProjectRole) apply(ctx context.Context, e *applyEnv) (any, error) {
	p, err := loadPrincipal(ctx, e.tx, c.PrincipalID)
	if errors.Is(err, errNotFound) {
		return nil, notFound("principal")
	}
	if err != nil {
		return nil, err
	}
	roles, err := projectRolesOf(ctx, e.tx, c.ProjectID, p.ID)
	if err != nil {
		return nil, err
	}
	evType := EvProjectRoleGranted
	if c.Grant {
		if p.State != StateActive {
			return nil, preconditionFailed("principal %s is disabled", p.Name)
		}
		if slices.Contains(roles, c.Role) {
			return nil, preconditionFailed("%s already has role %s in this project", p.Name, c.Role)
		}
	} else {
		evType = EvProjectRoleRevoked
		if !slices.Contains(roles, c.Role) {
			return nil, preconditionFailed("%s does not have role %s in this project", p.Name, c.Role)
		}
	}
	rev, err := bumpProject(ctx, e.tx, c.ProjectID, c.ExpectedRevision, e.ms)
	if err != nil {
		return nil, err
	}
	if c.Grant {
		_, err = e.tx.ExecContext(ctx, `INSERT INTO identity_project_roles (project_id, principal_id, role, granted_at, granted_by)
			VALUES (?, ?, ?, ?, ?)`, c.ProjectID, p.ID, c.Role, e.ms, e.actor.principal.ID)
	} else {
		_, err = e.tx.ExecContext(ctx, `DELETE FROM identity_project_roles WHERE project_id = ? AND principal_id = ? AND role = ?`,
			c.ProjectID, p.ID, c.Role)
	}
	if err != nil {
		return nil, err
	}
	if _, err := bumpPolicyRevision(ctx, e.tx); err != nil {
		return nil, err
	}
	return map[string]any{"project_id": c.ProjectID, "principal_id": p.ID, "role": c.Role, "revision": rev}, e.emit(evParams{
		typ: evType, aggType: "project", aggID: c.ProjectID, revision: rev, project: c.ProjectID,
		payload: map[string]any{"principal_id": p.ID, "role": c.Role}})
}

// SetPolicy 覆盖一项策略：ProjectID 为空时是实例级。值按登记的类型与范围
// 校验；固定策略（如破坏性操作必须由人执行）拒绝覆盖。ExpectedRevision 是该
// 覆盖项当前的修订，尚未覆盖时为 0。
type SetPolicy struct {
	ProjectID        ids.ID          `json:"project_id,omitempty"`
	Key              string          `json:"key"`
	Value            json.RawMessage `json:"value"`
	ExpectedRevision int64           `json:"expected_revision"`
}

func (c *SetPolicy) action() authz.Action {
	if c.ProjectID == "" {
		return ActSetSystemPolicy
	}
	return ActSetProjectPolicy
}
func (c *SetPolicy) project() ids.ID { return c.ProjectID }
func (c *SetPolicy) scope() string   { return string(c.ProjectID) }
func (c *SetPolicy) targets() []Target {
	return []Target{{Kind: "policy", ID: c.scope() + "/" + c.Key, ExpectedRevision: c.ExpectedRevision}}
}
func (c *SetPolicy) body() any { return c }
func (c *SetPolicy) check() error {
	if c.ProjectID != "" && !c.ProjectID.Valid() {
		return fmt.Errorf("project_id is not valid")
	}
	if c.ExpectedRevision < 0 {
		return fmt.Errorf("expected_revision must not be negative")
	}
	canon, err := normalizePolicy(c.Key, c.Value)
	if err != nil {
		return err
	}
	c.Value = json.RawMessage(canon)
	return nil
}
func (c *SetPolicy) describe(context.Context, commands.DBTX) (string, error) {
	where := "the instance"
	if c.ProjectID != "" {
		where = "project " + string(c.ProjectID)
	}
	return fmt.Sprintf("Set policy %s to %s for %s", c.Key, c.Value, where), nil
}
func (c *SetPolicy) apply(ctx context.Context, e *applyEnv) (any, error) {
	var cur int64
	err := e.tx.QueryRowContext(ctx, `SELECT revision FROM identity_policies WHERE scope = ? AND key = ?`, c.scope(), c.Key).Scan(&cur)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if cur != c.ExpectedRevision {
		return nil, preconditionFailed("policy %s is at revision %d, the request expected %d", c.Key, cur, c.ExpectedRevision)
	}
	var rev int64
	if err := e.tx.QueryRowContext(ctx, `INSERT INTO identity_policies (scope, key, value, revision, updated_at, updated_by)
		VALUES (?, ?, ?, 1, ?, ?) ON CONFLICT (scope, key) DO UPDATE SET value = excluded.value,
		revision = identity_policies.revision + 1, updated_at = excluded.updated_at, updated_by = excluded.updated_by
		RETURNING revision`, c.scope(), c.Key, string(c.Value), e.ms, e.actor.principal.ID).Scan(&rev); err != nil {
		return nil, err
	}
	global, err := bumpPolicyRevision(ctx, e.tx)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"key": c.Key, "value": c.Value, "revision": rev}
	if c.ProjectID == "" {
		return out, e.emit(evParams{typ: EvSystemPolicySet, aggType: "policy", aggID: e.s.instance, revision: global,
			payload: map[string]any{"key": c.Key, "value": c.Value, "revision": rev}})
	}
	prev, err := projectRevision(ctx, e.tx, c.ProjectID)
	if err != nil {
		return nil, err
	}
	prj, err := bumpProject(ctx, e.tx, c.ProjectID, prev, e.ms)
	if err != nil {
		return nil, err
	}
	out["project_id"] = c.ProjectID
	return out, e.emit(evParams{typ: EvProjectPolicySet, aggType: "project", aggID: c.ProjectID, revision: prj,
		project: c.ProjectID, payload: map[string]any{"key": c.Key, "value": c.Value, "revision": rev}})
}

// ResetHumanFactor 由另一位管理员重置某人的认证因子（恢复码也丢失时）：推进
// auth_epoch、停用因子、作废恢复码与人类授权，进入待设置状态，结果附带一次性
// 设置码。不能重置自己；自己的恢复走恢复码或本机离线恢复。
type ResetHumanFactor struct {
	PrincipalID      ids.ID `json:"principal_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

func (c *ResetHumanFactor) action() authz.Action { return ActResetHumanFactor }
func (c *ResetHumanFactor) project() ids.ID      { return "" }
func (c *ResetHumanFactor) targets() []Target {
	return []Target{{Kind: "principal", ID: string(c.PrincipalID), ExpectedRevision: c.ExpectedRevision}}
}
func (c *ResetHumanFactor) body() any { return c }
func (c *ResetHumanFactor) check() error {
	if !c.PrincipalID.Valid() || c.ExpectedRevision < 1 || c.Reason == "" || !shortTextRE.MatchString(c.Reason) {
		return fmt.Errorf("principal_id, expected_revision and a short plain-text reason are required")
	}
	return nil
}
func (c *ResetHumanFactor) describe(ctx context.Context, q commands.DBTX) (string, error) {
	return "Reset the authenticator of " + describePrincipal(ctx, q, c.PrincipalID) + ": " + c.Reason, nil
}
func (c *ResetHumanFactor) apply(ctx context.Context, e *applyEnv) (any, error) {
	p, err := principalFor(ctx, e.tx, c.PrincipalID, c.ExpectedRevision)
	if err != nil {
		return nil, err
	}
	if p.Kind != authz.Human {
		return nil, errcode.New(errcode.Forbidden, "only people have authenticators")
	}
	if p.ID == e.actor.principal.ID {
		return nil, errcode.New(errcode.Forbidden, "you cannot reset your own authenticator here; use a recovery code")
	}
	rev, epoch, err := bumpAuthEpoch(ctx, e.tx, p.ID, e.ms)
	if err != nil {
		return nil, err
	}
	if err := e.s.disableFactors(ctx, e.tx, p.ID); err != nil {
		return nil, err
	}
	if err := e.s.invalidateRecoveryCodes(ctx, e.tx, p.ID); err != nil {
		return nil, err
	}
	// 管理员设置流程必须重新设置口令，历史记录不能冒充本次已完成。
	// 自助恢复仍保留已由口令和恢复码验证过的原口令。
	if _, err := e.tx.ExecContext(ctx, `DELETE FROM identity_passwords WHERE principal_id = ?`, p.ID); err != nil {
		return nil, err
	}
	revoked, err := e.s.revokeGrants(ctx, e.tx, p.ID, "factor_reset")
	if err != nil {
		return nil, err
	}
	setup, err := ids.DeriveChild(e.op, "setup")
	if err != nil {
		return nil, err
	}
	if err := setAccount(ctx, e.tx, p.ID, factorSetupPending, setup, e.ms); err != nil {
		return nil, err
	}
	code, err := e.s.issueSetupCode(ctx, e.tx, p.ID, setup, e.actor.principal.ID)
	if err != nil {
		return nil, err
	}
	if _, err := bumpPolicyRevision(ctx, e.tx); err != nil {
		return nil, err
	}
	e.secret = code
	return map[string]any{"principal_id": p.ID, "revision": rev, "auth_epoch": epoch,
			"setup_code_expires_at": clock.Format(e.now.Add(e.s.cfg.SetupCodeTTL))},
		e.emit(evParams{typ: EvFactorReset, aggType: "principal", aggID: p.ID, revision: rev,
			payload: map[string]any{"principal_id": p.ID, "reason": c.Reason, "revoked_grants": revoked}})
}

// RevokeSession 由管理员吊销任意会话。吊销记录与回执在 main.db 同一事务提交，
// 会话在下一次核对时即失效。
type RevokeSession struct {
	SessionID ids.ID `json:"session_id"`
	Reason    string `json:"reason"`
}

func (c *RevokeSession) action() authz.Action { return ActRevokeSession }
func (c *RevokeSession) project() ids.ID      { return "" }
func (c *RevokeSession) targets() []Target {
	return []Target{{Kind: "session", ID: string(c.SessionID)}}
}
func (c *RevokeSession) body() any { return c }
func (c *RevokeSession) check() error {
	if !c.SessionID.Valid() || c.Reason == "" || !shortTextRE.MatchString(c.Reason) {
		return fmt.Errorf("session_id and a short plain-text reason are required")
	}
	return nil
}
func (c *RevokeSession) describe(context.Context, commands.DBTX) (string, error) {
	return fmt.Sprintf("Revoke session %s: %s", c.SessionID, c.Reason), nil
}
func (c *RevokeSession) apply(ctx context.Context, e *applyEnv) (any, error) {
	sess, err := loadSession(ctx, e.s.runtime, c.SessionID)
	if errors.Is(err, errNotFound) {
		return nil, notFound("session")
	}
	if err != nil {
		return nil, err
	}
	res, err := e.tx.ExecContext(ctx, `INSERT INTO identity_session_revocations (session_id, principal_id, revoked_at, revoked_by, reason, operation_id)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (session_id) DO NOTHING`, sess.ID, sess.PrincipalID, e.ms, e.actor.principal.ID, c.Reason, e.op)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, preconditionFailed("session %s is already revoked", sess.ID)
	}
	if _, err := bumpPolicyRevision(ctx, e.tx); err != nil {
		return nil, err
	}
	// 吊销记录是 main.db 中独立的聚合（与 runtime.db 的会话记录分开计修订）。
	return map[string]any{"session_id": sess.ID, "principal_id": sess.PrincipalID}, e.emit(evParams{typ: EvSessionRevoked,
		aggType: "session_revocation", aggID: sess.ID, revision: 1,
		payload: map[string]any{"session_id": sess.ID, "principal_id": sess.PrincipalID, "reason": c.Reason}})
}
