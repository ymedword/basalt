package dbmigrate

// 本文件是未归类文件的**启发式内容扫描**（设计 §2.11.5 / C-49）：只提示，不证明。
//
// 三条纪律写死在接口形状里：
//
//	① 只返回「规则 ID → 命中文件数」，**不**返回命中的文件名、行号或原文——
//	   报告外发时不可能夹带凭据；
//	② 结果**不**参与任何放行 / 删除判断：`ReviewStatus`、`Disposition` 与清单摘要
//	   都不因命中而改变，未命中更**不**等于"包内无秘密"；
//	③ 只扫 `class=unclassified` 条目：已归类业务文件不在核查范围，跑全仓扫描会把
//	   正常业务文本当噪声源。
//
// This file is the heuristic content scan for unclassified files (C-49). It returns
// "rule id -> matching file count" and nothing else: no paths, no lines, no matches.
// The result never feeds a disposition decision and never proves the package is clean.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strings"
)

const (
	// contentHintPrivateKey 命中 PEM 私钥头。
	contentHintPrivateKey = "private_key_header"
	// contentHintTokenShape 命中已知令牌形态（厂商前缀 + 固定长度）。
	contentHintTokenShape = "token_shape"
	// contentHintHighEntropy 命中高熵且字符类混合的串（base64 / 随机口令的常见残余）。
	contentHintHighEntropy = "high_entropy"
	// contentHintBinary 表示文件含 NUL 字节，按二进制跳过内容判读——计入报告是为了
	// 让"没看"这件事可见，而不是静默当作干净。
	contentHintBinary = "binary_skipped"
	// contentHintTruncated 表示文件超过扫描上限，只读了前一段——同样计入报告。
	contentHintTruncated = "truncated"
)

const (
	// contentScanLimit 是单文件读取上限（1 MiB）：扫描是提示，不追求覆盖大二进制。
	contentScanLimit = 1 << 20
	// contentScanBinaryProbe 是判定二进制的探测窗口。
	contentScanBinaryProbe = 8 << 10
	// minEntropyRun 是参与熵判读的最短串长。
	minEntropyRun = 24
	// minEntropyBits 是判为高熵的阈值（每字符比特数）；普通英文约 4.0 以下。
	minEntropyBits = 4.2
)

// contentPrivateKeyHeader 匹配 PEM 私钥头（含 RSA / EC / OPENSSH 等变体）。
// contentPrivateKeyHeader matches a PEM private-key header, including RSA/EC/OPENSSH.
var contentPrivateKeyHeader = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)

// contentTokenShapes 是"可证明形态"的令牌判据：厂商前缀 + 固定长度，不做猜测。
// 新增形态只在这个列表里加，扫描结果永远只报规则 ID 与计数。
// contentTokenShapes lists provable token shapes: vendor prefix plus fixed length.
var contentTokenShapes = []*regexp.Regexp{
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`),
}

// contentEntropyRun 抓一段可能是随机串的字符序列；是否高熵由熵判读决定。
// 长度下限与 minEntropyRun 共用同一常量，避免正则与判据各写一份而漂移。
// contentEntropyRun extracts candidate random-looking runs for the entropy check.
var contentEntropyRun = regexp.MustCompile(
	fmt.Sprintf(`[A-Za-z0-9+/=_\-]{%d,}`, minEntropyRun),
)

// ScanUnclassifiedHints 对清单里的**未归类**条目做启发式内容扫描，返回
// 「规则 ID → 命中文件数」。空清单返回空 map（不是 nil，调用方可无差别 range）。
//
// 命中只意味着"值得人工看一眼"：本函数不删文件、不放行文件、不改清单状态，也不构成
// 任何"包内无秘密"的证明——调用方的报告**不得**写成已排除敏感信息。
// ScanUnclassifiedHints returns "rule id -> matching file count" for unclassified
// entries only. Hits are hints for a human reviewer, never a safety proof.
func ScanUnclassifiedHints(ctx context.Context, workspace string, m AssetManifest) (map[string]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := strings.TrimSpace(workspace)
	if root == "" {
		return nil, errors.New("dbmigrate: 内容扫描必须给出工作区根目录（<WorkDir>/.basalt）")
	}
	if err := validateAssetManifest(m); err != nil {
		return nil, err
	}

	hints := map[string]int{}
	source, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: 打开工作区 %s 做内容扫描: %w", root, err)
	}
	defer func() { _ = source.Close() }()

	for _, entry := range m.Entries {
		if entry.Class != AssetClassUnclassified {
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		raw, truncated, err := readScanWindow(source, entry.RelPath)
		if err != nil {
			return nil, err
		}
		if truncated {
			hints[contentHintTruncated]++
		}
		probe := raw
		if len(probe) > contentScanBinaryProbe {
			probe = probe[:contentScanBinaryProbe]
		}
		if bytes.IndexByte(probe, 0) >= 0 {
			hints[contentHintBinary]++
			continue
		}
		if contentPrivateKeyHeader.Match(raw) {
			hints[contentHintPrivateKey]++
		}
		if matchTokenShape(raw) {
			hints[contentHintTokenShape]++
		}
		if containsHighEntropyRun(raw) {
			hints[contentHintHighEntropy]++
		}
	}
	return hints, nil
}

// readScanWindow 通过工作区根句柄读一个条目的前 contentScanLimit 字节；返回
// truncated 表示文件更长、只读了窗口。根句柄保证路径不会逃出工作区。
// readScanWindow reads the first contentScanLimit bytes through the workspace root
// handle, so a manifest relpath can never escape the workspace.
func readScanWindow(source *os.Root, rel string) ([]byte, bool, error) {
	file, err := source.Open(rel)
	if err != nil {
		return nil, false, fmt.Errorf("dbmigrate: 内容扫描读 %s: %w", rel, err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, contentScanLimit+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, false, fmt.Errorf("dbmigrate: 内容扫描读 %s: %w", rel, readErr)
	}
	if closeErr != nil {
		return nil, false, fmt.Errorf("dbmigrate: 内容扫描关闭 %s: %w", rel, closeErr)
	}
	if len(raw) > contentScanLimit {
		return raw[:contentScanLimit], true, nil
	}
	return raw, false, nil
}

// matchTokenShape 报告内容里是否出现任一已知令牌形态。
// matchTokenShape reports whether any known token shape appears.
func matchTokenShape(raw []byte) bool {
	for _, pattern := range contentTokenShapes {
		if pattern.Match(raw) {
			return true
		}
	}
	return false
}

// containsHighEntropyRun 报告内容里是否存在一段"像随机串"的序列：长度够、至少一位
// 数字 + 一位字母、香农熵达标，且**不是**全十六进制——全十六进制串在本仓里几乎总是
// SHA-256 摘要（资产清单、迁移报告），报出来只会淹没真正需要看的命中。
// containsHighEntropyRun reports a random-looking run. All-hex runs are treated as
// digests and skipped, because manifests here are full of SHA-256 values.
func containsHighEntropyRun(raw []byte) bool {
	for _, run := range contentEntropyRun.FindAll(raw, 64) {
		if isAllHex(run) || !hasDigitAndLetter(run) {
			continue
		}
		if shannonEntropy(run) >= minEntropyBits {
			return true
		}
	}
	return false
}

// isAllHex 报告序列是否全部由十六进制数字组成（摘要形态）。
// isAllHex reports whether the run is a plain hex digest.
func isAllHex(run []byte) bool {
	for _, b := range run {
		switch {
		case b >= '0' && b <= '9', b >= 'a' && b <= 'f', b >= 'A' && b <= 'F':
		default:
			return false
		}
	}
	return true
}

// hasDigitAndLetter 要求序列同时含数字与字母，避免把长英文单词判成随机串。
// hasDigitAndLetter requires both a digit and a letter so long words never qualify.
func hasDigitAndLetter(run []byte) bool {
	var digit, letter bool
	for _, b := range run {
		switch {
		case b >= '0' && b <= '9':
			digit = true
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z':
			letter = true
		}
	}
	return digit && letter
}

// shannonEntropy 返回序列的每字符香农熵（比特）。
// shannonEntropy returns the per-character Shannon entropy in bits.
func shannonEntropy(run []byte) float64 {
	if len(run) == 0 {
		return 0
	}
	var counts [256]int
	for _, b := range run {
		counts[b]++
	}
	total := float64(len(run))
	entropy := 0.0
	for _, count := range counts {
		if count == 0 {
			continue
		}
		p := float64(count) / total
		entropy -= p * math.Log2(p)
	}
	return entropy
}
