package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type us20IsolationProbe struct {
	report IsolationCheckReport
	err    error
	calls  int
}

func (p *us20IsolationProbe) CheckIsolation(context.Context) (IsolationCheckReport, error) {
	p.calls++
	return p.report, p.err
}

func TestUS20_IsolationCLI_Check(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		err     error
	}{
		{"disabled", false, nil}, {"ready", true, nil}, {"runtime_missing", true, errors.New("not_ready")},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := &us20IsolationProbe{report: IsolationCheckReport{Enabled: test.enabled, Backend: "docker", Profiles: []IsolationCheckProfile{{Name: "gvisor", Runtime: "runsc", Bound: true, Available: test.err == nil}}, Tools: map[string]string{"run_code": "gvisor"}}, err: test.err}
			cmd := NewRootCommand(Deps{IsolationProbe: p})
			var out bytes.Buffer
			var diagnostic bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&diagnostic)
			cmd.SetArgs([]string{"isolation", "check"})
			err := cmd.ExecuteContext(context.Background())
			if (err != nil) != (test.err != nil) || p.calls != 1 {
				t.Fatalf("exit/calls: %v/%d", err, p.calls)
			}
			var got IsolationCheckReport
			if json.Unmarshal(out.Bytes(), &got) != nil || got.Enabled != test.enabled || len(got.Profiles) != 1 {
				t.Fatalf("report %s", out.String())
			}
		})
	}
	cmd := NewRootCommand(Deps{})
	cmd.SetArgs([]string{"isolation", "check"})
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("missing probe accepted")
	}
}
