package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// BackupScheduleInput excludes derived status fields. Revision is the expected
// revision, bound into the approval and checked again by the management owner.
type BackupScheduleInput struct {
	Enabled        bool   `json:"enabled"`
	Timezone       string `json:"timezone"`
	Time           string `json:"time"`
	DrainSeconds   int    `json:"drain_seconds,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	Revision       int64  `json:"revision"`
}

func (s BackupScheduleInput) Schedule() backupmgmt.Schedule {
	return backupmgmt.Schedule{Enabled: s.Enabled, Timezone: s.Timezone, Time: s.Time, DrainSeconds: s.DrainSeconds, TimeoutSeconds: s.TimeoutSeconds, Revision: s.Revision}
}

// ApproveBackup commits only a human-authorized operation receipt. It never
// calls the backup owner while identity's transaction/security lock is held.
// The HTTP adapter passes the resulting operation ID to that owner's durable
// idempotent admission after Execute returns. A disconnected request can replay
// the identical grant/key without approving or launching a second operation.
type ApproveBackup struct {
	Kind           string               `json:"kind"`
	JobID          ids.ID               `json:"job_id,omitempty"`
	DrainSeconds   int                  `json:"drain_seconds,omitempty"`
	TimeoutSeconds int                  `json:"timeout_seconds,omitempty"`
	Schedule       *BackupScheduleInput `json:"schedule,omitempty"`
}

func (c *ApproveBackup) action() authz.Action {
	switch c.Kind {
	case "start":
		return ActBackupStart
	case "resume":
		return ActBackupResume
	case "schedule":
		return ActBackupSchedule
	}
	return ""
}
func (c *ApproveBackup) project() ids.ID { return "" }
func (c *ApproveBackup) targets() []Target {
	target := "instance"
	if c.Kind == "resume" {
		target = string(c.JobID)
	}
	revision := int64(0)
	if c.Schedule != nil {
		revision = c.Schedule.Revision
	}
	return []Target{{Kind: "backup_" + c.Kind, ID: target, ExpectedRevision: revision}}
}
func (c *ApproveBackup) body() any { return c }
func backupTimeouts(drain, timeout int) error {
	d, t := drain, timeout
	if d == 0 {
		d = 120
	}
	if t == 0 {
		t = 7200
	}
	if d < 1 || d > 3600 || t < 1 || t > 86400 || d > t {
		return fmt.Errorf("invalid backup timeout")
	}
	return nil
}
func (c *ApproveBackup) check() error {
	switch c.Kind {
	case "start", "resume":
		if c.Schedule != nil || (c.Kind == "start" && c.JobID != "") || (c.Kind == "resume" && !c.JobID.Valid()) {
			return fmt.Errorf("invalid backup target")
		}
		return backupTimeouts(c.DrainSeconds, c.TimeoutSeconds)
	case "schedule":
		if c.Schedule == nil || c.JobID != "" || c.DrainSeconds != 0 || c.TimeoutSeconds != 0 {
			return fmt.Errorf("invalid schedule request")
		}
		s := c.Schedule
		if s.Revision < 0 || s.Timezone == "" || s.Timezone == "Local" {
			return fmt.Errorf("invalid schedule timezone or revision")
		}
		if _, err := time.LoadLocation(s.Timezone); err != nil {
			return fmt.Errorf("invalid IANA timezone")
		}
		t, err := time.Parse("15:04", s.Time)
		if err != nil || t.Format("15:04") != s.Time {
			return fmt.Errorf("daily time must be HH:MM")
		}
		return backupTimeouts(s.DrainSeconds, s.TimeoutSeconds)
	default:
		return fmt.Errorf("unknown backup approval kind")
	}
}
func (c *ApproveBackup) describe(context.Context, commands.DBTX) (string, error) {
	drain, timeout := c.DrainSeconds, c.TimeoutSeconds
	if c.Schedule != nil {
		drain, timeout = c.Schedule.DrainSeconds, c.Schedule.TimeoutSeconds
	}
	if drain == 0 {
		drain = 120
	}
	if timeout == 0 {
		timeout = 7200
	}
	limits := fmt.Sprintf("Drain limit %d seconds; total limit %d seconds. All business reads and writes pause until backup work exits.", drain, timeout)
	switch c.Kind {
	case "start":
		return "Start a full instance backup. " + limits, nil
	case "resume":
		return fmt.Sprintf("Resume full instance backup %s. %s", c.JobID, limits), nil
	default:
		return fmt.Sprintf("Set daily full instance backup schedule: enabled=%t, %s %s. %s", c.Schedule.Enabled, c.Schedule.Time, c.Schedule.Timezone, limits), nil
	}
}
func (c *ApproveBackup) apply(context.Context, *applyEnv) (any, error) {
	return map[string]any{"approved": true, "kind": c.Kind, "job_id": c.JobID, "schedule": c.Schedule}, nil
}

// CheckBackupScheduleOwner revalidates the human who approved a persisted
// schedule without borrowing a browser session. Session expiry does not revoke
// a plan; disabling its owner or removing the system admin role does.
func (s *Service) CheckBackupScheduleOwner(ctx context.Context, principalID ids.ID) error {
	return s.WithBackupScheduleOwner(ctx, principalID, func() error { return nil })
}

// WithBackupScheduleOwner keeps the existing security guard through scheduled
// admission. A concurrent revoke either finishes first (and is observed here),
// or waits for this already-authorized admission. The callback must only admit
// bounded local work, never wait for the backup itself or call identity writes.
func (s *Service) WithBackupScheduleOwner(ctx context.Context, principalID ids.ID, admit func() error) error {
	if !principalID.Valid() || admit == nil {
		return errcode.New(errcode.Forbidden, "backup schedule owner is not an active administrator")
	}
	lctx, held, err := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return err
	}
	defer held.Release()
	p, err := loadPrincipal(lctx, s.main, principalID)
	if errors.Is(err, errNotFound) {
		return errcode.New(errcode.Forbidden, "backup schedule owner is not an active administrator")
	}
	if err != nil {
		return err
	}
	if p.Kind != authz.Human || p.State != StateActive {
		return errcode.New(errcode.Forbidden, "backup schedule owner is not an active administrator")
	}
	ok, err := hasSystemRole(lctx, s.main, principalID, RoleAdmin)
	if err != nil {
		return err
	}
	if !ok {
		return errcode.New(errcode.Forbidden, "backup schedule owner is not an active administrator")
	}
	return admit()
}
