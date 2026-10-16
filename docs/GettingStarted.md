# Basalt 起步手册：从零到第一个 US 跑通

本手册面向**本仓库的开发者**：从一台没装任何 Go 工具链的机器开始，到「仓库初始化 → 工程地基交付 → 逐 US 开发循环」全部跑起来。按顺序手动执行即可。

**本文的边界**（避免与其它文档职责重叠）：

| 文档 | 面向 | 管什么 |
| --- | --- | --- |
| **本手册** | 本仓库开发者 | 开发机环境、仓库初始化、三步链开发循环 |
| [Operations.md](Operations.md) | 部署与运维 | 交付后怎么装、看、备份、升级、管凭证 |
| [AiProgrammingGuide.md](AiProgrammingGuide.md) §4 / §5 | AI 与开发者 | 每个 US 具体做什么、完成的定义 |
| README「快速开始」（US-13 交付） | 拿到 basalt 的使用者 | 干净检出 → 编译 → `init` → 跑起来 |

**事实源**：门禁命令与三条实测纪律取自 [AiProgrammingGuide.md](AiProgrammingGuide.md) §3.2；工具链版本取自 [TechnicalSolution.md](TechnicalSolution.md) §1.2（2026-10-14 经模块代理实测）；US 顺序与依赖取自 [AiProgrammingGuide.md](AiProgrammingGuide.md) §1.3；`git init` 由用户执行的口径取自 [Roadmap.md](Roadmap.md) §4.4。

**标注约定**：`**实测（日期）**` = 在开发机上真跑过；`**待实测**` = 命令形态有依据但未在本机验证，首次执行时以实际输出为准并回写本手册。

## 0. 步骤总览

| # | 步骤 | 谁执行 | 产物 / 完成判据 |
| --- | --- | --- | --- |
| 1 | 开工前定三件事（模块路径 / 远程仓库 / 是否装 GCC） | 你 | 三个答案，写进第 1 节的表 |
| 2 | 装工具链（Go 1.27.2 / make / GCC / 四个 Go 工具） | 你 | 第 2.8 核对表逐条通过 |
| 3 | 仓库初始化（`git init` + 三个仓库级配置文件） | 你 | 首个 commit 落地、`git status` 干净 |
| 4 | 原型五链路（**已决：分阶段执行**，不阻塞 US-2 / US-3；见第 4 节） | 你与 AI | 五条链路实测结论回写 `contracts/`，契约冻结 |
| 5 | 逐 US 开发循环（三步链，US-1~US-14） | AI 实现、你裁决 | 每个 US：门禁全绿 + 七项证据 + 一个 commit |

## 1. 开工前先定三件事

这三件事会影响后面几节的具体命令，**先定再动手**。

### 1.1 模块路径（module path）

`go mod init` 要一个模块路径，它是本仓库所有 Go 代码的导入前缀，事后再改会牵动全仓 import。[AiProgrammingGuide.md](AiProgrammingGuide.md) §3.2 给的形态是 `github.com/<org>/basalt`。

- 计划托管到 GitHub → 用 `github.com/<你的 org 或用户名>/basalt`
- 计划自托管（内网 GitLab / Gitea）→ 用实际域名路径（如 `git.example.com/team/basalt`）
- **一旦定了就不要再改**：模块路径出现在每个 `.go` 文件的 import 里，也出现在 `go.mod`

### 1.2 远程仓库（决定 CI 是否生效）

CI 工作流（`.github/workflows/`，双 job：`test` 开 CGO 跑 `-race` / `build` 用 `CGO_ENABLED=0` 四平台交叉编译）是 **US-1 的交付物**。它只有在推到 GitHub 后才真正运行；不推也能开发，但本机就是唯一的门禁执行者（见 1.3）。

### 1.3 本机装不装 C 编译器（决定能否本地跑竞态检测）

门禁里的 `go test -race` **需要 CGO 与 C 编译器**（[AiProgrammingGuide.md](AiProgrammingGuide.md) §3.2 纪律 2），而发布构建要求 `CGO_ENABLED=0`——两者相反，必须分属不同命令。

| 选择 | 后果 |
| --- | --- |
| 装 GCC（推荐） | 本机 `make check` 能整条跑通，竞态检测本地即时可见 |
| 不装 GCC | `make check` 会在 `test` 目标失败；**不得**改 Makefile 去掉 `-race` 让它变绿（那是静默取消竞态检测）——只能如实登记为"本机未验证"，把竞态检测留给 CI 的 `test` job（Linux runner 自带 gcc） |

### 1.4 定案记录（2026-10-15）

| # | 事项 | 定案 | 落到哪 |
| --- | --- | --- | --- |
| 1 | 模块路径 | `github.com/ymedword/basalt` | US-1 的 `go mod init` 入参；全仓 import 前缀，**定了不再改**（1.1） |
| 2 | 远程仓库 | GitHub · **Public** · 账号 `ymedword` · 仓库名 `basalt` | §3.3 的 `git remote add origin https://github.com/ymedword/basalt.git`；推到 GitHub 后 CI 生效（1.2） |
| 3 | 本机装 C 编译器 | **装**（MSYS2 + mingw-w64 gcc，见 2.4） | 本机 `make check` 整条可跑通，`-race` 本地即时可见（1.3） |

**第 2 / 3 步的执行分工（2026-10-15 定案，同日修订）**：初次定案为「全部由用户执行」；用户随后**明确指令改为由 AI 执行第 2、3 步**——工具链安装、`git init`、三个仓库级配置文件与首个 commit 已由 AI 在本机执行完毕（代跑均由用户指令逐次触发，与 CLAUDE.md「禁止**自动** commit / push / tag」不冲突）。远程仓库由用户在 GitHub 网页创建（`ymedword/basalt`）；**经用户明确授权，`push` 亦已执行**（2026-10-15：`main` → `origin/main`，本地分支已跟踪）。此后每个 US 的 commit 仍按原口径由用户执行，这两次授权**不构成**对后续的默认授权。

## 2. 环境准备（你执行）

### 2.1 现状盘点（本机实测，2026-10-15）

在这台开发机上逐项探测的结果：

| 工具 | 现状 | 说明 |
| --- | --- | --- |
| `git` | **已装** `/mingw64/bin/git` 2.53.0.windows.2 | Git Bash 随 Git for Windows 提供 |
| `winget` | **已装** v1.29.380 | 本节多数安装走它 |
| `go` / `gofmt` | 未装 | 见 2.2（`gofmt` 随 Go 一起装） |
| `gofumpt` / `staticcheck` / `gosec` / `govulncheck` | 未装 | 见 2.5（`go install`） |
| `make` | 未装 | 见 2.3 |
| `gcc` / `cc` | 未装 | 见 2.4（`-race` 需要） |
| `node` / `npm` | 未装 | **核心阶段 US-1~US-10 不需要**，见 2.7 |

### 2.2 装 Go 1.27.2

⛔ **不要用 `winget` 装 Go**——**实测（2026-10-15）**：winget 源上的 `GoLang.Go` 当前是 **1.27.0**，比本项目的**构建版本 1.27.2** 旧（[TechnicalSolution.md](TechnicalSolution.md) §1.2）。

从官方下载页 <https://go.dev/dl/> 取 **`go1.27.2.windows-amd64.msi`**，双击按默认选项装完，然后**重开一个 Git Bash**（MSI 会写系统 PATH，已开着的终端看不到），验证：

```bash
go version
```

期望输出：`go version go1.27.2 windows/amd64`。低于 1.27.2 → 卸载重装，不要将就；更新的版本也可用（`go.mod` 的 `go` 行只是**最低版本要求**，准确构建版本由 `toolchain` 指令与 CI 工具链配置固定），但**本机与 CI 一致**最省事、构建结果可复现。

### 2.3 装 make

门禁一条命令 `make check` 靠它。**实测（2026-10-15）**：winget 源上可用的 GNU Make 有两个，选新的那个：

```bash
winget install --id ezwinports.make --exact --accept-source-agreements
```

装完重开 Git Bash，验证：

```bash
make --version
```

期望输出首行含 `GNU Make 4.4.1`。

> 另一个 `GnuWin32.Make` 是 **3.81**（2006 年版本），**不要**装——新版 Makefile 语法（`$(...)` 递归展开、`||` 组合）在老版本上行为不一致。

### 2.4 装 GCC（`-race` 需要，见 1.3）

**待实测**：以下为 MSYS2 官方路径的命令形态，装完必须用 2.6 的探针验证。

```bash
winget install --id MSYS2.MSYS2 --exact --accept-source-agreements
```

MSYS2 装到 `C:\msys64`。在它自己的环境里装 mingw-w64 的 GCC：

```bash
/c/msys64/usr/bin/bash -lc 'pacman -S --needed --noconfirm mingw-w64-ucrt-x86_64-gcc'
```

装完**把 GCC 目录加进 Git Bash 的 PATH**——追加到 `~/.bashrc` 末尾（先跑一次 `/c/msys64/ucrt64/bin/gcc --version` 确认该路径存在，MSYS2 若装在别处就换成实际路径）：

```bash
export PATH="$PATH:/c/msys64/ucrt64/bin"
```

重开 Git Bash，验证：

```bash
gcc --version
```

期望输出首行含 `gcc (Rev..., Built by MSYS2 project) ...`。

> 备选（一步拿到 gcc + make，省掉 MSYS2 两步）：`winget install --id BrechtSanders.WinLibs.POSIX.UCRT --exact`（**实测搜索到** 16.2.0-14.0.0-r1）。本手册主推 MSYS2——它是 Windows 上 GCC/mingw-w64 的事实标准，后续要别的 Unix 工具也顺手。

**若你日常在 PowerShell / cmd 里跑命令**（而不是 Git Bash）：`~/.bashrc` 里的 PATH 对它不生效，需要改 Windows 用户环境变量（`Win+R` → `sysdm.cpl` → 高级 → 环境变量 → 用户变量 `Path` → 新建一行填绝对路径）。**不要**用 `setx PATH ...` 整串覆盖——那会把系统 PATH 截断。

### 2.5 装四个 Go 工具（`go install`）

> **顺序提醒（2026-10-15 实测踩到）**：`go install` 要走模块代理，而本机 `proxy.golang.org` 不可达（见 2.6）——**必须先执行 2.6 的第一条**（`go env -w GOPROXY=...`）再回到本节，否则四个工具全部安装失败。

版本全部取自 [TechnicalSolution.md](TechnicalSolution.md) §1.2（2026-10-14 经 `goproxy.cn` 实测）：

```bash
go install mvdan.cc/gofumpt@v0.12.0
go install honnef.co/go/tools/cmd/staticcheck@v0.8.2
go install golang.org/x/vuln/cmd/govulncheck@v1.9.0
# gosec **不得**用 `go install ...@v2.29.0` 装——见下方「gosec 的构建配方」。
```

#### gosec 的构建配方（2026-10-16 实测踩到）

`go install github.com/securego/gosec/v2/cmd/gosec@v2.29.0` 装出来的二进制**在 Go 1.27 上读不了导出数据**：它带的 `golang.org/x/tools` **v0.49.0** 最高只认导出数据版本 4，而 go1.27.2 的编译器**无条件**写版本 5（源码读数：`$(go env GOROOT)/src/cmd/compile/internal/noder/unified.go` 的 `const uirVersion = pkgbits.V5`，对应 `pkgbits` 的 `V5 Version` 条目）。

后果不是报错停下，而是**静默降级**——扫描器只按语法树判，类型相关规则整批失效。2026-10-16 的探针读数（三处明摆着的违规：`uint32(int)` 窄化、`os.ReadFile` 变量路径、`0644` 写入）：

| 扫描器 | 探针命中 | 仓库命中 | 「无法解析导出数据」报错 |
| --- | --- | --- | --- |
| `go install` 装出的 v2.29.0（x/tools v0.49.0） | **1 / 4** | 不报错也不命中——**读数不可信** | 探针侧 3 条；仓库侧时有时无 |
| 按下方配方构建（x/tools v0.50.0） | **4 / 4** | 0 命中（本轮修复后） | 零 |

结论一句话：**命中数不能证明扫描完整**——同一个二进制既会打印「cannot decode … version 5 is greater than maximum supported version 4」，也会一声不吭地漏掉三条规则。配方固定 `gosec v2.29.0 + golang.org/x/tools v0.50.0`，在任意临时模块里构建：

```bash
mkdir -p "$(go env GOPATH)/tmp/gosec-build" && cd "$(go env GOPATH)/tmp/gosec-build"
go mod init basalt-gosec-build
go get github.com/securego/gosec/v2/cmd/gosec@v2.29.0
go get golang.org/x/tools@v0.50.0
go build -o "$(go env GOPATH)/bin/gosec$(go env GOEXE)" github.com/securego/gosec/v2/cmd/gosec
```

装完的**来源判据**（两道都要看，`Version: dev` 是 `go build` 的正常读数）：

```bash
go version -m "$(go env GOPATH)/bin/gosec$(go env GOEXE)" | grep -E "gosec/v2|x/tools"
# 期望两行：mod github.com/securego/gosec/v2 v2.29.0 …／dep golang.org/x/tools v0.50.0 …
```

它们装进 `$(go env GOPATH)/bin`（默认 `C:\Users\<你>\go\bin`）。**这个目录不在默认 PATH 里**，先查出实际路径：

```bash
go env GOPATH
```

把 `<上面输出的路径>/bin` 追加到 `~/.bashrc`（**写绝对路径，不要每次开 shell 都调 `go env`**，那会拖慢启动）：

```bash
export PATH="$PATH:/c/Users/<你>/go/bin"
```

重开 Git Bash，逐个验证：

```bash
gofumpt -version && staticcheck -version && gosec -version && govulncheck -version
```

> 版本纪律：`go install` 报 `not found` 时，**不得**凭记忆改版本号——先跑 `GOPROXY=https://goproxy.cn go list -m -versions <模块路径>` 核实可用版本，再与 [TechnicalSolution.md](TechnicalSolution.md) §1.2 对照后决定。

### 2.6 Go 环境变量与一次探针验证

**设模块代理**（**实测**：本机 `proxy.golang.org` 不可达，见 [TechnicalSolution.md](TechnicalSolution.md) §1.2 注）：

```bash
go env -w GOPROXY=https://goproxy.cn,direct
go env GOPROXY        # 回读确认
```

`go env -w` 把设置写进 Go 自己的环境文件，对本机所有项目持久生效——**不是**仓库配置，不进 git。

**探针（同时验证 GOPROXY 与 `-race`）**：在仓库之外建一个一次性小工程，确认工具链真能编译、真能跑竞态检测：

```bash
mkdir -p ~/raceprobe && cd ~/raceprobe
go mod init raceprobe
printf 'package raceprobe\n\nfunc Add(a, b int) int { return a + b }\n' > add.go
printf 'package raceprobe\n\nimport "testing"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal("Add(1, 2) != 3")\n\t}\n}\n' > add_test.go
go test -race ./...
```

期望输出含 `ok` 与包名 `raceprobe`（`go test` 的输出列之间有制表符，这里只判这两个词都在即可）。跑完删掉：`cd ~ && rm -rf ~/raceprobe`。

| 探针报错 | 含义 | 回到哪一步 |
| --- | --- | --- |
| `cgo: C compiler "gcc" not found` | `-race` 缺 C 编译器 | 2.4（装 GCC 并确认 PATH） |
| `dial tcp ... proxy.golang.org` | 代理没设上 | 本节第一条命令 |
| `gofmt` 相关报错 | Go 装错了版本 | 2.2 |

### 2.7 现在**不要**装的东西

| 工具 | 为什么现在不需要 |
| --- | --- |
| Node.js / npm | 只在 **US-11**（管理台，Vue + Vite 产物 `go:embed`）与 **US-13**（官网 VitePress）需要，且**仅构建期**（[TechnicalSolution.md](TechnicalSolution.md) §1.2 / §7.4）。到那两个 US 时按颗粒度文档装，并用 `npm registry` 核实版本 |
| Python | 本项目开发不需要。Python 只在「某个 Agent 用 `shell` 工具跑 `scripts/*.py`」时才需要——那是运行时/演示侧，不是开发机前置（[Operations.md](Operations.md) §1.2） |
| `sqlite3` CLI | 只有在线备份需要（[Operations.md](Operations.md) §1.2） |
| Docker / 数据库 / 消息队列 | 底座是单二进制 + 内嵌 SQLite，零外部服务（[Operations.md](Operations.md) §1.1） |

### 2.8 环境完成判据

逐条复制执行，**全部有期望输出**才算就绪：

```bash
go version            # go version go1.27.2 windows/amd64
make --version        # GNU Make 4.4.1
gcc --version         # gcc (MSYS2 ...)
gofumpt -version      # v0.12.0
staticcheck -version  # 2025.x（以实际为准，能打印版本即可）
gosec -version        # Version: dev（**实测**：`go install` 装法不含 ldflags 版本注入；真实模块版本用 `go version -m` 反查 = v2.29.0，见 §8）
govulncheck -version  # Go: go1.27.2 / Scanner: v1.9.0
go env GOPROXY        # https://goproxy.cn,direct
```

**另需**：2.6 的 `-race` 探针输出过 `ok raceprobe`。

## 3. 仓库初始化（你执行）

### 3.1 `git init` 与三个仓库级配置文件

**`git init` 由你执行**——AI 不得自行初始化仓库（[CLAUDE.md](../CLAUDE.md) 硬性禁止；[Roadmap.md](Roadmap.md) §4.4 把它列为准备阶段第 0 步）。

```bash
cd /d/code/basalt
git init -b main
```

接着在仓库根建三个文件（内容建议值如下；`git init` 之后立即做，**早于**第一个 commit——行尾问题一旦入库，后面每个 US 的格式门禁都会对本机文件误报）。

**`.gitattributes`**（行尾钉死 LF）：

```gitattributes
# 默认：入库与检出统一 LF——gofmt 要求 LF，CRLF 会让格式门禁永远报"未格式化"
* text=auto eol=lf
# 例外：批处理脚本必须 CRLF（cmd.exe 对 LF 结尾的 .bat / .cmd 会出问题）
*.bat text eol=crlf
*.cmd text eol=crlf
```

为什么需要它：Git for Windows 默认 `core.autocrlf=true`，会把检出到工作区的文本文件转成 CRLF，而 `gofmt -l` 会把 CRLF 判成格式差异——于是「本机文件没动过，门禁却说未格式化」。`.gitattributes` 的优先级高于 `core.autocrlf`，写进去就与本机设置无关了。

**`.gitignore`**：

```gitignore
# 运行时工作区（`basalt init` 生成：SQLite 库 / 记忆 / 日志 / Agent 目录）——是数据，不入库
.basalt/

# 构建产物
/bin/
*.exe

# 本地环境文件（凭证一律走 ${ENV_VAR}，明文不得落盘——万一写下也绝不入库）
*.local.env
.env
```

**`.gitmessage`**（提交信息模板，规范原文见 [EngineeringStandards.md](EngineeringStandards.md) §5；[AiProgrammingGuide.md](AiProgrammingGuide.md) §8.2 第 4 条要求「模板入库」）：

```text
<type>(<scope>): <subject>

**Problem:**
<English: why this change is needed — symptom / trigger / root cause>

<中文：为什么需要这次改动——现象 / 触发场景 / 根因>

**Design:**
<English: what and how — key decisions, referenced principles / documents>

<中文：改了什么、怎么改——关键决策，引用的原则 / 文档章节>

**Testing:**
<English: how it was verified — commands run this round and their results; state what was NOT verified>

<中文：如何验证——本轮当场跑过的命令与结果；未验证 / 未覆盖处如实写明>

**Impact:**
<English: scope — packages / contracts / dependencies / compatibility / follow-ups>

<中文：影响范围——涉及包 / 契约 / 依赖 / 兼容性、后续项>
```

标题**英文**祈使句 ≤ 72 字符；正文四段**逐段对照**（先英文段、空行、再中文段）；`<type>` 取 `feat` / `fix` / `docs` / `refactor` / `test` / `chore` / `ci` / `build` / `perf` / `revert`。完整填写说明见 `.gitmessage` 内的 `#` 注释。挂上模板：

```bash
git config commit.template .gitmessage    # 仓库级；此后 git commit 自动带出模板
```

### 3.2 首个 commit

```bash
git add -A
git status --short | wc -l        # 先看清要提交多少文件，不该有 .basalt/ 或二进制
git commit                        # 打开编辑器，模板已带出
```

首个提交信息建议：`chore: initialize repository with phase-1 docs and project skills`。

⚠ **此后每个 US 的 commit 也由你执行**——AI 全程不自动 commit / push / tag（[AiProgrammingGuide.md](AiProgrammingGuide.md) §8.2 第 4 条）。

### 3.3 远程仓库与 CI（可选）

**已执行（2026-10-15）**：`origin` = `https://github.com/ymedword/basalt.git`（用户在 GitHub 网页创建仓库后授权推送）；`main` 已推至 `origin/main`，本地分支已跟踪。参考命令：

```bash
git remote add origin https://github.com/ymedword/basalt.git
git push -u origin main
```

推送按**单次**凭证助手执行（`git -c credential.helper= -c credential.https://github.com.helper='!gh auth git-credential' push -u origin main`），**未向 git 配置写入任何凭证**；以后本机被要求登录时先跑一次 `gh auth setup-git`。

推到 GitHub 后，**US-1 交付的 CI 工作流**才会真正开跑。在此之前没有 CI，本机 `make check` 是唯一门禁执行者。

### 3.4 完成判据

```bash
git log --oneline              # 一行：首个 commit
git status --short             # 空
git ls-files --eol             # 看 w/ 列：应为 lf（.bat / .cmd 例外；当前仓库没有这两个后缀）
git check-attr text -- docs/TechnicalSolution.md   # 实测显示 text: auto（`* text=auto eol=lf` 的读回值；要 `set` 需写 `* text`，会把二进制也当文本——判据以实测的 auto 为准，见 §8）
```

## 4. 开工前的前置项：原型五链路（已决：分阶段执行）

这一节是**交叉点**——它是原定的「写第一行业务代码前」硬项，但所需能力分散在多个 US；执行时机**已决**（2026-10-15，口径丙，见本节文末裁决记录）——**改为分阶段执行，不阻塞 US-2 / US-3 开工**。

**是什么**：用最小原型跑通五条运行时链路——多轮工具调用（含并列工具调用的 `tool_call_id` 回填）、同会话并发（409 / 429）、执行取消（杀进程树、会话可继续）、运行中热加载（不出现新旧混搭）、崩溃后恢复（`kill -9` 重启后 `running` → `failed`、`pending` → `unknown`，且恢复可重入）。完整清单与验收口径见 [AiProgrammingGuide.md](AiProgrammingGuide.md) §3.4 与 [DemandAnalysis.md](DemandAnalysis.md) §13.5。

**为什么重要**：`contracts/` 里的运行时契约四件套（Run 契约 / 完整消息模型 / 配置快照语义 / `Policy` 与 `CommandRunner` 边界）在五链路通过之前，状态是**待验证设计草案**，不得当成已冻结使用；通过后实测结论（尤其偏离契约之处）要**回写 `contracts/`**（[AiProgrammingGuide.md](AiProgrammingGuide.md) §3.3 行 174/175）。这一条**不因分阶段执行而改变**——变的是**执行时点**（分阶段，不构成 US-2 / US-3 的开工阻断），不是"可以不当回事"。

**交叉点**：这五条链路要用到的能力分散在各 US（按 [AiProgrammingGuide.md](AiProgrammingGuide.md) §4 各 US 的 task 清单核对）——链路 1（多轮工具调用）与链路 3（取消：`AgentService.Run` 的取消与超时、Runner 杀进程树）在 **US-4**；链路 4 的**快照语义**（每次运行重建、运行内冻结）在 **US-4**（文件监听 fsnotify 到 US-11，但链路 4 的验证不依赖监听——改完文件跑下一次即可）；链路 2 的**会话内准入**在 **US-4**、HTTP 409 / 429 在 **US-9**；链路 5 依赖 US-4 的两阶段审计写，其**恢复流程与实例锁在 US 序列里尚无明确归属**。所以「写第一行代码之前就全部验证」字面上不可行——五条链路全部可验的最早时点是 **US-4 交付后**（链路 2 的 HTTP 面顺延到 US-9 补齐）。三种可行口径（**已决 = 丙**）：

| 口径 | 做法 | 优点 | 代价 |
| --- | --- | --- | --- |
| **甲：US-1 交付后做独立原型冲刺** | 在仓库之外的空目录（一次性代码，不入库）按 §3.4 写最小探针跑五条链路 | 最早暴露契约问题；US-2 起都在冻结后的契约上落地，不返工 | 要写一段不入库的探针代码 |
| **乙：US-4 交付后立刻做** | 首个能跑完整循环的时点，直接用本仓真实实现验证（链路 1/3/4 与 5 当场验，链路 2 的 HTTP 面到 US-9 补齐） | 零额外代码 | US-2 / US-3 已按未冻结契约落地，契约若改须返工 |
| **丙：乙 + US-1 小探针（已决）** | US-1 交付后先用不入库探针验链路 5 的恢复算法与实例锁（纯 SQLite + 锁，不碰 provider / 循环）；其余同乙 | 最早钉死最难的两处语义（恢复可重入 / 实例锁），成本远小于甲 | 探针只覆盖链路 5，其余仍等 US-4 |

**已决（2026-10-15，用户裁决）：口径丙 —— 乙 + US-1 小探针**。落地方式：

- **US-1 交付后**：写一个**不入库**的小探针（`go run` 于本仓之外的空目录，同一执行约定见 [Compatibility.md](Compatibility.md) 的对拍程序段）：①按 [contracts/run-and-errors.md](contracts/run-and-errors.md) 的恢复语义，在内存 SQLite 上手工构造反例状态（`running` 的 Run / `pending` 的审计行 / 半截工具组），验证恢复算法可重入、补组以消息为入口、事务边界正确；②验证实例锁语义（活实例持锁时第二写模式进程拒绝启动；被 `SIGSTOP` 暂停的实例不得被抢锁）。记读数，偏离契约处回写 `contracts/`。
- **US-4 交付后**：用真实实现验链路 1 / 3 / 4 / 5；链路 2 的 HTTP 409 / 429 面到 **US-9** 补齐。
- **契约状态与最终冻结条件**：四件契约在**对应链路**验证通过前保持「**待验证设计草案**」（状态头见各契约文件），**但这不是 US-2 / US-3 的开工阻断**——分阶段推进是有意为之（US-1 探针先钉死最难的两处语义，返工面压到最小）；**最终冻结条件 = 五链路全部通过 + 真实模型调用往返**，届时把实测偏差（尤其偏离契约之处）回写 `contracts/` 并更新状态头。
- **待落位**：崩溃恢复 + 实例锁的**实现**在 US 序列里尚无明确归属——**建议归 US-4**（`/module-doc-gen 4` 时定案，届时把本节结论带过去）。
- **提前触发的一项决策**：Go 标准库没有跨平台文件锁（Unix 侧 `syscall.Flock` 可用；Windows 侧需要额外手段，如 `golang.org/x/sys` 或第三方库）——探针在 Windows 上验证锁语义时，按「新增第三方坐标先问用户」纪律需先向你请示（届时列入 US-1 颗粒度文档的「待决事项」）。
- 以上内容要带进 **US-1 颗粒度文档的「已决事项」段**（`/module-doc-gen 1` 时写入；历史快照不改写的口径见 [Roadmap.md](Roadmap.md) §4.3）。

## 5. 开发循环：三步链（AI 执行，你在停点确认）

**进入本节的前置**：第 2 节核对表逐条通过 + 第 3 节首个 commit 已落地（工作区干净）。

### 5.1 总览

**怎么执行这些命令**：它们是本仓库的**项目内 skill**（`.claude/skills/`）；speckit 层（`.specify/` 与 10 个 `speckit-*` skill）已随准备阶段就位，**不需要**再装 spec-kit CLI。在**本仓库根目录**打开 Claude Code 会话（VS Code 扩展，或终端里跑 `claude`），把 `/module-doc-gen 1` 当成消息发出去即可——会话的工作目录必须是仓库根，否则项目 skill 加载不到。

```text
/module-doc-gen N   →  docs/us/us{N}-<slug>.md（六节颗粒度文档）
        ↓ 你评审
/us-register N      →  docs/us/REGISTRY.md 追加一行（+ 三项能力追加判断题）
        ↓ 你在 diff 停点确认
/module-dev N       →  specify → clarify → plan → tasks（固定软停点）→ analyze → implement → 七项证据
        ↓
commit（你手动）
```

每个 US 都是这条链，**US-1 也不例外**——工程地基的**装法**参照 `/init-foundation` skill，但流程与验收仍走 `/module-dev 1`（[CLAUDE.md](../CLAUDE.md) 工作流段）。

**分支**：本仓库**没有** `.specify/extensions.yml`（即无 `before_specify` hook），speckit 不会自动建分支——每个 US 开跑前手动建：`git checkout -b {NNN}-<slug>`（NNN 取 specs 目录序号，如 `001-foundation`）。建分支不是 commit / push，可以让 AI 建，也可以你自己建。开跑前确认工作区干净（`git status --short` 为空），中途要回滚才有退路。

### 5.2 步骤 A：`/module-doc-gen 1`

产出 `docs/us/us1-*.md`，六节结构（概述 / 设计要点与约束 / 交付物清单 / 验收测试 / 范围边界 / 验证与验收）。

**你评审什么**：交付物清单是否与技术方案对齐、harness 是否覆盖每条 FR、范围边界是否正确、文末「待决事项」里有没有需要你裁决的。这份文档是后面 `/module-dev` 对账的底单，写歪了后面全歪。

### 5.3 步骤 B：`/us-register 1`

在 `docs/us/REGISTRY.md` 追加一行（**追加，不改历史行**），并逐项问你三个「能力追加」判断题：新依赖坐标 / 新构建步骤 / 新机器可判不变式。

⚠ 这个 skill 有**固定停点**：打印 diff 后停下等你确认才落盘。三项判断题按实际答——本批移植时 Go 门禁命令、工具链句、全局不变量清单**已预写**进 `module-dev` 配置区，所以 US-1 大概率三项都是「已有，无需追加」。

### 5.4 步骤 C：`/module-dev 1`

依次执行：`specify`（从颗粒度文档 §1/§2 提炼 WHAT/WHY）→ `clarify`（有歧义就问，答案只从颗粒度文档与技术方案找）→ `plan`（固定技术栈句 + 本 US 落位）→ **`tasks` + 固定软停点** → `analyze` → `implement`。

⚠ **唯一固定停点**：`tasks` 生成后，AI 会把它与颗粒度文档「交付物清单」逐项比对，输出「齐 / 缺什么 / 多什么」，**停下等你确认**才继续——这个停点不得跳过。

⚠ **`analyze` 是实施前的阻断检查**：停点确认后先跑一次 `/speckit-analyze`，**CRITICAL 问题清零前不得进入 `implement`**（修 spec / plan / tasks，不得稀释原则）；分析结论贴进验收报告。

实现期间的硬约束（AI 侧）：写前核实第三方 API、已定字面量逐字保真、异常不吞、失败路径审计与 WARN 双轨；测试纪律：不得删断言 / 不得跳过测试 / 不得放宽阈值。

### 5.5 每个 US 收口

1. 再跑一次 `/speckit-analyze`（与实施前那次同源：这次核对**实现与文档**的一致性，不只看 spec / plan / tasks 三件）
2. 对齐**七项证据**（[AiProgrammingGuide.md](AiProgrammingGuide.md) §5）：门禁全绿 / 测试映射 / 交付物核对 / 前序回归 / 全局不变量自查 / 验收 Demo / 变更导读
3. 你手动 commit（四段双语模板：Problem / Design / Testing / Impact——规范见 [EngineeringStandards.md](EngineeringStandards.md) §5，模板见 3.1）

### 5.6 US 顺序与依赖（[AiProgrammingGuide.md](AiProgrammingGuide.md) §1.3）

```text
US-1 → US-2 → US-3 → US-4 ─→ US-5 ─→ US-9 → US-10 → US-11 → US-12 → US-13 → US-14
                            ├─→ US-6（Memory，只依赖 US-4）
                            ├─→ US-7（MCP，只依赖 US-4）
                            └─→ US-8（通知渠道，只依赖 US-4）
```

| US | 名称 | 硬依赖 |
| --- | --- | --- |
| US-1 | 工程地基（骨架 / 门禁 / 配置 / 日志 / 工作区 / 持久化） | — |
| US-2 | Provider 对接 | US-1 |
| US-3 | Agent 目录与 Profile 加载 | US-2 |
| US-4 | ReAct 循环与首个工具闭环 | US-3 |
| US-5 | Shell 与 HTTP 工具（Demo 一） | US-4 |
| US-6 | Memory（Demo 二） | US-4 |
| US-7 | MCP 集成（Demo 三） | US-4 |
| US-8 | 通知渠道 | US-4 |
| US-9 | Web Service（Demo 四 / 五） | US-4 |
| US-10 | 定时任务 | US-4 + US-9 |
| US-11 | 观测与管理台 | US-9 + US-10 |
| US-12 | Agent 动态管理 | US-11 + US-8 |
| US-13 | 官网展示主页与 README | US-12 |
| US-14 | 发布收尾（三个每日 Demo + v0.1.0） | US-13 + US-5 / 6 / 7 / 8 / 10 |

**US-15 起是扩展阶段**：US 是开放序列，编号只增不改；扩展立项走**同一条三步链**（零新 skill），见 [Roadmap.md](Roadmap.md) §4.2。台账 `docs/us/REGISTRY.md` 是唯一登记处。

## 6. 提交、版本锚点与发布形态

- **提交时机**：每个 US 收口一个 commit；提交信息用**英文** Conventional Commits 标题 + 四段**双语**正文（逐段英中对照；模板见 3.1，规范原文见 [EngineeringStandards.md](EngineeringStandards.md) §5）。AI 不自动 commit / push / tag
- **版本号只有两个载体**：构建注入（`-ldflags "-X main.version=..."`）与 git tag；**文档正文不写版本号**（写进去的版本号就是会腐烂的事实，[Roadmap.md](Roadmap.md) §4.4）
- **`v0.1.0`** = 第一阶段交付范围（US-1~US-14），打 tag 由你手动执行
- **发布形态**：`CGO_ENABLED=0` 静态单二进制，四平台交叉编译（linux/amd64、linux/arm64、windows/amd64、darwin/arm64）——由 US-1 的 CI `build` job 承载

## 7. 排障

| 现象 | 原因 | 怎么办 |
| --- | --- | --- |
| 装完 MSI 后 `go: command not found` | 装它的终端在 PATH 写入之前就开着 | 重开 Git Bash（或重开 VS Code 让集成终端重启） |
| `winget` 装到的 Go 是 1.27.0 | winget 源版本落后（**实测**） | 卸载，改从 <https://go.dev/dl/> 装官方 MSI（2.2） |
| 没有管理员权限，装不了 MSI | — | 下载同版本的 `.zip`，解压到任意目录，把 `<解压目录>\go\bin` 加进 PATH |
| `go test -race` 报 `C compiler "gcc" not found` | `-race` 走 cgo，需要 C 编译器 | 装 GCC（2.4）并确认它在 PATH 里 |
| `gofumpt: command not found` | `$(go env GOPATH)/bin` 不在 PATH | 2.5 最后一步 |
| `go mod download` 连不上、报 `proxy.golang.org` | 代理没设 | `go env -w GOPROXY=https://goproxy.cn,direct`（2.6） |
| `go install` 报 `not found` | 版本号可能已不存在 | 先 `GOPROXY=https://goproxy.cn go list -m -versions <模块>` 核实，**不得**凭记忆改版本 |
| `gofmt -l` 对本机文件常报"未格式化"，文件看着没问题 | 行尾被转成 CRLF | 建 `.gitattributes`（3.1），然后 `git add --renormalize .` 重整索引 |
| 在 Git Bash 里 `make` 跑了，配方却像被 cmd.exe 解释（`$(...)` 报了奇怪的错） | 原生 make 找不到 `sh.exe`，退回 cmd | **待实测**：在 Makefile 顶部加 `SHELL := /bin/sh`；US-1 交付 Makefile 时必须在本机实跑一次 `make check` |
| 门禁"全绿"但其实没检 | 手写了 `gofmt -l . && gofumpt -l .` 这类串联（格式差异时退出码为 0） | 判卷顺序是**先查退出码、再判输出**——只跑 `make check`（Makefile 的 `fmt` 目标已按此写），不手拼门禁命令 |
| `make check` 里 `sec` 目标失败 | `gosec` / `govulncheck` 真发现了问题或没装 | 修问题或回 2.5；**不得**把 `sec` 从 `check` 里摘掉 |

## 8. 本手册的验证状态（如实登记）

**已实测（2026-10-15，本机）**：

- 本机无 Go / make / gcc / node 工具链；`git` 2.53.0.windows.2 与 `winget` v1.29.380 可用（2.1 表）
- winget 源读数：`GoLang.Go` = **1.27.0**（与要求的 1.27.2 不一致）；`ezwinports.make` = 4.4.1；`GnuWin32.Make` = 3.81；`MSYS2.MSYS2` = 20260611；`BrechtSanders.WinLibs.POSIX.UCRT` = 16.2.0-14.0.0-r1

**已实测（2026-10-15，第 2 / 3 步首次执行；本次由 AI 按用户明确指令代跑，见 1.4）**：

- Go：`go version go1.27.2 windows/amd64`——MSI 从 <https://go.dev/dl/> 下载，sha256 与 go.dev 官方下载 JSON 记录一致（66,154,496 字节）；装到 `C:\Program Files\Go`（2.2）
- make：`GNU Make 4.4.1`——winget 装到 `…\WinGet\Packages\ezwinports.make_…\bin`，已写入用户 PATH（2.3）
- GCC：`gcc.exe (Rev5, Built by MSYS2 project) 16.1.0`——MSYS2 20260611 装到 `C:\msys64`，pacman 装 `mingw-w64-ucrt-x86_64-gcc 16.1.0-5`（binutils 首次下载遇 mirror 超时，pacman 自动重试成功）；`ucrt64/bin` 已追加进 `~/.bashrc`（2.4）
- 四个 Go 工具（`go version -m` 反查模块版本）：gofumpt `v0.12.0`；staticcheck `2026.2.2 (0.8.2)`；gosec **`Version: dev`**（**偏离 2.8 原期望的 `2.29.0`**——`go install` 装法不带 ldflags 注入；模块版本反查 = `v2.29.0`，**版本以模块坐标为准**）；govulncheck `Go: go1.27.2` / `Scanner: govulncheck@v1.9.0`（2.5）
- `-race` 探针：输出 `ok raceprobe`——CGO 与 MSYS2 gcc 配合成立（2.4 的 **待实测** 消项）；`go env GOPROXY` = `https://goproxy.cn,direct`（2.6）
- 顺序陷阱：`go install` 必须先设 `GOPROXY`（2.5 已加提醒）——按原文顺序（2.5 在 2.6 前）在本机跑不通
- `~/.bashrc` 原不存在：Git Bash 的 `/etc/profile.d/bash_profile.sh` 会自动生成 `~/.bash_profile` 来加载它（首次新开 shell 打印一行红色 WARNING，属预期）
- 系统级 `/etc/gitconfig` 原已设 `core.autocrlf=true` 与 `init.defaultBranch=master`——3.1 的 `.gitattributes` 必要性成立，`git init -b main` 的 `-b` 不可省
- 第 3 节：`git init -b main` + 三个仓库级配置文件 + 首个 commit 落地（53 文件 / 12,033 行）；`git status --short` 空；`git ls-files --eol` 的 w/ 列全为 lf（唯一非 lf = 二进制 png，属预期）；spec-kit CLI 生成的 5 个 `.specify/` JSON **原为 CRLF**，已规整为 LF（3.1 / 3.2 / 3.4）
- `git check-attr text -- docs/TechnicalSolution.md` 实测返回 **`text: auto`**：3.4 原文期望的 `text: set` 与 3.1 给定的 `.gitattributes` 内容（`* text=auto eol=lf`）对不上——`auto` 才是该内容的读回值，改成 `set` 会把二进制也按文本处理（错误方向），**判据以实测为准**（3.4 已注）

**已实测（2026-10-16，US-4 收口复核）**：

- **gosec 重装（2.5 配方修正）**：原 `go install ...@v2.29.0` 的二进制带 `x/tools v0.49.0`，在 Go 1.27 上读不了导出数据（探针读数 4 条违规只报 1 条、仓库侧时而不报错也不命中）→ 按 2.5 配方重建为 `gosec v2.29.0 + x/tools v0.50.0`（`go version -m` 两行读数已核；探针 4/4、仓库 0 命中且零解码报错）
- `make check` 整条：修 sec 之前读数 **exit 2**（`sec` 目标失败，gosec 5 条：G101×2 / G304×2 / G306×1）；修复后 **exit 0**（七目标全绿，`govulncheck` 报 `No vulnerabilities found.`）
- 11 条全局不变量复核中，不变量 7（禁用进程本地时区）**一度判红**：旧判据只查 `time.Local` 字面量，3 处命中于 `internal/cli/session_test.go` 夹具；`internal/cli/session.go` 渲染路径的 `.Local()` 是同一条禁令的另一种写法、**躲过了旧判据**（用户裁决：判据扩展 + 实现改 UTC，不设例外、不收窄范围）。修复后按扩展判据（并入 `.Local()` 与 `time.Now().Location()`）复核**零命中**；测试改用 UTC 夹具 + 一个显式非 UTC 固化时区夹具验证换算

**仍未实测（首次执行时以实际输出为准，并把读数回写本节）**：

- 原生 make 在 Git Bash 下调用配方的 shell 归属（第 7 节倒数第四行）
- 推送到 GitHub 后的 CI（`.github/workflows/` 属 US-1 交付物，尚未交付）

**已决（2026-10-15，用户裁决）**：第 4 节原型五链路的执行时机 = **口径丙**（乙 + US-1 小探针，见 §4 文末裁决记录）——**分阶段执行，不阻塞 US-2 / US-3 开工**。
