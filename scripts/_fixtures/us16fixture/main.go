// Command us16fixture 生成 US-16 隔离演练所需的 SQLite 夹具与原生备份集。
//
// # 为什么需要它
//
// 两条演练脚本（`scripts/us16-handoff-drill.sh` / `scripts/us16-legacy-upgrade-drill.sh`）
// 驱动的都是**存量 SQLite 实例**的离线链路：`db verify --handoff`、`secrets adopt-legacy`、
// `instance adopt`、`db export`。产品 CLI 无法在 SQLite 侧造出带托管密文的源实例——
// 运行期 `serve` 只连 PostgreSQL，`instance adopt` 案例 4 只写名册。没有真夹具，
// 演练就只能"看起来跑过"。
//
// # 为什么放在 `_fixtures/`
//
// go 工具链的通配符（`./...`）、gofmt / gofumpt、`go vet`、staticcheck 都以 `_` 开头的
// 目录为界跳过，本程序因此不进构建、覆盖率与判卷面；演练脚本用**显式路径**
// `go build ./scripts/_fixtures/us16fixture` 单独构建它。放在 module 内是为了能
// import `internal/...`——夹具用的封密、密钥落盘、操作审计都走产品自己的实现，
// 手抄一份算法只会让夹具与产品各自漂移。
//
// # 输出纪律
//
// stdout 只打印 `key=value` 形式的**非敏感摘要**（路径、公开标识、哈希、计数）。
// 明文、密钥字节、nonce 与密文一律不出现在任何输出行里——演练日志会被收进证据。
//
// us16fixture builds the SQLite fixtures and native backup sets the US-16 drills
// drive. It lives under an underscore directory so the go toolchain, the format
// gates and the linters all skip it, and it prints public digests only.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/app"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// 夹具里的固定字面量：与 `internal/app/verify_handoff_test.go` 同形，
// 便于把演练读数与单元测试的断言逐条对上。
//
// `fixturePlaintext` / `fixtureLegacyPlaintext` **故意不写成 `sk-` 开头的密钥形态**：
// 本文件不是 `*_test.go`，落在 `TestRepoInvariants_NoPlaintextCredential` 的扫描面
// （`scripts/`）内；产品侧没有任何按形状识别明文密钥的逻辑，演练只拿这个串做"任何
// 可读面都不得出现"的逐字比对，因此换掉前缀不减弱判别力，而写成像真密钥会把一条
// 发布面判据判红。**不得**为"看起来更真"改回 `sk-` 形态。
// The plaintext tokens are deliberately not key-shaped: this file is not a *_test.go and
// therefore sits inside the release-surface scan; the drill only needs a unique literal.
const (
	fixtureInstanceID = "inst_fedcba9876543210"
	fixtureBaseURL    = "https://api.example.com/v1"
	fixturePlaintext  = "handoff-drill-plaintext-4w7q"
	// fixtureLegacyPlaintext 是 schema 3～5 存量源端里被封进密文的合成明文（同上：非密钥形态）。
	fixtureLegacyPlaintext = "legacy-drill-plaintext-v3"
	fixtureConnectionID    = "handoff-openai"
	fixtureSeedOpID        = "op-handoff-drill-seed-1"
	fixtureLegacyOpID      = "op-handoff-drill-legacy-adopt-1"
	fixtureAdoptOpID       = "op-handoff-drill-adopt-1"
	// fixtureLegacySeedOpID 是 legacy 源端预置的完成态操作行：凭据行的
	// `operation_id` 外键指向它，迁移 0006 提交前的 `foreign_key_check` 会逐行核对。
	fixtureLegacySeedOpID = "op-legacy-drill-seed-1"
	fixtureProcedureRef   = "OPS-2026-007"
	fixtureAnchorRef      = "CHG-2026-051"
	// fixtureKeyPathConfigKey 是 `secrets.master_key_file` 的**整键名**（进审计详情）；
	// fixtureKeyPathField 是它在 `secrets:` 段里的字段名。
	fixtureKeyPathConfigKey = "secrets.master_key_file"
	fixtureKeyPathField     = "master_key_file"
)

var (
	fixtureCredentialID  = fmt.Sprintf("cred_%026d", 700)
	fixtureConnectionUID = core.ResourceUID(fmt.Sprintf("rs_%026d", 700))
	// fixtureTargetBinding 是 legacy 谱系落库的 `target_binding`。它必须等于按发布定义的
	// `base_url` 重算出来的值：真实 v3～v5 实例里这一列就是这么算出来的，
	// `db verify --handoff` 的 (b) 维又用同一份定义重算后比对——两边不一致会报
	// `target_mismatch`。受控升级本身仍按**列值**重建 AAD、不重算（判别力在
	// `secrets_adopt_legacy_e2e_test.go`：那边源端没有发布行，重算就无从下手）。
	// The column must equal the binding recomputed from the published base_url, as a
	// real legacy instance records it; the upgrade itself still reads the column.
	fixtureTargetBinding = mustCredentialTargetBinding()
	// fixtureClock 是固定时钟：同一夹具重复生成得到逐字节相同的库。
	fixtureClock = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
)

func main() {
	if len(os.Args) < 2 {
		fail("用法：us16fixture <handoff-copy|legacy-source|backup|inspect|seed-scale> [参数]")
	}
	var err error
	switch os.Args[1] {
	case "handoff-copy":
		err = runHandoffCopy(os.Args[2:])
	case "legacy-source":
		err = runLegacySource(os.Args[2:])
	case "backup":
		err = runBackup(os.Args[2:])
	case "inspect":
		err = runInspect(os.Args[2:])
	case "seed-scale":
		err = runSeedScale(os.Args[2:])
	default:
		err = fmt.Errorf("未知子命令 %q（可用：handoff-copy / legacy-source / backup / inspect / seed-scale）", os.Args[1])
	}
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "us16fixture: "+format+"\n", args...)
	os.Exit(1)
}

// mustCredentialTargetBinding 在包初始化期算一次目标绑定：夹具生成不出来的库不该被当成
// "演练失败"来排查，直接以非零退出报出夹具自身的错误。
// mustCredentialTargetBinding fails the fixture at init when the domain refuses the
// fixture constants, so a broken jig never reads as a product finding.
func mustCredentialTargetBinding() string {
	target, err := app.CredentialTargetBinding(core.BindingModelConnection, fixtureBaseURL, "")
	if err != nil {
		fail("计算夹具目标绑定: %v", err)
	}
	return target
}

// ─────────────────────────── handoff-copy ───────────────────────────

// runHandoffCopy 造一座"迁移准备副本"：`<root>/.basalt` + 同级 `basalt.yaml` +
// 工作区外的主密钥 + 真 v6 库（初始化记录 / 托管密文 / 发布定义 / 两条审计）。
// 这条链路与 `secrets adopt-legacy` + `instance adopt` 跑完后的副本事实一致。
func runHandoffCopy(args []string) error {
	fs := flag.NewFlagSet("handoff-copy", flag.ContinueOnError)
	root := fs.String("root", "", "副本根目录（生成 <root>/.basalt 与 <root>/basalt.yaml）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*root) == "" {
		return fmt.Errorf("缺少 --root")
	}
	rootDir, err := filepath.Abs(*root)
	if err != nil {
		return fmt.Errorf("规范化 --root: %w", err)
	}
	ws := filepath.Join(rootDir, ".basalt")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		return fmt.Errorf("创建工作区: %w", err)
	}

	key, err := app.GenerateMasterKey()
	if err != nil {
		return fmt.Errorf("生成测试主密钥: %w", err)
	}
	keyPath := filepath.Join(rootDir, "private", "master.key")
	if err := app.WriteMasterKey(keyPath, ws, key); err != nil {
		return fmt.Errorf("写测试主密钥: %w", err)
	}

	configBody := []byte(renderConfig(handoffConfigLines(keyPath)))
	configPath := filepath.Join(rootDir, "basalt.yaml")
	if err := os.WriteFile(configPath, configBody, 0o600); err != nil {
		return fmt.Errorf("写配置: %w", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "instance.json"),
		instanceMarkerBytes(fixtureInstanceID), 0o600); err != nil {
		return fmt.Errorf("写名册: %w", err)
	}
	if err := buildHandoffDatabase(ws, key, configBody); err != nil {
		return err
	}

	fingerprint, err := app.MasterKeyFingerprint(key)
	if err != nil {
		return fmt.Errorf("计算主密钥指纹: %w", err)
	}
	keyFileSHA, err := hashFileString(keyPath)
	if err != nil {
		return err
	}
	dbSHA, err := hashFileString(filepath.Join(ws, "basalt.db"))
	if err != nil {
		return err
	}
	lines := []string{
		"workspace=" + ws,
		"config=" + configPath,
		"key_file=" + keyPath,
		"instance_id=" + fixtureInstanceID,
		"key_id=" + key.KeyID,
		"fingerprint=" + fingerprint,
		"key_file_sha256=" + keyFileSHA,
		"db_sha256=" + dbSHA,
		"config_sha256=" + hexSHA256(configBody),
		"schema=" + fmt.Sprintf("%d", storage.SupportedSchemaVersion),
		"credentials=1",
		"ciphertexts=1",
		"roster=present",
	}
	printLines(lines)
	return nil
}

// handoffConfigLines 是迁移准备副本的配置行：键路径持久化 + 运行期库连接占位。
// `database.dsn` 用 `${BASALT_DRILL_DSN}` 占位，运行期由进程展开——因此配置字节
// 恒定，`target_config_sha256` 不会随演练环境变化。
func handoffConfigLines(keyPath string) []string {
	return []string{
		"http:",
		"  admin_auth:",
		"    mode: local_trust",
		"secrets:",
		"  " + fixtureKeyPathField + ": '" + keyPath + "'",
		"database:",
		"  dsn: ${BASALT_DRILL_DSN}",
	}
}

// buildHandoffDatabase 写三条操作行（种子 / 记录来源 / 采纳审计）与全部身份材料。
func buildHandoffDatabase(ws string, key app.MasterKey, configBody []byte) error {
	ctx := context.Background()
	dbPath := filepath.Join(ws, "basalt.db")
	created, err := storage.Bootstrap(ctx, dbPath, storage.SupportedSchemaVersion)
	if err != nil {
		return fmt.Errorf("引导 SQLite v6: %w", err)
	}
	if !created {
		return fmt.Errorf("夹具必须是新建库：%s 已存在", dbPath)
	}
	db, err := storage.OpenWritable(ctx, dbPath, storage.SupportedSchemaVersion)
	if err != nil {
		return fmt.Errorf("打开夹具库: %w", err)
	}
	store := storage.NewManagementStore(db)
	inherit := core.OperationRecord{
		TargetKind:   core.TargetKindInstance,
		TargetID:     core.InstanceTargetMasterKey,
		ActorSubject: "migration:startup",
		AuthMethod:   core.AuthMethodMigration,
		Source:       core.AuthMethodMigration,
		Status:       core.OperationPending,
		CreatedAt:    fixtureClock,
	}
	seed := inherit
	seed.OperationID = fixtureSeedOpID
	seed.Action = core.ActionSystemSecretsInit
	seed.TargetKind = core.TargetKindSystem
	if err := store.BeginOperation(ctx, seed); err != nil {
		return fmt.Errorf("预写种子操作: %w", err)
	}
	if err := store.FinishOperation(ctx, fixtureSeedOpID,
		core.OperationSuccess, "", "", nil, fixtureClock); err != nil {
		return fmt.Errorf("回填种子操作: %w", err)
	}

	legacy := inherit
	legacy.OperationID = fixtureLegacyOpID
	legacy.Action = core.ActionInstanceSecretsLegacyAdopt
	if err := store.BeginOperation(ctx, legacy); err != nil {
		return fmt.Errorf("预写受控升级操作: %w", err)
	}
	if err := store.FinishOperation(ctx, fixtureLegacyOpID,
		core.OperationSuccess, "", "", nil, fixtureClock); err != nil {
		return fmt.Errorf("回填受控升级操作: %w", err)
	}

	detail, err := json.Marshal(adoptDetail{
		CandidateInstanceID:       fixtureInstanceID,
		CandidateInstanceIDSHA256: adoptInstanceIDSHA256(fixtureInstanceID),
		Case:                      2,
		KeyPathPersisted:          true,
		KeyPathConfigKey:          fixtureKeyPathConfigKey,
		ManagedCiphertexts:        1,
		TargetConfigSHA256:        hexSHA256(configBody),
	})
	if err != nil {
		return fmt.Errorf("渲染采纳审计详情: %w", err)
	}
	adopt := inherit
	adopt.OperationID = fixtureAdoptOpID
	adopt.Action = core.ActionInstanceAdopt
	if err := store.BeginOperation(ctx, adopt); err != nil {
		return fmt.Errorf("预写采纳操作: %w", err)
	}
	if err := store.FinishOperation(ctx, fixtureAdoptOpID,
		core.OperationSuccess, "", "", detail, fixtureClock); err != nil {
		return fmt.Errorf("回填采纳操作: %w", err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("关闭夹具库: %w", err)
	}

	fingerprint, err := app.MasterKeyFingerprint(key)
	if err != nil {
		return fmt.Errorf("计算主密钥指纹: %w", err)
	}
	raw, err := openRawSQLite(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = raw.Close() }()
	now := fixtureClock.UnixMilli()
	if _, err := raw.Exec(
		`INSERT INTO instance_secrets (scope, key_id, fingerprint, initialized_at, operation_id)
		 VALUES ('master_key', ?, ?, ?, ?)`,
		key.KeyID, fingerprint, now, fixtureLegacyOpID); err != nil {
		return fmt.Errorf("写初始化记录: %w", err)
	}
	if err := insertManagedCredential(raw, key, now); err != nil {
		return err
	}
	definition := fmt.Sprintf(
		"name: %s\nbase_url: %s\napi_key: ${HANDOFF_API_KEY}\ndefault_model: handoff-model\ncredential_mode: managed\n",
		fixtureConnectionID, fixtureBaseURL)
	inserted, err := raw.Exec(
		`INSERT INTO resource_versions (resource_kind, resource_uid, resource_id, version_no,
		                                content_hash, definition_yaml, created_at, operation_id)
		 VALUES ('model_connection', ?, ?, 1, ?, ?, ?, ?)`,
		string(fixtureConnectionUID), fixtureConnectionID,
		hexSHA256([]byte(definition)), definition, now, fixtureSeedOpID)
	if err != nil {
		return fmt.Errorf("写发布定义: %w", err)
	}
	versionID, err := inserted.LastInsertId()
	if err != nil {
		return fmt.Errorf("读发布定义版本号: %w", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO resource_pointers (resource_kind, resource_id, resource_uid, published_version_id,
		                                applied_version_id, scope, owner_agent_uid, revision, deleted_at,
		                                updated_at, operation_id)
		 VALUES ('model_connection', ?, ?, ?, ?, 'shared', NULL, 1, NULL, ?, ?)`,
		fixtureConnectionID, string(fixtureConnectionUID), versionID, versionID, now, fixtureSeedOpID); err != nil {
		return fmt.Errorf("写发布指针: %w", err)
	}
	if _, err := raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("检查点 WAL: %w", err)
	}
	return nil
}

// insertManagedCredential 用给定密钥真封一条托管模型连接凭据（身份行 + 版本行）。
// 裸 SQL 是刻意的：夹具要精确控制 `state` / `current_version` 的可篡改面。
func insertManagedCredential(raw *sql.DB, key app.MasterKey, now int64) error {
	target, err := app.CredentialTargetBinding(core.BindingModelConnection, fixtureBaseURL, "")
	if err != nil {
		return fmt.Errorf("计算目标绑定: %w", err)
	}
	aad := app.CredentialAAD{
		OwnerKind:     core.BindingModelConnection,
		OwnerUID:      fixtureConnectionUID,
		CredentialID:  fixtureCredentialID,
		VersionNo:     1,
		TargetBinding: target,
	}
	nonce, ciphertext, err := app.SealCredential(key, aad, []byte(fixturePlaintext))
	if err != nil {
		return fmt.Errorf("封装托管凭据: %w", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO credentials (credential_id, owner_kind, owner_uid, owner_name, state,
		                          current_version, key_id, created_at, updated_at, operation_id)
		 VALUES (?, 'model_connection', ?, ?, 'active', 1, ?, ?, ?, ?)`,
		fixtureCredentialID, string(fixtureConnectionUID), fixtureConnectionID,
		key.KeyID, now, now, fixtureSeedOpID); err != nil {
		return fmt.Errorf("写凭据身份行: %w", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO credential_versions (credential_id, version_no, state, key_id, target_binding,
		                                  nonce, ciphertext, created_at, updated_at, operation_id)
		 VALUES (?, 1, 'active', ?, ?, ?, ?, ?, ?, ?)`,
		fixtureCredentialID, key.KeyID, target, nonce, ciphertext, now, now,
		fixtureSeedOpID); err != nil {
		return fmt.Errorf("写凭据版本行: %w", err)
	}
	return nil
}

// ─────────────────────────── legacy-source ───────────────────────────

// runLegacySource 造一座 schema 3～5 的存量源实例：无名册、无 `instance_secrets`、
// N 条可解托管凭据。密钥落点由 `--key-mode` 决定，对应三种真实取钥场景：
//
//	unreachable —— 配置指向**源主机**的绝对路径，副本主机上不存在（K2 的动机）
//	local       —— 配置指向本机可读路径（K3 `config_path`）
//	absent      —— 配置里没有 `secrets.master_key_file`（K1 `backup_copy`）
func runLegacySource(args []string) error {
	fs := flag.NewFlagSet("legacy-source", flag.ContinueOnError)
	root := fs.String("root", "", "源端根目录（生成 <root>/.basalt 与 <root>/basalt.yaml）")
	schema := fs.Int("schema", 5, "源端 schema 版本（3～5）")
	creds := fs.Int("creds", 1, "托管凭据条数（≥1）")
	keyMode := fs.String("key-mode", "unreachable", "取钥场景：unreachable | local | absent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*root) == "" {
		return fmt.Errorf("缺少 --root")
	}
	if *schema < 3 || *schema >= storage.SupportedSchemaVersion {
		return fmt.Errorf("--schema 必须是 3～%d（实收 %d）", storage.SupportedSchemaVersion-1, *schema)
	}
	if *creds < 1 {
		return fmt.Errorf("--creds 至少为 1（实收 %d）", *creds)
	}
	rootDir, err := filepath.Abs(*root)
	if err != nil {
		return fmt.Errorf("规范化 --root: %w", err)
	}
	ws := filepath.Join(rootDir, ".basalt")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		return fmt.Errorf("创建工作区: %w", err)
	}

	key, err := app.GenerateMasterKey()
	if err != nil {
		return fmt.Errorf("生成测试主密钥: %w", err)
	}
	keyPath := filepath.Join(rootDir, "private", "master.key")
	if err := app.WriteMasterKey(keyPath, ws, key); err != nil {
		return fmt.Errorf("写测试主密钥: %w", err)
	}

	configKeyPath := keyPath
	switch *keyMode {
	case "unreachable":
		// 源主机的路径：绝对路径、形态合法，但副本主机上读不到。它在配置里保持
		// 字面形态，正是"配置路径不可达"这条真实约束的可判定形态。
		configKeyPath = filepath.Join(rootDir, "source-host-only", "master.key")
	case "absent":
		configKeyPath = ""
	case "local":
		// configKeyPath 已经是本机可读路径。
	default:
		return fmt.Errorf("未知 --key-mode %q（可用：unreachable / local / absent）", *keyMode)
	}
	configBody := []byte(renderConfig(legacyConfigLines(configKeyPath)))
	configPath := filepath.Join(rootDir, "basalt.yaml")
	if err := os.WriteFile(configPath, configBody, 0o600); err != nil {
		return fmt.Errorf("写配置: %w", err)
	}
	dbPath := filepath.Join(ws, "basalt.db")
	if err := buildLegacySQLite(dbPath, *schema, *creds, key); err != nil {
		return err
	}

	fingerprint, err := app.MasterKeyFingerprint(key)
	if err != nil {
		return fmt.Errorf("计算主密钥指纹: %w", err)
	}
	keyFileSHA, err := hashFileString(keyPath)
	if err != nil {
		return err
	}
	dbSHA, err := hashFileString(dbPath)
	if err != nil {
		return err
	}
	printLines([]string{
		"workspace=" + ws,
		"config=" + configPath,
		"key_file=" + keyPath,
		"key_id=" + key.KeyID,
		"fingerprint=" + fingerprint,
		"key_file_sha256=" + keyFileSHA,
		"db_sha256=" + dbSHA,
		"config_sha256=" + hexSHA256(configBody),
		"schema=" + fmt.Sprintf("%d", *schema),
		"credentials=" + fmt.Sprintf("%d", *creds),
		"roster=absent",
	})
	return nil
}

// legacyConfigLines 是存量源端的配置行；`configKeyPath` 为空即整段省略（K1 形态）。
func legacyConfigLines(configKeyPath string) []string {
	lines := []string{
		"http:",
		"  admin_auth:",
		"    mode: local_trust",
	}
	if configKeyPath != "" {
		lines = append(lines, "secrets:", "  "+fixtureKeyPathField+": '"+configKeyPath+"'")
	}
	return append(lines, "database:", "  dsn: ${BASALT_DRILL_DSN}")
}

// buildLegacySQLite 在 path 上造一座 schema 3～5 的真库。`schema.sql` 已是**全量当前
// schema**，旧谱系只能按"删掉 v6 增量"还原：去掉高版本才建的表与列，版本行停在目标
// 版本。受控升级实际读的三件事——`schema_version`、`instance_secrets` 缺表、`credentials`
// 行——都与真 v3～v5 库同形。
func buildLegacySQLite(path string, schema, creds int, key app.MasterKey) error {
	ctx := context.Background()
	created, err := storage.Bootstrap(ctx, path, schema)
	if err != nil {
		return fmt.Errorf("引导 SQLite v%d: %w", schema, err)
	}
	if !created {
		return fmt.Errorf("夹具必须是新建库：%s 已存在", path)
	}
	db, err := storage.OpenWritable(ctx, path, schema)
	if err != nil {
		return fmt.Errorf("打开夹具库: %w", err)
	}
	store := storage.NewManagementStore(db)
	if err := store.BeginOperation(ctx, core.OperationRecord{
		OperationID:  fixtureLegacySeedOpID,
		Action:       "system.test_seed",
		TargetKind:   core.TargetKindInstance,
		TargetID:     core.InstanceTargetMasterKey,
		ActorSubject: "cli:fixture",
		AuthMethod:   "cli",
		Source:       "cli",
		Status:       core.OperationPending,
		CreatedAt:    fixtureClock,
	}); err != nil {
		return fmt.Errorf("预写种子操作: %w", err)
	}
	if err := store.FinishOperation(ctx, fixtureLegacySeedOpID,
		core.OperationSuccess, "", "", nil, fixtureClock); err != nil {
		return fmt.Errorf("回填种子操作: %w", err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("关闭夹具库: %w", err)
	}
	// 关净句柄即检查点并删除 WAL 索引文件——备份与副本要逐字节相同。
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, statErr := os.Lstat(path + suffix); statErr == nil {
			return fmt.Errorf("关闭夹具库后仍残留 %s——三份副本无法逐字节相同", path+suffix)
		}
	}

	raw, err := openRawSQLite(path)
	if err != nil {
		return err
	}
	defer func() { _ = raw.Close() }()
	stale := []string{
		"instance_secrets", "execution_policy", "network_exceptions", "mcp_credential_sources",
	}
	if schema < 5 {
		stale = append(stale, "network_domain_policy")
	}
	if schema < 4 {
		stale = append(stale, "schedule_deliveries", "resource_connection_tests")
	}
	for _, table := range stale {
		if _, execErr := raw.Exec(`DROP TABLE IF EXISTS "` + table + `"`); execErr != nil {
			return fmt.Errorf("撤掉 v%d 增量表 %s: %w", schema, table, execErr)
		}
	}
	if schema < 4 {
		if _, execErr := raw.Exec(`ALTER TABLE scheduled_tasks DROP COLUMN deliver_to`); execErr != nil {
			return fmt.Errorf("撤掉 scheduled_tasks.deliver_to: %w", execErr)
		}
	}
	now := fixtureClock.UnixMilli()
	for i := 0; i < creds; i++ {
		ownerUID := core.ResourceUID(fmt.Sprintf("rs_%026d", 100+i))
		credentialID := fmt.Sprintf("cred_%026d", 100+i)
		connectionID := fmt.Sprintf("legacy-openai-%d", i+1)
		aad := app.CredentialAAD{
			OwnerKind:     core.BindingModelConnection,
			OwnerUID:      ownerUID,
			CredentialID:  credentialID,
			VersionNo:     1,
			TargetBinding: fixtureTargetBinding,
		}
		nonce, ciphertext, sealErr := app.SealCredential(key, aad, []byte(fixtureLegacyPlaintext))
		if sealErr != nil {
			return fmt.Errorf("封装第 %d 条凭据: %w", i+1, sealErr)
		}
		if _, execErr := raw.Exec(
			`INSERT INTO credentials (credential_id, owner_kind, owner_uid, owner_name, state,
			                          current_version, key_id, created_at, updated_at, operation_id)
			 VALUES (?, ?, ?, ?, 'active', 1, ?, ?, ?, ?)`,
			credentialID, string(core.BindingModelConnection), string(ownerUID), connectionID,
			key.KeyID, now, now, fixtureLegacySeedOpID); execErr != nil {
			return fmt.Errorf("写第 %d 条凭据身份行: %w", i+1, execErr)
		}
		if _, execErr := raw.Exec(
			`INSERT INTO credential_versions (credential_id, version_no, state, key_id,
			                                  target_binding, nonce, ciphertext, created_at,
			                                  updated_at, operation_id)
			 VALUES (?, 1, 'active', ?, ?, ?, ?, ?, ?, ?)`,
			credentialID, key.KeyID, fixtureTargetBinding, nonce, ciphertext, now, now,
			fixtureLegacySeedOpID); execErr != nil {
			return fmt.Errorf("写第 %d 条凭据版本行: %w", i+1, execErr)
		}
		// 每条 active 托管凭据都要有**当前发布**的模型连接：真实 v3～v5 实例里，托管凭据
		// 之所以存在，正是因为一条 `credential_mode: managed` 的连接发布过；
		// `db verify --handoff` 的 (b) 维按发布定义重算目标绑定再与版本列值比对，夹具只写
		// 凭据不写发布，副本升级后必然报 `binding_missing`——那是夹具缺件，不是产品行为。
		// Every active managed credential implies a published connection: (b) recomputes
		// the target from the published definition, so the fixture must record it too.
		definition := fmt.Sprintf(
			"name: %s\nbase_url: %s\napi_key: ${LEGACY_API_KEY}\ndefault_model: legacy-model\ncredential_mode: managed\n",
			connectionID, fixtureBaseURL)
		inserted, execErr := raw.Exec(
			`INSERT INTO resource_versions (resource_kind, resource_uid, resource_id, version_no,
			                                content_hash, definition_yaml, created_at, operation_id)
			 VALUES ('model_connection', ?, ?, 1, ?, ?, ?, ?)`,
			string(ownerUID), connectionID, hexSHA256([]byte(definition)), definition, now,
			fixtureLegacySeedOpID)
		if execErr != nil {
			return fmt.Errorf("写第 %d 条连接的发布定义: %w", i+1, execErr)
		}
		versionID, execErr := inserted.LastInsertId()
		if execErr != nil {
			return fmt.Errorf("读第 %d 条连接的发布版本号: %w", i+1, execErr)
		}
		if _, execErr := raw.Exec(
			`INSERT INTO resource_pointers (resource_kind, resource_id, resource_uid, published_version_id,
			                                applied_version_id, scope, owner_agent_uid, revision, deleted_at,
			                                updated_at, operation_id)
			 VALUES ('model_connection', ?, ?, ?, ?, 'shared', NULL, 1, NULL, ?, ?)`,
			connectionID, string(ownerUID), versionID, versionID, now,
			fixtureLegacySeedOpID); execErr != nil {
			return fmt.Errorf("写第 %d 条连接的发布指针: %w", i+1, execErr)
		}
	}
	if _, err := raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("检查点 WAL: %w", err)
	}
	return nil
}

// ─────────────────────────── backup ───────────────────────────

// backupManifest 是 `backup.json` 的结构（§2.12.0b 字段表，format_version=1）。
// 字段名与 `internal/app` 的同名结构逐字一致：产品侧读的是 JSON 标签，两侧各持一份
// 定义只在这条 JSON 契约上耦合。
type backupManifest struct {
	FormatVersion      int    `json:"format_version"`
	SourceHost         string `json:"source_host"`
	SourceWorkspace    string `json:"source_workspace"`
	SourceStoppedAt    string `json:"source_stopped_at"`
	BackupCreatedAt    string `json:"backup_created_at"`
	Operator           string `json:"operator"`
	SchemaVersion      int    `json:"schema_version"`
	DBSHA256           string `json:"db_sha256"`
	WALSHA256          string `json:"wal_sha256"`
	SHMSHA256          string `json:"shm_sha256"`
	SourceConfigSHA256 string `json:"source_config_sha256"`
	BackupConfigSHA256 string `json:"backup_config_sha256"`
	RosterPresent      bool   `json:"roster_present"`
	RosterSHA256       string `json:"roster_sha256"`
	InstanceID         string `json:"instance_id"`
	KeyFingerprint     string `json:"key_fingerprint"`
	KeyID              string `json:"key_id"`
	KeyFileSHA256      string `json:"key_file_sha256"`
	KeyFileName        string `json:"key_file"`
	CredentialVersions int    `json:"credential_version_count"`
	CiphertextCount    int    `json:"ciphertext_count"`
	RecordsAbsent      bool   `json:"records_absent"`
	ProcedureRef       string `json:"procedure_ref"`
	AnchorRef          string `json:"anchor_ref"`
}

// runBackup 从源端造一份原生备份集：库 / 配置 / 名册（若有）/ 备份内密钥副本 +
// `backup.json`。所有哈希都在落盘后由本程序**重算文件**得出，不是从源端抄。
func runBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	source := fs.String("source", "", "源端根目录（含 .basalt/ 与 basalt.yaml）")
	out := fs.String("out", "", "备份输出目录（须为空或不存在）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*source) == "" || strings.TrimSpace(*out) == "" {
		return fmt.Errorf("缺少 --source / --out")
	}
	sourceRoot, err := filepath.Abs(*source)
	if err != nil {
		return fmt.Errorf("规范化 --source: %w", err)
	}
	outDir, err := filepath.Abs(*out)
	if err != nil {
		return fmt.Errorf("规范化 --out: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("创建备份目录: %w", err)
	}

	srcWS := filepath.Join(sourceRoot, ".basalt")
	dbPath := filepath.Join(srcWS, "basalt.db")
	configPath := filepath.Join(sourceRoot, "basalt.yaml")
	rosterPath := filepath.Join(srcWS, "instance.json")
	keyPath := filepath.Join(sourceRoot, "private", "master.key")

	dbSHA, err := copyInto(dbPath, filepath.Join(outDir, "basalt.db"))
	if err != nil {
		return err
	}
	configSHA, err := copyInto(configPath, filepath.Join(outDir, "basalt.yaml"))
	if err != nil {
		return err
	}
	if _, err := copyInto(keyPath, filepath.Join(outDir, "master.key")); err != nil {
		return err
	}
	keyFileSHA, err := hashFileString(filepath.Join(outDir, "master.key"))
	if err != nil {
		return err
	}

	manifest := backupManifest{
		FormatVersion:      1,
		SourceHost:         "drill-source-host",
		SourceWorkspace:    srcWS,
		SourceStoppedAt:    "2026-10-28T10:00:00Z",
		BackupCreatedAt:    "2026-10-28T10:05:00Z",
		Operator:           "drill-operator",
		SchemaVersion:      readSchemaVersion(dbPath),
		DBSHA256:           dbSHA,
		SourceConfigSHA256: configSHA,
		BackupConfigSHA256: configSHA,
		KeyID:              readKeyID(filepath.Join(outDir, "master.key")),
		KeyFingerprint:     readKeyFingerprint(filepath.Join(outDir, "master.key")),
		KeyFileSHA256:      keyFileSHA,
		KeyFileName:        "master.key",
		CredentialVersions: 1,
		CiphertextCount:    1,
		RecordsAbsent:      true,
		ProcedureRef:       fixtureProcedureRef,
		AnchorRef:          fixtureAnchorRef,
	}
	// #nosec G304 -- 路径 = `--source` 根 + 常量叶子（`.basalt/instance.json`）。变量
	// 部分就是演练交进来的源端根，叶子名在本函数里写死，没有可穿出根的相对段；夹具的
	// 全部读范围就是演练自建的临时目录（drill 传 `--source "$SRC"`）。收窄成"必须落在
	// 根内"是同义反复——路径本来就是根拼常量，故此处只做具名豁免。
	// The tainted part is the root the drill hands us; the leaf is a literal, so there is
	// no sub-path segment that could escape it.
	if roster, readErr := os.ReadFile(rosterPath); readErr == nil {
		manifest.RosterPresent = true
		manifest.RosterSHA256 = hexSHA256(roster)
		manifest.InstanceID = readInstanceID(roster)
		if _, err := copyInto(rosterPath, filepath.Join(outDir, "instance.json")); err != nil {
			return err
		}
	} else if !os.IsNotExist(readErr) {
		return fmt.Errorf("读源端名册: %w", readErr)
	}

	raw, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("渲染备份清单: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "backup.json"), raw, 0o600); err != nil {
		return fmt.Errorf("写备份清单: %w", err)
	}
	printLines([]string{
		"backup_dir=" + outDir,
		"manifest_sha256=" + hexSHA256(raw),
		"db_sha256=" + manifest.DBSHA256,
		"config_sha256=" + manifest.BackupConfigSHA256,
		"key_file_sha256=" + manifest.KeyFileSHA256,
		"key_id=" + manifest.KeyID,
		"fingerprint=" + manifest.KeyFingerprint,
		"schema=" + fmt.Sprintf("%d", manifest.SchemaVersion),
		"roster=" + boolWord(manifest.RosterPresent),
	})
	return nil
}

// ─────────────────────────── inspect ───────────────────────────

// runInspect 只读汇报一座工作区的关键事实：演练要判"受控升级写出了什么"（记录数 /
// 名册存在性 / 各表行数），又不想为此引入 sqlite3 或 python 这类外部读取器。
// 只读连接不会建 `-wal` / `-shm`，因此 inspect 本身不改变被检对象的字节。
// runInspect reports the facts a drill asserts on; the read-only handle leaves no residue.
func runInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "工作区目录（<root>/.basalt）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*workspace) == "" {
		return fmt.Errorf("缺少 --workspace")
	}
	ws, err := filepath.Abs(*workspace)
	if err != nil {
		return fmt.Errorf("规范化 --workspace: %w", err)
	}
	dbPath := filepath.Join(ws, "basalt.db")
	if _, statErr := os.Stat(dbPath); statErr != nil {
		return fmt.Errorf("工作区没有 basalt.db: %w", statErr)
	}
	raw, err := openRawSQLiteReadOnly(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = raw.Close() }()

	var version int
	if err := raw.QueryRow(`SELECT coalesce(max(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("读 schema_version: %w", err)
	}
	counts := make(map[string]int, 5)
	for _, table := range []string{
		"instance_secrets", "credentials", "credential_versions",
		"resource_versions", "resource_pointers",
	} {
		n, countErr := countRows(raw, table)
		if countErr != nil {
			return countErr
		}
		counts[table] = n
	}

	rosterPath := filepath.Join(ws, "instance.json")
	// #nosec G304 -- 同 runBackup：`--workspace` 根 + 常量叶子 `instance.json`。演练只
	// 检它自己刚跑完的工作区（drill 传 `--workspace "$cp_root/.basalt"`）。
	// Same shape as runBackup: the workspace root plus a literal leaf name.
	rosterBody, readErr := os.ReadFile(rosterPath)
	rosterPresent := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("读名册: %w", readErr)
	}
	instanceID := ""
	if rosterPresent {
		instanceID = readInstanceID(rosterBody)
	}

	printLines([]string{
		"workspace=" + ws,
		"schema=" + fmt.Sprintf("%d", version),
		"instance_secrets=" + fmt.Sprintf("%d", counts["instance_secrets"]),
		"credentials=" + fmt.Sprintf("%d", counts["credentials"]),
		"credential_versions=" + fmt.Sprintf("%d", counts["credential_versions"]),
		"resource_versions=" + fmt.Sprintf("%d", counts["resource_versions"]),
		"resource_pointers=" + fmt.Sprintf("%d", counts["resource_pointers"]),
		"roster=" + boolWord(rosterPresent),
		"instance_id=" + instanceID,
	})
	return nil
}

// ─────────────────────────── seed-scale ───────────────────────────

// scaleContentTemplate 是合成消息正文的固定模板。**不得**改成随机串：T008c 比较的是
// "同一份内容在不同规模下的导出字节与 WAL 增量"，正文一变，行数与字节数就不再可比。
// The seed text is fixed on purpose: T008c compares byte counts across scales.
const scaleContentTemplate = "第 %d 条合成消息：迁移规模演练用正文，长度固定以便在不同规模之间比较导出字节、目标表体积与 WAL 增量。" +
	"channel=web user=scale-user role=%s 正文末尾固定补足：%s"

// scalePadding 把正文长度补到接近真实对话（≈400 字节）：太短会让 TOAST 压缩把
// 目标表体积压成常数，反而看不出规模效应；太长则与真实消息形态不符。
const scalePadding = "迁移演练占位正文。"

// runSeedScale 往一座**已存在**的 v6 源库里灌入规模演练数据：S 个会话、R 条 Run、
// N 条消息，全部落在同一个事务里，写前不删任何既有行。
//
// 为什么由夹具而不是脚本造：`seq` 是会话内从 1 起的追加序、`message_count` 是冗余计数，
// 两者写错会让导出正常、导入也正常，却在**核对时**才暴露——命令行里拼 SQL 既难保
// 可复现，也把"消息的形状"分散到两个文件里。
func runSeedScale(args []string) error {
	fs := flag.NewFlagSet("seed-scale", flag.ContinueOnError)
	dbPath := fs.String("db", "", "目标 v6 SQLite 库（<workspace>/basalt.db）")
	messages := fs.Int("messages", 0, "合成消息条数（> 0）")
	sessionCount := fs.Int("sessions", 64, "会话数（>= 1 且 <= messages）")
	runCount := fs.Int("runs", 0, "Run 条数（0 = 每个会话一条）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(*dbPath) == "":
		return fmt.Errorf("缺少 --db")
	case *messages <= 0:
		return fmt.Errorf("--messages 必须大于 0（实收 %d）", *messages)
	case *sessionCount <= 0:
		return fmt.Errorf("--sessions 必须大于 0（实收 %d）", *sessionCount)
	case *sessionCount > *messages:
		return fmt.Errorf("--sessions（%d）不得大于 --messages（%d）：空会话不是本次演练的形态",
			*sessionCount, *messages)
	}
	path, err := filepath.Abs(*dbPath)
	if err != nil {
		return fmt.Errorf("规范化 --db: %w", err)
	}
	runs := *runCount
	if runs <= 0 {
		runs = *sessionCount
	}
	if runs < *sessionCount {
		return fmt.Errorf("--runs（%d）至少等于会话数（%d）：每个会话都要有归属 Run", runs, *sessionCount)
	}

	raw, err := openRawSQLite(path)
	if err != nil {
		return err
	}
	defer func() { _ = raw.Close() }()

	var version int
	if err := raw.QueryRow(`SELECT coalesce(max(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("读 schema_version: %w", err)
	}
	if version != storage.SupportedSchemaVersion {
		return fmt.Errorf("源库是 v%d，规模演练只灌 v%d（先用当前发布版建库）",
			version, storage.SupportedSchemaVersion)
	}

	ctx := context.Background()
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启灌数事务: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// 会话：先建父行，`message_count` 在消息灌完后回填（一次 UPDATE，不逐条累加）。
	for i := range *sessionCount {
		id := fmt.Sprintf("scale-sess-%06d", i)
		created := fixtureClock.Add(time.Duration(i) * time.Second).UnixMilli()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (session_id, profile_name, channel, user_id, status, message_count, created_at, last_active_at)
			 VALUES (?, ?, 'web', 'scale-user', 'active', 0, ?, ?)`,
			id, "scale-agent", created, created); err != nil {
			return fmt.Errorf("插入会话 %s: %w", id, err)
		}
	}

	// Run：按会话轮转分配；`session_id` 与 messages 的归属保持一致。
	for i := range runs {
		id := fmt.Sprintf("scale-run-%06d", i)
		session := fmt.Sprintf("scale-sess-%06d", i%*sessionCount)
		started := fixtureClock.Add(time.Duration(i) * time.Second).UnixMilli()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO runs (run_id, session_id, agent, channel, user_id, trigger, status,
			                   iterations, tool_calls, started_at, finished_at)
			 VALUES (?, ?, 'scale-agent', 'web', 'scale-user', 'api', 'completed', 2, 1, ?, ?)`,
			id, session, started, started+1500); err != nil {
			return fmt.Errorf("插入 Run %s: %w", id, err)
		}
	}

	// 消息：会话 i%S 的第 seq 条。`seq` 从 1 起连续，导入侧的唯一索引
	// idx_messages_session_seq 会逐条判卷——这里错一位，演练会在导入时才炸。
	insert, err := tx.PrepareContext(ctx,
		`INSERT INTO messages (session_id, run_id, seq, role, content, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("准备消息插入: %w", err)
	}
	defer func() { _ = insert.Close() }()

	for i := range *messages {
		sessionIndex := i % *sessionCount
		seq := i / *sessionCount
		role := "user"
		if seq%2 == 1 {
			role = "assistant"
		}
		content := fmt.Sprintf(scaleContentTemplate, i, role, strings.Repeat(scalePadding, 8))
		session := fmt.Sprintf("scale-sess-%06d", sessionIndex)
		runID := fmt.Sprintf("scale-run-%06d", sessionIndex%runs)
		created := fixtureClock.Add(time.Duration(seq)*time.Second + time.Duration(sessionIndex)).UnixMilli()
		if _, err := insert.ExecContext(ctx, session, runID, seq+1, role, content, created); err != nil {
			return fmt.Errorf("插入第 %d 条消息: %w", i, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET message_count = (
		     SELECT count(*) FROM messages WHERE messages.session_id = sessions.session_id)
		 WHERE session_id LIKE 'scale-sess-%'`); err != nil {
		return fmt.Errorf("回填会话消息计数: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交灌数事务: %w", err)
	}
	committed = true

	// 与其余夹具同口径：来源库落盘成单个文件，`-wal` / `-shm` 不为导出留尾巴。
	if _, err := raw.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("检查点: %w", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("读源库尺寸: %w", err)
	}
	dbSHA, err := hashFileString(path)
	if err != nil {
		return err
	}
	sessionMessages, err := countRows(raw, "messages")
	if err != nil {
		return err
	}
	printLines([]string{
		"db=" + path,
		"db_sha256=" + dbSHA,
		"db_bytes=" + fmt.Sprintf("%d", info.Size()),
		"sessions=" + fmt.Sprintf("%d", *sessionCount),
		"runs=" + fmt.Sprintf("%d", runs),
		"messages=" + fmt.Sprintf("%d", sessionMessages),
	})
	return nil
}

// countRows 数一张表的行数；表不存在（旧谱系还没建到它）记 0 而不是报错——演练要的是
// "升级后有没有这条记录"，不是"这张表必须存在"。
func countRows(raw *sql.DB, table string) (int, error) {
	var exists int
	if err := raw.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&exists); err != nil {
		return 0, fmt.Errorf("查表 %s 是否存在: %w", table, err)
	}
	if exists == 0 {
		return 0, nil
	}
	var n int
	// 表名来自调用方的常量清单，不含外部输入。
	if err := raw.QueryRow(`SELECT count(*) FROM "` + table + `"`).Scan(&n); err != nil {
		return 0, fmt.Errorf("数表 %s 的行数: %w", table, err)
	}
	return n, nil
}

// copyInto 逐字节复制文件并返回**副本**的哈希（声明必须落在副本上，不是源端）。
func copyInto(from, to string) (string, error) {
	// #nosec G304 -- `copyInto` 的每个调用方都传"根 + 常量叶子"（`basalt.db` /
	// `basalt.yaml` / `private/master.key` / `.basalt/instance.json`），源端根来自
	// `--source`，备份根来自 `--out`。备份工具按定义要读操作者给的目录，夹具没有、
	// 也不该有比这更细的读范围可以收窄。
	// Every caller passes root + literal leaf; a backup fixture reads the tree it is given.
	body, err := os.ReadFile(from)
	if err != nil {
		return "", fmt.Errorf("读 %s: %w", from, err)
	}
	// #nosec G703 -- 写端同理：`to` 全部是 `--out` 根 + 常量叶子，落点由演练指定的
	// 备份目录决定；这里写出的是刚读进来的同名字节（备份集），不是由路径内容决定的
	// 意外目标。
	// The write side mirrors the read: the destination is the drill's backup root.
	if err := os.WriteFile(to, body, 0o600); err != nil {
		return "", fmt.Errorf("写 %s: %w", to, err)
	}
	return hexSHA256(body), nil
}

// ─────────────────────────── 公共助手 ───────────────────────────

// adoptDetail 是 `instance.adopt` 审计终态的 JSON 形态（与 `internal/app` 的同名
// 非导出结构逐字段一致）；夹具靠它给出 `target_config_sha256` 基线。
type adoptDetail struct {
	CandidateInstanceID       string `json:"candidate_instance_id"`
	CandidateInstanceIDSHA256 string `json:"candidate_instance_id_sha256"`
	Case                      int    `json:"case"`
	KeyPathPersisted          bool   `json:"key_path_persisted"`
	KeyPathConfigKey          string `json:"key_path_config_key,omitempty"`
	ManagedCiphertexts        int    `json:"managed_ciphertexts"`
	TargetConfigSHA256        string `json:"target_config_sha256,omitempty"`
}

// openRawSQLite 用独立原始连接读写夹具库：`storage.DB` 不对外暴露执行面，
// 而造夹具必须真写。PRAGMA 与生产 DSN 同源（外键 + 忙等）。
func openRawSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite",
		"file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("打开原始 SQLite 连接: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("连通原始 SQLite 连接: %w", err)
	}
	return db, nil
}

// openRawSQLiteReadOnly 只读打开：`inspect` 只数行，不得因为读一下就建出 `-wal` / `-shm`。
// openRawSQLiteReadOnly never writes, so inspecting a workspace leaves no WAL residue.
func openRawSQLiteReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite",
		"file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("只读打开 SQLite 连接: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("连通只读 SQLite 连接: %w", err)
	}
	return db, nil
}

// renderConfig 把配置行拼成最终字节：行尾一律 `\n`，空行留白由调用方决定。
func renderConfig(lines []string) string {
	return strings.Join(lines, "\n") + "\n"
}

// instanceMarkerBytes 渲染实例名册（与产品侧 `marshalInstanceMarker` 逐字节一致）。
func instanceMarkerBytes(instanceID string) []byte {
	raw, err := json.MarshalIndent(struct {
		Version    int    `json:"version"`
		InstanceID string `json:"instance_id"`
	}{Version: 1, InstanceID: instanceID}, "", "  ")
	if err != nil {
		// 结构体是常量形态，序列化失败只可能是编程错误。
		panic(fmt.Sprintf("渲染实例名册: %v", err))
	}
	return append(raw, '\n')
}

// readInstanceID 从名册字节里取实例标识；名册形态非法时返回空串（清单在校验侧
// 仍会因 `roster_present=true` + 空 `instance_id` 被拒，不需要在这里再造一套拒绝）。
func readInstanceID(raw []byte) string {
	var marker struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(raw, &marker); err != nil {
		return ""
	}
	return marker.InstanceID
}

// readSchemaVersion 读 `schema_version` 表首行的版本号。
func readSchemaVersion(path string) int {
	db, err := openRawSQLite(path)
	if err != nil {
		return 0
	}
	defer func() { _ = db.Close() }()
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		return 0
	}
	return version
}

// readKeyID / readKeyFingerprint 从备份那份**已知合法的**密钥文件里取公开标识。
// 密钥字节本身不落任何输出行：加载后立刻清零。
//
// 工作区根传空串是刻意的——`LoadMasterKey` 只在配置引用的工作区语境下判"密钥不得
// 放在工作区内"；夹具在备份目录里读的是独立副本，没有任何工作区语境。
func readKeyID(path string) string {
	doc, err := readKeyDocument(path)
	if err != nil {
		return ""
	}
	return doc.KeyID
}

func readKeyFingerprint(path string) string {
	doc, err := readKeyDocument(path)
	if err != nil {
		return ""
	}
	return doc.fingerprint
}

// keyDocument 是密钥文件的公开投影：`key_id` 与指纹。
type keyDocument struct {
	KeyID       string
	fingerprint string
}

func readKeyDocument(path string) (keyDocument, error) {
	key, err := app.LoadMasterKey(path, "")
	if err != nil {
		return keyDocument{}, err
	}
	defer key.Zero()
	fingerprint, err := app.MasterKeyFingerprint(key)
	if err != nil {
		return keyDocument{}, err
	}
	return keyDocument{KeyID: key.KeyID, fingerprint: fingerprint}, nil
}

func hashFileString(path string) (string, error) {
	// #nosec G304 -- 唯一调用方传 `filepath.Join(outDir, "master.key")`：备份根 + 常量
	// 叶子。哈希要落在**副本**上（声明必须由本程序重算），所以必须按路径重读一次。
	// The only caller passes the backup root plus the literal `master.key`.
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读 %s: %w", path, err)
	}
	return hexSHA256(body), nil
}

func hexSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func adoptInstanceIDSHA256(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func boolWord(value bool) string {
	if value {
		return "present"
	}
	return "absent"
}

func printLines(lines []string) {
	for _, line := range lines {
		fmt.Println(line)
	}
}
