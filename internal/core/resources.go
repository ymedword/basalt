package core

// 本文件承载运行资源清单的持久形态与规整序列化规则（US-11）：清单在准入时冻结，
// 一次 Run 一份，哈希必须只由持久结构决定——不混入时间戳、随机值或展示字段。
// 后续任务在同一文件补齐 ResourceGate、准入解析与视图投影（T015）。
// This file declares the persistent run-resource manifest and its canonical
// serialization: one manifest per run, frozen at admission, hashed only over the
// persisted structure, with no timestamps, randomness, or view-only fields.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// runResourceManifestFormatVersion 是清单持久形态的首版版本号；结构变更必须同时升级
// schema 与视图，不得让旧读者按子集猜测（schema.sql 的 DEFAULT 1 与之同值）。
// runResourceManifestFormatVersion pins the first manifest format; structural changes
// must bump it together with the schema and views.
const runResourceManifestFormatVersion = 1

// ResourceGate 是发布与准入共用的**进程级唯一**读写闸（颗粒度文档约束 25）：准入持读锁、
// US-12 的资源发布持写锁，一次发布对 Run 来说要么全可见、要么全不可见。不得出现第二套互斥。
// ResourceGate is the single process-wide publish/admission gate: admission takes the
// read lock, publishing takes the write lock, and no second mutex may exist.
type ResourceGate struct {
	mu sync.RWMutex
}

// RLock 取得准入读锁（多个 Run 可同时准入）。
// RLock takes the admission read lock.
func (g *ResourceGate) RLock() { g.mu.RLock() }

// RUnlock 释放准入读锁。
// RUnlock releases the admission read lock.
func (g *ResourceGate) RUnlock() { g.mu.RUnlock() }

// Lock 取得发布写锁（与全部准入互斥）。
// Lock takes the publish write lock, excluding every admission.
func (g *ResourceGate) Lock() { g.mu.Lock() }

// Unlock 释放发布写锁。
// Unlock releases the publish write lock.
func (g *ResourceGate) Unlock() { g.mu.Unlock() }

// MCPHandle 是某个 MCP server 在本次运行内固定的连接视图：本 US 只要求它报出自己的
// server 名（版本号记在 ResolvedResources 与清单里），US-12 的 supervisor 在此基础上
// 追加启动 / 停止生命周期。方法名避开 `Server` 字段，便于连接类型就地适配。
// MCPHandle is the per-run view of one MCP server; US-11 only requires its name, and
// US-12 will extend the lifecycle around it.
type MCPHandle interface {
	ServerName() string
}

// RunRuntime 是准入时固定的不可变运行时视图：Provider 客户端映射与 MCP 句柄。技能版本
// 目录由 ResolvedResources.SkillDirs 固定。Release 幂等；引用计数归零后才允许回收旧快照。
//
// 准入读锁的租约**随 Resolve 返回、延续到 StartRun 成功后**：成功路径由调用方调用
// ReleaseAdmission 归还（运行时本身活到运行结束，再经 Release 归还引用）；解析之后、
// 账本落库之前的任何失败路径经 Release 一并归还（人工 review ② US-12 交接裁决）。
// 两个方法都幂等，先 ReleaseAdmission 后 Release 不得重复归还。
// The admission read-lock lease travels with the resolved runtime and is released by
// ReleaseAdmission after StartRun succeeds; failure paths release it through Release.
type RunRuntime interface {
	Provider(name string) (ProviderClient, bool)
	MCP(name string) (MCPHandle, bool)
	ReleaseAdmission()
	Release()
}

// ResolvedResources 是准入解析出的发布版本集合：版本与运行时来自**同一快照**，
// SkillDirs 给出每个技能发布版本的绝对目录（运行中按清单读文件，不回读工作副本）。
// ResolvedResources pairs the published versions with the exact runtime they were
// resolved against; SkillDirs holds each skill version's absolute directory.
type ResolvedResources struct {
	Provider   ResourceVersionRef
	MCPServers []ResourceVersionRef
	Skills     []SkillVersionRef
	SkillDirs  map[string]string // 技能名 → 发布版本绝对目录
	Runtime    RunRuntime        // 与上述版本逐一对应；调用方负责 Release
}

// RunResources 是准入解析端口：实现（app）在 ResourceGate.RLock 内查发布指针、构造
// ResolvedResources 并固定运行时；任一绑定资源缺失 → ResourceUnavailableError。
// RunResources resolves one agent's bound resources under the gate's read lock.
type RunResources interface {
	Resolve(ctx context.Context, entry AgentEntry) (ResolvedResources, error)
}

// RunSnapshotBuilder 把准入解析结果与配置快照合成为一次运行的冻结输入。
// RunSnapshotBuilder folds the resolved resources into one run's frozen snapshot.
type RunSnapshotBuilder interface {
	Build(ctx context.Context, entry AgentEntry, resolved ResolvedResources) (ConfigSnapshot, error)
}

// ResourceKind 是资源版本表的三值字面量（schema.sql CHECK 逐字）。
// ResourceKind is the three-value literal set of resource kinds.
type ResourceKind string

const (
	// ResourceModelConnection 是模型连接（Provider）资源。
	ResourceModelConnection ResourceKind = "model_connection"
	// ResourceMCPServer 是 MCP 服务资源。
	ResourceMCPServer ResourceKind = "mcp_server"
	// ResourceSkill 是技能资源。
	ResourceSkill ResourceKind = "skill"
)

// ErrResourceNotFound 表示按标识查不到资源版本 / 指针——只读面映射 404，
// 发布路径不得把"没查到"当成"无指针、可随意写"。
// ErrResourceNotFound means a resource version or pointer does not exist; readers map
// it to 404 and publish paths never treat it as a blank slate.
var ErrResourceNotFound = errors.New("resource not found")

// ResourceVersion 是 resource_versions 行的领域形态，也是 US-12 发布路径的输入
// （storage 与 US-12 共用本类型，**不得**另建同名类型）。
// ResourceVersion is the domain shape of one immutable resource version row.
type ResourceVersion struct {
	ID             int64
	Kind           ResourceKind
	ResourceID     string
	VersionNo      int
	ContentHash    string
	DefinitionYAML string
	ManifestJSON   string
	StoragePath    string
	CreatedAt      int64 // Unix 毫秒
	OperationID    string
}

// ResourcePointer 是 resource_pointers 行的领域形态：发布指针与运行时实际生效指针
// 可以不同（MCP 首版需重启）。可空列用指针表达"未生效 / 未删除"。
// ResourcePointer is the domain shape of a publish pointer; the published and applied
// versions may differ until a restart catches MCP up.
type ResourcePointer struct {
	Kind               ResourceKind
	ResourceID         string
	PublishedVersionID int64
	AppliedVersionID   *int64 // nil = 基线尚未生效（MCP 首版重启前）
	DeletedAt          *int64 // Unix 毫秒；nil = 未删除
	UpdatedAt          int64
	OperationID        string
}

// ResourceVersionRef 指向一个不可变资源版本（契约 run-and-errors §1.1）。
// 名字是 provider / MCP server / 技能名；版本号逐资源从 1 起。
// ResourceVersionRef points at one immutable resource version.
type ResourceVersionRef struct {
	Name        string `json:"name"`
	VersionID   int64  `json:"version_id"`
	VersionNo   int    `json:"version_no"`
	ContentHash string `json:"content_hash"`
}

// FileHash 是技能清单中的一个文件：路径相对技能目录，哈希为小写十六进制。
// FileHash is one file inside a skill version; the path is relative to the skill dir.
type FileHash struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// SkillVersionRef 冻结一个绑定技能的发布版本与完整文件清单。
// Files 由 BuildRunResourceManifest 规范化为非 nil、按 path 升序；运行中按需读取时
// 只认这份清单，不回读发布目录以外的最新版本。
// SkillVersionRef freezes one bound skill's published version and full file list.
type SkillVersionRef struct {
	ResourceVersionRef
	Files []FileHash `json:"files"`
}

// RunResourceManifest 是准入时固定的资源版本清单（契约 run-and-errors §1.1）：
// 与 ConfigSnapshot 是两份记录——config_hash 保持原义，本清单只记 Provider / MCP /
// 技能的发布版本，**不得**倒写进 config_hash，也**不得**用当前版本给历史 Run 补造。
//
// 规整序列化规则（T007）：
//   - 字段顺序固定为结构体声明序（encoding/json 的输出即声明序），紧凑、无多余空白；
//   - 清单内**不得**出现 map——键序只由结构体声明决定，不引入第二套约定；
//   - MCPServers / Skills 按 name 升序、每个技能的 Files 按 path 升序，由
//     BuildRunResourceManifest 在构造时规范化；三个列表一律非 nil（空清单序列化为 []）；
//   - 不含时间戳、随机值与 HTTP 视图字段；哈希只对持久结构计算。
//
// RunResourceManifest is the resource ledger fixed at admission. The JSON field order
// is the struct declaration order; maps are forbidden; the builder normalizes list
// order (names ascending, file paths ascending) and makes every list non-nil.
type RunResourceManifest struct {
	FormatVersion  int                  `json:"format_version"`
	Hash           string               `json:"hash"`
	AgentLifecycle string               `json:"agent_lifecycle_state"`
	Provider       ResourceVersionRef   `json:"provider"`
	MCPServers     []ResourceVersionRef `json:"mcp_servers"`
	Skills         []SkillVersionRef    `json:"skills"`
}

// MarshalManifest 输出清单的规整 JSON：字段顺序固定、紧凑无多余空白。
// 调用方**必须**先经 BuildRunResourceManifest 规范化列表顺序；本函数不做排序，
// 也不读取系统时间——同一入参在任意时刻产出逐字节相同的输出。
// MarshalManifest emits the canonical compact JSON in struct-declaration order; it
// performs no sorting and never reads the clock, so equal input gives equal bytes.
func MarshalManifest(m RunResourceManifest) ([]byte, error) {
	return json.Marshal(m)
}

// HashManifest 计算 `manifest_hash`：把 Hash 字段置空后取规整序列化字节的 sha256，
// 结果为小写十六进制。Hash 是派生值——**不得**把它自己纳入哈希输入（否则自引用）。
// HashManifest computes the manifest hash over the canonical bytes with Hash cleared,
// returning lowercase hex; the hash never feeds on itself.
func HashManifest(m RunResourceManifest) (string, error) {
	m.Hash = ""
	raw, err := MarshalManifest(m)
	if err != nil {
		return "", fmt.Errorf("清单序列化失败: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// BuildRunResourceManifest 由准入解析结果构造清单：先按 name 升序规范化 MCP 与技能、
// 再按 path 升序规范化每个技能的文件清单，三个列表一律非 nil，最后回填 `Hash`。
// 输入切片不被修改（调用方可能还在别处持有同一份解析结果）。
// BuildRunResourceManifest normalizes ordering (names ascending, file paths ascending),
// makes every list non-nil, fills Hash, and never mutates its input.
//
// 缺失绑定资源（Provider / MCP / 技能名字为空）直接报错：注册不变量已被破坏的运行
// **不得**带着空清单落库——空清单会被读成"这一跑确实没用任何资源"。
// A missing binding is an error: an empty manifest would be read as "no resources used".
func BuildRunResourceManifest(state AgentState, resolved ResolvedResources) (RunResourceManifest, error) {
	if resolved.Provider.Name == "" {
		return RunResourceManifest{}, errors.New("构造资源清单：缺少 Provider 发布版本")
	}

	manifest := RunResourceManifest{
		FormatVersion:  runResourceManifestFormatVersion,
		AgentLifecycle: string(state),
		Provider:       resolved.Provider,
		MCPServers:     make([]ResourceVersionRef, 0, len(resolved.MCPServers)),
		Skills:         make([]SkillVersionRef, 0, len(resolved.Skills)),
	}

	for _, ref := range resolved.MCPServers {
		if ref.Name == "" {
			return RunResourceManifest{}, errors.New("构造资源清单：MCP 服务发布版本的 name 为空")
		}
		manifest.MCPServers = append(manifest.MCPServers, ref)
	}
	slices.SortFunc(manifest.MCPServers, compareVersionRef)

	for _, skill := range resolved.Skills {
		if skill.Name == "" {
			return RunResourceManifest{}, errors.New("构造资源清单：技能发布版本的 name 为空")
		}
		normalized := skill
		normalized.Files = make([]FileHash, len(skill.Files))
		copy(normalized.Files, skill.Files)
		slices.SortFunc(normalized.Files, func(a, b FileHash) int {
			return strings.Compare(a.Path, b.Path)
		})
		manifest.Skills = append(manifest.Skills, normalized)
	}
	slices.SortFunc(manifest.Skills, func(a, b SkillVersionRef) int {
		return strings.Compare(a.Name, b.Name)
	})

	hash, err := HashManifest(manifest)
	if err != nil {
		return RunResourceManifest{}, err
	}
	manifest.Hash = hash
	return manifest, nil
}

// compareVersionRef 是 ResourceVersionRef 的 name 升序比较器（清单与视图共用同一口径）。
// compareVersionRef orders version refs by name ascending.
func compareVersionRef(a, b ResourceVersionRef) int {
	return strings.Compare(a.Name, b.Name)
}
