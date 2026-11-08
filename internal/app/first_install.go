package app

// 本文件把既有的「新实例首次准备」实现投影成命令面端口（US-25 §3.1.1）。
//
// 为什么要有这一层：`cli` 只认自己声明的端口（依赖方向是 app → cli → core），
// 而首次准备的唯一实现在 `instance_bootstrap.go`。适配器因此无状态、不取实例锁、
// 不读配置——它只调用 `ensureNewInstanceConfig` 并把报告字段一一映射过去。
//
// 半截名册按失败关闭处理（C-04 / FR-25-05）：`detectNewInstance` 只按"库与名册
// 都不存在"判新实例，名册文件存在但内容只写了一半时它会答"存量"。这本身是对的
// （不能把半截名册当没写过、自动续做），但 `init` 若就此继续建库，会把一个失去
// 自动初始化资格的工作区伪装成初始化成功。适配器因此在非 Fresh 时追加一次只读
// 名册校验：读不懂 / 版本过高 / 标识非法一律上抛，由命令面提示人工处理。
//
// The adapter projects ensureNewInstanceConfig onto the command-face port. It is
// stateless and takes no instance lock. A half-written roster is never treated as
// "not created": the read-only validation fails closed so init cannot bootstrap a
// workspace whose automatic initialisation is no longer provable.

import (
	"github.com/ymedword/basalt/internal/cli"
)

// firstInstallAdapter 是首次安装准备的装配层实现。
// firstInstallAdapter is the assembly-layer implementation of the first-install port.
type firstInstallAdapter struct{}

// Prepare 按固定顺序准备新实例（配置 → provisioning.pending → 内置安装意图 → 名册），
// 已完成的工作区零写入；非 Fresh 时追加名册可解析性校验。
// Prepare runs the fixed order for a new instance and validates an existing roster
// read-only when the workspace is not fresh.
func (firstInstallAdapter) Prepare(workDir string) (cli.FirstInstallReport, error) {
	report, err := ensureNewInstanceConfig(workDir, programRuntimeRoot())
	if err != nil {
		return cli.FirstInstallReport{}, err
	}
	if !report.Fresh {
		// 存量工作区没有名册是正常形态（US-14 之前的工作区）；有名册但读不懂才是
		// 故障——读取函数本身区分这两种情况，这里不额外猜。
		// A missing roster is normal for legacy workspaces; an unreadable one is not.
		if _, _, markerErr := readInstanceMarker(workDir); markerErr != nil {
			return cli.FirstInstallReport{}, markerErr
		}
	}
	return firstInstallReport(report), nil
}

// firstInstallReport 把装配层报告映射成命令面报告；字段一一对应，`Fresh` 语义不变。
// firstInstallReport maps the assembly report field by field; Fresh keeps its meaning.
func firstInstallReport(report instanceBootstrapReport) cli.FirstInstallReport {
	return cli.FirstInstallReport{
		Fresh:                report.Fresh,
		ConfigCreated:        report.ConfigCreated,
		ProvisioningRecorded: report.ProvisioningRecorded,
		MarkerCreated:        report.MarkerCreated,
		InstanceID:           report.InstanceID,
	}
}
