package httpapi

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/httpauth"
	"github.com/oujinhaoai/lantai/internal/query"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/schemas"
)

func (h *Handler) routes(m *http.ServeMux) {
	h.identityRoutes(m)
	h.executionRoutes(m)
	h.collaborationRoutes(m)
	h.extensionRoutes(m)
	h.route(m, "GET /api/v1/meta", true, h.meta)
	h.route(m, "POST /api/v1/sessions/exchange", true, h.exchange)
	h.route(m, "POST /api/v1/sessions/login", true, h.login)
	h.route(m, "GET /api/v1/whoami", false, h.whoami)
	h.route(m, "DELETE /api/v1/sessions/current", false, h.logout)
	h.route(m, "GET /api/v1/projects", false, h.projects)
	h.route(m, "POST /api/v1/projects", false, h.createProject)
	h.route(m, "GET /api/v1/projects/{key}", false, h.project)
	h.route(m, "GET /api/v1/types", false, h.types)
	h.route(m, "GET /api/v1/types/{type}", false, h.assetType)
	h.route(m, "POST /api/v1/uploads", false, h.createUpload)
	h.route(m, "GET /api/v1/uploads/{upload_id}", false, h.upload)
	h.route(m, "DELETE /api/v1/uploads/{upload_id}", false, h.cancelUpload)
	h.route(m, "POST /api/v1/uploads/{upload_id}/files/{sha256}/complete", false, h.completeFile)
	h.route(m, "POST /api/v1/uploads/{upload_id}/check", false, h.checkBlobs)
	h.route(m, "POST /api/v1/uploads/{upload_id}/commit", false, h.commit)
	h.route(m, "GET /api/v1/assets", false, h.search)
	h.route(m, "GET /api/v1/assets/{asset_id}", false, h.asset)
	h.route(m, "GET /api/v1/assets/{asset_id}/versions/{version_id}", false, h.version)
	h.route(m, "POST /api/v1/assets/{asset_id}/versions/{version_id}/read-grants", false, h.readGrant)
	h.route(m, "PATCH /api/v1/assets/{asset_id}/metadata", false, h.patchAsset)
	h.route(m, "GET /api/v1/operations/{operation_id}", false, h.operation)
}

func (h *Handler) meta(w http.ResponseWriter, r *http.Request, _ authz.Context) error {
	stage := "M1"
	capabilities := []string{"sessions", "projects", "asset_types", "uploads", "commit_version", "exact_read", "read_grants", "conditional_metadata", "search", "operation_status"}
	unsupported := []string{"managed_runner", "third_party_web_ui", "resident_extension_services", "automatic_triggers", "federation"}
	if h.deps.Tasks != nil {
		stage = "M2"
		capabilities = append(capabilities, "tasks")
	}
	if h.deps.Flows != nil {
		capabilities = append(capabilities, "manual_flows")
	}
	if h.deps.Execution != nil {
		capabilities = append(capabilities, "manual_cli_execution")
	}
	if h.deps.Jobs != nil {
		capabilities = append(capabilities, "official_check_jobs")
	}
	if h.deps.Evidence != nil {
		capabilities = append(capabilities, "review_evidence")
	}
	if h.deps.Reviews != nil {
		capabilities = append(capabilities, "human_review", "effective_context")
	}
	if h.deps.Discussions != nil {
		capabilities = append(capabilities, "discussions")
	}
	if h.deps.Collaboration != nil {
		capabilities = append(capabilities, "filtered_events", "inbox")
	}
	if h.deps.Lifecycle != nil {
		capabilities = append(capabilities, "lifecycle")
	}
	if h.deps.Extensions != nil {
		capabilities = append(capabilities, "extension_governance", "extension_cli_commands")
	}
	return respond(w, 200, map[string]any{"api_version": "v1", "instance_id": h.cfg.InstanceID, "stage": stage, "capabilities": capabilities, "views": []string{"brief", "full"}, "max_json_bytes": h.cfg.MaxJSONBytes, "max_page_size": 100, "transfer": map[string]any{"resumable_parts": true, "range": true, "class_source": "authenticated_session"}, "unsupported": unsupported})
}

func (h *Handler) sessionInput(w http.ResponseWriter, r *http.Request) (SessionRequest, error) {
	var in SessionRequest
	if err := h.decode(w, r, &in); err != nil {
		return in, err
	}
	if in.Channel == "" {
		in.Channel = identity.ChannelCLI
	}
	if in.TTLSeconds < 0 || in.TTLSeconds > 86400 {
		return in, invalid("ttl_seconds is outside the accepted range")
	}
	// These endpoints never combine caller credentials with submitted credentials.
	if r.Header.Get("Authorization") != "" {
		return in, invalid("session creation takes credentials in the JSON body")
	}
	return in, nil
}
func (h *Handler) exchange(w http.ResponseWriter, r *http.Request, _ authz.Context) error {
	in, err := h.sessionInput(w, r)
	if err != nil {
		return err
	}
	if in.Token == "" || in.Name != "" || in.Password != "" || in.Code != "" {
		return invalid("exchange requires only token credentials")
	}
	out, err := h.deps.Identity.ExchangeToken(r.Context(), in.Token, identity.SessionRequest{Scopes: in.Scopes, Projects: in.Projects, TTL: time.Duration(in.TTLSeconds) * time.Second, Channel: in.Channel, Purpose: in.Purpose, Model: in.Model})
	if err != nil {
		return err
	}
	return respond(w, 201, SessionResponse{Token: out.Token, Session: out.Session})
}
func (h *Handler) login(w http.ResponseWriter, r *http.Request, _ authz.Context) error {
	in, err := h.sessionInput(w, r)
	if err != nil {
		return err
	}
	if in.Token != "" || in.Purpose != "" || in.Model != "" || in.Name == "" || in.Password == "" || in.Code == "" {
		return invalid("login requires name, password and authenticator code")
	}
	if in.Channel == identity.ChannelBrowser {
		if !h.loginOrigin(r) {
			return errcode.New(errcode.Forbidden, "browser login requires an allowed exact Origin").WithDetails(errcode.Detail{Reason: "origin_not_allowed"})
		}
	}
	source := r.RemoteAddr
	if host, _, err := net.SplitHostPort(source); err == nil {
		source = host
	}
	out, err := h.deps.Identity.Login(r.Context(), identity.LoginRequest{Name: in.Name, Password: in.Password, Code: in.Code, Source: source, Channel: in.Channel, Scopes: in.Scopes, Projects: in.Projects, TTL: time.Duration(in.TTLSeconds) * time.Second})
	if err != nil {
		return err
	}
	res := SessionResponse{Token: out.Token, CSRFToken: out.CSRFToken, Session: out.Session}
	if in.Channel == identity.ChannelBrowser {
		http.SetCookie(w, httpauth.SessionCookie(out.Token, out.Session.ExpiresAt))
		res.Token = ""
	}
	return respond(w, 201, res)
}
func (h *Handler) loginOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" || len(r.Header.Values("Origin")) != 1 {
		return false
	}
	u, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Host == "" {
		return false
	}
	for _, allowed := range h.cfg.AllowedOrigins {
		if strings.EqualFold(strings.TrimSuffix(allowed, "/"), u.Scheme+"://"+u.Host) {
			return true
		}
	}
	return false
}
func (h *Handler) whoami(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	v, err := h.deps.Identity.WhoAmI(r.Context(), who)
	if err != nil {
		return err
	}
	return respond(w, 200, v)
}
func (h *Handler) logout(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	if err := h.deps.Identity.EndSession(r.Context(), who, who.SessionID); err != nil {
		return err
	}
	if _, err := r.Cookie(httpauth.CookieName); err == nil {
		http.SetCookie(w, httpauth.ClearCookie())
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) createProject(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	k, err := key(r)
	if err != nil {
		return err
	}
	var in CreateProjectRequest
	if err = h.decode(w, r, &in); err != nil {
		return err
	}
	p, err := h.deps.Catalog.CreateProject(r.Context(), catalog.ProjectRequest{Who: who, IdempotencyKey: k, Key: in.Key, Name: in.Name, ProjectType: in.ProjectType, Summary: in.Summary})
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/v1/projects/"+url.PathEscape(p.Key))
	etag(w, p.Description.Revision)
	return respond(w, http.StatusCreated, projectView(p, true))
}
func (h *Handler) projects(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	full, err := view(r)
	if err != nil {
		return err
	}
	n, err := limit(r)
	if err != nil {
		return err
	}
	p, err := h.deps.Catalog.ListProjects(r.Context(), who, ids.ID(r.URL.Query().Get("cursor")), n)
	if err != nil {
		return err
	}
	out := struct {
		Items      []ProjectView `json:"items"`
		NextCursor string        `json:"next_cursor,omitempty"`
	}{Items: make([]ProjectView, 0, len(p.Items)), NextCursor: p.NextCursor}
	for _, v := range p.Items {
		out.Items = append(out.Items, projectView(v, full))
	}
	return respond(w, 200, out)
}
func (h *Handler) project(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	full, err := view(r)
	if err != nil {
		return err
	}
	p, err := h.deps.Catalog.GetProject(r.Context(), who, r.PathValue("key"))
	if err != nil {
		return err
	}
	etag(w, p.Description.Revision)
	return respond(w, 200, projectView(p, full))
}
func (h *Handler) types(w http.ResponseWriter, _ *http.Request, _ authz.Context) error {
	items := make([]map[string]any, 0, len(manifest.Types))
	for _, t := range manifest.Types {
		items = append(items, map[string]any{"asset_type": t, "type_schema": manifest.TypesContract, "metadata_fields": manifest.KnownMetadata(t)})
	}
	return respond(w, 200, map[string]any{"items": items, "file_roles": manifest.Roles, "relations": manifest.Relations, "project_types": catalog.ProjectTypes})
}
func (h *Handler) assetType(w http.ResponseWriter, r *http.Request, _ authz.Context) error {
	t := manifest.AssetType(r.PathValue("type"))
	if !t.Valid() {
		return errcode.New(errcode.NotFound, "")
	}
	// Return the complete schema library so its local $refs remain resolvable.
	raw, err := schemas.FS.ReadFile("catalog/v1/asset-types.schema.json")
	if err != nil {
		return err
	}
	return respond(w, 200, map[string]any{"asset_type": t, "type_schema": manifest.TypesContract, "schema_pointer": "#/$defs/" + string(t), "schema": json.RawMessage(raw)})
}

func (h *Handler) createUpload(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	k, err := key(r)
	if err != nil {
		return err
	}
	var in CreateUploadRequest
	if err = h.decode(w, r, &in); err != nil {
		return err
	}
	u, err := h.deps.Storage.CreateUpload(r.Context(), storage.CreateUploadRequest{Who: who, IdempotencyKey: k, ProjectID: in.ProjectID, Files: in.Files})
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/v1/uploads/"+string(u.UploadID))
	return respond(w, 201, uploadView(u))
}
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	id, err := pathID(r, "upload_id")
	if err != nil {
		return err
	}
	u, err := h.deps.Storage.GetUpload(r.Context(), who, id)
	if err != nil {
		return err
	}
	return respond(w, 200, uploadView(u))
}
func (h *Handler) cancelUpload(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	id, err := pathID(r, "upload_id")
	if err != nil {
		return err
	}
	if err = h.deps.Catalog.CancelVersion(r.Context(), who, id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
func (h *Handler) completeFile(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	id, err := pathID(r, "upload_id")
	if err != nil {
		return err
	}
	sha := r.PathValue("sha256")
	if !digest.ValidHex(sha) {
		return invalid("invalid content SHA-256")
	}
	var in struct{}
	if err = h.decode(w, r, &in); err != nil {
		return err
	}
	v, err := h.deps.Storage.CompleteFile(r.Context(), who, id, sha)
	if err != nil {
		return err
	}
	return respond(w, 200, v)
}
func (h *Handler) checkBlobs(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	id, err := pathID(r, "upload_id")
	if err != nil {
		return err
	}
	var in struct {
		Files []storage.FileSpec `json:"files"`
	}
	if err = h.decode(w, r, &in); err != nil {
		return err
	}
	v, err := h.deps.Storage.CheckBlobs(r.Context(), who, id, in.Files)
	if err != nil {
		return err
	}
	return respond(w, 200, map[string]any{"items": v})
}
func (h *Handler) commit(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	id, err := pathID(r, "upload_id")
	if err != nil {
		return err
	}
	k, err := key(r)
	if err != nil {
		return err
	}
	var in CommitRequest
	if err = h.decode(w, r, &in); err != nil {
		return err
	}
	v, err := h.deps.Catalog.CommitVersion(r.Context(), catalog.VersionRequest{Who: who, IdempotencyKey: k, UploadID: id, AssetID: in.AssetID, Slug: in.Slug, BaseVersionID: in.BaseVersionID, Content: in.Content, Describe: in.Describe, Task: in.Task})
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/v1/assets/"+string(v.AssetID)+"/versions/"+string(v.VersionID))
	return respond(w, 201, v)
}

func (h *Handler) asset(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	id, err := pathID(r, "asset_id")
	if err != nil {
		return err
	}
	full, err := view(r)
	if err != nil {
		return err
	}
	v, err := h.deps.Catalog.GetAsset(r.Context(), who, id)
	if err != nil {
		return err
	}
	if !full {
		v.Description.Extra = nil
	}
	etag(w, v.Description.Revision)
	return respond(w, 200, AssetView{AssetID: v.Asset.AssetID, ProjectID: v.Asset.ProjectID, AssetType: v.Description.AssetType, Description: v.Description, Latest: versionView(v.Latest, h.cfg.InstanceID)})
}
func (h *Handler) version(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	a, err := pathID(r, "asset_id")
	if err != nil {
		return err
	}
	id, err := pathID(r, "version_id")
	if err != nil {
		return err
	}
	full, err := view(r)
	if err != nil {
		return err
	}
	v, err := h.deps.Catalog.GetVersion(r.Context(), who, a, id)
	if err != nil {
		return err
	}
	out := ExactVersionView{Version: versionView(v.Version, h.cfg.InstanceID)}
	if full {
		out.Manifest = &ManifestView{Contract: manifest.Contract, Document: v.Manifest}
	}
	return respond(w, 200, out)
}
func (h *Handler) readGrant(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	a, err := pathID(r, "asset_id")
	if err != nil {
		return err
	}
	id, err := pathID(r, "version_id")
	if err != nil {
		return err
	}
	var in ReadGrantRequest
	if err = h.decode(w, r, &in); err != nil {
		return err
	}
	v, err := h.deps.Storage.IssueReadGrant(r.Context(), storage.ReadRequest{Who: who, AssetID: a, VersionID: id, Path: in.Path, Purpose: in.Purpose})
	if err != nil {
		return err
	}
	return respond(w, 201, v)
}
func (h *Handler) patchAsset(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	a, err := pathID(r, "asset_id")
	if err != nil {
		return err
	}
	k, err := key(r)
	if err != nil {
		return err
	}
	rev, err := revision(r)
	if err != nil {
		return err
	}
	var in catalog.AssetPatch
	if err = h.decode(w, r, &in); err != nil {
		return err
	}
	v, err := h.deps.Catalog.PatchAsset(r.Context(), who, k, a, rev, in)
	if err != nil {
		return err
	}
	etag(w, v.Revision)
	return respond(w, 200, v)
}
func (h *Handler) search(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	if _, err := view(r); err != nil {
		return err
	}
	n, err := limit(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	v, err := h.deps.Query.Search(r.Context(), query.SearchRequest{Who: who, Limit: n, Cursor: q.Get("cursor"), Filter: query.Filter{ProjectID: ids.ID(q.Get("project_id")), Text: q.Get("q"), AssetType: manifest.AssetType(q.Get("asset_type"))}})
	if err != nil {
		return err
	}
	return respond(w, 200, v)
}
func (h *Handler) operation(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	id, err := pathID(r, "operation_id")
	if err != nil {
		return err
	}
	v, err := h.deps.Operations.Operation(r.Context(), who, id)
	if err != nil {
		return err
	}
	return respond(w, 200, v)
}
