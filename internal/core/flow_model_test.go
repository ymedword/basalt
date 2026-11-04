package core

import (
	"math"
	"testing"
)

func TestFlow_ModelUsageNeverUnderstatesKnownComponents(t *testing.T) {
	for _, usage := range []Usage{{PromptTokens: 60, CompletionTokens: 40, TotalTokens: 1}, {PromptTokens: -1, CompletionTokens: 2, TotalTokens: 2}, {PromptTokens: math.MaxInt, CompletionTokens: math.MaxInt, TotalTokens: 1}} {
		if _, known, err := FlowUsageTotal(usage); err == nil || known {
			t.Fatalf("contradictory usage accepted: %+v", usage)
		}
	}
	for _, usage := range []Usage{{PromptTokens: 60, CompletionTokens: 40}, {PromptTokens: 60, CompletionTokens: 40, TotalTokens: 100}} {
		total, known, err := FlowUsageTotal(usage)
		if err != nil || !known || total != 100 {
			t.Fatalf("known components lost: %d %t %v", total, known, err)
		}
	}
	if total, known, err := FlowUsageTotal(Usage{}); err != nil || known || total != 0 {
		t.Fatal(total, known, err)
	}
}
