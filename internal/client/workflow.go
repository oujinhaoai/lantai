package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// PushInput is a local command document, not a second server manifest format.
// Fields other than project_id are passed unchanged to the commit endpoint;
// only files[].sha256/size are computed from the local working copy.
type PushInput struct {
	ProjectID     string          `json:"project_id"`
	AssetID       string          `json:"asset_id,omitempty"`
	Slug          string          `json:"slug,omitempty"`
	BaseVersionID string          `json:"base_version_id,omitempty"`
	Content       ContentInput    `json:"content"`
	Describe      json.RawMessage `json:"describe,omitempty"`
}
type ContentInput struct {
	AssetType   string          `json:"asset_type"`
	VersionNote string          `json:"version_note,omitempty"`
	Files       []InputFile     `json:"files"`
	Uses        json.RawMessage `json:"uses,omitempty"`
	Rights      json.RawMessage `json:"rights,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
}
type InputFile struct {
	Path   string `json:"path"`
	Role   string `json:"role"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type UploadFile struct {
	SHA256          string `json:"sha256"`
	Size            int64  `json:"size"`
	PartSize        int64  `json:"part_size"`
	PartCount       int    `json:"part_count"`
	State           string `json:"state"`
	ReceivedParts   []int  `json:"received_parts"`
	PartURLTemplate string `json:"part_url_template"`
}
type Upload struct {
	UploadID    string       `json:"upload_id"`
	OperationID string       `json:"operation_id"`
	State       string       `json:"state"`
	Files       []UploadFile `json:"files"`
}
type PushState struct {
	Schema      string          `json:"schema"`
	Origin      string          `json:"origin"`
	RequestHash string          `json:"request_hash"`
	CreateKey   string          `json:"create_key"`
	CommitKey   string          `json:"commit_key"`
	UploadID    string          `json:"upload_id,omitempty"`
	OperationID string          `json:"operation_id,omitempty"`
	Committing  bool            `json:"committing,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// PreparePush hashes local files using bounded memory. Paths are always rooted in
// a user-selected working directory, never the server data directory.
func PreparePush(ctx context.Context, in PushInput, directory string) (PushInput, error) {
	if in.ProjectID == "" || len(in.Content.Files) == 0 {
		return in, errcode.New(errcode.SchemaInvalid, "project_id and content.files are required")
	}
	paths := make([]string, len(in.Content.Files))
	for i, f := range in.Content.Files {
		paths[i] = f.Path
	}
	if err := ValidatePaths(paths); err != nil {
		return in, err
	}
	in.Content.Files = append([]InputFile(nil), in.Content.Files...)
	root, err := os.OpenRoot(directory)
	if err != nil {
		return in, err
	}
	defer root.Close()
	for i := range in.Content.Files {
		f := &in.Content.Files[i]
		file, err := root.Open(filepath.FromSlash(f.Path))
		if err != nil {
			return in, err
		}
		sha, size, err := hashFile(ctx, file)
		file.Close()
		if err != nil {
			return in, err
		}
		if f.SHA256 != "" && (sha != f.SHA256 || size != f.Size) {
			return in, errcode.New(errcode.HashMismatch, "local file does not match declared size or digest")
		}
		f.SHA256, f.Size = sha, size
	}
	return in, nil
}

func commitBody(in PushInput) (map[string]json.RawMessage, error) {
	b, e := json.Marshal(in)
	if e != nil {
		return nil, e
	}
	var out map[string]json.RawMessage
	e = json.Unmarshal(b, &out)
	delete(out, "project_id")
	return out, e
}

// Push persists both keys before the first request. A failed call can be resumed
// by passing the same state file; it never chooses a replacement key implicitly.
// uploadOnly leaves commit for a later push (using the same input and state).
func (c *Client) Push(ctx context.Context, in PushInput, directory, statePath string, uploadOnly bool) (Response, error) {
	if statePath == "" {
		return Response{}, errors.New("client: a persistent state file is required")
	}
	unlock, err := lockState(statePath)
	if err != nil {
		return Response{}, err
	}
	defer unlock()
	in, err = PreparePush(ctx, in, directory)
	if err != nil {
		return Response{}, err
	}
	raw, _ := json.Marshal(in)
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	var state PushState
	err = LoadState(statePath, &state)
	if errors.Is(err, os.ErrNotExist) {
		state = PushState{Schema: "lantai.client-push/v1", Origin: c.Origin(), RequestHash: hash, CreateKey: string(ids.New()), CommitKey: string(ids.New())}
		if err = SaveState(statePath, state); err != nil {
			return Response{}, err
		}
	} else if err != nil {
		return Response{}, err
	}
	if state.Schema != "lantai.client-push/v1" || state.Origin != c.Origin() || state.RequestHash != hash || state.CreateKey == "" || state.CommitKey == "" {
		return Response{}, errors.New("client: state belongs to a different server or request; use its original input")
	}
	body, err := commitBody(in)
	if err != nil {
		return Response{}, err
	}
	// Reconcile the stable operation before resending a commit whose response was
	// lost. The authoritative idempotent command still supplies the exact receipt.
	if state.Committing && !uploadOnly {
		if state.OperationID != "" {
			if _, err = c.Do(ctx, http.MethodGet, "/api/v1/operations/"+url.PathEscape(state.OperationID), nil, Options{}); err != nil {
				return Response{}, err
			}
		}
		return c.finishPush(ctx, body, statePath, &state)
	}
	var upload Upload
	if state.UploadID == "" {
		type spec struct {
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		}
		files := make([]spec, 0, len(in.Content.Files))
		seen := map[string]bool{}
		for _, f := range in.Content.Files {
			if !seen[f.SHA256] {
				files = append(files, spec{f.SHA256, f.Size})
				seen[f.SHA256] = true
			}
		}
		r, err := c.Do(ctx, http.MethodPost, "/api/v1/uploads", map[string]any{"project_id": in.ProjectID, "files": files}, Options{IdempotencyKey: state.CreateKey})
		if err != nil {
			return r, err
		}
		if err = r.Decode(&upload); err != nil {
			return r, err
		}
		if upload.UploadID == "" || upload.OperationID == "" {
			return r, errors.New("client: server omitted upload identity")
		}
		state.UploadID, state.OperationID = upload.UploadID, upload.OperationID
		if err = SaveState(statePath, state); err != nil {
			return Response{}, err
		}
	} else {
		r, err := c.Do(ctx, http.MethodGet, "/api/v1/uploads/"+url.PathEscape(state.UploadID), nil, Options{})
		if err != nil {
			return r, err
		}
		if err = r.Decode(&upload); err != nil {
			return r, err
		}
	}
	local := map[string]InputFile{}
	for _, f := range in.Content.Files {
		local[f.SHA256] = f
	}
	if upload.UploadID != state.UploadID || upload.OperationID != state.OperationID || len(upload.Files) != len(local) {
		return Response{}, errors.New("client: upload no longer matches the saved request")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return Response{}, err
	}
	defer root.Close()
	seen := map[string]bool{}
	for _, f := range upload.Files {
		input, ok := local[f.SHA256]
		if !ok || seen[f.SHA256] || f.Size != input.Size {
			return Response{}, errors.New("client: server upload file mismatch")
		}
		seen[f.SHA256] = true
		if f.State == "verified" {
			continue
		}
		if f.PartSize < 1 || f.PartCount < 1 || int64(f.PartCount) != (max(f.Size, 1)-1)/f.PartSize+1 {
			return Response{}, errors.New("client: invalid server part geometry")
		}
		received := map[int]bool{}
		for _, p := range f.ReceivedParts {
			if p < 1 || p > f.PartCount {
				return Response{}, errors.New("client: invalid received part number")
			}
			received[p] = true
		}
		for p := 1; p <= f.PartCount; p++ {
			if received[p] {
				continue
			}
			offset := int64(p-1) * f.PartSize
			size := min(f.PartSize, f.Size-offset)
			u, err := c.PartURL(f.PartURLTemplate, p)
			if err != nil {
				return Response{}, err
			}
			file, openErr := root.Open(filepath.FromSlash(input.Path))
			if openErr != nil {
				return Response{}, openErr
			}
			err = c.uploadPart(ctx, u, file, offset, size)
			file.Close()
			if err != nil {
				return Response{}, err
			}
		}
		if _, err = c.Do(ctx, http.MethodPost, "/api/v1/uploads/"+url.PathEscape(state.UploadID)+"/files/"+f.SHA256+"/complete", map[string]any{}, Options{}); err != nil {
			return Response{}, err
		}
	}
	if uploadOnly {
		return c.Do(ctx, http.MethodGet, "/api/v1/uploads/"+url.PathEscape(state.UploadID), nil, Options{})
	}
	state.Committing = true
	if err = SaveState(statePath, state); err != nil {
		return Response{}, err
	}
	return c.finishPush(ctx, body, statePath, &state)
}
func (c *Client) finishPush(ctx context.Context, body any, path string, state *PushState) (Response, error) {
	r, err := c.Do(ctx, http.MethodPost, "/api/v1/uploads/"+url.PathEscape(state.UploadID)+"/commit", body, Options{IdempotencyKey: state.CommitKey})
	if err != nil {
		return r, err
	}
	state.Result = bytes.Clone(r.Body)
	if err = SaveState(path, state); err != nil {
		return Response{}, fmt.Errorf("client: server accepted commit but local receipt save failed; retry the same state: %w", err)
	}
	return r, nil
}
