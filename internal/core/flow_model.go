package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
)

// ErrFlowModelNotIssued is returned only before ProviderService's sole Chat
// dispatch, allowing the original caller reservation to be returned safely.
var ErrFlowModelNotIssued = errors.New("flow model request was not issued")

// ErrFlowBudget 是发出前祖先预算拒绝；不会将未知结果改写为已知停止。
// ErrFlowBudget identifies a pre-dispatch ancestor allocation refusal.
var ErrFlowBudget = errors.New("flow budget exhausted")

// ErrFlowUsageInvalid preserves a contradictory provider report as unknown;
// it cannot release an ancestor reservation or authorize the next model call.
var ErrFlowUsageInvalid = errors.New("flow model usage report is inconsistent")

// FlowUsageTotal is stricter than legacy ordinary-Run accounting. Missing totals
// may use nonnegative components; a positive total cannot contradict their sum.
func FlowUsageTotal(usage Usage) (int64, bool, error) {
	if usage.TotalTokens < 0 || usage.PromptTokens < 0 || usage.CompletionTokens < 0 {
		return 0, false, ErrFlowUsageInvalid
	}
	prompt, completion, total := int64(usage.PromptTokens), int64(usage.CompletionTokens), int64(usage.TotalTokens)
	if prompt > math.MaxInt64-completion {
		return 0, false, ErrFlowUsageInvalid
	}
	parts := prompt + completion
	if total > 0 && total < parts {
		return 0, false, ErrFlowUsageInvalid
	}
	if total == 0 {
		total = parts
	}
	return total, total > 0, nil
}

// SnapshotFlowModelRequest copies and hashes the complete outbound payload plus
// source locators. It neither writes prompts to a second ledger nor logs them.
func SnapshotFlowModelRequest(ctx context.Context, req ChatRequest) (ChatRequest, string, error) {
	type payload struct {
		Provider, Model, ConfigHash string
		Messages                    []Message
		Tools                       []ToolSpec
		Evidence                    []EvidenceRef
	}
	wire := payload{req.Provider, req.Model, req.ConfigHash, req.Messages, req.Tools, req.FlowEvidenceRefs}
	raw, err := json.Marshal(wire)
	if err != nil {
		return ChatRequest{}, "", ErrFlowDispatch
	}
	raw, err = CanonicalFlowJSON(ctx, raw)
	if err != nil {
		return ChatRequest{}, "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var frozen payload
	if err := decoder.Decode(&frozen); err != nil {
		return ChatRequest{}, "", err
	}
	req.Messages, req.Tools, req.FlowEvidenceRefs = frozen.Messages, frozen.Tools, frozen.Evidence
	sum := sha256.Sum256(raw)
	return req, hex.EncodeToString(sum[:]), nil
}
