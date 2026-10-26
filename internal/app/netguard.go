package app

// 本文件是受控拨号（14D §2.2.2「网络·解析 / 连接固定 / 代理」与 D12）在**装配层**的
// 落点：进程级守门器在这里装一次，例外集合从这里喂给守门器。
//
// 三条分工不能混：
//   - **装配在 `Run`**：那是二进制唯一的进程入口。包内测试直接调 `newRuntime` /
//     `OpenRuntime` 时不会走到这里——测试替身连的是本机假服务，不是生产出口。
//   - **例外集合是活快照**：守门器每次拨号读当前值（`NetGuardOptions.Exceptions`），
//     删除例外后下一次判定即失效，不需要重建任何客户端。快照由 `newRuntime` 在迁移
//     之后从库里装载，管理面写入后由写路径替换（T107 接线）。
//   - **失败即拒**：装载例外失败只记 WARN 并保留旧快照（初始为空 = 全部按受保护拒），
//     **不得**为了"少报错"塞一份宽集合。
// The process guard is installed once in Run; the exception snapshot is fed from the
// database and read live by the guard, and a failed refresh keeps the previous
// (fail-closed) snapshot.

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
)

// networkExceptionSnapshot 是守门器读到的例外集合快照：写入侧整体替换，读侧拿一份
// 副本（守门器在拨号路径上，不能持锁遍历共享切片）。
// networkExceptionSnapshot hands the guard a copy; writers replace it wholesale.
type networkExceptionSnapshot struct {
	mu   sync.RWMutex
	list []core.NetworkException
}

// Replace 整体替换快照（拷贝入参，调用方之后改切片不影响已装载的集合）。
// Replace swaps the snapshot; the caller's slice is copied in.
func (s *networkExceptionSnapshot) Replace(list []core.NetworkException) {
	copied := append([]core.NetworkException(nil), list...)
	s.mu.Lock()
	s.list = copied
	s.mu.Unlock()
}

// Snapshot 返回当前集合的副本；空集合返回空切片（不是 nil，守门器遍历无特殊分支）。
// Snapshot returns a copy; an empty set is an empty slice, never nil.
func (s *networkExceptionSnapshot) Snapshot() []core.NetworkException {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]core.NetworkException, len(s.list))
	copy(out, s.list)
	return out
}

// processExceptions 是进程级例外快照：装配层一处持有，守门器经闭包读它。
// 进程内只有一份——例外是实例级事实，不是运行期局部配置。
// The one process-wide snapshot read by the guard's Exceptions closure.
var processExceptions networkExceptionSnapshot

// installOutboundGuard 装配进程级受控拨号：解析 → 分类 → 只拨已校验的 IP，代理只认
// 显式 `http.proxy`。**必须**在任何出站客户端被构造之前调用（`Run` 里紧跟配置装载）。
// installOutboundGuard installs the process-wide guard before any client is built.
func installOutboundGuard(cfg *cli.Config, logger *slog.Logger) error {
	proxyURL, err := outboundProxyURL(cfg)
	if err != nil {
		return err
	}
	tool.SetNetGuard(tool.NewNetGuard(tool.NetGuardOptions{
		ProxyURL:   proxyURL,
		Exceptions: processExceptions.Snapshot,
	}))
	// 只记"是否经代理"这一个布尔：代理 URL 的 userinfo 可能是凭证，端点细节也不该
	// 无谓地落到日志里。目标由代理解析这一点在页面同处标注（§2.2.2）。
	// Only a boolean is logged; the proxy URL may carry credentials in userinfo.
	logger.Info("出网受控拨号已装配", "explicit_proxy", proxyURL != nil)
	return nil
}

// outboundProxyURL 把 `http.proxy` 解析成守门器要的形状；空串 = 直连（nil）。
// 环境变量代理在这一层之外：守门器与 `guardedTransport()` 都不读它们。
// outboundProxyURL parses the explicit proxy; empty means direct egress.
func outboundProxyURL(cfg *cli.Config) (*url.URL, error) {
	if cfg == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(cfg.HTTP.Proxy)
	if raw == "" {
		return nil, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		// 装载期已校过形态；这里再兜一次仍是 fail-closed——配了代理却按直连跑，
		// 等于换了一个出口。
		// Shape is validated at load time; a second refusal here stays fail-closed.
		return nil, fmt.Errorf("http.proxy 无法装配为代理地址——拒绝以直连方式启动（取值不回显：可能含凭证）")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("http.proxy 装配失败：需要 http(s)://[userinfo@]host:port（取值不回显：可能含凭证）")
	}
	return parsed, nil
}

// refreshNetworkExceptions 从库里重载例外集合并整体替换快照。读失败只记 WARN 并保留
// 旧快照（初始为空 = 受保护地址一律拒）：宁可少放行，也不猜一份宽集合。
// refreshNetworkExceptions reloads the set; a read failure keeps the previous snapshot.
func refreshNetworkExceptions(ctx context.Context, store *storage.NetworkExceptionStore, logger *slog.Logger) {
	list, err := store.List(ctx)
	if err != nil {
		logger.Warn("装载网络访问例外失败——保留上一份快照（受保护地址仍一律拒绝）", "error", err)
		return
	}
	processExceptions.Replace(list)
}
