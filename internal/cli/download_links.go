package cli

import (
	"context"
	"net/http"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func backupsCommand(ctx context.Context, c *client.Client, action string, o options) (client.Response, error) {
	path := "/api/v1/admin/backups"
	switch action {
	case "overview", "":
	case "history":
		path += "/history"
	case "show", "download-link":
		if !ids.ID(o.id).Valid() {
			return client.Response{}, usage("--id backup job ULID is required")
		}
		if action == "download-link" {
			if o.key == "" {
				return client.Response{}, usage("--idempotency-key is required")
			}
			raw, err := input(o.input)
			if err != nil {
				return client.Response{}, err
			}
			var in client.BackupDownloadInput
			if err = strictDecode(raw, &in); err != nil {
				return client.Response{}, err
			}
			return c.BackupDownloadLink(ctx, o.id, in, o.key)
		}
		path += "/" + o.id
	default:
		return client.Response{}, usage("backups requires overview, history, show or download-link")
	}
	return c.Do(ctx, http.MethodGet, path, nil, client.Options{})
}
