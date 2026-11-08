package app

// 本文件是 `secrets check-key` 的装配侧实现（US-25 §3.1.4）：Worker 启动预检与运维
// 手工核验共用的**离线**入口。它不装配运行期、不取实例锁、不迁移、不连库——只按
// `LoadMasterKey` 的既有语义重读一次密钥文件与直接父目录权限，算出公开摘要后立即清零。
//
// 为什么单独一条命令而不是让 entrypoint 自己解析 JSON：镜像里没有 `jq` / `python`，
// 而密钥形态（JSON、32 字节、`k_` 前缀）与权限（文件 / **直接父目录**）的判据已经
// 集中在 `LoadMasterKey` 一处；命令面只是把这份判据变成可执行的退出码。
// The offline implementation behind `secrets check-key`: it reuses LoadMasterKey's file
// and parent-directory rules, returns public digest values only, and zeroes the key.

import (
	"github.com/ymedword/basalt/internal/cli"
)

// masterKeyCheckPort 是无状态适配器：每次 Check 都重新读文件与权限（`LoadMasterKey`
// 不缓存），因此预检结果与随后 `gateway` 启动时的读取是同一份判据、同一份事实。
// masterKeyCheckPort is stateless; every Check re-reads the file and its permissions.
type masterKeyCheckPort struct{}

// Check 校验密钥并返回公开摘要。错误来自 `LoadMasterKey` / `MasterKeyFingerprint`，
// 只点名字段与原因，**不含**密钥字节；成功路径在返回前 `Zero()`，不留密钥材料。
// Check validates the key and returns public digests; the key is zeroed before return.
func (masterKeyCheckPort) Check(path, workspaceRoot string) (cli.MasterKeyCheckReport, error) {
	key, err := LoadMasterKey(path, workspaceRoot)
	if err != nil {
		return cli.MasterKeyCheckReport{}, err
	}
	// 值接收者不影响清零效果：`Key` 是切片，清零落在同一份底层数组上。
	// The value receiver still clears the shared backing array behind the slice.
	defer key.Zero()

	fingerprint, err := MasterKeyFingerprint(key)
	if err != nil {
		return cli.MasterKeyCheckReport{}, err
	}
	return cli.MasterKeyCheckReport{KeyID: key.KeyID, Fingerprint: fingerprint}, nil
}
