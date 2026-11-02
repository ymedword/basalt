# Tasks: 制品与版本发布

**Input**: `specs/017-release-artifacts/plan.md`、`spec.md`、`data-model.md`、`contracts/release-cli.md`

**Tests**: 本 US 明确要求自动化验收；测试任务与对应实现同批落地，关键回归先写红态。

**Organization**: 按 17A（构建/打包/签名/校验）、17B（汇总/记录/SBOM/CI/验证器）、
17C（升级/恢复/私有快照/收口）三个切片推进。

## Phase 1: Setup（17A 发布主链路）

- [x] T001 建立 `scripts/releasekit/types.go`、`inputs.go` 与 `inputs_test.go`：版本 / tag / mode / 脏工作区校验，`TestUS17_Version_RejectsDirty`
- [x] T002 实现 `scripts/releasekit/build.go` 与测试：`CGO_ENABLED=0`、`-trimpath`、版本注入、buildinfo、`HostMatches`
- [x] T003 实现 `scripts/releasekit/package.go` 与 `package_test.go`：确定性 `.zip`/`.tar.gz`、`MANIFEST.json`、full/binary 档、路径与内容分离扫描（`TestUS17_Package_*`）
- [x] T004 实现 `scripts/releasekit/checksum.go`、`sign.go`、`verify.go` 与测试：`SHA256SUMS`、Ed25519、篡改/缺签名/无签名预演（`TestUS17_Sign_*`、`TestUS17_Verify_*`）
- [x] T005 实现 `scripts/releasekit/evidence.go`、`matrix.go`、`manifest.go`、`record.go` 与测试：证据、支持矩阵、签名顺序、记录无秘密（`TestUS17_Record_*`、`TestUS17_Assemble_RequiresFullEvidence`）
- [x] T006 实现 `scripts/releasekit/sbom.go`、`provenance.go` 与测试：CycloneDX、前端/runtime 覆盖、确定性（`TestUS17_SBOM_*`）
- [x] T007 实现 `scripts/releasekit/release.go`、`main.go`：`preflight/build/package/verify-package/assemble/verify/repro` 子命令与 `RunRelease`/`AssembleRelease`
- [x] T008 在 `scripts/release-package` 增加 `CheckReleaseBinaryForTarget` 与 `config_test.go`：读取 buildinfo 判定目标平台（`TestCheckReleaseBinaryForTarget_ReadsBuildInfo`）

## Phase 2: 17B 汇总、验证器与 CI

- [x] T009 实现 `cmd/basalt-verify/main.go`：仅下载目录校验，退出码 0/1/2
- [x] T010 新增 `release/package-VERIFY.txt`、`release/notes/TEMPLATE.md`、`release/support-matrix.json`、`release/go-licenses.json`
- [x] T011 新增 `.github/workflows/release.yml`：四平台构建/上传，`contents: read`，无 tag/push/release；`TestUS17_Workflow_NoPublish`
- [x] T012 新增 `harness/stages/us17-base.json`、`us17-windows.json`、`us17-linux.json`、`us17-linux-arm64.json`、`us17-aggregate.json`；`TestUS17_StageLint`
- [x] T013 增加 `scripts/releasekit/docs_test.go`：文档命令与 CLI 对账（`TestUS17_Docs_CommandsMatchCLI`）
- [x] T014 扩展 `Makefile`：`release-check` / `release-rehearsal` / `release-verify` / `private-backup-test`

## Phase 3: 17C 升级、私有快照与收口

- [x] T015 实现 `scripts/releasekit/upgrade.go`：三对象备份、`basalt.old`、启动/健康检查、失败恢复
- [x] T016 实现 `scripts/privatebackup/{snapshot,archive,restore,main}.go` 与 `backup_test.go`：快照、清单、逃逸拒绝、逐文件恢复（`TestUS17_PrivateSnapshot_*`）
- [x] T017 扩展 `internal/e2e/harness_test.go`：新增 `BASALT_E2E_ARTIFACT` / `BASALT_E2E_VERSION` 分支，禁止临时构建
- [x] T018 新增 `internal/e2e/us17_artifact_test.go`：四平台归档绑定、解压、启动、`/health`、mock 任务、最小 PATH
- [x] T019 新增 `internal/e2e/us17_upgrade_test.go`：升级成功/失败恢复/对照组/迁移路径
- [x] T020 新增 `internal/e2e/us17_private_test.go`、`us17_repro_test.go`、`us17_verify_test.go`
- [x] T021 更新 `docs/Operations.md`、`docs/TechnicalDesign.md` §7.2、`docs/Compatibility.md`
- [x] T022 运行 source 定向测试、`go vet` / `staticcheck`、`gofumpt`，记录读数
- [x] T023 运行 `us17-base` 与可用平台 stage；缺 DSN / 缺平台按四态登记，不冒充通过
- [x] T024 `/speckit-analyze` 复跑、独立复核、七项证据与 run-state 收口

## Dependencies

- T001–T007 顺序依赖（types → inputs → build/package → checksum/sign/verify → record/SBOM → CLI）。
- T008 独立；T009 依赖 T004/T005；T010–T014 依赖 T007。
- T015–T020 依赖 T003/T005/T014；T021–T024 最后。

## Implementation Strategy

先让 `us17-unit`、`us17-race`、`us17-sign-verify`、`us17-record`、`us17-package-safety`、
`us17-sbom`、`us17-workflow`、`us17-docs`、`us17-stage-lint` 在 Windows source 层全绿；
e2e 与 Linux/arm64 证据按实际环境登记。

## 2026-11-02 范围调整说明

- 勾选表示**实现完成**，不表示对应验收已在真实环境通过；判定依据以 `run-state.md` §4（U1–U7）与 §7 为准。
- T015–T020 的升级／失败恢复、arm64 归档、仅下载验证、repro、clean-checkout 等执行验收延期到首次正式发布前，不因已勾选而记为通过。
- 本次收口修补：full 档增加 runtime 完整性硬门禁；`releasekit upgrade` 标为受限入口；`docs/Operations.md` 补人工升级／恢复口径；报告与证据引用按实际结果校正。
