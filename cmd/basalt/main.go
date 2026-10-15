// Command basalt 是本项目的唯一入口：自托管 Agent OS 的单二进制。
// Command basalt is the project's only entry point: the self-hosted agent OS,
// shipped as a single binary.
//
// 时区数据库经 import _ "time/tzdata" 内嵌（见同包 tzdata 测试）——目标环境
// 可能没有系统 zoneinfo，内嵌是"单二进制可移植"的一部分，不是可选项。
// The IANA time zone database is embedded on purpose: the target environment may
// ship without system zoneinfo, so this is part of single-binary portability.
//
// 本文件除 main 之外零逻辑：装配与退出码归 internal/app，业务归各能力包。
// Nothing but main lives here; assembly and exit codes belong to internal/app.
package main

import (
	"os"
	_ "time/tzdata"
)

// version 是构建期注入的版本标识（-X main.version=<值>）；未注入时为 dev，
// 与 cli.NewRootCommand 的缺省值一致。由同包 run 转交给装配层。
// version receives the build-time stamp via -X main.version and defaults to dev.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}
