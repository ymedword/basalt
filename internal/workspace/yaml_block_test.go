package workspace

// 本文件是定义文件块编辑的验收测试（颗粒度文档 §3.2.1：workspace/yaml_block_test.go，
// 计划 14 例；实际 16 例——在计划之外补「待写入条目形状」与「多文档」两组防御用例，
// 均属新增测试，不改动文档既有验收语义）。
// 全部用例只走字节面：输入是内存里的定义文件文本，断言块外字节逐字保留、
// 不可安全保留的构造一律 ErrEditUnsupported，不触碰磁盘。
// This file covers the top-level block editor (16 cases; the card plans 14 and the
// two extras are defensive additions). Everything is byte-level: the tests assert
// that bytes outside the edited span survive verbatim.

import (
	"io/fs"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// providerEntry 渲染一个 providers 列表项（缩进与既有块一致）。
func providerEntry(name, baseURL string) []byte {
	return []byte("  - name: " + name + "\n    base_url: " + baseURL + "\n")
}

// TestYAMLBlock_ReplacePreservesCommentsAndSurroundings：颗粒度文档 §4.3.1 的强制回归——
// 替换 `main` 条目后，块外注释（顶部 / 条目之前 / 底部）与相邻条目原样保留，
// 旧值消失；条目内注释未丢时 commentsLost 为 false。
// The mandated §4.3.1 regression: replacing one entry keeps every surrounding comment.
func TestYAMLBlock_ReplacePreservesCommentsAndSurroundings(t *testing.T) {
	doc := []byte(`# 顶部注释
providers:
  # 主连接
  - name: main
    base_url: https://old.example
    api_key: ${OLD_KEY}
  - name: backup
    base_url: https://backup.example
    api_key: ${BACKUP_KEY}
# 底部注释
`)
	rendered := []byte(`  - name: main
    base_url: https://api.deepseek.com
    api_key: ${DEEPSEEK_API_KEY}
`)

	out, commentsLost, err := ReplaceEntry(doc, "providers", "main", rendered)
	require.NoError(t, err)
	require.False(t, commentsLost, "条目内无注释，不得报告注释丢失")

	for _, want := range []string{"# 顶部注释", "  # 主连接", "# 底部注释", "name: backup", "https://backup.example"} {
		require.Contains(t, string(out), want)
	}
	require.NotContains(t, string(out), "https://old.example", "旧条目内容必须整体消失")

	require.Equal(t, `# 顶部注释
providers:
  # 主连接
  - name: main
    base_url: https://api.deepseek.com
    api_key: ${DEEPSEEK_API_KEY}
  - name: backup
    base_url: https://backup.example
    api_key: ${BACKUP_KEY}
# 底部注释
`, string(out), "块外字节必须逐字保留")
}

// TestYAMLBlock_FindBlockLocatesBlock：键行与块尾定位；键缺失返回 (-1,-1)；
// 空块（`key:` 无数值）与空表（`{}`）不当作错误。
func TestYAMLBlock_FindBlockLocatesBlock(t *testing.T) {
	t.Run("existing block", func(t *testing.T) {
		doc := []byte("# head\nproviders:\n  - name: a\n  - name: b\n\nbudget:\n  max_iterations: 10\n")
		start, end, err := FindBlock(doc, "providers")
		require.NoError(t, err)
		require.Equal(t, 1, start)
		require.Equal(t, 4, end, "块尾不含与下一段之间的空行")
	})

	t.Run("absent key", func(t *testing.T) {
		start, end, err := FindBlock([]byte("budget:\n  max_iterations: 10\n"), "providers")
		require.NoError(t, err)
		require.Equal(t, -1, start)
		require.Equal(t, -1, end)
	})

	t.Run("empty document", func(t *testing.T) {
		start, end, err := FindBlock(nil, "providers")
		require.NoError(t, err)
		require.Equal(t, -1, start)
		require.Equal(t, -1, end)
	})

	t.Run("null block", func(t *testing.T) {
		start, end, err := FindBlock([]byte("providers:\n"), "providers")
		require.NoError(t, err)
		require.Equal(t, 0, start)
		require.Equal(t, 1, end)
	})

	t.Run("empty flow mapping document", func(t *testing.T) {
		start, end, err := FindBlock([]byte("{}\n"), "providers")
		require.NoError(t, err)
		require.Equal(t, -1, start)
		require.Equal(t, -1, end)
	})
}

// TestYAMLBlock_FindBlockAfterConsecutiveComments：中文验收点——键前有**两行以上**
// 连续注释、中文注释或 CRLF 行尾时，块定位仍必须落在键行。这是评测夹具配置
// （两行中文注释 + `providers:`）把启动对账打红的那个形状：goccy 的
// `Position.Offset` 在连续注释行之后少算偏移（两行少 1、三行少 2），
// 旧实现按偏移换行号，于是键行被定位到上一行，`scanEntries` 首项判定失败。
// The block must still resolve when the key follows two or more comment lines,
// CJK comments or CRLF endings — goccy's Offset under-counts after comment runs.
func TestYAMLBlock_FindBlockAfterConsecutiveComments(t *testing.T) {
	cases := map[string]struct {
		doc  string
		want int
	}{
		"two ascii comments": {"# a\n# b\nproviders:\n  - name: probe\n", 2},
		"three ascii comments": {
			"# a\n# b\n# c\nproviders:\n  - name: probe\n", 3,
		},
		"three cjk comments": {"# 一\n# 二\n# 三\nproviders:\n  - name: probe\n", 3},
		"two cjk comments before key": {
			"# 评测夹具配置：探针条目只为让路由查表与审计链走完整路径。\n" +
				"# 凭证写占位形态，值由进程内自设的哨兵提供——明文不落盘。\n" +
				"providers:\n  - name: probe\n", 2,
		},
		"crlf two comments":  {"# a\r\n# b\r\nproviders:\r\n  - name: probe\r\n", 2},
		"crlf mixed endings": {"# a\r\n# b\nproviders:\r\n  - name: probe\r\n", 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			start, _, err := FindBlock([]byte(tc.doc), "providers")
			require.NoError(t, err)
			require.Equal(t, tc.want, start, "块定位必须落在键行")
		})
	}

	t.Run("replace keeps every outside byte", func(t *testing.T) {
		doc := "# 顶部一\n# 顶部二\nproviders:\n  - name: main\n    base_url: https://old\n" +
			"budget:\n  max_iterations: 10\n"
		rendered := []byte("  - name: main\n    base_url: https://new\n")
		out, commentsLost, err := ReplaceEntry([]byte(doc), "providers", "main", rendered)
		require.NoError(t, err)
		require.False(t, commentsLost)
		require.Equal(t, "# 顶部一\n# 顶部二\nproviders:\n  - name: main\n    base_url: https://new\n"+
			"budget:\n  max_iterations: 10\n", string(out))
	})
}

// TestYAMLBlock_BlockEntriesScansEntries：条目名、行区间与 Raw 原文；条目间空行与
// 前导注释不进入条目区间；嵌套的 `- ` 行不得被误判为条目边界。
func TestYAMLBlock_BlockEntriesScansEntries(t *testing.T) {
	doc := []byte(`servers:
  # 条目一
  - name: files
    command: npx
    args:
      - -y
      - server-files

  - name: search
    command: uvx
# 段尾注释
`)
	entries, err := BlockEntries(doc, "servers")
	require.NoError(t, err)
	require.Len(t, entries, 2)

	require.Equal(t, "files", entries[0].Name)
	require.Equal(t, 2, entries[0].Start)
	require.Equal(t, 7, entries[0].End, "条目间空行属于分隔，不进入前一个条目")
	require.Equal(t, "  - name: files\n    command: npx\n    args:\n      - -y\n      - server-files\n", string(entries[0].Raw))
	require.NotContains(t, string(entries[0].Raw), "条目一", "条目之前的前导注释不进入条目区间")

	require.Equal(t, "search", entries[1].Name)
	require.Equal(t, 8, entries[1].Start)
	require.Equal(t, 10, entries[1].End, "段尾注释不进入条目区间")
	require.Equal(t, "  - name: search\n    command: uvx\n", string(entries[1].Raw))

	t.Run("missing key yields no entries", func(t *testing.T) {
		entries, err := BlockEntries([]byte("budget:\n  x: 1\n"), "servers")
		require.NoError(t, err)
		require.Empty(t, entries)
	})
}

// TestYAMLBlock_ReplaceEntryKeepsOutsideBytes：CRLF 文件替换单个条目时，只改目标
// 条目字节；新行沿用文件的行终止符，其余行（含尾注释）逐字不变。
func TestYAMLBlock_ReplaceEntryKeepsOutsideBytes(t *testing.T) {
	doc := []byte("# head\r\nproviders:\r\n  - name: main\r\n    base_url: https://old.example\r\n  - name: backup\r\n    base_url: https://b.example\r\n# tail\r\n")
	rendered := []byte("  - name: main\n    base_url: https://new.example\n")

	out, commentsLost, err := ReplaceEntry(doc, "providers", "main", rendered)
	require.NoError(t, err)
	require.False(t, commentsLost)
	require.Equal(t, "# head\r\nproviders:\r\n  - name: main\r\n    base_url: https://new.example\r\n  - name: backup\r\n    base_url: https://b.example\r\n# tail\r\n", string(out))

	t.Run("rendered indent is shifted to the block indent", func(t *testing.T) {
		out, _, err := ReplaceEntry(doc, "providers", "main", []byte("    - name: main\n      base_url: https://new.example\n"))
		require.NoError(t, err)
		require.Contains(t, string(out), "  - name: main\r\n    base_url: https://new.example\r\n")
	})
}

// TestYAMLBlock_ReplaceEntryReportsCommentsLost：条目区间内含注释（行内或整行）时，
// 整条替换无法保留注释——返回已生成的候选字节并报告 commentsLost=true，
// 由调用方决定拒绝写入还是记录 comments_lost。
func TestYAMLBlock_ReplaceEntryReportsCommentsLost(t *testing.T) {
	t.Run("inline comment", func(t *testing.T) {
		doc := []byte("providers:\n  - name: main\n    base_url: https://old.example # 旧地址\n  - name: backup\n    base_url: https://b.example\n")
		out, commentsLost, err := ReplaceEntry(doc, "providers", "main", providerEntry("main", "https://new.example"))
		require.NoError(t, err)
		require.True(t, commentsLost, "条目内的行内注释无法保留，必须上报")
		require.NotContains(t, string(out), "旧地址")
		require.Contains(t, string(out), "https://new.example")
		require.Contains(t, string(out), "name: backup")
	})

	t.Run("full-line comment inside entry", func(t *testing.T) {
		doc := []byte("providers:\n  - name: main\n    # 旧说明\n    base_url: https://a.example\n")
		_, commentsLost, err := ReplaceEntry(doc, "providers", "main", providerEntry("main", "https://b.example"))
		require.NoError(t, err)
		require.True(t, commentsLost)
	})
}

// TestYAMLBlock_ReplaceEntryRejectsUnknownAndDuplicateNames：目标不存在 → fs.ErrNotExist
// （调用方可映射为资源不存在）；同名条目重复、待写入条目名与目标不一致 → 拒绝。
func TestYAMLBlock_ReplaceEntryRejectsUnknownAndDuplicateNames(t *testing.T) {
	doc := []byte("providers:\n  - name: main\n    base_url: https://a.example\n  - name: backup\n")

	out, _, err := ReplaceEntry(doc, "providers", "ghost", providerEntry("ghost", "https://c.example"))
	require.Error(t, err)
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.Nil(t, out)

	_, _, err = ReplaceEntry(doc, "mcp", "main", providerEntry("main", "https://c.example"))
	require.ErrorIs(t, err, fs.ErrNotExist, "顶层键缺失同样按目标不存在处理")

	dup := []byte("providers:\n  - name: main\n  - name: main\n")
	_, _, err = ReplaceEntry(dup, "providers", "main", providerEntry("main", "https://c.example"))
	require.ErrorIs(t, err, ErrEditUnsupported, "同名条目无法确定编辑目标")

	_, _, err = ReplaceEntry(doc, "providers", "main", providerEntry("backup", "https://c.example"))
	require.ErrorIs(t, err, ErrEditUnsupported, "待写入条目名必须等于目标条目名")
}

// TestYAMLBlock_ReplaceEntryRejectsBadRenderedEntry：待写入条目必须是单条块式列表项、
// 含字符串 name、无锚点；流式 / 多条 / 缺 name / 重复键 / 多文档一律拒绝。
func TestYAMLBlock_ReplaceEntryRejectsBadRenderedEntry(t *testing.T) {
	doc := []byte("providers:\n  - name: main\n    base_url: https://a.example\n")

	cases := []struct {
		reason   string
		rendered string
	}{
		{"flow item", "[{name: main}]"},
		{"two items", "- name: main\n- name: other\n"},
		{"missing name", "- base_url: https://a.example\n"},
		{"not a list", "name: main\n"},
		{"anchor", "- &a\n  name: main\n"},
		{"duplicate key inside item", "- name: main\n  name: second\n"},
		{"document separator", "- name: main\n---\n"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			out, _, err := ReplaceEntry(doc, "providers", "main", []byte(tc.rendered))
			require.Error(t, err)
			require.ErrorIs(t, err, ErrEditUnsupported)
			require.Nil(t, out)
		})
	}
}

// TestYAMLBlock_AppendEntryAfterLastItem：新条目紧跟最后一个条目（之后的空行与
// 下一段保持原样）；同名追加拒绝；待写入缩进被平移到既有块缩进。
func TestYAMLBlock_AppendEntryAfterLastItem(t *testing.T) {
	doc := []byte("providers:\n  - name: main\n    base_url: https://a.example\n\nbudget:\n  max_iterations: 10\n")

	out, err := AppendEntry(doc, "providers", providerEntry("backup", "https://b.example"))
	require.NoError(t, err)
	require.Equal(t, "providers:\n  - name: main\n    base_url: https://a.example\n  - name: backup\n    base_url: https://b.example\n\nbudget:\n  max_iterations: 10\n", string(out))

	t.Run("duplicate name is refused", func(t *testing.T) {
		out, err := AppendEntry(doc, "providers", providerEntry("main", "https://c.example"))
		require.ErrorIs(t, err, ErrEditUnsupported)
		require.Nil(t, out)
	})

	t.Run("rendered indent follows the block", func(t *testing.T) {
		out, err := AppendEntry(doc, "providers", []byte("    - name: backup\n      base_url: https://b.example\n"))
		require.NoError(t, err)
		require.Contains(t, string(out), "  - name: backup\n    base_url: https://b.example\n")
	})
}

// TestYAMLBlock_AppendEntryCreatesBlock：键缺失时的创建路径——空文档、纯注释、
// 末尾无换行、空表 `{}`（basalt init 的 mcp_servers.yaml 模板）与空块 `key:`。
func TestYAMLBlock_AppendEntryCreatesBlock(t *testing.T) {
	entry := providerEntry("main", "https://a.example")

	t.Run("empty document", func(t *testing.T) {
		out, err := AppendEntry(nil, "providers", entry)
		require.NoError(t, err)
		require.Equal(t, "providers:\n  - name: main\n    base_url: https://a.example\n", string(out))
	})

	t.Run("comment-only document", func(t *testing.T) {
		out, err := AppendEntry([]byte("# 仅注释\n"), "providers", entry)
		require.NoError(t, err)
		require.Equal(t, "# 仅注释\nproviders:\n  - name: main\n    base_url: https://a.example\n", string(out))
	})

	t.Run("missing trailing newline", func(t *testing.T) {
		out, err := AppendEntry([]byte("budget:\n  max_iterations: 10"), "providers", entry)
		require.NoError(t, err)
		require.Equal(t, "budget:\n  max_iterations: 10\nproviders:\n  - name: main\n    base_url: https://a.example\n", string(out))
	})

	t.Run("empty flow mapping template", func(t *testing.T) {
		out, err := AppendEntry([]byte("# MCP 服务器\n{}\n"), "servers", []byte("  - name: files\n    command: npx\n"))
		require.NoError(t, err)
		require.Equal(t, "# MCP 服务器\nservers:\n  - name: files\n    command: npx\n", string(out))
	})

	t.Run("null block", func(t *testing.T) {
		out, err := AppendEntry([]byte("servers:\n"), "servers", []byte("  - name: files\n    command: npx\n"))
		require.NoError(t, err)
		require.Equal(t, "servers:\n  - name: files\n    command: npx\n", string(out))
	})
}

// TestYAMLBlock_RemoveEntryKeepsSiblings：删除中间条目只移除该条目区间，相邻条目与
// 空行分隔原样保留；删光条目后顶层键仍在、可继续追加。
func TestYAMLBlock_RemoveEntryKeepsSiblings(t *testing.T) {
	doc := []byte("providers:\n  - name: main\n    base_url: https://a.example\n\n  - name: backup\n    base_url: https://b.example\n\nbudget:\n  x: 1\n")

	out, commentsLost, err := RemoveEntry(doc, "providers", "main")
	require.NoError(t, err)
	require.False(t, commentsLost)
	require.Equal(t, "providers:\n\n  - name: backup\n    base_url: https://b.example\n\nbudget:\n  x: 1\n", string(out))

	out, _, err = RemoveEntry(out, "providers", "backup")
	require.NoError(t, err)
	start, _, err := FindBlock(out, "providers")
	require.NoError(t, err)
	require.Equal(t, 0, start, "删光条目后顶层键保留")

	entries, err := BlockEntries(out, "providers")
	require.NoError(t, err)
	require.Empty(t, entries)

	out, err = AppendEntry(out, "providers", providerEntry("main", "https://c.example"))
	require.NoError(t, err)
	require.Contains(t, string(out), "  - name: main\n    base_url: https://c.example\n")

	t.Run("missing target", func(t *testing.T) {
		out, _, err := RemoveEntry(doc, "providers", "ghost")
		require.ErrorIs(t, err, fs.ErrNotExist)
		require.Nil(t, out)
	})
}

// TestYAMLBlock_RemoveEntryReportsCommentsLost：删除条目必然带走条目内注释，
// commentsLost 如实上报；条目之外的注释不受影响。
func TestYAMLBlock_RemoveEntryReportsCommentsLost(t *testing.T) {
	doc := []byte("# 段首\nproviders:\n  - name: main\n    base_url: https://a.example # 旧\n  - name: backup\n    base_url: https://b.example\n")

	out, commentsLost, err := RemoveEntry(doc, "providers", "main")
	require.NoError(t, err)
	require.True(t, commentsLost)
	require.Contains(t, string(out), "# 段首")
	require.NotContains(t, string(out), "旧")
	require.Contains(t, string(out), "name: backup")
}

// TestBlockEditor_PreservesOutsideBytes：整份实例配置上做替换与删除，未触及的段
// （http / log / budget）与其余条目逐字保留，整体字节精确相等。
func TestBlockEditor_PreservesOutsideBytes(t *testing.T) {
	const doc = `# 实例配置
http:
  addr: 127.0.0.1:8080
log:
  level: info
providers:
  - name: deepseek
    base_url: https://api.deepseek.com
    api_key: ${DEEPSEEK_API_KEY}
  - name: qwen
    base_url: https://dashscope.aliyuncs.com
    api_key: ${DASHSCOPE_API_KEY}
budget:
  max_iterations: 10
`

	replaced, commentsLost, err := ReplaceEntry([]byte(doc), "providers", "deepseek",
		[]byte("  - name: deepseek\n    base_url: https://api.deepseek.com/v1\n    api_key: ${DEEPSEEK_API_KEY}\n"))
	require.NoError(t, err)
	require.False(t, commentsLost)
	require.Equal(t, `# 实例配置
http:
  addr: 127.0.0.1:8080
log:
  level: info
providers:
  - name: deepseek
    base_url: https://api.deepseek.com/v1
    api_key: ${DEEPSEEK_API_KEY}
  - name: qwen
    base_url: https://dashscope.aliyuncs.com
    api_key: ${DASHSCOPE_API_KEY}
budget:
  max_iterations: 10
`, string(replaced))

	removed, _, err := RemoveEntry([]byte(doc), "providers", "qwen")
	require.NoError(t, err)
	require.Equal(t, `# 实例配置
http:
  addr: 127.0.0.1:8080
log:
  level: info
providers:
  - name: deepseek
    base_url: https://api.deepseek.com
    api_key: ${DEEPSEEK_API_KEY}
budget:
  max_iterations: 10
`, string(removed))
}

// TestBlockEditor_RejectsFlowStyle：流式集合、流式映射与非列表值不能做行级块编辑，
// 读写面一致拒绝。
func TestBlockEditor_RejectsFlowStyle(t *testing.T) {
	docs := map[string]string{
		"flow sequence":           "providers: []\n",
		"flow sequence with item": "providers: [{name: main}]\n",
		"flow mapping value":      "providers: {name: main}\n",
		"flow root":               "{providers: []}\n",
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			_, _, err := FindBlock([]byte(doc), "providers")
			require.ErrorIs(t, err, ErrEditUnsupported)

			_, err = BlockEntries([]byte(doc), "providers")
			require.ErrorIs(t, err, ErrEditUnsupported)

			_, _, err = ReplaceEntry([]byte(doc), "providers", "main", providerEntry("main", "https://a.example"))
			require.ErrorIs(t, err, ErrEditUnsupported)

			out, err := AppendEntry([]byte(doc), "providers", providerEntry("main", "https://a.example"))
			require.ErrorIs(t, err, ErrEditUnsupported)
			require.Nil(t, out)
		})
	}
}

// TestBlockEditor_RejectsDuplicateKeys：重复键由解析器拒绝，块编辑不得在解析失败
// 的文档上动手（顶层重复与条目内重复同判）。
func TestBlockEditor_RejectsDuplicateKeys(t *testing.T) {
	docs := map[string]string{
		"duplicate top-level key": "providers:\n  - name: a\nproviders:\n  - name: b\n",
		"duplicate key in item":   "providers:\n  - name: a\n    name: b\n",
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			_, _, err := FindBlock([]byte(doc), "providers")
			require.ErrorIs(t, err, ErrEditUnsupported)

			out, err := AppendEntry([]byte(doc), "providers", providerEntry("c", "https://c.example"))
			require.ErrorIs(t, err, ErrEditUnsupported)
			require.Nil(t, out)
		})
	}
}

// TestBlockEditor_RejectsAnchorsAliasesAndMergeKeys：锚点 / 别名 / 合并键的引用语义
// 无法靠行区间保留，目标块含任一构造即拒绝。
func TestBlockEditor_RejectsAnchorsAliasesAndMergeKeys(t *testing.T) {
	docs := map[string]string{
		"anchor on block": "providers: &servers\n  - name: a\n",
		"anchor on item":  "providers:\n  - &m\n    name: a\n",
		"alias item":      "base: &b\n  name: a\nproviders:\n  - *b\n",
		"merge key":       "providers:\n  - <<: {name: base}\n    name: a\n",
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			_, _, err := FindBlock([]byte(doc), "providers")
			require.ErrorIs(t, err, ErrEditUnsupported)

			_, err = BlockEntries([]byte(doc), "providers")
			require.ErrorIs(t, err, ErrEditUnsupported)
		})
	}
}

// TestYAMLBlock_ConcurrentReaders：同一份定义字节被并发读取与改写——块编辑是纯函数，
// 不共享可变状态：每次读取得到同一视图，每次改写都基于调用方传入的原字节，输入文档
// 在并发访问后逐字节不变（012-race 必跑）。
// TestYAMLBlock_ConcurrentReaders pins the editor as a pure function under the race
// detector: reads agree, rewrites never mutate the caller's bytes.
func TestYAMLBlock_ConcurrentReaders(t *testing.T) {
	doc := []byte("providers:\n" +
		"  - name: main\n    base_url: https://a.example\n" +
		"  - name: backup\n    base_url: https://b.example\n")
	snapshot := append([]byte(nil), doc...)

	base, err := BlockEntries(doc, "providers")
	require.NoError(t, err)
	require.Len(t, base, 2)

	const readers = 16
	start := make(chan struct{})
	errs := make([]error, readers)
	entries := make([][]BlockEntry, readers)
	replaced := make([][]byte, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			entries[index], errs[index] = BlockEntries(doc, "providers")
			if errs[index] != nil {
				return
			}
			replaced[index], _, errs[index] = ReplaceEntry(doc, "providers", "main",
				providerEntry("main", "https://c.example"))
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < readers; i++ {
		require.NoError(t, errs[i], "reader %d", i)
		require.Equal(t, base, entries[i], "并发读取必须得到同一块视图")
		require.Contains(t, string(replaced[i]), "https://c.example")
		require.Contains(t, string(replaced[i]), "https://b.example", "块外条目逐字保留")
	}
	require.Equal(t, string(snapshot), string(doc), "并发访问不得改写调用方字节")
	after, err := BlockEntries(doc, "providers")
	require.NoError(t, err)
	require.Equal(t, base, after)
}

// TestBlockEditor_RejectsMultiDocument：多文档分隔符（含文件末尾的裸 `---`）会改变
// 行归属，块编辑一律拒绝。
func TestBlockEditor_RejectsMultiDocument(t *testing.T) {
	docs := map[string]string{
		"two documents":      "providers:\n  - name: a\n---\nproviders:\n  - name: b\n",
		"trailing separator": "providers:\n  - name: a\n---\n",
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			_, _, err := FindBlock([]byte(doc), "providers")
			require.ErrorIs(t, err, ErrEditUnsupported)

			out, err := AppendEntry([]byte(doc), "providers", providerEntry("c", "https://c.example"))
			require.ErrorIs(t, err, ErrEditUnsupported)
			require.Nil(t, out)
		})
	}
}
