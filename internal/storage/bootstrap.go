package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ymedword/basalt/internal/core"
)

// Bootstrap 是 init 的引导入口：写模式打开的**策略层薄封装**。
// Bootstrap is the bootstrap entry used by init.
//
// 与 OpenWritable 同序（装配 + 取写权 + 重判态），差别在两条策略：旧版本库在此
// **立即拒绝**（core.ErrSchemaOutdated）而不是正常打开——迁移不是引导行为（颗粒度
// 文档 §2.4 裁决注）；已有当前版本库**跳过持久化初始化**（连 WAL 都不切，装配
// 顺序第 3 条见 openWritable）。
//
// created 为 true 仅当**本进程实际创建**了 schema：判定发生在写权之内，所以
// 并发竞争的其余各方在写锁释放后重判所见必然是"已存在"，一律 Kept（false）且
// 不报错——「先判态后写入」的竞态在这里被结构性地关掉（约束 16）。
// created is true only for the process that actually created the schema; the
// re-judgement happens inside the write lock, so every competitor sees the
// version row and returns Kept without an error.
//
// 「Kept 零写」在本入口**逐字节**成立：已有当前版本库时连 WAL 切换都跳过——
// journal_mode 是库级持久属性，切换即改写库头（实测：完整 v1、DELETE 模式的库
// 二跑 init 报"零写"且 exit 0，库头却变成 WAL、SHA256 改变）。装配顺序见
// openWritable。
// On this entry point "Kept writes nothing" holds byte-for-byte: an existing
// current-version database skips even the persistent WAL switch.
//
// 返回前必定关闭写句柄：init 不留长期写连接，否则后续命令会与"看起来还活着"
// 的 init 抢写锁。
// The handle is always closed before returning; a bootstrap never keeps a
// long-lived writer.
func Bootstrap(ctx context.Context, path string, target int) (created bool, err error) {
	db, created, err := openWritable(ctx, path, target, true)
	if err != nil {
		return false, err
	}
	// 关闭失败并入错误链：created 已知，但句柄未干净释放不得算成功。
	return created, db.Close()
}

// openWritable 是 OpenWritable 与 Bootstrap 的共同实现；refuseOutdated 是二者
// 唯一的策略差异（Bootstrap 拒绝旧库，OpenWritable 正常打开但不迁移）。
// openWritable backs both entry points; refuseOutdated is their only difference.
//
// 两条路径共用同一段「取写权 → 重判态」代码，不是为省几行：策略分叉必须发生在
// **写权之内**，否则 Bootstrap 的拒绝判定会落到锁外，与并发迁移者产生窗口。
// The policy branch deliberately lives inside the write lock.
//
// **装配顺序即正确性**（「判态 → WAL → 读池」，两处都踩过）：
//  1. **判态先于持久化修改**：WAL 切换会改写库头，是**库级持久属性**，必须等
//     ensureWritableState 在写权内放行之后再做——否则「拒绝一个过高 / 损坏 /
//     旧版库」这个动作本身就把库改了（实测：过高版本的 DELETE 模式库在被拒绝的
//     init 里被切成 WAL，SHA256 改变），零写承诺（颗粒度文档 §2.3）随之落空。
//  2. **WAL 先于读池**：journal_mode 的切换要求此刻没有别的连接持有该库，否则
//     SQLite **静默拒绝**（实测：先开读池再设 WAL，回读仍是 delete）。
//  3. **Kept 也不得写**：已有当前版本库（Bootstrap 路径 created=false）连 WAL
//     切换都必须跳过——「Kept」承诺的是对库字节零改动，WAL 切换改写库头即违约
//     （实测：完整 v1、DELETE 模式的库二跑 init 后库头变 WAL、SHA256 改变）。
//     OpenWritable 是长期服务入口，其装配策略**另行规定**：一律确保 WAL——打开
//     即服务，多读并发依赖它；该路径不适用零写承诺。
//
// The judgement must precede the persistent WAL switch (a refusal must not touch
// the file), and the WAL switch must precede the read pool. On the Bootstrap
// path an existing current-version database skips the switch entirely — Kept
// means zero bytes written.
func openWritable(ctx context.Context, path string, target int, refuseOutdated bool) (*DB, bool, error) {
	if target < 1 {
		return nil, false, fmt.Errorf("storage: 目标版本 %d 非法——版本从 1 起", target)
	}

	db := &DB{path: path}
	if err := db.openWritePool(ctx); err != nil {
		return nil, false, err
	}

	created, err := db.ensureWritableState(ctx, target, refuseOutdated)
	if err != nil {
		// 打开失败即关闭：带错误的句柄不交给调用方；关闭失败并入错误链。
		return nil, false, errors.Join(err, db.Close())
	}
	// 条件即两条路径的策略分叉（装配顺序第 3 条）：Bootstrap（refuseOutdated）
	// 只对本进程新建的库做持久化初始化，已有当前版本库**连 WAL 都不切**——Kept
	// 的零写承诺是对库字节零改动；OpenWritable 则一律确保 WAL。
	// The condition encodes the split: only OpenWritable, or a Bootstrap that
	// actually created the database, runs the persistent initialisation.
	if created || !refuseOutdated {
		if err := applyWAL(ctx, db.write); err != nil {
			return nil, false, errors.Join(err, db.Close())
		}
	}
	if err := db.openReadPool(ctx, false); err != nil {
		return nil, false, errors.Join(err, db.Close())
	}
	return db, created, nil
}

// ensureWritableState 在写权内重判态并按状态矩阵处置，返回"本进程是否为实际
// 创建者"（颗粒度文档 §2.3 状态矩阵的写路径一列）。
// ensureWritableState re-judges the state under the write lock.
func (db *DB) ensureWritableState(ctx context.Context, target int, refuseOutdated bool) (bool, error) {
	created := false
	err := withWriteLock(ctx, db.write, func(ctx context.Context, conn *sql.Conn) error {
		state, current, err := inspectSchema(ctx, conn, target)
		if err != nil {
			return err
		}
		switch state {
		case stateNew:
			created = true
			return createNewDatabase(ctx, conn, schemaFS, target)
		case stateCurrent:
			return nil // 空事务提交即可（Kept 的可写形态）
		case stateOutdated:
			if refuseOutdated {
				return fmt.Errorf("%w（库内 v%d < 目标 v%d）——升级是持锁重命令的显式动作",
					core.ErrSchemaOutdated, current, target)
			}
			return nil // 打开成功、不迁移
		case stateTooNew:
			return fmt.Errorf("%w（库内 v%d > 目标 v%d）——请升级二进制",
				core.ErrSchemaTooNew, current, target)
		case stateCorrupt:
			return core.ErrSchemaCorrupt
		}
		// 五种状态在上方穷举——保留兜底，防新增状态漏改（不可达）。
		return fmt.Errorf("storage: 未处理的 schema 状态 %d", state)
	})
	return created, err
}
