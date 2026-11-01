package core

// 本文件声明**知识库集合绑定**（设计 D-19 / A-6）：`AGENT.md` frontmatter 的
// `knowledge:` 是「**声明即绑定**」——集合名 + 权限；数据库 `agent_resource_bindings`
// 只做投影，检索期还要与**调用方自身**的项目与 scope 复核。四者关系一句话：
//
//	声明（AGENT.md）→ 注册闸门（名字有没有实体）→ 投影（名字 → 稳定集合 ID）
//	→ 检索授权（声明 ∩ 调用方项目 ∩ 调用方 scope）
//
// **声明不能扩大调用方权限**：声明只缩小可见范围，任何"按声明放行"的写法都必须先
// 通过调用方自己的判定。
// This file declares the knowledge-collection binding. The AGENT.md `knowledge:` entry is
// the binding itself; PostgreSQL only projects it, and retrieval re-checks the caller's
// own project and scope, so a declaration can only narrow what the caller already sees.

import (
	"fmt"
	"strings"
)

// KnowledgeAccess 是集合声明的权限字面量。当前只有只读一档：知识库**写入**只发生在
// 摄取端（调用方持有 `knowledge:write`），Agent 运行期不得借声明获得写权限。
// KnowledgeAccess has exactly one value today; agent declarations can never grant write.
type KnowledgeAccess string

const (
	// KnowledgeAccessRead 是唯一的集合权限：检索与来源读取。
	KnowledgeAccessRead KnowledgeAccess = "read"
)

// ParseKnowledgeAccess 解析声明的权限：空串（未声明）= `read`，其余取值一律拒绝。
// 未知权限若被静默当成 `read`，「写权限被降级成读」与「读权限被扩成写」两种事故都
// 无从发现——所以这里是闭集判定，不是宽容解析。
// ParseKnowledgeAccess accepts the empty string (defaulted to read) and refuses anything
// else, so an unknown access can never be silently downgraded or widened.
func ParseKnowledgeAccess(raw string) (KnowledgeAccess, error) {
	switch trimmed := strings.TrimSpace(raw); {
	case trimmed == "":
		return KnowledgeAccessRead, nil
	case KnowledgeAccess(trimmed) == KnowledgeAccessRead:
		return KnowledgeAccessRead, nil
	default:
		return "", fmt.Errorf("%w: 未知的集合权限 %q（只接受 read）", ErrAgentKnowledgeBindingInvalid, raw)
	}
}

// KnowledgeBinding 是 `knowledge:` 的一项：集合名 + 权限。名字是**声明标签**，
// 稳定身份是检索期解析出的 `collection_id`（`kb_collections.collection_id`）。
// KnowledgeBinding is one declaration entry; the name is a label and the stable identity
// is the collection id resolved at retrieval time.
type KnowledgeBinding struct {
	Name   string          `json:"name"`
	Access KnowledgeAccess `json:"access"`
}

// KnowledgeCollectionBinding 是**解析后**的集合绑定：声明名 → 稳定集合 ID。投影行由它
// 产生；它不参与声明解析（声明里写不出 ID）。
// KnowledgeCollectionBinding pairs a declared name with the resolved collection id; the
// projection rows are produced from it, and a declaration can never carry an id directly.
type KnowledgeCollectionBinding struct {
	Name         string
	CollectionID string
}

// ValidateKnowledgeBindings 校验声明的结构：名字非空、权限合法、名字不重复。
// 名字是否**有实体**不在这里判——那是注册闸门与检索授权的职责（库查询）。
// ValidateKnowledgeBindings checks structure only; whether a name has an entity is judged
// by the registration gate and the retrieval authz, both of which need a database read.
func ValidateKnowledgeBindings(bindings []KnowledgeBinding) error {
	names := make(map[string]struct{}, len(bindings))
	for _, entry := range bindings {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			return fmt.Errorf("%w: knowledge 绑定缺少集合名", ErrAgentKnowledgeBindingInvalid)
		}
		if _, err := ParseKnowledgeAccess(string(entry.Access)); err != nil {
			return fmt.Errorf("%w: knowledge 绑定 %q 的权限非法", ErrAgentKnowledgeBindingInvalid, name)
		}
		if _, ok := names[name]; ok {
			return fmt.Errorf("%w: knowledge 绑定重复集合名 %q", ErrAgentKnowledgeBindingInvalid, name)
		}
		names[name] = struct{}{}
	}
	return nil
}

// KnowledgeBindingNames 返回声明里的集合名（去空白、按声明序）：注册闸门与检索授权
// 都按这份名字集查询，两处因此不可能对"声明了什么"产生不同理解。
// KnowledgeBindingNames returns the declared names in declaration order, so the gate and
// the authz always query the very same set.
func KnowledgeBindingNames(bindings []KnowledgeBinding) []string {
	names := make([]string, 0, len(bindings))
	for _, entry := range bindings {
		if name := strings.TrimSpace(entry.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// ValidateKnowledgeCollectionBindings 校验**解析后**的绑定：名字非空且不重复、集合 ID
// 非空。投影行由它产出的数据写入，因此在写库前拒绝残缺行——一条没有 ID 的绑定既不能
// 参与检索，又会在投影里冒充"已绑定"。
// ValidateKnowledgeCollectionBindings checks the resolved bindings before they become
// projection rows; an empty id would masquerade as a bound collection.
func ValidateKnowledgeCollectionBindings(bindings []KnowledgeCollectionBinding) error {
	names := make(map[string]struct{}, len(bindings))
	for _, entry := range bindings {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			return fmt.Errorf("%w: 知识集合绑定缺少名称", ErrAgentKnowledgeBindingInvalid)
		}
		if strings.TrimSpace(entry.CollectionID) == "" {
			return fmt.Errorf("%w: 知识集合 %q 缺少稳定集合 ID", ErrAgentKnowledgeBindingInvalid, name)
		}
		if _, ok := names[name]; ok {
			return fmt.Errorf("%w: 知识集合绑定重复名称 %q", ErrAgentKnowledgeBindingInvalid, name)
		}
		names[name] = struct{}{}
	}
	return nil
}
