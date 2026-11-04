package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/ymedword/basalt/internal/core"
)

// ErrFlowToolUnavailable fails closed for tools without a reviewed code-owned
// adapter. A name, MCP readOnlyHint or author-supplied classification cannot opt in.
var ErrFlowToolUnavailable = errors.New("tool has no trusted Flow adapter")

// FlowAdapterSpec pins the original schema separately from the stricter Flow
// overlay. It is execution metadata and never changes ordinary InputSchema.
type FlowAdapterSpec struct {
	Name, Version, RecoveryClass, OriginalSchemaHash, SchemaHash string
	Validator                                                    core.SchemaValidator
}

// FlowAdapterFor recognizes concrete built-in implementations, not just names.
// The first set deliberately excludes unknown MCP, generic HTTP and shell calls.
func FlowAdapterFor(ctx context.Context, pinned core.Tool, schemas *SchemaCompiler) (FlowAdapterSpec, error) {
	var spec FlowAdapterSpec
	if pinned == nil || schemas == nil {
		return spec, ErrFlowToolUnavailable
	}
	class := ""
	switch value := pinned.(type) {
	case *ReadFileTool:
		if value == nil {
			return spec, ErrFlowToolUnavailable
		}
		class = "readonly"
	case *ListDirTool:
		if value == nil {
			return spec, ErrFlowToolUnavailable
		}
		class = "readonly"
	case *WriteFileTool:
		if value == nil {
			return spec, ErrFlowToolUnavailable
		}
		class = "verifiable_write"
	case *NotifyTool:
		if value == nil {
			return spec, ErrFlowToolUnavailable
		}
		class = "unverifiable_write"
	default:
		return spec, ErrFlowToolUnavailable
	}
	original := pinned.InputSchema()
	decoded, err := core.DecodeFlowJSON(ctx, original)
	if err != nil {
		return spec, ErrFlowToolUnavailable
	}
	shape, ok := decoded.(map[string]any)
	if !ok || shape["type"] != "object" {
		return spec, ErrFlowToolUnavailable
	}
	// These reviewed builtins have flat property schemas. The only common overlay
	// is to reject unspecified fields; unknown keywords are still compiler errors.
	shape["additionalProperties"] = false
	if _, notify := pinned.(*NotifyTool); notify {
		shape["required"] = []string{"channel", "content"}
	}
	strict, err := json.Marshal(shape)
	if err != nil {
		return spec, ErrFlowToolUnavailable
	}
	hash, err := schemas.Compile(ctx, strict)
	if err != nil {
		return spec, errors.Join(ErrFlowToolUnavailable, err)
	}
	sum := sha256.Sum256(original)
	return FlowAdapterSpec{Name: pinned.Name(), Version: "builtin-flow-v1", RecoveryClass: class, OriginalSchemaHash: hex.EncodeToString(sum[:]), SchemaHash: hash, Validator: flowBuiltinValidator{schemas: schemas, tool: pinned.Name()}}, nil
}

type flowBuiltinValidator struct {
	schemas *SchemaCompiler
	tool    string
}

func (v flowBuiltinValidator) Validate(ctx context.Context, hash string, input json.RawMessage) error {
	if err := v.schemas.Validate(ctx, hash, input); err != nil {
		return err
	}
	if v.tool == "notify" {
		var request struct {
			Channel string `json:"channel"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(input, &request); err != nil {
			return ErrFlowToolUnavailable
		}
		if request.Channel == "" {
			return &core.SchemaViolationError{Path: "$.channel"}
		}
		if request.Content == "" || len(request.Content) > 20*1024 {
			return &core.SchemaViolationError{Path: "$.content"}
		}
	}
	return nil
}
