package observability

import (
	"errors"
	"strings"
	"unicode"
)

// AnchorKind 标识独立查询范围，不暗示不同范围之间的因果关系。
// AnchorKind identifies an independent query scope.
type AnchorKind string

// 六种锚点均来自已有真实标识，不构造虚假的 Run。
// Anchors reuse real identifiers without inventing runs.
const (
	AnchorRun       AnchorKind = "run"
	AnchorRequest   AnchorKind = "request"
	AnchorTask      AnchorKind = "task"
	AnchorOperation AnchorKind = "operation"
	AnchorIngest    AnchorKind = "ingest"
	AnchorSession   AnchorKind = "session"
	AnchorFlowTask  AnchorKind = "flow_task"
)

// Anchor 把标识与作用域显式传递，不通过 context 隐式携带。
// Anchor carries query identity explicitly.
type Anchor struct {
	Kind  AnchorKind `json:"kind"`
	Value string     `json:"value"`
}

// ErrInvalidQuery 表示锚点或有界查询参数不合法，不包含请求原文。
// ErrInvalidQuery identifies an invalid bounded query without echoing input.
var ErrInvalidQuery = errors.New("invalid observability query")

// Validate 拒绝空值、控制字符和不支持的锚点。
// Validate rejects empty, oversized, or unsupported anchors.
func (a Anchor) Validate() error {
	switch a.Kind {
	case AnchorRun, AnchorRequest, AnchorTask, AnchorOperation, AnchorIngest, AnchorSession, AnchorFlowTask:
	default:
		return ErrInvalidQuery
	}
	if a.Value == "" || len(a.Value) > 256 || strings.TrimSpace(a.Value) != a.Value {
		return ErrInvalidQuery
	}
	for _, r := range a.Value {
		if unicode.IsControl(r) {
			return ErrInvalidQuery
		}
	}
	if a.Kind == AnchorRequest {
		if len(a.Value) > 64 {
			return ErrInvalidQuery
		}
		for _, r := range a.Value {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r)) {
				return ErrInvalidQuery
			}
		}
	}
	return nil
}
