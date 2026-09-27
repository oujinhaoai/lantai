package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
)

// ListPrincipals 列出全部主体（管理员）。
func (s *Service) ListPrincipals(ctx context.Context, who authz.Context) ([]Principal, error) {
	v, err := s.current(ctx, who)
	if err != nil {
		return nil, err
	}
	if err := s.require(ctx, s.main, v, ActReadPrincipals, authz.Resource{}); err != nil {
		return nil, err
	}
	rows, err := s.main.QueryContext(ctx, `SELECT `+principalCols+` FROM identity_principals ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Principal
	for rows.Next() {
		p, err := scanPrincipal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPrincipal 按 ID 读取主体（管理员）。
func (s *Service) GetPrincipal(ctx context.Context, who authz.Context, id ids.ID) (Principal, error) {
	v, err := s.current(ctx, who)
	if err != nil {
		return Principal{}, err
	}
	if err := s.require(ctx, s.main, v, ActReadPrincipals, authz.Resource{}); err != nil {
		return Principal{}, err
	}
	p, err := loadPrincipal(ctx, s.main, id)
	if errors.Is(err, errNotFound) {
		return p, notFound("principal")
	}
	return p, err
}

// Member 是项目成员及其角色。
type Member struct {
	PrincipalID ids.ID              `json:"principal_id"`
	Name        string              `json:"name"`
	Kind        authz.PrincipalKind `json:"kind"`
	Roles       []Role              `json:"roles"`
}

// Members 列出项目成员；要求调用者是该项目成员或管理员。返回项目访问控制
// 修订，供角色变更命令作为预期修订。
func (s *Service) Members(ctx context.Context, who authz.Context, project ids.ID) ([]Member, int64, error) {
	v, err := s.current(ctx, who)
	if err != nil {
		return nil, 0, err
	}
	if err := s.require(ctx, s.main, v, ActReadMembers, authz.Resource{ProjectID: project}); err != nil {
		return nil, 0, err
	}
	rev, err := projectRevision(ctx, s.main, project)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.main.QueryContext(ctx, `SELECT principal_id, role FROM identity_project_roles WHERE project_id = ? ORDER BY principal_id, role`, project)
	if err != nil {
		return nil, 0, err
	}
	var order []ids.ID
	byID := map[ids.ID]*Member{}
	for rows.Next() {
		var id ids.ID
		var r Role
		if err := rows.Scan(&id, &r); err != nil {
			rows.Close()
			return nil, 0, err
		}
		m, ok := byID[id]
		if !ok {
			m = &Member{PrincipalID: id}
			byID[id] = m
			order = append(order, id)
		}
		m.Roles = append(m.Roles, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	out := make([]Member, 0, len(order))
	for _, id := range order {
		m := byID[id]
		p, err := loadPrincipal(ctx, s.main, id)
		if err == nil {
			m.Name, m.Kind = p.Name, p.Kind
		} else if !errors.Is(err, errNotFound) {
			return nil, 0, err
		}
		out = append(out, *m)
	}
	return out, rev, nil
}

// CheckStartup 是实例启动前的核对步骤：已登记的 TOTP 因子必须能用当前主密钥
// 解开，主密钥缺失或与因子不符时实例不开放服务。
func (s *Service) CheckStartup(ctx context.Context) error {
	rows, err := s.main.QueryContext(ctx, `SELECT factor_id, principal_id, key_id, sealed_secret
		FROM identity_totp_factors WHERE state IN ('enabled', 'pending') ORDER BY factor_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var factor factorRow
		if err := rows.Scan(&factor.ID, &factor.PrincipalID, &factor.KeyID, &factor.Sealed); err != nil {
			return err
		}
		secret, err := s.openFactor(factor)
		if err != nil {
			return fmt.Errorf("identity: authenticator factor %s cannot be opened; restore the matching master key and intact factor record from backup: %w", factor.ID, err)
		}
		valid := len(secret) == totp.SecretLen
		clear(secret)
		if !valid {
			return fmt.Errorf("identity: authenticator factor %s has an invalid secret; restore its intact factor record from backup", factor.ID)
		}
	}
	return rows.Err()
}
