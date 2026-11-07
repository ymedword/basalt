//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type us19Fault struct{ table, event, condition string }

// The marker is a nontransactional sequence plus a session advisory lock. The
// parent kills its own app process and terminates only this marked SQL backend,
// so no sleeping trigger or transaction survives into restart/fixture cleanup.
func us19InstallFault(t *testing.T, u *us19Harness, fault us19Fault) (wait func() int, remove func()) {
	t.Helper()
	db := u.h.openDB(t)
	sum := sha256.Sum256([]byte(u.h.Schema.Name))
	key := int64(binary.BigEndian.Uint32(sum[:4]) & 0x7fffffff)
	condition := fault.condition
	if condition == "" {
		condition = "true"
	}
	statement := fmt.Sprintf(`CREATE SEQUENCE us19_fault_seq;
CREATE FUNCTION us19_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF %s THEN PERFORM nextval('us19_fault_seq'); PERFORM pg_advisory_lock(%d::bigint); PERFORM pg_sleep(60); END IF; RETURN NEW; END $$;
CREATE TRIGGER us19_fault BEFORE %s ON %s FOR EACH ROW EXECUTE FUNCTION us19_fault()`, condition, key, fault.event, fault.table)
	_, err := db.Exec(statement)
	require.NoError(t, err)
	wait = func() int {
		var pid int
		require.Eventually(t, func() bool {
			var fired bool
			if db.QueryRow(`SELECT is_called FROM us19_fault_seq`).Scan(&fired) != nil || !fired {
				return false
			}
			return db.QueryRow(`SELECT pid FROM pg_locks WHERE locktype='advisory' AND classid=0 AND objid=$1 AND granted`, key).Scan(&pid) == nil
		}, 12*time.Second, 15*time.Millisecond, "real SQL fault did not fire")
		require.Positive(t, pid)
		return pid
	}
	remove = func() {
		_, err := db.Exec(fmt.Sprintf(`DROP TRIGGER us19_fault ON %s; DROP FUNCTION us19_fault(); DROP SEQUENCE us19_fault_seq`, fault.table))
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		u.p.kill()
		u.p.waitExit(t, 10*time.Second)
		_, _ = db.Exec(`SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype='advisory' AND classid=0 AND objid=$1 AND granted`, key)
	})
	return wait, remove
}

func TestFlow_CrashWindowMatrix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fault    *us19Fault
		unknown  bool
		complete bool
		rework   bool
		cancel   bool
	}{
		{name: "accept-before-commit", fault: &us19Fault{"flow_task_revisions", "INSERT", "NEW.revision=0"}},
		{name: "accept-after-commit", fault: &us19Fault{"flow_node_attempts", "INSERT", "true"}},
		{name: "claim-before-commit", fault: &us19Fault{"flow_node_attempts", "INSERT", "true"}},
		{name: "claim-after-commit", fault: &us19Fault{"runs", "INSERT", "NEW.flow_task_id IS NOT NULL"}},
		{name: "dispatch-before-commit", fault: &us19Fault{"flow_action_attempts", "UPDATE", "NEW.outcome='dispatching'"}, unknown: true},
		{name: "dispatch-after-commit", unknown: true},
		{name: "receiver-ack-before-audit-terminal", fault: &us19Fault{"llm_calls", "UPDATE", "NEW.status='success'"}, unknown: true},
		{name: "audit-terminal-before-flow-result", fault: &us19Fault{"flow_artifacts", "INSERT", "true"}},
		{name: "flow-result-committed-before-observation", complete: true},
		{name: "revision-before-commit", fault: &us19Fault{"flow_task_revisions", "INSERT", "NEW.revision>0"}, rework: true},
		{name: "revision-after-commit", fault: &us19Fault{"runs", "INSERT", "NEW.flow_task_id IS NOT NULL AND EXISTS(SELECT 1 FROM flow_tasks t WHERE t.task_id=NEW.flow_task_id AND t.current_revision>0)"}, rework: true},
		{name: "cancel-intent-before-leaf-join", fault: &us19Fault{"runs", "UPDATE", "NEW.status='cancelled'"}, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received atomic.Int64
			entered := make(chan struct{})
			releaseReceiver := make(chan struct{})
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				n := received.Add(1)
				if n == 2 && (tc.name == "dispatch-after-commit" || tc.cancel) {
					close(entered)
					select {
					case <-r.Context().Done():
					case <-releaseReceiver:
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				text := `{"answer":"real binary result"}`
				if tc.rework && n == 2 {
					text = `{"answer":7}`
				}
				_, _ = w.Write([]byte(usageResponse(text, 5, 3).Body))
			}))
			defer func() { close(releaseReceiver); model.Close() }()
			u := newUS19Harness(t, model.URL)
			if tc.rework {
				node := u.definition["nodes"].([]any)[0].(map[string]any)
				node["failure"] = map[string]any{"max_attempts": 3, "max_reworks": 1, "rework_target": "draft"}
			}
			uid := u.publish(t)
			db := u.h.openDB(t)
			var wait func() int
			var remove func()
			if tc.fault != nil {
				wait, remove = us19InstallFault(t, u, *tc.fault)
			}
			response := make(chan *webResponse, 1)
			requestErr := make(chan error, 1)
			go func() {
				r, err := doWebRequest(context.Background(), http.MethodPost, u.addr, "/api/v1/flow-tasks", mustJSON(map[string]any{"definition_uid": uid, "input": map[string]any{"answer": "immutable input"}}), u.headers("crash-root"))
				response <- r
				requestErr <- err
			}()
			var pid int
			consumed := false
			var cancelDone chan error
			if tc.complete {
				r := <-response
				require.NoError(t, <-requestErr)
				requireWebStatus(t, r, 202, "OK", "", "accept before result observation")
				var id string
				require.NoError(t, db.QueryRow(`SELECT task_id FROM flow_tasks`).Scan(&id))
				u.waitStatus(t, id, "succeeded")
			} else if tc.name == "dispatch-after-commit" {
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Fatal("real receiver did not observe dispatch")
				}
			} else if tc.cancel {
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Fatal("real receiver did not observe cancellable call")
				}
				var taskID string
				require.NoError(t, db.QueryRow(`SELECT task_id FROM flow_tasks`).Scan(&taskID))
				current := webRequest(t, u.addr, http.MethodGet, "/api/v1/flow-tasks/"+taskID, "", u.headers(""))
				requireWebStatus(t, current, 200, "OK", "", "cancel read")
				headers := u.headers("")
				headers["If-Match"] = current.Header.Get("ETag")
				cancelDone = make(chan error, 1)
				go func() {
					_, err := doWebRequest(context.Background(), http.MethodPost, u.addr, "/api/v1/flow-tasks/"+taskID+"/cancel", mustJSON(map[string]any{"operation_id": "cancel-before-join", "reason": "stop the observed call"}), headers)
					cancelDone <- err
				}()
				pid = wait()
				var intent string
				require.NoError(t, db.QueryRow(`SELECT close_intent FROM flow_tasks WHERE task_id=$1`, taskID).Scan(&intent))
				require.Equal(t, "cancel", intent)
			} else {
				pid = wait()
			}
			if tc.name == "accept-after-commit" {
				r := <-response
				require.NoError(t, <-requestErr)
				requireWebStatus(t, r, 202, "OK", "", "durable acceptance receipt")
				consumed = true
			}
			u.p.kill()
			u.p.waitExit(t, 10*time.Second)
			if pid > 0 {
				var killed bool
				require.NoError(t, db.QueryRow(`SELECT pg_terminate_backend($1)`, pid).Scan(&killed))
				require.True(t, killed)
				remove()
			}
			var beforeTask string
			var beforeCount int
			require.NoError(t, db.QueryRow(`SELECT count(*),COALESCE(min(task_id),'') FROM flow_tasks`).Scan(&beforeCount, &beforeTask))
			if tc.name == "accept-before-commit" {
				require.Zero(t, beforeCount)
			} else {
				require.Equal(t, 1, beforeCount)
			}
			callsAtCrash := received.Load()
			var beforeActions string
			require.NoError(t, db.QueryRow(`SELECT COALESCE(string_agg(action_id,',' ORDER BY action_id),'') FROM flow_actions WHERE task_id=$1`, beforeTask).Scan(&beforeActions))
			u.p = startServe(t, u.h, u.addr)
			if beforeTask == "" {
				r := u.start(t, uid, "crash-root")
				requireWebStatus(t, r, 202, "OK", "", "retry uncommitted acceptance")
				var value struct {
					ID string `json:"task_id"`
				}
				decodeWebData(t, r, &value, "retry acceptance")
				beforeTask = value.ID
			}
			if tc.cancel {
				u.waitStatus(t, beforeTask, "cancelled")
				r := u.control(t, beforeTask, "resume", "resume-cancelled")
				requireWebStatus(t, r, 409, "FLOW_CLOSED", "", "cancelled task never resurrects")
				require.Equal(t, callsAtCrash, received.Load())
				var held int64
				require.NoError(t, db.QueryRow(`SELECT COALESCE(sum(reserved_tokens),0) FROM flow_budget_allocations WHERE task_id=$1 AND state IN ('pending','unknown')`, beforeTask).Scan(&held))
				require.Positive(t, held)
				select {
				case <-cancelDone:
				case <-time.After(3 * time.Second):
					t.Fatal("cancel request did not join after process kill")
				}
			} else if tc.unknown {
				// US-24：启动不做全局扫描，A1 只按**过期租约**收尾叶子 Run（FR-24-02）。
				// 根上有未决动作时 `Recover` 在写库前返回 ErrActionUnknown（R4–R7），根保持
				// 非终态、动作守卫原样保留——它不是"本轮被写成了 blocked"，也不会自动推进。
				// The root now stays non-terminal with its guard intact; it is never auto-advanced.
				u.waitRunOver(t, beforeTask)
				var rootStatus string
				require.NoError(t, db.QueryRow(`SELECT status FROM flow_tasks WHERE task_id=$1`, beforeTask).Scan(&rootStatus))
				require.False(t, flowStatusTerminal(rootStatus), "未决动作不得被自动收为终态")
				r := u.control(t, beforeTask, "resume", "resume-unknown")
				requireWebStatus(t, r, 409, "FLOW_ACTION_UNKNOWN", "", "unknown action must not resume")
				require.Equal(t, callsAtCrash, received.Load(), "unknown cannot issue another model call")
				var held int64
				require.NoError(t, db.QueryRow(`SELECT COALESCE(sum(reserved_tokens),0) FROM flow_budget_allocations WHERE task_id=$1 AND state IN ('pending','unknown')`, beforeTask).Scan(&held))
				require.Positive(t, held)
				closeResult := u.control(t, beforeTask, "cancel", "close-unknown")
				requireCloseAccepted(t, closeResult, "close without pretending no call happened")
				// 关闭可以收敛（收敛判据用 Isolated 而不是 Joined，FR-24-38），但不抹掉未知事实。
				u.waitStatus(t, beforeTask, "cancelled")
				require.Equal(t, callsAtCrash, received.Load())
				var afterHeld int64
				require.NoError(t, db.QueryRow(`SELECT COALESCE(sum(reserved_tokens),0) FROM flow_budget_allocations WHERE task_id=$1 AND state IN ('pending','unknown')`, beforeTask).Scan(&afterHeld))
				require.Equal(t, held, afterHeld)
			} else {
				// 非未决窗口由租约回收自动推进：A3 把 worker_lease_expired 的 blocked 根重入
				// queued（FR-24-37），无需人工 resume；这里判的仍是"计划内的已知调用恰好发生"。
				// Known windows converge automatically through the reclaimer; no resume needed.
				u.waitStatus(t, beforeTask, "succeeded")
				expectedCalls, expectedRuns := 2, 1
				if tc.rework {
					expectedCalls, expectedRuns = 3, 2
					var revision int
					require.NoError(t, db.QueryRow(`SELECT current_revision FROM flow_tasks WHERE task_id=$1`, beforeTask).Scan(&revision))
					require.Equal(t, 1, revision)
				}
				require.EqualValues(t, expectedCalls, received.Load(), "only planned known calls may occur across the crash")
				require.Equal(t, expectedRuns, scalarInt(t, db, `SELECT count(*) FROM runs WHERE flow_task_id=$1`, beforeTask))
			}
			var identities []string
			rows, err := db.Query(`SELECT action_id FROM flow_actions WHERE task_id=$1 ORDER BY action_id`, beforeTask)
			require.NoError(t, err)
			for rows.Next() {
				var id string
				require.NoError(t, rows.Scan(&id))
				identities = append(identities, id)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			if tc.unknown || tc.cancel || tc.complete {
				var afterActions string
				require.NoError(t, db.QueryRow(`SELECT COALESCE(string_agg(action_id,',' ORDER BY action_id),'') FROM flow_actions WHERE task_id=$1`, beforeTask).Scan(&afterActions))
				require.Equal(t, beforeActions, afterActions, "unknown or committed work must retain the exact action identity set")
			}
			t.Logf("kill_window_observed=true sql_fault=%t window=%s task=%s calls_at_crash=%d calls_after_restart=%d action_ids=%v", tc.fault != nil, tc.name, beforeTask, callsAtCrash, received.Load(), identities)
			if !tc.complete && !consumed {
				select {
				case <-response:
				case <-time.After(3 * time.Second):
					t.Fatal("original HTTP request did not join after process kill")
				}
			}
		})
	}
}
