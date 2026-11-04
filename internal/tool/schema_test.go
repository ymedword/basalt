package tool

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFlow_NestedSchemaViolationZeroDispatch(t *testing.T) {
	c := NewSchemaCompiler()
	hash, err := c.Compile(context.Background(), json.RawMessage(`{"type":"object","properties":{"account":{"type":"object","properties":{"amount":{"type":"integer","minimum":1,"maximum":9007199254740993},"currency":{"type":"string","enum":["CNY"]}},"required":["amount","currency"],"additionalProperties":false}},"required":["account"],"additionalProperties":false}`))
	require.NoError(t, err)
	for _, input := range []string{
		`{"account":{"amount":"1","currency":"CNY"}}`,
		`{"account":{"amount":0,"currency":"CNY"}}`,
		`{"account":{"amount":9007199254740994,"currency":"CNY"}}`,
		`{"account":{"amount":1,"currency":"USD"}}`,
		`{"account":{"amount":1,"currency":"CNY","secret":"canary"}}`,
		`{"account":{"amount":1,"currency":"CNY","amount":2}}`,
		`{"account":{"amount":1,"currency":"CNY"}} {}`,
	} {
		t.Run(input, func(t *testing.T) {
			calls := 0
			err := c.Validate(context.Background(), hash, json.RawMessage(input))
			if err == nil {
				calls++
			}
			require.Error(t, err, "invalid nested input must be rejected before dispatch")
			require.Zero(t, calls, "schema violation must issue zero calls")
			require.NotContains(t, err.Error(), "canary", "validation errors must not echo values")
		})
	}
	require.NoError(t, c.Validate(context.Background(), hash, json.RawMessage(`{"account":{"amount":9007199254740993,"currency":"CNY"}}`)), "large integers must remain exact")
}

func TestFlow_UnknownSchemaKeywordRejected(t *testing.T) {
	c := NewSchemaCompiler()
	for _, schema := range []string{
		`{"type":"string","pattern":".*"}`,
		`{"$ref":"https://example.invalid/schema"}`,
		`{"type":["string","null"]}`,
		`{"type":"object","properties":{}}`,
		`{"type":"object","properties":{"x":{"type":"string","format":"email"}},"additionalProperties":false}`,
		`{"type":"array","items":[{"type":"string"}]}`,
		`{"type":"string","type":"integer"}`,
		`{"type":"string","x_unknown":true}`,
	} {
		_, err := c.Compile(context.Background(), json.RawMessage(schema))
		require.Error(t, err, "unsupported schema must be rejected, not ignored: %s", schema)
	}
}

func TestFlow_SchemaAdapterFailureFailsClosed(t *testing.T) {
	c := NewSchemaCompiler()
	require.Error(t, c.Validate(context.Background(), "missing", json.RawMessage(`{}`)), "missing compiled schema must fail closed")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Compile(ctx, json.RawMessage(`{"type":"boolean"}`))
	require.ErrorIs(t, err, context.Canceled)
	hash, err := c.Compile(context.Background(), json.RawMessage(`{"type":"array","items":{"type":"integer"}}`))
	require.NoError(t, err)
	require.Error(t, c.Validate(ctx, hash, json.RawMessage(`[]`)))
	require.Error(t, c.Validate(context.Background(), hash, json.RawMessage("["+strings.Repeat("0,", 1024)+"0]")))
	require.Error(t, c.Validate(context.Background(), hash, json.RawMessage(strings.Repeat("[", 18)+"0"+strings.Repeat("]", 18))))
	require.Error(t, c.Validate(context.Background(), hash, json.RawMessage(`[`+strings.Repeat(" ", 1<<20)+`]`)))
	require.Error(t, c.Validate(context.Background(), hash, json.RawMessage(`[1e1000000000]`)))
}

func TestFlow_SchemaCompilerConcurrentCache(t *testing.T) {
	c := NewSchemaCompiler()
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			hash, err := c.Compile(context.Background(), json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"],"additionalProperties":false}`))
			require.NoError(t, err)
			require.NoError(t, c.Validate(context.Background(), hash, json.RawMessage(`{"x":1}`)))
		})
	}
	wg.Wait()
}
