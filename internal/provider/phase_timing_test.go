package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
)

type timingRoundTrip func(*http.Request) (*http.Response, error)

func (f timingRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type timingReadSignal struct {
	io.ReadCloser
	once    sync.Once
	started chan struct{}
}

func (b *timingReadSignal) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.ReadCloser.Read(p)
}

func TestPhaseTiming_NonStreamingFirstResponseIsSeparateFromBodyRead(t *testing.T) {
	release, readStarted := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] == true {
			t.Error("timing must preserve the non-streaming protocol")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok","reasoning_content":"retained","custom":"retained"}}]}`)
	}))
	defer func() { releaseOnce.Do(func() { close(release) }); srv.Close() }()
	c, err := NewOpenAIClient(Options{Name: "p", BaseURL: srv.URL, APIKey: "synthetic"})
	require.NoError(t, err)
	c.http.Transport = timingRoundTrip(func(r *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err == nil {
			resp.Body = &timingReadSignal{ReadCloser: resp.Body, started: readStarted}
		}
		return resp, err
	})
	capture := &core.ModelTiming{}
	done := make(chan error, 1)
	go func() {
		resp, err := c.Chat(t.Context(), core.ChatRequest{Model: "m", Timing: capture})
		if err == nil && (resp.Content != "ok" || resp.ReasoningContent != "retained" || resp.ProviderExtra["custom"] != "retained") {
			err = io.ErrUnexpectedEOF
		}
		done <- err
	}()
	select {
	case <-readStarted:
	case err := <-done:
		require.NoError(t, err)
		t.Fatal("response read never started")
	case <-time.After(5 * time.Second):
		t.Fatal("response header capture stalled")
	}
	require.NotNil(t, capture.Wait)
	require.Equal(t, "completed", capture.Wait.Status)
	require.Nil(t, capture.Read, "headers have arrived while the JSON body is still blocked")
	releaseOnce.Do(func() { close(release) })
	require.NoError(t, <-done)
	require.Equal(t, "completed", capture.Read.Status)
	require.False(t, capture.Read.StartedAt.Before(capture.Wait.FinishedAt))
}

type timingStore struct{ finished bool }

func (*timingStore) BeginCall(context.Context, core.LLMCall) (int64, error) { return 71, nil }
func (s *timingStore) FinishCall(context.Context, int64, core.LLMCallResult) error {
	s.finished = true
	return nil
}

type timingClient func(context.Context, core.ChatRequest) (core.ChatResponse, error)

func (f timingClient) Chat(ctx context.Context, r core.ChatRequest) (core.ChatResponse, error) {
	return f(ctx, r)
}

type timingSink func(context.Context, core.PhaseTiming)

func (f timingSink) RecordPhase(ctx context.Context, p core.PhaseTiming) { f(ctx, p) }

func TestPhaseTiming_AuditCorrelationAndSingleCall(t *testing.T) {
	store := &timingStore{}
	calls := 0
	var got []core.PhaseTiming
	client := timingClient(func(_ context.Context, r core.ChatRequest) (core.ChatResponse, error) {
		calls++
		require.NotNil(t, r.Timing)
		at := time.Now()
		r.Timing.Wait = &core.PhaseTiming{Phase: core.PhaseFirstResponse, Parent: "model_call", Status: "completed", StartedAt: at, FinishedAt: at, Duration: 0}
		return core.ChatResponse{Content: "ok"}, nil
	})
	sink := timingSink(func(_ context.Context, p core.PhaseTiming) {
		require.True(t, store.finished, "phase evidence must follow the audit terminal write")
		got = append(got, p)
	})
	service := NewProviderService(map[string]core.ProviderClient{"p": client}, map[string]string{"p": "m"}, store, nil)
	resp, err := service.Chat(t.Context(), core.ChatRequest{Provider: "p", RunID: "run-timing", Round: 2, Phases: sink})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Content)
	require.Equal(t, 1, calls)
	require.Len(t, got, 1)
	require.Equal(t, int64(71), got[0].CallID)
	require.Equal(t, "2", got[0].StepID)
	require.Equal(t, "run-timing", got[0].RunID)
}
