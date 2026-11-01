package dbmigrate

// 本文件是 JSONL 的写 / 读（颗粒度文档 §2.11.1、§3.1.2 的 `writeJSONL` / `readJSONL`）：
// 一行一条记录，每个值写成 `{"t":"<标签>","v":"<规范字面量>"}`，NULL 写成 `null`——
// **不**依赖驱动的 `map[string]any` 默认序列化（那会把 int64 变科学计数、[]byte 变字符串）。
// 读取用**显式上限**的扫描器，超限显式报错，**不**静默截断。
// This file is the JSONL wire format: one record per line, every value as `{"t","v"}`, NULL
// as `null`. Readers bound line length explicitly and never truncate silently.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
)

// jsonValue 是一个值的线上形态：`t` 是类型标签，`v` 是规范字面量。
// NULL 不写这个对象，直接写 JSON `null`（用 nil 指针表示）——`{"t":"s","v":""}` 与
// `null` 因此在线上就是两个不同形态，不会撞在一起。
// jsonValue is the wire form of one value; NULL is a nil pointer, never an empty object.
type jsonValue struct {
	Tag   ValueTag `json:"t"`
	Value string   `json:"v"`
}

// toWire 把一行按给定列序转成线上形态；缺列即拒绝（**不**按 NULL 补）。
func toWire(row Row, columns []string) (map[string]*jsonValue, error) {
	wire := make(map[string]*jsonValue, len(columns))
	for _, column := range columns {
		value, ok := row[column]
		if !ok {
			return nil, fmt.Errorf("%w: 缺列 %q", ErrInvalidValue, column)
		}
		// 复用编码器做校验：非法 UTF-8 / NUL / 未知标签在这里就红，而不是等到导入端。
		if _, err := encodeCanonicalValue(value); err != nil {
			return nil, fmt.Errorf("列 %q: %w", column, err)
		}
		if value.IsNull() {
			wire[column] = nil
			continue
		}
		wire[column] = &jsonValue{Tag: value.Tag, Value: string(value.Raw)}
	}
	return wire, nil
}

// fromWire 把线上形态还原成一行；`t` 缺失或未知即拒绝。
func fromWire(wire map[string]*jsonValue, columns []string) (Row, error) {
	row := make(Row, len(columns))
	for _, column := range columns {
		value, ok := wire[column]
		if !ok {
			return nil, fmt.Errorf("%w: 缺列 %q", ErrInvalidValue, column)
		}
		if value == nil {
			row[column] = Null()
			continue
		}
		if !value.Tag.Known() {
			return nil, fmt.Errorf("%w: 列 %q 的类型标签 %q 未知", ErrInvalidValue, column, value.Tag)
		}
		row[column] = EncodedValue{Tag: value.Tag, Raw: []byte(value.Value)}
	}
	if len(wire) != len(columns) {
		return nil, fmt.Errorf("%w: 记录里有 %d 列，清单声明 %d 列（多列不得忽略）",
			ErrInvalidValue, len(wire), len(columns))
	}
	return row, nil
}

// writeJSONL 把行写成 `root` 下的 `name`（每行一个 JSON 对象，键按 encoding/json 的
// 字典序输出，因此同样的行永远得到同样的字节），返回**文件原始字节**的 SHA-256（十六进制）。
// `columns` 决定"哪些列必须出现"，不决定键的书写顺序。
// 空表也会产出 0 字节文件，其摘要即空串摘要——与"没写文件"区分开。
// 走 `os.Root`：文件名固定，但目录由调用方给定，根限定让"名字里带 `..`"无处可去。
// The root-scoped handle means a crafted name can never escape the export directory.
func writeJSONL(root *os.Root, name string, rows []Row, columns []string) (string, error) {
	file, err := root.Create(name)
	if err != nil {
		return "", fmt.Errorf("dbmigrate: 创建 JSONL: %w", err)
	}
	defer func() {
		// 关闭错误在这里不影响摘要：写失败会在下面逐行报出，此处只保证句柄释放。
		_ = file.Close()
	}()

	sum := sha256.New()
	writer := bufio.NewWriter(io.MultiWriter(file, sum))
	encoder := json.NewEncoder(writer)
	for i, row := range rows {
		wire, err := toWire(row, columns)
		if err != nil {
			return "", fmt.Errorf("第 %d 行: %w", i, err)
		}
		// Encoder 会自己追加换行；map 的键序由 encoding/json 排定，稳定可比。
		if err := encoder.Encode(wire); err != nil {
			return "", fmt.Errorf("dbmigrate: 写第 %d 行: %w", i, err)
		}
	}
	if err := writer.Flush(); err != nil {
		return "", fmt.Errorf("dbmigrate: 刷新 JSONL: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("dbmigrate: 同步 JSONL: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("dbmigrate: 关闭 JSONL: %w", err)
	}
	return hexSum(sum), nil
}

// readJSONL 读回 `root` 下 `name` 的全部行，按清单列序校验列集合。
// 单行超过 `MaxRowBytes` 即显式失败——**不得**用默认 64 KiB 缓冲静默截断。
// 读同样走 `os.Root`：导入端的文件名来自清单，不允许它把读取带到导出目录之外。
func readJSONL(root *os.Root, name string, columns []string) ([]Row, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: 打开 JSONL: %w", err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxRowBytes)

	var rows []Row
	for scanner.Scan() {
		var wire map[string]*jsonValue
		if err := json.Unmarshal(scanner.Bytes(), &wire); err != nil {
			return nil, fmt.Errorf("dbmigrate: 解析第 %d 行: %w", len(rows)+1, err)
		}
		row, err := fromWire(wire, columns)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行: %w", len(rows)+1, err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("dbmigrate: 读 JSONL（第 %d 行起）: %w", len(rows)+1, err)
	}
	return rows, nil
}

// hexSum 取哈希器的十六进制摘要。
func hexSum(sum hash.Hash) string {
	return hex.EncodeToString(sum.Sum(nil))
}
