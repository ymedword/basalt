package cli

// 本文件解析 `basalt.yaml` 的 `author:` 段（颗粒度文档 §3.1.9）：作者模型一句话
// 生成 Agent 文件所需的 Provider / 模型与温度。缺段 = 作者功能关闭；有段但缺
// `provider` / `model` 不阻断启动——端点被调用时才返回 503（internal/app 的作者
// 服务）。未知键与类型 / 范围错误在装载期拒绝：配置拼写错误必须当场可见，而不是
// 等到调用端点才让人回头猜。
// This file parses the author: section of basalt.yaml. An absent section disables the
// feature; missing provider/model keys do not block startup (the endpoint answers 503
// when called). Unknown keys and type/range errors fail at load time so a typo is
// visible immediately instead of surfacing as a late 503.

import (
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/ymedword/basalt/internal/core"
)

// author 段的键名与温度取值域（键名逐字取自颗粒度文档 §3.1.9）。
// The author-section key names and the temperature domain, verbatim from the module card.
const (
	authorKeyProvider    = "provider"
	authorKeyModel       = "model"
	authorKeyTemperature = "temperature"

	// defaultAuthorTemperature 是 temperature 缺省值——只作用于"段存在但省略
	// temperature"；整段缺失返回零值（作者功能关闭，不发生任何调用）。
	// defaultAuthorTemperature applies only when the section exists but omits the key;
	// a fully absent section returns the zero value instead.
	defaultAuthorTemperature = 0.2

	// authorTemperatureMin / authorTemperatureMax 是 temperature 的闭区间边界。
	// authorTemperatureMin and authorTemperatureMax bound the closed interval.
	authorTemperatureMin = 0.0
	authorTemperatureMax = 2.0
)

// ParseAuthorSection 把 `author:` 段的原始映射解析为类型化配置。
// ParseAuthorSection parses the raw author: mapping into the typed configuration.
//
// 缺段（nil）→ 零值：作者功能关闭，下游以 `Provider == ""` 判定。有段但省略
// `provider` / `model` → 不报错（字段留空），端点调用时 503。`temperature` 缺省
// 0.2、闭区间 `[0,2]`。未知键与类型错误一律拒绝——这是装载期可见的配置拼写保护。
// A nil section yields the zero value (feature off). A present section may omit
// provider/model without failing startup; the endpoint reports 503 at call time.
// Temperature defaults to 0.2 and must lie in [0, 2]. Unknown keys and type errors are
// rejected so configuration typos surface at load time.
func ParseAuthorSection(raw map[string]any) (core.AuthorConfig, error) {
	if raw == nil {
		return core.AuthorConfig{}, nil
	}

	cfg := core.AuthorConfig{Temperature: defaultAuthorTemperature}

	// 键序排序后遍历：同一份坏配置两次运行必须报同一个未知键——map 遍历顺序在
	// Go 里随机，不排序会让排障的人以为配置变了。
	// Sorted traversal keeps the reported unknown key deterministic across runs.
	for _, key := range slices.Sorted(maps.Keys(raw)) {
		value := raw[key]
		switch key {
		case authorKeyProvider:
			provider, err := authorStringValue(key, value)
			if err != nil {
				return core.AuthorConfig{}, err
			}
			cfg.Provider = provider
		case authorKeyModel:
			model, err := authorStringValue(key, value)
			if err != nil {
				return core.AuthorConfig{}, err
			}
			cfg.Model = model
		case authorKeyTemperature:
			temperature, err := authorTemperatureValue(value)
			if err != nil {
				return core.AuthorConfig{}, err
			}
			cfg.Temperature = temperature
		default:
			return core.AuthorConfig{}, fmt.Errorf(
				"配置项 author 段包含未知键 %q——只允许 provider / model / temperature", key)
		}
	}
	return cfg, nil
}

// authorSection 从解析后的配置树里取出 `author:` 段；缺段 / 空值为 nil（功能关闭），
// 段存在但不是映射 → 类型错误。类型检查必须在 ParseAuthorSection 之前：签名只接受
// 映射，非映射形态（如 `author: oops`）在调用方这一层就要拒绝，不能静默当缺段。
// authorSection extracts the raw section; nil means absent, while a present non-mapping
// value is a type error (it must not be silently read as absent).
func authorSection(tree map[string]any) (map[string]any, error) {
	node, ok := tree["author"]
	if !ok || node == nil {
		return nil, nil
	}
	section, ok := node.(map[string]any)
	if !ok {
		return nil, fmt.Errorf(
			"配置项类型非法：author 段必须是映射（键值对），实际是 %s", nodeTypeName(node))
	}
	return section, nil
}

// authorStringValue 读取字符串键；缺键 / 空值视为未配置（返回空串，不报错）。
// authorStringValue reads a string key; absent or null means unset.
func authorStringValue(key string, value any) (string, error) {
	if value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf(
			"配置项 author.%s 必须是字符串（当前为 %s）", key, nodeTypeName(value))
	}
	return text, nil
}

// authorTemperatureValue 读取并校验 temperature：缺省 0.2、闭区间 `[0,2]`。
// YAML 的整数解码为 int64 / uint64、小数解码为 float64，三种数值形态都接受；
// NaN 与无穷必须显式拒绝——区间比较对 NaN 恒为假，不查会静默放行。
// authorTemperatureValue accepts int64/uint64/float64 and rejects NaN and infinities
// explicitly, because range comparisons are always false for NaN.
func authorTemperatureValue(value any) (float64, error) {
	switch number := value.(type) {
	case nil:
		return defaultAuthorTemperature, nil
	case int64:
		if number < authorTemperatureMin || number > authorTemperatureMax {
			return 0, authorTemperatureRangeError(number)
		}
		return float64(number), nil
	case uint64:
		if number > authorTemperatureMax {
			return 0, authorTemperatureRangeError(number)
		}
		return float64(number), nil
	case float64:
		if math.IsNaN(number) || number < authorTemperatureMin || number > authorTemperatureMax {
			return 0, authorTemperatureRangeError(number)
		}
		return number, nil
	default:
		return 0, fmt.Errorf(
			"配置项 author.temperature 必须是数字（当前为 %s）", nodeTypeName(value))
	}
}

// authorTemperatureRangeError 是越界 / 非有限值的固定文案：点名键与闭区间。
// authorTemperatureRangeError names the key and the closed interval.
func authorTemperatureRangeError(value any) error {
	return fmt.Errorf(
		"配置项 author.temperature 必须是 [0,2] 区间内的有限数字（当前值 %v）", value)
}
