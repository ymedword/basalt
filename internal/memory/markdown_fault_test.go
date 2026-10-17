package memory

// 本文件是 markdown 载体**原子替换失败路径**的故障注入验收（FR-014 / spec.md:137）。
// 既有 `TestMarkdownStore_AppendReplacesAtomicallyWithoutResidue` 的失败态是配额拒绝——
// 它发生在临时文件创建之前，证明不了写入 / 关闭 / rename 失败后的清理与原文件不变。
// 三类失败无法跨平台确定性地用真实文件系统制造，故经生产代码里的两个未导出注入点
// （`openTemp` / `rename`，生产恒为 nil）注入；另覆盖「写入与关闭同时失败」的复合
// 路径——两个错误都要进错误文本，且清理**仍须**尝试（不得因关闭失败提前返回）。
// This file injects deterministic write / close / rename failures — and a combined
// write-and-close failure — to prove temp-file cleanup and a byte-identical original
// file on every atomic-replace failure path.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

var (
	errInjectedWrite  = errors.New("injected write failure")
	errInjectedClose  = errors.New("injected close failure")
	errInjectedRename = errors.New("injected rename failure")
)

// injectedTempFile 包装真实临时文件，按需让 WriteString / Close 失败。
type injectedTempFile struct {
	*os.File
	writeErr error
	closeErr error
}

func (f *injectedTempFile) WriteString(s string) (int, error) {
	if f.writeErr != nil {
		// 先落半截内容：确保清理路径面对的是真实存在且非空的临时文件。
		if _, err := f.File.WriteString(s[:len(s)/2]); err != nil {
			return 0, err
		}
		return 0, f.writeErr
	}
	return f.File.WriteString(s)
}

func (f *injectedTempFile) Close() error {
	closeErr := f.File.Close()
	if f.closeErr != nil {
		return errors.Join(f.closeErr, closeErr)
	}
	return closeErr
}

// TestMarkdownStore_ReplaceFailuresLeaveOriginalAndNoResidue 覆盖四条失败路径：临时
// 文件写入失败、关闭失败、rename 失败、写入与关闭同时失败。每条都断言：错误原样上抛
// （errors.Is 可判）、原文件逐字节不变、零临时文件残留（FR-014 的两个判据）；复合
// 失败另断言错误文本同时点名关闭失败（清理仍已尝试）。
// 逆改点：删掉 tempFailure 的 root.Remove → 四条子用例的残留断言红；把「关闭失败
// 仍尝试清理」改回提前返回 → 复合失败子用例红。
// 验收点：FR-014；第二轮复核 F2-4。
func TestMarkdownStore_ReplaceFailuresLeaveOriginalAndNoResidue(t *testing.T) {
	cases := []struct {
		name     string
		inject   func(store *MarkdownStore)
		wantErr  error
		wantText string
	}{
		{
			name: "临时文件写入失败",
			inject: func(store *MarkdownStore) {
				store.openTemp = func(root *os.Root, name string, flag int, perm os.FileMode) (tempFile, error) {
					f, err := root.OpenFile(name, flag, perm)
					if err != nil {
						return nil, err
					}
					return &injectedTempFile{File: f, writeErr: errInjectedWrite}, nil
				}
			},
			wantErr: errInjectedWrite,
		},
		{
			name: "临时文件关闭失败",
			inject: func(store *MarkdownStore) {
				store.openTemp = func(root *os.Root, name string, flag int, perm os.FileMode) (tempFile, error) {
					f, err := root.OpenFile(name, flag, perm)
					if err != nil {
						return nil, err
					}
					return &injectedTempFile{File: f, closeErr: errInjectedClose}, nil
				}
			},
			wantErr: errInjectedClose,
		},
		{
			name: "rename 失败",
			inject: func(store *MarkdownStore) {
				store.rename = func(*os.Root, string, string) error { return errInjectedRename }
			},
			wantErr: errInjectedRename,
		},
		{
			name: "写入与关闭同时失败",
			inject: func(store *MarkdownStore) {
				store.openTemp = func(root *os.Root, name string, flag int, perm os.FileMode) (tempFile, error) {
					f, err := root.OpenFile(name, flag, perm)
					if err != nil {
						return nil, err
					}
					return &injectedTempFile{File: f, writeErr: errInjectedWrite, closeErr: errInjectedClose}, nil
				}
			},
			wantErr:  errInjectedWrite,
			wantText: "临时文件关闭失败",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			store := NewMarkdownStore(root)
			scope := core.Scope{Agent: "fault"}
			file := filepath.Join(root, "memory", "agents", "fault", "MEMORY.md")

			require.NoError(t, store.Append(context.Background(), scope, core.SectionCore, "原有条目"))
			before, err := os.ReadFile(file)
			require.NoError(t, err)

			c.inject(store)
			err = store.Append(context.Background(), scope, core.SectionCore, "新条目")
			require.ErrorIs(t, err, c.wantErr, "失败必须原样上抛（errors.Is 可判）")
			if c.wantText != "" {
				require.Contains(t, err.Error(), c.wantText, "复合失败的错误文本必须点名关闭失败")
			}

			after, err := os.ReadFile(file)
			require.NoError(t, err)
			require.Equal(t, string(before), string(after), "失败后原文件逐字节不变")

			requireNoFaultResidue(t, root)
		})
	}
}

// requireNoFaultResidue 判临时文件零残留（`<名字>.tmp-<16 位十六进制>`）。
func requireNoFaultResidue(t *testing.T, root string) {
	t.Helper()
	var found []string
	err := filepath.WalkDir(filepath.Join(root, "memory"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.Contains(d.Name(), ".tmp-") {
			found = append(found, path)
		}
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, found, "失败路径必须清理临时文件，实际残留：%v", found)
}
