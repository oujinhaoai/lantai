package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// MaxResourceJSONBytes covers the default 16 MiB rendered manifest, worst-case
// six-byte JSON escaping per source byte, and 64 KiB of fixed response envelope.
// It is also a hard ceiling for current descriptions, which can grow separately.
// Requests, ordinary responses and error envelopes retain the 8 MiB budget.
const MaxResourceJSONBytes int64 = 6*(16<<20) + (64 << 10)

// ReadResourceJSON reads only an exact local resource through the normal
// authenticated, nonredirecting API transport. Callers must reauthorize every
// page/chunk; this method neither caches objects nor grants download access.
func (c *Client) ReadResourceJSON(ctx context.Context, path string) (Response, error) {
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || strings.Contains(path, "#") || u.RawPath != "" || u.ForceQuery {
		return Response{}, errors.New("client: invalid exact resource path")
	}
	parts := strings.Split(u.Path, "/")
	valid := len(parts) >= 5 && parts[0] == "" && parts[1] == "api" && parts[2] == "v1" && parts[3] == "assets" && ids.ID(parts[4]).Valid()
	full := u.RawQuery == "view=full"
	asset := len(parts) == 5 && full
	version := len(parts) == 7 && parts[5] == "versions" && ids.ID(parts[6]).Valid() && full
	rights := len(parts) == 8 && parts[5] == "versions" && ids.ID(parts[6]).Valid() && parts[7] == "rights" && u.RawQuery == ""
	if !valid || !(asset || version || rights) {
		return Response{}, errors.New("client: exact full asset/version or current rights path required")
	}
	return c.do(ctx, http.MethodGet, path, nil, Options{}, MaxResourceJSONBytes)
}
