package execution

import (
	"encoding/json"
	"io/fs"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/schemas"
)

// Go 枚举必须与 schema 枚举逐项一致，schema 是唯一来源。
func TestEnumsMatchSchema(t *testing.T) {
	raw, err := fs.ReadFile(schemas.FS, "common/v1/execution.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs map[string]struct {
			Enum []string `json:"enum"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	check := func(def string, got []string) {
		t.Helper()
		want := slices.Clone(doc.Defs[def].Enum)
		got = slices.Clone(got)
		sort.Strings(want)
		sort.Strings(got)
		if len(want) == 0 || !slices.Equal(want, got) {
			t.Errorf("%s: schema %v, Go %v", def, want, got)
		}
	}
	check("protocol_id", strs(Protocols))
	check("execution_action", strs(ExecutionActions))
	check("host_control_action", strs(HostControlActions))
	check("task_run_state", strs(TaskRunStates))
	check("instance_health", strs(InstanceHealthStates))
	check("effect_class", strs(EffectClasses))
	check("effect_state", strs(EffectStates))
	check("resume_class", strs(ResumeClasses))
	check("cancel_mode", strs(CancelModes))
	check("invocation_outcome", strs(InvocationOutcomes))
	check("check_verdict", strs(CheckVerdicts))
}

func strs[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

func TestActionFamiliesAreSeparate(t *testing.T) {
	if _, err := ParseExecutionAction("start"); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"health", "drain", "negotiate", "configure", "stop"} {
		if _, err := ParseExecutionAction(a); err == nil {
			t.Errorf("host control action %q accepted by the execution protocol", a)
		}
	}
	for _, a := range []string{"start", "lookup", "resume", "result", "cancel"} {
		if _, err := ParseHostControlAction(a); err == nil {
			t.Errorf("execution action %q accepted by host control", a)
		}
	}
	if ControlActions(Processor) != nil {
		t.Fatal("the oneshot processor protocol has no control actions")
	}
	if len(ControlActions(AgentExecution)) != 7 || len(ControlActions(HostControl)) != 6 {
		t.Fatal("control action lists")
	}
	for _, s := range SupportMatrix() {
		if s.Status != "enabled" && s.Status != "reserved" {
			t.Errorf("%s reports %s; unexpected build capability", s.Protocol, s.Status)
		}
	}
}

var now = time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)

func reasonOf(t *testing.T, err error, code errcode.Code) string {
	t.Helper()
	e, ok := errcode.As(err)
	if !ok || e.Code != code || len(e.Details) == 0 {
		t.Fatalf("want %s with details, got %v", code, err)
	}
	return e.Details[0].Reason
}

func TestCheckFence(t *testing.T) {
	attempt := ids.New()
	lease := LeaseState{AttemptID: attempt, LeaseFence: 4, RecoveryEpoch: 2, ExpiresAt: now.Add(time.Minute)}
	good := Fence{AttemptID: attempt, LeaseFence: 4, RecoveryEpoch: 2}
	if err := CheckFence(good, lease, now); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		f      Fence
		l      LeaseState
		at     time.Time
		reason string
	}{
		// 恢复后 fence 数值可能重复：只比较数值会误放行旧执行权。
		"recovered with same fence": {Fence{AttemptID: attempt, LeaseFence: 4, RecoveryEpoch: 1}, lease, now, ReasonRecoveryEpoch},
		"new attempt":               {Fence{AttemptID: ids.New(), LeaseFence: 4, RecoveryEpoch: 2}, lease, now, ReasonAttemptReplaced},
		"old fence":                 {Fence{AttemptID: attempt, LeaseFence: 3, RecoveryEpoch: 2}, lease, now, ReasonFenceMismatch},
		"terminated":                {good, LeaseState{AttemptID: attempt, LeaseFence: 4, RecoveryEpoch: 2, ExpiresAt: now.Add(time.Minute), Terminated: true}, now, ReasonAttemptEnded},
		"expiry boundary":           {good, lease, now.Add(time.Minute), ReasonLeaseExpired},
	}
	for name, c := range cases {
		if got := reasonOf(t, CheckFence(c.f, c.l, c.at), errcode.LeaseStale); got != c.reason {
			t.Errorf("%s: reason %s, want %s", name, got, c.reason)
		}
	}
}

func TestCheckActivation(t *testing.T) {
	v1 := Activation{ExtensionID: "org.example.asset-tools", ExtensionVersion: "0.1.0", PackageDigest: digest.Of([]byte("v1")), Generation: 7}
	v2 := Activation{ExtensionID: "org.example.asset-tools", ExtensionVersion: "0.2.0", PackageDigest: digest.Of([]byte("v2")), Generation: 8}
	upgraded := ActivationState{Current: v2, Enabled: true, Draining: []Draining{{Activation: v1, Deadline: now.Add(time.Minute)}}}
	if err := CheckActivation(v2, upgraded, now); err != nil {
		t.Fatalf("current activation: %v", err)
	}
	if err := CheckActivation(v1, upgraded, now); err != nil {
		t.Fatalf("draining activation within its deadline: %v", err)
	}
	swapped := v2
	swapped.PackageDigest = digest.Of([]byte("same version, different bytes"))
	cases := map[string]struct {
		a      Activation
		s      ActivationState
		at     time.Time
		reason string
	}{
		"drain expired":       {v1, upgraded, now.Add(time.Minute), ReasonDrainExpired},
		"not draining":        {v1, ActivationState{Current: v2, Enabled: true}, now, ReasonGenerationStale},
		"revoked":             {v2, ActivationState{Current: v2, Enabled: true, Revoked: true}, now, ReasonRevoked},
		"revoked drains none": {v1, ActivationState{Current: v2, Enabled: true, Revoked: true, Draining: upgraded.Draining}, now, ReasonRevoked},
		"disabled":            {v2, ActivationState{Current: v2}, now, ReasonDisabled},
		"digest swapped":      {swapped, upgraded, now, ReasonPackageMismatch},
	}
	for name, c := range cases {
		if got := reasonOf(t, CheckActivation(c.a, c.s, c.at), errcode.ExtensionActivationStale); got != c.reason {
			t.Errorf("%s: reason %s, want %s", name, got, c.reason)
		}
	}
	// 正常停用：当前代次进入排空名单，在途调用在期限内可以收尾。
	disabled := ActivationState{Current: v2, Draining: []Draining{{Activation: v2, Deadline: now.Add(time.Minute)}}}
	if err := CheckActivation(v2, disabled, now); err != nil {
		t.Fatalf("disabled generation within its drain deadline: %v", err)
	}
	if got := reasonOf(t, CheckActivation(v2, disabled, now.Add(time.Minute)), errcode.ExtensionActivationStale); got != ReasonDrainExpired {
		t.Fatalf("disabled generation after drain deadline: %s", got)
	}
	revokedWhileDraining := disabled
	revokedWhileDraining.Revoked = true
	if got := reasonOf(t, CheckActivation(v2, revokedWhileDraining, now), errcode.ExtensionActivationStale); got != ReasonRevoked {
		t.Fatalf("revocation must stop draining calls too: %s", got)
	}
}

// 最终接受边界缺少时间时必须拒绝，不能因零值时间放行过期租约或排空期限。
func TestZeroTimeFailsClosed(t *testing.T) {
	attempt := ids.New()
	expired := LeaseState{AttemptID: attempt, LeaseFence: 1, RecoveryEpoch: 1, ExpiresAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	fence := Fence{AttemptID: attempt, LeaseFence: 1, RecoveryEpoch: 1}
	if errcode.CodeOf(CheckFence(fence, expired, time.Time{})) != errcode.Internal {
		t.Fatal("zero time must not accept an expired lease")
	}
	act := Activation{ExtensionID: "org.example.asset-tools", ExtensionVersion: "0.1.0", PackageDigest: digest.Of([]byte("p")), Generation: 1}
	state := ActivationState{Current: Activation{Generation: 2}, Enabled: true,
		Draining: []Draining{{Activation: act, Deadline: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}}}
	if errcode.CodeOf(CheckActivation(act, state, time.Time{})) != errcode.Internal {
		t.Fatal("zero time must not accept an expired drain")
	}
	if Accept(Acceptance{Fence: &fence, Lease: &expired}) == nil {
		t.Fatal("Accept without Now must fail")
	}
}

func TestAcceptChecksBothCredentials(t *testing.T) {
	attempt := ids.New()
	fence := Fence{AttemptID: attempt, LeaseFence: 1, RecoveryEpoch: 1}
	lease := LeaseState{AttemptID: attempt, LeaseFence: 1, RecoveryEpoch: 1, ExpiresAt: now.Add(time.Hour)}
	act := Activation{ExtensionID: "org.example.asset-tools", ExtensionVersion: "0.1.0", PackageDigest: digest.Of([]byte("p")), Generation: 3}
	state := ActivationState{Current: act, Enabled: true}
	if err := Accept(Acceptance{Now: now, Fence: &fence, Lease: &lease, Activation: &act, ActivationState: &state}); err != nil {
		t.Fatal(err)
	}
	if err := Accept(Acceptance{Now: now, Fence: &fence, Lease: &lease}); err != nil {
		t.Fatalf("results without an extension only check the fence: %v", err)
	}
	stale := state
	stale.Current.Generation = 4
	stale.Current.PackageDigest = digest.Of([]byte("p2"))
	if errcode.CodeOf(Accept(Acceptance{Now: now, Fence: &fence, Lease: &lease, Activation: &act, ActivationState: &stale})) != errcode.ExtensionActivationStale {
		t.Fatal("a valid fence must not excuse a stale activation")
	}
	old := fence
	old.LeaseFence = 0
	if errcode.CodeOf(Accept(Acceptance{Now: now, Fence: &old, Lease: &lease, Activation: &act, ActivationState: &state})) != errcode.LeaseStale {
		t.Fatal("a valid activation must not excuse a stale fence")
	}
	if errcode.CodeOf(Accept(Acceptance{Now: now})) != errcode.LeaseStale {
		t.Fatal("results without a fence must be rejected")
	}
	if errcode.CodeOf(Accept(Acceptance{Now: now, Fence: &fence, Lease: &lease, Activation: &act})) != errcode.ExtensionActivationStale {
		t.Fatal("an activation without its current state cannot be accepted")
	}
}

func TestRecoveryEpochMapping(t *testing.T) {
	want := map[Subject]errcode.Code{
		SubjectSession:    errcode.TokenRevoked,
		SubjectAttempt:    errcode.LeaseStale,
		SubjectHumanGrant: errcode.HumanProofRequired,
		SubjectBlobGrant:  errcode.BlobGrantRequired,
		SubjectReadGrant:  errcode.Forbidden,
		SubjectActivation: errcode.ExtensionActivationStale,
		SubjectOperation:  errcode.OperationNeedsReconciliation,
	}
	for s, code := range want {
		if err := CheckRecoveryEpoch(s, 2, 2); err != nil {
			t.Errorf("%s same epoch: %v", s, err)
		}
		if got := errcode.CodeOf(CheckRecoveryEpoch(s, 1, 2)); got != code {
			t.Errorf("%s: %s, want %s", s, got, code)
		}
		if c, ok := EpochCode(s); !ok || c != code {
			t.Errorf("EpochCode(%s) = %s", s, c)
		}
	}
	a, b := digest.Of([]byte("a")), digest.Of([]byte("b"))
	if CheckOperation(a, a) != nil || errcode.CodeOf(CheckOperation(a, b)) != errcode.IdempotencyConflict {
		t.Fatal("operation hash check")
	}
	if CheckStartKey(a, a) != nil || errcode.CodeOf(CheckStartKey(a, b)) != errcode.StartKeyConflict {
		t.Fatal("start key check")
	}
}

func TestStatesStaySeparate(t *testing.T) {
	// 业务执行结果、宿主健康、审定结论互不相通。
	if TaskRunState("ready").Valid() || TaskRunState("approved").Valid() {
		t.Fatal("health or review values leaked into TaskRun states")
	}
	if InstanceHealth("execution_succeeded").Valid() || InstanceHealth("approved").Valid() {
		t.Fatal("execution or review values leaked into instance health")
	}
	if RunNeedsReconciliation.Terminal() || RunCancelling.Terminal() || !RunExecutionSucceeded.Terminal() {
		t.Fatal("terminal classification")
	}
	if !HealthReady.Dispatchable() || HealthDraining.Dispatchable() {
		t.Fatal("dispatchable classification")
	}
	cases := []struct {
		confirmed, expired bool
		effects            int
		want               TaskRunState
	}{
		{true, false, 0, RunCancelled},
		{true, true, 0, RunCancelled},
		{true, false, 1, RunNeedsReconciliation},
		{false, true, 0, RunNeedsReconciliation},
		{false, false, 0, RunCancelling},
		{false, false, 2, RunCancelling},
	}
	for _, c := range cases {
		if got := ResolveCancel(c.confirmed, c.expired, c.effects); got != c.want {
			t.Errorf("ResolveCancel(%v,%v,%d) = %s, want %s", c.confirmed, c.expired, c.effects, got, c.want)
		}
	}
}

func TestRecoverDecisions(t *testing.T) {
	type key struct {
		c EffectClass
		s EffectState
	}
	want := map[key]RetryDecision{
		{EffectExternalNonIdempotent, EffectUnknown}:     DecisionBlocked,
		{EffectExternalNonIdempotent, EffectDispatched}:  DecisionBlocked,
		{EffectExternalQueryable, EffectUnknown}:         DecisionReconcileFirst,
		{EffectExternalIdempotent, EffectUnknown}:        DecisionReuseKey,
		{EffectLantaiCommand, EffectDispatched}:          DecisionReuseKey,
		{EffectPureRead, EffectUnknown}:                  DecisionRerun,
		{EffectLocalReplaceable, EffectDispatched}:       DecisionRerun,
		{EffectExternalNonIdempotent, EffectNotExecuted}: DecisionRerun,
		{EffectExternalNonIdempotent, EffectIntended}:    DecisionRerun,
		{EffectExternalIdempotent, EffectIntended}:       DecisionReuseKey,
		{EffectExternalQueryable, EffectCompleted}:       DecisionDone,
	}
	for k, w := range want {
		if got := Recover(k.c, k.s); got != w {
			t.Errorf("Recover(%s, %s) = %s, want %s", k.c, k.s, got, w)
		}
	}
	// 结果不明时，只有纯读/可替换产物允许直接重跑。
	for _, c := range EffectClasses {
		d := Recover(c, EffectUnknown)
		if d == DecisionRerun && c != EffectPureRead && c != EffectLocalReplaceable {
			t.Errorf("%s with unknown effect must not simply rerun", c)
		}
	}
}

func TestInvocationOutcomeSeparatesFaultsFromLegitimateFail(t *testing.T) {
	done := Invocation{Dispatched: true, StopConfirmed: true, Exited: true, ExitCode: 0, ResultValid: true}
	cases := []struct {
		name  string
		in    Invocation
		want  InvocationOutcome
		fault bool
	}{
		{"completed (pass or legitimate fail)", done, InvocationCompleted, false},
		{"rejected before dispatch", Invocation{}, InvocationNotDispatched, false},
		{"cancelled and stopped", Invocation{Dispatched: true, StopConfirmed: true, Cancelled: true}, InvocationCancelled, false},
		{"cancelled but stop unknown", Invocation{Dispatched: true, Cancelled: true}, InvocationUnresolved, false},
		{"timeout", Invocation{Dispatched: true, StopConfirmed: true, TimedOut: true}, InvocationRuntimeFault, true},
		{"non-zero exit with a valid result", Invocation{Dispatched: true, StopConfirmed: true, Exited: true, ExitCode: 1, ResultValid: true}, InvocationRuntimeFault, true},
		{"exit 0 without result", Invocation{Dispatched: true, StopConfirmed: true, Exited: true}, InvocationRuntimeFault, true},
		{"killed without exit code", Invocation{Dispatched: true, StopConfirmed: true}, InvocationRuntimeFault, true},
	}
	for _, c := range cases {
		got := ClassifyInvocation(c.in)
		if got != c.want || got.CountsAsFault() != c.fault {
			t.Errorf("%s: %s (fault=%v), want %s (fault=%v)", c.name, got, got.CountsAsFault(), c.want, c.fault)
		}
	}
	if Verdict(InvocationCompleted, VerdictFail) != VerdictFail || Verdict(InvocationCompleted, VerdictPass) != VerdictPass {
		t.Fatal("a completed run keeps its business verdict")
	}
	for _, o := range []InvocationOutcome{InvocationRuntimeFault, InvocationNotDispatched, InvocationCancelled, InvocationUnresolved} {
		if Verdict(o, VerdictPass) != VerdictUnknown {
			t.Errorf("%s must never yield pass", o)
		}
	}
	if Verdict(InvocationCompleted, VerdictUnknown) != VerdictUnknown {
		t.Fatal("unsupported input reported as unknown stays unknown")
	}
}
