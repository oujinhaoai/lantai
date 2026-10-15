package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // scratch images have no zoneinfo database.

	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

const maxBackupReceipts = 10000

type backupReceipt struct {
	Key  string `json:"key"`
	Hash string `json:"hash"`
}
type backupRecord struct {
	Job      BackupJob
	Receipts []backupReceipt
}

// Keep old flat journals readable; new receipt metadata shares the atomic job
// write, so a crash can never accept the same trigger as two distinct jobs.
func (r backupRecord) MarshalJSON() ([]byte, error) {
	type job BackupJob
	return json.Marshal(struct {
		job
		Receipts []backupReceipt `json:"request_receipts,omitempty"`
	}{job(r.Job), r.Receipts})
}
func (r *backupRecord) UnmarshalJSON(raw []byte) error {
	type job BackupJob
	var v struct {
		job
		Receipts []backupReceipt `json:"request_receipts,omitempty"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return err
	}
	r.Job = BackupJob(v.job)
	r.Receipts = v.Receipts
	return nil
}

type backupManagementState struct {
	Version          int                        `json:"version"`
	Schedule         backupmgmt.Schedule        `json:"schedule"`
	ScheduleReceipts []scheduleReceipt          `json:"schedule_receipts,omitempty"`
	Events           []backupmgmt.ScheduleEvent `json:"events,omitempty"`
}
type scheduleReceipt struct {
	Key    string              `json:"key"`
	Hash   string              `json:"hash"`
	Result backupmgmt.Schedule `json:"result"`
}

// ConfigureBackups makes optional backup configuration observable without
// taking the normal core offline. A corrupt journal disables all backup writes;
// it is never silently discarded or replaced by an empty state.
func (a *App) ConfigureBackups(ctx context.Context, root, rootID string) *BackupControl {
	if b, err := a.EnableBackups(ctx, root, rootID); err == nil {
		return b
	}
	if a.backupControl != nil {
		return a.backupControl
	}
	b := &BackupControl{app: a, root: root, rootID: rootID, jobs: map[ids.ID]BackupJob{}, receipts: map[ids.ID][]backupReceipt{}, ctx: ctx, run: a.Backup, now: func() time.Time { return a.Instance.Clock().Now() }}

	// Load history even when the external mount is missing. Do not initialize or
	// adopt a changed mount; the first successful preflight pins its inode.
	if err := b.loadJobs(); err != nil {
		b.managementErr = err
		b.jobs = map[ids.ID]BackupJob{}
		b.receipts = map[ids.ID][]backupReceipt{}
	}
	if err := b.loadManagement(); err != nil {
		b.managementErr = err
		b.management = backupManagementState{}
	}
	a.backupControl = b
	b.startScheduler()
	return b
}
func (a *App) BackupManagement() backupmgmt.Service {
	if a.backupControl == nil {
		return nil
	}
	return a.backupControl
}

func (b *BackupControl) loadJobs() error {
	if err := b.checkJournalDirs(false); err != nil {
		return err
	}
	entries, err := os.ReadDir(b.journalDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if handled, e := b.recoverJournalTemp(entry); handled || e != nil {
			if e != nil {
				return e
			}
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return errors.New("invalid backup journal")
		}
		id := ids.ID(strings.TrimSuffix(entry.Name(), ".json"))
		if !id.Valid() {
			return errors.New("invalid backup journal ID")
		}
		j, receipts, e := readBackupRecord(filepath.Join(b.journalDir(), entry.Name()))
		if e != nil {
			return e
		}
		if j.ID != id {
			return errors.New("invalid backup journal identity")
		}
		switch j.State {
		case "draining", "backing_up", "cancelling":
			j.State, j.Error, j.UpdatedAt = "interrupted", "process_interrupted", b.now()
			b.receipts[id] = receipts
			if e = b.save(j); e != nil {
				return e
			}
		case "complete", "failed", "cancelled", "interrupted":
		default:
			return errors.New("invalid backup journal state")
		}
		b.jobs[id], b.receipts[id] = j, receipts
	}
	return b.validateAllReceipts()
}
func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	raw, err := hex.DecodeString(s)
	return err == nil && hex.EncodeToString(raw) == s
}
func validateReceipts(rs []backupReceipt) error {
	if len(rs) > maxBackupReceipts {
		return errors.New("too many backup receipts")
	}
	seen := map[string]bool{}
	for _, r := range rs {
		if !validDigest(r.Key) || !validDigest(r.Hash) || seen[r.Key] {
			return errors.New("invalid backup receipt")
		}
		seen[r.Key] = true
	}
	return nil
}
func (b *BackupControl) validateAllReceipts() error {
	seen := map[string]bool{}
	slots := map[string]bool{}
	for _, j := range b.jobs {
		if j.Source == "schedule" {
			slot := j.ScheduleTimezone + "/" + j.ScheduledFor
			if slots[slot] {
				return errors.New("duplicate scheduled job")
			}
			slots[slot] = true
		}
	}
	for _, rs := range b.receipts {
		for _, r := range rs {
			if seen[r.Key] {
				return errors.New("duplicate backup receipt")
			}
			seen[r.Key] = true
		}
	}
	for _, r := range b.management.ScheduleReceipts {
		if seen[r.Key] {
			return errors.New("backup operation reused across actions")
		}
		seen[r.Key] = true
	}
	return nil
}
func backupDigest(raw []byte) string { s := sha256.Sum256(raw); return hex.EncodeToString(s[:]) }
func normalizeBackupRequest(req BackupRequest) (BackupRequest, error) {
	if req.Resume != "" && !req.Resume.Valid() {
		return req, backupmgmt.ErrInvalid
	}
	if req.DrainSeconds == 0 {
		req.DrainSeconds = 120
	}
	if req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = 7200
	}
	if req.DrainSeconds < 1 || req.DrainSeconds > 3600 || req.TimeoutSeconds < 1 || req.TimeoutSeconds > 86400 || req.DrainSeconds > req.TimeoutSeconds {
		return req, backupmgmt.ErrInvalid
	}
	return req, nil
}
func requestHash(req BackupRequest) string { raw, _ := json.Marshal(req); return backupDigest(raw) }
func requestKey(key string) (string, error) {
	if len(key) < 1 || len(key) > 200 {
		return "", backupmgmt.ErrInvalid
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			return "", backupmgmt.ErrInvalid
		}
	}
	return backupDigest([]byte(key)), nil
}
func (b *BackupControl) Trigger(req backupmgmt.Request, key string) (backupmgmt.Job, error) {
	req, err := normalizeBackupRequest(req)
	if err != nil {
		return BackupJob{}, err
	}
	k, err := requestKey(key)
	if err != nil {
		return BackupJob{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.managementErr != nil {
		return BackupJob{}, backupmgmt.ErrUnavailable
	}
	for _, r := range b.management.ScheduleReceipts {
		if r.Key == k {
			return BackupJob{}, backupmgmt.ErrIdempotencyConflict
		}
	}
	for id, rs := range b.receipts {
		for _, r := range rs {
			if r.Key == k {
				if r.Hash != requestHash(req) {
					return BackupJob{}, backupmgmt.ErrIdempotencyConflict
				}
				return b.jobs[id], nil
			}
		}
	}
	total := 0
	for _, rs := range b.receipts {
		total += len(rs)
	}
	if total >= maxBackupReceipts {
		return BackupJob{}, fmt.Errorf("%w: receipt_capacity", backupmgmt.ErrUnavailable)
	}
	return b.startLocked(req, k, "manual", "")
}
func (b *BackupControl) availabilityLocked() backupmgmt.Availability {
	// External storage can block indefinitely. Once work is admitted, status and
	// cancellation must never queue behind an external filesystem health probe.
	// The worker owns live destination checks under its consistency protection.
	if b.active != "" {
		cached := b.lastAvailability
		cached.CheckDeferred = true
		if b.managementErr != nil {
			cached.Available = false
			cached.Error = "journal_invalid"
		}
		return cached
	}
	current := b.inspectAvailabilityLocked()
	checked := b.now()
	current.CheckedAt = &checked
	b.lastAvailability = current
	return current
}

func (b *BackupControl) inspectAvailabilityLocked() backupmgmt.Availability {
	a := backupmgmt.Availability{Configured: b.root != "" && b.rootID != ""}
	if b.managementErr != nil {
		a.Error = "journal_invalid"
		return a
	}
	if !a.Configured {
		a.Error = "not_configured"
		return a
	}
	if !validBackupRootID(b.rootID) || !filepath.IsAbs(b.root) || filepath.Clean(b.root) != b.root {
		a.Error = "invalid_root_identity"
		return a
	}
	if err := b.preflight(); err != nil {
		a.Error = availabilityCode(err)
		return a
	}
	a.Available = true
	return a
}
func availabilityCode(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "permissions"):
		return "root_permissions_unsafe"
	case strings.Contains(s, "identity"):
		return "root_identity_invalid"
	case strings.Contains(s, "overlaps"):
		return "root_overlaps_data"
	case strings.Contains(s, "space") || strings.Contains(s, "capacity"):
		return "insufficient_space"
	case strings.Contains(s, "replaced") || strings.Contains(s, "remounted"):
		return "root_replaced"
	default:
		return "root_unavailable"
	}
}
func (b *BackupControl) Overview() (backupmgmt.Overview, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := backupmgmt.Overview{Availability: b.availabilityLocked(), Schedule: b.scheduleViewLocked(), RecentScheduleEvents: []backupmgmt.ScheduleEvent{}}
	if j, ok := b.jobs[b.active]; ok {
		copy := j
		result.Current = &copy
	}
	for _, j := range b.jobs {
		if j.State == "complete" && (result.LastSuccess == nil || j.UpdatedAt.After(result.LastSuccess.UpdatedAt)) {
			copy := j
			result.LastSuccess = &copy
		}
	}
	first := max(0, len(b.management.Events)-20)
	for i := len(b.management.Events) - 1; i >= first; i-- {
		result.RecentScheduleEvents = append(result.RecentScheduleEvents, b.management.Events[i])
	}
	return result, nil
}
func (b *BackupControl) History() ([]backupmgmt.Job, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	jobs := make([]BackupJob, 0, len(b.jobs))
	for _, j := range b.jobs {
		jobs = append(jobs, j)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
			return jobs[i].ID > jobs[j].ID
		}
		return jobs[i].CreatedAt.After(jobs[j].CreatedAt)
	})
	return jobs, nil
}
func (b *BackupControl) managementFile() string {
	return filepath.Join(b.app.Instance.Layout().Home, "backups", "management.json")
}
func (b *BackupControl) loadManagement() error {
	b.management = backupManagementState{Version: 1, Schedule: backupmgmt.Schedule{Timezone: "UTC", Time: "02:00", DrainSeconds: 120, TimeoutSeconds: 7200}}
	if err := b.checkJournalDirs(false); err != nil {
		return err
	}
	file := b.managementFile()
	st, err := os.Lstat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return b.validateAllReceipts()
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() > 4<<20 {
		return errors.New("unsafe management journal")
	}
	if err = fsutil.CheckPrivate(file); err != nil {
		return err
	}
	if (runtime.GOOS == "linux" || runtime.GOOS == "darwin") && !backupOwner(st) {
		return errors.New("foreign management journal owner")
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 4<<20 {
		return errors.New("management journal exceeds size limit")
	}
	if _, err = canonjson.Decode(raw); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&b.management); err != nil {
		return err
	}
	if err = d.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing management journal")
	}
	if b.management.Version != 1 {
		return errors.New("invalid management journal version")
	}
	if err = validateStoredSchedule(b.management.Schedule); err != nil {
		return err
	}
	if len(b.management.ScheduleReceipts) > maxBackupReceipts || len(b.management.Events) > 10000 {
		return errors.New("management journal capacity")
	}
	seen := map[string]bool{}
	for _, r := range b.management.ScheduleReceipts {
		if !validDigest(r.Key) || !validDigest(r.Hash) || seen[r.Key] {
			return errors.New("invalid schedule receipt")
		}
		seen[r.Key] = true
		if err = validateStoredSchedule(r.Result); err != nil {
			return err
		}
	}
	slots := map[string]bool{}
	for _, e := range b.management.Events {
		if _, err = time.Parse("2006-01-02", e.Date); err != nil {
			return err
		}
		if _, err = time.LoadLocation(e.Timezone); err != nil {
			return err
		}
		if e.At.IsZero() {
			return errors.New("invalid schedule event time")
		}
		switch e.State {
		case "started", "skipped_busy", "unavailable", "missed", "dst_missing", "authorization_revoked", "authorization_check_failed":
		default:
			return errors.New("invalid schedule event state")
		}
		key := e.Timezone + "/" + e.Date
		if slots[key] && e.State != "authorization_revoked" {
			return errors.New("duplicate schedule event")
		}
		if e.State != "authorization_revoked" {
			slots[key] = true
		}
		if e.State == "started" {
			j, ok := b.jobs[e.JobID]
			if !ok || j.Source != "schedule" || j.ScheduleTimezone != e.Timezone || j.ScheduledFor != e.Date {
				return errors.New("invalid schedule event job")
			}
		} else if e.JobID != "" {
			return errors.New("unexpected schedule event job")
		}
	}
	return b.validateAllReceipts()
}
func (b *BackupControl) saveManagementLocked() error {
	if err := b.checkJournalDirs(true); err != nil {
		return err
	}
	raw, err := json.Marshal(b.management)
	if len(raw) > 4<<20 || len(b.management.Events) > 10000 {
		return errors.New("backup management journal capacity")
	}
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(b.managementFile()), 0700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(b.managementFile(), raw, 0600)
}
func normalizeSchedule(s backupmgmt.Schedule) (backupmgmt.Schedule, error) {
	if s.Timezone == "" || s.Timezone == "Local" || strings.Contains(s.Timezone, "..") {
		return s, backupmgmt.ErrInvalid
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return s, backupmgmt.ErrInvalid
	}
	if len(s.Time) != 5 {
		return s, backupmgmt.ErrInvalid
	}
	if t, err := time.Parse("15:04", s.Time); err != nil || t.Format("15:04") != s.Time {
		return s, backupmgmt.ErrInvalid
	}
	req, err := normalizeBackupRequest(BackupRequest{DrainSeconds: s.DrainSeconds, TimeoutSeconds: s.TimeoutSeconds})
	if err != nil {
		return s, err
	}
	s.DrainSeconds, s.TimeoutSeconds = req.DrainSeconds, req.TimeoutSeconds
	return s, nil
}
func scheduleHash(s backupmgmt.Schedule) string {
	s.UpdatedAt = time.Time{}
	s.NextRunAt = nil
	s.DisabledReason = ""
	s.ApprovalOperation = ""
	raw, _ := json.Marshal(s)
	return backupDigest(raw)
}
func (b *BackupControl) SaveSchedule(s backupmgmt.Schedule, key string) (backupmgmt.Schedule, error) {
	s, err := normalizeSchedule(s)
	if !s.AuthorizedBy.Valid() {
		return s, backupmgmt.ErrInvalid
	}
	if err != nil {
		return s, err
	}
	k, err := requestKey(key)
	if err != nil {
		return s, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.managementErr != nil {
		return s, backupmgmt.ErrUnavailable
	}
	if b.closing || b.ctx.Err() != nil {
		return s, backupmgmt.ErrUnavailable
	}
	for _, rs := range b.receipts {
		for _, r := range rs {
			if r.Key == k {
				return s, backupmgmt.ErrIdempotencyConflict
			}
		}
	}
	for _, r := range b.management.ScheduleReceipts {
		if r.Key == k {
			if r.Hash != scheduleHash(s) {
				return s, backupmgmt.ErrIdempotencyConflict
			}
			return r.Result, nil
		}
	}
	if len(b.management.ScheduleReceipts) >= maxBackupReceipts {
		return s, backupmgmt.ErrUnavailable
	}
	if s.Revision != b.management.Schedule.Revision {
		return s, backupmgmt.ErrRevisionConflict
	}
	requestDigest := scheduleHash(s)
	prior := b.management
	s.Revision = b.management.Schedule.Revision + 1
	s.UpdatedAt = b.now()
	s.ApprovalOperation = key
	s.DisabledReason = ""
	s.NextRunAt = nil
	if s.Enabled {
		at, _, _ := b.nextOccurrenceLocked(s, s.UpdatedAt)
		s.NextRunAt = &at
	}
	b.management.Schedule = s
	b.management.ScheduleReceipts = append(append([]scheduleReceipt(nil), b.management.ScheduleReceipts...), scheduleReceipt{Key: k, Hash: requestDigest, Result: s})
	if err = b.saveManagementLocked(); err != nil {
		b.management = prior
		b.managementErr = err
		return s, err
	}
	return s, nil
}
func (b *BackupControl) scheduleViewLocked() backupmgmt.Schedule { return b.management.Schedule }
func (b *BackupControl) startScheduler() {
	ctx, cancel := context.WithCancel(b.ctx)
	b.schedulerCancel = cancel
	b.schedulerDone = make(chan struct{})
	go func() {
		defer close(b.schedulerDone)
		b.runScheduleContext(ctx, b.now(), true)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.runScheduleContext(ctx, b.now(), false)
			}
		}
	}()
}
func slotUsed(s backupManagementState, jobs map[ids.ID]BackupJob, zone, date string) bool {
	for _, e := range s.Events {
		if e.Timezone == zone && e.Date == date && e.State != "authorization_revoked" {
			return true
		}
	}
	for _, j := range jobs {
		if j.Source == "schedule" && j.ScheduleTimezone == zone && j.ScheduledFor == date {
			return true
		}
	}
	return false
}

// dailyOccurrence chooses the first actual instant for repeated wall times.
// Nonexistent spring-forward times produce a missed slot at the first later
// local minute. All operations are bounded and use the embedded IANA database.
func dailyOccurrence(date, wall string, loc *time.Location) (time.Time, bool) {
	day, _ := time.ParseInLocation("2006-01-02", date, loc)
	pivot := day.Add(12 * time.Hour)
	start := pivot.Add(-30 * time.Hour).Truncate(time.Minute)
	var later time.Time
	for t := start; !t.After(pivot.Add(30 * time.Hour)); t = t.Add(time.Minute) {
		local := t.In(loc)
		if local.Format("2006-01-02") != date {
			continue
		}
		hm := local.Format("15:04")
		if hm == wall {
			return t.UTC(), true
		}
		if hm > wall && later.IsZero() {
			later = t.UTC()
		}
	}
	if later.IsZero() {
		later = day.AddDate(0, 0, 1).UTC()
	}
	return later, false
}
func (b *BackupControl) nextOccurrenceLocked(s backupmgmt.Schedule, after time.Time) (time.Time, string, bool) {
	loc, _ := time.LoadLocation(s.Timezone)
	local := after.In(loc)
	for n := 0; n < 370; n++ {
		date := local.AddDate(0, 0, n).Format("2006-01-02")
		at, exists := dailyOccurrence(date, s.Time, loc)
		if at.After(after) && !slotUsed(b.management, b.jobs, s.Timezone, date) {
			return at, date, exists
		}
	}
	return after.AddDate(1, 0, 0), "", false
}
func (b *BackupControl) runSchedule(now time.Time, restarting bool) {
	b.runScheduleContext(b.ctx, now, restarting)
}

func (b *BackupControl) runScheduleContext(ctx context.Context, now time.Time, restarting bool) {
	b.mu.Lock()
	snapshot := b.management.Schedule
	if b.closing || b.managementErr != nil || !snapshot.Enabled {
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	// Lock order matches the administrator API: security authority, then the
	// backup mutex. Never wait for the security guard while holding b.mu.
	authorize := b.authorizeSchedule
	if authorize == nil {
		authorize = b.app.Identity.WithBackupScheduleOwner
	}
	err := authorize(ctx, snapshot.AuthorizedBy, func() error {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.management.Schedule.Revision != snapshot.Revision || b.management.Schedule.AuthorizedBy != snapshot.AuthorizedBy {
			return nil
		}
		if b.management.Schedule.DisabledReason == "authorization_check_failed" {
			b.management.Schedule.DisabledReason = ""
			if err := b.saveManagementLocked(); err != nil {
				b.managementErr = err
				return err
			}
		}
		b.runScheduleLocked(now, restarting)
		return nil
	})
	if err == nil || ctx.Err() != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closing || b.managementErr != nil || !b.management.Schedule.Enabled || b.management.Schedule.Revision != snapshot.Revision || b.management.Schedule.AuthorizedBy != snapshot.AuthorizedBy {
		return
	}
	// A transient authority check failure skips execution but does not assume
	// the administrator was revoked. It is visible and rechecked next tick.
	reason := "authorization_check_failed"
	if errcode.CodeOf(err) == errcode.Forbidden {
		reason = "authorization_revoked"
	}
	b.management.Schedule.DisabledReason = reason
	if reason == "authorization_revoked" {
		b.management.Schedule.Enabled = false
		b.management.Schedule.NextRunAt = nil
		b.management.Schedule.Revision++
		b.management.Schedule.UpdatedAt = now
	}
	// Only a failed check at the actual due slot consumes that day's attempt.
	// An early transient database/authority error remains observable but cannot
	// silently suppress a future daily backup after the check recovers.
	due := snapshot.NextRunAt != nil && !snapshot.NextRunAt.After(now)
	if reason == "authorization_revoked" || due {
		loc, _ := time.LoadLocation(snapshot.Timezone)
		at := now
		if due {
			at = *snapshot.NextRunAt
		}
		date := at.In(loc).Format("2006-01-02")
		if reason == "authorization_revoked" || !slotUsed(b.management, b.jobs, snapshot.Timezone, date) {
			b.management.Events = append(b.management.Events, backupmgmt.ScheduleEvent{Date: date, Timezone: snapshot.Timezone, State: reason, Error: reason, At: now})
		}
		if due && reason != "authorization_revoked" {
			next, _, _ := b.nextOccurrenceLocked(snapshot, now)
			b.management.Schedule.NextRunAt = &next
		}
	}

	if e := b.saveManagementLocked(); e != nil {
		b.managementErr = e
	}
}

func (b *BackupControl) runScheduleLocked(now time.Time, restarting bool) {
	if b.closing || b.managementErr != nil {
		return
	}
	s := b.management.Schedule
	if !s.Enabled {
		return
	}
	if s.NextRunAt == nil {
		at, _, _ := b.nextOccurrenceLocked(s, now)
		b.management.Schedule.NextRunAt = &at
		if err := b.saveManagementLocked(); err != nil {
			b.managementErr = err
		}
		return
	}
	if s.NextRunAt.After(now) {
		return
	}
	loc, _ := time.LoadLocation(s.Timezone)
	date := s.NextRunAt.In(loc).Format("2006-01-02")
	_, exists := dailyOccurrence(date, s.Time, loc)
	if !slotUsed(b.management, b.jobs, s.Timezone, date) {
		event := backupmgmt.ScheduleEvent{Date: date, Timezone: s.Timezone, At: now}
		switch {
		case !exists:
			event.State = "dst_missing"
		case restarting || now.Sub(*s.NextRunAt) > 90*time.Second:
			event.State = "missed"
		case b.active != "":
			event.State = "skipped_busy"
		case !b.availabilityLocked().Available:
			event.State = "unavailable"
			event.Error = b.availabilityLocked().Error
		default:
			j, err := b.startLocked(BackupRequest{DrainSeconds: s.DrainSeconds, TimeoutSeconds: s.TimeoutSeconds}, "", "schedule", date)
			if err != nil {
				event.State, event.Error = "unavailable", "backup_start_failed"
			} else {
				event.State, event.JobID = "started", j.ID
			}
		}
		b.management.Events = append(b.management.Events, event)
	}
	// A suspended process or restart never catches up missed dates. Each last due
	// slot is recorded once, and next_run_at is strictly in the future.
	at, _, _ := b.nextOccurrenceLocked(s, now)
	b.management.Schedule.NextRunAt = &at
	if err := b.saveManagementLocked(); err != nil {
		b.managementErr = err
	}
}

// Ancestors below the already trusted instance home must stay private and real.
// We never follow a journal-directory alias or repair someone else's directory.
func (b *BackupControl) checkJournalDirs(create bool) error {
	for _, path := range []string{filepath.Join(b.app.Instance.Layout().Home, "backups"), b.journalDir()} {
		st, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			if !create {
				continue
			}
			if err = os.Mkdir(path, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			st, err = os.Lstat(path)
		}
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe backup journal directory")
		}
		if err = fsutil.CheckPrivate(path); err != nil {
			return err
		}
		if (runtime.GOOS == "linux" || runtime.GOOS == "darwin") && !backupOwner(st) {
			return errors.New("foreign backup journal directory")
		}
	}
	return nil
}

func validBackupRootID(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if r < 33 || r > 126 || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

func validateStoredSchedule(s backupmgmt.Schedule) error {
	normalized, err := normalizeSchedule(s)
	if err != nil {
		return err
	}
	if normalized.DrainSeconds != s.DrainSeconds || normalized.TimeoutSeconds != s.TimeoutSeconds || s.Revision < 0 {
		return errors.New("invalid stored backup schedule")
	}
	switch s.DisabledReason {
	case "", "authorization_revoked", "authorization_check_failed":
	default:
		return errors.New("invalid schedule disabled reason")
	}
	if s.DisabledReason == "authorization_revoked" && s.Enabled {
		return errors.New("revoked schedule is enabled")
	}
	if s.Revision > 0 && (!s.AuthorizedBy.Valid() || s.ApprovalOperation == "") {
		return errors.New("missing backup schedule authority")
	}
	if s.Revision == 0 {
		if s.Enabled || !s.UpdatedAt.IsZero() {
			return errors.New("invalid initial backup schedule")
		}
	} else if s.UpdatedAt.IsZero() {
		return errors.New("missing backup schedule update time")
	}
	if s.Enabled {
		if s.NextRunAt == nil || s.NextRunAt.IsZero() {
			return errors.New("missing next backup schedule time")
		}
		loc, _ := time.LoadLocation(s.Timezone)
		at, _ := dailyOccurrence(s.NextRunAt.In(loc).Format("2006-01-02"), s.Time, loc)
		if !at.Equal(*s.NextRunAt) {
			return errors.New("invalid next backup schedule time")
		}
	} else if s.NextRunAt != nil {
		return errors.New("disabled backup schedule has next time")
	}
	return nil
}

func backupPrivateOwner(st os.FileInfo) bool {
	return (runtime.GOOS != "linux" && runtime.GOOS != "darwin") || backupOwner(st)
}
