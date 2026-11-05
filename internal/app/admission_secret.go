package app

// 本文件实现 US-21 学习准入的**启发式凭证扫描**（颗粒度文档 §3.1.7，FR-21-14）：
// 提议阶段的候选文件与来源声明、以及决策 / 撤回理由，在写入任何内容之前先用固定
// 模式扫一遍；命中即按 `core.ErrAdmissionSecretDetected` 拒绝，且错误与日志**只允许**
// 出现文件路径，绝不回显命中文本或上下文。
//
// 两条纪律：
//   - **只报路径**：`ScanAdmissionSecrets` 返回命中的文件相对路径（去重、保持输入顺序），
//     不返回匹配到的文本、不返回行号与前后文——错误文案与 WARN 日志都不得据此拼出凭证；
//   - **占位不算凭证**：`${ENV_VAR}` 是项目约定的凭证占位形态（CLAUDE.md 硬性禁止明文
//     落盘），扫描前整体剔除，避免把 `password = "${DB_PASSWORD}"` 这类正确写法误判。
//
// 局限（文档要求同步声明）：这是启发式扫描，**不证明**候选没有凭证；人工审查仍是最终
// 闸门，文档与页面不得写成「已证明无凭证」。
//
// This file implements the heuristic credential scan for admissions. It reports file
// paths only — never the matched text — and ignores ${ENV_VAR} placeholders.

import (
	"regexp"

	"github.com/ymedword/basalt/internal/core"
)

// admissionEnvPlaceholder 匹配项目约定的凭证占位；扫描前整体剔除。
// admissionEnvPlaceholder matches the documented ${ENV_VAR} placeholder and is stripped
// before scanning so placeholders never count as credentials.
var admissionEnvPlaceholder = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// admissionSecretPatterns 是文档 §3.1.7 点名的固定模式集合：每家 SDK 的已知前缀、
// PEM 私钥头、Authorization 头，以及通用四类赋值长串。
// admissionSecretPatterns is the fixed pattern set from the module card §3.1.7.
var admissionSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bghp_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)authorization\s*:\s*bearer\s+[A-Za-z0-9._~+/=-]{16,}`),
	regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|token|password)\b\s*[:=]\s*["']?[A-Za-z0-9_./+=-]{16,}`),
}

// ScanAdmissionSecrets 返回命中凭证模式的候选文件路径（去重、保持输入顺序）。
// 返回的是**路径**，不是命中文本；调用方只能据此拒绝，不得打印文件内容。
//
// ScanAdmissionSecrets returns the paths of candidate files that match a credential
// pattern. Only paths are returned; the matched text is never exposed.
func ScanAdmissionSecrets(files []core.AuthorFile) []string {
	var hits []string
	seen := map[string]bool{}
	for _, file := range files {
		if seen[file.Path] || !AdmissionTextHasSecret(file.Content) {
			continue
		}
		seen[file.Path] = true
		hits = append(hits, file.Path)
	}
	return hits
}

// AdmissionTextHasSecret 判断单段文本是否命中任一卷材模式；占位先剔除。
// AdmissionTextHasSecret reports whether one text matches any credential pattern.
func AdmissionTextHasSecret(text string) bool {
	if text == "" {
		return false
	}
	stripped := admissionEnvPlaceholder.ReplaceAllString(text, "")
	for _, pattern := range admissionSecretPatterns {
		if pattern.MatchString(stripped) {
			return true
		}
	}
	return false
}

// AdmissionSourceHasSecret 检查来源声明的全部自由文本字段（`ref` / `notes` /
// `evidence` / `counterexamples` / `limitations`）；命中即拒绝整份来源。
// AdmissionSourceHasSecret scans every free-text field of the source declaration.
func AdmissionSourceHasSecret(source core.AdmissionSource) bool {
	if AdmissionTextHasSecret(source.Ref) || AdmissionTextHasSecret(source.Notes) {
		return true
	}
	for _, group := range [][]string{source.Evidence, source.Counterexamples, source.Limitations} {
		for _, item := range group {
			if AdmissionTextHasSecret(item) {
				return true
			}
		}
	}
	return false
}
