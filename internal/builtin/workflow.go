package builtin

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/ymedword/basalt/examples"
)

const workflowAssets = "workflows/bid-readiness/defaults"

const workflowPreparedFile = "control/workflow-defaults.json"

// WorkflowPrepared 读取启动与显式安装共用的一次性准备事实。
// 已存在但损坏的回执也保守视为已处理，不借自动准备复活用户删除的角色。
// WorkflowPrepared conservatively recognizes the shared installation receipt.
func WorkflowPrepared(wsRoot string) (prepared bool, err error) {
	root, err := os.OpenRoot(wsRoot)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	_, err = root.Lstat(workflowPreparedFile)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// WorkflowAgents 返回默认采购工作流的四个角色，不含实验对照 Agent。
// WorkflowAgents lists the bundled workflow roles, excluding evaluation controls.
func WorkflowAgents() []string {
	return []string{"bid-requirements", "bid-technical", "bid-delivery", "bid-reviewer"}
}

// InstallWorkflow 只准备素材；既有目录、生命周期或调用方登记的身份均保留。
// 每个单元完整写到工作区内暂存目录后才改名；不产生模型、摄取、发布或任务操作。
// InstallWorkflow atomically prepares assets without overwriting operator-owned identities.
func InstallWorkflow(wsRoot string, known func(string) (bool, error)) (results []AgentResult, err error) {
	root, err := os.OpenRoot(wsRoot)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	for _, name := range WorkflowAgents() {
		// 任何状态文件（包括损坏的）都表示已有身份，不借安装器修复或复活。
		// Even corrupt lifecycle files count as existing identities; recovery owns them.
		_, stateErr := root.Lstat(filepath.Join("control", "agents", name+".json"))
		if stateErr != nil && !errors.Is(stateErr, fs.ErrNotExist) {
			return nil, stateErr
		}
		owned := stateErr == nil
		if known != nil {
			registered, readErr := known(name)
			if readErr != nil {
				return nil, readErr
			}
			owned = owned || registered
		}
		state := StateKept
		if !owned {
			state, err = installWorkflowTree(root, examples.Assets, path.Join(workflowAssets, "agents", name), filepath.Join("agents", name))
			if err != nil {
				return nil, fmt.Errorf("准备工作流 Agent %s: %w", name, err)
			}
		}
		results = append(results, AgentResult{Name: name, State: state})
	}
	_, err = installWorkflowTree(root, examples.Assets, path.Join(workflowAssets, "materials"), filepath.Join("samples", "bid-readiness"))
	if err != nil {
		return results, err
	}
	// 完整安装后才记回执；不修改已有开箱标记，不把旧实例变成自动接管实例。
	// Record completion without altering legacy ownership or any existing receipt.
	if err := root.MkdirAll("control", 0o750); err != nil {
		return results, err
	}
	receipt, err := root.OpenFile(workflowPreparedFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return results, nil
	}
	if err != nil {
		return results, err
	}
	_, writeErr := receipt.WriteString("{\"version\":1,\"prepared\":true}\n")
	return results, errors.Join(writeErr, receipt.Close())
}

// 完整复制后原子改名，避免崩溃留下半个正式目录并被下次误认成用户素材。
// Stage complete trees before rename so an interrupted copy is never treated as installed.
func installWorkflowTree(root *os.Root, source fs.FS, src, dest string) (state string, err error) {
	if _, statErr := root.Lstat(dest); statErr == nil {
		return StateKept, nil
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return "", statErr
	}
	if err := root.MkdirAll("control", 0o750); err != nil {
		return "", err
	}
	temp := filepath.Join("control", "workflow-stage-"+rand.Text())
	if err := root.Mkdir(temp, 0o750); err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, root.RemoveAll(temp)) }()
	err = fs.WalkDir(source, src, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(filepath.FromSlash(src), filepath.FromSlash(file))
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(temp, rel)
		if entry.IsDir() {
			return root.MkdirAll(target, 0o750)
		}
		data, readErr := fs.ReadFile(source, file)
		if readErr != nil {
			return readErr
		}
		return root.WriteFile(target, data, 0o600)
	})
	if err != nil {
		return "", err
	}
	if err := root.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", err
	}
	if _, statErr := root.Lstat(dest); statErr == nil {
		return StateKept, nil
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return "", statErr
	}
	if err := root.Rename(temp, dest); err != nil {
		return "", err
	}
	return StateInstalled, nil
}
