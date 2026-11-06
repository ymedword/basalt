package core

// 本文件实现**启发式凭证扫描**（US-21 颗粒度文档 §3.1.7 / FR-21-14；US-22 起下沉到
// `internal/core`，由技能准入与记忆抽取两侧共用同一份规则，C-12）：提议阶段的候选文件与
// 来源声明、决策 / 撤回理由，以及 US-22 的抽取候选正文与证据引文，在写入任何内容之前
// 先用固定模式扫一遍；命中即拒绝或丢弃该条，且错误与日志**只允许**出现文件路径，绝不回显
// 命中文本或上下文。
//
// 两条纪律：
//   - **只报路径**：`ScanFilesForSecrets` 返回命中的文件相对路径（去重、保持输入顺序），
//     不返回匹配到的文本、不返回行号与前后文——错误文案与 WARN 日志都不得据此拼出凭证；
//   - **占位不算凭证**：`${ENV_VAR}` 是项目约定的凭证占位形态（CLAUDE.md 硬性禁止明文
//     落盘），扫描前整体剔除，避免把 `password = "${DB_PASSWORD}"` 这类正确写法误判。
//
// 局限（文档要求同步声明）：这是启发式扫描，**不证明**候选没有凭证；人工审查仍是最终
// 闸门，文档与页面不得写成「已证明无凭证」。
//
// This file is the heuristic credential scan shared by skill admission (US-21) and memory
// extraction (US-22). It reports file paths only — never the matched text — and ignores
// ${ENV_VAR} placeholders. One rule set, two callers.

import "regexp"

// envPlaceholder 匹配项目约定的凭证占位；扫描前整体剔除。
// envPlaceholder matches the documented ${ENV_VAR} placeholder and is stripped before
// scanning so placeholders never count as credentials.
var envPlaceholder = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// secretScanPatterns 是文档 §3.1.7 点名的固定模式集合：每家 SDK 的已知前缀、
// PEM 私钥头、Authorization 头，以及通用四类赋值长串。
// secretScanPatterns is the fixed pattern set from the module card §3.1.7.
var secretScanPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bghp_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)authorization\s*:\s*bearer\s+[A-Za-z0-9._~+/=-]{16,}`),
	regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|token|password)\b\s*[:=]\s*["']?[A-Za-z0-9_./+=-]{16,}`),
}

// ScanFilesForSecrets 返回命中凭证模式的文件路径（去重、保持输入顺序）。
// 返回的是**路径**，不是命中文本；调用方只能据此拒绝，不得打印文件内容。
//
// ScanFilesForSecrets returns the paths of files that match a credential pattern.
// Only paths are returned; the matched text is never exposed.
func ScanFilesForSecrets(files []AuthorFile) []string {
	var hits []string
	seen := map[string]bool{}
	for _, file := range files {
		if seen[file.Path] || !TextHasSecret(file.Content) {
			continue
		}
		seen[file.Path] = true
		hits = append(hits, file.Path)
	}
	return hits
}

// TextHasSecret 判断单段文本是否命中任一凭证模式；占位先剔除。US-21 的候选文件 /
// 理由与 US-22 的抽取候选正文 / 证据引文都调用这一个函数。
// TextHasSecret reports whether one text matches any credential pattern. The US-21
// admission path and the US-22 extraction path both call this single function.
func TextHasSecret(text string) bool {
	if text == "" {
		return false
	}
	stripped := envPlaceholder.ReplaceAllString(text, "")
	for _, pattern := range secretScanPatterns {
		if pattern.MatchString(stripped) {
			return true
		}
	}
	return false
}

// SourceHasSecret 检查来源声明的全部自由文本字段（`ref` / `notes` /
// `evidence` / `counterexamples` / `limitations`）；命中即拒绝整份来源。
// SourceHasSecret scans every free-text field of the source declaration.
func SourceHasSecret(source AdmissionSource) bool {
	if TextHasSecret(source.Ref) || TextHasSecret(source.Notes) {
		return true
	}
	for _, group := range [][]string{source.Evidence, source.Counterexamples, source.Limitations} {
		for _, item := range group {
			if TextHasSecret(item) {
				return true
			}
		}
	}
	return false
}
