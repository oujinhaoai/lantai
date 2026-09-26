package authztest

import (
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// 桩与真实实现（identity）运行同一套件，保证用桩开发的模块接入真实授权后语义不变。
func TestStaticSatisfiesContract(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC))
	RunAuthorizerContract(t, NewStaticHarness(New(clk, ids.New()), clk))
}
