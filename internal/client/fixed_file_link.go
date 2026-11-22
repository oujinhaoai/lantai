package client

import (
	"context"
	"net/url"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type FixedFileLinkInput struct {
	AssetID   string `json:"asset_id"`
	VersionID string `json:"version_id"`
	Path      string `json:"path"`
}

type FixedFileLinkResult struct {
	FixedFileLinkInput
	URL string `json:"url"`
}

// FixedFileLink only reads current authorized manifest metadata. It does not
// issue a read/download grant or claim to verify local or remote file bytes.
func (c *Client) FixedFileLink(ctx context.Context, in FixedFileLinkInput) (FixedFileLinkResult, error) {
	var out FixedFileLinkResult
	if !ids.ID(in.AssetID).Valid() || !ids.ID(in.VersionID).Valid() || worksetPath(in.Path) != nil {
		return out, worksetInvalid("exact asset ID, version ID and file path required")
	}
	u, err := url.Parse(c.Origin())
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return out, worksetInvalid("fixed file link requires an HTTPS server origin")
	}
	instance, err := c.worksetInstance(ctx)
	if err != nil {
		return out, err
	}
	manifest, err := c.worksetManifest(ctx, WorksetEntry{AssetID: in.AssetID, VersionID: in.VersionID}, instance)
	if err != nil {
		return out, err
	}
	for _, file := range manifest.Files {
		if file.SourcePath != in.Path {
			continue
		}
		q := url.Values{"file": {in.Path}, "version": {in.VersionID}}
		u.Path = "/"
		// The fragment hosts a second URL; keep its already escaped query bytes,
		// while retaining a decoded Fragment so net/url recognizes RawFragment.
		u.RawFragment = "/assets/" + in.AssetID + "?" + q.Encode()
		u.Fragment, _ = url.PathUnescape(u.RawFragment)
		return FixedFileLinkResult{FixedFileLinkInput: in, URL: u.String()}, nil
	}
	return out, errcode.New(errcode.NotFound, "exact file is absent from the manifest")
}
