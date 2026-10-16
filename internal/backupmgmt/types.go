// Package backupmgmt defines the administration contract shared by the core and
// HTTP adapters. It contains no authentication or filesystem authority.
package backupmgmt

import (
	"errors"
	"io/fs"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

var (
	ErrBusy                = errors.New("backup already active")
	ErrUnavailable         = errors.New("backup unavailable")
	ErrIdempotencyConflict = errors.New("backup idempotency conflict")
	ErrInvalid             = errors.New("invalid backup request")
	ErrRevisionConflict    = errors.New("backup schedule revision conflict")
	ErrNotFound            = fs.ErrNotExist
	ErrScopeStale          = errors.New("backup control scope no longer matches the active attempt")
)

type Request struct {
	Resume         ids.ID `json:"resume,omitempty"`
	DrainSeconds   int    `json:"drain_seconds,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}
type Job struct {
	ID               ids.ID    `json:"id"`
	BackupID         ids.ID    `json:"backup_id,omitempty"`
	CaptureStarted   bool      `json:"capture_started,omitempty"`
	State            string    `json:"state"`
	Error            string    `json:"error,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	Source           string    `json:"source,omitempty"`
	ScheduledFor     string    `json:"scheduled_for,omitempty"`
	ScheduleTimezone string    `json:"schedule_timezone,omitempty"`
	ScheduleRevision int64     `json:"schedule_revision,omitempty"`
	Attempt          int       `json:"attempt,omitempty"`
}
type Availability struct {
	// CheckedAt records the last completed admission/idle probe, not a live
	// guarantee. During an active job filesystem probing is deliberately deferred.
	CheckedAt     *time.Time `json:"checked_at,omitempty"`
	CheckDeferred bool       `json:"check_deferred"`
	Configured    bool       `json:"configured"`
	Available     bool       `json:"available"`
	Error         string     `json:"error,omitempty"`
}
type Schedule struct {
	AuthorizedBy      ids.ID     `json:"authorized_by,omitempty"`
	ApprovalOperation string     `json:"approval_operation,omitempty"`
	DisabledReason    string     `json:"disabled_reason,omitempty"`
	Enabled           bool       `json:"enabled"`
	Timezone          string     `json:"timezone"`
	Time              string     `json:"time"`
	DrainSeconds      int        `json:"drain_seconds,omitempty"`
	TimeoutSeconds    int        `json:"timeout_seconds,omitempty"`
	Revision          int64      `json:"revision"`
	UpdatedAt         time.Time  `json:"updated_at"`
	NextRunAt         *time.Time `json:"next_run_at"`
}
type ScheduleEvent struct {
	Date     string    `json:"date"`
	Timezone string    `json:"timezone"`
	State    string    `json:"state"`
	Error    string    `json:"error,omitempty"`
	At       time.Time `json:"at"`
	JobID    ids.ID    `json:"job_id,omitempty"`
}
type Overview struct {
	Availability         Availability    `json:"availability"`
	Current              *Job            `json:"current"`
	LastSuccess          *Job            `json:"last_success"`
	Schedule             Schedule        `json:"schedule"`
	RecentScheduleEvents []ScheduleEvent `json:"recent_schedule_events"`
}
type Service interface {
	Overview() (Overview, error)
	History() ([]Job, error)
	Trigger(Request, string) (Job, error)
	Status(ids.ID) (Job, error)
	Cancel(ids.ID) (Job, error)
	SaveSchedule(Schedule, string) (Schedule, error)
}
