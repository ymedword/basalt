package diagnostics_test

// 本文件是新包必备的可运行 Example（颗粒度文档 §4「新包 Example」）：`Output` 注释与真实
// 输出逐字一致，`go test` 不带 `-run` 时默认执行。
// This file is the package's runnable example; its Output comment matches the real
// output and go test runs it by default.

import (
	"fmt"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/diagnostics"
)

// ExampleNewEventID 演示事件标识的形态：29 个字符（`ev_` + 26 位 Crockford 载荷）、
// 大写字面量、格式可校验，同一毫秒生成的两个标识互不相同。
// ExampleNewEventID shows the identifier shape: 29 characters, uppercase payload,
// format-valid, and two identifiers minted in the same millisecond differ.
func ExampleNewEventID() {
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	id, err := diagnostics.NewEventID(now)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	again, err := diagnostics.NewEventID(now)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(len(id), strings.HasPrefix(id, diagnostics.EventIDPrefix), id != again, diagnostics.ValidateEventID(id) == nil)
	// Output:
	// 29 true true true
}
