package execution

import (
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestEqualMalformedExecutionCredentialsNeverPass(t *testing.T) {
	attempt := ids.New()
	for _, bad := range []Fence{
		{},
		{AttemptID: "invalid", LeaseFence: 1, RecoveryEpoch: 1},
		{AttemptID: attempt, LeaseFence: 0, RecoveryEpoch: 1},
		{AttemptID: attempt, LeaseFence: -1, RecoveryEpoch: 1},
		{AttemptID: attempt, LeaseFence: 1, RecoveryEpoch: 0},
		{AttemptID: attempt, LeaseFence: 9007199254740992, RecoveryEpoch: 1},
		{AttemptID: attempt, LeaseFence: 1, RecoveryEpoch: 9007199254740992},
		{TaskID: "not-a-task", AttemptID: attempt, LeaseFence: 1, RecoveryEpoch: 1},
	} {
		current := LeaseState{AttemptID: bad.AttemptID, LeaseFence: bad.LeaseFence, RecoveryEpoch: bad.RecoveryEpoch, ExpiresAt: now.Add(time.Hour)}
		if errcode.CodeOf(CheckFence(bad, current, now)) != errcode.LeaseStale {
			t.Fatalf("matching malformed fence was accepted: %+v", bad)
		}
	}
	good := Activation{ExtensionID: "org.example.check", ExtensionVersion: "1.0.0", PackageDigest: digest.Of([]byte("package")), Generation: 1}
	for _, change := range []func(*Activation){
		func(a *Activation) { *a = Activation{} },
		func(a *Activation) { a.ExtensionID = "not namespaced" },
		func(a *Activation) { a.ExtensionVersion = "one" },
		func(a *Activation) { a.PackageDigest = "sha256:short" },
		func(a *Activation) { a.Generation = 0 },
		func(a *Activation) { a.Generation = 9007199254740992 },
	} {
		bad := good
		change(&bad)
		if errcode.CodeOf(CheckActivation(bad, ActivationState{Current: bad, Enabled: true}, now)) != errcode.ExtensionActivationStale {
			t.Fatalf("matching malformed activation was accepted: %+v", bad)
		}
		if errcode.CodeOf(CheckActivation(bad, ActivationState{Current: good, Draining: []Draining{{Activation: bad, Deadline: now.Add(time.Hour)}}}, now)) != errcode.ExtensionActivationStale {
			t.Fatal("draining bypassed activation shape checks")
		}
	}
}

func TestEqualInvalidEpochsAndHashesNeverPass(t *testing.T) {
	for subject, code := range epochCodes {
		for _, bad := range []int64{-1, 0, 9007199254740992} {
			if errcode.CodeOf(CheckRecoveryEpoch(subject, bad, bad)) != code {
				t.Fatalf("matching invalid epoch accepted: %s %d", subject, bad)
			}
		}
	}
	if errcode.CodeOf(CheckRecoveryEpoch(Subject("unknown"), 1, 1)) != errcode.Forbidden {
		t.Fatal("unknown subject accepted through the equality branch")
	}
	for _, bad := range []digest.Digest{"", "sha256:short", "md5:unsupported"} {
		if errcode.CodeOf(CheckOperation(bad, bad)) != errcode.IdempotencyConflict {
			t.Fatal("equal invalid operation hashes passed")
		}
		if errcode.CodeOf(CheckStartKey(bad, bad)) != errcode.StartKeyConflict {
			t.Fatal("equal invalid start hashes passed")
		}
	}
}
