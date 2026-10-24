package app

// 本文件是通知渠道的**管理读面**适配器（`web.Options.Notify` 的装配实现）。
//
// 发送路径的只读仓储（`storage.NotifyChannelRepo`）只关心"这一发要发给谁"，行里的
// `credential_mode` / `scope` / `revision` 已经够用；管理台还要回答四件它答不了的事：
// 属主 Agent 的名字、凭据身份状态与尾号、地址可用性（由 web 按模式判定）、以及
// 条件写要用的 ETag。这四项需要凭据仓储与生命周期索引，只有装配层（`internal/app`）
// 同时够得着，所以读面在此**组合**——不把凭据查询下沉进 web，也不让 web 自己算 ETag。
//
// 纪律：
//   - 只有**一份**渠道 ETag 配方（本文件的 `notifyChannelETag`），管理台列表、渠道视图、
//     `/resources/notify_channel/{name}/scope` 与 `/credential/revoke` 全部复用它；
//   - 原始地址只进哈希输入，**没有任何**出站载体；web 只投影 `url_host`；
//   - 凭据读失败**必须**上报——把"读不到"说成"没有凭据"会让页面显示 env 模式。
//
// This file is the admin read face of notification channels. The send-path repository
// answers "who receives this delivery"; the console additionally needs the owner name,
// the credential identity state, and the one conditional ETag used by every conditional
// write. Only the assembly layer can reach the credential store and the lifecycle index,
// so the composition lives here rather than in web.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// notifyChannelETagVersion 是渠道条件 ETag 规整串的版本前缀：字段集变化必须换版本
// （与 `resourceETagVersion` 同一纪律，两套字段集各自演进，**不得**混用对方的前缀）。
// notifyChannelETagVersion prefixes the channel canonical string; the field set is
// distinct from the resource ETag and must never reuse its prefix.
const notifyChannelETagVersion = "basalt/notify-channel-etag/v1"

// notifyChannelReadPort 在只读渠道仓储之上补齐管理读面的四项事实。
// `channels` 与发送路径是**同一个**读面实现（写后下一发即可见，零缓存）。
// notifyChannelReadPort enriches the shared read repository with the admin-only facts.
type notifyChannelReadPort struct {
	channels    core.NotifyChannelReader
	credentials *storage.CredentialStore
	agents      *storage.ResourceStore
	tests       core.ResourceConnectionTestStore
	lookupEnv   func(string) (string, bool)
}

// notifyReadDeps 是管理读面回答「本地就绪 / 上次实测」所需的两个可选来源。
// `ConnectionTests` 为 nil 表示本次装配不保留投影（历史仍在管理操作台账）；
// `LookupEnv` 为 nil 时回落 `os.LookupEnv`——与发送路径同一真值来源。
// notifyReadDeps supplies the optional readiness sources; a nil lookup falls back to
// os.LookupEnv, the same source the send path resolves.
type notifyReadDeps struct {
	ConnectionTests core.ResourceConnectionTestStore
	LookupEnv       func(string) (string, bool)
}

// 编译期断言：适配器仍满足 web 消费的只读面（接口在 core，实现可替换）。
var _ core.NotifyChannelReader = (*notifyChannelReadPort)(nil)

// newNotifyChannelReadPort 组装管理读面；`agents` 只用于属主名反查。
// newNotifyChannelReadPort wires the admin read face; agents is only used for name lookup.
func newNotifyChannelReadPort(channels core.NotifyChannelReader,
	credentials *storage.CredentialStore, agents *storage.ResourceStore, deps notifyReadDeps,
) *notifyChannelReadPort {
	lookupEnv := deps.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	return &notifyChannelReadPort{
		channels: channels, credentials: credentials, agents: agents,
		tests: deps.ConnectionTests, lookupEnv: lookupEnv,
	}
}

// ListNotifyChannels 逐行补齐属主名、凭据状态与条件 ETag；任何一行读失败整表失败——
// 半张表带着零值凭据状态出门比 500 更有害（页面会把 managed 渠道显示成 env）。
// ListNotifyChannels enriches every row; a single read failure fails the whole list
// rather than letting zero values masquerade as "no credential".
func (p *notifyChannelReadPort) ListNotifyChannels(ctx context.Context) ([]core.NotifyChannel, error) {
	channels, err := p.channels.ListNotifyChannels(ctx)
	if err != nil {
		return nil, err
	}
	tests, err := p.connectionTestIndex(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]core.NotifyChannel, 0, len(channels))
	for _, ch := range channels {
		enriched, err := p.enrich(ctx, ch, tests)
		if err != nil {
			return nil, err
		}
		out = append(out, enriched)
	}
	return out, nil
}

// connectionTestIndex 一次读齐全部渠道的最后测试结果（UID → 记录）；未装配投影仓储
// 时返回 nil，页面据 `last_test = null` 显示「从未测试」。
// connectionTestIndex loads every channel's last test at once; nil means no projection.
func (p *notifyChannelReadPort) connectionTestIndex(ctx context.Context) (map[core.ResourceUID]core.ResourceConnectionTest, error) {
	if p.tests == nil {
		return nil, nil
	}
	rows, err := p.tests.ListConnectionTests(ctx, core.ResourceNotifyChannel)
	if err != nil {
		return nil, fmt.Errorf("读取通知渠道的最后连接测试: %w", err)
	}
	index := make(map[core.ResourceUID]core.ResourceConnectionTest, len(rows))
	for _, row := range rows {
		index[row.ResourceUID] = row
	}
	return index, nil
}

// Channel 读单条渠道（写后回读与详情页用同一套补齐规则）。名称不存在 →
// `core.ErrResourceNotFound`；墓碑行不出现在读面。
// Channel reads one enriched row; a miss maps to ErrResourceNotFound.
func (p *notifyChannelReadPort) Channel(ctx context.Context, name string) (core.NotifyChannel, error) {
	channels, err := p.ListNotifyChannels(ctx)
	if err != nil {
		return core.NotifyChannel{}, err
	}
	for _, ch := range channels {
		if ch.Name == name {
			return ch, nil
		}
	}
	return core.NotifyChannel{}, fmt.Errorf("%w: 通知渠道 %s", core.ErrResourceNotFound, name)
}

// enrich 补齐一行：凭据身份状态 / 尾号、属主名、条件 ETag，以及本地就绪与最后测试。
// 凭据段缺失（env 模式）时 `credential_state` 保持空串——契约用空串表达"env 或尚未写入"，
// 不是"已撤销"。availability 与 last_test 是两件事：前者的输入是本机配置与环境变量
// 存在性，后者只来自一次真实测试的记录；nil LastTest 就是「从未测过」。
// enrich fills the credential state, owner name, conditional ETag and the two readiness
// facts; availability is local configuration, last_test is a real recorded test.
func (p *notifyChannelReadPort) enrich(ctx context.Context, ch core.NotifyChannel,
	tests map[core.ResourceUID]core.ResourceConnectionTest,
) (core.NotifyChannel, error) {
	digest, err := loadCredentialDigest(ctx, p.credentials, core.ResourceNotifyChannel, ch.ResourceUID)
	if err != nil {
		return core.NotifyChannel{}, fmt.Errorf("读取通知渠道 %s 的凭据状态: %w", ch.Name, err)
	}
	if digest != nil {
		ch.CredentialState = digest.IdentityState
		ch.CredentialVersion = digest.Version
		ch.Last4 = digest.Last4
	}
	owner, err := resolveOwnerAgentName(ctx, p.agents, ch.Scope, ch.OwnerAgentUID,
		"通知渠道 "+ch.Name)
	if err != nil {
		return core.NotifyChannel{}, err
	}
	ch.OwnerAgentName = owner
	ch.ETag = notifyChannelETag(ch, digest)
	ch.Availability = notifyChannelAvailability(ch, p.lookupEnv)
	if rec, ok := tests[ch.ResourceUID]; ok {
		rec := rec
		ch.LastTest = &rec
	}
	return ch, nil
}

// notifyChannelAvailability 只回答「本地配置看起来能不能用」：托管地址要求 active 的
// 凭据身份；env 地址要求精确 `${VAR}` 占位且环境变量非空；完全没有地址配置才是
// unconfigured。它**不**预判主密钥可读性、**不**代表实测可用——那是 last_test 的事。
// notifyChannelAvailability answers local readiness only; live availability is last_test.
func notifyChannelAvailability(ch core.NotifyChannel, lookupEnv func(string) (string, bool)) string {
	if ch.CredentialMode == core.CredentialModeManaged {
		if ch.CredentialState == core.CredentialIdentityActive {
			return core.AvailabilityReady
		}
		return core.AvailabilityUnavailable
	}
	if strings.TrimSpace(ch.URL) == "" {
		return core.AvailabilityUnconfigured
	}
	name, ok := core.EnvPlaceholderName(ch.URL)
	if !ok {
		return core.AvailabilityUnavailable
	}
	if value, found := lookupEnv(name); !found || strings.TrimSpace(value) == "" {
		return core.AvailabilityUnavailable
	}
	return core.AvailabilityReady
}

// notifyChannelETag 是渠道条件 ETag 的**唯一**配方（FR-081 的渠道版）：定义（类型 /
// 地址引用 / 说明）+ 身份 + 修订号 + 范围 + 属主 + 凭据状态。只改类型或说明不递增
// `revision`（`notify_channels` 的写面如此），所以定义字段**必须**进哈希，否则两个人
// 同时改说明时后写会覆盖先写而 ETag 不变。
// `url` 只进哈希输入：哈希是十六进制摘要，原值不出站。
// notifyChannelETag is the one channel conditional ETag recipe; the definition fields
// must be hashed because a type/description edit does not bump the revision.
func notifyChannelETag(ch core.NotifyChannel, digest *credentialDigest) string {
	var b etagBuilder
	b.write(notifyChannelETagVersion)
	b.field("type", ch.Type)
	b.field("url", ch.URL)
	b.field("description", ch.Description)
	b.field("resource_uid", string(ch.ResourceUID))
	b.field("revision", strconv.Itoa(ch.Revision))
	b.field("scope", string(normalizedScope(ch.Scope)))
	b.field("owner_agent_uid", string(ch.OwnerAgentUID))
	b.credential(digest)
	return b.sum()
}

// normalizedScope 把空范围读成 shared（列默认值与 v3 迁移回填口径），避免同一行
// 出现两种"共享"写法让 ETag 不可比。
// normalizedScope reads an empty scope as shared so the same row never hashes twice.
func normalizedScope(scope core.ResourceScope) core.ResourceScope {
	if scope == "" {
		return core.ScopeShared
	}
	return scope
}
