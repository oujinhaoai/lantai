package httpapi

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/httpauth"
)

// SensitiveRequest transports the core's closed command set. The server builds
// the challenge summary; clients must display it before requesting a human code.
type SensitiveRequest struct {
	Action      authz.Action    `json:"action"`
	Command     json.RawMessage `json:"command"`
	OperationID ids.ID          `json:"operation_id,omitempty"`
	GrantID     ids.ID          `json:"grant_id,omitempty"`
}

func (h *Handler) identityRoutes(m *http.ServeMux) {
	h.route(m, "POST /api/v1/sessions/setup", true, h.setup)
	h.route(m, "POST /api/v1/identity/challenges", false, h.challenge)
	h.route(m, "POST /api/v1/identity/challenges/{challenge_id}/verify", false, h.verifyChallenge)
	h.route(m, "POST /api/v1/identity/commands", false, h.executeIdentity)
	h.route(m, "GET /api/v1/identity/principals/{principal_id}", false, h.principal)
	h.route(m, "GET /api/v1/identity/projects/{project_id}/members", false, h.members)
	h.route(m, "POST /api/v1/identity/factor/enroll", false, h.enroll)
	h.route(m, "PUT /api/v1/identity/password", false, h.password)
	h.route(m, "POST /api/v1/identity/factor/confirm", false, h.confirm)
}

func parseSensitive(in SensitiveRequest) (identity.Command, error) {
	var command identity.Command
	switch in.Action {
	case identity.ActRegisterPrincipal:
		command = &identity.RegisterPrincipal{}
	case identity.ActIssueCredential:
		command = &identity.IssueCredential{}
	case identity.ActRevokeCredential:
		command = &identity.RevokeCredential{}
	case identity.ActGrantProjectRole, identity.ActRevokeProjectRole:
		command = &identity.SetProjectRole{}
	default:
		return nil, invalid("identity action is not exposed by this API version")
	}
	if len(in.Command) == 0 || in.Command[0] != '{' {
		return nil, invalid("command must be a JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(in.Command))
	d.DisallowUnknownFields()
	if err := d.Decode(command); err != nil {
		return nil, invalid("command fields do not match the selected action")
	}
	if c, ok := command.(*identity.SetProjectRole); ok && c.Grant != (in.Action == identity.ActGrantProjectRole) {
		return nil, invalid("role grant flag must match the selected action")
	}
	return command, nil
}
func source(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func (h *Handler) challenge(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	var in SensitiveRequest
	if err := h.decode(w, r, &in); err != nil {
		return err
	}
	if in.GrantID != "" {
		return invalid("a challenge request does not take a grant")
	}
	c, err := parseSensitive(in)
	if err != nil {
		return err
	}
	var out identity.Challenge
	if in.OperationID != "" {
		if !in.OperationID.Valid() {
			return invalid("invalid operation_id")
		}
		out, err = h.deps.Identity.Rechallenge(r.Context(), who, in.OperationID, c)
	} else {
		out, err = h.deps.Identity.CreateChallenge(r.Context(), who, c)
	}
	if err != nil {
		return err
	}
	return respond(w, 201, out)
}
func (h *Handler) verifyChallenge(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	id, err := pathID(r, "challenge_id")
	if err != nil {
		return err
	}
	var in struct {
		Code string `json:"code"`
	}
	if err = h.decode(w, r, &in); err != nil {
		return err
	}
	out, err := h.deps.Identity.VerifyChallenge(r.Context(), who, id, in.Code, source(r))
	if err != nil {
		return err
	}
	return respond(w, 201, out)
}
func (h *Handler) executeIdentity(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	k, err := key(r)
	if err != nil {
		return err
	}
	var in SensitiveRequest
	if err = h.decode(w, r, &in); err != nil {
		return err
	}
	if !in.GrantID.Valid() || in.OperationID != "" {
		return invalid("execution requires grant_id; operation is fixed by the grant")
	}
	c, err := parseSensitive(in)
	if err != nil {
		return err
	}
	out, err := h.deps.Identity.Execute(r.Context(), who, in.GrantID, k, c)
	if err != nil {
		return err
	}
	// The core deliberately excludes one-time secrets from its persisted Result.
	// They are delivered only in this direct response, never operation polling.
	return respond(w, 200, struct {
		identity.Result
		Secret string `json:"secret,omitempty"`
	}{Result: out, Secret: out.Secret})
}
func (h *Handler) principal(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	id, err := pathID(r, "principal_id")
	if err != nil {
		return err
	}
	out, err := h.deps.Identity.GetPrincipal(r.Context(), who, id)
	if err != nil {
		return err
	}
	etag(w, out.Revision)
	return respond(w, 200, out)
}
func (h *Handler) members(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	id, err := pathID(r, "project_id")
	if err != nil {
		return err
	}
	out, rev, err := h.deps.Identity.Members(r.Context(), who, id)
	if err != nil {
		return err
	}
	if out == nil {
		out = []identity.Member{}
	}
	etag(w, rev)
	return respond(w, 200, map[string]any{"items": out, "revision": rev})
}
func (h *Handler) setup(w http.ResponseWriter, r *http.Request, _ authz.Context) error {
	var in struct {
		Name    string `json:"name"`
		Code    string `json:"code"`
		Channel string `json:"channel,omitempty"`
	}
	if err := h.decode(w, r, &in); err != nil {
		return err
	}
	if in.Channel == "" {
		in.Channel = identity.ChannelCLI
	}
	if in.Channel == identity.ChannelBrowser && !h.loginOrigin(r) {
		return invalid("browser setup requires an allowed exact Origin")
	}
	out, err := h.deps.Identity.StartSetup(r.Context(), identity.SetupRequest{Name: in.Name, Code: in.Code, Source: source(r), Channel: in.Channel})
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
func (h *Handler) enroll(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	var in struct{}
	if err := h.decode(w, r, &in); err != nil {
		return err
	}
	out, err := h.deps.Identity.EnrollFactor(r.Context(), who)
	if err != nil {
		return err
	}
	return respond(w, 201, map[string]any{"secret": out.Secret, "uri": out.URI})
}
func (h *Handler) password(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	var in struct {
		Password string `json:"password"`
	}
	if err := h.decode(w, r, &in); err != nil {
		return err
	}
	if err := h.deps.Identity.SetPassword(r.Context(), who, in.Password); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
func (h *Handler) confirm(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	var in struct {
		Code string `json:"code"`
	}
	if err := h.decode(w, r, &in); err != nil {
		return err
	}
	out, err := h.deps.Identity.ConfirmFactor(r.Context(), who, in.Code)
	if err != nil {
		return err
	}
	return respond(w, 200, map[string]any{"recovery_codes": out})
}
