// Package errcode 提供全协议共用的结构化错误。
//
// 错误码的唯一来源是 schemas/common/v1/error-codes.json；本包的常量与
// 登记表由 scripts/gen/errcodes 生成（codes_gen.go）。每个错误码的 HTTP
// 状态、retryable 与 recovery_action 固定，构造错误时不能改写，保证同一
// 错误在 REST、CLI、MCP 与 adapter 协议中含义一致。
package errcode

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Code 是登记过的稳定错误码。
type Code string

// RecoveryAction 是调用方下一步的机器可读建议。
type RecoveryAction string

// 与 schemas/common/v1/defs.schema.json 的 recovery_action 枚举一致。
const (
	ActionNone           RecoveryAction = "none"
	ActionRetry          RecoveryAction = "retry"
	ActionReauthenticate RecoveryAction = "reauthenticate"
	ActionRefreshState   RecoveryAction = "refresh_state"
	ActionPollOperation  RecoveryAction = "poll_operation"
	ActionReconcile      RecoveryAction = "reconcile"
	ActionHumanAction    RecoveryAction = "human_action"
	ActionFixRequest     RecoveryAction = "fix_request"
	ActionUploadContent  RecoveryAction = "upload_content"
	ActionResync         RecoveryAction = "resync"
)

// Spec 是错误码的登记信息。
type Spec struct {
	Code       Code
	HTTPStatus int
	Retryable  bool
	Recovery   RecoveryAction
	Owner      string
	Summary    string
}

var byCode = func() map[Code]Spec {
	m := make(map[Code]Spec, len(specs))
	for _, s := range specs {
		m[s.Code] = s
	}
	return m
}()

// Lookup 返回登记信息；被替代的旧码与未登记码返回 false。
func Lookup(c Code) (Spec, bool) {
	s, ok := byCode[c]
	return s, ok
}

// All 返回按错误码排序的全部登记项。
func All() []Spec {
	out := make([]Spec, len(specs))
	copy(out, specs)
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// Superseded 报告 c 是否为已被替代的旧草案错误码，并返回替代码。
func Superseded(c Code) ([]Code, bool) {
	r, ok := superseded[c]
	return r, ok
}

// Detail 是错误的逐项细节。
type Detail struct {
	// Reason 为小写下划线的机器可读原因。
	Reason  string         `json:"reason"`
	Pointer *string        `json:"pointer,omitempty"`
	Message string         `json:"message,omitempty"`
	Ref     string         `json:"ref,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

// PointerDetail 构造带 JSON Pointer 的细节；空字符串指文档根。
func PointerDetail(reason, pointer, message string) Detail {
	return Detail{Reason: reason, Pointer: &pointer, Message: message}
}

// Error 是结构化错误。cause 只用于日志与 errors.Is/As，不会序列化给调用方。
type Error struct {
	Code        Code
	Message     string
	Hint        string
	OperationID string
	RetryAfter  time.Duration
	Refs        []string
	Details     []Detail
	cause       error
}

// New 构造错误；未登记或已被替代的错误码属于编程错误，直接 panic。
func New(code Code, message string) *Error {
	if _, ok := byCode[code]; !ok {
		if r, old := superseded[code]; old {
			panic(fmt.Sprintf("errcode: %s is superseded by %v", code, r))
		}
		panic(fmt.Sprintf("errcode: unregistered code %q", code))
	}
	if message == "" {
		message = byCode[code].Summary
	}
	return &Error{Code: code, Message: message}
}

// Newf 按格式构造消息。消息会返回给调用方，不得包含凭据、签名 URL 或无权来源信息。
func Newf(code Code, format string, args ...any) *Error {
	return New(code, fmt.Sprintf(format, args...))
}

// Wrap 构造错误并保留内部原因。
func Wrap(code Code, message string, cause error) *Error {
	e := New(code, message)
	e.cause = cause
	return e
}

// WithHint 设置修复提示。
func (e *Error) WithHint(hint string) *Error { e.Hint = hint; return e }

// WithOperation 关联操作 ID。
func (e *Error) WithOperation(id string) *Error { e.OperationID = id; return e }

// WithRetryAfter 设置建议等待时间。
func (e *Error) WithRetryAfter(d time.Duration) *Error { e.RetryAfter = d; return e }

// WithRefs 追加相关引用；只放调用者有权看到的引用。
func (e *Error) WithRefs(refs ...string) *Error { e.Refs = append(e.Refs, refs...); return e }

// WithDetails 追加细节。
func (e *Error) WithDetails(d ...Detail) *Error { e.Details = append(e.Details, d...); return e }

// Spec 返回登记信息。
func (e *Error) Spec() Spec { return byCode[e.Code] }

// HTTPStatus 返回登记的 HTTP 状态码。
func (e *Error) HTTPStatus() int { return byCode[e.Code].HTTPStatus }

// Retryable 返回登记的可重试性。
func (e *Error) Retryable() bool { return byCode[e.Code].Retryable }

// Recovery 返回登记的下一步建议。
func (e *Error) Recovery() RecoveryAction { return byCode[e.Code].Recovery }

func (e *Error) Error() string {
	var sb strings.Builder
	sb.WriteString(string(e.Code))
	sb.WriteString(": ")
	sb.WriteString(e.Message)
	if e.cause != nil {
		sb.WriteString(": ")
		sb.WriteString(e.cause.Error())
	}
	return sb.String()
}

// Unwrap 支持 errors.Is/As 访问内部原因。
func (e *Error) Unwrap() error { return e.cause }

// Is 使 errors.Is(err, errcode.New(X, "")) 按错误码匹配。
func (e *Error) Is(target error) bool {
	var t *Error
	if errors.As(target, &t) {
		return t.Code == e.Code
	}
	return false
}

// As 从错误链中取出 *Error。
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// CodeOf 返回错误链中的错误码；非结构化错误视为 INTERNAL。
func CodeOf(err error) Code {
	if e, ok := As(err); ok {
		return e.Code
	}
	return Internal
}

// From 把任意错误转换为结构化错误；非结构化错误统一为 INTERNAL，
// 对外消息不带内部细节，原因保留在 cause 中供日志使用。
func From(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := As(err); ok {
		return e
	}
	return Wrap(Internal, byCode[Internal].Summary, err)
}

// Envelope 是 lantai.error/v1 的 JSON 形态。
type Envelope struct {
	Error Body `json:"error"`
}

// Body 是错误信封的主体。
type Body struct {
	Code           Code           `json:"code"`
	Message        string         `json:"message"`
	Hint           string         `json:"hint,omitempty"`
	Retryable      bool           `json:"retryable"`
	RecoveryAction RecoveryAction `json:"recovery_action"`
	RetryAfterMS   *int64         `json:"retry_after_ms,omitempty"`
	RequestID      string         `json:"request_id,omitempty"`
	OperationID    string         `json:"operation_id,omitempty"`
	Refs           []string       `json:"refs,omitempty"`
	Details        []Detail       `json:"details,omitempty"`
}

// Envelope 生成对外错误信封；requestID 可为空。
func (e *Error) Envelope(requestID string) Envelope {
	s := byCode[e.Code]
	b := Body{
		Code:           e.Code,
		Message:        e.Message,
		Hint:           e.Hint,
		Retryable:      s.Retryable,
		RecoveryAction: s.Recovery,
		RequestID:      requestID,
		OperationID:    e.OperationID,
		Refs:           e.Refs,
		Details:        e.Details,
	}
	if e.RetryAfter > 0 {
		ms := e.RetryAfter.Milliseconds()
		if ms == 0 {
			ms = 1
		}
		b.RetryAfterMS = &ms
	}
	return Envelope{Error: b}
}

// CheckBody 核对错误信封是否与登记一致：错误码已登记且未被替代，
// retryable 与 recovery_action 与登记相同。结构校验由 schema 完成。
func CheckBody(b Body) error {
	if r, old := superseded[b.Code]; old {
		return fmt.Errorf("errcode: %s is superseded by %v", b.Code, r)
	}
	s, ok := byCode[b.Code]
	if !ok {
		return fmt.Errorf("errcode: unregistered code %q", b.Code)
	}
	if b.Retryable != s.Retryable {
		return fmt.Errorf("errcode: %s retryable=%v, registry says %v", b.Code, b.Retryable, s.Retryable)
	}
	if b.RecoveryAction != s.Recovery {
		return fmt.Errorf("errcode: %s recovery_action=%s, registry says %s", b.Code, b.RecoveryAction, s.Recovery)
	}
	return nil
}
