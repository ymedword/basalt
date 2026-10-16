//go:build windows

package tool

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"golang.org/x/sys/windows"
)

// 本文件判 G9（Job 关联失败路径的清理顺序，Windows 读数）。它落在**内部测试包**且带
// Windows 构建标记，两条理由都是硬性的：① 清理序列 `cleanupSpawnFailure` 是未导出符号，
// 外部测试包 `tool_test` 看不见它——放外部包等于这条用例根本判不了卷；② Job 与句柄 API
// 只在 Windows 存在，不带构建标记会让 Linux CI 直接编译失败。
// G9 lives in an internal, Windows-tagged test file: the cleanup sequence is unexported
// and the Job/handle APIs exist on Windows only.

// TestLocalRunner_CleansUpHandlesInFixedOrderOnAssociateFailure 覆盖 G9：③ / ④ 失败时的清理
// 顺序**逐项固定**——先整树终止兜底（挂起态的进程也可能已占住内核资源），再依次关进程、
// 线程、Job 句柄；某一步失败**不得**让后面的步骤被跳过，各步错误一律 Join 上抛（清理既不
// 掩盖原始失败原因、也不因中途出错而漏还句柄）。
// Acceptance point (G9): the cleanup sequence is fixed; every step runs and every error
// survives.
func TestLocalRunner_CleansUpHandlesInFixedOrderOnAssociateFailure(t *testing.T) {
	t.Run("顺序固定", func(t *testing.T) {
		var steps []string
		record := func(step string) func() error {
			return func() error {
				steps = append(steps, step)
				return nil
			}
		}
		require.NoError(t, cleanupSpawnFailure(
			record("terminate-tree"), record("close-process"), record("close-thread"), record("close-job"),
		))
		require.Equal(t, []string{"terminate-tree", "close-process", "close-thread", "close-job"}, steps,
			"清理顺序即契约：整树终止 → 进程 → 线程 → Job")
	})

	t.Run("任一步失败不中断后续且错误全部上抛", func(t *testing.T) {
		var steps []string
		alwaysFail := func(step string) func() error {
			return func() error {
				steps = append(steps, step)
				return errors.New(step + " 失败")
			}
		}
		err := cleanupSpawnFailure(
			alwaysFail("terminate-tree"), alwaysFail("close-process"),
			alwaysFail("close-thread"), alwaysFail("close-job"),
		)
		require.Equal(t, []string{"terminate-tree", "close-process", "close-thread", "close-job"}, steps,
			"某一步失败不得让后面的步骤被跳过（漏关句柄就是漏资源）")
		for _, step := range []string{"terminate-tree", "close-process", "close-thread", "close-job"} {
			require.ErrorContains(t, err, step+" 失败", "每一步的错误都要留在返回值里")
		}
	})

	t.Run("真实句柄确实被关掉", func(t *testing.T) {
		job, err := windows.CreateJobObject(nil, nil)
		require.NoError(t, err)
		process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(os.Getpid()))
		require.NoError(t, err)
		require.NoError(t, cleanupSpawnFailure(
			func() error { return killTree(uintptr(job)) },
			func() error { return closeHandle("进程", process) },
			// 本例不另开线程句柄：清理序列允许单步无实体资源（与 Unix 侧的 close 同形）。
			func() error { return nil },
			func() error { return closeHandle("Job 对象", job) },
		))
		// 关过的句柄再关一次必然拿 ERROR_INVALID_HANDLE——这是"真的关掉了"的读数，
		// 绕开 closeHandle 的宽容判定直接问内核。
		require.ErrorIs(t, windows.CloseHandle(job), windows.ERROR_INVALID_HANDLE, "Job 句柄必须真的被关掉")
		require.ErrorIs(t, windows.CloseHandle(process), windows.ERROR_INVALID_HANDLE, "进程句柄必须真的被关掉")
	})
}
