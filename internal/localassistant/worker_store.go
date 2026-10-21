package localassistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
)

const WorkerConfigSchema = "lantai.assistant-worker-config/v1"
const workerPrivateDirectory = ".lantai-assistant"

var workerULID = regexp.MustCompile(`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`)
var workerAlias = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type WorkerRoots struct {
	Directory   string `json:"directory"`
	State       string `json:"state"`
	Destination string `json:"destination"`
	Output      string `json:"output"`
}

// WorkerConfig is selected locally, never supplied by a web job. Exactly one
// project/workspace alias is linked per explicit worker process and short session.
type WorkerConfig struct {
	Schema           string      `json:"schema"`
	Origin           string      `json:"origin"`
	InstanceID       string      `json:"instance_id"`
	ProjectID        string      `json:"project_id"`
	Alias            string      `json:"alias"`
	WorkspaceAlias   string      `json:"workspace_alias"`
	Roots            WorkerRoots `json:"roots"`
	Actions          []string    `json:"actions"`
	SessionFile      string      `json:"session_file"`
	CAFile           string      `json:"ca_file,omitempty"`
	PollSeconds      int         `json:"poll_seconds,omitempty"`
	HeartbeatSeconds int         `json:"heartbeat_seconds,omitempty"`
}

type workerStore struct {
	config                        WorkerConfig
	configPath, workspace, digest string
	root                          *os.Root
	rootInfo                      os.FileInfo
}

func openWorker(configPath, workspace string) (*workerStore, error) {
	abs, e := filepath.Abs(workspace)
	if e != nil || workspace == "" {
		return nil, invalid("worker workspace required")
	}
	if e = noLinks(abs, false); e != nil {
		return nil, e
	}
	root, e := os.OpenRoot(abs)
	if e != nil {
		return nil, invalid("worker workspace cannot be opened")
	}
	s := &workerStore{workspace: abs, root: root}
	fail := func(err error) (*workerStore, error) { root.Close(); return nil, err }
	s.rootInfo, e = root.Stat(".")
	if e != nil {
		return fail(e)
	}
	if filepath.IsAbs(configPath) {
		if !inside(abs, configPath) {
			return fail(invalid("worker config escapes workspace"))
		}
		s.configPath = filepath.Clean(configPath)
	} else {
		s.configPath, e = s.path(configPath, false)
		if e != nil {
			return fail(e)
		}
	}
	if e = noLinks(s.configPath, false); e != nil {
		return fail(e)
	}
	raw, e := client.ReadPrivate(s.configPath)
	if e != nil || strict(raw, &s.config) != nil {
		return fail(invalid("worker config must be private, strict JSON"))
	}
	c := &s.config
	api, e := client.New(client.Config{BaseURL: c.Origin})
	if e != nil {
		return fail(invalid("worker requires strict HTTPS origin"))
	}
	origin := api.Origin()
	api.Close()
	if c.Schema != WorkerConfigSchema || origin != c.Origin || !workerAlias.MatchString(c.Alias) || !workerAlias.MatchString(c.WorkspaceAlias) || !workerULID.MatchString(c.ProjectID) || !workerULID.MatchString(c.InstanceID) || len(c.Actions) == 0 || len(c.Actions) > 8 {
		return fail(invalid("worker config identity, origin, alias or actions invalid"))
	}
	seen := map[string]bool{}
	for _, action := range c.Actions {
		if !allowed(action) || seen[action] {
			return fail(invalid("worker actions must be a unique existing whitelist"))
		}
		seen[action] = true
	}
	if c.PollSeconds == 0 {
		c.PollSeconds = 3
	}
	if c.HeartbeatSeconds == 0 {
		c.HeartbeatSeconds = 2
	}
	if c.PollSeconds < 1 || c.PollSeconds > 60 || c.HeartbeatSeconds < 1 || c.HeartbeatSeconds > 30 {
		return fail(invalid("worker timing must be bounded"))
	}
	var roots []string
	for _, rel := range []string{c.Roots.Directory, c.Roots.State, c.Roots.Destination, c.Roots.Output} {
		p, e := s.path(rel, true)
		if e != nil {
			return fail(e)
		}
		roots = append(roots, p)
	}
	private := filepath.Join(abs, workerPrivateDirectory)
	for i, p := range roots {
		if overlap(p, private) || overlap(p, filepath.Dir(s.configPath)) {
			return fail(invalid("worker business roots overlap private configuration"))
		}
		for _, other := range roots[i+1:] {
			if overlap(p, other) {
				return fail(invalid("worker roots must be independent"))
			}
		}
	}
	session, e := s.path(c.SessionFile, false)
	if e != nil {
		return fail(e)
	}
	var credential struct {
		Schema string `json:"schema"`
		Origin string `json:"origin"`
		Token  string `json:"token"`
	}
	secret, e := client.ReadPrivate(session)
	if e != nil || strict(secret, &credential) != nil || credential.Schema != "lantai.client-session/v1" || credential.Origin != c.Origin || credential.Token == "" {
		return fail(invalid("worker requires an explicit private same-origin session"))
	}
	for _, p := range roots {
		if overlap(p, filepath.Dir(session)) {
			return fail(invalid("worker roots overlap credentials"))
		}
	}
	if c.CAFile != "" {
		ca, e := s.path(c.CAFile, false)
		if e != nil {
			return fail(e)
		}
		for _, p := range roots {
			if overlap(p, ca) {
				return fail(invalid("worker roots overlap CA"))
			}
		}
	}
	canonical, e := canonjson.Canonicalize(raw)
	if e != nil {
		return fail(e)
	}
	s.digest = hash(append(append([]byte(abs), 0), canonical...))
	return s, nil
}
func (s *workerStore) close() { _ = s.root.Close() }
func (s *workerStore) path(rel string, missing bool) (string, error) {
	if rel == "" || client.SafeRelative(rel) != nil || rel == "." {
		return "", invalid("worker paths must be safe workspace-relative paths")
	}
	p := filepath.Join(s.workspace, filepath.FromSlash(rel))
	if !inside(s.workspace, p) {
		return "", invalid("worker path escapes workspace")
	}
	if e := noLinks(p, missing); e != nil {
		return "", e
	}
	return p, nil
}
func (s *workerStore) check() error {
	st, e := os.Lstat(s.workspace)
	if e != nil || !os.SameFile(st, s.rootInfo) {
		return invalid("worker workspace changed")
	}
	if e = noLinks(s.workspace, false); e != nil {
		return e
	}
	raw, e := client.ReadPrivate(s.configPath)
	if e != nil {
		return invalid("worker config changed")
	}
	canonical, e := canonjson.Canonicalize(raw)
	if e != nil || hash(append(append([]byte(s.workspace), 0), canonical...)) != s.digest {
		return invalid("worker config changed; stop and inspect before restart")
	}
	return nil
}
func (s *workerStore) privatePath(name string) (string, error) {
	if filepath.Base(name) != name || name == "" || strings.Contains(name, "\\") {
		return "", invalid("invalid worker private file name")
	}
	return s.path(workerPrivateDirectory+"/"+name, true)
}
func (s *workerStore) save(name string, value any) error {
	if e := s.check(); e != nil {
		return e
	}
	p, e := s.privatePath(name)
	if e != nil {
		return e
	}
	o, e := client.OpenPinnedPrivateOutput(p)
	if e != nil {
		return invalid("worker private state cannot be pinned")
	}
	defer o.Close()
	b, e := json.Marshal(value)
	if e != nil {
		return e
	}
	return o.Write(append(b, '\n'))
}
func (s *workerStore) load(name string, value any) error {
	p, e := s.privatePath(name)
	if e != nil {
		return e
	}
	b, e := client.ReadPrivate(p)
	if e != nil {
		return e
	}
	return strict(b, value)
}
func (s *workerStore) api() (*client.Client, error) {
	p, e := s.path(s.config.SessionFile, false)
	if e != nil {
		return nil, e
	}
	var cred struct {
		Schema string `json:"schema"`
		Origin string `json:"origin"`
		Token  string `json:"token"`
	}
	b, e := client.ReadPrivate(p)
	if e != nil || strict(b, &cred) != nil || cred.Origin != s.config.Origin || cred.Schema != "lantai.client-session/v1" || cred.Token == "" {
		return nil, invalid("worker session invalid")
	}
	ca := ""
	if s.config.CAFile != "" {
		ca, e = s.path(s.config.CAFile, false)
		if e != nil {
			return nil, e
		}
	}
	return client.New(client.Config{BaseURL: s.config.Origin, SessionToken: cred.Token, CAFile: ca})
}
func within(root, path string) bool {
	return filepath.Clean(root) == filepath.Clean(path) || inside(root, path)
}
func sameJSON(a, b []byte) bool {
	x, e := canonjson.Canonicalize(a)
	y, f := canonjson.Canonicalize(b)
	return e == nil && f == nil && bytes.Equal(x, y)
}

// ConfigureWorker creates one new private config atomically from an explicitly
// selected private input. It never overwrites an existing device configuration.
func ConfigureWorker(ctx context.Context, inputFile, configFile, workspace string) (WorkerConfig, error) {
	ctx, release, err := lockWorkspace(ctx, workspace)
	if err != nil {
		return WorkerConfig{}, err
	}
	_ = ctx
	defer release()
	s, err := openWorker(inputFile, workspace)
	if err != nil {
		return WorkerConfig{}, err
	}
	defer s.close()
	path, err := s.path(configFile, true)
	if err != nil {
		return WorkerConfig{}, err
	}
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return WorkerConfig{}, invalid("worker config exists; preserve its original binding")
	}
	for _, rel := range []string{s.config.Roots.Directory, s.config.Roots.State, s.config.Roots.Destination, s.config.Roots.Output} {
		root, e := s.path(rel, true)
		if e != nil || overlap(root, filepath.Dir(path)) {
			return WorkerConfig{}, invalid("config must stay outside business roots")
		}
	}
	out, err := client.OpenPinnedPrivateOutput(path)
	if err != nil {
		return WorkerConfig{}, err
	}
	defer out.Close()
	// Preserve defaults and all selected capability restrictions, without ever
	// serializing the session token (the config contains only its local path).
	b, err := json.Marshal(s.config)
	if err == nil {
		err = out.Write(append(b, '\n'))
	}
	return s.config, err
}
