package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// ErrFlowProgress is a policy limit, never proof that an external action failed.
var ErrFlowProgress = errors.New("flow progress limit reached")

// ProgressWindow is a bounded durable node history. It contains hashes only;
// changing revisions or the wording of a result cannot reset this history.
type ProgressWindow struct {
	Calls       []string `json:"calls"`
	NoProgress  int      `json:"no_progress"`
	Basis       []string `json:"basis"`
	LastAuditID int64    `json:"last_audit_id,omitempty"`
}

// ProgressLimitError retains a safe closed reason for Run limit_reached.
type ProgressLimitError struct{ Reason string }

func (e *ProgressLimitError) Error() string { return e.Reason }
func (e *ProgressLimitError) Unwrap() error { return ErrFlowProgress }

// ProgressSignature normalizes semantic arguments without retaining raw input.
func ProgressSignature(ctx context.Context, call ToolCall) (string, error) {
	input, err := CanonicalFlowJSON(ctx, []byte(call.Arguments))
	if err != nil || call.Name == "" {
		return "", ErrFlowDispatch
	}
	sum := sha256.Sum256(append(append([]byte(call.Name), 0), input...))
	return hex.EncodeToString(sum[:]), nil
}

// CheckCandidate rejects the threshold call before it is dispatched. Completed
// observations, rather than result bytes, determine the no-progress limit.
func (w ProgressWindow) CheckCandidate(signature string) (string, error) {
	if signature == "" || len(w.Calls) > 16 || w.NoProgress < 0 || len(w.Basis) > 1024 {
		return "history_invalid", ErrFlowProgress
	}
	if w.NoProgress >= 6 {
		return "no_progress", ErrFlowProgress
	}
	n := len(w.Calls)
	if n >= 2 && w.Calls[n-1] == signature && w.Calls[n-2] == signature {
		return "repeated_call", ErrFlowProgress
	}
	if n >= 5 && w.Calls[n-1] != signature && w.Calls[n-2] == signature && w.Calls[n-3] == w.Calls[n-1] && w.Calls[n-4] == signature && w.Calls[n-5] == w.Calls[n-1] {
		return "alternating_calls", ErrFlowProgress
	}
	return "", nil
}

// Observe only accepts a real, terminal-audit-confirmed dispatch. A result hash
// or a model's claim of progress does not supply a verified progress basis.
func (w ProgressWindow) Observe(signature string, outcome ActionOutcome) (ProgressWindow, error) {
	if signature == "" || !outcome.Dispatched || !outcome.AuditConfirmed || len(w.Calls) > 16 || w.NoProgress < 0 || w.NoProgress > 6 {
		return w, ErrFlowProgress
	}
	w.Calls = append(append([]string(nil), w.Calls...), signature)
	if len(w.Calls) > 16 {
		w.Calls = w.Calls[len(w.Calls)-16:]
	}
	w.NoProgress++
	return w, nil
}

// AcceptBasis is called only by a trusted source/slot/claim or cursor validator.
// Repeating a previously seen basis, including across revisions, does not help.
func (w ProgressWindow) AcceptBasis(basis string) (ProgressWindow, error) {
	if basis == "" || len(basis) > 256 || len(w.Basis) >= 1024 {
		return w, ErrFlowProgress
	}
	duplicate := false
	parts := strings.Split(basis, ":")
	for _, old := range w.Basis {
		if old == basis {
			return w, nil
		}
		previous := strings.Split(old, ":")
		if len(parts) == 3 && parts[0] == "file" && len(previous) == 3 && previous[0] == "file" && (parts[1] == previous[1] || parts[2] == previous[2]) {
			duplicate = true
		}
	}
	w.Basis = append(append([]string(nil), w.Basis...), basis)
	// 别名虽无新进展也记住其路径关系，避免之后替换文件绕过累计去重。
	// Retain aliases even without progress so later replacement cannot mint a source.
	if !duplicate {
		w.NoProgress = 0
	}
	return w, nil
}
