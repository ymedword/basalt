package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/provider"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// newProviderClients 由配置**逐条**构造显式映射表（宪法 III 从条文变成结构）：遍历的是
// 配置数组（有序、显式），键取条目自己的名字。**不得**遍历映射表推断、**不得**取"唯一的
// 那个"当默认——Go 的 map 遍历顺序随机，任何"从表里挑一个"的写法都会让同一进程的不同请求
// 指到不同供应商（宪法第三条的违反症状）。
// newProviderClients builds the explicit map entry by entry, over the ordered
// configuration slice. Nothing is ever inferred from the map itself.
//
// 重名条目已在装载期被拒（config.go 第 7 类 fail-closed），故此处不会静默丢条目。
// Duplicate names are rejected at load time, so no entry can silently vanish here.
func newProviderClients(cfg *cli.Config) (map[string]core.ProviderClient, map[string]string, error) {
	catalog, err := newProviderCatalog(cfg)
	if err != nil {
		return nil, nil, err
	}
	return catalog.clients, catalog.models, nil
}

// providerCatalog 是装配期**一次**构建的显式 name → client 映射快照（宪法 III 从条文变成
// 结构）。US-12 改为按已发布版本重建同一形状的目录，调用方只认这份快照、不认底下的客户端
// 构造；本 US 用它在注册与运行装配之间共享同一份"哪些 Provider 生效"的读数。
// providerCatalog is the explicit name-to-client snapshot built once at assembly;
// US-12 will rebuild the same shape from published versions.
type providerCatalog struct {
	clients map[string]core.ProviderClient
	models  map[string]string
}

// with 返回一份**新**目录快照：复制两张映射表后只替换目标键，旧快照永不原地修改——已
// 固定该快照的在途 Run 继续用旧客户端，新 Run 准入读到新表（约束 16 的写时复制）。复制
// 的是既有键值对，键仍取自版本条目自身，不构成"遍历推断"。
// with returns a fresh snapshot with one entry replaced; the old snapshot is never
// mutated, so admitted runs keep their table while new admissions see the new one.
func (c *providerCatalog) with(name string, client core.ProviderClient, model string) *providerCatalog {
	clients := make(map[string]core.ProviderClient)
	models := make(map[string]string)
	if c != nil {
		clients = make(map[string]core.ProviderClient, len(c.clients)+1)
		models = make(map[string]string, len(c.models)+1)
		for k, v := range c.clients {
			clients[k] = v
		}
		for k, v := range c.models {
			models[k] = v
		}
	}
	clients[name] = client
	models[name] = model
	return &providerCatalog{clients: clients, models: models}
}

// newProviderCatalog 按配置数组的**有序**条目逐条建表，名字取条目自己的 name——不遍历
// 映射表推断、不取"唯一那个"当默认（宪法 III 的违反症状见 CLAUDE.md 原则 3）。
// newProviderCatalog builds the table entry by entry over the ordered configuration.
func newProviderCatalog(cfg *cli.Config) (*providerCatalog, error) {
	clients := make(map[string]core.ProviderClient, len(cfg.Providers))
	models := make(map[string]string, len(cfg.Providers))

	for i, entry := range cfg.Providers {
		// 托管凭据只有 serve 重路径能解：主密钥与凭据表都在那条装配里。轻路径（chat /
		// 评测）拿不到 Key，失败关闭而不是带着空 Key 发请求（不静默降级，R4.8）。
		// A managed entry can only be resolved by the serve runtime, which owns the
		// master key and the credential table; the light path fails closed instead of
		// sending with an empty key.
		if strings.TrimSpace(entry.CredentialMode) == "managed" {
			return nil, fmt.Errorf(
				"providers[%d]: credential_mode: managed 需要 serve 重路径的凭据解析器，轻路径不能使用托管连接", i)
		}
		client, err := provider.NewOpenAIClient(provider.Options{
			Name:    entry.Name,
			BaseURL: entry.BaseURL,
			APIKey:  entry.APIKey,
		})
		if err != nil {
			// 下标只有装配层给得出——构造层看得见单个条目，不知道自己在数组里的位置。
			// Only the assembly layer knows the entry's index.
			return nil, fmt.Errorf("providers[%d]: %w", i, err)
		}
		clients[entry.Name] = client
		models[entry.Name] = entry.DefaultModel
	}
	return &providerCatalog{clients: clients, models: models}, nil
}

// managedCredentialSource 是目录构建需要的**唯一**凭据面：按发布版本解析当前 Key。
// 生产装配传 `CredentialResolver`（超集）；nil 表示本次装配没有托管凭据能力——
// 遇到 `credential_mode: managed` 的已发布条目即失败关闭，**不**退回 env、**不**猜值。
// managedCredentialSource is the only credential face the catalog needs; a nil source
// fails a managed entry closed instead of falling back to env.
type managedCredentialSource interface {
	ResolveModelCredential(ctx context.Context, connection core.ResourceVersionRef) (string, error)
}

// newProviderCatalogFromPublished 按**已发布** model_connection 版本构建显式映射表（约束 3 /
// 约束 16 / 图 A ⑥）：键取自版本条目自身的 `name`，每个已发布资源恰一个条目，**不遍历
// 映射表推断**、**不取"唯一那个"当默认**、未发布的名称绝不进表。生产装配只走这一条；
// `newProviderCatalog(cfg)` 仅保留给轻路径与测试，不新增第三个目录构造器。
//
// override 非 nil（评测注入）时每个已发布名仍各建一条替身：**不构造真实客户端、不发出
// 任何请求**，也**不解析**环境变量或解密托管凭据——替身的调用仍经 `ProviderService`
// 审计与 Run 准入固定。override 为 nil 时按条目的凭据模式取 Key：env 解析精确 `${VAR}`
// 引用，managed 经 `managedCredentialSource` 解密当前 active 版本；任一失败一律失败关闭
// （启动装配记 ERROR 并退 1），错误只点名资源与变量名 / 凭据 ID，**不回显值**。
// newProviderCatalogFromPublished builds the explicit name-to-client table from published
// model-connection versions only. With an override it never constructs a real client,
// never issues a request, never reads the environment and never decrypts.
func newProviderCatalogFromPublished(ctx context.Context, store *storage.ResourceStore,
	override core.ProviderClient, logger *slog.Logger, credentials managedCredentialSource,
) (*providerCatalog, error) {
	return newProviderCatalogFromPublishedExcept(ctx, store, override, logger, "", credentials)
}

// newProviderCatalogFromPublishedExcept 与前者共用**同一条**构建路径，只在遍历已发布
// 指针时跳过 `skipResourceID`——删除模型连接要按"移除该资源后的已发布集合"重建映射表，
// 而目标自己的 `${VAR}` 可能已从环境撤掉：目标**不得**因为自己的凭证缺失挡住自己的
// 删除。跳过发生在解析条目、解析环境变量与构造客户端**之前**；其余资源仍按原规则
// 失败关闭（构建失败保留旧表）。这不是第三个目录构造器，前者的唯一生产语义由本函数
// 承载，前者只是 `skipResourceID == ""` 的薄包装。
// newProviderCatalogFromPublishedExcept is the same construction path with one published
// pointer skipped before any entry or credential resolution, so a deleted provider's own
// missing credential can never block its deletion.
func newProviderCatalogFromPublishedExcept(ctx context.Context, store *storage.ResourceStore,
	override core.ProviderClient, logger *slog.Logger, skipResourceID string,
	credentials managedCredentialSource,
) (*providerCatalog, error) {
	pointers, err := store.ListPointers(ctx, core.ResourceModelConnection)
	if err != nil {
		return nil, fmt.Errorf("列出已发布模型连接: %w", err)
	}

	clients := make(map[string]core.ProviderClient, len(pointers))
	models := make(map[string]string, len(pointers))
	for _, pointer := range pointers {
		if skipResourceID != "" && pointer.ResourceID == skipResourceID {
			continue
		}
		entry, version, err := publishedProviderEntry(ctx, store, pointer)
		if err != nil {
			return nil, err
		}
		if _, dup := clients[entry.Name]; dup {
			return nil, fmt.Errorf("已发布模型连接的条目名 %s 重复——按名定位必须唯一", entry.Name)
		}

		if override != nil {
			clients[entry.Name] = override
		} else {
			apiKey, err := catalogAPIKey(ctx, pointer, entry, version, credentials)
			if err != nil {
				return nil, err
			}
			client, err := provider.NewOpenAIClient(provider.Options{
				Name:    entry.Name,
				BaseURL: entry.BaseURL,
				APIKey:  apiKey,
			})
			if err != nil {
				return nil, fmt.Errorf("模型连接 %s: %w", entry.Name, err)
			}
			clients[entry.Name] = client
		}
		models[entry.Name] = entry.DefaultModel
	}

	if logger != nil {
		logger.Debug("模型连接目录：按已发布版本构建", "count", len(clients))
	}
	return &providerCatalog{clients: clients, models: models}, nil
}

// catalogAPIKey 按条目声明的凭据模式取 Key：env 走精确 `${VAR}` 解析，managed 走
// `managedCredentialSource`（按发布版本解密当前 active 版本，改地址必须重新录入 Key 的
// 判据也由它执行）。模式只由条目字段判定，**不**按值形态推断；没有解析器即失败关闭。
// catalogAPIKey resolves the key by the entry's declared mode and never infers it.
func catalogAPIKey(ctx context.Context, pointer core.ResourcePointer,
	entry workspace.ProviderEntry, version core.ResourceVersion, credentials managedCredentialSource,
) (string, error) {
	switch strings.TrimSpace(entry.CredentialMode) {
	case "", "env":
		return resolveProviderAPIKey(entry.Name, entry.APIKey)
	case "managed":
		if credentials == nil {
			return "", fmt.Errorf("模型连接 %s 使用托管凭据，但本次装配没有凭据解析器", entry.Name)
		}
		key, err := credentials.ResolveModelCredential(ctx, core.ResourceVersionRef{
			Name:        pointer.ResourceID,
			ResourceUID: pointer.ResourceUID,
			VersionID:   version.ID,
			VersionNo:   version.VersionNo,
			ContentHash: version.ContentHash,
		})
		if err != nil {
			// 错误只带资源名：解析器已负责"不回显材料"，这里只补上下文。
			return "", fmt.Errorf("模型连接 %s 的托管凭据不可用: %w", entry.Name, err)
		}
		if key == "" {
			return "", fmt.Errorf("模型连接 %s 的托管凭据解析为空", entry.Name)
		}
		return key, nil
	default:
		// 条目校验已把模式限定为两个字面量；走到这里说明存储被绕过，按失败关闭处理。
		return "", fmt.Errorf("模型连接 %s 的凭据模式 %q 非法", entry.Name, entry.CredentialMode)
	}
}

// publishedProviderEntry 从发布指针取版本并解析成**恰一个**条目：版本必须属于该资源，
// 定义原文必须是块式列表片段（发布时写入的块原文），条目名必须与资源标识一致——任何
// 一条不成立都说明存储与定义分叉，启动必须失败关闭而不是构造一张错表。版本一并返回，
// 供托管凭据按**发布版本**解析（准入固定语义）。
// publishedProviderEntry resolves one published version into exactly one entry and
// fails closed on any identity or shape mismatch.
func publishedProviderEntry(ctx context.Context, store *storage.ResourceStore,
	pointer core.ResourcePointer,
) (workspace.ProviderEntry, core.ResourceVersion, error) {
	var entry workspace.ProviderEntry
	version, err := store.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return entry, core.ResourceVersion{}, fmt.Errorf(
			"读取模型连接 %s 的发布版本 %d: %w", pointer.ResourceID, pointer.PublishedVersionID, err)
	}
	if version.Kind != core.ResourceModelConnection || version.ResourceID != pointer.ResourceID {
		return entry, core.ResourceVersion{}, fmt.Errorf(
			"模型连接 %s 的发布版本 %d 归属不符（got %s/%s）",
			pointer.ResourceID, version.ID, version.Kind, version.ResourceID)
	}

	entries, err := publishedProviderEntries(version.DefinitionYAML)
	if err != nil {
		return entry, core.ResourceVersion{}, fmt.Errorf(
			"模型连接 %s 的发布版本 %d 定义非法: %w", pointer.ResourceID, version.ID, err)
	}
	if len(entries) != 1 {
		return entry, core.ResourceVersion{}, fmt.Errorf(
			"模型连接 %s 的发布版本 %d 必须恰含一个条目（实际 %d）",
			pointer.ResourceID, version.ID, len(entries))
	}
	if entries[0].Name != pointer.ResourceID {
		return entry, core.ResourceVersion{}, fmt.Errorf(
			"模型连接 %s 的发布版本 %d 条目名 %q 与资源标识不一致",
			pointer.ResourceID, version.ID, entries[0].Name)
	}
	return entries[0], version, nil
}

// publishedProviderEntries 解析已发布版本的 `definition_yaml`，兼容两种已落库形态：
//
//   - **条目映射原文**（基线写入；序列指示符已剥离，顶层是映射）；
//   - **块式列表片段**（管理写路径写入；顶层是单条目列表）。
//
// 两种形态都交给 `workspace.ValidateProviderEntries` 判卷（字段规则只有一份）；
// 映射形态包成单条目列表再解析，只影响校验输入，**不改写**存储字节或哈希。
// publishedProviderEntries accepts both stored shapes: the baseline's dash-stripped
// entry mapping and the management path's block-list excerpt.
func publishedProviderEntries(definition string) ([]workspace.ProviderEntry, error) {
	entries, err := workspace.ValidateProviderEntries([]byte(definition))
	if err != nil {
		return nil, err
	}
	if len(entries) > 0 {
		return entries, nil
	}
	return workspace.ValidateProviderEntries(asProviderSequence(definition))
}

// asProviderSequence 把条目映射原文包成单条目块式列表（首行加 `- `、其余行缩进两格）：
// 序列指示符属于外层列表，剥离与回贴都在内存里完成。
// asProviderSequence re-wraps the mapping excerpt as a one-item block list in memory.
func asProviderSequence(definition string) []byte {
	var b strings.Builder
	started := false
	for _, line := range strings.Split(definition, "\n") {
		if strings.TrimSpace(line) != "" {
			if started {
				b.WriteString("  ")
			} else {
				b.WriteString("- ")
				started = true
			}
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// resolveProviderAPIKey 把条目里的精确 `${VAR}` 占位解析为环境值：未设置与空值同档拒绝
// （空凭证送出去只会换来一个 401，排障方向会被引到供应商身上）。错误只点名资源与变量名，
// **不**回显值——值可能正是凭证。
// resolveProviderAPIKey resolves the exact ${VAR} reference; missing and empty values are
// refused alike and the value never appears in the error.
func resolveProviderAPIKey(resourceID, reference string) (string, error) {
	varName, ok := core.EnvPlaceholderName(reference)
	if !ok {
		return "", fmt.Errorf("模型连接 %s 的 api_key 不是精确 ${VAR} 占位", resourceID)
	}
	value, ok := os.LookupEnv(varName)
	if !ok || value == "" {
		return "", fmt.Errorf("模型连接 %s 引用的环境变量 %s 未设置或为空", resourceID, varName)
	}
	return value, nil
}

// providerCatalogStore 是运行期生效目录的原子持有者（约束 16）：发布 / 撤回 / 删除在
// ResourceGate 写锁内 `Store` 一次，新 Run 准入与模型调用都从 `Load` 读——同一次替换对
// 所有读者原子可见，读到"一半新公司一半旧表"不可能发生。`rebuild` 只在构建成功时替换，
// 构建失败**保留旧表**（旧客户端继续服务，失败由调用方记录）。
// providerCatalogStore holds the runtime catalog atomically. rebuild replaces only on
// success, so a failed build leaves the old table serving.
type providerCatalogStore struct {
	current atomic.Pointer[providerCatalog]
}

// newProviderCatalogStore 用首次构建的目录初始化持有者；nil 目录不合法（读者会拿到空指针，
// 装配期就暴露，而不是在第一次请求时崩）。
// newProviderCatalogStore initialises the holder with the first catalog; nil is refused.
func newProviderCatalogStore(catalog *providerCatalog) (*providerCatalogStore, error) {
	if catalog == nil {
		return nil, fmt.Errorf("模型连接目录不能为空")
	}
	holder := &providerCatalogStore{}
	holder.current.Store(catalog)
	return holder, nil
}

// Load 返回当前生效的目录快照（调用方只读，不得修改返回的映射表）。
// Load returns the current catalog snapshot; callers treat it as read-only.
func (s *providerCatalogStore) Load() *providerCatalog {
	return s.current.Load()
}

// Store 原子替换当前目录（发布 / 撤回 / 删除的提交段唯一入口）。
// Store atomically publishes a new catalog built outside the write lock.
func (s *providerCatalogStore) Store(catalog *providerCatalog) {
	s.current.Store(catalog)
}

// rebuild 按已发布版本重建目录，成功才替换：构建失败返回错误并**保留旧表**——这是
// 「构建失败保留旧表」的结构判据（`TestProviderCatalog_BuildFailureKeepsOld`）。
// rebuild replaces the catalog only when the build succeeds.
func (s *providerCatalogStore) rebuild(ctx context.Context, store *storage.ResourceStore,
	override core.ProviderClient, logger *slog.Logger, credentials managedCredentialSource,
) error {
	catalog, err := newProviderCatalogFromPublished(ctx, store, override, logger, credentials)
	if err != nil {
		return err
	}
	s.current.Store(catalog)
	return nil
}

// newProviderService 装配路由 + 审计服务：先建映射表，再把它与模型表、仓储、日志器一并
// 显式传给构造函数（依赖全部走类型化参数——业务状态不进 context，宪法 VII）。
// newProviderService wires routing and audit: the map is built first and then passed
// explicitly along with the model table, the store and the logger.
//
// 本 US 无生产调用方（占位点见颗粒度文档 §3.5）：第一个消费者是 US-4 的 serve / chat 装配；
// 本 US 经集成用例走同一条装配路径，「交付即可用」而不是「到 US-4 才第一次被编译进来」。
// US-4 is the first production consumer; the integration case exercises this very
// path today so the deliverable is usable on arrival.
func newProviderService(cfg *cli.Config, store core.LLMCallStore,
	logger *slog.Logger,
) (*provider.ProviderService, error) {
	clients, models, err := newProviderClients(cfg)
	if err != nil {
		return nil, err
	}
	return provider.NewProviderService(clients, models, store, logger), nil
}

// modelClient 返回运行入口消费的模型客户端：路由查表与两阶段审计一步不少（宪法 III + V）。
// 映射表由调用方**一次构建**后传入，运行准入与模型调用因此共享同一份目录——"准入查到的
// 表"与"实际调用的表"不可能分叉。**生产装配不直接用本函数建目录快照**：发布要能在运行期
// 换表，运行入口消费的是下面的 `catalogProviderRouter`（按当前持有者路由）；本函数保留给
// 冻结快照的调用方与用例。
// modelClient wraps one frozen catalog snapshot; production routing goes through the
// live router below so a publish can swap the table at runtime.
func modelClient(catalog *providerCatalog, store core.LLMCallStore, logger *slog.Logger) *provider.ProviderService {
	return provider.NewProviderService(catalog.clients, catalog.models, store, logger)
}

// catalogProviderNames 返回目录里的 Provider 名集（升序）。遍历映射表**只用于枚举**，
// 不用于挑选默认值——名字集喂给装载器做成员判定，顺序固定才可复现（宪法 III 禁止的是
// "从表里挑一个"，不是列出全部键）。
// catalogProviderNames lists the catalog's names in ascending order; enumeration here
// never picks a default, it only feeds the loader's membership check.
func catalogProviderNames(catalog *providerCatalog) []string {
	if catalog == nil {
		return nil
	}
	names := make([]string, 0, len(catalog.clients))
	for name := range catalog.clients {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// catalogProviderRouter 是运行入口消费的模型客户端（约束 16 / 17 / 宪法 III + V）：
// 每次 `Pinned` / `Chat` 都从 `providerCatalogStore` 读**当前**快照，发布在 ResourceGate
// 写锁内替换快照后，下一个新 Run 立即路由到新客户端；在途 Run 拿到的是准入时固定下来的
// `Pinned` 视图，手上的表不随发布变化（写时复制 + 准入固定）。
//
// 路由仍然只查表：查不到即拒绝发出，不遍历、不推断、不取"唯一那个"当默认。两阶段审计由
// 交出的 `*provider.ProviderService` 承担，`Chat` 只是"按当前表选中一行"再交给它。
// The live router resolves the current snapshot on every call; admitted runs keep the
// pinned view they started with, so a publish changes new admissions only.
type catalogProviderRouter struct {
	catalog *providerCatalogStore
	store   core.LLMCallStore
	logger  *slog.Logger
}

// 编译期断言：运行入口与准入端口看到的是同一个模型客户端面。
// Compile-time assertion that the router is a model client in its own right.
var _ core.ProviderClient = (*catalogProviderRouter)(nil)

// newCatalogProviderRouter 装配运行期路由；日志器缺省与 `provider.NewProviderService`
// 同口径（nil 用 slog.Default，不另造一份）。
// newCatalogProviderRouter wires the live router over the catalog holder.
func newCatalogProviderRouter(catalog *providerCatalogStore, store core.LLMCallStore,
	logger *slog.Logger,
) *catalogProviderRouter {
	if logger == nil {
		logger = slog.Default()
	}
	return &catalogProviderRouter{catalog: catalog, store: store, logger: logger}
}

// Pinned 返回当前快照里某个 Provider 的**审计固定视图**：目录条目取版本条目自身的名字，
// 缺条目 → false（准入失败关闭）。视图与 `ProviderService.Pinned` 同形：只含这一条路由、
// 共用同一个审计仓储——运行内的模型调用因此既受准入固定表约束，又不绕过两阶段审计。
// Pinned returns the audited one-entry view of the current snapshot.
func (r *catalogProviderRouter) Pinned(name string) (*provider.ProviderService, bool) {
	snapshot := r.catalog.Load()
	if snapshot == nil {
		return nil, false
	}
	client, ok := snapshot.clients[name]
	if !ok {
		return nil, false
	}
	models := make(map[string]string, 1)
	if model, ok := snapshot.models[name]; ok {
		models[name] = model
	}
	return provider.NewProviderService(
		map[string]core.ProviderClient{name: client}, models, r.store, r.logger), true
}

// Chat 把一次模型调用路由到**调用发生那一刻**的目录快照。目录里没有该名字时的行为与
// `ProviderService.Chat` 的第 1 道拒绝逐字一致（同一错误包装、同一 WARN 键），因为两者
// 表达的是同一条宪法 III 判据：不在显式映射表内就拒绝发出。
// Chat routes against the snapshot current at call time; a missing name is refused
// exactly like ProviderService.Chat's first rejection.
func (r *catalogProviderRouter) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	pinned, ok := r.Pinned(req.Provider)
	if !ok {
		err := fmt.Errorf("%w: %s", core.ErrProviderNotConfigured, req.Provider)
		r.logger.Warn("provider 不在显式映射表内——拒绝发出", "provider", req.Provider,
			"run_id", req.RunID, "error", err)
		return core.ChatResponse{}, err
	}
	return pinned.Chat(ctx, req)
}
