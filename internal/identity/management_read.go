package identity

import (
	"context"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type PrincipalPage struct {
	Items      []Principal `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// CredentialInfo contains revocable management metadata, never the credential
// secret or its hash. The prefix is only a label, not an authentication token.
type CredentialInfo struct {
	ID          ids.ID     `json:"credential_id"`
	PrincipalID ids.ID     `json:"principal_id"`
	Prefix      string     `json:"prefix"`
	Scopes      []Scope    `json:"scopes"`
	Projects    []ids.ID   `json:"projects"`
	Label       string     `json:"label"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}
type CredentialPage struct {
	Items      []CredentialInfo `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

func managementPagination(after ids.ID, limit int) error {
	if limit < 1 || limit > 100 || after != "" && !after.Valid() {
		return errcode.New(errcode.SchemaInvalid, "invalid identity management pagination")
	}
	return nil
}

// PrincipalPage is an instance-administrator view. The existing action matrix
// still requires a fresh live normal nondelegated human session and admin scope.
// The complete bounded query runs while the security guard is held.
func (s *Service) PrincipalPage(ctx context.Context, who authz.Context, after ids.ID, limit int) (PrincipalPage, error) {
	out := PrincipalPage{Items: []Principal{}}
	if err := managementPagination(after, limit); err != nil {
		return out, err
	}
	err := s.BeginRead(ctx, who, ActReadPrincipals, authz.Resource{}, func(ctx context.Context, _ authz.Decision) error {
		rows, err := s.main.QueryContext(ctx, `SELECT `+principalCols+` FROM identity_principals WHERE principal_id>? ORDER BY principal_id LIMIT ?`, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPrincipal(rows)
			if err != nil {
				return err
			}
			if len(out.Items) == limit {
				out.NextCursor = string(out.Items[len(out.Items)-1].ID)
				break
			}
			out.Items = append(out.Items, p)
		}
		return rows.Err()
	})
	if err != nil {
		return PrincipalPage{}, err
	}
	return out, nil
}

func (s *Service) CredentialPage(ctx context.Context, who authz.Context, principal, after ids.ID, limit int) (CredentialPage, error) {
	out := CredentialPage{Items: []CredentialInfo{}}
	if !principal.Valid() {
		return out, errcode.New(errcode.SchemaInvalid, "principal_id required")
	}
	if err := managementPagination(after, limit); err != nil {
		return out, err
	}
	err := s.BeginRead(ctx, who, ActReadPrincipals, authz.Resource{}, func(ctx context.Context, _ authz.Decision) error {
		if _, err := loadPrincipal(ctx, s.main, principal); err != nil {
			if err == errNotFound {
				return notFound("principal")
			}
			return err
		}
		rows, err := s.main.QueryContext(ctx, `SELECT `+credentialCols+` FROM identity_credentials WHERE principal_id=? AND credential_id>? ORDER BY credential_id LIMIT ?`, principal, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCredential(rows)
			if err != nil {
				return err
			}
			if len(out.Items) == limit {
				out.NextCursor = string(out.Items[len(out.Items)-1].ID)
				break
			}
			v := CredentialInfo{ID: c.ID, PrincipalID: c.PrincipalID, Prefix: c.Prefix, Scopes: c.Scopes, Projects: c.Projects, Label: c.Label, CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt}
			if v.Scopes == nil {
				v.Scopes = []Scope{}
			}
			if v.Projects == nil {
				v.Projects = []ids.ID{}
			}
			if !c.RevokedAt.IsZero() {
				v.RevokedAt = &c.RevokedAt
			}
			out.Items = append(out.Items, v)
		}
		return rows.Err()
	})
	if err != nil {
		return CredentialPage{}, err
	}
	return out, nil
}
