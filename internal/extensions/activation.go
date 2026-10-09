package extensions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
)

// Activation is the runtime host observation for one enablement generation.
// "ready" means the authorized restricted probe passed in this environment.
type Activation struct {
	EnablementID      ids.ID                `json:"enablement_id"`
	Generation        int64                 `json:"generation"`
	State             string                `json:"state"`
	EnvironmentDigest digest.Digest         `json:"environment_digest"`
	Environment       Environment           `json:"environment"`
	ProbeID           ids.ID                `json:"probe_id,omitempty"`
	Failure           string                `json:"failure,omitempty"`
	Observation       *execution.Invocation `json:"observation,omitempty"`
	UpdatedAt         string                `json:"updated_at"`
}

// Environment binds probe evidence to package, entry, config and host facts;
// a change to any of them invalidates the probe instead of reusing it.
type Environment struct {
	Platform      string                `json:"platform"`
	Capabilities  IsolationCapabilities `json:"capabilities"`
	CoreRelease   digest.Digest         `json:"core_release_digest"`
	PackageDigest digest.Digest         `json:"package_digest"`
	EntryDigest   digest.Digest         `json:"entry_digest"`
	ConfigDigest  digest.Digest         `json:"config_digest"`
}

func (m *Manager) environment(e Enablement) Environment {
	return Environment{Platform: Platform(), Capabilities: m.caps, CoreRelease: m.d.Registry.release, PackageDigest: e.PackageDigest, EntryDigest: e.EntryDigest, ConfigDigest: e.ConfigDigest}
}

func (m *Manager) environmentDigest(e Enablement) digest.Digest {
	b, err := canonjson.CanonicalizeValue(m.environment(e))
	if err != nil {
		return ""
	}
	return digest.Of(b)
}

func (m *Manager) activation(ctx context.Context, id ids.ID, generation int64) (*Activation, error) {
	var raw string
	err := m.d.Runtime.QueryRowContext(ctx, `SELECT record FROM extensions_activations WHERE enablement_id=? AND generation=?`, id, generation).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a Activation
	return &a, json.Unmarshal([]byte(raw), &a)
}

func (m *Manager) saveActivation(ctx context.Context, a *Activation) error {
	a.UpdatedAt = clock.Format(m.d.Clock.Now())
	ctx, h, err := m.d.Gate.Acquire(ctx, commands.Request{})
	if err != nil {
		return err
	}
	defer h.Release()
	return m.writeActivation(ctx, a)
}

// writeActivation requires the caller to hold this manager's write gate.
func (m *Manager) writeActivation(ctx context.Context, a *Activation) error {
	a.UpdatedAt = clock.Format(m.d.Clock.Now())
	_, err := m.d.Runtime.ExecContext(ctx, `INSERT INTO extensions_activations(enablement_id,generation,state,environment_digest,record) VALUES(?,?,?,?,?) ON CONFLICT(enablement_id,generation) DO UPDATE SET state=excluded.state,environment_digest=excluded.environment_digest,record=excluded.record`, a.EnablementID, a.Generation, a.State, a.EnvironmentDigest, encode(a))
	return err
}

func (e Enablement) snapshot() ae.ActivationSnapshot {
	return ae.ActivationSnapshot{Contract: "lantai.activation-snapshot/v1", Activation: execution.Activation{ExtensionID: e.ExtensionID, ExtensionVersion: e.ExtensionVersion, PackageDigest: e.PackageDigest, Generation: e.Generation}, Entry: ae.PackageEntry{ExtensionID: e.ExtensionID, ExtensionVersion: e.ExtensionVersion, PackageDigest: e.PackageDigest, Target: e.Target, Entry: e.Entry, EntryDigest: e.EntryDigest}, EffectiveConfigRevision: e.ConfigRevision, EffectiveConfigDigest: e.ConfigDigest, EnablementPolicyRevision: e.PolicyRevision}
}

func (e Enablement) producer(contribution string) storage.Producer {
	return storage.Producer{ExtensionID: e.ExtensionID, ExtensionVersion: e.ExtensionVersion, PackageDigest: e.PackageDigest, Source: "package", ContributionID: contribution}
}

// Probe reruns the restricted capability probe for the current generation,
// e.g. after a core release changed the environment. The enable grant must
// have authorized probing; a probe never enables, re-enables or widens scope.
func (m *Manager) Probe(ctx context.Context, who authz.Context, id ids.ID) (Activation, error) {
	if err := m.authorize(ctx, who, identity.ActProbeExtension); err != nil {
		return Activation{}, err
	}
	e, err := m.enablement(ctx, m.d.Main, id)
	if err != nil {
		return Activation{}, err
	}
	if e == nil {
		return Activation{}, errcode.New(errcode.NotFound, "")
	}
	return m.runProbe(ctx, *e)
}

func (m *Manager) runProbe(ctx context.Context, e Enablement) (Activation, error) {
	if e.State != "enabled" || e.Target != "server" || !e.ProbeAuthorized {
		return Activation{}, failure(errcode.ExtensionActivationStale, "probe_not_authorized")
	}
	m.mu.Lock()
	if m.probing[e.ID] {
		m.mu.Unlock()
		return Activation{}, errcode.New(errcode.ResourceBusy, "probe already running")
	}
	m.probing[e.ID] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.probing, e.ID); m.mu.Unlock() }()
	rec, err := m.packageRecord(ctx, m.d.Main, e.ExtensionID, e.ExtensionVersion)
	if err != nil {
		return Activation{}, err
	}
	review, err := m.d.Packages.PackageReview(ctx, rec.Ref)
	if err != nil {
		return Activation{}, err
	}
	if !review.Approved || review.ReviewID != e.ReviewID {
		return Activation{}, failure(errcode.ExtensionActivationStale, "package_review_revoked")
	}
	pkg, fsys, err := m.readPackage(ctx, rec)
	if err != nil {
		return Activation{}, err
	}
	a := Activation{EnablementID: e.ID, Generation: e.Generation, State: "pending_probe", Environment: m.environment(e), EnvironmentDigest: m.environmentDigest(e)}
	if a.ProbeID, err = m.d.IDs.New(); err != nil {
		return a, err
	}
	if err = m.saveActivation(ctx, &a); err != nil {
		return a, err
	}
	epoch, err := m.d.Authority.RecoveryEpoch(ctx)
	if err != nil {
		return a, err
	}
	contribution := ""
	for _, c := range rec.Manifest.Contributes {
		if c.Target == "server" {
			contribution = c.ID
			break
		}
	}
	timeout := min(time.Duration(rec.Manifest.ResourceLimits.TimeoutSeconds)*time.Second, time.Minute)
	deadline := time.Now().Add(timeout)
	empty, err := tc.SnapshotDigest(nil)
	if err != nil {
		return a, err
	}
	in := ProcessorInput{Contract: "lantai.processor-input/v1", OperationID: a.ProbeID, Fence: ae.JobFence{JobID: a.ProbeID, JobAttemptID: a.ProbeID, LeaseFence: 1, RecoveryEpoch: epoch}, InputRefs: []ids.PermanentRef{}, InputDigest: empty, Deadline: clock.Format(deadline), Activation: e.snapshot(), Producer: e.producer(contribution), Config: e.Config, Mode: "probe"}
	if err = validate("lantai.processor-input/v1", in); err != nil {
		return a, err
	}
	job, err := json.Marshal(in)
	if err != nil {
		return a, err
	}
	// Register cancellation while admission still holds the security guard.
	// A revoke cannot fall between admission and registration of the live host.
	runCtx, cancel := context.WithCancel(ctx)
	lockCtx, held, err := m.d.Gate.Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		cancel()
		return a, err
	}
	err = m.probeCurrent(lockCtx, e, epoch)
	if err == nil {
		m.track(e.ID, a.ProbeID, cancel)
	}
	held.Release()
	if err != nil {
		cancel()
		return a, err
	}
	defer func() { cancel(); m.untrack(e.ID, a.ProbeID) }()
	res, err := RunOneShot(runCtx, RunSpec{Package: pkg, Files: fsys, Target: "server", Job: job, Deadline: deadline, BaseDir: m.d.BaseDir, MaxOutputFiles: 0, ValidateResult: validateProcessorResultJSON})
	obs := res.Observation
	a.Observation = &obs
	a.State, a.Failure = "probe_failed", res.Failure
	switch {
	case err != nil:
		a.Failure = "probe_not_dispatched"
	case execution.ClassifyInvocation(obs) != execution.InvocationCompleted:
		if a.Failure == "" {
			a.Failure = string(execution.ClassifyInvocation(obs))
		}
	default:
		var out ProcessorResult
		if json.Unmarshal(res.Result, &out) != nil || out.OperationID != in.OperationID || out.Producer != in.Producer || out.Status != "completed" || len(out.Checks) != 0 || len(out.Files) != 0 || len(out.Records) != 0 {
			a.Failure = "probe_result_invalid"
		} else {
			a.State, a.Failure = "ready", ""
		}
	}
	// Runtime observations are historical. Only current main authority may
	// accept a ready probe, serialized with revocation and review withdrawal.
	finalCtx, held, err := m.d.Gate.Acquire(context.WithoutCancel(ctx), commands.Request{Security: commands.ModeShared})
	if err != nil {
		return a, err
	}
	defer held.Release()
	if currentErr := m.probeCurrent(finalCtx, e, epoch); currentErr != nil {
		a.State, a.Failure = "probe_failed", "activation_changed"
	}
	if serr := m.writeActivation(finalCtx, &a); serr != nil {
		return a, serr
	}
	if a.State != "ready" {
		return a, failure(errcode.ExtensionActivationStale, "probe_failed:"+a.Failure)
	}
	return a, nil
}

// probeCurrent never treats an old runtime observation as authority.
func (m *Manager) probeCurrent(ctx context.Context, e Enablement, epoch int64) error {
	current, err := m.enablement(ctx, m.d.Main, e.ID)
	if err != nil {
		return err
	}
	if current == nil || current.State != "enabled" || !current.ProbeAuthorized || current.Generation != e.Generation || current.snapshot() != e.snapshot() || m.environmentDigest(*current) != m.environmentDigest(e) {
		return failure(errcode.ExtensionActivationStale, "activation_changed")
	}
	review, err := m.d.Packages.PackageReview(ctx, e.ReviewRef)
	if err != nil {
		return err
	}
	if !review.Approved || review.ReviewID != e.ReviewID {
		return failure(errcode.ExtensionActivationStale, "package_review_revoked")
	}
	currentEpoch, err := m.d.Authority.RecoveryEpoch(ctx)
	if err != nil {
		return err
	}
	if currentEpoch != epoch {
		return failure(errcode.ExtensionActivationStale, "recovery_epoch_changed")
	}
	return nil
}

// admission is every authoritative check before a dispatch or acceptance:
// enablement, package review, project allowlist, probe environment and trust.
type admission struct {
	enablement Enablement
	record     PackageRecord
}

func (m *Manager) allowed(ctx context.Context, project ids.ID, extension string) error {
	p, err := m.d.Policies.ResolvePolicies(ctx, project)
	if err != nil {
		return err
	}
	var list []string
	if v, ok := p["plugins.allowed"]; ok {
		if err = json.Unmarshal(v.Value, &list); err != nil {
			return err
		}
	}
	if !slices.Contains(list, extension) {
		return failure(errcode.Forbidden, "plugin_not_allowed_in_project")
	}
	return nil
}

// resolve finds the server enablement for a contribution in a project: a
// project-scoped enablement wins over an instance-scoped one. Instance
// enablement never implies project permission; the allowlist is checked too.
func (m *Manager) resolve(ctx context.Context, project ids.ID, contribution string) (admission, error) {
	list, err := m.listEnablements(ctx, "server", "enabled")
	if err != nil {
		return admission{}, err
	}
	var pick *Enablement
	for i := range list {
		e := &list[i]
		if !strings.HasPrefix(contribution, e.ExtensionID+".") || e.ScopeKind == "project" && e.ScopeID != project {
			continue
		}
		if pick == nil || e.ScopeKind == "project" {
			pick = e
		}
	}
	if pick == nil {
		return admission{}, failure(errcode.UnsupportedCapability, "processor_not_enabled")
	}
	return m.check(ctx, project, *pick, contribution)
}

func (m *Manager) check(ctx context.Context, project ids.ID, e Enablement, contribution string) (admission, error) {
	rec, err := m.packageRecord(ctx, m.d.Main, e.ExtensionID, e.ExtensionVersion)
	if err != nil {
		return admission{}, err
	}
	c, ok := rec.Manifest.Contribution(contribution)
	if !ok || c.Point != "asset.validator" || c.Target != e.Target {
		return admission{}, failure(errcode.ExtensionPointUnsupported, "contribution_not_in_package")
	}
	if err = m.allowed(ctx, project, e.ExtensionID); err != nil {
		return admission{}, err
	}
	review, err := m.d.Packages.PackageReview(ctx, rec.Ref)
	if err != nil {
		return admission{}, err
	}
	if !review.Approved || review.ReviewID != e.ReviewID {
		return admission{}, failure(errcode.ExtensionActivationStale, "package_review_revoked")
	}
	if !slices.Equal(m.caps.Unenforced(rec.Manifest), e.Unenforced) || len(e.Unenforced) > 0 && e.Trust != TrustUnenforced {
		return admission{}, failure(errcode.ExtensionPointUnsupported, "isolation_insufficient")
	}
	return admission{enablement: e, record: rec}, nil
}

func (m *Manager) ready(ctx context.Context, e Enablement) error {
	act, err := m.activation(ctx, e.ID, e.Generation)
	if err != nil {
		return err
	}
	switch {
	case act == nil || act.State == "pending_probe":
		return failure(errcode.ExtensionActivationStale, "probe_required")
	case act.State != "ready":
		return failure(errcode.ExtensionActivationStale, "probe_failed")
	case act.EnvironmentDigest != m.environmentDigest(e):
		return failure(errcode.ExtensionActivationStale, "probe_stale")
	}
	return nil
}

func isBuiltin(extension string) bool {
	builtins, err := builtinPackages()
	if err != nil {
		return false
	}
	for _, b := range builtins {
		if b.Manifest.ID == extension {
			return true
		}
	}
	return false
}

// Snapshot resolves the processor for a job without side effects. Builtin
// checks keep their compiled release binding; packages need a ready enablement.
func (m *Manager) Snapshot(ctx context.Context, project ids.ID, processor string, epoch int64) (ae.ActivationSnapshot, storage.Producer, error) {
	if processor == "org.lantai.corecheck.manifest" {
		return m.d.Registry.Snapshot(ctx, project, processor, epoch)
	}
	a, err := m.resolve(ctx, project, processor)
	if err != nil {
		return ae.ActivationSnapshot{}, storage.Producer{}, err
	}
	if err = m.ready(ctx, a.enablement); err != nil {
		return ae.ActivationSnapshot{}, storage.Producer{}, err
	}
	return a.enablement.snapshot(), a.enablement.producer(processor), nil
}

// Run dispatches one job attempt. For a package it rechecks admission, takes
// a breaker slot, spawns the verified entry once and settles the breaker. The
// attempt ID is the invocation ID, so repeats and late callbacks count once.
func (m *Manager) Run(ctx context.Context, project ids.ID, in ProcessorInput, raw []byte) (InvocationResult, error) {
	if in.Producer.Source == "builtin_release" {
		return m.d.Registry.RunBuiltin(ctx, in, raw)
	}
	var out InvocationResult
	if err := validate("lantai.processor-input/v1", in); err != nil {
		return out, err
	}
	// Admission is serialized with enable/disable (exclusive security), so a
	// call is either admitted before a retirement or sees the new generation.
	lockCtx, h, err := m.d.Gate.Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return out, err
	}
	a, deadline, err := m.admitRun(lockCtx, project, in, raw)
	runCtx, cancel := context.WithCancel(ctx)
	invocation := in.Fence.JobAttemptID
	if err == nil {
		m.track(a.enablement.ID, invocation, cancel)
	}
	h.Release()
	if err != nil {
		cancel()
		return out, err
	}
	e := a.enablement
	limits := a.record.Manifest.ResourceLimits
	defer func() { cancel(); m.untrack(e.ID, invocation) }()
	pkg, fsys, err := m.readPackage(ctx, a.record)
	if err != nil {
		return out, errors.Join(err, m.settle(context.WithoutCancel(ctx), invocation, execution.InvocationNotDispatched))
	}
	in.Config = e.Config
	job, err := json.Marshal(in)
	if err != nil {
		return out, errors.Join(err, m.settle(context.WithoutCancel(ctx), invocation, execution.InvocationNotDispatched))
	}
	res, err := RunOneShot(runCtx, RunSpec{Package: pkg, Files: fsys, Target: "server", Job: job, Inputs: []InputFile{{Name: "manifest.yaml", Data: raw}}, Deadline: deadline, BaseDir: m.d.BaseDir, MaxInputBytes: limits.MaxInputBytes, MaxOutputBytes: limits.MaxOutputBytes, MaxOutputFiles: 0, ValidateResult: validateProcessorResultJSON})
	out.Observation = res.Observation
	if err != nil {
		return out, errors.Join(err, m.settle(context.WithoutCancel(ctx), invocation, execution.InvocationNotDispatched))
	}
	if out.Observation.ResultValid {
		out.Observation.ResultValid = false
		var r ProcessorResult
		if json.Unmarshal(res.Result, &r) == nil && r.OperationID == in.OperationID && r.Producer == in.Producer && len(r.Files) == 0 && len(r.Records) == 0 && validChecks(r, in) {
			out.Observation.ResultValid = true
			out.Result = &r
		}
	}
	return out, m.settle(context.WithoutCancel(ctx), invocation, execution.ClassifyInvocation(out.Observation))
}

func (m *Manager) admitRun(ctx context.Context, project ids.ID, in ProcessorInput, raw []byte) (admission, time.Time, error) {
	a, err := m.resolve(ctx, project, in.Producer.ContributionID)
	if err != nil {
		return a, time.Time{}, err
	}
	e := a.enablement
	if e.snapshot() != in.Activation || e.producer(in.Producer.ContributionID) != in.Producer {
		return a, time.Time{}, failure(errcode.ExtensionActivationStale, "activation_changed_before_dispatch")
	}
	if err = m.ready(ctx, e); err != nil {
		return a, time.Time{}, err
	}
	limits := a.record.Manifest.ResourceLimits
	if len(in.InputRefs) != 1 || int64(len(raw)) > limits.MaxInputBytes {
		return a, time.Time{}, failure(errcode.QuotaExceeded, "processor_input_limit")
	}
	deadline, err := clock.Parse(in.Deadline)
	if err != nil {
		return a, time.Time{}, err
	}
	deadline = minTime(deadline, time.Now().Add(time.Duration(limits.TimeoutSeconds)*time.Second))
	return a, deadline, m.admit(ctx, e, in.Fence.JobAttemptID, project, in.Producer.ContributionID, deadline)
}

// validChecks accepts one check for the fixed input, or a legal "unsupported".
func validChecks(r ProcessorResult, in ProcessorInput) bool {
	if r.Status == "unsupported" {
		return len(r.Checks) == 0
	}
	if len(r.Checks) != 1 {
		return false
	}
	c := r.Checks[0]
	return !slices.Contains([]string{"integrity", "license_evidence", "purpose", "identity", "current_use", "authorization", "target"}, c.CheckKey) && c.Ref == in.InputRefs[0] && c.ManifestDigest == in.InputDigest && c.Producer == in.Producer
}

func (m *Manager) track(enablement, invocation ids.ID, cancel context.CancelFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running[enablement] == nil {
		m.running[enablement] = map[ids.ID]context.CancelFunc{}
	}
	m.running[enablement][invocation] = cancel
}

func (m *Manager) untrack(enablement, invocation ids.ID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running[enablement], invocation)
}

func (m *Manager) cancelRunning(enablement ids.ID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, cancel := range m.running[enablement] {
		cancel()
	}
}

// CheckSnapshot is the final-acceptance gate for a result. A builtin keeps its
// release binding. A package result is accepted only when its generation is
// current, or was retired by a normal drain after admission and the frozen
// deadline has not passed; revocation, review withdrawal or allowlist removal
// always reject, regardless of runtime observation lag.
func (m *Manager) CheckSnapshot(ctx context.Context, project ids.ID, s ae.ActivationSnapshot, invocation ids.ID, epoch int64) error {
	return m.checkSnapshot(ctx, project, s, invocation, epoch, true)
}

// CheckCurrentSnapshot is the gate for completed evidence consumed by a new
// review or publication. Draining grants only an admitted invocation permission
// to finish; it never makes a retired generation current for a new decision.
func (m *Manager) CheckCurrentSnapshot(ctx context.Context, project ids.ID, s ae.ActivationSnapshot, invocation ids.ID, epoch int64) error {
	return m.checkSnapshot(ctx, project, s, invocation, epoch, false)
}

func (m *Manager) checkSnapshot(ctx context.Context, project ids.ID, s ae.ActivationSnapshot, invocation ids.ID, epoch int64, allowDrain bool) error {
	if isBuiltin(s.Activation.ExtensionID) {
		return m.d.Registry.CheckBuiltinSnapshot(ctx, s, epoch)
	}
	inv, err := m.invocation(ctx, invocation)
	if err != nil {
		return err
	}
	if inv.Generation != s.Activation.Generation {
		return failure(errcode.ExtensionActivationStale, "invocation_generation_mismatch")
	}
	defined, retired, err := m.generationRecord(ctx, inv.EnablementID, inv.Generation)
	if err != nil {
		return err
	}
	if defined.snapshot() != s {
		return failure(errcode.ExtensionActivationStale, "snapshot_mismatch")
	}
	current, err := m.enablement(ctx, m.d.Main, inv.EnablementID)
	if err != nil {
		return err
	}
	if current == nil {
		return failure(errcode.ExtensionActivationStale, "enablement_missing")
	}
	if current.State != "enabled" || current.Generation != inv.Generation {
		if !allowDrain {
			return failure(errcode.ExtensionActivationStale, "activation_retired")
		}
		now := m.d.Clock.Now()
		if retired == nil || retired.Mode != "drain" {
			return failure(errcode.ExtensionActivationStale, "activation_revoked")
		}
		at, err := clock.Parse(retired.At)
		if err != nil || clock.FromMillis(inv.AdmittedAt).After(at) || now.After(clock.FromMillis(inv.Deadline)) {
			return failure(errcode.ExtensionActivationStale, "drain_expired")
		}
	}
	if !allowDrain {
		if current.snapshot() != s {
			return failure(errcode.ExtensionActivationStale, "snapshot_mismatch")
		}
		if err = m.ready(ctx, *current); err != nil {
			return err
		}
	}
	if _, err = m.check(ctx, project, defined, inv.Contribution); err != nil {
		return err
	}
	return nil
}

// Settle records the outcome of an invocation whose stop was unresolved, after
// T06 reconciliation confirmed how it ended.
func (m *Manager) Settle(ctx context.Context, invocation ids.ID, outcome execution.InvocationOutcome) error {
	if _, err := m.invocation(ctx, invocation); err != nil {
		if errcode.CodeOf(err) == errcode.NotFound {
			return nil // builtin calls have no breaker bookkeeping
		}
		return err
	}
	return m.settle(ctx, invocation, outcome)
}

// CLICommand is an enabled local command visible to the caller. It is data for
// an explicitly installed local registry; it grants no token or HumanGrant.
type CLICommand struct {
	EnablementID     ids.ID           `json:"enablement_id"`
	Generation       int64            `json:"generation"`
	ExtensionID      string           `json:"extension_id"`
	ExtensionVersion string           `json:"extension_version"`
	PackageDigest    digest.Digest    `json:"package_digest"`
	ContributionID   string           `json:"contribution_id"`
	Command          string           `json:"command"`
	Entry            string           `json:"entry"`
	EntryDigest      digest.Digest    `json:"entry_digest"`
	Config           json.RawMessage  `json:"config"`
	ConfigDigest     digest.Digest    `json:"config_digest"`
	Producer         storage.Producer `json:"producer"`
}

// CLICommands lists cli.command contributions enabled for the caller: an
// instance scope or the caller's own user scope, with a current review.
func (m *Manager) CLICommands(ctx context.Context, who authz.Context) ([]CLICommand, error) {
	if err := m.authorize(ctx, who, identity.ActListExtensionCLIs); err != nil {
		return nil, err
	}
	list, err := m.listEnablements(ctx, "cli", "enabled")
	if err != nil {
		return nil, err
	}
	out := []CLICommand{}
	for _, e := range list {
		if e.ScopeKind == "user" && e.ScopeID != who.PrincipalID {
			continue
		}
		rec, err := m.packageRecord(ctx, m.d.Main, e.ExtensionID, e.ExtensionVersion)
		if err != nil {
			return nil, err
		}
		review, err := m.d.Packages.PackageReview(ctx, rec.Ref)
		if err != nil {
			return nil, err
		}
		if !review.Approved || review.ReviewID != e.ReviewID {
			continue
		}
		for _, c := range rec.Manifest.Contributes {
			if c.Point != "cli.command" || c.Target != "cli" {
				continue
			}
			out = append(out, CLICommand{EnablementID: e.ID, Generation: e.Generation, ExtensionID: e.ExtensionID, ExtensionVersion: e.ExtensionVersion, PackageDigest: e.PackageDigest, ContributionID: c.ID, Command: strings.TrimPrefix(c.ID, e.ExtensionID+"."), Entry: e.Entry, EntryDigest: e.EntryDigest, Config: e.Config, ConfigDigest: e.ConfigDigest, Producer: e.producer(c.ID)})
		}
	}
	return out, nil
}

// PackageUse is an enabled package version that must not be trashed.
type PackageUse struct {
	EnablementID ids.ID
	Revision     int64
	Ref          ids.PermanentRef
}

// PackageUses reports enabled enablements referencing these versions, so the
// ledger lifecycle keeps referenced package bytes (reference retention).
func (m *Manager) PackageUses(ctx context.Context, refs []ids.PermanentRef) ([]PackageUse, error) {
	list, err := m.listEnablements(ctx, "", "enabled")
	if err != nil {
		return nil, err
	}
	out := []PackageUse{}
	for _, e := range list {
		for _, r := range refs {
			if e.ReviewRef.AssetID == r.AssetID && (r.VersionID == "" || e.ReviewRef.VersionID == r.VersionID) {
				out = append(out, PackageUse{EnablementID: e.ID, Revision: e.Revision, Ref: e.ReviewRef})
			}
		}
	}
	return out, nil
}
