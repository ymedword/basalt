package app

// 本文件实现 `config_without_key_path_sha256`（颗粒度文档 §2.12.0b「算法」，C-51）。
//
// 这个摘要回答的问题只有一个：**除 `secrets.master_key_file` 这一个标量以外，
// 这份配置文件的原始字节有没有变**。因此算法必须贴着原始字节走：
//
//	输入是原始字节，不是解析后的结构——解析 → 补默认值 → 重新序列化会丢未知字段、
//	改键顺序、补默认值，"只改了密钥路径"就会变成"整份看起来一样"的假通过；
//	唯一掩码是把该标量的原始字节区间替换为固定哨兵 `<masked>`；
//	注释、键顺序、行尾（CRLF / LF）、未知字段全部原样进入摘要——保守口径：
//	这些变化也算配置变化，必须走新的受控调整记录。
//
// 摘要只证明"文件字节层面除该标量外未变"；它**不**证明进程环境变量、外部依赖或
// 运行期解析结果没变，源端漂移仍以 `source_config_sha256`（整文件）为准。
// The digest covers raw bytes with exactly one scalar masked, so comments, key order and
// line endings still count as configuration changes.

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// 三个可判定拒绝（§2.12.0b）：键缺失 / 配置非法 / 定义有歧义——都不得靠猜。
// Three refusals: missing key, invalid configuration, ambiguous definition.
var (
	errConfigKeyPathMissing = errors.New("配置里没有 secrets.master_key_file")
	errConfigInvalid        = errors.New("配置内容非法")
	errConfigAmbiguous      = errors.New("配置存在歧义定义")
)

// configMaskedSentinel 是密钥路径标量的固定替换值（ASCII，不含 YAML 元字符）。
// configMaskedSentinel is the fixed ASCII replacement for the key-path scalar.
const configMaskedSentinel = "<masked>"

// configMaskedKeyPath 是唯一允许掩码的键路径。
// configMaskedKeyPath is the only key whose value may be masked.
const configMaskedKeyPath = "$.secrets.master_key_file"

// configMaskedSHA256 计算 `config_without_key_path_sha256`：原始字节、仅掩码
// `secrets.master_key_file` 的标量区间。失败原因经上面三个哨兵返回，调用方按
// §2.12.0b 的字面量映射退出码。
// configMaskedSHA256 hashes the raw bytes with only the key-path scalar masked out.
func configMaskedSHA256(configPath string) (string, error) {
	// 读取经目录句柄（`os.Root`，与 `readConfigFile` / `readFileUnder` 同一先例）：
	// 路径来自调用方参数，根限定既消除静态扫描的 G304，也把"跳到目录之外的软连接"
	// 挡在门外——fail-closed。
	// The read is scoped to the file's own directory through os.Root.
	raw, err := readFileUnder(filepath.Dir(configPath), filepath.Base(configPath))
	if err != nil {
		return "", fmt.Errorf("%w: 读取配置失败: %v", errConfigInvalid, err)
	}
	masked, err := maskMasterKeyPath(raw)
	if err != nil {
		return "", err
	}
	return hexSHA256(masked), nil
}

// locateMasterKeyPath 解析原始字节并定位**唯一**的 `secrets.master_key_file` 标量节点。
//
// 掩码（本文件）与 handoff 交接核对（`masterKeyPathValue`）共用这一份定位：两处若各写
// 一套遍历，"哪个值才算生效值"的口径就会分叉。失败原因经三个哨兵返回。
// locateMasterKeyPath resolves the single key-path scalar; masking and handoff
// verification share it so both agree on what counts as the effective value.
func locateMasterKeyPath(raw []byte) (*ast.MappingValueNode, error) {
	file, err := parser.ParseBytes(raw, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: YAML 解析失败: %v", errConfigInvalid, err)
	}
	// 多文档没有唯一的 `secrets` 段：不得猜"哪一个才是生效值"。
	if len(file.Docs) != 1 {
		return nil, fmt.Errorf("%w: 配置文件有 %d 个 YAML 文档，无法确定哪一份生效",
			errConfigAmbiguous, len(file.Docs))
	}

	var (
		found  *ast.MappingValueNode
		count  int
		merged bool
	)
	var walk func(node ast.Node)
	walk = func(node ast.Node) {
		if node == nil {
			return
		}
		switch value := node.(type) {
		case *ast.MappingNode:
			for _, item := range value.Values {
				walk(item)
			}
		case *ast.MappingValueNode:
			switch value.GetPath() {
			case configMaskedKeyPath:
				count++
				found = value
			case "$.secrets.<<":
				// 合并锚点会把键从别处带进来：生效值不在本文档里。
				merged = true
			}
			walk(value.Value)
		case *ast.SequenceNode:
			for _, item := range value.Values {
				walk(item)
			}
		}
	}
	walk(file.Docs[0].Body)
	if merged {
		return nil, fmt.Errorf("%w: secrets 段使用了合并锚点（<<），生效值不来自本文档", errConfigAmbiguous)
	}
	switch {
	case count == 0:
		return nil, errConfigKeyPathMissing
	case count > 1:
		return nil, fmt.Errorf("%w: secrets.master_key_file 定义了 %d 次", errConfigAmbiguous, count)
	}
	// 别名 / 锚点节点：值来自别处，标量区间不属于本行。
	switch found.Value.(type) {
	case *ast.StringNode:
	case *ast.AliasNode, *ast.AnchorNode:
		return nil, fmt.Errorf("%w: secrets.master_key_file 的值来自锚点 / 别名", errConfigAmbiguous)
	default:
		return nil, fmt.Errorf("%w: secrets.master_key_file 不是字符串标量（类型 %v）",
			errConfigInvalid, found.Value.Type())
	}
	return found, nil
}

// masterKeyPathValue 返回 `secrets.master_key_file` 的解码后字符串值——引号已去掉，
// 但 `${VAR}` **未展开**；展开由调用方按装载层的同一实现决定（handoff 用
// `ResolveEnvPlaceholder`，与本进程环境同口径）。
// masterKeyPathValue returns the decoded scalar; `${VAR}` is still unexpanded.
func masterKeyPathValue(raw []byte) (string, error) {
	found, err := locateMasterKeyPath(raw)
	if err != nil {
		return "", err
	}
	// locateMasterKeyPath 已排除了别名 / 锚点与非字符串标量。
	node, ok := found.Value.(*ast.StringNode)
	if !ok {
		return "", fmt.Errorf("%w: secrets.master_key_file 不是字符串标量", errConfigInvalid)
	}
	return node.Value, nil
}

// masterKeyPathAnchor 返回密钥路径标量在原文里的定位锚点（行号 + 行内键写法）。
// masterKeyPathAnchor reports the scalar's line and in-line key spelling.
func masterKeyPathAnchor(found *ast.MappingValueNode) (int, string) {
	line := found.Value.GetToken().Position.Line
	if line <= 0 {
		line = found.Key.GetToken().Position.Line
	}
	// 键在行里的写法以 AST 为准：嵌套映射写 `master_key_file:`，扁平写法才是
	// `secrets.master_key_file:`——只用点号形式去行内搜索会把正常的嵌套配置判成非法
	// （T074 实测）。
	// The key text comes from the AST: a nested mapping writes `master_key_file:`.
	keyText := adoptKeyPathConfigKey
	if keyNode := found.Key; keyNode != nil {
		if token := keyNode.GetToken(); token != nil && token.Value != "" {
			keyText = token.Value
		}
	}
	return line, keyText
}

// maskMasterKeyPath 定位并掩码密钥路径标量，返回新的字节切片（其余字节不变）。
// maskMasterKeyPath masks the key-path scalar byte range and leaves every other byte.
func maskMasterKeyPath(raw []byte) ([]byte, error) {
	found, err := locateMasterKeyPath(raw)
	if err != nil {
		return nil, err
	}
	line, keyText := masterKeyPathAnchor(found)
	start, end, err := masterKeyScalarRange(raw, line, keyText)
	if err != nil {
		return nil, err
	}
	masked := make([]byte, 0, len(raw)-(end-start)+len(configMaskedSentinel))
	masked = append(masked, raw[:start]...)
	masked = append(masked, configMaskedSentinel...)
	masked = append(masked, raw[end:]...)
	return masked, nil
}

// masterKeyScalarRange 在指定行上定位密钥路径标量的**原始字节区间**。
//
// 不走 `token.Position.Offset`：实测（goccy/go-yaml v1.19.2）双引号标量的 Offset
// 指向引号**内部**、单引号指向引号本身，`Origin` 还带前导空格与行尾换行——用它切片
// 会错位并改写相邻字节。这里改为按行文本扫描，并在引号 / 注释边界上失败关闭。
// The range comes from a line scan, not token offsets: measured offsets are inside the
// quotes for double-quoted scalars and the origins carry leading spaces.
func masterKeyScalarRange(raw []byte, line int, keyText string) (int, int, error) {
	lineStart, lineEnd := lineBounds(raw, line)
	if lineStart < 0 {
		return 0, 0, fmt.Errorf("%w: 定位不到第 %d 行", errConfigInvalid, line)
	}
	content := raw[lineStart:lineEnd]
	keyAt := bytes.Index(content, []byte(keyText))
	if keyAt < 0 {
		return 0, 0, fmt.Errorf("%w: 第 %d 行没有 %s 键", errConfigInvalid, line, keyText)
	}
	colonAt := bytes.IndexByte(content[keyAt:], ':')
	if colonAt < 0 {
		return 0, 0, fmt.Errorf("%w: 第 %d 行的 %s 后没有冒号", errConfigInvalid, line, keyText)
	}
	valueAt := keyAt + colonAt + 1
	for valueAt < len(content) && (content[valueAt] == ' ' || content[valueAt] == '\t') {
		valueAt++
	}
	if valueAt >= len(content) || content[valueAt] == '\r' || content[valueAt] == '\n' {
		return 0, 0, fmt.Errorf("%w: 第 %d 行的 %s 没有值", errConfigInvalid, line, adoptKeyPathConfigKey)
	}
	valueEnd, err := scalarEnd(content, valueAt)
	if err != nil {
		return 0, 0, err
	}
	return lineStart + valueAt, lineStart + valueEnd, nil
}

// scalarEnd 返回标量在 content 里的结束下标（不含）。引号必须成对闭合；注释只从
// **引号之外**的 ` #` 起算——引号内的 `#` 属于值本身。
// scalarEnd finds the scalar's end, honouring quotes so a '#' inside them stays a value.
func scalarEnd(content []byte, start int) (int, error) {
	switch content[start] {
	case '"':
		for i := start + 1; i < len(content); i++ {
			switch content[i] {
			case '\\':
				i++
			case '"':
				return i + 1, nil
			case '\r', '\n':
				return 0, fmt.Errorf("%w: 双引号标量未闭合", errConfigInvalid)
			}
		}
		return 0, fmt.Errorf("%w: 双引号标量未闭合", errConfigInvalid)
	case '\'':
		for i := start + 1; i < len(content); i++ {
			switch content[i] {
			case '\'':
				// 单引号里的 `''` 是一个转义的单引号，不是结束。
				if i+1 < len(content) && content[i+1] == '\'' {
					i++
					continue
				}
				return i + 1, nil
			case '\r', '\n':
				return 0, fmt.Errorf("%w: 单引号标量未闭合", errConfigInvalid)
			}
		}
		return 0, fmt.Errorf("%w: 单引号标量未闭合", errConfigInvalid)
	default:
		end := start
		for end < len(content) {
			if content[end] == '\r' || content[end] == '\n' {
				break
			}
			// YAML 的行内注释要求 `#` 前有空白；`/abs/key#1` 里的 `#` 仍是值。
			if content[end] == '#' && end > start && (content[end-1] == ' ' || content[end-1] == '\t') {
				break
			}
			end++
		}
		for end > start && (content[end-1] == ' ' || content[end-1] == '\t') {
			end--
		}
		return end, nil
	}
}

// lineBounds 返回第 line 行（1 起）在 raw 里的起止下标（不含换行符）。
// lineBounds returns the byte bounds of a 1-based line, excluding the newline.
func lineBounds(raw []byte, line int) (int, int) {
	if line < 1 {
		return -1, -1
	}
	start := 0
	for current := 1; current < line; current++ {
		next := bytes.IndexByte(raw[start:], '\n')
		if next < 0 {
			return -1, -1
		}
		start += next + 1
	}
	end := len(raw)
	if next := bytes.IndexByte(raw[start:], '\n'); next >= 0 {
		end = start + next
	}
	return start, end
}
