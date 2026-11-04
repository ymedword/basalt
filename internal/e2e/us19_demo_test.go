//go:build e2e

package e2e

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFlow_BinaryAcceptExecuteVerifyAndRead(t *testing.T) {
	model := newFakeModel(t, usageResponse(`{"answer":"warm"}`, 5, 3), usageResponse(`{"answer":"verified result"}`, 5, 3))
	u := newUS19Harness(t, model.URL())
	uid := u.publish(t)
	response := u.start(t, uid, "start-us19-binary")
	requireWebStatus(t, response, 202, "OK", "", "accept Flow")
	var task struct {
		ID string `json:"task_id"`
	}
	decodeWebData(t, response, &task, "accept Flow")
	require.NotEmpty(t, task.ID)
	u.waitStatus(t, task.ID, "succeeded")
	again := u.start(t, uid, "start-us19-binary")
	requireWebStatus(t, again, 200, "OK", "", "idempotent Flow")
	var repeated struct {
		ID string `json:"task_id"`
	}
	decodeWebData(t, again, &repeated, "idempotent Flow")
	require.Equal(t, task.ID, repeated.ID)
	detail := webRequest(t, u.addr, http.MethodGet, "/api/v1/flow-tasks/"+task.ID, "", u.headers(""))
	requireWebStatus(t, detail, 200, "OK", "", "Flow detail")
	require.NotEmpty(t, detail.Header.Get("ETag"))
	artifacts := webRequest(t, u.addr, http.MethodGet, "/api/v1/flow-tasks/"+task.ID+"/artifacts", "", u.headers(""))
	requireWebStatus(t, artifacts, 200, "OK", "", "Flow artifacts")
	require.Contains(t, artifacts.Raw, "verified result")
	db := u.h.openDB(t)
	require.Equal(t, 1, scalarInt(t, db, `SELECT count(*) FROM runs WHERE flow_task_id=$1`, task.ID))
	require.Equal(t, 1, scalarInt(t, db, `SELECT count(*) FROM flow_node_attempts WHERE task_id=$1`, task.ID))
	require.Equal(t, 2, len(model.Requests()), "one warm-up and one Flow inference; GET/replay must not call a model")
	u.restart(t)
	u.waitStatus(t, task.ID, "succeeded")
	require.Equal(t, 2, len(model.Requests()), "a committed task must not rerun on startup")
	closed := u.control(t, task.ID, "resume", "resume-completed")
	requireWebStatus(t, closed, 409, "FLOW_CLOSED", "", "closed Flow")
}

func TestFlow_BinaryCLIUsesServingInstanceWithoutSecondWriter(t *testing.T) {
	model := newFakeModel(t, usageResponse(`{"answer":"warm"}`, 5, 3), usageResponse(`{"answer":"cli accepted"}`, 5, 3))
	u := newUS19Harness(t, model.URL())
	uid := u.publish(t)
	secretDir := t.TempDir()
	require.NoError(t, os.Chmod(secretDir, 0o700))
	tightenMasterKeyDirACL(t, secretDir)
	secretPath := filepath.Join(secretDir, "flow-key")
	require.NoError(t, os.WriteFile(secretPath, []byte(u.key), 0o600))
	inputPath := filepath.Join(t.TempDir(), "input.json")
	require.NoError(t, os.WriteFile(inputPath, []byte(`{"answer":"frozen cli input"}`), 0o600))
	base := []string{"flow", "--server", "http://" + u.addr, "--token-file", secretPath}
	list := u.h.run(t, fixtureEnv(), append(base, "list")...)
	require.Zero(t, list.ExitCode, u.h.redact(list.Stderr))
	require.Contains(t, list.Stdout, uid)
	startArgs := append(base, "start", uid, "--input-file", inputPath, "--idempotency-key", "cli-same-key")
	started := u.h.run(t, fixtureEnv(), startArgs...)
	require.Zero(t, started.ExitCode, u.h.redact(started.Stderr))
	require.Contains(t, started.Stderr, "Idempotency-Key: cli-same-key")
	db := u.h.openDB(t)
	var id string
	require.NoError(t, db.QueryRow(`SELECT task_id FROM flow_tasks WHERE idempotency_key='cli-same-key'`).Scan(&id))
	u.waitStatus(t, id, "succeeded")
	repeated := u.h.run(t, fixtureEnv(), startArgs...)
	require.Zero(t, repeated.ExitCode, u.h.redact(repeated.Stderr))
	require.Contains(t, repeated.Stdout, id)
	status := u.h.run(t, fixtureEnv(), append(base, "status", id)...)
	require.Zero(t, status.ExitCode, u.h.redact(status.Stderr))
	require.Contains(t, status.Stdout, "succeeded")
	require.Equal(t, 1, scalarInt(t, db, `SELECT count(*) FROM flow_tasks`))
	require.Equal(t, 1, scalarInt(t, db, `SELECT count(*) FROM runs WHERE flow_task_id=$1`, id))
	require.Len(t, model.Requests(), 2, "one warm-up plus original Flow; CLI list/status/replay must not invoke a model or second runtime")
	require.NotContains(t, started.Stdout+started.Stderr+status.Stdout+status.Stderr, u.key)
}
