// Package event 实现事件信封 lantai.event-envelope/v1 的 Go 表示。
//
// 业务模块在自己的库事务里把信封写入 outbox；relay 按 event_id 唯一收录到
// events.db，并由 events.db 分配只表示收录顺序的 global_seq。信封存储为
// RFC 8785 规范化 JSON，搬运时逐字节复制，重复搬运不会产生第二个逻辑事件。
// 结构以 schemas/common/v1/event-envelope.schema.json 为准，本包通过它校验。
package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

// Contract 是信封的契约标识。
const Contract = "lantai.event-envelope/v1"

// Envelope 是事件信封。
type Envelope struct {
	EventID           ids.ID
	EventType         string
	SchemaVersion     int
	AggregateType     string
	AggregateID       ids.ID
	AggregateRevision int64
	ActorID           ids.ID
	SessionID         ids.ID // 可为空
	ProjectID         ids.ID // 实例级事件为空
	OperationID       ids.ID
	CorrelationID     ids.ID
	CausationID       ids.ID // 可为空
	OccurredAt        time.Time
	Payload           json.RawMessage
}

type wire struct {
	EventID           ids.ID          `json:"event_id"`
	EventType         string          `json:"event_type"`
	SchemaVersion     int             `json:"schema_version"`
	AggregateType     string          `json:"aggregate_type"`
	AggregateID       ids.ID          `json:"aggregate_id"`
	AggregateRevision int64           `json:"aggregate_revision"`
	ActorID           ids.ID          `json:"actor_id"`
	SessionID         ids.ID          `json:"session_id,omitempty"`
	ProjectID         ids.ID          `json:"project_id,omitempty"`
	OperationID       ids.ID          `json:"operation_id"`
	CorrelationID     ids.ID          `json:"correlation_id"`
	CausationID       ids.ID          `json:"causation_id,omitempty"`
	OccurredAt        string          `json:"occurred_at"`
	Payload           json.RawMessage `json:"payload"`
}

// MarshalJSON 输出契约格式（时间为 UTC 毫秒）。
func (e Envelope) MarshalJSON() ([]byte, error) {
	payload := e.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	return json.Marshal(wire{
		EventID: e.EventID, EventType: e.EventType, SchemaVersion: e.SchemaVersion,
		AggregateType: e.AggregateType, AggregateID: e.AggregateID, AggregateRevision: e.AggregateRevision,
		ActorID: e.ActorID, SessionID: e.SessionID, ProjectID: e.ProjectID,
		OperationID: e.OperationID, CorrelationID: e.CorrelationID, CausationID: e.CausationID,
		OccurredAt: clock.Format(e.OccurredAt), Payload: payload,
	})
}

// UnmarshalJSON 只做字段映射；完整校验请用 Parse。
func (e *Envelope) UnmarshalJSON(data []byte) error {
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	t, err := clock.Parse(w.OccurredAt)
	if err != nil {
		return err
	}
	*e = Envelope{
		EventID: w.EventID, EventType: w.EventType, SchemaVersion: w.SchemaVersion,
		AggregateType: w.AggregateType, AggregateID: w.AggregateID, AggregateRevision: w.AggregateRevision,
		ActorID: w.ActorID, SessionID: w.SessionID, ProjectID: w.ProjectID,
		OperationID: w.OperationID, CorrelationID: w.CorrelationID, CausationID: w.CausationID,
		OccurredAt: t, Payload: w.Payload,
	}
	return nil
}

// Validate 按权威 schema 校验信封。
func (e Envelope) Validate() error {
	_, err := e.Canonical()
	return err
}

// Canonical 校验信封并返回规范化 JSON，用于 outbox 与 events.db 存储。
func (e Envelope) Canonical() ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("event: marshal: %w", err)
	}
	return validateCanonical(raw)
}

// Parse 严格解析并校验存储或传输中的信封。
func Parse(data []byte) (Envelope, error) {
	if _, err := validateCanonical(data); err != nil {
		return Envelope{}, err
	}
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return Envelope{}, fmt.Errorf("event: %w", err)
	}
	return e, nil
}

func validateCanonical(raw []byte) ([]byte, error) {
	doc, err := canonjson.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("event: %w", err)
	}
	reg, err := schema.Default()
	if err != nil {
		return nil, err
	}
	if err := reg.Validate(Contract, doc); err != nil {
		return nil, err
	}
	return canonjson.Canonicalize(raw)
}

// Params 是构造新事件所需的业务字段；ID 与时间由 New 分配。
type Params struct {
	EventType         string
	SchemaVersion     int
	AggregateType     string
	AggregateID       ids.ID
	AggregateRevision int64
	ActorID           ids.ID
	SessionID         ids.ID
	ProjectID         ids.ID
	OperationID       ids.ID
	CorrelationID     ids.ID
	CausationID       ids.ID
	Payload           any
}

// ErrPayloadNotObject 表示 payload 不是 JSON 对象。
var ErrPayloadNotObject = errors.New("event: payload must be a JSON object")

// New 分配 event_id 与发生时间并校验信封。CorrelationID 为空时沿用 OperationID。
func New(gen *ids.Generator, clk clock.Clock, p Params) (Envelope, error) {
	id, err := gen.New()
	if err != nil {
		return Envelope{}, err
	}
	payload, err := json.Marshal(p.Payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("event: marshal payload: %w", err)
	}
	if p.Payload == nil {
		payload = []byte("{}")
	}
	if len(payload) == 0 || payload[0] != '{' {
		return Envelope{}, ErrPayloadNotObject
	}
	corr := p.CorrelationID
	if corr == "" {
		corr = p.OperationID
	}
	e := Envelope{
		EventID: id, EventType: p.EventType, SchemaVersion: p.SchemaVersion,
		AggregateType: p.AggregateType, AggregateID: p.AggregateID, AggregateRevision: p.AggregateRevision,
		ActorID: p.ActorID, SessionID: p.SessionID, ProjectID: p.ProjectID,
		OperationID: p.OperationID, CorrelationID: corr, CausationID: p.CausationID,
		OccurredAt: clk.Now(), Payload: payload,
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}
	return e, nil
}

// StoreEntry 是 events.db 的收录记录（lantai.event-store-entry/v1）。
type StoreEntry struct {
	GlobalSeq  int64
	RecordedAt time.Time
	EventID    ids.ID
}

// MarshalJSON 输出契约格式。
func (s StoreEntry) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		GlobalSeq  int64  `json:"global_seq"`
		RecordedAt string `json:"recorded_at"`
		EventID    ids.ID `json:"event_id"`
	}{s.GlobalSeq, clock.Format(s.RecordedAt), s.EventID})
}
