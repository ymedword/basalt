package app

// 本文件是主密钥路径的**进程内共享持有者**（14D 整改 R2-1）。
//
// 为什么需要可变路径：没有名册的存量实例在启动时解析不出主密钥路径（不猜旧路径），
// 此时五个凭据组件都只能拿到空串。用户在工作区管理台显式初始化后，实例标识与密钥
// 路径才第一次存在——模型连接 / MCP 逐槽位凭据 / 通知渠道必须在**同一进程内**立刻
// 按这条路径工作，而不是继续持有启动时的空串、等下一次重启才"看见"新密钥。
//
// 纪律：路径是**共享事实**，不是每个服务各自的副本。装配期建一个 locator，五个组件
// 与实例加密服务持有同一个指针；写入只发生在实例初始化成功（记录已写）之后，之前
// 任何组件看到的仍是空串，托管写入继续按失败关闭拒绝。
// The master-key path is shared mutable state on purpose: once a user enables managed
// credentials through the console, every credential component must see the new path in
// the same process. The locator starts empty and is only set after a successful init.

import "sync"

// MasterKeyLocator 在并发读写之间持有当前生效的主密钥路径。
// MasterKeyLocator holds the currently effective master-key path.
type MasterKeyLocator struct {
	mu   sync.RWMutex
	path string
}

// NewMasterKeyLocator 用启动时解析出的路径（可能为空串 = 尚未启用托管）构造持有者。
// NewMasterKeyLocator seeds the holder with the path resolved at startup.
func NewMasterKeyLocator(path string) *MasterKeyLocator {
	return &MasterKeyLocator{path: path}
}

// Path 返回当前生效路径；nil 持有者按空串读（未配置），不 panic。
// Path returns the effective path; a nil holder reads as unconfigured.
func (l *MasterKeyLocator) Path() string {
	if l == nil {
		return ""
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.path
}

// Set 在实例初始化成功后切换路径：只由 `InstanceSecretsService.Init` 在记录写入之后
// 调用，**不得**用于"启动时猜一个路径"。
// Set is called only by Init after the record has been written.
func (l *MasterKeyLocator) Set(path string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.path = path
	l.mu.Unlock()
}

// effectiveMasterKeyFile 解析一个组件实际使用的主密钥路径：持有者优先（共享事实），
// 没有持有者时退回静态字段（测试与半截装配）。
// effectiveMasterKeyFile prefers the shared locator and falls back to the static field.
func effectiveMasterKeyFile(locator *MasterKeyLocator, static string) string {
	if locator != nil {
		return locator.Path()
	}
	return static
}
