# US-16A Validation Quickstart

## Environment

Windows 在仓库根执行 `. .\dev-env\scripts\Enter-DevEnv.ps1`，只注入环境，不打印凭据。Go 1.27.3、Node 24.22.0、真实本地 PostgreSQL。WSL 使用 `dev-env/scripts/with-postgres.sh <command>`；BASALT_TEST_PG_DSN不可输出。

## Commands

```sh
sh scripts/check-acceptance.sh --lint-stages --stage harness/stages/us16a.json
sh scripts/check-acceptance.sh --stage harness/stages/us16a.json --task T004
make check
make acceptance
make e2e
```

完整US阶段含真实回环Collector，须显式带已有授权引用与测试范围（不授权外部模型或观测后端）。PowerShell先设置本轮自有工件路径，随后跑全部阶段断言：

```powershell
$spike = (Get-Content .loop/obs-spike-location.json -Raw | ConvertFrom-Json).path
$env:BASALT_OBS_TEST_COLLECTOR_BINARY = Join-Path $spike 'collector/otelcol.exe'
$env:BASALT_OBS_TEST_COLLECTOR_EVIDENCE = Join-Path (Get-Location) ('.loop/obs-walkthrough-' + [guid]::NewGuid().ToString('N'))
& 'C:\Program Files\Git\bin\bash.exe' scripts/check-acceptance.sh --stage-only --stage harness/stages/us16a.json --executor user --run-kind rerun --authorization-ref user-2026-11-01-otlp-spike-download --authorization-scope 'Owned loopback Collector 0.164.0 and synthetic PostgreSQL only; no external backend or real model.'
```

不带`--stage-only`会连同公共full profile重跑；单独`make acceptance`只证明公共检查。Collector测试缺指定二进制/证据目录即失败，不会SKIP。阶段二进制指纹属于实际Go测试程序，发布候选指纹由`make e2e`单列记录。

前端在 assets/console 使用既有 npm test / npm run build；dist和integrity同批更新。Collector只监听127.0.0.1，使用合成数据，首次下载在已批准scope；真实模型/外部后端不使用。现有8080演示实例不触碰，走查候选在隔离工作区与独立端口准备。

## Evidence

所有报告在 .loop/runs 下；任务/失败/裁决记录见 run-state.md。最终源码稳定窗口绑定候选二进制SHA-256。没有实际运行的项保持未验证；不要引用US16旧报告证明US16A。
