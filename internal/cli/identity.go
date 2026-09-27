package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"

	"github.com/oujinhaoai/lantai/internal/client"
)

func privateInput(path string) (json.RawMessage, error) {
	if path == "" {
		return nil, usage("--credentials-file is required for secret input")
	}
	b, err := client.ReadPrivate(path)
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		return nil, usage("credentials input must be JSON")
	}
	return b, nil
}
func setup(ctx context.Context, c *client.Client, o options) (client.Response, error) {
	if o.session == "" {
		return client.Response{}, usage("--session-file is required")
	}
	b, err := privateInput(o.credentials)
	if err != nil {
		return client.Response{}, err
	}
	var in struct {
		Name string `json:"name"`
		Code string `json:"code"`
	}
	if err = strictDecode(b, &in); err != nil {
		return client.Response{}, err
	}
	r, err := c.Do(ctx, http.MethodPost, "/api/v1/sessions/setup", map[string]string{"name": in.Name, "code": in.Code, "channel": "cli"}, client.Options{})
	if err != nil {
		return r, err
	}
	var out struct {
		Token string `json:"token"`
	}
	if err = r.Decode(&out); err != nil {
		return r, err
	}
	if out.Token == "" {
		return r, errors.New("client: server omitted setup session token")
	}
	if err = client.SaveState(o.session, sessionFile{Schema: "lantai.client-session/v1", Origin: c.Origin(), Token: out.Token}); err != nil {
		return client.Response{}, err
	}
	return r, nil
}
func identityCommand(ctx context.Context, c *client.Client, action string, o options) (client.Response, error) {
	method, path := http.MethodPost, ""
	var body any
	var err error
	secret := false
	switch action {
	case "challenge":
		path = "/api/v1/identity/challenges"
		body, err = input(o.input)
	case "verify":
		if o.id == "" {
			return client.Response{}, usage("--id challenge ID is required")
		}
		path = "/api/v1/identity/challenges/" + segment(o.id) + "/verify"
		body, err = privateInput(o.credentials)
	case "execute":
		if o.key == "" {
			return client.Response{}, usage("--idempotency-key is required")
		}
		path = "/api/v1/identity/commands"
		body, err = input(o.input)
		var in struct {
			Action string `json:"action"`
		}
		if b, ok := body.(json.RawMessage); ok {
			_ = json.Unmarshal(b, &in)
		}
		secret = in.Action == "identity.register_principal" || in.Action == "identity.issue_credential"
	case "principal":
		if o.id == "" {
			return client.Response{}, usage("--id principal ID is required")
		}
		method, path = http.MethodGet, "/api/v1/identity/principals/"+segment(o.id)
	case "members":
		if o.project == "" {
			return client.Response{}, usage("--project is required")
		}
		method, path = http.MethodGet, "/api/v1/identity/projects/"+segment(o.project)+"/members"
	case "enroll":
		path = "/api/v1/identity/factor/enroll"
		body = map[string]any{}
		secret = true
	case "password":
		method, path = http.MethodPut, "/api/v1/identity/password"
		body, err = privateInput(o.credentials)
	case "confirm":
		path = "/api/v1/identity/factor/confirm"
		body, err = privateInput(o.credentials)
		secret = true
	default:
		return client.Response{}, usage("identity requires challenge, verify, execute, principal, members, enroll, password, or confirm")
	}
	if err != nil {
		return client.Response{}, err
	}
	if secret {
		if o.secretFile == "" {
			return client.Response{}, usage("--secret-file is required to save one-time identity output")
		}
		if _, err = os.Lstat(o.secretFile); err == nil {
			return client.Response{}, usage("secret output already exists; choose a new private file")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return client.Response{}, err
		}
		if err = client.SaveState(o.secretFile, map[string]any{"pending": true}); err != nil {
			return client.Response{}, err
		}
	}
	r, err := c.Do(ctx, method, path, body, client.Options{IdempotencyKey: o.key})
	if err != nil {
		return r, err
	}
	if secret {
		if err = client.WritePrivate(o.secretFile, r.Body); err != nil {
			return client.Response{}, errors.New("client: identity command succeeded but saving its one-time secret failed; reconcile the operation before retrying")
		}
		// Enrollment's otpauth URI embeds the seed; show no portion on stdout.
		if action == "enroll" || action == "confirm" {
			r.Body = json.RawMessage(`{"saved":true}`)
		}
	}
	return r, nil
}
