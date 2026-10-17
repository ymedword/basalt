package memory

// 本文件是记忆**门面**（颗粒度文档 §2.3「门面侧」伪代码逐块同构）：作用域解析、
// 生效层回退（两层**不叠加**）、per-scope 写锁。注入期超配额 WARN 随 US4（T046）
// 落在同一文件。
// This file is the facade: scope resolution, effective-layer fallback without stacking,
// and the in-process per-scope write lock.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// sectionCoreTitle / sectionArchiveTitle 是两分区标题的**唯一字面量来源**——解析
	// （parseSections）、渲染（renderMemory）与门面切区（coreSectionOf）三处都引它俩；
	// 标题字面量写死三份必然漂移。
	// The single source of truth for the two section headings.
	sectionCoreTitle    = "## 核心记忆"
	sectionArchiveTitle = "## 归档记忆"
)

// Options 是门面的装配面。字段即依赖：没有一项从包级状态或 context 里取（宪法 VII
// ——业务状态必须显式传递）。
// Options is the facade's assembly surface; every dependency is an explicit field.
type Options struct {
	Backend core.LongTermMemoryStore // 载体（markdown / sqlite）——**必填**
	Logger  *slog.Logger             // 为空 → 进程缺省日志器（注入期 WARN 用，US4）
}

// Service 是记忆的**唯一门面**：作用域解析、生效层回退与写锁只有这一份实现，
// 载体是可替换的端口实现——换档时本类型一个字段都不动。
// Service is the single facade; carriers are swappable implementations behind the port.
type Service struct {
	backend core.LongTermMemoryStore
	logger  *slog.Logger
	locks   *scopeLocks
}

// 编译期断言：门面同时是「读写面」（工具与运行侧经它落记忆）与「提示词消费面」
// （PromptBuilder 只经这一个方法取注入文本）。
var (
	_ core.LongTermMemoryStore = (*Service)(nil)
	_ core.MemoryReader        = (*Service)(nil)
)

// NewService 组装门面。**没有载体就没有记忆**：`Backend == nil` 是装配错误，当场拒绝
// ——若放行，后续每次运行都会以「空记忆」静默降级，把装配故障吞进提示词里。
// NewService assembles the facade; a nil backend is an assembly error, refused up front
// rather than silently degrading every future run to "no memory".
func NewService(opts Options) (*Service, error) {
	if opts.Backend == nil {
		return nil, errors.New("memory: backend is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Service{backend: opts.Backend, logger: opts.Logger, locks: newScopeLocks()}, nil
}

// Append 向 scope 层的 section 分区追加一条记忆。临界区只覆盖这一笔写：配额判定与
// 追加同原子域（在载体内），门面这一层保证的是**同一作用域的写入串行**。
// Append serializes writes per scope; the quota decision lives in the carrier, in the
// same atomic region as the insert itself.
func (s *Service) Append(ctx context.Context, scope core.Scope, section core.Section, content string) error {
	unlock := s.locks.lock(scope)
	defer unlock()
	return s.backend.Append(ctx, scope, section, content)
}

// Load 读取整层原文——管理面与巡检用；注入面走 LoadEffective（只取核心区）。
// Load returns the whole layer; the injection surface goes through LoadEffective.
func (s *Service) Load(ctx context.Context, scope core.Scope) (string, error) {
	return s.backend.Load(ctx, scope)
}

// RecallByKeyword 经载体在归档区做整串检索。只读，不入写锁——与 Append 的串行面
// 无关（并发读由载体自身的读路径承担）。
// RecallByKeyword searches the archive through the carrier; reads never take the write lock.
func (s *Service) RecallByKeyword(ctx context.Context, scope core.Scope, query string) ([]string, error) {
	return s.backend.RecallByKeyword(ctx, scope, query)
}

// LoadEffective 返回**生效层**的核心区文本：有 Agent 上下文且该层有内容 → Agent 层，
// 否则全局层；两层**不叠加**（Agent 层命中时全局层一个字都不进）。回退判定**只此
// 一处**——提示词侧与工具侧都不得再判一次。
// The effective-layer judgment lives here and nowhere else; layers never stack.
//
// 载体 error 一律**原样上抛**（约束 23）：读取失败 = 拒绝启动（零 LLM 调用），
// 不得降级成「当空层」。
//
// 注入期超配额（约束 11 注入侧 / FR-006）：核心区总量超过写配额只可能来自**手工编辑**
// ——工具写入不可能越限。此路径记 WARN 并**完整返回**（不截断、不丢条），超限的证据
// 由 `coreSectionOf` 采集（它就是切区的地方）。
// The injection-side over-quota path can only come from hand edits; coreSectionOf logs
// the WARN and still returns the whole core region.
func (s *Service) LoadEffective(ctx context.Context, agent string) (string, error) {
	scope := scopeOf(agent)
	text, err := s.backend.Load(ctx, scope)
	if err != nil {
		return "", err
	}
	if !layerNonEmpty(text) {
		scope = core.GlobalScope
		text, err = s.backend.Load(ctx, scope)
		if err != nil {
			return "", err
		}
	}
	return s.coreSectionOf(scope, text)
}

// scopeOf 把「有无 Agent 上下文」解析成作用域——**只此一处**。空串 = 全局层，是正常
// 取值不是异常（运维直连会话没有 Agent 上下文，照样能读写全局层）。
// scopeOf resolves the agent context into a scope; the empty string means the global
// layer, a normal value rather than an error.
func scopeOf(agent string) core.Scope {
	if agent == "" {
		return core.GlobalScope
	}
	return core.Scope{Agent: agent}
}

// layerNonEmpty 判「该层是否有内容」——判据是**任一分区正文非空**（口径 2），**不是**
// 「核心区非空」：Agent 层只写了归档条目时，生效层仍是它自己那一层（段 2 因此得空
// 文本，**不得**据此回退去注全局层的核心区）。
//
// 结构行（文件标题与分区标题）与分区外的文本（模板说明段）都**不算内容**；分区内任何
// 非空行都算——裸段落按条目收集（约束 28），故也算内容。
// The layer is non-empty when any section holds content; headings and the section-less
// preamble never count, and a layer holding only archive entries stays the effective one.
func layerNonEmpty(text string) bool {
	inSection := false
	for _, line := range splitLines(text) {
		switch {
		case strings.HasPrefix(line, "## "): // 分区标题：进分区
			inSection = true
		case strings.HasPrefix(line, "# "): // 文件标题：分区之外（先判 "## " 故不误命中）
			inSection = false
		case inSection && strings.TrimSpace(line) != "":
			return true
		}
	}
	return false
}

// coreSectionOf 从载体 Load 的文本里切出**核心区**（含分区标题行）；核心区无条目 →
// `""`（段 2 不产生文本——空字符串让提示词侧连分隔符都不拼，约束 19）。
// coreSectionOf cuts the core region out of the loaded text; an empty core yields "".
//
// 注入面读的是**重建结果**（约束 28 / 口径 22）：`parseSections` 切出的是条目**正文**
// （无 `"- "` 前缀、无续行缩进），本函数按载体的文档化形态（`renderEntry`）渲染回来
// ——注入文本因此逐字节等于"该文件重建后核心区块的样子"，人手写的裸段落也一律以
// 列表项形态进提示词。**不得**直接把正文 join 出去（§4.3 R4a 的全文相等断言即判据）。
// The injection face returns the rebuilt core region: bodies come back through the same
// documented renderer the carriers use, so bare handwritten paragraphs are injected as
// list items too.
//
// 注入期超配额（约束 11 注入侧 / 口径 ④）：切区时同一份解析结果顺手检查**两条上限**
// ——单条 4096 与核心区总量 32768（FR-021），口径与写入面**同一算法**（`sectionBytes`：
// 条目正文 UTF-8 字节，parse 后已 TrimSpace），只排除文件标题 / 分区标题 / `"- "` 前缀 /
// 续行缩进这些载体添加物。超限记 WARN（层的规范路径 + 实际 / 上限两个数字；**不得**打印
// 记忆正文——本路径无审计行，日志是唯一留痕）并**照常完整返回**：手工编辑不归工具配额
// 管，截断或丢条会让 Agent 静默变哑。
// The same parse feeds the write-side byte metric; over the limit it logs a WARN
// (path, actual, limit — never memory text) and still returns every entry.
//
// 形态适配（唯一一处）：伪代码写 `coreSectionOf(text)` 单返回值；Go 落地把
// `parseSections` 的解析 error 经返回值上抛（P1-1：结构损坏不得吞成"空层"），
// 调用点因此多一层 error 传递——断言与行为判据不变。作用域参数只服务于 WARN 的
// 路径点位（WARN 需要 logger，故本函数是方法而非包级函数）。
func (s *Service) coreSectionOf(scope core.Scope, text string) (string, error) {
	coreEntries, _, err := parseSections(text)
	if err != nil {
		return "", err
	}
	if len(coreEntries) == 0 {
		return "", nil
	}
	for i, entry := range coreEntries {
		if len(entry) > core.MaxCoreEntryBytes {
			s.logger.Warn("memory: 核心记忆单条超出写配额，仍完整注入",
				"path", workspace.MemoryFilePath(scope.Agent),
				"entry", i+1,
				"bytes", len(entry),
				"limit", core.MaxCoreEntryBytes)
		}
	}
	if total := sectionBytes(coreEntries); total > core.MaxCoreSectionBytes {
		s.logger.Warn("memory: 核心记忆区超出写配额，仍完整注入",
			"path", workspace.MemoryFilePath(scope.Agent),
			"bytes", total,
			"limit", core.MaxCoreSectionBytes)
	}
	rendered := make([]string, 0, len(coreEntries))
	for _, entry := range coreEntries {
		rendered = append(rendered, renderEntry(entry))
	}
	return sectionCoreTitle + "\n\n" + strings.Join(rendered, "\n\n"), nil
}

// scopeLocks 是进程内 per-scope 写锁：**按完整 core.Scope 分档**——每个 Agent 一把、
// 全局层一把。同作用域串行；**不同 Agent 之间不得互相阻塞**（按「层」只建两把会让
// 所有 Agent 共用一把锁，把并发的多 Agent 写入排成一队）。跨进程**不承诺**（约束 13）。
// scopeLocks keys one mutex per full core.Scope: the same scope serializes, different
// agents never block each other. Cross-process serialization is explicitly not promised.
type scopeLocks struct {
	mu    sync.Mutex
	locks map[core.Scope]*sync.Mutex
}

func newScopeLocks() *scopeLocks {
	return &scopeLocks{locks: map[core.Scope]*sync.Mutex{}}
}

// lock 取该作用域的锁并返回释放函数。查表本身只持小锁，**不得**把载体写入放进表锁里
// （放进去了就把所有作用域串成一把大锁，分档失去意义）。
func (l *scopeLocks) lock(scope core.Scope) func() {
	l.mu.Lock()
	m, ok := l.locks[scope]
	if !ok {
		m = &sync.Mutex{}
		l.locks[scope] = m
	}
	l.mu.Unlock()

	m.Lock()
	return m.Unlock
}
