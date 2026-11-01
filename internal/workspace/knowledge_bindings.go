package workspace

// 本文件是 AGENT.md frontmatter 中 `knowledge:` 块的解析面（设计 D-19 / A-6、契约
// agent-md §2.7）：声明即绑定——元素是 `{name, access}`，`access` 缺省 `read`、闭集
// 只认 `read`。解析只做**结构**校验（列表、只允许两个已知键、名字非空且不重复）；
// 「名字在库里有没有活动实体」属于注册闸门与检索授权（需要查库），不在本文件判。
// This file parses the `knowledge:` block. Parsing checks structure only; whether a name
// has a live collection is judged by the registration gate and the retrieval authz,
// both of which need a database read.

import (
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"

	"github.com/ymedword/basalt/internal/core"
)

// knowledgeKey 是 frontmatter 中的顶层键字面量（契约 agent-md §2.7）。
// knowledgeKey is the top-level frontmatter key of the knowledge declaration.
const knowledgeKey = "knowledge"

// knowledgeBindingsDTO 是 `knowledge:` 块在 YAML 解码时的私有形态：元素只允许
// `name` / `access` 两个已知键，其余由 DisallowUnknownField 拒绝。
// knowledgeBindingsDTO is the private decode shape; unknown element keys are refused.
type knowledgeBindingsDTO []knowledgeBindingDTO

// knowledgeBindingDTO 是一条 `{name, access}` 声明；`access` 保留原文，取值合法性
// 交给 `core.ParseKnowledgeAccess`（闭集判定只有一份）。
// knowledgeBindingDTO keeps the raw access literal; the closed-set verdict lives in core.
type knowledgeBindingDTO struct {
	Name   string `yaml:"name"`
	Access string `yaml:"access"`
}

// ParseKnowledgeBindings 从 AGENT.md 原始字节解析 `knowledge:`：块缺失或值为空时
// 返回空集合（不声明 = 不检索任何集合）；结构非法（不是列表、元素键未知、缺名、
// 重名、权限不在闭集内）一律包装 `core.ErrAgentKnowledgeBindingInvalid` 拒绝——
// 加载期据此记 WARN `invalid knowledge` 并整只不注册。
// ParseKnowledgeBindings returns an empty slice when the block is absent and refuses any
// structurally invalid block with ErrAgentKnowledgeBindingInvalid.
func ParseKnowledgeBindings(raw []byte) ([]core.KnowledgeBinding, error) {
	front, _, err := splitFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", core.ErrAgentKnowledgeBindingInvalid, err)
	}
	return parseKnowledgeFront(front)
}

// parseKnowledgeFront 在**已切出**的 frontmatter 字节上解析声明块：装载路径
// （parseFrontmatter）与独立解析共用同一段结构校验，不把文件读两遍。
// parseKnowledgeFront parses the block from already-split frontmatter bytes so the
// loader and the standalone parser share one structural check.
func parseKnowledgeFront(front []byte) ([]core.KnowledgeBinding, error) {
	value, found, err := findFrontmatterValue(front, knowledgeKey)
	if err != nil {
		return nil, err
	}
	if !found || isEmptyYAMLValue(value) {
		return nil, nil
	}

	var dto knowledgeBindingsDTO
	if err := yaml.NodeToValue(value, &dto, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("%w: %s 结构非法: %v", core.ErrAgentKnowledgeBindingInvalid, knowledgeKey, err)
	}
	out := make([]core.KnowledgeBinding, 0, len(dto))
	for _, entry := range dto {
		access, err := core.ParseKnowledgeAccess(entry.Access)
		if err != nil {
			return nil, err
		}
		out = append(out, core.KnowledgeBinding{Name: strings.TrimSpace(entry.Name), Access: access})
	}
	if err := core.ValidateKnowledgeBindings(out); err != nil {
		return nil, err
	}
	return out, nil
}

// isEmptyYAMLValue 报告节点是否为「声明了但没内容」（空 / `null`）：两种写法都等于
// 未声明——有块无项不构成绑定，也不该被当成"声明了空集合"而拒载。
// isEmptyYAMLValue reports a null/empty node; either spelling means "no declaration".
func isEmptyYAMLValue(value ast.Node) bool {
	if value == nil {
		return true
	}
	_, isNull := value.(*ast.NullNode)
	return isNull
}
