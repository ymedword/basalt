package testutil

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// postChat 向假服务的 chat/completions 端点发一次最小 POST，回读状态与响应体。
// postChat sends one minimal POST to the fake chat/completions endpoint and returns
// the status and body.
func postChat(t *testing.T, base string) (int, string) {
	t.Helper()
	resp, err := http.Post(base+"/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","n":7,"messages":[]}`))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// TestFakeProvider_ReplaysScriptedResponses 是假客户端的自证用例（文档用例名：
// TestFakeProvider_ReplaysScriptedResponses——基础设施自证）：按序回放响应与错误，
// 并如实记录收到的请求；脚本用尽不再是"静默成功"。
// TestFakeProvider_ReplaysScriptedResponses proves the fake itself: it replays
// responses and errors in order, records what it received, and refuses to invent a
// step once the script runs out.
func TestFakeProvider_ReplaysScriptedResponses(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	fake := NewFakeProvider(
		FakeStep{Response: core.ChatResponse{Content: "first"}},
		FakeStep{Err: boom},
	)
	require.Equal(t, 0, fake.CallCount())

	first, err := fake.Chat(ctx, core.ChatRequest{Provider: "fake", Model: "m"})
	require.NoError(t, err)
	require.Equal(t, "first", first.Content)

	_, err = fake.Chat(ctx, core.ChatRequest{Provider: "fake", Model: "m2"})
	require.ErrorIs(t, err, boom)

	_, err = fake.Chat(ctx, core.ChatRequest{Provider: "fake"})
	require.Error(t, err, "脚本用尽必须报错——少写一步的用例不该静默通过")

	require.Equal(t, 3, fake.CallCount())
	reqs := fake.Requests()
	require.Len(t, reqs, 3)
	require.Equal(t, "m", reqs[0].Model, "请求按到达顺序记录")
	require.Equal(t, "m2", reqs[1].Model)
}

// TestFakeProvider_SignalsCallStartForDeterministicCancelSynchronization：
// Started(n) 的信号语义——调用进入即关通道（取消用例的同步基元自己先被判卷；
// 它若坏掉，用例表现为挂起或时紧时松，属最难查的一类）。
func TestFakeProvider_SignalsCallStartForDeterministicCancelSynchronization(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := NewFakeProvider(FakeStep{Block: true})

	started := fake.Started(1)
	select {
	case <-started:
		t.Fatal("调用尚未发生，信号通道不得已关闭")
	default:
	}

	done := make(chan error, 1)
	go func() {
		_, err := fake.Chat(ctx, core.ChatRequest{Provider: "fake"})
		done <- err
	}()

	<-started // 调用确已进入
	require.Equal(t, 1, fake.CallCount(), "信号到达时请求必须已被记录")
	cancel()

	require.ErrorIs(t, <-done, context.Canceled, "阻塞步在取消后以 ctx 错误返回")
	require.Equal(t, ctx.Err(), context.Canceled)
}

// TestFakeOpenAIServer_RecordsRequestsAndServesCannedResponse：假服务本身必须先被判卷——
// 按脚本回放 HTTP 状态与响应体，并记录收到的请求体（供逐键断言）。
func TestFakeOpenAIServer_RecordsRequestsAndServesCannedResponse(t *testing.T) {
	srv := NewFakeOpenAIServer(t,
		FakeOpenAIResponse{Body: `{"choices":[]}`}, // StatusCode 0 → 200
		FakeOpenAIResponse{StatusCode: http.StatusUnauthorized, Body: `{"error":{"message":"nope"}}`},
	)

	status, body := postChat(t, srv.URL())
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, `{"choices":[]}`, body)

	status, _ = postChat(t, srv.URL())
	require.Equal(t, http.StatusUnauthorized, status)

	reqs := srv.Requests()
	require.Len(t, reqs, 2)
	require.Equal(t, "m", reqs[0]["model"])
	require.Equal(t, float64(7), reqs[0]["n"])
}
