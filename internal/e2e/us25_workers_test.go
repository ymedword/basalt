//go:build e2e

package e2e

// US-25 容器验收 · 两个 Worker 同时登记（FR-25-18）。
//
// 控制面启动并导出共享主密钥与种子之后，起两个 Worker（基础服务 worker-a + 夹具追加
// 的 worker-b）；两者应在 worker_nodes 各有一行、InstanceID 互不相同、心跳在 TTL 内，
// 且 Worker 容器内的 `workers list` 不经过控制面就能读到这两行。
//
// Two workers on one seeded workspace: one row each, distinct instance ids, fresh
// heartbeats, and a read-only listing served from inside a worker container.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

// us25WorkerIDPattern 是 Worker 运行时实例标识的形态：`wrk_` + 32 位小写十六进制
// （internal/app/worker_runtime.go 的 newWorkerInstanceID）。
// The worker runtime instance-id shape.
var us25WorkerIDPattern = regexp.MustCompile(`^wrk_[0-9a-f]{32}$`)

// TestUS25_Workers_BothRegistered 判 FR-25-18。
//
// 逆改点：让两个 Worker 共用 InstanceID、心跳不续、或 workers list 只回一行 → 对应
// 断言红；让 Worker 在启动时改写名册 → 卷内 instance_id 断言红。
func TestUS25_Workers_BothRegistered(t *testing.T) {
	s := us25NewStack(t, "workers")
	instanceID := s.startControl(t)
	keyPath, seedPath := s.exportControlAssets(t)

	// 夹具保护：本用例要判"恰好两行"，先把控制面自己留下的登记行清掉。`serve` 与
	// `gateway` 都走 `startWorkerRuntime`（`run.go` 的 `needsSchedules` 覆盖两者），
	// 所以控制面也会占一行；清掉它之后，两行只能来自本用例起的两个 Worker。
	// 心跳是 UPDATE、不是 upsert，控制面不会把被清掉的行重新插回来。
	// Fixture guard: both serve and gateway register, so clear the control's own row first;
	// the heartbeat path only updates, so it will not re-insert.
	s.psql(t, "DELETE FROM worker_nodes")

	s.addWorkerService(t, us25WorkerSpec{
		Service: "worker-b", VolumePrefix: "workerb", Name: "worker-b", KeyHostPath: keyPath,
	})
	s.seedVolume(t, us25WorkerService, "worker-workspace", seedPath)
	s.seedVolume(t, "worker-b", "workerb-workspace", seedPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := s.up(ctx, "worker"); err != nil {
		t.Fatalf("启动两个 Worker 失败：%v", err)
	}

	// ① 两个 Worker 各登记一行。
	waitFor(t, us25StartTimeout, func() bool {
		return s.psql(t, "SELECT count(*) FROM worker_nodes") == "2"
	})
	if stopped := s.psql(t, "SELECT count(*) FROM worker_nodes WHERE stopped_at IS NOT NULL"); stopped != "0" {
		t.Fatalf("有 %s 行 Worker 已停机（要求 0——两个都应在线）", stopped)
	}

	// ② InstanceID 互不相同且形态正确。
	ids := us18NonEmptyLines(s.psql(t, "SELECT instance_id FROM worker_nodes ORDER BY instance_id"))
	if len(ids) != 2 {
		t.Fatalf("worker_nodes 有 %d 个实例标识（要求 2）：%v", len(ids), ids)
	}
	for _, id := range ids {
		if !us25WorkerIDPattern.MatchString(id) {
			t.Fatalf("实例标识 %q 不是 wrk_ + 32 位小写十六进制", id)
		}
	}
	if ids[0] == ids[1] {
		t.Fatalf("两个 Worker 的 InstanceID 相同：%s", ids[0])
	}

	// ③ 名称按配置落库（同名允许，但本用例两者不同）。
	names := s.psql(t, "SELECT string_agg(worker_name, ',' ORDER BY worker_name) FROM worker_nodes")
	if names != "worker-a,worker-b" {
		t.Fatalf("worker_nodes 的名称集合是 %q（要求 worker-a,worker-b）", names)
	}

	// ④ 心跳在 TTL（30s）内：两个都要fresh。
	fresh := s.psql(t,
		"SELECT count(*) FROM worker_nodes "+
			"WHERE last_heartbeat_at > (extract(epoch from clock_timestamp())*1000)::bigint - 30000")
	if fresh != "2" {
		beat := s.psql(t, "SELECT string_agg(last_heartbeat_at::text, ',' ORDER BY instance_id) FROM worker_nodes")
		t.Fatalf("心跳在 30s 内的 Worker 有 %s 个（要求 2；实际心跳 %s）", fresh, beat)
	}

	// ⑤ Worker 容器内只读列表：表头 + 两行；--json 是长度为 2 的裸数组。
	listArgv := []string{
		"sh", "-c",
		`export BASALT_DATABASE_DSN="$(cat /run/secrets/database_dsn)"; exec /opt/basalt/basalt workers list`,
	}
	listOut, err := s.exec(ctx, us25WorkerService, listArgv...)
	if err != nil {
		t.Fatalf("Worker 容器内 workers list 失败：%v（%s）", err, us18Tail(listOut, 2000))
	}
	// CLI 在同一条流上先打启动日志（`出网受控拨号已装配`），表头之后才是数据行：
	// 先定位表头，再判表头后恰好两行——不把日志行算进表格。
	// The CLI writes a startup log line to the same stream; locate the header first.
	lines := us18NonEmptyLines(listOut)
	header := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "INSTANCE") {
			header = i
			break
		}
	}
	if header < 0 {
		t.Fatalf("workers list 输出里没有表头：\n%s", us18Tail(listOut, 3000))
	}
	if data := lines[header+1:]; len(data) != 2 {
		t.Fatalf("workers list 表头后有 %d 行（要求 2）：\n%s", len(data), us18Tail(listOut, 3000))
	}
	for _, id := range ids {
		if !strings.Contains(listOut, id) {
			t.Fatalf("workers list 没有列出实例 %s：\n%s", id, us18Tail(listOut, 3000))
		}
	}

	jsonOut, err := s.exec(ctx, us25WorkerService, "sh", "-c",
		`export BASALT_DATABASE_DSN="$(cat /run/secrets/database_dsn)"; exec /opt/basalt/basalt workers list --json`)
	if err != nil {
		t.Fatalf("Worker 容器内 workers list --json 失败：%v（%s）", err, us18Tail(jsonOut, 2000))
	}
	payload := strings.TrimSpace(jsonOut)
	start := strings.Index(payload, "[")
	if start < 0 {
		t.Fatalf("workers list --json 输出不是数组：%s", us18Tail(jsonOut, 3000))
	}
	var nodes []struct {
		Instance string `json:"instance"`
		Name     string `json:"name"`
	}
	if err := json.Unmarshal([]byte(payload[start:]), &nodes); err != nil {
		t.Fatalf("解析 workers list --json 失败：%v（%s）", err, us18Tail(jsonOut, 3000))
	}
	if len(nodes) != 2 {
		t.Fatalf("workers list --json 返回 %d 项（要求 2）", len(nodes))
	}
	listed := make(map[string]string, len(nodes))
	for _, node := range nodes {
		listed[node.Instance] = node.Name
	}
	for _, id := range ids {
		if _, ok := listed[id]; !ok {
			t.Fatalf("workers list --json 缺少实例 %s（实际 %v）", id, listed)
		}
	}
	if listed[ids[0]] == listed[ids[1]] {
		t.Fatalf("两个实例在 JSON 里的名称相同：%q", listed[ids[0]])
	}

	// ⑥ 两个容器都在跑，基础 Worker 健康（健康检查 = 数据库可达 + 只读列表可读）。
	waitFor(t, us25StartTimeout, func() bool {
		return s.healthState(ctx, us25WorkerService) == "healthy"
	})
	if state := s.serviceState(ctx, "worker-b"); state != "running" {
		t.Fatalf("worker-b 状态 %q（要求 running）", state)
	}

	// ⑦ Worker 没有改写名册：三个卷里的 instance_id 都是控制面那一份。
	for _, volume := range []string{
		s.project + "_control-workspace",
		s.project + "_worker-workspace",
		s.project + "_workerb-workspace",
	} {
		roster, err := s.volumeRead(ctx, volume, "/.basalt/instance.json")
		if err != nil {
			t.Fatalf("读取 %s 里的名册失败：%v", volume, err)
		}
		if got := s.instanceID(t, roster); got != instanceID {
			t.Fatalf("%s 里的 instance_id=%s（要求与控制面一致 %s）", volume, got, instanceID)
		}
	}
}
