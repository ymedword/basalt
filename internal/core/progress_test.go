package core

import (
	"context"
	"errors"
	"testing"
)

func TestFlow_RepetitionAndAlternationStop(t *testing.T) {
	for _, tc := range []struct {
		name    string
		calls   []string
		reason  string
		allowed int
	}{
		{"repeat", []string{"a", "a", "a"}, "repeated_call", 2},
		{"alternate", []string{"a", "b", "a", "b", "a", "b"}, "alternating_calls", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w ProgressWindow
			count := 0
			for _, s := range tc.calls {
				reason, err := w.CheckCandidate(s)
				if err != nil {
					if !errors.Is(err, ErrFlowProgress) || reason != tc.reason {
						t.Fatalf("reason=%s err=%v", reason, err)
					}
					break
				}
				count++
				w, err = w.Observe(s, ActionOutcome{Dispatched: true, AuditConfirmed: true, Outcome: "completed"})
				if err != nil {
					t.Fatal(err)
				}
			}
			if count != tc.allowed {
				t.Fatalf("dispatched=%d want=%d", count, tc.allowed)
			}
		})
	}
}

func TestFlow_NoProgressAcrossRevision(t *testing.T) {
	var w ProgressWindow
	for _, sig := range []string{"a", "b", "c", "d", "e", "f"} {
		var err error
		w, err = w.Observe(sig, ActionOutcome{Dispatched: true, AuditConfirmed: true, OutputHash: sig})
		if err != nil {
			t.Fatal(err)
		}
	}
	if reason, err := w.CheckCandidate("g"); reason != "no_progress" || !errors.Is(err, ErrFlowProgress) {
		t.Fatal(reason, err)
	}
	// A reconstructed checkpoint retains both the calls and the completed count.
	copy := ProgressWindow{Calls: append([]string(nil), w.Calls...), NoProgress: w.NoProgress, Basis: []string{"source@1"}}
	copy, err := copy.AcceptBasis("source@1")
	if err != nil || copy.NoProgress != 6 {
		t.Fatal(copy, err)
	}
	if _, err := copy.Observe("g", ActionOutcome{Dispatched: true}); err == nil {
		t.Fatal("unknown observation accepted")
	}
}

func TestFlow_DeclaredPaginationMakesProgress(t *testing.T) {
	var w ProgressWindow
	for _, cursor := range []string{"1", "2", "3", "4", "5", "6", "7"} {
		sig, err := ProgressSignature(context.Background(), ToolCall{Name: "page", Arguments: `{"cursor":"` + cursor + `"}`})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.CheckCandidate(sig); err != nil {
			t.Fatal(err)
		}
		w, err = w.Observe(sig, ActionOutcome{Dispatched: true, AuditConfirmed: true})
		if err != nil {
			t.Fatal(err)
		}
		// This fact is supplied by the code-owned monotonic cursor validator.
		w, err = w.AcceptBasis("validated-cursor:" + cursor)
		if err != nil {
			t.Fatal(err)
		}
	}
	if w.NoProgress != 0 {
		t.Fatal(w)
	}
	a, _ := ProgressSignature(context.Background(), ToolCall{Name: "p", Arguments: `{"n":1.00}`})
	b, _ := ProgressSignature(context.Background(), ToolCall{Name: "p", Arguments: `{"n":1}`})
	if a != b {
		t.Fatal("semantic arguments differ")
	}
}

func TestFlow_FileReplacementAndAliasesCannotResetNoProgress(t *testing.T) {
	w := ProgressWindow{NoProgress: 5}
	var err error
	w, err = w.AcceptBasis("file:path-a:inode-a")
	if err != nil || w.NoProgress != 0 {
		t.Fatal("first source must supply progress", w, err)
	}
	w.NoProgress = 2
	w, err = w.AcceptBasis("file:path-b:inode-a")
	if err != nil || w.NoProgress != 2 || len(w.Basis) != 2 {
		t.Fatal("alias must be remembered without resetting progress", w, err)
	}
	w, err = w.AcceptBasis("file:path-b:inode-b")
	if err != nil || w.NoProgress != 2 {
		t.Fatal("replacing a remembered alias cannot mint a new source", w, err)
	}
	w, err = w.AcceptBasis("file:path-a:inode-c")
	if err != nil || w.NoProgress != 2 {
		t.Fatal("atomic replacement at an old path cannot supply progress", w, err)
	}
	w, err = w.AcceptBasis("file:path-c:inode-d")
	if err != nil || w.NoProgress != 0 {
		t.Fatal("independent path and physical source must still make progress", w, err)
	}
}
