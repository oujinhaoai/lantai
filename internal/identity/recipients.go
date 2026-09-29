package identity

import (
	"context"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// ProjectRecipients is a trusted core routing port, not a public membership API.
// It reads current active identities and roles without fabricating a user session
// or granting data access. Every inbox read must separately authorize the actual
// session and object. The caller provides the security/maintenance guard.
func (s *Service) ProjectRecipients(ctx context.Context, project ids.ID) ([]Member, error) {
	if !project.Valid() {
		return nil, errcode.New(errcode.SchemaInvalid, "")
	}
	rows, err := s.main.QueryContext(ctx, `SELECT p.principal_id,p.kind,r.role FROM identity_project_roles r JOIN identity_principals p ON p.principal_id=r.principal_id WHERE r.project_id=? AND p.state='active' ORDER BY p.principal_id,r.role`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		var role Role
		if err = rows.Scan(&m.PrincipalID, &m.Kind, &role); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].PrincipalID != m.PrincipalID {
			out = append(out, m)
		}
		last := &out[len(out)-1]
		last.Roles = append(last.Roles, role)
	}
	return out, rows.Err()
}
