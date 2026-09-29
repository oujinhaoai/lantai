// Package cli implements only remote HTTP commands. It has no access to the
// service's database, data directory, authorization engine, or plugin host.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/yamljson"
)

func Names() []string {
	return []string{"meta", "login", "whoami", "session", "identity", "project", "types", "upload", "push", "commit", "show", "pull", "metadata", "search", "operation", "task", "flow", "run", "job", "node", "message", "review", "rights", "evidence", "trash", "human", "context", "events", "resync", "inbox", "plugin", "ext"}
}
func Help() string {
	return `远程命令：meta | login | whoami | session exchange/setup/end | identity challenge/verify/execute/principal/members/enroll/password/confirm | project list/show/create | types | upload [status/cancel/check] | push | commit | show | pull | metadata get/set | search | operation
公共参数：--server HTTPS_ORIGIN --session-file FILE [--json] [--allow-http（仅loopback开发）]
凭据：LANTAI_SESSION_TOKEN 或私有会话文件；login --credentials-file FILE；session exchange --token-file FILE 或 LANTAI_TOKEN。
push/upload --input MANIFEST [--directory DIR] [--state FILE]；恢复必须复用同一state。
show/pull --asset ID --version ID（或 --ref lantai://...）；pull --directory DIR。
项目创建、commit、metadata set 必须显式 --idempotency-key KEY；metadata set 还需 --if-match '"REVISION"'。
协作命令：task | flow | run | job | node | message | review | rights | evidence | trash | human | context | events | resync | inbox。
扩展：plugin import/list/enablements/probe/commands（启停经 human prepare 的 extension_enable/extension_disable）；
ext install --package DIR --registry FILE | ext list | ext remove --plugin ID | ext run <plugin-id> <command> --registry FILE --output DIR [--input-file F] [--arg A]。
写命令用 --input JSON/YAML --idempotency-key KEY；列表用 --project ID --cursor ID --limit N；run list 用 --id TASK_ID。
human prepare 返回冻结目标与摘要；本人核对后 human verify --id CHALLENGE_ID --credentials-file FILE，再 human items/execute。
详见 docs/contracts/client.md。`
}

type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }
func usage(message string) error    { return &usageError{message} }

type sessionFile struct {
	Schema string `json:"schema"`
	Origin string `json:"origin"`
	Token  string `json:"token"`
}
type options struct {
	secretFile                                                                                                                                                           string
	server, session, input, directory, state, key, etag, asset, version, ref, upload, id, project, query, assetType, cursor, view, purpose, credentials, tokenFile, name string
	allowHTTP, json                                                                                                                                                      bool
	limit                                                                                                                                                                int
}

// Run receives the command name as args[0]. It never calls os.Exit, making
// cancellation and output ownership explicit for the executable and tests.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stdout, Help())
		return 0
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(stdout, Help())
		return 0
	}
	r, err := run(ctx, args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(stdout, Help())
		return 0
	}
	if err != nil {
		return writeError(stderr, err)
	}
	if len(r.Body) == 0 {
		r.Body = json.RawMessage(`{}`)
	}
	var value any
	if err = json.Unmarshal(r.Body, &value); err != nil {
		return writeError(stderr, errors.New("client: invalid response JSON"))
	}
	redactResult(args[0], value)
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	if err = enc.Encode(value); err != nil {
		return 3
	}
	return 0
}
func redactResult(command string, v any) {
	x, ok := v.(map[string]any)
	if !ok {
		return
	}
	switch command {
	case "login", "session":
		delete(x, "token")
		delete(x, "csrf_token")
	case "identity":
		delete(x, "secret")
		delete(x, "recovery_codes")
	case "upload":
		delete(x, "parts_url")
		if files, ok := x["files"].([]any); ok {
			for _, f := range files {
				if file, ok := f.(map[string]any); ok {
					delete(file, "part_url_template")
				}
			}
		}
	}
}

func run(ctx context.Context, args []string) (client.Response, error) {
	command := args[0]
	rest := args[1:]
	if command == "ext" {
		return extCommand(ctx, rest)
	}
	action := ""
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		switch command {
		case "session", "identity", "project", "upload", "metadata", "task", "flow", "run", "job", "node", "message", "review", "rights", "evidence", "trash", "human", "context", "events", "resync", "inbox", "plugin":
			action = rest[0]
			rest = rest[1:]
		}
	}
	var o options
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&o.server, "server", os.Getenv("LANTAI_SERVER"), "HTTPS gateway origin")
	f.StringVar(&o.session, "session-file", os.Getenv("LANTAI_SESSION"), "private session file")
	f.BoolVar(&o.allowHTTP, "allow-http", false, "allow loopback HTTP for development")
	f.BoolVar(&o.json, "json", false, "stable JSON output (default)")
	f.StringVar(&o.input, "input", "", "JSON/YAML request document")
	f.StringVar(&o.directory, "directory", "", "local working copy directory")
	f.StringVar(&o.state, "state", "", "private persistent transfer state")
	f.StringVar(&o.key, "idempotency-key", "", "stable caller-saved idempotency key")
	f.StringVar(&o.etag, "if-match", "", "quoted metadata revision ETag")
	f.StringVar(&o.asset, "asset", "", "asset id")
	f.StringVar(&o.version, "version", "", "version id")
	f.StringVar(&o.ref, "ref", "", "exact permanent lantai URI")
	f.StringVar(&o.upload, "upload", "", "upload id")
	f.StringVar(&o.id, "id", "", "operation id")
	f.StringVar(&o.project, "project", "", "project id for filtering")
	f.StringVar(&o.name, "name", "", "project key or type name")
	f.StringVar(&o.query, "query", "", "search text")
	f.StringVar(&o.assetType, "type", "", "asset type")
	f.StringVar(&o.cursor, "cursor", "", "opaque pagination cursor")
	f.StringVar(&o.view, "view", "brief", "brief or full")
	f.StringVar(&o.purpose, "purpose", "archive_review", "read purpose")
	f.IntVar(&o.limit, "limit", 50, "page size")
	f.StringVar(&o.credentials, "credentials-file", "", "private login JSON file")
	f.StringVar(&o.tokenFile, "token-file", "", "private long-lived credential file")
	f.StringVar(&o.secretFile, "secret-file", "", "private output for one-time identity secrets")
	if err := f.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return client.Response{}, err
		}
		return client.Response{}, usage("invalid command arguments; use --help")
	}
	if f.NArg() != 0 {
		return client.Response{}, usage("unexpected positional arguments")
	}
	if o.server == "" {
		return client.Response{}, usage("--server or LANTAI_SERVER is required")
	}
	c, err := client.New(client.Config{BaseURL: o.server, AllowHTTP: o.allowHTTP})
	if err != nil {
		return client.Response{}, usage(err.Error())
	}
	defer c.Close()
	if command != "meta" && command != "login" && !(command == "session" && (action == "exchange" || action == "setup")) {
		token := os.Getenv("LANTAI_SESSION_TOKEN")
		if token == "" && o.session != "" {
			var saved sessionFile
			if err = client.LoadState(o.session, &saved); err != nil {
				return client.Response{}, err
			}
			if saved.Schema != "lantai.client-session/v1" || saved.Origin != c.Origin() {
				return client.Response{}, usage("session file belongs to a different server")
			}
			token = saved.Token
		}
		if token == "" {
			return client.Response{}, errcode.New(errcode.AuthRequired, "session credential is required")
		}
		if err = c.SetSessionToken(token); err != nil {
			return client.Response{}, err
		}
	}
	if o.ref != "" {
		ref, err := ids.ParseURI(o.ref)
		if err != nil {
			return client.Response{}, usage("invalid permanent reference")
		}
		if o.asset != "" || o.version != "" {
			return client.Response{}, usage("use either --ref or --asset/--version")
		}
		meta, err := c.Do(ctx, http.MethodGet, "/api/v1/meta", nil, client.Options{})
		if err != nil {
			return meta, err
		}
		var instance struct {
			InstanceID string `json:"instance_id"`
		}
		if err = meta.Decode(&instance); err != nil {
			return meta, err
		}
		if instance.InstanceID != string(ref.InstanceID) {
			return client.Response{}, usage("permanent reference belongs to another instance")
		}
		o.asset, o.version = string(ref.AssetID), string(ref.VersionID)
	}
	return execute(ctx, c, command, action, o)
}

func input(path string) (json.RawMessage, error) {
	if path == "" {
		return nil, usage("--input is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, client.MaxJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > client.MaxJSONBytes {
		return nil, usage("input exceeds 8 MiB")
	}
	b, err = yamljson.ToJSON(b)
	if err != nil {
		return nil, usage("input must be a single valid JSON/YAML document")
	}
	return b, nil
}
func strictDecode(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return usage("request contains invalid or unknown fields")
	}
	return nil
}
func needed(v, message string) error {
	if v == "" {
		return usage(message)
	}
	return nil
}
func segment(s string) string { return url.PathEscape(s) }
func readPath(o options) (string, error) {
	if o.asset == "" || o.version == "" {
		return "", usage("--asset and --version (or --ref) are required")
	}
	return "/api/v1/assets/" + segment(o.asset) + "/versions/" + segment(o.version), nil
}
func execute(ctx context.Context, c *client.Client, command, action string, o options) (client.Response, error) {
	opts := client.Options{IdempotencyKey: o.key, IfMatch: o.etag}
	do := func(method, path string, body any) (client.Response, error) {
		return c.Do(ctx, method, path, body, opts)
	}
	switch command {
	case "task", "flow", "run", "job", "node", "message", "review", "rights", "evidence", "trash", "human", "context", "events", "resync", "inbox":
		return collaborationCommand(ctx, c, command, action, o)
	case "plugin":
		return pluginCommand(ctx, c, action, o)
	case "meta":
		return do(http.MethodGet, "/api/v1/meta", nil)
	case "login":
		return login(ctx, c, o, false)
	case "whoami":
		return do(http.MethodGet, "/api/v1/whoami", nil)
	case "session":
		switch action {
		case "setup":
			return setup(ctx, c, o)
		case "exchange":
			return login(ctx, c, o, true)
		case "end":
			r, err := do(http.MethodDelete, "/api/v1/sessions/current", nil)
			if err == nil && o.session != "" {
				err = client.SaveState(o.session, sessionFile{Schema: "lantai.client-session/v1", Origin: c.Origin()})
			}
			return r, err
		default:
			return client.Response{}, usage("session requires exchange or end")
		}
	case "project":
		switch action {
		case "", "list":
			q := url.Values{"cursor": {o.cursor}, "limit": {fmt.Sprint(o.limit)}, "view": {o.view}}
			return do(http.MethodGet, "/api/v1/projects?"+q.Encode(), nil)
		case "show":
			if err := needed(o.name, "--name project key is required"); err != nil {
				return client.Response{}, err
			}
			return do(http.MethodGet, "/api/v1/projects/"+segment(o.name)+"?view="+url.QueryEscape(o.view), nil)
		case "create":
			if err := needed(o.key, "--idempotency-key is required"); err != nil {
				return client.Response{}, err
			}
			b, err := input(o.input)
			if err != nil {
				return client.Response{}, err
			}
			return do(http.MethodPost, "/api/v1/projects", b)
		default:
			return client.Response{}, usage("project requires list, show, or create")
		}
	case "types":
		path := "/api/v1/types"
		if o.name != "" {
			path += "/" + segment(o.name)
		}
		return do(http.MethodGet, path, nil)
	case "search":
		q := url.Values{"q": {o.query}, "project_id": {o.project}, "asset_type": {o.assetType}, "cursor": {o.cursor}, "limit": {fmt.Sprint(o.limit)}, "view": {o.view}}
		return do(http.MethodGet, "/api/v1/assets?"+q.Encode(), nil)
	case "operation":
		if err := needed(o.id, "--id is required"); err != nil {
			return client.Response{}, err
		}
		return do(http.MethodGet, "/api/v1/operations/"+segment(o.id), nil)
	case "show":
		if o.asset == "" {
			return client.Response{}, usage("--asset is required")
		}
		path := "/api/v1/assets/" + segment(o.asset)
		if o.version != "" {
			path += "/versions/" + segment(o.version)
		}
		return do(http.MethodGet, path+"?view="+url.QueryEscape(o.view), nil)
	case "metadata":
		if o.asset == "" {
			return client.Response{}, usage("--asset is required")
		}
		path := "/api/v1/assets/" + segment(o.asset)
		switch action {
		case "", "get":
			return do(http.MethodGet, path+"?view=full", nil)
		case "set":
			if o.etag == "" || o.key == "" {
				return client.Response{}, usage("metadata set requires --if-match and --idempotency-key")
			}
			b, err := input(o.input)
			if err != nil {
				return client.Response{}, err
			}
			return do(http.MethodPatch, path+"/metadata", b)
		default:
			return client.Response{}, usage("metadata requires get or set")
		}
	case "commit":
		if o.upload == "" || o.key == "" {
			return client.Response{}, usage("commit requires --upload and --idempotency-key")
		}
		b, err := input(o.input)
		if err != nil {
			return client.Response{}, err
		}
		return do(http.MethodPost, "/api/v1/uploads/"+segment(o.upload)+"/commit", b)
	case "upload", "push":
		if command == "upload" && action != "" {
			if o.upload == "" {
				return client.Response{}, usage("--upload is required")
			}
			path := "/api/v1/uploads/" + segment(o.upload)
			switch action {
			case "status":
				return do(http.MethodGet, path, nil)
			case "cancel":
				return do(http.MethodDelete, path, nil)
			case "check":
				b, err := input(o.input)
				if err != nil {
					return client.Response{}, err
				}
				return do(http.MethodPost, path+"/check", b)
			default:
				return client.Response{}, usage("upload requires status, cancel, check, or --input")
			}
		}
		b, err := input(o.input)
		if err != nil {
			return client.Response{}, err
		}
		var in client.PushInput
		if err = strictDecode(b, &in); err != nil {
			return client.Response{}, err
		}
		if o.directory == "" {
			o.directory = filepath.Dir(o.input)
		}
		if o.state == "" {
			o.state = o.input + ".lantai-state.json"
		}
		return c.Push(ctx, in, o.directory, o.state, command == "upload")
	case "pull":
		return pull(ctx, c, o)
	case "identity":
		return identityCommand(ctx, c, action, o)
	default:
		return client.Response{}, usage("unknown remote command")
	}
}

func login(ctx context.Context, c *client.Client, o options, exchange bool) (client.Response, error) {
	if o.session == "" {
		return client.Response{}, usage("--session-file is required to save the issued credential")
	}
	body := map[string]any{"channel": "cli"}
	path := "/api/v1/sessions/login"
	if exchange {
		token := os.Getenv("LANTAI_TOKEN")
		if o.tokenFile != "" {
			b, err := client.ReadPrivate(o.tokenFile)
			if err != nil {
				return client.Response{}, err
			}
			token = strings.TrimSpace(string(b))
		}
		if token == "" {
			return client.Response{}, usage("--token-file or LANTAI_TOKEN is required")
		}
		body["token"] = token
		path = "/api/v1/sessions/exchange"
	} else {
		if o.credentials == "" {
			return client.Response{}, usage("--credentials-file is required")
		}
		b, err := client.ReadPrivate(o.credentials)
		if err != nil {
			return client.Response{}, err
		}
		var credentials struct {
			Name     string `json:"name"`
			Password string `json:"password"`
			Code     string `json:"code"`
		}
		if err = strictDecode(b, &credentials); err != nil {
			return client.Response{}, err
		}
		body["name"], body["password"], body["code"] = credentials.Name, credentials.Password, credentials.Code
	}
	if o.input != "" {
		b, err := input(o.input)
		if err != nil {
			return client.Response{}, err
		}
		var extra map[string]any
		if json.Unmarshal(b, &extra) != nil {
			return client.Response{}, usage("session options must be an object")
		}
		for k, v := range extra {
			switch k {
			case "scopes", "projects", "ttl_seconds", "purpose", "model":
				body[k] = v
			default:
				return client.Response{}, usage("unsupported session option")
			}
		}
	}
	r, err := c.Do(ctx, http.MethodPost, path, body, client.Options{})
	if err != nil {
		return r, err
	}
	var reply struct {
		Token string `json:"token"`
	}
	if err = r.Decode(&reply); err != nil {
		return r, err
	}
	if reply.Token == "" {
		return client.Response{}, errors.New("client: server did not issue a CLI session token")
	}
	if err = client.SaveState(o.session, sessionFile{Schema: "lantai.client-session/v1", Origin: c.Origin(), Token: reply.Token}); err != nil {
		return client.Response{}, err
	}
	return r, nil
}
func pull(ctx context.Context, c *client.Client, o options) (client.Response, error) {
	_, err := readPath(o)
	if err != nil {
		return client.Response{}, err
	}
	if o.directory == "" {
		return client.Response{}, usage("pull requires --directory")
	}
	return c.Pull(ctx, o.asset, o.version, o.directory, o.purpose)
}
func writeError(w io.Writer, err error) int {
	var api *client.APIError
	var body errcode.Body
	exit := 3
	if errors.Is(err, context.Canceled) {
		body = errcode.New(errcode.Internal, "client request canceled; saved transfer state can be resumed").Envelope("").Error
		exit = 130
	} else if errors.As(err, &api) {
		body = api.Body
		exit = domainExit(api.Status, body)
		if api.Protocol {
			exit = 3
		}
	} else if e, ok := errcode.As(err); ok {
		body = e.Envelope("").Error
		exit = domainExit(e.HTTPStatus(), body)
	} else {
		var u *usageError
		if errors.As(err, &u) {
			body = errcode.New(errcode.SchemaInvalid, u.message).Envelope("").Error
			exit = 2
		} else {
			body = errcode.New(errcode.Internal, err.Error()).Envelope("").Error
		}
	}
	_ = json.NewEncoder(w).Encode(errcode.Envelope{Error: body})
	return exit
}
func domainExit(status int, b errcode.Body) int {
	if status == 401 {
		return 5
	}
	if status == 409 || status == 412 {
		return 4
	}
	if b.Retryable || status == 429 {
		return 6
	}
	if status == 400 || status == 422 {
		return 2
	}
	return 1
}
