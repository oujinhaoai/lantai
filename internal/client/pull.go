package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// Pull fetches an exact manifest and verifies every grant against it before a
// bounded streaming download. It is shared by CLI and MCP.
func (c *Client) Pull(ctx context.Context, asset, versionID, directory, purpose string) (Response, error) {
	if asset == "" || versionID == "" || directory == "" {
		return Response{}, errors.New("exact version and destination required")
	}
	path := "/api/v1/assets/" + url.PathEscape(asset) + "/versions/" + url.PathEscape(versionID)
	r, err := c.Do(ctx, http.MethodGet, path+"?view=full", nil, Options{})
	if err != nil {
		return r, err
	}
	var version struct {
		Manifest *struct {
			Content struct {
				Files []InputFile `json:"files"`
			} `json:"content"`
		} `json:"manifest"`
	}
	if err = r.Decode(&version); err != nil {
		return r, err
	}
	if version.Manifest == nil {
		return r, errors.New("client: server omitted the exact version manifest")
	}
	paths := make([]string, len(version.Manifest.Content.Files))
	for i, f := range version.Manifest.Content.Files {
		paths[i] = f.Path
	}
	if err = ValidatePaths(paths); err != nil {
		return Response{}, err
	}
	for _, f := range version.Manifest.Content.Files {
		grant, err := c.Do(ctx, http.MethodPost, path+"/read-grants", map[string]string{"path": f.Path, "purpose": purpose}, Options{})
		if err != nil {
			return grant, err
		}
		var g DownloadGrant
		if err = grant.Decode(&g); err != nil {
			return grant, err
		}
		if g.Path != f.Path || g.Size != f.Size || g.SHA256 != f.SHA256 {
			return Response{}, errors.New("client: read grant does not match the exact manifest")
		}
		if err = c.Download(ctx, g, directory, f.Path); err != nil {
			return Response{}, err
		}
	}
	return r, nil
}
