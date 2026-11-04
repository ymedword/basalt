package core

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// CanonicalFlowJSON 保持精确数值并归一对象顺序、空白及等值数字，供幂等和输入冻结。
// CanonicalFlowJSON preserves exact values while normalizing semantic request identity.
func CanonicalFlowJSON(ctx context.Context, input json.RawMessage) ([]byte, error) {
	value, err := DecodeFlowJSON(ctx, input)
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeJSONNumbers(value)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil || len(encoded) > 1<<20 {
		return nil, errors.New("flow canonical json exceeds bound")
	}
	return encoded, nil
}

func normalizeJSONNumbers(value any) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized, err := normalizeJSONNumbers(child)
			if err != nil {
				return nil, err
			}
			typed[key] = normalized
		}
	case []any:
		for i, child := range typed {
			normalized, err := normalizeJSONNumbers(child)
			if err != nil {
				return nil, err
			}
			typed[i] = normalized
		}
	case json.Number:
		raw := string(typed)
		sign := ""
		if strings.HasPrefix(raw, "-") {
			sign, raw = "-", raw[1:]
		}
		exponent := 0
		if index := strings.IndexAny(raw, "eE"); index >= 0 {
			var err error
			exponent, err = strconv.Atoi(raw[index+1:])
			if err != nil {
				return nil, errors.New("flow number exponent invalid")
			}
			raw = raw[:index]
		}
		if index := strings.IndexByte(raw, '.'); index >= 0 {
			exponent -= len(raw) - index - 1
			raw = raw[:index] + raw[index+1:]
		}
		raw = strings.TrimLeft(raw, "0")
		if raw == "" {
			return json.Number("0"), nil
		}
		trimmed := strings.TrimRight(raw, "0")
		exponent += len(raw) - len(trimmed)
		if exponent >= 0 {
			return json.Number(sign + trimmed + strings.Repeat("0", exponent)), nil
		}
		return json.Number(sign + trimmed + "e" + strconv.Itoa(exponent)), nil
	}
	return value, nil
}
