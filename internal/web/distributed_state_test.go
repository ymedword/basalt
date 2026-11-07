package web

// 本文件是 US-24 在 HTTP 面的两处只读 / 只映射判据（模块卡 §3.1.13 / §3.1.14，
// T030）：
//
//	W1 `TestWeb_FlowRootBusyIs409`——执行者持有根租约时，管理面推进请求必须映射
//	   409 `FLOW_ROOT_BUSY`：resume 的冲突发生在后端调用内（不是参数校验短路），
//	   共享映射 `writeFlowError` 对包裹错误同样成立（revise 的推进写入走同一处），
//	   且相邻的未决动作错误不串档成同一个码。
//	W2 `TestWeb_RunViewLeaseFields`——运行视图的三个只读字段
//	   （`worker_instance_id` / `ownership_epoch` / `asset_bundle_hash`）原样投影：
//	   有租约 / v3 清单时给出真实值，历史 Run 输出空串与 0（字段恒在，不省略、
//	   不补造）；列表视图同样带这三个字段（列表不解析清单正文，值为空 = 未记录）。
//
// Acceptance for the two US-24 HTTP-facing readings: the root-lease conflict maps to
// 409 FLOW_ROOT_BUSY, and the run view projects the three lease / bundle fields
// verbatim with the keys always present.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// TestWeb_FlowRootBusyIs409 固定根租约冲突的线上契约。
func TestWeb_FlowRootBusyIs409(t *testing.T) {
	probe := &flowHandlerProbe{view: flowProbeView(), failure: flow.ErrFlowRootBusy}
	srv := newTestServer(t, func(o *Options) { o.Flows = probe })

	// ① resume 路径：请求参数合法，冲突来自后端（根租约被另一个执行者持有）。
	r := httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"operation_id":"resume-1","reason":"worker_lease_expired"}`))
	r.SetPathValue("id", "ft_fixture")
	r.Header.Set("If-Match", probe.view.ETag)
	w := httptest.NewRecorder()
	srv.handleFlowResume(w, r, callerPrincipal("owner", core.ScopeFlowsWrite))

	require.Equal(t, 1, probe.controls, "冲突发生在后端调用内，不是参数校验失败")
	require.Equal(t, probe.view.Version, probe.expected)
	require.Equal(t, http.StatusConflict, w.Code, "根租约冲突不得落成 500")
	var resumeEnvelope Envelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resumeEnvelope))
	require.Equal(t, "FLOW_ROOT_BUSY", resumeEnvelope.Code)
	require.NotEqual(t, codeInternal, resumeEnvelope.Code)

	// ② 共享映射：revise 等推进类写入经同一个 writeFlowError 落地；包裹后的错误
	// 必须仍按 errors.Is 命中，不得退回 500。
	for _, err := range []error{
		flow.ErrFlowRootBusy,
		fmt.Errorf("revise: %w", flow.ErrFlowRootBusy),
	} {
		recorder := httptest.NewRecorder()
		srv.writeFlowError(recorder, err)
		require.Equal(t, http.StatusConflict, recorder.Code)
		var envelope Envelope
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
		require.Equal(t, "FLOW_ROOT_BUSY", envelope.Code)
	}

	// ③ 相邻错误不串档：未决动作仍是 FLOW_ACTION_UNKNOWN，不因同属"根上有事"合并。
	recorder := httptest.NewRecorder()
	srv.writeFlowError(recorder, flow.ErrActionUnknown)
	var unknown Envelope
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &unknown))
	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Equal(t, "FLOW_ACTION_UNKNOWN", unknown.Code)

	// ④ 非租约类失败不得被误判成 409：证明上面的映射不是"一律 409"。
	recorder = httptest.NewRecorder()
	srv.writeFlowError(recorder, errors.New("storage exploded"))
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
}

// TestWeb_RunViewLeaseFields 固定运行视图三个只读字段的投影与字段集。
func TestWeb_RunViewLeaseFields(t *testing.T) {
	base := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	bundleHash := strings.Repeat("ab", 32)
	workerInstance := "abcdef0123456789abcdef0123456789"
	recorded := core.RunDetail{
		RunRecord: core.RunRecord{
			RunID: "run-24", SessionID: "s-1", Agent: "writer",
			Channel: "web", Trigger: core.Trigger("api"), StartedAt: base,
		},
		Status:           core.StatusCompleted,
		ManifestHash:     "manifest-hash-24",
		FinishedAt:       base.Add(time.Minute),
		WorkerInstanceID: workerInstance,
		OwnershipEpoch:   7,
		AssetBundleHash:  bundleHash,
		ResourceManifest: &core.RunResourceManifest{
			FormatVersion:   3,
			AgentUID:        core.AgentUID("ag_0123456789ABCDEFGHJKMNPQRS"),
			AssetBundleHash: bundleHash,
		},
	}
	legacy := core.RunDetail{
		RunRecord: core.RunRecord{
			RunID: "run-legacy", SessionID: "s-0", Agent: "writer",
			Channel: "web", Trigger: core.Trigger("api"), StartedAt: base,
		},
		Status:       core.StatusFailed,
		ManifestHash: "manifest-hash-old",
		FinishedAt:   base.Add(time.Minute),
	}

	fake := &runLedgerFake{detail: recorded, listed: []core.RunDetail{recorded, legacy}}
	srv := newRunLedgerServer(t, fake)

	// ① 详情：三项原样投影。
	rec := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/runs/run-24", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	detail := decodeRunEnvelope[RunDetailView](t, rec)
	require.Equal(t, workerInstance, detail.Data.WorkerInstanceID)
	require.Equal(t, int64(7), detail.Data.OwnershipEpoch)
	require.Equal(t, bundleHash, detail.Data.AssetBundleHash, "详情按 run 行投影 v3 清单固定的资产包哈希")

	// ② 字段集恒定：三个键总是出现，历史 Run 是空串 / 0，不省略也不补造。
	raw := map[string]any{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	data, ok := raw["data"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, data, "worker_instance_id")
	require.Contains(t, data, "ownership_epoch")
	require.Contains(t, data, "asset_bundle_hash")

	// ③ 列表：同一投影（列表不解析清单正文，历史行三项为零值形态）。
	rec = serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/runs", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	list := decodeRunEnvelope[[]RunView](t, rec)
	require.Len(t, list.Data, 2)
	require.Equal(t, workerInstance, list.Data[0].WorkerInstanceID)
	require.Equal(t, int64(7), list.Data[0].OwnershipEpoch)
	require.Empty(t, list.Data[1].WorkerInstanceID, "历史 Run 没有租约行：空串即未记录")
	require.Zero(t, list.Data[1].OwnershipEpoch)
	require.Empty(t, list.Data[1].AssetBundleHash)

	// ④ 取消响应也用 runRecordView：正在运行的 Run 被取消后同样带三个读数。
	fake.cancelView = recorded
	rec = serveAdmin(srv, adminRequest(http.MethodPost, "/api/v1/runs/run-24/cancel", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	cancelled := decodeRunEnvelope[RunView](t, rec)
	require.Equal(t, workerInstance, cancelled.Data.WorkerInstanceID)
	require.Equal(t, int64(7), cancelled.Data.OwnershipEpoch)
}
