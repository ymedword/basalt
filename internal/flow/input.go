package flow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// OutputRevalidator checks completed output provenance against current authority;
// the checkpoint's Verified bit alone is never permission to reuse source text.
type OutputRevalidator interface {
	CheckOutput(ctx context.Context, principal core.Principal, task Task, output NodeOutput) error
}

// TrustedState is a server-read state and remaining budget, not a model payload.
type TrustedState struct {
	Task      Task
	Remaining core.UsageEstimate
}

// ResolveNodeInput freezes explicit mappings into canonical bytes before a Run
// or an action is prepared. A later caller mutation cannot change these bytes.
func ResolveNodeInput(ctx context.Context, p core.Principal, state TrustedState, node Node, outputs OutputRevalidator) (json.RawMessage, string, error) {
	if !p.Valid() || state.Task.CallerID != p.CallerID {
		return nil, "", core.ErrUnauthorized
	}
	if state.Task.Revision.FormatVersion != 1 || state.Task.Revision.TaskID != state.Task.ID || state.Task.Revision.Revision != state.Task.Version.Revision {
		return nil, "", ErrCheckpointUnsupported
	}
	if len(node.Inputs) == 0 {
		input, err := CanonicalJSON(ctx, state.Task.Revision.Input)
		if err != nil || flowInputHash(input) != state.Task.Revision.InputHash {
			return nil, "", ErrStateConflict
		}
		return input, flowInputHash(input), nil
	}
	values := make(map[string]any, len(node.Inputs))
	for _, mapping := range node.Inputs {
		if !validID(mapping.Target) || (mapping.Source == "") == (len(mapping.Literal) == 0) {
			return nil, "", ErrDefinitionInvalid
		}
		if _, duplicate := values[mapping.Target]; duplicate {
			return nil, "", ErrDefinitionInvalid
		}
		var value any
		var err error
		if len(mapping.Literal) > 0 {
			value, err = core.DecodeFlowJSON(ctx, mapping.Literal)
		} else {
			value, err = trustedStateValue(ctx, p, state, mapping.Source, outputs, true)
		}
		if err != nil {
			return nil, "", err
		}
		values[mapping.Target] = value
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, "", err
	}
	input, err := CanonicalJSON(ctx, encoded)
	if err != nil {
		return nil, "", err
	}
	return input, flowInputHash(input), nil
}

// SelectBranch compares only declared typed program state. It cannot interpret
// natural-language completion claims or execute an author-provided expression.
func SelectBranch(ctx context.Context, p core.Principal, state TrustedState, branch Branch, outputs OutputRevalidator) (string, error) {
	if !p.Valid() || state.Task.CallerID != p.CallerID {
		return "", core.ErrUnauthorized
	}
	actual, err := trustedStateValue(ctx, p, state, branch.Field, outputs, false)
	if err != nil {
		return "", err
	}
	expected, err := core.DecodeFlowJSON(ctx, branch.Value)
	if err != nil {
		return "", err
	}
	matched := false
	if branch.Op == "eq" || branch.Op == "ne" {
		left, err := CanonicalJSON(ctx, mustJSON(actual))
		if err != nil {
			return "", err
		}
		right, err := CanonicalJSON(ctx, mustJSON(expected))
		if err != nil {
			return "", err
		}
		matched = string(left) == string(right)
		if branch.Op == "ne" {
			matched = !matched
		}
	} else {
		left, ok := actual.(json.Number)
		if !ok {
			return "", ErrDefinitionInvalid
		}
		right, ok := expected.(json.Number)
		if !ok {
			return "", ErrDefinitionInvalid
		}
		a, ok := new(big.Rat).SetString(string(left))
		if !ok {
			return "", ErrDefinitionInvalid
		}
		b, ok := new(big.Rat).SetString(string(right))
		if !ok {
			return "", ErrDefinitionInvalid
		}
		switch branch.Op {
		case "lt":
			matched = a.Cmp(b) < 0
		case "lte":
			matched = a.Cmp(b) <= 0
		case "gt":
			matched = a.Cmp(b) > 0
		case "gte":
			matched = a.Cmp(b) >= 0
		default:
			return "", ErrDefinitionInvalid
		}
	}
	if matched {
		return branch.Then, nil
	}
	return branch.Else, nil
}

func trustedStateValue(ctx context.Context, p core.Principal, state TrustedState, field string, outputs OutputRevalidator, allowInput bool) (any, error) {
	parts := strings.Split(field, ".")
	if len(parts) == 3 && parts[0] == "slots" && parts[2] == "state" {
		slot, found := state.Task.Revision.Slots[parts[1]]
		if !found || slot.ID != parts[1] || !slices.Contains([]string{"missing", "found", "conflict", "unverifiable", "verified"}, slot.State) || slot.State == "verified" && len(slot.VerificationIDs) == 0 {
			return nil, ErrStateConflict
		}
		return slot.State, nil
	}
	if len(parts) == 2 && parts[0] == "budget" {
		value := state.Remaining.Tokens
		if parts[1] == "remaining_tool_calls" {
			value = state.Remaining.ToolCalls
		} else if parts[1] != "remaining_tokens" {
			return nil, ErrDefinitionInvalid
		}
		if value < 0 {
			return nil, ErrBudget
		}
		return json.Number(strconv.FormatInt(value, 10)), nil
	}
	if len(parts) == 2 && parts[0] == "attempts" {
		for _, node := range state.Task.AcceptedVersion.Definition.Nodes {
			if node.ID == parts[1] {
				return json.Number(strconv.Itoa(state.Task.Revision.Attempts[node.ID])), nil
			}
		}
		return nil, ErrDefinitionInvalid
	}
	var content json.RawMessage
	var path []string
	if len(parts) > 1 && parts[0] == "input" && allowInput {
		content, path = state.Task.Revision.Input, parts[1:]
	} else if len(parts) > 2 && parts[0] == "outputs" {
		output, found := state.Task.Revision.Outputs[parts[1]]
		if !found || !output.Verified || output.NodeAttemptID == "" || output.Revision > state.Task.Version.Revision || outputs == nil {
			return nil, ErrStateConflict
		}
		canonical, err := CanonicalJSON(ctx, output.Value)
		if err != nil || flowInputHash(canonical) != output.Hash {
			return nil, ErrStateConflict
		}
		if err := outputs.CheckOutput(ctx, p, state.Task, output); err != nil {
			return nil, err
		}
		content, path = canonical, parts[2:]
	} else {
		return nil, ErrDefinitionInvalid
	}
	value, err := core.DecodeFlowJSON(ctx, content)
	if err != nil {
		return nil, err
	}
	for _, part := range path {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("flow mapped value is not an object")
		}
		value, ok = object[part]
		if !ok {
			return nil, errors.New("flow mapped field is absent")
		}
	}
	return value, nil
}

func mustJSON(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }

func flowInputHash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
