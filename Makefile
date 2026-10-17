# Basalt 门禁 / Basalt quality gate
#
# 一条命令跑全：`make check`（= fmt lint sec test test-integration cover build 七目标聚合）。
# `make check` runs the whole gate: formatting, lint, security, tests, integration
# build-tagged tests, per-package coverage thresholds and the release build.
#
# 三条实测纪律（写错会让门禁"报绿而实际没检"，见 docs/DevelopmentLoop.md §2.2）：
#   1. `gofmt -l` / `gofumpt -l` 发现格式差异时**退出码仍为 0**（只打印到 stdout）；
#      语法错误时反而**退出码非 0**——故判卷顺序**必须先查退出码、再判输出**；
#   2. `go test -race` 需要 CGO 与 C 编译器，与 `CGO_ENABLED=0` 发布构建要求的
#      CGO 状态相反——两者分属不同目标 / job；
#   3. build tag 隔离的测试默认不跑——`test-integration` 是**独立**目标，不能省。
#
# 显式指定配方 shell：不同平台的 make 默认 shell 不一致，统一到 POSIX sh。
SHELL := /bin/sh

.PHONY: check fmt lint sec test test-integration cover build acceptance evals

check: fmt lint sec test test-integration cover build

# Unified development acceptance: seven gates plus fixed behavior/injection evals.
acceptance: ; sh scripts/check-acceptance.sh --profile full
evals: ; sh scripts/check-acceptance.sh --profile evals

# 格式：两个工具各查一遍；**先查退出码、再判输出**（顺序写反会把"解析不了"判成通过）。
# 判卷逻辑落在 scripts/check-format.sh，不写成配方：本机 make 是 winget 装的
# ezwinports 原生构建（GNU Make 4.4.1 Built for Windows32），按 ANSI 代码页读取
# Makefile——配方里的中文会被解码错位并破坏引号配对，整条配方直接报语法错误
# （实测复现，详见 docs/DevelopmentLoop.md §7 陷阱表）。脚本由 sh 直读，无此问题。
fmt: ; sh scripts/check-format.sh

# 静态检查：判定集**显式指定**（命名与注释规范由门禁判卷），不得裸跑 staticcheck。
lint: ; go vet ./... && staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./...

# 安全：gosec（安全扫描）+ govulncheck（依赖漏洞）——两者都必须零命中。
sec: ; gosec -quiet ./... && govulncheck ./...

# 测试：-race 需要 CGO / C 编译器（与 build 目标的 CGO 状态相反，故分属不同目标）。
test: ; go test -race -cover ./...

# 集成冒烟：build tag 隔离的用例默认不执行，必须单列一步（本 US 空跑通过即绿）。
test-integration: ; go test -tags=integration ./...

# 覆盖率底线：go test -cover 只打印百分比、不会因低于阈值而失败——由脚本按包判失败。
cover: ; sh scripts/check-coverage.sh

# 发布构建：单二进制纪律（CGO_ENABLED=0）；版本标识经 -X 注入入口包。
# 配方里的 `$$(...)` 必须双写——单写会被 Make 当变量引用展开为空。
build: ; CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$$(git describe --tags --always)" -o bin/basalt ./cmd/basalt
