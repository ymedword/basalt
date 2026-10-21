package core

import "time"

// 本文件只在测试构建里存在（`_test.go` 后缀），不进入发布二进制。
// This file exists only in test builds; nothing here ships.

// SetCancelWaitForTest 把取消等待上限改小（取消超时用例经它把 30 秒缩到毫秒级）。
// SetCancelWaitForTest shortens the cancel wait so timeout cases need not wait 30 seconds.
//
// 只在**运行开始之前**调用：字段不设并发保护，生产路径恒为构造期写入的缺省值。
// Call it before any run starts: the field is not concurrency-protected on purpose and
// production always keeps the constructor default.
func SetCancelWaitForTest(s *AgentService, d time.Duration) {
	s.cancelWait = d
}
