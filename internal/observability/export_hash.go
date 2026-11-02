package observability

// ExportStateHash 仅哈希实际外发投影，私有会话与配置变化不制造遥测新状态。
// ExportStateHash hashes the export projection so private session/config changes cannot create new telemetry states.
func ExportStateHash(item ExportItem) (string, error) {
	if item.SourceKind == SourceMetricSnapshot {
		return CanonicalStateHash(item.Fact)
	}
	item.StateHash = ""
	payload, err := (&Mapper{}).mapItem(item)
	if err != nil {
		// 坏记录仍可入队并逐项隔离，不能阻断整个扫描窗口或复制原文。
		// Invalid records can be queued and isolated without blocking a window or copying raw content.
		return CanonicalStateHash(struct {
			Source            SourceKind
			ID, Signal, Error string
		}{item.SourceKind, item.SourceID, string(item.Signal), "mapping_error"})
	}
	return CanonicalStateHash(payload)
}
