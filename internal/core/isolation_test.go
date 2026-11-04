package core

import (
	"context"
	"errors"
	"testing"
)

func TestUS20_CommandZeroValue(t *testing.T) {
	if (Command{}).Isolation != nil || (CommandResult{}).Execution != nil {
		t.Fatal("零值命令必须保持本机兼容")
	}
}

func TestUS20_ErrorKindMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want ErrorKind
	}{
		{&SandboxViolationError{Rule: "隔离拒绝", Target: "config_missing"}, ErrorKindSandboxDenied},
		{context.DeadlineExceeded, ErrorKindTimeout},
		{context.Canceled, ErrorKindTimeout},
		{&InvalidInputError{Tool: "run_code", Reason: "input_rejected"}, ErrorKindInvalidInput},
		{errors.New("create_unknown"), ErrorKindRuntimeError},
	} {
		if got := classifyToolError(tc.err); got != tc.want {
			t.Fatalf("隔离不得新增错误类别：got %s want %s", got, tc.want)
		}
	}
}

func TestUS20_PolicyDenialReasonUnchanged(t *testing.T) {
	err := &SandboxViolationError{Rule: "隔离拒绝", Target: "image_untrusted"}
	if err.Reason != "" {
		t.Fatal("隔离拒绝不得占用出网 reason")
	}
	if ActionRunCode != "RUN_CODE" {
		t.Fatal("动作字面量漂移")
	}
}
