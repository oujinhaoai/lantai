package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// RecoveryControl is a separate owner-claimed installation client. It has no
// normal session, no configurable endpoint and no administrator-factor API.
// The server retains ownership of every target, phase and acceptance check.
type RecoveryControl struct {
	c     *Client
	claim string
}
type RecoveryRestoreInput struct {
	PackageSHA256    string `json:"package_sha256"`
	ConfirmNewTarget bool   `json:"confirm_new_target"`
}
type RecoveryCompleteInput struct {
	ReviewID             string `json:"review_id"`
	KnownChangesReviewed bool   `json:"known_changes_reviewed"`
	SourceOffline        bool   `json:"source_offline"`
	Activate             bool   `json:"activate"`
	AcceptRecoveryPoint  bool   `json:"accept_recovery_point"`
}

func NewRecoveryControl(config Config, claim string) (*RecoveryControl, error) {
	raw, err := base64.RawURLEncoding.DecodeString(claim)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != claim || config.SessionToken != "" {
		return nil, errors.New("client: an explicit installation claim is required; ordinary sessions cannot claim a target")
	}
	c, err := New(config)
	if err != nil {
		return nil, err
	}
	return &RecoveryControl{c: c, claim: claim}, nil
}
func (r *RecoveryControl) Close() { r.c.Close() }
func (r *RecoveryControl) Status(ctx context.Context) (Response, error) {
	return r.request(ctx, http.MethodGet, "status", nil, 0, "")
}
func (r *RecoveryControl) Cancel(ctx context.Context) (Response, error) {
	return r.request(ctx, http.MethodPost, "cancel", nil, 0, "")
}
func (r *RecoveryControl) Check(ctx context.Context) (Response, error) {
	return r.request(ctx, http.MethodPost, "review", bytes.NewReader([]byte("{}")), 2, "application/json")
}
func (r *RecoveryControl) Restore(ctx context.Context, in RecoveryRestoreInput) (Response, error) {
	if !digest.ValidHex(in.PackageSHA256) || !in.ConfirmNewTarget {
		return Response{}, errors.New("client: exact package digest and explicit new-target confirmation required")
	}
	return r.json(ctx, "restore", in)
}
func (r *RecoveryControl) Complete(ctx context.Context, in RecoveryCompleteInput) (Response, error) {
	if !ids.ID(in.ReviewID).Valid() || !in.KnownChangesReviewed || !in.SourceOffline || !in.Activate || !in.AcceptRecoveryPoint {
		return Response{}, errors.New("client: current core review and every explicit cutover confirmation required")
	}
	return r.json(ctx, "complete", in)
}
func (r *RecoveryControl) json(ctx context.Context, action string, in any) (Response, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return Response{}, err
	}
	return r.request(ctx, http.MethodPost, action, bytes.NewReader(raw), int64(len(raw)), "application/json")
}

// Upload streams only the caller's exact private package. Hash it before the
// request, keep the same open file, and require the server's verified digest.
// No claim or recovery material is created, printed, or retained by the client.
func (r *RecoveryControl) Upload(ctx context.Context, path, expectedSHA string) (Response, error) {
	if !digest.ValidHex(expectedSHA) {
		return Response{}, errors.New("client: reviewed package digest required")
	}
	st, err := os.Lstat(path)
	if err != nil {
		return Response{}, err
	}
	if !st.Mode().IsRegular() || st.Size() <= 0 {
		return Response{}, errors.New("client: selected package must be a nonempty regular private file")
	}
	if err := checkPrivate(path, st); err != nil {
		return Response{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Response{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return Response{}, errors.New("client: package changed while opening")
	}
	hash, size, err := hashFile(ctx, f)
	if err != nil {
		return Response{}, err
	}
	if size != st.Size() || hash != expectedSHA {
		return Response{}, errcode.New(errcode.HashMismatch, "selected package differs from reviewed digest")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Response{}, err
	}
	response, err := r.request(ctx, http.MethodPost, "upload", f, size, "application/octet-stream")
	if err != nil {
		return response, err
	}
	status, err := r.Status(ctx)
	if err != nil {
		return Response{}, err
	}
	var value struct {
		Package struct {
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		} `json:"package"`
	}
	if err := status.Decode(&value); err != nil {
		return Response{}, err
	}
	if value.Package.SHA256 != expectedSHA || value.Package.Size != size {
		return Response{}, errcode.New(errcode.HashMismatch, "target verified a different package")
	}
	return status, nil
}
func (r *RecoveryControl) request(ctx context.Context, method, action string, body io.Reader, size int64, contentType string) (Response, error) {
	path := "/setup/recovery/" + action
	// All callers use the fixed actions above. Do not accept a user-selected URL.
	u := r.c.base.ResolveReference(&url.URL{Path: path})
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return Response{}, errors.New("client: invalid recovery request")
	}
	req.GetBody = nil
	req.ContentLength = size
	req.Header.Set("Origin", r.c.Origin())
	req.Header.Set("Authorization", "Bearer "+r.claim)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := r.c.recovery.Do(req)
	if err != nil {
		return Response{}, safeNetworkError(ctx, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		return Response{}, safeNetworkError(ctx, err)
	}
	if len(data) > 1<<20 {
		return Response{}, errors.New("client: recovery response exceeds budget")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		code := errcode.PreconditionFailed
		if res.StatusCode == 401 || res.StatusCode == 403 {
			code = errcode.Forbidden
		}
		// Setup has text errors. Do not echo a response that may contain a claim,
		// package data or future factor material; reconcile status explicitly.
		return Response{}, &APIError{Status: res.StatusCode, Body: errcode.Body{Code: code, Message: "recovery request rejected; inspect the original target status before retrying"}}
	}
	if !json.Valid(data) || bytes.Contains(data, []byte(r.claim)) {
		return Response{}, errors.New("client: invalid recovery response")
	}
	return Response{Status: res.StatusCode, Body: data}, nil
}

// HashRecoveryPackage streams a private package for an explicit local review.
func HashRecoveryPackage(ctx context.Context, path string) (string, int64, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, errors.New("client: private regular package required")
	}
	if err := checkPrivate(path, st); err != nil {
		return "", 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	pinned, err := f.Stat()
	if err != nil || !os.SameFile(st, pinned) {
		return "", 0, errors.New("client: package replaced")
	}
	return hashFile(ctx, f)
}
