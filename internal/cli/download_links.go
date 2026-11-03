package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/oujinhaoai/lantai/internal/apiv1"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func backupsCommand(ctx context.Context, c *client.Client, action string, o options) (client.Response, error) {
	path := "/api/v1/admin/backups"
	switch action {
	case "overview", "":
	case "history":
		path += "/history"
		if o.explicitCompletion {
			if o.limit < 1 || o.limit > 20 || o.cursor != "" && !ids.ID(o.cursor).Valid() {
				return client.Response{}, usage("explicit backup history requires --limit 1..20 and an original job --cursor")
			}
			path += "?" + url.Values{"completion": {"explicit"}, "limit": {fmt.Sprint(o.limit)}, "cursor": {o.cursor}}.Encode()
		} else if o.cursor != "" {
			return client.Response{}, usage("history continuation requires --explicit-completion")
		}
	case "schedule-get":
		path += "/schedule"
	case "start", "resume", "schedule-set":
		if o.key == "" {
			return client.Response{}, usage("--idempotency-key is required; preserve the original approval, request and key")
		}
		if action == "resume" && !ids.ID(o.id).Valid() {
			return client.Response{}, usage("--id original backup job ULID is required")
		}
		raw, err := input(o.input)
		if err != nil {
			return client.Response{}, err
		}
		method := http.MethodPost
		if action == "schedule-set" {
			var in apiv1.BackupScheduleRequest
			if err := strictDecode(raw, &in); err != nil {
				return client.Response{}, err
			}
			path, method = path+"/schedule", http.MethodPut
		} else {
			var in apiv1.BackupRunRequest
			if err := strictDecode(raw, &in); err != nil {
				return client.Response{}, err
			}
			if action == "resume" {
				path += "/" + o.id + "/resume"
			}
		}
		// Forward original bytes: defaults and approval matching belong to core.
		return c.Do(ctx, method, path, raw, client.Options{IdempotencyKey: o.key})
	case "cancel":
		if !ids.ID(o.id).Valid() {
			return client.Response{}, usage("--id original backup job ULID is required")
		}
		return c.Do(ctx, http.MethodPost, path+"/"+o.id+"/cancel", struct{}{}, client.Options{})
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
		return client.Response{}, usage("backups requires overview, history, show, start, resume, cancel, schedule-get, schedule-set or download-link")
	}
	return c.Do(ctx, http.MethodGet, path, nil, client.Options{})
}
