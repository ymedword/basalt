package core

// 本文件是「资源连接测试最后结果」的领域类型与端口（R7 范围修订、schema v4 的
// `resource_connection_tests`）。它只保留**最后一次实测投影**：权威历史仍在
// `management_operations`（每次测试一行、终态 detail 可查），本表让列表页一次读齐
// 「上回测得怎样、什么时候测的」。
//
// 两条纪律（用户 review 修正 #3）：
//   - `availability`（本地配置就绪）与「连接测试成功」是两件事，**不得**互相冒充；
//   - `unknown` 是独立终态（超时 / 传输歧义），**不得**折叠成失败，也**不得**据此重发。
//
// This file declares the last-connection-test projection stored in
// resource_connection_tests. The authoritative history stays in management_operations;
// local readiness and a successful live test are separate facts, and unknown never
// collapses into failure nor triggers a resend.

import (
	"context"
	"time"
)

// 连接测试状态三值（`resource_connection_tests.status` 的 CHECK 子句逐字）。
// The three status literals checked by the v4 table.
const (
	// ConnectionTestSuccess 表示服务端已确认受理（飞书还要求业务回执 code=0）。
	ConnectionTestSuccess = "success"
	// ConnectionTestFailed 表示确定失败：请求未发出，或服务端明确拒绝。
	ConnectionTestFailed = "failed"
	// ConnectionTestUnknown 表示结果未知（超时 / 传输歧义）：可能已送达，不得自动重发。
	ConnectionTestUnknown = "unknown"
)

// 本地配置就绪三值（视图字段 `availability`）。它只回答「本机配置看起来能不能用」，
// **不**回答「实测可用」——实测结论与时间戳在 `last_test`，两者分开显示。
// The three local-readiness literals; they never stand for a successful live test.
const (
	// AvailabilityReady 表示地址引用与凭据身份就绪（env 值已注入 / 托管凭据 active）。
	AvailabilityReady = "ready"
	// AvailabilityUnconfigured 表示根本没有可发送的地址配置（创建时省略了地址）。
	AvailabilityUnconfigured = "unconfigured"
	// AvailabilityUnavailable 表示有配置但当前不可用（凭据撤销 / env 值缺失）。
	AvailabilityUnavailable = "unavailable"
)

// NotifyConnectivityTestText 是渠道「测试连接」发送的固定文案：它与定时代投的实际
// 业务正文不同，接收方一眼能区分"管理员做的连通性测试"与真实通知。
// The fixed connectivity-test text; receivers can tell it apart from real delivery.
const NotifyConnectivityTestText = "Basalt 连通性测试"

// NotifyChannelTestResult 是一次渠道连通性测试的脱敏结果。`Status` 取
// success / failed / unknown 三值；`Detail` 只放固定短语（目标 host、飞书回执码、
// 变量名），不含地址原值与凭据。
// NotifyChannelTestResult is the redacted connectivity-test outcome.
type NotifyChannelTestResult struct {
	OK        bool
	Status    string
	LatencyMS int64
	Detail    string
}

// ResourceConnectionTest 是一条「最后一次连接测试」投影。
// `Kind` 取 `model_connection` / `notify_channel`（与 schema CHECK 同一集合）；
// `Detail` 只放脱敏固定短语——地址原值、凭据、响应片段都不得进入。
// ResourceConnectionTest is the last-test projection; Detail stays a redacted phrase.
type ResourceConnectionTest struct {
	Kind        ResourceKind
	ResourceUID ResourceUID
	TestedAt    time.Time
	Status      string
	Detail      string
	OperationID string
}

// ResourceConnectionTestStore 是投影的读写端口（实现在 internal/storage）。
// `Record` 是 upsert：每个资源只保留最后一次；历史以管理操作台账为准。
// `List` 按资源种类一次取齐，供列表页做 UID → 结果映射。
// The store upserts one row per resource and lists a whole kind at once.
type ResourceConnectionTestStore interface {
	RecordConnectionTest(ctx context.Context, rec ResourceConnectionTest) error
	ListConnectionTests(ctx context.Context, kind ResourceKind) ([]ResourceConnectionTest, error)
}
