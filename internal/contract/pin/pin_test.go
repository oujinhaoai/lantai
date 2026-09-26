package pin_test

import (
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/pin"
	"github.com/oujinhaoai/lantai/internal/contract/pin/pintest"
)

var (
	t0  = time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	hA  = strings.Repeat("a", 64)
	hB  = strings.Repeat("b", 64)
	hNo = strings.Repeat("c", 64)
)

func TestValidate(t *testing.T) {
	good := pin.Pin{PinID: ids.New(), Kind: pin.KindCommit, OwnerModule: "ledger",
		Owner: pin.Owner{Kind: "operation", ID: ids.New()}, Blobs: []string{hA, hB}, CreatedAt: t0}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*pin.Pin){
		"commit expiry":  func(p *pin.Pin) { p.ExpiresAt = t0.Add(time.Hour) },
		"wrong owner":    func(p *pin.Pin) { p.Owner.Kind = "backup" },
		"unsorted":       func(p *pin.Pin) { p.Blobs = []string{hB, hA} },
		"duplicate":      func(p *pin.Pin) { p.Blobs = []string{hA, hA} },
		"empty":          func(p *pin.Pin) { p.Blobs = nil },
		"uppercase hash": func(p *pin.Pin) { p.Blobs = []string{strings.ToUpper(hA)} },
	} {
		p := good
		mutate(&p)
		if p.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestHeldAcrossSources(t *testing.T) {
	ctx := t.Context()
	uploads, commits := pintest.New(), pintest.New()
	up := pin.Pin{PinID: ids.New(), Kind: pin.KindUpload, OwnerModule: "storage", Owner: pin.Owner{Kind: "upload_session", ID: ids.New()},
		Blobs: []string{hA}, CreatedAt: t0, ExpiresAt: t0.Add(24 * time.Hour)}
	cm := pin.Pin{PinID: ids.New(), Kind: pin.KindCommit, OwnerModule: "ledger", Owner: pin.Owner{Kind: "operation", ID: ids.New()},
		Blobs: []string{hB}, CreatedAt: t0}
	if err := uploads.Add(ctx, up); err != nil {
		t.Fatal(err)
	}
	if err := uploads.Add(ctx, up); err != nil {
		t.Fatal("re-adding the same pin must be idempotent")
	}
	reuse := up
	reuse.Blobs = []string{hB}
	if errcode.CodeOf(uploads.Add(ctx, reuse)) != errcode.IdempotencyConflict {
		t.Fatal("pin id reuse with different content accepted")
	}
	if err := commits.Add(ctx, cm); err != nil {
		t.Fatal(err)
	}
	check := func(h string, now time.Time, want bool) {
		t.Helper()
		got, err := pin.Held(ctx, h, now, uploads, commits)
		if err != nil || got != want {
			t.Fatalf("Held(%s…) = %v, %v; want %v", h[:4], got, err, want)
		}
	}
	check(hA, t0, true)
	check(hB, t0, true)
	check(hNo, t0, false)
	check(hA, t0.Add(24*time.Hour), false)  // 上传 pin 到期
	check(hB, t0.Add(1000*time.Hour), true) // 提交 pin 不按时间失效
	if err := commits.Release(ctx, cm.PinID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	check(hB, t0.Add(2*time.Hour), false)

	commits.Fail = pintest.ErrUnavailable
	held, err := pin.Held(ctx, hNo, t0, uploads, commits)
	if !held || err == nil {
		t.Fatal("an unavailable source must be treated as holding the blob")
	}
}
