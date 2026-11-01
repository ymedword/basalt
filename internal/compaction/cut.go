package compaction

// 本文件是**切点判定**（T084）：压缩只能在**完整工具交互组**之间切。
// `assistant(tool_calls)` 与它的全部 `tool` 结果必须同进同出——拆散的半组历史会被供应商
// 拒收，也会让 assistant 转述失去对应原文。
// Cut points may only land on whole tool-interaction groups: splitting a group would send
// a tool-call message whose results were dropped, which providers reject.

// FindCut 返回**保留段**的起点下标：`messages[:cut]` 进入覆盖段（被摘要），
// `messages[cut:]` 原样保留。返回值只在切点落在组边界上时 `ok=true`。
//
// 口径：从尾部向前累计**完整工具组**，数到 `keepRecent` 组时，下一组的起点就是切点；
// 此时若切点之前没有任何消息（整段都在保留范围内），返回 `ok=false` —— 没有可压缩的
// 内容就是"不该压缩"，**不得**退化成"压缩全部"。
// `keepRecent < 1` 视为未接线（设计不写默认值），同样 `ok=false`。
// FindCut returns the first index of the kept tail; ok is false when there is nothing
// older than the kept groups to compact, because "nothing to compact" is not a cut.
func FindCut(messages []StoredMessage, keepRecent int) (int, bool) {
	if keepRecent < 1 || len(messages) == 0 {
		return 0, false
	}
	idx := len(messages)
	groups := 0
	for idx > 0 {
		start := groupStart(messages, idx)
		groups++
		if groups == keepRecent {
			if start == 0 {
				// 全部消息都在保留组内：没有覆盖段。
				return 0, false
			}
			return start, true
		}
		idx = start
	}
	return 0, false
}

// groupStart 返回到 `end`（不含）为止、与 `messages[end-1]` **同一工具组**的最左下标。
// 分组键是 `ToolGroupID`（由 `AssignToolGroups` 补齐）；空标识的消息各自成组，
// **不**把相邻的空标识消息并成一组（并组会让切点跨过两条独立消息）。
// groupStart walks left over the same ToolGroupID; empty ids never merge.
func groupStart(messages []StoredMessage, end int) int {
	gid := messages[end-1].ToolGroupID
	if gid == "" {
		return end - 1
	}
	start := end - 1
	for start > 0 && messages[start-1].ToolGroupID == gid {
		start--
	}
	return start
}

// CountGroups 数出一段消息里的完整工具组数（`DroppedGroups` 与保留组计数共用同一口径）。
// CountGroups counts the tool groups in a run of messages.
func CountGroups(messages []StoredMessage) int {
	groups := 0
	idx := len(messages)
	for idx > 0 {
		idx = groupStart(messages, idx)
		groups++
	}
	return groups
}
