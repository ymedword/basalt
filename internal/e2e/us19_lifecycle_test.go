//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type us19Latency struct{ P50, P95, P99 float64 }

func us19Percentiles(values []float64) us19Latency {
	copy := slices.Clone(values)
	slices.Sort(copy)
	value := func(q float64) float64 {
		if len(copy) == 0 {
			return math.Inf(1)
		}
		return copy[max(0, int(math.Ceil(q*float64(len(copy))))-1)]
	}
	return us19Latency{value(.5), value(.95), value(.99)}
}

// SC-002：真实候选二进制，200独立任务／4active／32open／20节点／50ms回环模型。
// SC-002 uses the real candidate with immutable workload dimensions, not domain stubs.
func TestFlow_BinaryCapacity200TasksAndUnqueuedLatency(t *testing.T) {
	var modelCalls, concurrent, maximum atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		modelCalls.Add(1)
		now := concurrent.Add(1)
		defer concurrent.Add(-1)
		for old := maximum.Load(); now > old && !maximum.CompareAndSwap(old, now); old = maximum.Load() {
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
		w.Header().Set("Content-Type", "application/json")
		_, err = io.WriteString(w, usageResponse(`{"answer":"frozen local benchmark"}`, 5, 3).Body)
		if err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	u := newUS19Harness(t, server.URL, us19HarnessOptions{config: func(config string) string {
		return strings.Replace(config, "max_active: 2", "max_active: 4", 1) + "server:\n  max_concurrent_runs: 8\n"
	}})
	prototype := u.definition["nodes"].([]any)[0].(map[string]any)
	nodes := make([]any, 20)
	for i := range 20 {
		node := make(map[string]any, len(prototype)+1)
		for k, v := range prototype {
			node[k] = v
		}
		node["id"] = fmt.Sprintf("node-%02d", i)
		if i < 19 {
			node["next"] = fmt.Sprintf("node-%02d", i+1)
		}
		nodes[i] = node
	}
	u.definition["entry"], u.definition["nodes"] = "node-00", nodes
	u.definition["limits"].(map[string]any)["max_duration_seconds"] = 1800
	uid := u.publish(t)
	db := u.h.openDB(t)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		rows, err := db.Query(`SELECT status,reason,count(*) FROM flow_tasks GROUP BY status,reason ORDER BY status,reason`)
		if err == nil {
			for rows.Next() {
				var status, reason string
				var count int
				if err := rows.Scan(&status, &reason, &count); err == nil {
					t.Logf("failure task ledger status=%s reason=%s count=%d", status, reason, count)
				}
			}
			if err := rows.Close(); err != nil {
				t.Log(err)
			}
		}
		u.p.kill()
		u.p.waitExit(t, 10*time.Second)
		t.Logf("candidate stderr tail %s", u.h.redact(us19LogTail(u.p.stderr.String(), 8192)))
	})
	active := map[string]bool{}
	var accepted, reads, queue []float64
	started := time.Now()
	maxOpen, maxConnections, maxLocks := 0, 0, 0
	maxWaitingLocks := 0
	var maxLockWaitMS float64
	var maxRSS int64
	type resourceSample struct {
		Accepted, Connections, WaitingLocks int
		RSS, Goroutines                     *int64
		CPUPercent                          *float64
		ElapsedSeconds                      float64
	}
	var resources []resourceSample
	acceptedCount := 0
	for acceptedCount < 200 || len(active) > 0 {
		require.Less(t, time.Since(started), 12*time.Minute, "capacity workload failed to drain")
		for acceptedCount < 200 && len(active) < 32 {
			before := time.Now()
			resp := u.start(t, uid, fmt.Sprintf("capacity-%03d", acceptedCount))
			accepted = append(accepted, float64(time.Since(before).Microseconds())/1000)
			requireWebStatus(t, resp, 202, "OK", "", "accept fixed capacity workload")
			var task struct {
				ID string `json:"task_id"`
			}
			decodeWebData(t, resp, &task, "capacity task")
			require.NotEmpty(t, task.ID)
			active[task.ID] = true
			acceptedCount++
		}
		rows, err := db.Query(`SELECT task_id,status FROM flow_tasks`)
		require.NoError(t, err)
		open := 0
		for rows.Next() {
			var id, status string
			require.NoError(t, rows.Scan(&id, &status))
			if status == "succeeded" {
				delete(active, id)
			} else {
				require.Contains(t, []string{"queued", "running"}, status, "unexpected stop in fixed successful workload")
				open++
			}
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		maxOpen = max(maxOpen, open)
		for id := range active {
			before := time.Now()
			resp := webRequest(t, u.addr, http.MethodGet, "/api/v1/flow-tasks/"+id, "", u.headers(""))
			reads = append(reads, float64(time.Since(before).Microseconds())/1000)
			if resp.Status != 200 {
				t.Logf("capacity diagnostic: accepted=%d active=%d read_ms=%.3f active_keys=%d", acceptedCount, len(active), reads[len(reads)-1], scalarInt(t, db, `SELECT count(*) FROM caller_keys WHERE revoked_at IS NULL AND expires_at>extract(epoch from now())*1000`))
				activity, err := db.Query(`SELECT state,COALESCE(wait_event_type,''),COALESCE(wait_event,''),left(query,180) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid()`)
				require.NoError(t, err)
				for activity.Next() {
					var state, kind, event, query string
					require.NoError(t, activity.Scan(&state, &kind, &event, &query))
					t.Logf("PG state=%s wait=%s/%s query=%s", state, kind, event, query)
				}
				require.NoError(t, activity.Err())
				require.NoError(t, activity.Close())
				u.p.kill()
				u.p.waitExit(t, 10*time.Second)
				t.Logf("candidate diagnostic %s", u.h.redact(u.p.stderr.String()))
			}
			requireWebStatus(t, resp, 200, "OK", "", "read under capacity pressure")
			break
		}
		connections := scalarInt(t, db, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()`)
		locks := scalarInt(t, db, `SELECT count(*) FROM pg_locks l JOIN pg_class c ON c.oid=l.relation JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema()`)
		var waiting int
		var waitMS float64
		require.NoError(t, db.QueryRow(`SELECT count(*),COALESCE(max(extract(epoch FROM (clock_timestamp()-a.query_start))*1000),0) FROM pg_stat_activity a WHERE a.wait_event_type='Lock' AND a.pid IN (SELECT l.pid FROM pg_locks l JOIN pg_class c ON c.oid=l.relation JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema())`).Scan(&waiting, &waitMS))
		maxWaitingLocks, maxLockWaitMS = max(maxWaitingLocks, waiting), max(maxLockWaitMS, waitMS)
		maxConnections, maxLocks = max(maxConnections, connections), max(maxLocks, locks)
		sample := webRequest(t, u.addr, http.MethodGet, "/api/v1/system/sample", "", nil)
		requireWebStatus(t, sample, 200, "OK", "", "sample candidate process")
		var memory struct {
			RSS        *int64   `json:"memory_rss_bytes"`
			Goroutines *int64   `json:"goroutines"`
			CPUPercent *float64 `json:"cpu_percent"`
		}
		decodeWebData(t, sample, &memory, "process memory")
		if memory.RSS != nil {
			maxRSS = max(maxRSS, *memory.RSS)
		}
		// 保留按负载阶段的实际序列，峰值不能单独证明资源没有持续增长。
		// Keep the measured trend; a maximum alone is not evidence of a plateau.
		if len(resources) == 0 || time.Since(started).Seconds()-resources[len(resources)-1].ElapsedSeconds >= 10 || len(active) == 0 {
			resources = append(resources, resourceSample{acceptedCount, connections, waiting, memory.RSS, memory.Goroutines, memory.CPUPercent, time.Since(started).Seconds()})
		}
		if len(active) > 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	require.Equal(t, 4000, scalarInt(t, db, `SELECT count(*) FROM runs WHERE flow_task_id IS NOT NULL`))
	require.EqualValues(t, 4001, modelCalls.Load(), "exactly 4000 leaves plus the ordinary warm-up")
	require.EqualValues(t, 4, maximum.Load())
	require.Equal(t, 32, maxOpen)
	rows, err := db.Query(`SELECT a.started_at-t.created_at FROM flow_tasks t JOIN LATERAL (SELECT started_at FROM flow_node_attempts WHERE task_id=t.task_id ORDER BY started_at,node_attempt_id LIMIT 1) a ON true`)
	require.NoError(t, err)
	for rows.Next() {
		var ms float64
		require.NoError(t, rows.Scan(&ms))
		queue = append(queue, ms)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	// A separate four-task cohort starts on an empty scheduler, so every node is
	// free from competition by additional queued roots; load queue waits stay separate.
	noQueueStart := time.Now().UnixMilli()
	var cohort []string
	for i := range 4 {
		resp := u.start(t, uid, fmt.Sprintf("no-queue-%d", i))
		requireWebStatus(t, resp, 202, "OK", "", "accept unqueued cohort")
		var task struct {
			ID string `json:"task_id"`
		}
		decodeWebData(t, resp, &task, "unqueued task")
		cohort = append(cohort, task.ID)
	}
	for _, id := range cohort {
		u.waitStatus(t, id, "succeeded")
	}
	rows, err = db.Query(`SELECT started_at-COALESCE(lag(finished_at) OVER(PARTITION BY task_id ORDER BY started_at,node_attempt_id),created_at) FROM flow_node_attempts JOIN flow_tasks USING(task_id) WHERE flow_tasks.created_at>=$1 ORDER BY task_id,started_at,node_attempt_id`, noQueueStart)
	require.NoError(t, err)
	var ready []float64
	for rows.Next() {
		var ms float64
		require.NoError(t, rows.Scan(&ms))
		ready = append(ready, ms)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, ready, 80)
	a, r, n := us19Percentiles(accepted), us19Percentiles(reads), us19Percentiles(ready)
	configBytes, err := os.ReadFile(filepath.Join(u.h.Root, "basalt.yaml"))
	require.NoError(t, err)
	var pgVersion string
	require.NoError(t, db.QueryRow(`SHOW server_version`).Scan(&pgVersion))
	report, err := json.Marshal(map[string]any{"tasks": 200, "nodes_per_task": 20, "active": 4, "max_open": maxOpen, "mock_delay_ms": 50, "accept_ms": a, "read_ms": r, "ready_no_capacity_queue_ms": n, "queued_first_node_ms": us19Percentiles(queue), "accept_samples": len(accepted), "read_samples": len(reads), "no_queue_samples": len(ready), "failure_rate": 0, "max_rss_bytes": maxRSS, "shared_test_db_connections": maxConnections, "own_schema_relation_locks": maxLocks, "max_lock_waiting_connections": maxWaitingLocks, "max_observed_lock_wait_ms": maxLockWaitMS, "resource_samples": resources, "os": runtime.GOOS, "arch": runtime.GOARCH, "logical_cpus": runtime.NumCPU(), "postgres_version": pgVersion, "config_sha256": fmt.Sprintf("%x", sha256.Sum256(configBytes)), "elapsed_seconds": time.Since(started).Seconds(), "binary_sha256": binarySHA256})
	require.NoError(t, err)
	t.Logf("US19_CAPACITY %s", report)
	require.LessOrEqual(t, a.P95, 2000.0)
	require.LessOrEqual(t, r.P95, 1000.0)
	require.LessOrEqual(t, n.P95, 1000.0)
}

func us19LogTail(raw string, limit int) string {
	if len(raw) > limit {
		return raw[len(raw)-limit:]
	}
	return raw
}

// SC-003：32个真实open任务，四个已外发动作只核对为unknown，绝不当成功恢复。
// Reconcile the real 32-task backlog without replaying any interrupted dispatch.
func TestFlow_BinaryRestartReconciles32OpenWithin30Seconds(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := calls.Add(1)
		if n > 1 && n <= 5 {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, usageResponse(`{"answer":"restart safe result"}`, 5, 3).Body)
	}))
	t.Cleanup(server.Close)
	u := newUS19Harness(t, server.URL, us19HarnessOptions{config: func(config string) string {
		return strings.Replace(config, "max_active: 2", "max_active: 4", 1)
	}})
	uid := u.publish(t)
	db := u.h.openDB(t)
	for i := range 32 {
		response := u.start(t, uid, fmt.Sprintf("restart-32-%02d", i))
		requireWebStatus(t, response, 202, "OK", "", "accept restart backlog")
	}
	require.Eventually(t, func() bool { return calls.Load() == 5 }, 10*time.Second, 20*time.Millisecond)
	require.Equal(t, 32, scalarInt(t, db, `SELECT count(*) FROM flow_tasks WHERE status IN ('queued','running')`))
	require.Equal(t, 4, scalarInt(t, db, `SELECT count(*) FROM llm_calls WHERE run_id IN (SELECT run_id FROM runs WHERE flow_task_id IS NOT NULL) AND status='pending'`))
	u.p.kill()
	u.p.waitExit(t, 10*time.Second)
	close(release)
	started := time.Now()
	u.p = startServeWithEnv(t, u.h, u.addr, u.env)
	require.Eventually(t, func() bool {
		return scalarInt(t, db, `SELECT count(*) FROM flow_tasks WHERE status IN ('succeeded','blocked')`) == 32
	}, 30*time.Second-time.Since(started), 25*time.Millisecond)
	elapsed := time.Since(started)
	require.LessOrEqual(t, elapsed, 30*time.Second)
	require.Equal(t, 28, scalarInt(t, db, `SELECT count(*) FROM flow_tasks WHERE status='succeeded'`))
	require.Equal(t, 4, scalarInt(t, db, `SELECT count(*) FROM flow_tasks WHERE status='blocked'`))
	require.Equal(t, 4, scalarInt(t, db, `SELECT count(*) FROM flow_action_attempts WHERE outcome='unknown'`))
	require.EqualValues(t, 33, calls.Load(), "one warm-up, four interrupted, twenty-eight safe; unknown calls must not replay")
	require.Equal(t, 32, scalarInt(t, db, `SELECT count(*) FROM flow_node_attempts`))
	t.Logf("US19_RESTART open=32 safe_succeeded=28 unknown_blocked=4 replayed=0 reconciliation_ms=%.3f", float64(elapsed.Microseconds())/1000)
}

func TestFlow_BinaryInteractiveReserveAndDeferredFairness(t *testing.T) {
	for _, mode := range []string{"instance", "same-caller"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			release := make(chan struct{})
			finish := sync.OnceFunc(func() { close(release) })
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if calls.Add(1) == 2 {
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, usageResponse(`{"answer":"interactive reserve"}`, 5, 3).Body)
			}))
			t.Cleanup(server.Close)
			t.Cleanup(finish)
			instanceLimit := 8
			if mode == "instance" {
				instanceLimit = 2
			}
			u := newUS19Harness(t, server.URL, us19HarnessOptions{config: func(config string) string {
				return config + fmt.Sprintf("server:\n  max_concurrent_runs: %d\n", instanceLimit)
			}})
			if mode == "same-caller" {
				u.key = us19IssueKey(t, u.addr, 2)
				u.h.registerSecret("BASALT_US19_RESERVED_KEY", u.key)
			}
			uid := u.publish(t)
			db := u.h.openDB(t)
			for i := range 3 {
				requireWebStatus(t, u.start(t, uid, fmt.Sprint("reserve-", i)), 202, "OK", "", "accept reserved Flow")
			}
			require.Eventually(t, func() bool { return calls.Load() == 2 }, 5*time.Second, 20*time.Millisecond)
			require.Eventually(t, func() bool {
				return scalarInt(t, db, `SELECT count(*) FROM flow_transitions WHERE phase='yield'`) > 0
			}, 5*time.Second, 20*time.Millisecond, "a denied Flow must defer durably rather than consume attempts or fail")
			interactive := webRequest(t, u.addr, http.MethodPost, "/api/v1/agents/flow-reader/invoke", mustJSON(map[string]any{"message": "interactive request while Flow occupies its entire sublimit"}), u.headers(""))
			requireWebStatus(t, interactive, 200, "OK", "", "interactive slot remains available")
			require.Equal(t, 1, scalarInt(t, db, `SELECT count(*) FROM runs WHERE flow_task_id IS NOT NULL`), "queued Flow roots cannot consume the reserved interactive slot")
			finish()
			require.Eventually(t, func() bool { return scalarInt(t, db, `SELECT count(*) FROM flow_tasks WHERE status='succeeded'`) == 3 }, 10*time.Second, 25*time.Millisecond)
			require.EqualValues(t, 5, calls.Load(), "one warm-up, one interactive and three Flow leaves; no failed admission model calls")
			require.Equal(t, 3, scalarInt(t, db, `SELECT count(*) FROM flow_node_attempts n JOIN runs r ON r.run_id=n.run_id AND r.flow_task_id=n.task_id AND r.flow_node_attempt_id=n.node_attempt_id`), "only three real leaf Runs are admitted; unadmitted attempt history remains")
			require.Equal(t, 3, scalarInt(t, db, `SELECT count(*) FROM flow_task_revisions r JOIN flow_tasks t ON t.task_id=r.task_id AND t.current_revision=r.revision WHERE (r.checkpoint->'attempts'->>'draft')::int=1`), "deferred roots retain their original attempt budgets")
			t.Logf("US19_FAIRNESS scope=%s flow_roots=3 interactive_completed=true original_models=5 failed_admission_dispatches=0", mode)
		})
	}
}
