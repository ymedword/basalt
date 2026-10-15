package main

import (
	"testing"
	"time"
)

// TestTZData_LoadLocationPortable（默认层，可移植）：进程内加载检查——**不**判隔离有效性。
// 平台时区源（Linux /usr/share/zoneinfo、Windows 注册表等）无法用环境变量遮蔽，
// 强隔离断言放默认层会让正确实现在装有系统 tzdata 的机器上判红（评审修订三稿 P2）。
// 隔离有效性由 //go:build tzisolated 组在「已删系统时区数据与 GOROOT zoneinfo.zip」
// 的容器里判卷（CI 必跑步骤）。
func TestTZData_LoadLocationPortable(t *testing.T) {
	if _, err := time.LoadLocation("Asia/Shanghai"); err != nil {
		t.Fatalf("Asia/Shanghai 解析失败: %v", err)
	}
}
