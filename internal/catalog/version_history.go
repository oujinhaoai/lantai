package catalog

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// VersionPage enumerates only currently readable committed versions. It does
// not expose counts, identifiers or a cursor position for unreadable versions.
type VersionPage struct {
	Items      []commit.Committed `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type versionCursorCodec struct{ aead cipher.AEAD }
type versionCursorData struct {
	Asset         ids.ID
	Principal     ids.ID
	Session       ids.ID
	AuthEpoch     int64
	RecoveryEpoch int64
	After         int64
}

func newVersionCursorCodec() (*versionCursorCodec, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &versionCursorCodec{aead: aead}, nil
}

func (c *versionCursorCodec) seal(value versionCursorData) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	// A hidden scan position must not leak through ciphertext length when the
	// version number changes decimal width. JSON accepts trailing whitespace.
	const paddedSize = 512
	if len(raw) > paddedSize {
		return "", errcode.New(errcode.Internal, "version cursor exceeds its fixed envelope")
	}
	padded := make([]byte, paddedSize)
	for i := range padded {
		padded[i] = ' '
	}
	copy(padded, raw)
	nonce := make([]byte, c.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, padded, []byte("lantai.catalog.version-cursor/v1"))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *versionCursorCodec) open(raw string) (versionCursorData, error) {
	var value versionCursorData
	if len(raw) > 4096 {
		return value, errcode.New(errcode.CursorExpired, "version cursor is invalid; restart version history")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) < c.aead.NonceSize() {
		return value, errcode.New(errcode.CursorExpired, "version cursor is invalid; restart version history")
	}
	n := c.aead.NonceSize()
	plain, err := c.aead.Open(nil, b[:n], b[n:], []byte("lantai.catalog.version-cursor/v1"))
	if err != nil {
		return value, errcode.New(errcode.CursorExpired, "version cursor is no longer available")
	}
	if err = json.Unmarshal(plain, &value); err != nil || value.After < 1 {
		return value, errcode.New(errcode.CursorExpired, "version cursor is invalid; restart version history")
	}
	return value, nil
}

// ListVersions uses the ledger's asset-indexed committed enumeration and the
// same current asset, rights and lifecycle checks as exact version reads. A
// security guard covers the complete response. It never calls GetAsset: an
// unreadable latest version must not hide an otherwise readable older version.
func (s *Service) ListVersions(ctx context.Context, who authz.Context, asset ids.ID, cursor string, limit int) (VersionPage, error) {
	out := VersionPage{Items: []commit.Committed{}}
	if !asset.Valid() || limit < 1 || limit > 100 {
		return out, invalid("invalid version history pagination")
	}
	ctx, held, err := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return out, err
	}
	defer held.Release()
	a, err := s.ledger.Asset(ctx, asset)
	if err != nil {
		return out, notFoundIfMissing(err)
	}
	if err = s.canRead(ctx, who, a.ProjectID, "asset", asset); err != nil {
		return out, err
	}
	if s.versionCursor == nil {
		return out, errcode.New(errcode.Internal, "version pagination is unavailable")
	}
	wanted := versionCursorData{Asset: asset, Principal: who.PrincipalID, Session: who.SessionID, AuthEpoch: who.AuthEpoch, RecoveryEpoch: who.RecoveryEpoch}
	after := int64(0)
	if cursor != "" {
		value, e := s.versionCursor.open(cursor)
		if e != nil {
			return out, e
		}
		after = value.After
		value.After = 0
		if value != wanted {
			return out, errcode.New(errcode.CursorExpired, "version cursor belongs to another asset or session")
		}
	}
	// Scan limits are not included in the response: hidden versions yield only
	// an opaque continuation, like the existing directory search contract.
	for scanned := 0; scanned < 1024; {
		batchSize := min(100, 1024-scanned)
		batch, e := s.ledger.AssetVersions(ctx, asset, after, batchSize)
		if e != nil {
			return out, e
		}
		if len(batch) == 0 {
			return out, nil
		}
		for i, v := range batch {
			if v.AssetID != asset || v.ProjectID != a.ProjectID || !v.VersionID.Valid() || v.VersionNumber <= after {
				return VersionPage{}, errcode.New(errcode.Internal, "invalid authoritative version history")
			}
			after = v.VersionNumber
			scanned++
			e = s.canReadRights(ctx, who, v)
			if e == nil {
				out.Items = append(out.Items, v)
			} else {
				switch errcode.CodeOf(e) {
				case errcode.NotFound, errcode.Forbidden, errcode.UseRestricted, errcode.RightsPending, errcode.AssetPurged:
				default:
					return VersionPage{}, e
				}
			}
			if len(out.Items) == limit || scanned == 1024 {
				if i+1 < len(batch) || len(batch) == batchSize {
					wanted.After = after
					out.NextCursor, e = s.versionCursor.seal(wanted)
				}
				return out, e
			}
		}
		if len(batch) < batchSize {
			return out, nil
		}
	}
	return out, nil
}
