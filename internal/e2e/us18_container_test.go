//go:build e2e

package e2e

// US-18 容器验收：A4–A17 的真实镜像 + 真实 Compose 用例（A1 在 us18_image_test.go）。
// 每个用例一套独立项目 / 卷 / 宿主端口；判据只取容器行为、PostgreSQL 账本与发布
// 工具记录，不读实现内部状态。
// The US-18 container verdicts A4–A17, each on an isolated compose project. Every
// assertion reads container behaviour, the PostgreSQL ledger or release tooling
// output — never private implementation state.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// us18ComposeConfig 渲染 compose 配置，可临时从环境里删掉若干变量：A4 用变量缺失去
// 证明 fail-closed 发生在 compose 层，而不只是应用自己的校验。
// us18ComposeConfig renders the compose config with selected variables removed, so A4
// proves the fail-closed gate lives in the compose layer too.
func us18ComposeConfig(ctx context.Context, s *composeStack, drop ...string) (string, error) {
	env := make(map[string]string, len(s.env))
	for key, value := range s.env {
		env[key] = value
	}
	for _, key := range drop {
		delete(env, key)
	}
	args := []string{
		"compose", "-p", s.project,
		"-f", filepath.Join(s.dir, "compose.yaml"),
		"-f", filepath.Join(s.dir, "compose.test.yaml"),
		"config",
	}
	cmd := exec.CommandContext(ctx, "docker", args...) // 参数由夹具构造，无自由文本
	cmd.Dir = s.dir
	cmd.Env = mergeEnv(os.Environ(), env)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	return buf.String(), runErr
}

// us18StartStackTemplate 起一套只用镜像内置配置模板的栈（不接假模型）。
// us18StartStackTemplate starts a stack on the image's built-in config template.
func us18StartStackTemplate(t *testing.T, name string) *composeStack {
	t.Helper()
	s := newComposeStack(t, name)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := s.up(ctx); err != nil {
		t.Fatalf("启动栈 %s 失败: %v", name, err)
	}
	return s
}

// us18StartStackWithModel 起一套接了容器可达假模型的栈：配置里两个 Provider 都指向
// 该模型，工作区按需播种知识库手册。
// us18StartStackWithModel starts a stack wired to the container-reachable fake model,
// seeding the knowledge manual when asked.
func us18StartStackWithModel(t *testing.T, name string, model *us18Model, extra string, withManual bool) *composeStack {
	t.Helper()
	s := newComposeStack(t, name)
	us18SeedConfig(t, s, us18Config(t, model.URL(), extra))
	if withManual {
		us18SeedWorkspace(t, s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := s.up(ctx); err != nil {
		t.Fatalf("启动栈 %s 失败: %v", name, err)
	}
	return s
}

// us18ContainerID 返回唯一 basalt 容器的 ID（A7 的“重建换 ID”判据）。
// us18ContainerID returns the single basalt container ID.
func us18ContainerID(t *testing.T, s *composeStack, ctx context.Context) string {
	t.Helper()
	out, err := s.compose(ctx, "ps", "-q", "basalt")
	if err != nil {
		t.Fatalf("读取 basalt 容器 ID 失败: %v", err)
	}
	ids := us18NonEmptyLines(out)
	if len(ids) != 1 {
		t.Fatalf("期望恰好一个 basalt 容器，实际 %d 个：%v", len(ids), ids)
	}
	return ids[0]
}

// us18AsyncResult 是一次异步调用的读数。
// us18AsyncResult is one asynchronous invoke reading.
type us18AsyncResult struct {
	status int
	env    us18Envelope
	raw    []byte
	err    error
}

// us18AsyncInvoke 在后台发一次调用：A12 停机窗口与 A13 崩溃窗口都要在请求仍在途时
// 操作容器，不能用同步助手把用例卡住。
// us18AsyncInvoke issues an invoke in the background so the test can stop or kill the
// container while the request is still in flight.
func us18AsyncInvoke(t *testing.T, s *composeStack, token, name, message string) <-chan us18AsyncResult {
	t.Helper()
	body, err := json.Marshal(map[string]string{"message": message})
	if err != nil {
		t.Fatalf("编码调用请求失败: %v", err)
	}
	ch := make(chan us18AsyncResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		defer cancel()
		resp, raw, reqErr := s.request(ctx, http.MethodPost,
			"/api/v1/agents/"+name+"/invoke", body, token)
		result := us18AsyncResult{raw: raw, err: reqErr}
		if reqErr == nil {
			result.status = resp.StatusCode
			if decodeErr := json.Unmarshal(raw, &result.env); decodeErr != nil {
				result.err = decodeErr
			}
		}
		ch <- result
	}()
	return ch
}

// us18WorkspaceVolumeScript 以一次性 root 容器在 workspace 卷上跑一段脚本：A11 要把
// 卷改成不可写再改回来，宿主侧不需要 root。
// us18WorkspaceVolumeScript runs a script on the workspace volume through a one-shot
// root container, so the host needs no privileges.
func us18WorkspaceVolumeScript(t *testing.T, s *composeStack, ctx context.Context, script string) {
	t.Helper()
	volume := us18VolumeName(t, s, ctx, "workspace")
	out, err := us18DockerOutput(ctx, "run", "--rm", "--user", "0",
		"-v", volume+":/volume",
		"--entrypoint", "sh", s.env["BASALT_IMAGE"], "-c", script)
	if err != nil {
		t.Fatalf("工作区卷脚本失败: %v（%s）", err, strings.TrimSpace(out))
	}
}

// us18Knowledge 是一套已落库的知识库夹具：调用方 Key、集合、文档与版本。
// us18Knowledge is the seeded knowledge fixture: caller key, collection, document.
type us18Knowledge struct {
	key        string
	collection string
	document   string
	version    string
}

// us18SeedKnowledge 建调用方 Key → 集合 → 摄取手册。
// us18SeedKnowledge creates the caller key, collection and ingests the manual.
func us18SeedKnowledge(t *testing.T, s *composeStack) us18Knowledge {
	t.Helper()
	key := us18IssueCallerKey(t, s, "us18-caller")
	collection := us18CreateCollection(t, s, key, "us18-knowledge")
	ingested := us18Ingest(t, s, key, collection, us18ManualPath)
	return us18Knowledge{
		key:        key,
		collection: collection,
		document:   ingested.DocumentID,
		version:    ingested.VersionID,
	}
}

// verify 断言同一文档版本仍能命中（重建 / 恢复后的数据留存判据）。
// verify asserts the same document version is still the top hit.
func (k us18Knowledge) verify(t *testing.T, s *composeStack, label string) {
	t.Helper()
	hit, _ := us18SearchTop(t, s, k.key, k.collection, us18ManualQuery)
	if hit.DocumentID != k.document || hit.VersionID != k.version {
		t.Fatalf("%s：命中 = %s/%s，期望 %s/%s",
			label, hit.DocumentID, hit.VersionID, k.document, k.version)
	}
}

// TestUS18_Compose_ConfigRequiresSecrets 覆盖 A4：compose 层的变量必填、secret 缺失
// 的点名失败，以及 ingress 置零时应用自己的启动门禁。
// TestUS18_Compose_ConfigRequiresSecrets covers A4: the compose-level required
// variables, the missing-secret failure naming the file, and the application's own
// zero-ingress startup gate.
func TestUS18_Compose_ConfigRequiresSecrets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	s := newComposeStack(t, "config")

	// 第一段：缺 BASALT_IMAGE / 任一 ingress → config 非零退出并点名变量。
	// Segment 1: a missing image or ingress variable makes `compose config` fail and
	// names the variable.
	for _, key := range []string{"BASALT_IMAGE", "BASALT_INGRESS_RUN_QPS", "BASALT_INGRESS_READ_BURST"} {
		out, err := us18ComposeConfig(ctx, s, key)
		if err == nil {
			t.Fatalf("缺 %s 时 docker compose config 必须失败：%s", key, out)
		}
		if !strings.Contains(out, key) {
			t.Fatalf("缺 %s 的 config 错误必须点名变量，实际：%s", key, us18Tail(out, 2000))
		}
	}

	// 全齐：config 成功，渲染结果里没有任何 DSN 明文。口令只存在于 0400 的 secret
	// 文件里，宿主用户读不到它，因此这里断言“不含任何 DSN 形态”，而不是比对一个
	// 自己读不到的字符串。
	// All present: config succeeds and renders no DSN material. The password lives in
	// a 0400 secret the host cannot read, so the verdict forbids any DSN shape.
	out, err := us18ComposeConfig(ctx, s)
	if err != nil {
		t.Fatalf("配置齐全时 docker compose config 必须成功：%v（%s）", err, us18Tail(out, 2000))
	}
	if strings.Contains(out, "postgres://") || strings.Contains(out, "sslmode=") {
		t.Fatalf("compose config 输出不得含 DSN 明文：%s", us18Tail(out, 2000))
	}

	// 第二段：把 database_dsn 移走 → up 失败且错误 / 日志点名该 secret。
	// Segment 2: moving database_dsn away makes up fail and names the secret.
	secretPath := filepath.Join(s.secretsDir, "database_dsn")
	if err := os.Rename(secretPath, secretPath+".missing"); err != nil {
		t.Fatalf("移走 database_dsn 失败: %v", err)
	}
	upOut, upErr := s.compose(ctx, "up", "-d", "--wait", "--wait-timeout", "60")
	s.lastOutput = upOut
	if upErr == nil {
		t.Fatalf("缺 database_dsn 时 up 必须失败：%s", us18Tail(upOut, 2000))
	}
	combined := upOut
	if logs, logErr := s.logs(ctx, "basalt"); logErr == nil {
		combined += "\n" + logs
	}
	if !strings.Contains(combined, "database_dsn") {
		t.Fatalf("缺 secret 的失败必须点名 database_dsn，实际：%s", us18Tail(combined, 2000))
	}
	// Docker 可能把缺失的绑定挂载点补成目录；先清掉再放回原文件。
	if info, statErr := os.Stat(secretPath); statErr == nil && info.IsDir() {
		if removeErr := os.RemoveAll(secretPath); removeErr != nil {
			t.Fatalf("清理占位目录失败: %v", removeErr)
		}
	}
	if err := os.Rename(secretPath+".missing", secretPath); err != nil {
		t.Fatalf("放回 database_dsn 失败: %v", err)
	}
	if err := s.fixSecretOwnership(ctx, "database_dsn"); err != nil {
		t.Fatalf("恢复 secret 属主失败: %v", err)
	}
	if err := s.up(ctx); err != nil {
		t.Fatalf("secret 恢复后必须能启动: %v", err)
	}

	// 第三段：ingress 置零 → serve 拒绝启动，日志点名。
	// Segment 3: zeroed ingress makes serve refuse startup with the fixed literal.
	ingress := []string{
		"BASALT_INGRESS_RUN_QPS", "BASALT_INGRESS_RUN_BURST",
		"BASALT_INGRESS_INGEST_QPS", "BASALT_INGRESS_INGEST_BURST",
		"BASALT_INGRESS_READ_QPS", "BASALT_INGRESS_READ_BURST",
	}
	for _, key := range ingress {
		s.env[key] = "0"
	}
	if err := s.upExpectFailure(ctx); err != nil {
		t.Fatalf("ingress 置零时必须拒绝启动: %v", err)
	}
	logs, logErr := s.logs(ctx, "basalt")
	if logErr != nil {
		t.Fatalf("读取拒绝启动的日志失败: %v", logErr)
	}
	if !strings.Contains(logs, "服务启动要求下列配置项大于零") {
		t.Fatalf("零 ingress 的失败日志必须含固定文案，实际：%s", us18Tail(logs, 2000))
	}
	for _, key := range ingress {
		s.env[key] = us18IngressQPS
	}
	if err := s.up(ctx); err != nil {
		t.Fatalf("ingress 恢复后必须能启动: %v", err)
	}
}

// TestUS18_Compose_AdminLogin 覆盖 A5：管理面令牌只从实例私有卷取得；正确令牌 200，
// 匿名 / 错误令牌 401；容器非 root、根文件系统只读；启动到可登录满足 SC-001 的 60s。
// TestUS18_Compose_AdminLogin covers A5: the admin token comes from the instance
// private volume; a correct token is 200, anonymous and wrong tokens are 401, and the
// container is non-root on a read-only root filesystem.
func TestUS18_Compose_AdminLogin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	s := newComposeStack(t, "login")
	started := time.Now()
	if err := s.up(ctx); err != nil {
		t.Fatalf("启动栈失败: %v", err)
	}
	token := us18Token(t, s)
	instance, err := s.instanceID(ctx)
	if err != nil {
		t.Fatalf("读取实例标识失败: %v", err)
	}

	status, envelope, raw := us18AdminDo(t, s, http.MethodGet, "/api/v1/workspace/tree", nil, nil)
	us18RequireOK(t, status, envelope, raw, "带令牌读工作区树")
	if elapsed := time.Since(started); elapsed > 60*time.Second {
		t.Fatalf("从启动到可登录耗时 %s，超出 SC-001 的 60s", elapsed)
	}

	// 非 root + 只读根文件系统：写根目录必须失败（A5 / T022）。
	if out, err := s.exec(ctx, "basalt", "id", "-u"); err != nil || strings.TrimSpace(out) != "10001" {
		t.Fatalf("应用容器必须以 UID 10001 运行，got %q %v", out, err)
	}
	if out, err := s.exec(ctx, "basalt", "sh", "-c", "echo probe > /basalt-root-probe"); err == nil {
		t.Fatalf("根文件系统必须只读，写 / 却成功了：%s", out)
	}

	for _, probe := range []struct {
		label string
		token string
	}{
		{"匿名", ""},
		{"错误令牌", "us18-wrong-token"},
	} {
		status, _, _ := us18Do(t, s, http.MethodGet, "/api/v1/workspace/tree", nil, probe.token, nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("%s读工作区树必须 401，实际 HTTP %d", probe.label, status)
		}
	}

	// 凭据只放私有卷：工作区里不得出现令牌或主密钥文件。
	// Credentials live only in the private volume.
	out, err := s.exec(ctx, "basalt", "sh", "-c",
		`find /data -name 'admin-token' -o -name 'master.key'`)
	if err != nil {
		t.Fatalf("检查工作区凭据失败: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("工作区不得存放实例凭据，实际：%s", out)
	}
	privateOut, err := s.exec(ctx, "basalt", "cat", us18AdminTokenPath(instance))
	if err != nil {
		t.Fatalf("读取私有卷令牌失败: %v", err)
	}
	if strings.TrimSpace(privateOut) != token {
		t.Fatalf("私有卷令牌与登录所用令牌不一致")
	}
}

// TestUS18_Compose_TaskAndKnowledge 覆盖 A6：mock 模型完成一次 Run（账本终态
// completed、回复非空），上传的手册可摄取并检索命中同一版本。
// TestUS18_Compose_TaskAndKnowledge covers A6: one completed run through the mock
// model and one ingested manual that stays searchable at the same version.
func TestUS18_Compose_TaskAndKnowledge(t *testing.T) {
	model := newUS18Model(t)
	s := us18StartStackWithModel(t, "task", model, "", true)
	us18BuildAgent(t, s, "worker", "容器任务执行", nil)

	// 账本 final_text 取自该 Run 的 assistant 消息，无状态 invoke 不落消息；走会话
	// 入口才能同时验证「账本回复非空」这一 A6 判据，模型调用与终态断言不变。
	// The ledger reply is derived from the run's assistant message, which only session
	// runs persist; the model and terminal-state assertions stay unchanged.
	session := us18CreateSession(t, s, "worker")
	run := us18PostMessage(t, s, session, "你好")
	if run.FinalText != us18DefaultReply {
		t.Fatalf("回复 = %q，期望假模型的固定答案 %q", run.FinalText, us18DefaultReply)
	}
	if run.Status != "completed" {
		t.Fatalf("调用状态 = %q，期望 completed", run.Status)
	}
	if model.calls() < 1 {
		t.Fatalf("mock 模型没有收到调用")
	}
	status, finalText := us18RunDetail(t, s, run.RunID)
	if status != "completed" {
		t.Fatalf("账本终态 = %q，期望 completed", status)
	}
	if strings.TrimSpace(finalText) == "" {
		t.Fatalf("账本里的 final_text 为空")
	}

	knowledge := us18SeedKnowledge(t, s)
	knowledge.verify(t, s, "同一栈检索")
}

// TestUS18_Compose_RecreatePreservesIdentity 覆盖 A7：容器重建换 ID，实例标识、旧
// 管理令牌与知识数据全部保留；私有目录不在工作区卷里。
// TestUS18_Compose_RecreatePreservesIdentity covers A7: recreation changes the
// container ID while the instance identity, old token and knowledge stay usable.
func TestUS18_Compose_RecreatePreservesIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	model := newUS18Model(t)
	s := us18StartStackWithModel(t, "recreate", model, "", true)
	us18BuildAgent(t, s, "worker", "容器任务执行", nil)
	token := us18Token(t, s)
	instance, err := s.instanceID(ctx)
	if err != nil {
		t.Fatalf("读取实例标识失败: %v", err)
	}
	knowledge := us18SeedKnowledge(t, s)
	before := us18ContainerID(t, s, ctx)

	if err := s.up(ctx, "--force-recreate"); err != nil {
		t.Fatalf("强制重建失败: %v", err)
	}
	us18WaitHealth(t, s, 120*time.Second)

	after := us18ContainerID(t, s, ctx)
	if after == before {
		t.Fatalf("up --force-recreate 后容器 ID 未变（%s）", before)
	}
	recreatedInstance, err := s.instanceID(ctx)
	if err != nil {
		t.Fatalf("重建后读取实例标识失败: %v", err)
	}
	if recreatedInstance != instance {
		t.Fatalf("实例标识 = %q，期望 %q", recreatedInstance, instance)
	}
	// 管理面助手用的是重建前缓存的令牌——它必须仍然有效。
	status, envelope, raw := us18AdminDo(t, s, http.MethodGet, "/api/v1/workspace/tree", nil, nil)
	us18RequireOK(t, status, envelope, raw, "重建后用旧令牌读工作区树")
	if token == "" {
		t.Fatal("管理面令牌为空")
	}
	knowledge.verify(t, s, "重建后检索")

	privateOut, err := s.exec(ctx, "basalt", "sh", "-c", `find /var/lib/basalt -name master.key -print`)
	if err != nil {
		t.Fatalf("检查私有卷失败: %v", err)
	}
	if strings.TrimSpace(privateOut) == "" {
		t.Fatal("私有卷里找不到 master.key——实例身份不在 /var/lib/basalt")
	}
	workspaceOut, err := s.exec(ctx, "basalt", "sh", "-c", `find /data -name master.key -print`)
	if err != nil {
		t.Fatalf("检查工作区失败: %v", err)
	}
	if strings.TrimSpace(workspaceOut) != "" {
		t.Fatalf("工作区不得存放主密钥，实际：%s", workspaceOut)
	}
}

// TestUS18_Compose_BackupRestore 覆盖 A8：四件套备份恢复到全新项目后，旧令牌、旧
// 知识、托管模型连接与实例标识全部可用，secret 属主 / 权限符合规定。
// TestUS18_Compose_BackupRestore covers A8: the four-part backup restores into a
// brand-new project with the old token, knowledge, managed credential and instance
// identity intact, and with the secret owners preserved.
func TestUS18_Compose_BackupRestore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	model := newUS18Model(t)
	s := us18StartStackWithModel(t, "backup", model, "", true)
	us18BuildAgent(t, s, "worker", "容器任务执行", nil)
	token := us18Token(t, s)
	instance, err := s.instanceID(ctx)
	if err != nil {
		t.Fatalf("读取实例标识失败: %v", err)
	}
	knowledge := us18SeedKnowledge(t, s)
	us18CreateManagedConnection(t, s, "us18-managed", model.URL(), us18FakeKey)
	if ok, errorCode := us18TestConnection(t, s, "us18-managed"); !ok {
		t.Fatalf("托管模型连接测试失败：%s", errorCode)
	}

	backupDir := filepath.Join(t.TempDir(), "backup")
	if err := s.backup(ctx, backupDir); err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	dest, err := s.restore(ctx, backupDir)
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	us18WaitHealth(t, dest, 120*time.Second)

	destToken := us18Token(t, dest)
	if destToken != token {
		t.Fatalf("恢复后的管理令牌与备份前不一致")
	}
	destInstance, err := dest.instanceID(ctx)
	if err != nil {
		t.Fatalf("恢复后读取实例标识失败: %v", err)
	}
	if destInstance != instance {
		t.Fatalf("恢复后的实例标识 = %q，期望 %q", destInstance, instance)
	}
	status, envelope, raw := us18AdminDo(t, dest, http.MethodGet, "/api/v1/workspace/tree", nil, nil)
	us18RequireOK(t, status, envelope, raw, "恢复后用旧令牌读工作区树")
	knowledge.verify(t, dest, "恢复后检索")
	if ok, errorCode := us18TestConnection(t, dest, "us18-managed"); !ok {
		t.Fatalf("恢复后的托管模型连接测试失败：%s", errorCode)
	}

	for _, want := range []struct {
		name     string
		uid, gid int
		mode     os.FileMode
	}{
		{"postgres_password", 0, 0, 0o400},
		{"database_dsn", 10001, 10001, 0o400},
	} {
		uid, gid, mode := secretStat(t, filepath.Join(dest.secretsDir, want.name))
		if uid != want.uid || gid != want.gid || mode.Perm() != want.mode {
			t.Fatalf("恢复后 %s 属主 / 权限 = %d:%d %o，期望 %d:%d %o",
				want.name, uid, gid, mode.Perm(), want.uid, want.gid, want.mode)
		}
	}
}

// TestUS18_Compose_UpgradeRollback 覆盖 A16 的操作链：镜像 A 写数据 → 备份 → 换镜像
// B 升级 → 数据 / 身份 / 旧令牌仍可用 → 用备份 + 镜像 A 恢复到新项目。
// TestUS18_Compose_UpgradeRollback covers A16: image A writes data, the stack is
// backed up, image B upgrades it, and the rollback chain restores the backup onto
// image A in a brand-new project.
func TestUS18_Compose_UpgradeRollback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	model := newUS18Model(t)
	s := us18StartStackWithModel(t, "upgrade", model, "", true)
	us18BuildAgent(t, s, "worker", "容器任务执行", nil)
	token := us18Token(t, s)
	instance, err := s.instanceID(ctx)
	if err != nil {
		t.Fatalf("读取实例标识失败: %v", err)
	}
	knowledge := us18SeedKnowledge(t, s)
	us18CreateManagedConnection(t, s, "us18-managed", model.URL(), us18FakeKey)

	versionA := us18InfoVersion(t, s)
	if versionA != us18DefaultVersion {
		t.Fatalf("升级前版本 = %q，期望 %q", versionA, us18DefaultVersion)
	}

	backupDir := filepath.Join(t.TempDir(), "upgrade-backup")
	if err := s.backup(ctx, backupDir); err != nil {
		t.Fatalf("升级前备份失败: %v", err)
	}

	imageA := s.env["BASALT_IMAGE"]
	imageB := us18ImageFor(t, us18UpgradeVersion)
	s.env["BASALT_IMAGE"] = imageB
	if err := s.up(ctx, "--force-recreate"); err != nil {
		t.Fatalf("升级到镜像 B 失败: %v", err)
	}
	us18WaitHealth(t, s, 120*time.Second)
	if got := us18InfoVersion(t, s); got != us18UpgradeVersion {
		t.Fatalf("升级后版本 = %q，期望 %q", got, us18UpgradeVersion)
	}
	upgradedInstance, err := s.instanceID(ctx)
	if err != nil {
		t.Fatalf("升级后读取实例标识失败: %v", err)
	}
	if upgradedInstance != instance {
		t.Fatalf("升级后实例标识 = %q，期望 %q", upgradedInstance, instance)
	}
	status, envelope, raw := us18AdminDo(t, s, http.MethodGet, "/api/v1/workspace/tree", nil, nil)
	us18RequireOK(t, status, envelope, raw, "升级后用旧令牌读工作区树")
	knowledge.verify(t, s, "升级后检索")

	// 回退：只支持“恢复升级前备份 + 旧镜像”，恢复到全新项目。
	// Rollback is restore-backup-plus-old-image into a fresh project, nothing else.
	s.env["BASALT_IMAGE"] = imageA
	dest, err := s.restore(ctx, backupDir)
	if err != nil {
		t.Fatalf("回退恢复失败: %v", err)
	}
	us18WaitHealth(t, dest, 120*time.Second)
	if got := us18InfoVersion(t, dest); got != versionA {
		t.Fatalf("回退后版本 = %q，期望 %q", got, versionA)
	}
	destToken := us18Token(t, dest)
	if destToken != token {
		t.Fatalf("回退后管理令牌与升级前不一致")
	}
	destInstance, err := dest.instanceID(ctx)
	if err != nil {
		t.Fatalf("回退后读取实例标识失败: %v", err)
	}
	if destInstance != instance {
		t.Fatalf("回退后实例标识 = %q，期望 %q", destInstance, instance)
	}
	knowledge.verify(t, dest, "回退后检索")
	for _, want := range []struct {
		name     string
		uid, gid int
		mode     os.FileMode
	}{
		{"postgres_password", 0, 0, 0o400},
		{"database_dsn", 10001, 10001, 0o400},
	} {
		uid, gid, mode := secretStat(t, filepath.Join(dest.secretsDir, want.name))
		if uid != want.uid || gid != want.gid || mode.Perm() != want.mode {
			t.Fatalf("回退后 %s 属主 / 权限 = %d:%d %o，期望 %d:%d %o",
				want.name, uid, gid, mode.Perm(), want.uid, want.gid, want.mode)
		}
	}
}

// TestUS18_Compose_DatabaseUnavailable 覆盖 A9：停库后 health 仍 200、业务面 5xx、
// 无本地数据库回退；起库后同一请求无需重启即恢复 200。
// TestUS18_Compose_DatabaseUnavailable covers A9: with PostgreSQL stopped the health
// probe stays 200, business calls turn 5xx, no local database file appears, and the
// same call recovers once PostgreSQL is back.
func TestUS18_Compose_DatabaseUnavailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	s := newComposeStack(t, "faults")
	if err := s.up(ctx); err != nil {
		t.Fatalf("启动栈失败: %v", err)
	}
	token, err := s.adminToken(ctx)
	if err != nil {
		t.Fatalf("读取管理面令牌失败: %v", err)
	}

	if err := s.stopService(ctx, "postgres"); err != nil {
		t.Fatalf("停止 PostgreSQL 失败: %v", err)
	}
	// 健康检查只代表进程存活：必须仍是 200。
	resp, body, err := s.request(ctx, http.MethodGet, "/api/v1/health", nil, "")
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"status":"ok"`)) {
		t.Fatalf("停库后 health 必须是 200 且 ok，got %v %s", err, body)
	}
	// 业务必须失败（5xx），且不得偷偷落到本地文件。
	// /api/v1/runs 走 ListRuns，存储错误返回 500；/api/v1/workspace/tree 是纯文件
	// 系统读，停库后仍 200，不能当数据库探针。
	resp, body, err = s.request(ctx, http.MethodGet, "/api/v1/runs", nil, token)
	if err != nil || resp.StatusCode < 500 {
		t.Fatalf("停库后业务必须 5xx，got %v %d %s", err, resp.StatusCode, body)
	}
	out, err := s.exec(ctx, "basalt", "sh", "-c",
		`find /data -maxdepth 3 \( -name '*.db' -o -name '*.sqlite' \) -print`)
	if err != nil {
		t.Fatalf("检查本地数据库文件失败: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("运行期不得出现本地数据库文件，got %q", out)
	}
	if err := s.startService(ctx, "postgres"); err != nil {
		t.Fatalf("启动 PostgreSQL 失败: %v", err)
	}
	waitFor(t, 60*time.Second, func() bool {
		resp, _, err := s.request(ctx, http.MethodGet, "/api/v1/runs", nil, token)
		return err == nil && resp.StatusCode == http.StatusOK
	})
}

// TestUS18_Compose_WrongCredentials 覆盖 A10：错误口令让应用退出非零，日志点名
// PostgreSQL 错误但不回显 DSN 口令。
// TestUS18_Compose_WrongCredentials covers A10: a wrong password makes the app exit
// non-zero with the PostgreSQL failure named and the password never echoed.
func TestUS18_Compose_WrongCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	s := newComposeStack(t, "badcred")
	// 容器只认 secret 文件：entrypoint 从 BASALT_DATABASE_DSN_FILE 读入并导出。
	// 写宿主环境变量不会进入容器，测试会假绿。
	dsn := "postgres://basalt:wrong-password-canary@postgres:5432/basalt?sslmode=disable"
	s.writeSecret(t, "database_dsn", dsn)
	// 属主必须按准备步骤修好，否则失败发生在“读不到 secret”，测不到认证失败。
	if err := s.fixSecretOwnership(ctx, "database_dsn"); err != nil {
		t.Fatalf("设置 secret 属主失败: %v", err)
	}
	if err := s.upExpectFailure(ctx); err != nil {
		t.Fatalf("错误口令必须让应用退出非零: %v", err)
	}
	logs, err := s.logs(ctx, "basalt")
	if err != nil {
		t.Fatalf("读取失败日志失败: %v", err)
	}
	if !strings.Contains(logs, "连接 PostgreSQL 失败") && !strings.Contains(logs, "打开 PostgreSQL") {
		t.Fatalf("失败日志必须含实际错误字面量，got %s", us18Tail(logs, 2000))
	}
	if strings.Contains(logs, "wrong-password-canary") {
		t.Fatal("日志不得回显 DSN 口令")
	}
}

// TestUS18_Compose_VolumePermissions 覆盖 A11：工作区不可写时应用退出非零并点名
// 路径；属主改回 10001 后可启动。
// TestUS18_Compose_VolumePermissions covers A11: an unwritable workspace makes the
// app exit non-zero naming the path, and restoring ownership starts it again.
func TestUS18_Compose_VolumePermissions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	s := newComposeStack(t, "volperm")
	us18SeedConfig(t, s, us18TemplateConfig(t))

	// 先正常起一次，让工作区骨架存在：骨架缺失时 `workspace.Init` 自己就会炸在
	// mkdir 上，测不到启动前的可写探针（A11 的判别点）。
	// Start once so the skeleton exists: with it missing, Init itself fails before the
	// writability probe, which is exactly what A11 must exercise.
	if err := s.up(ctx); err != nil {
		t.Fatalf("首次启动失败: %v", err)
	}
	if err := s.stopService(ctx, "basalt"); err != nil {
		t.Fatalf("停止应用失败: %v", err)
	}
	us18WorkspaceVolumeScript(t, s, ctx, "chown 0:0 /volume/.basalt && chmod 0555 /volume/.basalt")
	if err := s.upExpectFailure(ctx); err != nil {
		t.Fatalf("工作区不可写时必须拒绝启动: %v", err)
	}
	logs, err := s.logs(ctx, "basalt")
	if err != nil {
		t.Fatalf("读取失败日志失败: %v", err)
	}
	if !strings.Contains(logs, "工作区不可写") || !strings.Contains(logs, "/data") {
		t.Fatalf("卷权限失败必须点名路径，got %s", us18Tail(logs, 2000))
	}

	us18WorkspaceVolumeScript(t, s, ctx, "chown -R 10001:10001 /volume && chmod 0750 /volume/.basalt")
	if err := s.up(ctx); err != nil {
		t.Fatalf("属主恢复后必须能启动: %v", err)
	}
}

// TestUS18_Compose_SecretPermissions 覆盖 A17：不可读的 database_dsn 让应用退出
// 非零且日志不打印内容；按准备步骤设属主后可启动，两个 secret 的属主 / 权限与
// 非世界可读一并断言。
// TestUS18_Compose_SecretPermissions covers A17: an unreadable database_dsn fails the
// app with Permission denied and no secret content, and the prepared owners start it.
func TestUS18_Compose_SecretPermissions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	s := newComposeStack(t, "secrets")
	password := "pass-canary" // 两个 secret 必须同源；第二层启动要真的连上库
	dsn := "postgres://basalt:" + password + "@postgres:5432/basalt?sslmode=disable"
	// 两个文件先按“部署用户所有、0600”写入；只修 postgres_password 的属主，
	// 让 postgres 能启动而 database_dsn 仍保持宿主用户所有——失败必须限定在应用侧。
	s.writeSecret(t, "database_dsn", dsn)
	s.writeSecret(t, "postgres_password", password)
	if err := s.fixSecretOwnership(ctx, "postgres_password"); err != nil {
		t.Fatalf("设置 postgres_password 属主失败: %v", err)
	}
	if err := s.upExpectFailure(ctx); err != nil {
		t.Fatalf("secret 属主 / 权限不符时应用必须退出非零: %v", err)
	}
	logs, err := s.logs(ctx, "basalt")
	if err != nil {
		t.Fatalf("读取失败日志失败: %v", err)
	}
	if !strings.Contains(strings.ToLower(logs), "permission denied") {
		t.Fatalf("日志必须留下 Permission denied，got %s", us18Tail(logs, 2000))
	}
	if strings.Contains(logs, password) {
		t.Fatal("日志不得回显 secret 内容")
	}

	// 第二层：按文档准备步骤设属主后必须能启动；权限断言同时排除世界可读。
	if err := s.fixSecretOwnership(ctx); err != nil {
		t.Fatalf("设置 secret 属主失败: %v", err)
	}
	for _, want := range []struct {
		name     string
		uid, gid int
		mode     os.FileMode
	}{
		{"postgres_password", 0, 0, 0o400},
		{"database_dsn", 10001, 10001, 0o400},
	} {
		uid, gid, mode := secretStat(t, filepath.Join(s.secretsDir, want.name))
		if uid != want.uid || gid != want.gid || mode.Perm() != want.mode {
			t.Fatalf("%s 属主 / 权限 = %d:%d %o，期望 %d:%d %o",
				want.name, uid, gid, mode.Perm(), want.uid, want.gid, want.mode)
		}
		if mode.Perm()&0o004 != 0 {
			t.Fatalf("%s 不得世界可读（mode %o）", want.name, mode.Perm())
		}
	}
	if err := s.up(ctx); err != nil {
		t.Fatalf("secret 修好后必须能启动: %v", err)
	}
	if out, err := s.exec(ctx, "basalt", "id", "-u"); err != nil || strings.TrimSpace(out) != "10001" {
		t.Fatalf("应用容器必须以 UID 10001 运行，got %q %v", out, err)
	}
}

// TestUS18_Compose_GracefulShutdown 覆盖 A12：空闲停机与在途会话运行停机都在宽限内
// 以退出码 0 收尾，Run 落终态，重启后锁无残留。
// TestUS18_Compose_GracefulShutdown covers A12: idle and in-flight session shutdowns
// both exit 0 within the grace window, the run reaches a terminal state, and a
// restart works with no lock residue.
func TestUS18_Compose_GracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	model := newUS18Model(t, us18ModelStep{
		Delay:    15 * time.Second,
		Response: textResponse(us18DefaultReply),
	})
	s := us18StartStackWithModel(t, "shutdown", model, "", false)
	us18BuildAgent(t, s, "worker", "容器任务执行", nil)

	// IDLE：没有在途请求，SIGTERM 后立即收尾。
	started := time.Now()
	if err := s.stopService(ctx, "basalt"); err != nil {
		t.Fatalf("空闲停机失败: %v", err)
	}
	elapsed := time.Since(started)
	if elapsed > 90*time.Second {
		t.Fatalf("空闲停机耗时 %s，超出 90s 宽限", elapsed)
	}
	if code, err := s.serviceExitCode(ctx, "basalt"); err != nil || code != 0 {
		t.Fatalf("空闲停机退出码 = %d（%v），期望 0", code, err)
	}
	if err := s.startService(ctx, "basalt"); err != nil {
		t.Fatalf("空闲停机后重启失败: %v", err)
	}
	us18WaitHealth(t, s, 90*time.Second)

	// 会话运行：模型延迟 15s，SIGTERM 必须在 70s 宽限内等它收尾。
	// Session run: the model holds the request for 15s; shutdown must wait within the
	// 70s window and exit 0.
	pending := us18AsyncInvoke(t, s, us18Token(t, s), "worker", "停机前的在途调用")
	waitFor(t, 60*time.Second, func() bool {
		return us18PSQL(t, s, "SELECT count(*) FROM runs WHERE status = 'running'") != "0"
	})
	waitFor(t, 60*time.Second, func() bool { return model.calls() >= 1 })

	started = time.Now()
	if err := s.stopService(ctx, "basalt"); err != nil {
		t.Fatalf("在途停机失败: %v", err)
	}
	elapsed = time.Since(started)
	if elapsed > 90*time.Second {
		t.Fatalf("在途停机耗时 %s，超出 90s 宽限", elapsed)
	}
	if code, err := s.serviceExitCode(ctx, "basalt"); err != nil || code != 0 {
		t.Fatalf("在途停机退出码 = %d（%v），期望 0", code, err)
	}

	res := <-pending
	if res.err != nil {
		t.Fatalf("优雅停机期间的调用必须收到完整响应: %v", res.err)
	}
	if res.status != http.StatusOK || res.env.Code != "OK" {
		t.Fatalf("优雅停机期间的调用响应 = HTTP %d code=%q（%s）",
			res.status, res.env.Code, us18Tail(string(res.raw), 600))
	}
	var reading us18InvokeReading
	if err := json.Unmarshal(res.env.Data, &reading); err != nil {
		t.Fatalf("解析停机期间的调用响应失败: %v", err)
	}
	if reading.Status != "completed" {
		t.Fatalf("停机期间 Run 终态 = %q，期望 completed", reading.Status)
	}
	if rowStatus, _ := us18RunRow(t, s, reading.RunID); rowStatus != "completed" {
		t.Fatalf("账本终态 = %q，期望 completed", rowStatus)
	}

	// 重启正常、锁无残留。
	if err := s.startService(ctx, "basalt"); err != nil {
		t.Fatalf("停机后重启失败（实例锁可能有残留）: %v", err)
	}
	us18WaitHealth(t, s, 90*time.Second)
}

// TestUS18_Compose_RestartDoesNotRerun 覆盖 A13：SIGKILL 后在途 Run 被恢复扫描收口
// 为 failed、半截工具组为 unknown，mock 没有新增调用，也不出现第二次外部动作。
// TestUS18_Compose_RestartDoesNotRerun covers A13: after SIGKILL the recovery scan
// closes the run as failed and the tool call as unknown, with no new model call and
// no second external action.
func TestUS18_Compose_RestartDoesNotRerun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	// 挂起点必须是一个真的会阻塞的工具调用：容器基础层是 Alpine，`sleep` 等 applet 全是
	// busybox 的软链，执行侧按真实路径命中「交互式 shell」集合直接拒绝（runner.go 的
	// realpath 判定），Run 会立刻走完而不是停在 running。改用 FIFO：write_file 的
	// open(O_WRONLY) 在没有读者时阻塞，不依赖任何可执行件，也不触碰 shell 白名单。
	// The blocking point must be a genuinely blocking tool call: Alpine applets are
	// busybox symlinks, so the runner's realpath check refuses `sleep` as an
	// interactive shell. A FIFO write blocks on open with no reader and needs no
	// executable at all.
	toolCall := mustJSON(map[string]any{
		"path":    "block.pipe",
		"content": "崩溃前的外部动作",
	})
	model := newUS18Model(t,
		us18ModelStep{Response: toolCallResponse("call-block", "write_file", toolCall)},
	)
	s := us18StartStackWithModel(t, "crash", model, "", false)
	us18BuildAgent(t, s, "worker", "崩溃恢复任务", []string{"write_file"})
	us18WorkspaceVolumeScript(t, s, ctx,
		"set -eu\n"+
			"mkfifo /volume/.basalt/agents/worker/block.pipe\n"+
			"chown 10001:10001 /volume/.basalt/agents/worker/block.pipe\n")

	token := us18Token(t, s)
	pending := us18AsyncInvoke(t, s, token, "worker", "崩溃前的外部动作")
	runID := us18WaitDB(t, s,
		"SELECT run_id FROM runs WHERE status = 'running' ORDER BY started_at DESC LIMIT 1",
		func(out string) bool { return out != "" },
		90*time.Second)
	us18WaitDB(t, s,
		"SELECT string_agg(status, ',' ORDER BY id) FROM tool_invocations WHERE run_id = '"+runID+"'",
		func(out string) bool { return strings.Contains(out, "pending") },
		90*time.Second)
	if model.calls() < 1 {
		t.Fatalf("SIGKILL 前 mock 模型应已收到 1 次调用，实际 %d", model.calls())
	}

	// kill 是 SIGKILL：没有优雅收尾、没有恢复扫描的机会，正是崩溃现场。
	if out, err := s.compose(ctx, "kill", "basalt"); err != nil {
		t.Fatalf("kill basalt 失败: %v（%s）", err, us18Tail(out, 1000))
	}

	// 崩溃后的调用必然失败或无终态；它不是判据（判据是账本与 mock 调用数）。
	res := <-pending
	if res.err == nil && res.env.Code == "OK" {
		var reading us18InvokeReading
		if err := json.Unmarshal(res.env.Data, &reading); err == nil && reading.Status == "completed" {
			t.Fatalf("SIGKILL 后的调用不得以 completed 结束")
		}
	}

	if err := s.up(ctx); err != nil {
		t.Fatalf("崩溃后重启失败: %v", err)
	}
	us18WaitDB(t, s, "SELECT status FROM runs WHERE run_id = '"+runID+"'",
		func(out string) bool { return out == "failed" }, 120*time.Second)
	if statuses := us18ToolStatuses(t, s, runID); statuses != "unknown" {
		t.Fatalf("半截工具组状态 = %q，期望 unknown", statuses)
	}
	if count := us18PSQL(t, s,
		"SELECT count(*) FROM tool_invocations WHERE run_id = '"+runID+"'"); count != "1" {
		t.Fatalf("工具调用行数 = %s，期望 1（不得重跑）", count)
	}
	if calls := model.calls(); calls != 1 {
		t.Fatalf("恢复扫描后 mock 调用数 = %d，期望 1（不得重跑）", calls)
	}
}

// TestUS18_Compose_SecondInstanceRejected 覆盖 A14：共享同一工作区卷的第二个应用
// 容器必须因实例锁退出非零，日志点名锁。
// TestUS18_Compose_SecondInstanceRejected covers A14: a second app container sharing
// the workspace volume exits non-zero and names the instance lock.
func TestUS18_Compose_SecondInstanceRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	s := us18StartStackTemplate(t, "second")

	name := s.project + "-second"
	t.Cleanup(func() {
		// 排障现场与 compose 栈同一开关：KEEP=1 时保留第二实例容器供人工检视。
		// Same switch as the stack fixture: KEEP=1 preserves the second container.
		if os.Getenv(us18KeepEnv) == "1" {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if _, err := us18DockerOutput(cleanupCtx, "rm", "-f", name); err != nil {
			t.Logf("清理第二实例容器失败（可能从未创建）：%v", err)
		}
	})
	if out, err := s.compose(ctx, "run", "-d", "--no-deps", "--name", name, "basalt"); err != nil {
		t.Fatalf("启动第二实例失败: %v（%s）", err, us18Tail(out, 2000))
	}

	var status string
	waitFor(t, 90*time.Second, func() bool {
		out, err := us18DockerOutput(ctx, "inspect", "--format", "{{.State.Status}}", name)
		if err != nil {
			return false
		}
		status = strings.TrimSpace(out)
		return status == "exited"
	})
	codeOut, err := us18DockerOutput(ctx, "inspect", "--format", "{{.State.ExitCode}}", name)
	if err != nil {
		t.Fatalf("读取第二实例退出码失败: %v", err)
	}
	if code := strings.TrimSpace(codeOut); code == "0" {
		t.Fatalf("第二实例必须退出非零，实际 %s", code)
	}
	// 日志走 us18DockerLogs：容器的 slog 输出在 stderr 上，只读 stdout 会恒为空串
	// （A14 实测根因）。短重试窗口另行容忍日志驱动落后于 State 翻 exited 的写入延迟。
	// Read through us18DockerLogs: the container's slog goes to stderr, so a stdout-only
	// read is always empty (A14's actual root cause). The short retry window separately
	// tolerates the log driver lagging the exited state.
	logDeadline := time.Now().Add(30 * time.Second)
	var logs string
	for {
		if out, logErr := us18DockerLogs(ctx, name); logErr == nil {
			logs = out
			if strings.Contains(logs, "工作区实例锁已被占用") {
				return
			}
		}
		if time.Now().After(logDeadline) {
			t.Fatalf("第二实例日志必须点名实例锁（退出码 %s），got %s",
				strings.TrimSpace(codeOut), us18Tail(logs, 2000))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestUS18_Compose_SecretPermissions 已在上方（A17）；本文件其余用例的编排顺序
// 与模块卡 §3.1 的测试函数清单一致。

// TestUS18_Cleanup_NoResidue 覆盖 A15：项目名前缀固定且随机，down -v 之后容器 /
// 卷 / 网络 / 镜像均无残留。
// TestUS18_Cleanup_NoResidue covers A15: the project name is prefixed and random,
// and down -v leaves no container, volume, network or image behind.
func TestUS18_Cleanup_NoResidue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	prefix := os.Getenv(us18ProjectPrefixEnv)
	if prefix == "" {
		prefix = us18DefaultPrefix
	}
	s := us18StartStackTemplate(t, "cleanup")
	if !strings.HasPrefix(s.project, prefix+"-") {
		t.Fatalf("项目名 %q 必须以下列前缀开头：%s-", s.project, prefix)
	}
	if len(s.project) <= len(prefix)+2 {
		t.Fatalf("项目名 %q 缺少随机后缀", s.project)
	}
	// 卷名由 compose 加项目前缀：清理按项目名前缀扫，不会碰到用户实例。
	for _, logical := range []string{"workspace", "private", "database-data"} {
		if name := us18VolumeName(t, s, ctx, logical); !strings.HasPrefix(name, s.project+"_") {
			t.Fatalf("卷 %q 的实际名 %q 必须以项目名 %q 开头", logical, name, s.project)
		}
	}

	if err := s.down(ctx, true); err != nil {
		t.Fatalf("down -v 失败: %v", err)
	}
	if residue := cleanupResidue(s.project); residue != 0 {
		t.Fatalf("down -v 后项目 %s 仍有 %d 项残留", s.project, residue)
	}
}
