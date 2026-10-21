// Package device contains the first-party, outbound local-assistant wire
// protocol. Jobs and JobAttempt authority remains exclusively with T06 jobs.
package device

import (
	"encoding/json"
	"time"

	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

const Protocol = "lantai.local-assistant/v1"

type Device struct {
	ID               ids.ID    `json:"device_id"`
	ProjectID        ids.ID    `json:"project_id"`
	OwnerID          ids.ID    `json:"owner_id"`
	Alias            string    `json:"alias"`
	WorkspaceAlias   string    `json:"workspace_alias"`
	WorkspaceBinding string    `json:"workspace_binding"`
	Actions          []string  `json:"actions"`
	State            string    `json:"state"` // pending, active, revoked, or the derived expired view
	Revision         int64     `json:"revision"`
	ExpiresAt        time.Time `json:"expires_at"`
	LastSeenAt       time.Time `json:"last_seen_at"`
}
type Claim struct {
	ProjectID        ids.ID   `json:"project_id"`
	Alias            string   `json:"alias"`
	WorkspaceAlias   string   `json:"workspace_alias"`
	WorkspaceBinding string   `json:"workspace_binding"`
	Actions          []string `json:"actions"`
}
type ClaimResponse struct {
	Device    Device `json:"device"`
	ClaimCode string `json:"claim_code,omitempty"`
}
type ApproveDevice struct {
	ExpectedRevision int64  `json:"expected_revision"`
	ClaimCode        string `json:"claim_code"`
	WorkspaceAlias   string `json:"workspace_alias"`
	WorkspaceBinding string `json:"workspace_binding"`
}
type Control struct {
	ExpectedRevision int64 `json:"expected_revision"`
}
type DevicePage struct {
	Items      []Device `json:"items"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

// JobData contains immutable inputs and device observations, never an
// independent execution state. JobView projects the authoritative T06 Job.
type JobData struct {
	DeviceID         ids.ID          `json:"device_id"`
	OwnerID          ids.ID          `json:"owner_id"`
	WorkspaceAlias   string          `json:"workspace_alias"`
	WorkspaceBinding string          `json:"workspace_binding"`
	Job              json.RawMessage `json:"job"`
	JobDigest        string          `json:"job_digest"`
	ApprovalDigest   string          `json:"approval_digest,omitempty"`
	Inspection       *Inspection     `json:"inspection,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
	Stopped          bool            `json:"stopped"`
}
type JobView struct {
	JobData
	ID              ids.ID       `json:"job_id"`
	OperationID     ids.ID       `json:"operation_id"`
	ProjectID       ids.ID       `json:"project_id"`
	State           string       `json:"state"`
	Revision        int64        `json:"revision"`
	Fence           *ae.JobFence `json:"job_fence,omitempty"`
	LeaseExpiresAt  string       `json:"lease_expires_at,omitempty"`
	CancelRequested bool         `json:"cancel_requested"`
	FailureCode     string       `json:"failure_code,omitempty"`
}
type JobPage struct {
	Items      []JobView `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}
type CreateJob struct {
	ExpectedDeviceRevision int64           `json:"expected_device_revision"`
	ProjectID              ids.ID          `json:"project_id"`
	WorkspaceAlias         string          `json:"workspace_alias"`
	Job                    json.RawMessage `json:"job"`
	JobDigest              string          `json:"job_digest"`
}
type Inspection struct {
	Schema           string            `json:"schema"`
	JobID            string            `json:"job_id"`
	Action           string            `json:"action"`
	JobDigest        string            `json:"job_digest"`
	ApprovalDigest   string            `json:"approval_digest"`
	WorkspaceBinding string            `json:"workspace_binding"`
	InputSHA256      string            `json:"input_sha256"`
	Origin           string            `json:"origin,omitempty"`
	Paths            map[string]string `json:"paths"`
	NetworkRequired  bool              `json:"network_required"`
	RemoteWrites     bool              `json:"remote_writes"`
	SourceDeleted    bool              `json:"source_deleted"`
	TimeoutSeconds   int               `json:"timeout_seconds"`
	Warnings         []string          `json:"warnings"`
}
type InspectRequest struct {
	Inspection Inspection `json:"inspection"`
}
type ApproveJob struct {
	ExpectedRevision int64  `json:"expected_revision"`
	JobDigest        string `json:"job_digest"`
	ApprovalDigest   string `json:"approval_digest"`
	WorkspaceBinding string `json:"workspace_binding"`
}
type LeaseRequest struct {
	Fence ae.JobFence `json:"job_fence"`
}
type ResultRequest struct {
	Fence   ae.JobFence     `json:"job_fence"`
	Result  json.RawMessage `json:"result"`
	Stopped bool            `json:"stopped"`
}
type Poll struct {
	DeviceID ids.ID   `json:"device_id"`
	Job      *JobView `json:"job,omitempty"`
	Stop     bool     `json:"stop"`
	Reason   string   `json:"reason,omitempty"`
}
