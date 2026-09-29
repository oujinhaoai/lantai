package httpapi

import (
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
)

type SessionRequest struct {
	Token      string           `json:"token,omitempty"`
	Name       string           `json:"name,omitempty"`
	Password   string           `json:"password,omitempty"`
	Code       string           `json:"code,omitempty"`
	Channel    string           `json:"channel,omitempty"`
	Scopes     []identity.Scope `json:"scopes,omitempty"`
	Projects   []ids.ID         `json:"projects,omitempty"`
	TTLSeconds int64            `json:"ttl_seconds,omitempty"`
	Purpose    string           `json:"purpose,omitempty"`
	Model      string           `json:"model,omitempty"`
}

type SessionResponse struct {
	Token     string               `json:"token,omitempty"`
	CSRFToken string               `json:"csrf_token,omitempty"`
	Session   identity.SessionInfo `json:"session"`
}

type CreateProjectRequest struct {
	Key         string `json:"key"`
	Name        string `json:"name,omitempty"`
	ProjectType string `json:"project_type,omitempty"`
	Summary     string `json:"summary,omitempty"`
}

type ProjectView struct {
	ProjectID          ids.ID                     `json:"project_id"`
	Key                string                     `json:"key"`
	State              commit.ProjectState        `json:"state"`
	OperationID        ids.ID                     `json:"operation_id,omitempty"`
	Description        catalog.ProjectDescription `json:"description"`
	DescriptionPending bool                       `json:"description_pending,omitempty"`
	DescriptionError   errcode.Code               `json:"description_error,omitempty"`
	CreatedAt          *time.Time                 `json:"created_at,omitempty"`
	CreatedBy          ids.ID                     `json:"created_by,omitempty"`
}

func projectView(p catalog.Project, full bool) ProjectView {
	v := ProjectView{ProjectID: p.ProjectID, Key: p.Key, State: p.State, OperationID: p.OperationID,
		Description: p.Description, DescriptionPending: p.DescriptionPending, DescriptionError: p.DescriptionError}
	if full {
		v.CreatedAt, v.CreatedBy = &p.CreatedAt, p.CreatedBy
	} else {
		v.Description.Extra = nil
	}
	return v
}

type CreateUploadRequest struct {
	ProjectID ids.ID             `json:"project_id"`
	Files     []storage.FileSpec `json:"files"`
}

type UploadFileView struct {
	storage.UploadFile
	// Substitute only {part_number}, using an integer in [1, part_count].
	PartURLTemplate string `json:"part_url_template"`
}

type UploadView struct {
	storage.Upload
	Files []UploadFileView `json:"files"`
}

func uploadView(u storage.Upload) UploadView {
	v := UploadView{Upload: u, Files: make([]UploadFileView, 0, len(u.Files))}
	for _, f := range u.Files {
		if f.Received == nil {
			f.Received = []int{}
		}
		v.Files = append(v.Files, UploadFileView{UploadFile: f, PartURLTemplate: u.PartsURL + f.SHA256 + "/parts/{part_number}"})
	}
	return v
}

type CommitRequest struct {
	Task          *catalog.TaskBinding `json:"task,omitempty"`
	AssetID       ids.ID               `json:"asset_id,omitempty"`
	Slug          string               `json:"slug,omitempty"`
	BaseVersionID ids.ID               `json:"base_version_id,omitempty"`
	Content       catalog.ContentInput `json:"content"`
	Describe      *catalog.AssetPatch  `json:"describe,omitempty"`
}

type ReadGrantRequest struct {
	Path    string        `json:"path"`
	Purpose authz.Purpose `json:"purpose"`
}

type VersionView struct {
	OperationID    ids.ID           `json:"operation_id"`
	ProjectID      ids.ID           `json:"project_id"`
	AssetID        ids.ID           `json:"asset_id"`
	VersionID      ids.ID           `json:"version_id"`
	VersionNumber  int64            `json:"version_number"`
	ManifestDigest digest.Digest    `json:"manifest_digest"`
	CommittedAt    time.Time        `json:"committed_at"`
	Ref            ids.PermanentRef `json:"ref"`
	URI            string           `json:"uri"`
}

func versionView(v commit.Committed, instance ids.ID) VersionView {
	ref := v.Ref(instance)
	uri, _ := ref.URI()
	return VersionView{OperationID: v.OperationID, ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: v.VersionID,
		VersionNumber: v.VersionNumber, ManifestDigest: v.ManifestDigest, CommittedAt: v.CommittedAt, Ref: ref, URI: uri}
}

type AssetView struct {
	AssetID     ids.ID                   `json:"asset_id"`
	ProjectID   ids.ID                   `json:"project_id"`
	AssetType   manifest.AssetType       `json:"asset_type"`
	Description catalog.AssetDescription `json:"description"`
	Latest      VersionView              `json:"latest"`
}

type ExactVersionView struct {
	Version  VersionView   `json:"version"`
	Manifest *ManifestView `json:"manifest,omitempty"`
}

type ManifestView struct {
	Contract string `json:"contract"`
	manifest.Document
}
