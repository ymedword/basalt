// Package pgtemp 的夹具闸门：把**全测试库范围**的准备操作按对象计锁限流。
//
// 共享锁表的容量是全库共用的：`max_locks_per_transaction` × `max_connections`（本机测试库
// 64 × 100 ≈ 6,400 条）。同库跑 `go test ./...` 时，每个包各自建 schema、各自迁移，谁也
// 不知道别人占了多少锁——进程内的信号量没有意义，闸门必须**跨进程**，所以用会话级
// advisory 锁表示槽位：锁挂在一条常驻连接上，取到后一直持有到释放。
//
// 起因（2026-11-05 实测）：迁移锁改为按 schema 派生后，几十个 `t.Parallel()` 夹具真正
// 并行迁移，把锁表打爆——`pg_locks` 峰值 17,834 行、22 个并发会话，报
// `ERROR: out of shared memory (SQLSTATE 53200)`；连 `DROP SCHEMA ... CASCADE` 也失败
// 并留下残留 schema。
//
// 两种操作各占多少锁（2026-11-05 对真实迁移链实测，schema 335 个关系）：
//   - 全量迁移在一个事务里建完全部对象：峰值 **1,136** 条锁，耗时 **2.862s**；
//   - `DROP SCHEMA ... CASCADE`：**1,597** 条锁，提交耗时 **644ms**。
//
// 路数按实测择优（2026-11-05，同一批 1,724 条用例整包跑 `internal/app`，每次只改这两个
// 常量）：迁移 3 路 + 回收 1 路 = 3 × 1,136 + 1,597 = **5,005 < 6,400**，留约 1,400 条
// 余量给其它并行会话，整包 **802.7s**、全程无 53200、无回收超时；迁移 1 路 + 回收 2 路
// 1,057.3s（最慢）、迁移 2 路 + 回收 1 路 829.5s（另有一次回收撞 10s 上限假红）、
// 共用 1 路 878.95s；迁移 3 路 + 回收 2 路 = 6,602 > 6,400，会再次打爆锁表。
//
// 方向是「迁移路数优先」：Go 的顶层 `t.Parallel()` 用例要等全部串行用例跑完才并发启动，
// 等待因此高度集中（实测前 11 分钟合计等待 < 1s，全部 1,997s 集中在最后 3 分钟）。
//
// 等待一律交给服务端 `pg_advisory_lock` 排队：客户端 25ms 轮询会带来约 4,800 次查询/秒的
// 常驻背景负载，是早期变慢的主要嫌疑之一。
//
// 这是**夹具的准备节奏**，不改任何断言或覆盖率判据，也不改变产品侧的并行语义（不同
// schema 的迁移仍可并行）——所以验证「同 schema 互斥 / 不同 schema 并行」的迁移用例
// **不走**这个闸门，仍然直接调用迁移路径。
// Fixture-provisioning gate shared by every package that builds throwaway schemas:
// session-level advisory locks give a cross-process slot pool sized from measured
// lock counts (3 migrations × 1,136 + 1 drop × 1,597 = 5,005 of ~6,400).
package pgtemp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	// 槽位容量与锁预算的对应关系见包注释；改这两个数前先重量锁的占用。
	// Capacities are sized from the measured per-operation lock counts (see above).
	maxConcurrentMigrations = 3
	maxConcurrentDrops      = 1

	// 槽位键的种子；完整键 = 种子 + "/" + 槽位序号。两类闸门用不同种子，互不相干。
	// Each gate has its own key namespace so migrations and drops never share slots.
	migrationSlotSeed = "basalt/pgtest-migration-slot"
	dropSlotSeed      = "basalt/pgtest-drop-slot"
)

var (
	// slotPool 是**整个测试进程共用**的槽位连接池：槽位锁挂在会话上，等待者需要一个
	// 常驻连接；每次抢占都新建连接会让 SCRAM 握手 / TCP 建立变成主要开销。
	// slotPool is shared by the whole test process; each waiter needs a session-level
	// lock, so a reusable pool avoids paying a fresh handshake per acquisition.
	slotPoolOnce sync.Once
	slotPool     *sql.DB
	slotPoolErr  error
)

// AcquireMigrationSlot 取得一个迁移槽位，返回释放函数。迁移（`postgres.Open` 内部的全量
// 迁移）在一个事务里持约 1,136 个锁表条目，必须先排队，否则几十个并行夹具会把锁表打爆。
// 调用方**必须**在迁移返回后立刻释放，不要跨用例持有。
// AcquireMigrationSlot takes one migration slot; release it as soon as the migration
// returns rather than holding it for the whole test.
func AcquireMigrationSlot(ctx context.Context) (func(), error) {
	return acquireSlot(ctx, maxConcurrentMigrations, migrationSlotSeed)
}

// AcquireDropSlot 取得一个回收槽位，返回释放函数。`DROP SCHEMA ... CASCADE` 在删每个
// 表 / 索引时持锁（实测 1,597 条），与并发迁移叠加同样会触发 53200。
// AcquireDropSlot takes one drop slot; DROP SCHEMA CASCADE holds a lock per object.
func AcquireDropSlot(ctx context.Context) (func(), error) {
	return acquireSlot(ctx, maxConcurrentDrops, dropSlotSeed)
}

// slotPoolDB 惰性建立槽位连接池。池本身不关闭——它的生命周期就是这个测试进程。
// slotPoolDB lazily builds the shared slot pool; it lives as long as the process.
func slotPoolDB() (*sql.DB, error) {
	slotPoolOnce.Do(func() {
		base := os.Getenv(TestDSNEnv)
		if base == "" {
			slotPoolErr = fmt.Errorf("未设置 %s——夹具槽位需要同一个测试库", TestDSNEnv)
			return
		}
		pool, err := sql.Open("pgx", base)
		if err != nil {
			slotPoolErr = fmt.Errorf("打开槽位连接池: %w", err)
			return
		}
		// 阻塞等待期间每个等待者占一条连接；释放后只保留少量空闲连接复用。
		pool.SetMaxIdleConns(maxConcurrentMigrations + maxConcurrentDrops + 1)
		pool.SetConnMaxIdleTime(time.Minute)
		slotPool = pool
	})
	return slotPool, slotPoolErr
}

// slotKey 把一个槽位序号派生成 int64 advisory 键（sha256 前 8 字节，大端；与生产的
// advisoryKey 同一派生方式）。键只用于 `pg_advisory_lock`，**不落库**。
// slotKey derives the advisory key for one slot.
func slotKey(seed string, i int) int64 {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", seed, i)))
	hi := int64(binary.BigEndian.Uint32(sum[:4]))
	lo := int64(binary.BigEndian.Uint32(sum[4:8]))
	return hi<<32 | lo
}

// acquireSlot 在测试库上抢一个槽位，返回释放函数。槽位用**会话级** advisory 锁表示：
// 锁挂在一条常驻连接上，取到后一直持有到释放，因此跨进程互斥。槽位全被占时用
// `pg_advisory_lock` **阻塞排队**——等待发生在服务端，不轮询、不占 CPU。
// acquireSlot takes one cluster-wide slot via a session advisory lock and returns its
// release function; waiting is a server-side queue, not a client-side poll.
func acquireSlot(ctx context.Context, capacity int, seed string) (func(), error) {
	pool, err := slotPoolDB()
	if err != nil {
		return nil, err
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("取槽位连接: %w", err)
	}
	fail := func(err error) (func(), error) {
		_ = conn.Close()
		return nil, err
	}
	release := func(key int64) func() {
		return func() {
			// 释放不能依赖调用方上下文（cleanup 时它已被取消）。
			releaseCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = conn.ExecContext(releaseCtx, `SELECT pg_advisory_unlock($1)`, key)
			_ = conn.Close()
		}
	}

	// 起点取本连接的后端 pid：同一时刻的不同连接 pid 不同（跨测试进程也成立），并发
	// 调用者因此从不同槽位开始扫，不会全部撞在 0 号；pid 恒为正，取模不会出负数。
	// The start slot is the connection's backend PID: distinct connections (including
	// those of other test binaries) get distinct starts without any shared counter.
	var backendPID int
	if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		return fail(fmt.Errorf("读取槽位连接的后端 pid: %w", err))
	}
	start := backendPID % capacity

	// 先非阻塞试一遍全部槽位：空闲时只有一次往返，不进入排队。
	for i := range capacity {
		slot := (start + i) % capacity
		var taken bool
		if err := conn.QueryRowContext(ctx,
			`SELECT pg_try_advisory_lock($1)`, slotKey(seed, slot)).Scan(&taken); err != nil {
			return fail(fmt.Errorf("尝试占夹具槽位 %d: %w", slot, err))
		}
		if taken {
			return release(slotKey(seed, slot)), nil
		}
	}

	// 全部占用：阻塞在起点槽位上排队。起点按 pid 分散是为了让多个等待者落在不同槽位
	// ——若所有等待者都堵在同一个槽，别的槽释放后无人接手，并行度会退化成 1
	// （2026-11-05 实测：阻塞在固定 0 号槽让整包从 601.7s 拖到 765.7s）。
	// Block on the rotated start slot so waiters spread out over the pool.
	key := slotKey(seed, start)
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		return fail(fmt.Errorf("等待夹具槽位: %w", err))
	}
	return release(key), nil
}
