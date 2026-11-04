package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ymedword/basalt/internal/core"
)

// SchemaCompiler 编译并缓存 Flow 使用的有限 JSON Schema；普通工具路径不受影响。
// SchemaCompiler compiles bounded Flow schemas without changing ordinary tool validation.
type SchemaCompiler struct {
	mu      sync.RWMutex
	schemas map[string]*jsonschema.Schema
}

// NewSchemaCompiler 返回显式装配的编译器，不发网络请求。
// NewSchemaCompiler constructs a compiler without making network requests.
func NewSchemaCompiler() *SchemaCompiler { return &SchemaCompiler{} }

// Compile 验证已批准关键字子集并返回内容地址。
// Compile validates the approved keyword subset and returns a content address.
func (c *SchemaCompiler) Compile(ctx context.Context, schema json.RawMessage) (string, error) {
	value, err := core.DecodeFlowJSON(ctx, schema)
	if err != nil {
		return "", err
	}
	if err := checkFlowSchema(value, 0); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", errors.New("flow schema encoding failed")
	}
	digest := sha256.Sum256(canonical)
	hash := hex.EncodeToString(digest[:])
	c.mu.RLock()
	_, found := c.schemas[hash]
	c.mu.RUnlock()
	if found {
		return hash, nil
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(denyFlowSchemaLoader{})
	location := "https://basalt.invalid/flow-schema/" + hash
	if err := compiler.AddResource(location, value); err != nil {
		return "", errors.New("flow schema resource invalid")
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return "", errors.New("flow schema rules invalid")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.schemas == nil {
		c.schemas = make(map[string]*jsonschema.Schema)
	}
	if _, found := c.schemas[hash]; !found && len(c.schemas) >= 4096 {
		return "", errors.New("flow schema cache capacity exhausted")
	}
	c.schemas[hash] = compiled
	return hash, nil
}

// Validate 在发出工具之前验证固定规则及输入。
// Validate checks pinned rules and input before a tool may dispatch.
func (c *SchemaCompiler) Validate(ctx context.Context, hash string, input json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.RLock()
	schema := c.schemas[hash]
	c.mu.RUnlock()
	if schema == nil {
		return errors.New("flow schema unavailable")
	}
	value, err := core.DecodeFlowJSON(ctx, input)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &core.SchemaViolationError{Path: "$"}
	}
	if err := schema.Validate(value); err != nil {
		// 第三方错误包含枚举／实例值，只投影已验证的安全路径，不回传原文。
		// Third-party errors may include values; expose only a bounded safe location.
		path := "$"
		var validation *jsonschema.ValidationError
		if errors.As(err, &validation) {
			for len(validation.Causes) > 0 {
				validation = validation.Causes[0]
			}
			for _, part := range validation.InstanceLocation {
				if !safeSchemaPathPart(part) {
					break
				}
				path += "/" + part
			}
		}
		return &core.SchemaViolationError{Path: path}
	}
	return ctx.Err()
}

type denyFlowSchemaLoader struct{}

func (denyFlowSchemaLoader) Load(string) (any, error) {
	return nil, errors.New("flow schema external resources forbidden")
}

func safeSchemaPathPart(part string) bool {
	if len(part) == 0 || len(part) > 64 {
		return false
	}
	for _, r := range part {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func checkFlowSchema(value any, depth int) error {
	object, ok := value.(map[string]any)
	if !ok || depth > 16 {
		return errors.New("flow schema object or depth invalid")
	}
	kind, ok := object["type"].(string)
	if !ok || !strings.Contains("|object|array|string|integer|number|boolean|null|", "|"+kind+"|") {
		return errors.New("flow schema requires one supported type")
	}
	for key, value := range object {
		switch key {
		case "type", "title", "description", "enum", "const":
		case "properties":
			properties, ok := value.(map[string]any)
			if !ok || kind != "object" {
				return errors.New("flow schema properties invalid")
			}
			for _, child := range properties {
				if err := checkFlowSchema(child, depth+1); err != nil {
					return err
				}
			}
		case "required", "additionalProperties":
			if kind != "object" {
				return errors.New("flow schema object keyword on other type")
			}
		case "items":
			if kind != "array" {
				return errors.New("flow schema items on other type")
			}
			if err := checkFlowSchema(value, depth+1); err != nil {
				return err
			}
		case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum":
			if kind != "number" && kind != "integer" {
				return errors.New("flow schema numeric keyword on other type")
			}
		case "minLength", "maxLength":
			if kind != "string" {
				return errors.New("flow schema length keyword on other type")
			}
		case "minItems", "maxItems":
			if kind != "array" {
				return errors.New("flow schema array keyword on other type")
			}
		default:
			return errors.New("flow schema unsupported keyword")
		}
	}
	if kind == "object" {
		if _, ok := object["additionalProperties"].(bool); !ok {
			return errors.New("flow schema object requires additionalProperties boolean")
		}
	}
	if kind == "array" {
		if _, ok := object["items"]; !ok {
			return errors.New("flow schema array requires items")
		}
	}
	return nil
}
