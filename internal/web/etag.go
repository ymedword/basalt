package web

// 本文件是条件写的 ETag 判据唯一归属（颗粒度文档 §3.1.8）：`parseIfMatch` 只接受
// 逐字强 ETag 形状 `"sha256:<64 位小写十六进制>"`；`*`、弱 ETag（`W/`）、逗号列表、
// 缺失一律判非法并由调用方映射 412。单文件 ETag 与受管文件集合 ETag 共用同一语法、
// **不同来源**——本层只判形状，比较对象由适配器在各自的读取路径上给出；两者互换
// 不可能通过本层"猜"，只能由调用方拿错来源，因此来源绑定写在 handler / 适配器。
// This file is the single home of the If-Match shape rule: a strong ETag literal and
// nothing else. The single-file and managed-file-set ETags share this grammar but not
// their sources; source binding lives in the adapters, never in the parser.

// etagPrefix / etagHexLen / etagSuffix 是强 ETag 字面量的三段固定形状。
// The three fixed segments of a strong ETag literal.
const (
	etagPrefix = `"sha256:`
	etagSuffix = `"`
	etagHexLen = 64
	etagShape  = `"sha256:<64 位小写十六进制>"`
)

// parseIfMatch 只接受逐字的强 ETag 形状（含双引号）：`W/` 前缀、通配 `*`、裸哈希、
// 前后空白、大写十六进制与任何长度偏差一律判非法（约束 25 / FR-004）。返回值是
// 原样字符串——与当前 ETag 的比较由适配器在写锁内进行，本层不做缓存或归一化。
// parseIfMatch accepts only the exact strong-ETag shape and returns it verbatim;
// everything else (weak, wildcard, list, bare digest) is rejected.
func parseIfMatch(raw string) (string, bool) {
	if len(raw) != len(etagPrefix)+etagHexLen+len(etagSuffix) ||
		raw[:len(etagPrefix)] != etagPrefix ||
		raw[len(raw)-1:] != etagSuffix {
		return "", false
	}
	digest := raw[len(etagPrefix) : len(etagPrefix)+etagHexLen]
	for i := 0; i < len(digest); i++ {
		if c := digest[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return raw, true
}

// formatETag 把 64 位小写十六进制摘要包成强 ETag 字面量——服务端产生 ETag 的唯一
// 出口，与 parseIfMatch 共用同一份形状常量（渲染与校验不得各写一份）。
// formatETag renders a digest into the strong-ETag literal; parsing and rendering
// share the same shape constants.
func formatETag(digest string) string {
	return etagPrefix + digest + etagSuffix
}
