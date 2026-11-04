package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SchemaValidator 在外发前校验固定 hash 对应的规则；实现缺失必须失败关闭。
// SchemaValidator validates pinned rules before dispatch; missing adapters fail closed.
type SchemaValidator interface {
	Validate(ctx context.Context, schemaHash string, input json.RawMessage) error
}

// SchemaViolationError 区别已知输入违规与校验适配器不可用。
// SchemaViolationError distinguishes invalid input from an unavailable adapter.
type SchemaViolationError struct{ Path string }

func (e *SchemaViolationError) Error() string { return "flow schema violation at " + e.Path }

// DecodeFlowJSON 拒绝重复键、尾随文档和超限结构，数值保持 json.Number。
// DecodeFlowJSON rejects ambiguous or unbounded JSON and preserves exact numbers.
func DecodeFlowJSON(ctx context.Context, input json.RawMessage) (any, error) {
	if len(input) == 0 || len(input) > 1<<20 || !utf8.Valid(input) {
		return nil, errors.New("flow json size or encoding invalid")
	}
	d := json.NewDecoder(bytes.NewReader(input))
	d.UseNumber()
	properties := 0
	v, err := decodeFlowValue(ctx, d, 0, &properties)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("flow json trailing document")
	}
	return v, nil
}

func decodeFlowValue(ctx context.Context, d *json.Decoder, depth int, properties *int) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if depth > 16 {
		return nil, errors.New("flow json depth exceeded")
	}
	token, err := d.Token()
	if err != nil {
		return nil, errors.New("flow json syntax invalid")
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			object := make(map[string]any)
			for d.More() {
				keyToken, err := d.Token()
				if err != nil {
					return nil, errors.New("flow json object invalid")
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errors.New("flow json object key invalid")
				}
				if _, found := object[key]; found {
					return nil, errors.New("flow json duplicate key")
				}
				*properties++
				if *properties > 1024 {
					return nil, errors.New("flow json property limit exceeded")
				}
				child, err := decodeFlowValue(ctx, d, depth+1, properties)
				if err != nil {
					return nil, err
				}
				object[key] = child
			}
			if end, err := d.Token(); err != nil || end != json.Delim('}') {
				return nil, errors.New("flow json object incomplete")
			}
			return object, nil
		case '[':
			array := make([]any, 0)
			for d.More() {
				if len(array) >= 1024 {
					return nil, errors.New("flow json array limit exceeded")
				}
				child, err := decodeFlowValue(ctx, d, depth+1, properties)
				if err != nil {
					return nil, err
				}
				array = append(array, child)
			}
			if end, err := d.Token(); err != nil || end != json.Delim(']') {
				return nil, errors.New("flow json array incomplete")
			}
			return array, nil
		default:
			return nil, errors.New("flow json delimiter invalid")
		}
	case json.Number:
		// 先约束指数复杂度再交给精确运算，防止短字符串造成巨量 big.Int 分配。
		// Bound exponent work before exact arithmetic, without rounding to float64.
		raw := string(value)
		if at := strings.IndexAny(raw, "eE"); at >= 0 {
			exponent, err := strconv.ParseInt(raw[at+1:], 10, 32)
			if err != nil || exponent < -10000 || exponent > 10000 {
				return nil, errors.New("flow json numeric complexity exceeded")
			}
		}
		return value, nil
	default:
		return value, nil
	}
}
