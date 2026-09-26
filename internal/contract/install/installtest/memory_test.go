package installtest

import (
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/install"
)

func TestMemoryInstallerSatisfiesContract(t *testing.T) {
	RunInstallerContract(t, func(t *testing.T) Harness {
		m := New(clock.NewFake(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)))
		return Harness{Installer: m, Put: m.PutBlob, Corrupt: m.Corrupt}
	})
}

func TestFailNextInjectsStorageErrors(t *testing.T) {
	m := New(clock.NewFake(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)))
	req := request(Harness{Put: m.PutBlob})
	m.FailNext(errcode.New(errcode.StorageFull, ""))
	if _, err := m.Install(t.Context(), req); errcode.CodeOf(err) != errcode.StorageFull {
		t.Fatalf("want STORAGE_FULL, got %v", err)
	}
	if _, err := m.Install(t.Context(), req); err != nil {
		t.Fatalf("failure should only affect one call: %v", err)
	}
	if m.InstallCalls() != 1 {
		t.Fatalf("calls = %d", m.InstallCalls())
	}
	var _ install.Installer = m
}
