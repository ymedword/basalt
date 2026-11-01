package app

// 本文件是 `secrets adopt-legacy` 的**端到端**判卷（颗粒度文档 §3.1.8c / C-51，
// check `us16-handoff`）：源端、备份、隔离副本三份**逐字节相同**的真 schema 3～5
// SQLite 库，真迁移、真记录、真审计；每条拒绝路径都判"副本一个字节没动"。
//
// 与 `secrets_adopt_legacy_test.go` 的分工：那里用非 SQLite 字节绕开迁移，只判清单自洽
// 与名册门（两者都在打开库之前）；这里必须真库，因为迁移前后 schema、密文摘要、
// 审计行才是本命令的核心副作用。
//
// 夹具刻意造三种"看起来对"的假成功：`key_id` 相符但密文解不开、AAD 字段重建不出、
// 备份自证哈希齐全但与源端漂移——每一条都必须红。
// These tests exercise the controlled legacy upgrade end to end on a genuine legacy
// database with byte-identical source, backup and copy.

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// legacySeedOperationID 是夹具里预置的那条完成态操作行：凭据行的 `operation_id` 外键
// 指向它，迁移 0006 提交前的 `foreign_key_check` 会逐行核对。
// legacySeedOperationID anchors the seeded credential rows' foreign key.
const legacySeedOperationID = "op-legacy-e2e-seed-1"

// legacyE2EParams 是夹具变形旋钮；零值即"schema v5、1 条可解密文、配置带密钥路径"。
// legacyE2EParams dials the fixture; the zero value is the healthy legacy instance.
type legacyE2EParams struct {
	// Schema 是源端库的 schema 版本（默认 v5）。
	Schema int
	// Creds 是托管凭据条数（默认 1）。
	Creds int
	// Roster 非空时源端与备份都带这份名册（清单按它声明 roster_present=true）。
	Roster []byte
	// SealWith 非 nil 时用这把钥匙封密文，但**库里仍写源端的 key_id**——
	// "标识相符、材料不符"正是连续校验要拦的假成功。
	SealWith *MasterKey
	// ClearTargetBinding 把落库的 `target_binding` 清空：AAD 重建不出来。
	ClearTargetBinding bool
	// CorruptCiphertext 翻转密文首字节：AAD 可重建，但 GCM 认证失败。
	CorruptCiphertext bool
	// ConfigKeyPath 非空时用它代替源端密钥路径写进配置（"配置指向另一把钥匙"）。
	ConfigKeyPath string
}

// legacyAdoptE2E 是端到端夹具：源端 + 备份 + 隔离副本。
// legacyAdoptE2E holds the source, the backup and the isolated copy.
type legacyAdoptE2E struct {
	t           *testing.T
	sourceRoot  string
	sourceWS    string
	copyRoot    string
	copyWS      string
	dataRoot    string
	config      []byte
	configPath  string
	key         MasterKey
	keyPath     string
	keyFileSHA  string
	dbBytes     []byte
	params      legacyE2EParams
	backupDir   string
	manifest    legacyBackupManifest
	manifestSHA string
}

// newLegacyAdoptE2E 造夹具。夹具本身就是"受控备份"的形状：三份库字节相同，
// `basalt.yaml` 两份相同，清单里的每个哈希都由这三份文件算出。
//
// 源端、备份、隔离副本各占一个临时根目录，布局都镜像 `<WorkDir>/.basalt` +
// 同级 `basalt.yaml`；用户数据根指向另一个空目录，用来判"离线命令不写数据根"。
// newLegacyAdoptE2E builds the three byte-identical sides and the matching manifest.
func newLegacyAdoptE2E(t *testing.T, mutate func(*legacyE2EParams)) *legacyAdoptE2E {
	t.Helper()
	params := legacyE2EParams{Schema: 5, Creds: 1}
	if mutate != nil {
		mutate(&params)
	}

	dataRoot := t.TempDir()
	t.Setenv("LOCALAPPDATA", dataRoot)
	t.Setenv("XDG_DATA_HOME", dataRoot)

	sourceRoot := t.TempDir()
	sourceWS := filepath.Join(sourceRoot, workspaceDirName)
	require.NoError(t, os.MkdirAll(sourceWS, 0o700))

	key := testMasterKey()
	keyPath := writeMasterKeyFile(t, filepath.Join(sourceRoot, "private"), key.KeyID, key.Key)
	keyFileSHA, err := hashFileString(keyPath)
	require.NoError(t, err)

	configKeyPath := params.ConfigKeyPath
	if configKeyPath == "" {
		configKeyPath = keyPath
	}
	config := []byte(adoptConfigYAML("local_trust", "", adoptKeyLine(configKeyPath)))
	require.NoError(t, os.WriteFile(filepath.Join(sourceRoot, configFileName), config, 0o600))

	dbPath := filepath.Join(sourceWS, databaseFileName)
	buildLegacySQLite(t, dbPath, params, key)
	dbBytes, err := os.ReadFile(dbPath)
	require.NoError(t, err)

	roster := params.Roster
	if roster != nil {
		require.NoError(t, os.WriteFile(filepath.Join(sourceWS, instanceMarkerFileName), roster, 0o600))
	}

	e := &legacyAdoptE2E{
		t:          t,
		sourceRoot: sourceRoot,
		sourceWS:   sourceWS,
		dataRoot:   dataRoot,
		config:     config,
		key:        key,
		keyPath:    keyPath,
		keyFileSHA: keyFileSHA,
		dbBytes:    dbBytes,
		params:     params,
	}
	// 副本：库与配置逐字节复制自源端，名册按清单声明复制。
	copyRoot := t.TempDir()
	copyWS := filepath.Join(copyRoot, workspaceDirName)
	require.NoError(t, os.MkdirAll(copyWS, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(copyWS, databaseFileName), dbBytes, 0o600))
	if roster != nil {
		require.NoError(t, os.WriteFile(filepath.Join(copyWS, instanceMarkerFileName), roster, 0o600))
	}
	e.copyRoot = copyRoot
	e.copyWS = copyWS
	e.configPath = filepath.Join(copyRoot, configFileName)
	require.NoError(t, os.WriteFile(e.configPath, config, 0o600))

	fingerprint, err := MasterKeyFingerprint(key)
	require.NoError(t, err)
	manifest := legacyAdoptManifest(dbBytes, config)
	manifest.SchemaVersion = params.Schema
	manifest.CiphertextCount = params.Creds
	manifest.KeyID = key.KeyID
	manifest.KeyFingerprint = fingerprint
	manifest.KeyFileSHA256 = keyFileSHA
	if roster != nil {
		marker, markerErr := parseInstanceMarker(roster)
		require.NoError(t, markerErr)
		manifest.RosterPresent = true
		manifest.RosterSHA256 = hexSHA256(roster)
		manifest.InstanceID = marker.InstanceID
	}
	e.manifest = manifest
	e.rebuildBackup(nil)
	return e
}

// setConfig 把新配置同时落到源端与副本，并同步清单里的两个配置哈希（库哈希不变）：
// "配置漂移"类用例必须先让三处一致，否则拒绝发生在更早的副本一致性门上。
func (e *legacyAdoptE2E) setConfig(body []byte) {
	e.t.Helper()
	e.config = body
	require.NoError(e.t, os.WriteFile(filepath.Join(e.sourceRoot, configFileName), body, 0o600))
	require.NoError(e.t, os.WriteFile(e.configPath, body, 0o600))
	e.manifest.SourceConfigSHA256 = hexSHA256(body)
	e.manifest.BackupConfigSHA256 = hexSHA256(body)
	e.rebuildBackup(nil)
}

// buildLegacySQLite 在 path 上造一座 schema v3 / v5 的真库：一条完成态操作行（凭据行的
// `operation_id` 外键指向它）与 N 条托管凭据。密文由 `SealWith`（默认源端密钥）封，
// 库里写的 `key_id` 恒为源端那把——两档故意解耦，才能造出"标识相符但解不开"。
//
// 凭据行走原始 SQL 而不是 `CredentialStore.Create`：v5 的 `credentials` 还没有
// `binding_key`，Create 的列清单对不上旧谱系。
// Credential rows go in through raw SQL because the v5 credentials table predates
// binding_key, so the current store cannot write the old lineage.
func buildLegacySQLite(t *testing.T, path string, params legacyE2EParams, key MasterKey) {
	t.Helper()
	ctx := context.Background()
	// `schema.sql` 已是**全量当前 schema**（每张表 `IF NOT EXISTS`），仓库不再保留
	// v1 基线，所以旧谱系只能按"删掉 v6 增量"还原：去掉 v6 才新建的四张表，版本行
	// 停在目标版本。受控升级实际读的三件事——schema_version、`instance_secrets`
	// 缺表、`credentials` 行——都与真 v3 / v5 库同形。
	created, err := storage.Bootstrap(ctx, path, params.Schema)
	require.NoError(t, err)
	require.True(t, created, "夹具必须是新建库")

	db, err := storage.OpenWritable(ctx, path, params.Schema)
	require.NoError(t, err)
	store := storage.NewManagementStore(db)
	require.NoError(t, store.BeginOperation(ctx, core.OperationRecord{
		OperationID:  legacySeedOperationID,
		Action:       "system.test_seed",
		TargetKind:   core.TargetKindInstance,
		TargetID:     core.InstanceTargetMasterKey,
		ActorSubject: "cli:test",
		AuthMethod:   "cli",
		Source:       "cli",
		Status:       core.OperationPending,
		CreatedAt:    baselineClock(),
	}))
	require.NoError(t, store.FinishOperation(ctx, legacySeedOperationID,
		core.OperationSuccess, "", "", nil, baselineClock()))
	require.NoError(t, db.Close())
	// 关净句柄即检查点并删除 WAL 索引文件——后面三份副本要逐字节相同。
	require.NoFileExists(t, path+"-wal")
	require.NoFileExists(t, path+"-shm")

	raw := openRawSQLite(t, path)
	defer func() { require.NoError(t, raw.Close()) }()
	// 逐代撤掉比目标版本更新的增量：这些对象由 0004 / 0005 / 0006 建立，撤掉之后
	// `Migrate(6)` 才会真的逐版前进（不撤则 ALTER TABLE 撞重复列、CREATE 变空操作，
	// 迁移机制等于没被验证）。
	// Undo every increment newer than the target so Migrate really walks the chain.
	stale := []string{
		"instance_secrets", "execution_policy", "network_exceptions", "mcp_credential_sources",
	}
	if params.Schema < 5 {
		stale = append(stale, "network_domain_policy")
	}
	if params.Schema < 4 {
		stale = append(stale, "schedule_deliveries", "resource_connection_tests")
	}
	for _, table := range stale {
		_, dropErr := raw.Exec(`DROP TABLE IF EXISTS "` + table + `"`)
		require.NoError(t, dropErr)
	}
	if params.Schema < 4 {
		_, dropErr := raw.Exec(`ALTER TABLE scheduled_tasks DROP COLUMN deliver_to`)
		require.NoError(t, dropErr)
	}
	now := baselineClock().UnixMilli()
	for i := 0; i < params.Creds; i++ {
		ownerUID, credentialID := legacyCredentialIDs(i)
		sealKey := key
		if params.SealWith != nil {
			sealKey = *params.SealWith
		}
		aad := CredentialAAD{
			OwnerKind:     core.BindingModelConnection,
			OwnerUID:      ownerUID,
			CredentialID:  credentialID,
			VersionNo:     1,
			TargetBinding: adoptTargetID,
		}
		nonce, ciphertext, sealErr := SealCredential(sealKey, aad, []byte("sk-legacy-plaintext"))
		require.NoError(t, sealErr)
		if params.CorruptCiphertext {
			ciphertext[0] ^= 0xff
		}
		_, execErr := raw.Exec(
			`INSERT INTO credentials (credential_id, owner_kind, owner_uid, owner_name, state,
			                          current_version, key_id, created_at, updated_at, operation_id)
			 VALUES (?, ?, ?, ?, 'active', 1, ?, ?, ?, ?)`,
			credentialID, string(core.BindingModelConnection), string(ownerUID), "legacy-openai",
			key.KeyID, now, now, legacySeedOperationID)
		require.NoError(t, execErr)
		_, execErr = raw.Exec(
			`INSERT INTO credential_versions (credential_id, version_no, state, key_id,
			                                  target_binding, nonce, ciphertext, created_at,
			                                  updated_at, operation_id)
			 VALUES (?, 1, 'active', ?, ?, ?, ?, ?, ?, ?)`,
			credentialID, key.KeyID, adoptTargetID, nonce, ciphertext, now, now, legacySeedOperationID)
		require.NoError(t, execErr)
		if params.ClearTargetBinding {
			// 先按合法 AAD 封，再把绑定列清空：`SealCredential` 自己会拒非法 AAD，
			// 本用例要判的是 adopt-legacy 侧的 `unusableAAD` 守卫。
			_, updateErr := raw.Exec(
				`UPDATE credential_versions SET target_binding = '' WHERE credential_id = ?`, credentialID)
			require.NoError(t, updateErr)
		}
	}
	_, err = raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)
}

// legacyCredentialIDs 给出第 i 条凭据的（资源 UID, 凭据 ID）：两者都是领域校验过的
// 固定字面量，且逐条不同（`UNIQUE (owner_kind, owner_uid)`）。
func legacyCredentialIDs(i int) (core.ResourceUID, string) {
	owner := core.ResourceUID(fmt.Sprintf("rs_%026d", 100+i))
	return owner, fmt.Sprintf("cred_%026d", 100+i)
}

// openRawSQLite 用一个独立的原始连接读写夹具库：`storage.DB` 不对外暴露执行面，
// 而造 jig 与逐表计数都必须真写 / 真读。PRAGMA 与生产 DSN 同源（外键 + 忙等）。
func openRawSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	// 路径里的 `%` / `#` / `?` 由 t.TempDir() 保证不会出现，因此这里不做 URI 转义。
	db, err := sql.Open("sqlite",
		"file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	return db
}

// rawQueryInt 执行一条只回单个整数的查询。
func rawQueryInt(t *testing.T, path, query string, args ...any) int {
	t.Helper()
	db := openRawSQLite(t, path)
	defer func() { require.NoError(t, db.Close()) }()
	var value int
	require.NoError(t, db.QueryRow(query, args...).Scan(&value))
	return value
}

// rawCount 数一张表的行数；表不存在返回 -1（受控升级之前副本还没有 `instance_secrets`）。
func rawCount(t *testing.T, path, table string) int {
	t.Helper()
	if rawQueryInt(t, path,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table) == 0 {
		return -1
	}
	return rawQueryInt(t, path, `SELECT count(*) FROM "`+table+`"`)
}

// rawSchemaVersion 读 schema 版本（对旧谱系同样可用）。
func rawSchemaVersion(t *testing.T, path string) int {
	t.Helper()
	return rawQueryInt(t, path, `SELECT max(version) FROM schema_version`)
}

// rawTableCounts 逐表数行：判"除许可写入之外没有任何表被动过"。
func rawTableCounts(t *testing.T, path string) map[string]int {
	t.Helper()
	db := openRawSQLite(t, path)
	defer func() { require.NoError(t, db.Close()) }()
	rows, err := db.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	require.NoError(t, err)
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())

	counts := make(map[string]int, len(names))
	for _, name := range names {
		var count int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM "`+name+`"`).Scan(&count))
		counts[name] = count
	}
	return counts
}

// sealedDigests 列出全部含密文行的公开摘要（不含明文）：迁移前后必须逐条相同。
// sealedDigests lists every ciphertext row's public shape, never plaintext.
func sealedDigests(t *testing.T, path string) []string {
	t.Helper()
	db := openRawSQLite(t, path)
	defer func() { require.NoError(t, db.Close()) }()
	rows, err := db.Query(
		`SELECT c.credential_id, v.version_no, v.key_id, v.target_binding, v.nonce, v.ciphertext
		   FROM credential_versions v JOIN credentials c ON c.credential_id = v.credential_id
		  WHERE v.ciphertext IS NOT NULL`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var out []string
	for rows.Next() {
		var (
			credentialID string
			versionNo    int
			keyID        string
			target       string
			nonce        []byte
			ciphertext   []byte
		)
		require.NoError(t, rows.Scan(&credentialID, &versionNo, &keyID, &target, &nonce, &ciphertext))
		out = append(out, fmt.Sprintf("%s|%d|%s|%s|%s|%s", credentialID, versionNo, keyID, target,
			hex.EncodeToString(nonce), hex.EncodeToString(ciphertext)))
	}
	sort.Strings(out)
	return out
}

// treeDigest 逐文件算摘要：判"源端 / 备份一个字节都没动"。
func treeDigest(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out[rel] = hexSHA256(raw)
		return nil
	}))
	return out
}

// legacyAdoptSnapshot 是拒绝路径的零写入读数：五处彼此独立，任一处变化都说明叛了。
// legacyAdoptSnapshot is five independent zero-write readings.
type legacyAdoptSnapshot struct {
	copyDBHash   string
	copyOpsRows  int
	secretsRows  int
	copyFiles    map[string]string
	sourceDigest map[string]string
	backupDigest map[string]string
}

// adopt 发起一次受控升级；`mutate` 改写请求（缺省：本地源端见证 + 显式确认）。
func (e *legacyAdoptE2E) adopt(mutate ...func(*cli.AdoptLegacyRequest)) (cli.AdoptLegacyResult, error) {
	e.t.Helper()
	req := cli.AdoptLegacyRequest{
		Workspace:      e.copyWS,
		Backup:         e.backupDir,
		ManifestSHA256: e.manifestSHA,
		Source:         e.sourceWS,
		Confirm:        true,
	}
	for _, fn := range mutate {
		fn(&req)
	}
	return legacyAdoptPort{Now: baselineClock}.AdoptLegacySecrets(context.Background(), req)
}

// rebuildBackup 把清单落盘到**新的**备份目录（清单内容由 `mutate` 改写，哈希不重算——
// "清单声明与盘上文件不符"正是要测的形态之一）。`e.backupDir` / `e.manifestSHA` 随之更新。
func (e *legacyAdoptE2E) rebuildBackup(mutate func(*legacyBackupManifest)) {
	e.t.Helper()
	if mutate != nil {
		mutate(&e.manifest)
	}
	files := map[string][]byte{
		databaseFileName: e.dbBytes,
		configFileName:   e.config,
	}
	if e.params.Roster != nil {
		files[instanceMarkerFileName] = e.params.Roster
	}
	e.backupDir, e.manifestSHA = writeLegacyAdoptBackup(e.t, e.manifest, files)
}

// snapshot 采一次零写入读数。
func (e *legacyAdoptE2E) snapshot() legacyAdoptSnapshot {
	e.t.Helper()
	copyDB, err := os.ReadFile(filepath.Join(e.copyWS, databaseFileName))
	require.NoError(e.t, err)
	return legacyAdoptSnapshot{
		copyDBHash:   hexSHA256(copyDB),
		copyOpsRows:  rawCount(e.t, filepath.Join(e.copyWS, databaseFileName), "management_operations"),
		secretsRows:  rawCount(e.t, filepath.Join(e.copyWS, databaseFileName), "instance_secrets"),
		copyFiles:    treeDigest(e.t, e.copyWS),
		sourceDigest: treeDigest(e.t, e.sourceRoot),
		backupDigest: treeDigest(e.t, e.backupDir),
	}
}

// requireUnchanged 断言拒绝路径真的一个字节都没落：副本库、三张表的行数、源端与备
// 份整棵树都不变。只看"返回了错误"不足以证明零写。
func (e *legacyAdoptE2E) requireUnchanged(before legacyAdoptSnapshot) {
	e.t.Helper()
	after := e.snapshot()
	require.Equal(e.t, before.copyDBHash, after.copyDBHash, "拒绝路径不得改动副本库字节")
	require.Equal(e.t, before.copyOpsRows, after.copyOpsRows, "拒绝路径不得写管理操作行")
	require.Equal(e.t, before.secretsRows, after.secretsRows, "拒绝路径不得写 instance_secrets")
	require.Equal(e.t, before.backupDigest, after.backupDigest, "拒绝路径不得改动备份目录")
}

// copyRecord 读回副本的初始化记录。
func (e *legacyAdoptE2E) copyRecord() (storage.InstanceSecretRecord, bool) {
	e.t.Helper()
	ctx := context.Background()
	db, err := storage.OpenReadOnly(ctx, filepath.Join(e.copyWS, databaseFileName),
		storage.SupportedSchemaVersion)
	require.NoError(e.t, err)
	defer func() { require.NoError(e.t, db.Close()) }()
	record, exists, err := storage.NewInstanceSecretsStore(db).
		Load(ctx, core.InstanceSecretScopeMasterKey)
	require.NoError(e.t, err)
	return record, exists
}

// operation 读回一条管理操作行。
func (e *legacyAdoptE2E) operation(opID string) core.OperationRecord {
	e.t.Helper()
	ctx := context.Background()
	db, err := storage.OpenReadOnly(ctx, filepath.Join(e.copyWS, databaseFileName),
		storage.SupportedSchemaVersion)
	require.NoError(e.t, err)
	defer func() { require.NoError(e.t, db.Close()) }()
	op, err := storage.NewManagementStore(db).GetOperation(ctx, opID)
	require.NoError(e.t, err)
	return op
}

// decodeDetail 解出操作行的 `detail_json`。
func decodeDetail(t *testing.T, op core.OperationRecord) map[string]any {
	t.Helper()
	var detail map[string]any
	require.NoError(t, json.Unmarshal(op.DetailJSON, &detail))
	return detail
}

// TestSecretsAdoptLegacy_UpgradesLegacySchemaAndWritesRecord 是 C-51 的主路径。
//
// 中文验收点：schema 3 与 5 都能升到 6；副本多出恰好一条 `record_origin=attested_legacy`
// 记录与一条 success 的 `instance.secrets_legacy_adopt` 审计；密文行逐字节未变；
// 源端与备份一个字节都没动；身份证据档位落 `attested_no_roster`，不得写成"已证明"。
// TestSecretsAdoptLegacy_UpgradesLegacySchemaAndWritesRecord pins the happy path.
func TestSecretsAdoptLegacy_UpgradesLegacySchemaAndWritesRecord(t *testing.T) {
	for _, schema := range []int{3, 5} {
		t.Run(fmt.Sprintf("schema_v%d", schema), func(t *testing.T) {
			e := newLegacyAdoptE2E(t, func(p *legacyE2EParams) { p.Schema = schema })
			before := e.snapshot()
			sealedBefore := sealedDigests(t, filepath.Join(e.copyWS, databaseFileName))
			require.Len(t, sealedBefore, 1, "护栏：密文摘要为空时下面的逐条相等断言会空过")

			result, err := e.adopt()
			require.NoError(t, err)
			require.Equal(t, "completed", result.FinalState)
			require.Equal(t, schema, result.SchemaBefore)
			require.Equal(t, storage.SupportedSchemaVersion, result.SchemaAfter)
			require.Equal(t, 1, result.Ciphertexts)
			require.Equal(t, "source", result.SourceWitness)
			require.Equal(t, "config_path", result.KeySource)
			require.Equal(t, "attested_no_roster", result.IdentityEvidence)
			require.False(t, result.RosterPresent)
			require.NotEmpty(t, result.OperationID)
			require.Equal(t, e.keyFileSHA, result.KeyFileSHA256)

			record, exists := e.copyRecord()
			require.True(t, exists, "受控升级必须写初始化记录")
			require.Equal(t, e.key.KeyID, record.KeyID)
			require.Equal(t, result.KeyFingerprint, record.Fingerprint)
			require.Equal(t, result.OperationID, record.OperationID,
				"记录必须指向本次操作——它就是 instance adopt 的证据链起点")

			op := e.operation(result.OperationID)
			require.Equal(t, core.OperationStatus(core.ActionInstanceSecretsLegacyAdopt),
				core.OperationStatus(op.Action), "动作字面量必须是 §3.1.8c 冻结点")
			require.Equal(t, core.OperationSuccess, op.Status)
			detail := decodeDetail(t, op)
			require.Equal(t, "attested_legacy", detail["record_origin"])
			require.Equal(t, "attested_no_roster", detail["identity_evidence"])
			require.Equal(t, "source", detail["source_witness"])
			require.Equal(t, float64(1), detail["ciphertext_count"])
			require.Equal(t, float64(schema), detail["schema_before"])
			require.Equal(t, float64(storage.SupportedSchemaVersion), detail["schema_after"])
			require.Equal(t, e.manifest.AnchorRef, detail["anchor_ref"])
			require.Equal(t, e.manifestSHA, detail["backup_manifest_sha256"])
			require.NotContains(t, string(op.DetailJSON), "sk-legacy-plaintext",
				"审计详情不得含明文")

			copyDB := filepath.Join(e.copyWS, databaseFileName)
			require.Equal(t, storage.SupportedSchemaVersion, rawSchemaVersion(t, copyDB))
			require.Equal(t, sealedBefore, sealedDigests(t, copyDB),
				"迁移只改表结构，密文行必须逐字节不变")
			require.Equal(t, 1, rawCount(t, copyDB, "instance_secrets"))
			require.Equal(t, before.copyOpsRows+1, rawCount(t, copyDB, "management_operations"))

			// 源端与备份零改动：副本是唯一被写的对象。
			after := e.snapshot()
			require.Equal(t, before.sourceDigest, after.sourceDigest, "源端不得被写")
			require.Equal(t, before.backupDigest, after.backupDigest, "备份是只读基线")
			require.Empty(t, treeDigest(t, e.dataRoot), "离线命令不得写用户数据根")
		})
	}
}

// TestSecretsAdoptLegacy_RefusesSourceDrift 判源端漂移门（§2.12.0b 校验 9）。
//
// 中文验收点：源端库或配置在备份之后变过 → `source_changed` 且副本零写入；
// 源端不可读时带外见证可用（`source_witness=attestation`），但**不得**写成已验；
// 两种见证都没给 → `attestation_missing`。
// TestSecretsAdoptLegacy_RefusesSourceDrift pins the drift gate and the two witnesses.
func TestSecretsAdoptLegacy_RefusesSourceDrift(t *testing.T) {
	t.Run("source_database_changed", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		before := e.snapshot()
		// 备份之后源端又写了一条记录：库字节变了。
		require.NoError(t, os.WriteFile(filepath.Join(e.sourceWS, databaseFileName),
			append(append([]byte{}, e.dbBytes...), 0x00), 0o600))

		result, err := e.adopt()
		requireOfflineRefusal(t, err, legacyCodeSourceChanged, "源端库在备份之后发生了变化")
		require.Equal(t, "not_applied", result.FinalState)
		require.Equal(t, "source", result.SourceWitness, "已经走到源端核对这一步")
		e.requireUnchanged(before)
	})

	t.Run("source_config_changed", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		before := e.snapshot()
		drifted := append(append([]byte{}, e.config...), []byte("# operator note\n")...)
		require.NoError(t, os.WriteFile(filepath.Join(e.sourceRoot, configFileName), drifted, 0o600))

		_, err := e.adopt()
		requireOfflineRefusal(t, err, legacyCodeSourceChanged, "源端 basalt.yaml 在备份之后发生了变化")
		e.requireUnchanged(before)
	})

	t.Run("attestation_accepted_when_source_unreadable", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		result, err := e.adopt(func(req *cli.AdoptLegacyRequest) {
			req.Source = ""
			req.AttestSourceUnchanged = "OPS-2026-008"
		})
		require.NoError(t, err)
		require.Equal(t, "attestation", result.SourceWitness,
			"带外见证是 attested，不是已验——档位不得升格")
		require.Equal(t, "completed", result.FinalState)
	})

	t.Run("no_witness_is_refused", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		before := e.snapshot()
		_, err := e.adopt(func(req *cli.AdoptLegacyRequest) { req.Source = "" })
		requireOfflineRefusal(t, err, legacyCodeAttestationMissing, "--attest-source-unchanged")
		e.requireUnchanged(before)
	})
}

// TestSecretsAdoptLegacy_RefusesUntrustedBackup 判"备份不可信"的各类形态。
//
// 中文验收点：清单读不到 / 清单字节与外部锚点不符 / 清单声明备份已带记录 / 清单声明
// v6 / 备份文件与清单声明不符 / 副本配置与备份不一致——逐条拒绝且副本零写入。
// TestSecretsAdoptLegacy_RefusesUntrustedBackup walks the untrusted-backup shapes.
func TestSecretsAdoptLegacy_RefusesUntrustedBackup(t *testing.T) {
	t.Run("missing_backup_json", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		before := e.snapshot()
		empty := t.TempDir()
		_, err := e.adopt(func(req *cli.AdoptLegacyRequest) { req.Backup = empty })
		requireOfflineRefusal(t, err, legacyCodeBackupInvalid, "读取 backup.json 失败")
		e.requireUnchanged(before)
	})

	t.Run("manifest_changed_after_anchor", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		before := e.snapshot()
		_, err := e.adopt(func(req *cli.AdoptLegacyRequest) {
			req.ManifestSHA256 = strings.Repeat("0", 64)
		})
		requireOfflineRefusal(t, err, legacyCodeBackupInvalid, "在命令面核对之后被改动")
		e.requireUnchanged(before)
	})

	t.Run("records_absent_false", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		e.rebuildBackup(func(m *legacyBackupManifest) { m.RecordsAbsent = false })
		before := e.snapshot()
		_, err := e.adopt()
		requireOfflineRefusal(t, err, legacyCodeBackupRecordPresent, "secrets recover-record")
		e.requireUnchanged(before)
	})

	t.Run("backup_declares_current_schema", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		e.rebuildBackup(func(m *legacyBackupManifest) {
			m.SchemaVersion = storage.SupportedSchemaVersion
		})
		before := e.snapshot()
		_, err := e.adopt()
		requireOfflineRefusal(t, err, legacyCodeStateNotLegacy, "已是新谱系")
		e.requireUnchanged(before)
	})

	t.Run("backup_file_disagrees_with_manifest", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		// 清单自证哈希是对的，但盘上的文件在清单之后被换过：重算即露。
		tampered := append(append([]byte{}, e.dbBytes...), 0x00)
		require.NoError(t, os.WriteFile(filepath.Join(e.backupDir, databaseFileName), tampered, 0o600))
		before := e.snapshot()
		_, err := e.adopt()
		requireOfflineRefusal(t, err, legacyCodeBackupInvalid, "备份文件 basalt.db 与清单声明不符")
		e.requireUnchanged(before)
	})

	t.Run("copy_config_drifted_from_backup", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		drifted := append(append([]byte{}, e.config...), []byte("# local edit\n")...)
		require.NoError(t, os.WriteFile(e.configPath, drifted, 0o600))
		before := e.snapshot()
		_, err := e.adopt()
		requireOfflineRefusal(t, err, legacyCodeBackupInvalid, "副本 basalt.yaml 与备份不一致")
		e.requireUnchanged(before)
	})
}

// TestSecretsAdoptLegacy_RefusesWrongKeyOrCorruptCiphertext 判"密钥 / 密文连续"。
//
// 中文验收点：key_id 相符但密文是另一把钥匙封的 → `ciphertext_undecryptable`；
// 密文被改一个字节 → 同一码；AAD 字段重建不出 → `aad_mismatch`；配置指向另一把
// 钥匙 → `key_unverified`；四种都零写入。
// TestSecretsAdoptLegacy_RefusesWrongKeyOrCorruptCiphertext pins the continuity gate.
func TestSecretsAdoptLegacy_RefusesWrongKeyOrCorruptCiphertext(t *testing.T) {
	otherKey := testMasterKey()
	otherKey.KeyID = "k_other"
	otherKey.Key = make([]byte, 32)
	for i := range otherKey.Key {
		otherKey.Key[i] = byte(200 - i)
	}
	t.Cleanup(func() { otherKey.Zero() })

	t.Run("ciphertext_sealed_with_another_key", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, func(p *legacyE2EParams) { p.SealWith = &otherKey })
		before := e.snapshot()
		_, err := e.adopt()
		requireOfflineRefusal(t, err, legacyCodeCiphertextUndecryptable, "无法用原密钥解开")
		e.requireUnchanged(before)
	})

	t.Run("ciphertext_corrupted_byte", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, func(p *legacyE2EParams) { p.CorruptCiphertext = true })
		before := e.snapshot()
		_, err := e.adopt()
		requireOfflineRefusal(t, err, legacyCodeCiphertextUndecryptable, "无法用原密钥解开")
		e.requireUnchanged(before)
	})

	t.Run("aad_field_unusable", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, func(p *legacyE2EParams) { p.ClearTargetBinding = true })
		before := e.snapshot()
		_, err := e.adopt()
		requireOfflineRefusal(t, err, legacyCodeAADMismatch, "AAD 字段无法重建")
		e.requireUnchanged(before)
	})

	t.Run("config_points_at_another_key", func(t *testing.T) {
		// 配置里的路径指向了另一把钥匙：先撞"密钥文件哈希与清单不符"。取钥来源是
		// 配置路径（不是备份外覆盖），所以类别是"备份自证对不上"。
		otherPath := writeMasterKeyFile(t, filepath.Join(t.TempDir(), "private"),
			otherKey.KeyID, otherKey.Key)
		e := newLegacyAdoptE2E(t, func(p *legacyE2EParams) { p.ConfigKeyPath = otherPath })
		before := e.snapshot()
		_, err := e.adopt()
		requireOfflineRefusal(t, err, legacyCodeAttestationMismatch, "密钥文件哈希与清单不符")
		e.requireUnchanged(before)
	})

	t.Run("manifest_key_id_disagrees_with_file", func(t *testing.T) {
		// 文件哈希相符、key_id 不符：清单与文件说的不是同一把钥匙。
		e := newLegacyAdoptE2E(t, nil)
		e.rebuildBackup(func(m *legacyBackupManifest) { m.KeyID = "mk_other" })
		before := e.snapshot()
		_, err := e.adopt()
		requireOfflineRefusal(t, err, legacyCodeKeyUnverified, "key_id 与清单不符")
		e.requireUnchanged(before)
	})
}

// TestSecretsAdoptLegacy_NoRuntimeSideEffects 判"离线命令不装配运行期"。
//
// 中文验收点：受控升级的写入只有三处（3～5 → 6 迁移、1 条记录、1 条审计）——
// 除 `schema_version` / `management_operations` / `instance_secrets` 与迁移自建的
// 空表之外，**任何**表的行数都不得变；源端、备份、用户数据根都零写入；
// 不生成密钥、不写名册、不改配置。
// TestSecretsAdoptLegacy_NoRuntimeSideEffects pins the sanctioned write set.
func TestSecretsAdoptLegacy_NoRuntimeSideEffects(t *testing.T) {
	e := newLegacyAdoptE2E(t, nil)
	copyDB := filepath.Join(e.copyWS, databaseFileName)
	before := rawTableCounts(t, copyDB)
	require.NotEmpty(t, before, "护栏：读不到表清单时逐表零变化断言会空过")
	configBefore := hexSHA256(e.config)

	result, err := e.adopt()
	require.NoError(t, err)
	require.Equal(t, "completed", result.FinalState)

	after := rawTableCounts(t, copyDB)
	for table, count := range before {
		switch table {
		case "schema_version":
			require.Equal(t, count+1, after[table], "受控升级只追加一行 v6 版本行")
		case "management_operations":
			require.Equal(t, count+1, after[table], "只允许一条审计行")
		default:
			require.Equal(t, count, after[table],
				"受控升级只能迁移 + 写记录 + 写审计，不得改动表 %s", table)
		}
	}
	// 新增表只有两种来源：v6 迁移建表（`execution_policy` 由迁移播种一行），与那条记录。
	require.Equal(t, 1, after["instance_secrets"], "只允许一条记录")
	require.Equal(t, 1, after["execution_policy"], "迁移只播种一行 strict 档位")
	for table, count := range after {
		if _, existed := before[table]; existed {
			continue
		}
		switch table {
		case "schema_version", "management_operations", "instance_secrets", "execution_policy":
			continue
		}
		require.Equal(t, 0, count, "迁移新建的表 %s 不得被本命令写行", table)
	}

	// 不写名册、不改配置、不生成密钥、不碰用户数据根。
	_, rosterPresent, err := readInstanceMarkerAt(e.copyWS)
	require.NoError(t, err)
	require.False(t, rosterPresent, "受控升级不写名册（身份归 instance adopt）")
	afterConfig, err := os.ReadFile(e.configPath)
	require.NoError(t, err)
	require.Equal(t, configBefore, hexSHA256(afterConfig), "受控升级不改配置")
	require.Empty(t, treeDigest(t, e.dataRoot), "不得生成主密钥 / 私有目录")
}

// TestSecretsAdoptLegacy_NoBackupReportsDisposal 判"没有可用备份时的处置口径"。
//
// 中文验收点：备份目录不存在 → `backup_invalid` 且文案点名 `backup.json`；
// 副本已有记录 → `record_already_exists`（幂等，**不覆盖**既有记录，重做只能重新复制）；
// 副本是迁移到一半的残骸（v6 但无记录）→ `state_not_legacy`，文案要求丢弃副本重新复制。
// TestSecretsAdoptLegacy_NoBackupReportsDisposal pins the disposal guidance.
func TestSecretsAdoptLegacy_NoBackupReportsDisposal(t *testing.T) {
	t.Run("absent_backup_names_the_missing_artifact", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		before := e.snapshot()
		_, err := e.adopt(func(req *cli.AdoptLegacyRequest) {
			req.Backup = filepath.Join(t.TempDir(), "does-not-exist")
		})
		requireOfflineRefusal(t, err, legacyCodeBackupInvalid, "backup.json")
		e.requireUnchanged(before)
	})

	// 重跑与残骸两种形态都要先造出"半成品"：先真跑一次，再把副本现状当成新的备份基线。
	seedMigrated := func(t *testing.T) *legacyAdoptE2E {
		t.Helper()
		e := newLegacyAdoptE2E(t, nil)
		first, err := e.adopt()
		require.NoError(t, err)
		require.Equal(t, "completed", first.FinalState)
		return e
	}

	t.Run("rerun_refuses_existing_record", func(t *testing.T) {
		e := seedMigrated(t)
		record, exists := e.copyRecord()
		require.True(t, exists)

		// 把已迁移的副本当成新备份基线：清单改回 v5 形态，库哈希按迁移后的字节重算。
		migrated, err := os.ReadFile(filepath.Join(e.copyWS, databaseFileName))
		require.NoError(t, err)
		e.dbBytes = migrated
		e.rebuildBackup(func(m *legacyBackupManifest) {
			m.SchemaVersion = 5
			m.DBSHA256 = hexSHA256(migrated)
		})

		before := e.snapshot()
		result, retryErr := e.adopt()
		requireOfflineRefusal(t, retryErr, legacyCodeRecordAlreadyExists, "从备份重新复制")
		require.Equal(t, "not_applied", result.FinalState)
		e.requireUnchanged(before)

		after, stillThere := e.copyRecord()
		require.True(t, stillThere)
		require.Equal(t, record.OperationID, after.OperationID, "重跑不得覆盖既有记录")
		require.Equal(t, record.Fingerprint, after.Fingerprint)
	})

	t.Run("partial_migration_copy_is_disposed", func(t *testing.T) {
		e := seedMigrated(t)
		copyDB := filepath.Join(e.copyWS, databaseFileName)

		// 造残骸：v6 谱系在，记录没了（模拟崩溃在迁移与写记录之间）。
		raw := openRawSQLite(t, copyDB)
		_, err := raw.Exec(`DELETE FROM instance_secrets`)
		require.NoError(t, err)
		_, err = raw.Exec(`DELETE FROM management_operations WHERE action = ?`,
			core.ActionInstanceSecretsLegacyAdopt)
		require.NoError(t, err)
		require.NoError(t, raw.Close())

		partial, err := os.ReadFile(copyDB)
		require.NoError(t, err)
		e.dbBytes = partial
		e.rebuildBackup(func(m *legacyBackupManifest) {
			m.SchemaVersion = 5
			m.DBSHA256 = hexSHA256(partial)
		})

		before := e.snapshot()
		_, refuseErr := e.adopt()
		requireOfflineRefusal(t, refuseErr, legacyCodeStateNotLegacy,
			"丢弃副本，从备份重新复制再跑")
		e.requireUnchanged(before)
	})
}

// TestSecretsAdoptLegacy_RefusesRosterBeforeAdoption 判名册门在**采纳之前**。
//
// 中文验收点：副本有不在清单里的名册 → `backup_identity_mismatch` 且零写入；
// 清单与副本名册相符 → 放行，证据档位升到 `roster`，审计里成组出现
// `roster_sha256` / `instance_id`（无册时整组缺席）。
// TestSecretsAdoptLegacy_RefusesRosterBeforeAdoption pins the roster gate.
func TestSecretsAdoptLegacy_RefusesRosterBeforeAdoption(t *testing.T) {
	roster, err := marshalInstanceMarker(instanceMarker{
		Version:    instanceMarkerVersion,
		InstanceID: "inst_0123456789abcdef",
	})
	require.NoError(t, err)

	t.Run("copy_carries_unbacked_roster", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		require.NoError(t, os.WriteFile(filepath.Join(e.copyWS, instanceMarkerFileName),
			roster, 0o600))
		before := e.snapshot()
		_, err := e.adopt()
		requireOfflineRefusal(t, err, legacyCodeIdentityMismatch, "副本已有名册")
		e.requireUnchanged(before)
	})

	t.Run("matching_roster_is_accepted_as_roster_evidence", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, func(p *legacyE2EParams) { p.Roster = roster })
		result, err := e.adopt()
		require.NoError(t, err)
		require.True(t, result.RosterPresent)
		require.Equal(t, "roster", result.IdentityEvidence)

		detail := decodeDetail(t, e.operation(result.OperationID))
		require.Equal(t, true, detail["roster_present"])
		require.Equal(t, hexSHA256(roster), detail["roster_sha256"])
		require.Equal(t, "inst_0123456789abcdef", detail["instance_id"])
	})
}

// TestSecretsAdoptLegacy_RecordWithoutRosterIsNotExportable 判"记录不等于身份"。
//
// 中文验收点：受控升级**不**写名册；因此刚采纳过的副本仍不能 `db export`——
// 导出以名册为唯一的实例标识来源，缺册即 `instance_identity_missing`，
// 且文案指向 `instance adopt`，不得当作新实例、不得自动补写名册。
// TestSecretsAdoptLegacy_RecordWithoutRosterIsNotExportable keeps the two steps apart.
func TestSecretsAdoptLegacy_RecordWithoutRosterIsNotExportable(t *testing.T) {
	e := newLegacyAdoptE2E(t, nil)
	_, err := e.adopt()
	require.NoError(t, err)

	_, rosterPresent, err := readInstanceMarkerAt(e.copyWS)
	require.NoError(t, err)
	require.False(t, rosterPresent, "受控升级不写名册")

	out := filepath.Join(t.TempDir(), "package")
	_, exportErr := migrationPort{Now: baselineClock}.Export(context.Background(),
		cli.MigrationExportRequest{Workspace: e.copyWS, Out: out})
	require.Error(t, exportErr)
	require.ErrorIs(t, exportErr, errInstanceIdentityMissing,
		"有记录但无名册的副本不得被当成可导出的实例")
	require.Contains(t, exportErr.Error(), "instance adopt")
	require.NoDirExists(t, out)
}

// TestSecretsAdoptLegacy_KeyOverrideRequiresProvenance 判取钥来源的锚点要求（K2）。
//
// 中文验收点：备份外取钥（`--master-key-file`）**必须**带 `--expect-key-sha256`；
// 锚点不符即 `key_unverified`；锚点相符才放行并把 `key_source` 记成 `external_anchor`；
// 配置里没有密钥路径时，在算规范化摘要这一步就以 `master_key_path_unpersisted` 拒绝。
// TestSecretsAdoptLegacy_KeyOverrideRequiresProvenance pins the K2 anchor rules.
func TestSecretsAdoptLegacy_KeyOverrideRequiresProvenance(t *testing.T) {
	t.Run("external_key_without_anchor", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		before := e.snapshot()
		_, err := e.adopt(func(req *cli.AdoptLegacyRequest) { req.MasterKeyFile = e.keyPath })
		requireOfflineRefusal(t, err, legacyCodeKeyUnverified, "必须带 --expect-key-sha256")
		e.requireUnchanged(before)
	})

	t.Run("external_key_with_wrong_anchor", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		before := e.snapshot()
		_, err := e.adopt(func(req *cli.AdoptLegacyRequest) {
			req.MasterKeyFile = e.keyPath
			req.ExpectKeySHA256 = strings.Repeat("0", 64)
		})
		requireOfflineRefusal(t, err, legacyCodeKeyUnverified, "与外部锚点不符")
		e.requireUnchanged(before)
	})

	t.Run("external_key_with_matching_anchor", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		result, err := e.adopt(func(req *cli.AdoptLegacyRequest) {
			req.MasterKeyFile = e.keyPath
			req.ExpectKeySHA256 = strings.ToUpper(e.keyFileSHA) // 大小写不敏感
		})
		require.NoError(t, err)
		require.Equal(t, "external_anchor", result.KeySource)
		require.Equal(t, e.keyFileSHA, result.KeyFileSHA256)

		detail := decodeDetail(t, e.operation(result.OperationID))
		require.Equal(t, "external_anchor", detail["key_source"])
		require.Equal(t, e.keyFileSHA, detail["key_file_sha256"],
			"审计只记密钥文件摘要，不记材料")
	})

	t.Run("config_without_key_path_is_refused", func(t *testing.T) {
		// 没有 `secrets.master_key_file` 时算不出 `config_without_key_path_sha256`：
		// 没有它就无法证明后续受控调整是否越界，所以先于取钥拒绝。
		//
		// 这条同时是 **K1 不可达**的实测证据：配置缺密钥路径时端口在掩码门就拒，
		// 走不到 `keySource == "backup_copy"` 的取钥分支（已进 run-state 披露）。
		e := newLegacyAdoptE2E(t, nil)
		e.setConfig([]byte(adoptConfigYAML("local_trust", "")))

		before := e.snapshot()
		_, err := e.adopt()
		requireOfflineRefusal(t, err, "master_key_path_unpersisted",
			"没有 secrets.master_key_file")
		e.requireUnchanged(before)
	})
}

// adoptInstance 在副本上跑一次 `instance adopt`（迁移准备模式的第二步）。
func (e *legacyAdoptE2E) adoptInstance(mutate ...func(*cli.InstanceAdoptRequest)) (cli.InstanceAdoptReport, error) {
	e.t.Helper()
	req := cli.InstanceAdoptRequest{Workspace: e.copyWS, ConfigPath: e.configPath}
	for _, fn := range mutate {
		fn(&req)
	}
	return instanceAdoptPort{Now: baselineClock}.Adopt(context.Background(), req)
}

// requireAdoptRefusalCode 判采纳拒绝码与文案片段。
func requireAdoptRefusalCode(t *testing.T, err error, code, want string) {
	t.Helper()
	require.Error(t, err)
	var refusal *adoptRefusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, code, refusal.Code)
	require.True(t, strings.HasPrefix(err.Error(), code+":"), "错误文案必须以稳定码开头：%s", err)
	if want != "" {
		require.Contains(t, err.Error(), want)
	}
}

// TestInstanceAdopt_LegacySchemaRequiresAdoptLegacy 判两步链的**顺序**不能反过来。
//
// 中文验收点：对还没跑受控升级的 v3～v5 副本直接 `instance adopt` → 拒绝码
// `legacy_schema_requires_adopt_legacy`，文案指向 `secrets adopt-legacy`；
// 副本一个字节没动（不迁移、不写名册、不写审计）。
// TestInstanceAdopt_LegacySchemaRequiresAdoptLegacy pins the step order.
func TestInstanceAdopt_LegacySchemaRequiresAdoptLegacy(t *testing.T) {
	e := newLegacyAdoptE2E(t, nil)
	before := e.snapshot()

	_, err := e.adoptInstance()
	requireAdoptRefusalCode(t, err, adoptCodeLegacySchema, "secrets adopt-legacy")
	e.requireUnchanged(before)

	_, rosterPresent, rosterErr := readInstanceMarkerAt(e.copyWS)
	require.NoError(t, rosterErr)
	require.False(t, rosterPresent, "拒绝路径不得写名册")
}

// TestInstanceAdopt_NoRuntimeSideEffectsOnCopy 判"第二步只做三件事"。
//
// 中文验收点：在已受控升级的副本上跑 `instance adopt`——写一份名册、写一条
// `instance.adopt` 审计行，其余表逐张零变化；配置、主密钥、用户数据根都不动；
// 记录不被改写（`WroteSecretsRecord=false`），也不新铸私密目录。
// TestInstanceAdopt_NoRuntimeSideEffectsOnCopy pins the second step's write set.
func TestInstanceAdopt_NoRuntimeSideEffectsOnCopy(t *testing.T) {
	e := newLegacyAdoptE2E(t, nil)
	_, err := e.adopt()
	require.NoError(t, err)
	record, hasRecord := e.copyRecord()
	require.True(t, hasRecord)

	copyDB := filepath.Join(e.copyWS, databaseFileName)
	beforeCounts := rawTableCounts(t, copyDB)
	configBefore, err := os.ReadFile(e.configPath)
	require.NoError(t, err)

	report, err := e.adoptInstance()
	require.NoError(t, err)
	require.False(t, report.WroteSecretsRecord, "第二步不得改写初始化记录")
	// `NewPrivateDir` 是**推导出的计划路径**（报告里给出未来目录），有没有落盘看
	// `NewPrivateDirCreated` 与磁盘本身；已有记录的副本不该铸私密目录。
	require.False(t, report.NewPrivateDirCreated, "已有记录时不得新铸私密目录")
	if report.NewPrivateDir != "" {
		require.NoDirExists(t, report.NewPrivateDir)
	}

	afterCounts := rawTableCounts(t, copyDB)
	for table, count := range beforeCounts {
		if table == "management_operations" {
			require.Equal(t, count+1, afterCounts[table], "只允许一条 instance.adopt 审计行")
			continue
		}
		require.Equal(t, count, afterCounts[table], "第二步不得改动表 %s", table)
	}
	for table := range afterCounts {
		if _, existed := beforeCounts[table]; !existed {
			require.Zero(t, afterCounts[table], "第二步不得新建带行的表 %s", table)
		}
	}

	marker, rosterPresent, err := readInstanceMarkerAt(e.copyWS)
	require.NoError(t, err)
	require.True(t, rosterPresent, "第二步的产物就是名册")
	require.Equal(t, report.InstanceID, marker.InstanceID)
	require.NotEmpty(t, report.AuditOperationID)

	afterConfig, err := os.ReadFile(e.configPath)
	require.NoError(t, err)
	require.Equal(t, hexSHA256(configBefore), hexSHA256(afterConfig), "未调整时不得改写配置")
	require.Empty(t, treeDigest(t, e.dataRoot), "不得写用户数据根")

	stillThere, stillHas := e.copyRecord()
	require.True(t, stillHas)
	require.Equal(t, record.OperationID, stillThere.OperationID, "记录必须仍指向受控升级那一步")
}

// TestInstanceAdopt_ConfigChangeRequiresRecord 判受控调整的三条出口。
//
// 中文验收点：整文件哈希与审计基线不符且没有变更引用 → `config_changed_unattested`；
// 有引用但改了密钥路径以外的字节 → `config_change_out_of_scope`；
// 只改密钥路径并带引用 → 采纳成功、`config_adjusted=true`、目标哈希进审计。
// TestInstanceAdopt_ConfigChangeRequiresRecord pins the controlled-adjustment exits.
func TestInstanceAdopt_ConfigChangeRequiresRecord(t *testing.T) {
	// drifted 造"已受控升级 + 配置被改过"的副本。
	drifted := func(t *testing.T) *legacyAdoptE2E {
		t.Helper()
		e := newLegacyAdoptE2E(t, nil)
		_, err := e.adopt()
		require.NoError(t, err)
		body := append(append([]byte{}, e.config...), []byte("# ops note\n")...)
		require.NoError(t, os.WriteFile(e.configPath, body, 0o600))
		return e
	}

	t.Run("changed_without_ref_is_refused", func(t *testing.T) {
		e := drifted(t)
		before := e.snapshot()
		_, err := e.adoptInstance()
		requireAdoptRefusalCode(t, err, adoptCodeConfigChangedUnattested, "--config-change-ref")
		e.requireUnchanged(before)
	})

	t.Run("ref_cannot_cover_out_of_scope_change", func(t *testing.T) {
		e := drifted(t)
		before := e.snapshot()
		_, err := e.adoptInstance(func(req *cli.InstanceAdoptRequest) {
			req.ConfigChangeRef = "CHG-2026-051"
		})
		requireAdoptRefusalCode(t, err, adoptCodeConfigChangeOutOfScope, "越界修改即使有变更引用也不得采纳")
		e.requireUnchanged(before)
	})

	t.Run("key_path_relocation_with_ref_is_adjusted", func(t *testing.T) {
		e := newLegacyAdoptE2E(t, nil)
		_, err := e.adopt()
		require.NoError(t, err)
		// 只改密钥路径：同一把密钥换到新位置，其余字节逐字相同。
		newPath := writeMasterKeyFile(t, filepath.Join(t.TempDir(), "private"), e.key.KeyID, e.key.Key)
		body := []byte(adoptConfigYAML("local_trust", "", adoptKeyLine(newPath)))
		require.NoError(t, os.WriteFile(e.configPath, body, 0o600))

		report, err := e.adoptInstance(func(req *cli.InstanceAdoptRequest) {
			req.ConfigChangeRef = "CHG-2026-051"
		})
		require.NoError(t, err)
		require.True(t, report.ConfigChecked)
		require.True(t, report.ConfigAdjusted)
		require.Equal(t, "CHG-2026-051", report.ConfigChangeRef)
		require.Equal(t, hexSHA256(body), report.TargetConfigSHA256)
		require.Equal(t, newPath, report.KeyFile)
	})
}
