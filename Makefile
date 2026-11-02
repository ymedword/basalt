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
# 环境依赖（D-01，US-16）：`test-integration` 与 `cover` 需要真实 PostgreSQL——缺
# `BASALT_TEST_PG_DSN` 即失败（不跳过、不回退 SQLite），`make check` 因此整体依赖 PG；
# `test` 保持纯单元，不读 DSN。
# `test-integration` and `cover` require a real PostgreSQL (D-01): a missing
# BASALT_TEST_PG_DSN fails the target instead of skipping it.
#
# 显式指定配方 shell：不同平台的 make 默认 shell 不一致，统一到 POSIX sh。
SHELL := /bin/sh

.PHONY: check fmt lint sec test test-integration cover build acceptance evals example-tests e2e

check: fmt lint sec test test-integration cover build

# 示例单测的解释器：本机 python / python3 是 Microsoft Store 占位符（运行即 exit 49、
# 无输出），可用的解释器只有 py -3 启动器；其他平台用 `make example-tests PYTHON=python` 覆盖。
# The interpreter for the example tests: on this machine python/python3 are Store stubs that
# exit 49, so the launcher is the only usable default. Override with PYTHON=python elsewhere.
PYTHON ?= py -3

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
# 装载**必须**带 `-tags=integration`：US-16 起 `internal/app` 的存储相关验收整体在集成层
# （`//go:build integration`），不带 tag 时这些调用方不在编译集里，生产者（测试助手、
# 接线构造器）会被判成 U1000「未使用」——门禁看上去在判、其实只判了半个代码库，
# 与陷阱 56（平台分片文件集）同类。untagged 文件在两个编译集里都在，故本改动只增不减。
# The analysis must load with `-tags=integration`: since US-16 the storage-facing
# acceptance lives behind that tag, and without it the gate silently only sees half the
# codebase (test helpers and wiring constructors read as unused).
lint: ; go vet -tags=integration ./... && staticcheck -tags=integration -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./...

# 安全：gosec（安全扫描）+ govulncheck（依赖漏洞）——两者都必须零命中。
sec: ; gosec -quiet ./... && govulncheck ./...

# 测试：-race 需要 CGO / C 编译器（与 build 目标的 CGO 状态相反，故分属不同目标）。
test: ; go test -race -cover ./...

# 集成冒烟：build tag 隔离的用例默认不执行，必须单列一步。缺 DSN 由前置闸门判失败，
# 不让测试层"全部跳过"冒充通过。配方**保持 ASCII**（本机 make 按 ANSI 读 Makefile）。
# A missing DSN fails the guard before any test runs; the recipe stays ASCII.
test-integration: ; @test -n "$${BASALT_TEST_PG_DSN:-}" || { echo "test-integration: BASALT_TEST_PG_DSN is not set (fail-closed per D-01; skipping is not allowed). Start the dev-env test database and run through dev-env/scripts/with-postgres.sh; setup: docs/DevelopmentLoop.md section 10." >&2; exit 1; }; go test -tags=integration -count=1 ./...

# 覆盖率底线：go test -cover 只打印百分比、不会因低于阈值而失败——由脚本按包判失败。
# 判卷同时跑 `-tags=integration`（覆盖率口径含 PG 存储），故脚本自身也要求 DSN。
# The judge also runs with -tags=integration, so it requires the DSN as well.
cover: ; sh scripts/check-coverage.sh

# 发布构建：单二进制纪律（CGO_ENABLED=0）；版本标识经 -X 注入入口包。
# 配方里的 `$$(...)` 必须双写——单写会被 Make 当变量引用展开为空。
#
# 产物名按宿主补可执行后缀（`go env GOEXE`：Windows 为 .exe，其余为空）：Windows 的
# os/exec 只按 PATHEXT 解析带扩展名的目标，无扩展名的 PE 文件会报
# "executable file not found in %PATH%"——e2e 直接执行候选产物，故名字必须能被执行。
# The artifact name carries the host executable suffix (go env GOEXE); os/exec on
# Windows only resolves names with a PATHEXT extension.
EXE ?= $(shell go env GOEXE)
BIN ?= bin/basalt$(EXE)

build: ; CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$$(git describe --tags --always)" -o $(BIN) ./cmd/basalt

# 示例单测：docs-readonly 的 Go 测试（主 module 内）+ 五个 Demo 的业务脚本（缺解释器 = 失败，不是跳过）。
# 二者都**不**进 check / acceptance / acceptance.json / CI——它们是示例与发布期的独立入口。
example-tests: ; go test -count=1 ./examples/shared/mcp/docs-readonly && $(PYTHON) scripts/run-example-tests.py

# 发布 e2e：候选二进制取自 build 的同一份产物（不另建），以绝对路径交给 TestMain。
# `-json` 不可省：`go test` 在包通过且无 `-v` 时会吞掉包级 stdout，`binary_sha256=` /
# `binary_source=candidate` 标记随之消失（实测：e2e 全绿但 stage 判「found 0」）。
# -json is required: a passing package without -v swallows package stdout, including the marker.
# Whole-package watchdog only; individual test deadlines and assertions are unchanged.
E2E_TIMEOUT ?= 10m

e2e: build example-tests
	@log=$$(mktemp); \
	if BASALT_E2E_BINARY=$(CURDIR)/$(BIN) go test -json -tags=e2e -count=1 -timeout=$(E2E_TIMEOUT) ./internal/e2e > $$log 2>&1; then \
		cat $$log; \
		grep -q 'binary_source=candidate' $$log || { echo "e2e: binary_source=candidate missing (release evidence must run the built candidate)" >&2; rm -f $$log; exit 1; }; \
	else \
		code=$$?; cat $$log; rm -f $$log; exit $$code; \
	fi; \
	rm -f $$log
