package apiv1_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/apiv1"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

func strictDecode(t *testing.T, data []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("generated type cannot decode contract JSON: %v\n%s", err, data)
	}
}

// 契约包输出的 JSON 能被生成的传输类型无损解码（客户端方向）。
func TestGeneratedTypesDecodeContractOutput(t *testing.T) {
	env := errcode.New(errcode.LeaseStale, "stale").WithOperation(string(ids.New())).
		WithDetails(errcode.PointerDetail("fence_mismatch", "/fence", "old fence")).Envelope("req-1")
	data, _ := json.Marshal(env)
	var got apiv1.ErrorEnvelope
	strictDecode(t, data, &got)
	if got.Error.Code != string(errcode.LeaseStale) || got.Error.RecoveryAction != apiv1.RecoveryActionRefreshState || got.Error.Retryable {
		t.Fatalf("decoded %+v", got.Error)
	}

	pending := true
	view := commands.View{
		OperationID: ids.New(), OwnerModule: "ledger", CommandType: "ledger.commit_version", Stage: commands.StageCommitted,
		ProjectionPending: &pending, ResultRefs: []commands.ResultRef{{Kind: "version", ID: ids.New(), Revision: 1}},
		NextAction: errcode.ActionNone, CreatedAt: time.Date(2026, 9, 26, 10, 0, 0, 120_000_000, time.UTC),
		UpdatedAt: time.Date(2026, 9, 26, 10, 0, 1, 0, time.UTC),
	}
	vdata, _ := json.Marshal(view)
	var op apiv1.OperationStatus
	strictDecode(t, vdata, &op)
	if op.Stage != apiv1.OperationStatusStageCommitted || !op.CreatedAt.Equal(view.CreatedAt) {
		t.Fatalf("decoded %+v", op)
	}
}

// 生成类型把 Timestamp 映射为 time.Time，默认编码会省略末尾的零毫秒，
// 不满足契约的 3 位毫秒格式；服务端输出必须使用契约包的编码（例如 commands.View）。
func TestGeneratedTimestampEncodingIsNotContractSafe(t *testing.T) {
	reg, err := schema.Default()
	if err != nil {
		t.Fatal(err)
	}
	op := apiv1.OperationStatus{
		OperationId: string(ids.New()), OwnerModule: "ledger", CommandType: "ledger.commit_version",
		Stage: apiv1.OperationStatusStagePrepared, NextAction: apiv1.RecoveryActionPollOperation, Retryable: true,
		CreatedAt: time.Date(2026, 9, 26, 10, 0, 0, 120_000_000, time.UTC),
		UpdatedAt: time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC),
	}
	data, _ := json.Marshal(op)
	if reg.ValidateJSON("lantai.operation/v1", data) == nil {
		t.Fatal("expected default time.Time encoding to violate the timestamp format; update the docs if the generator changed")
	}
}
