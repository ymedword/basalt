package releasekit

// 本文件锁定部署源码与镜像锁文件的一致性：A3 要求 Dockerfile 的 ARG BASE_IMAGE、
// compose 的数据库镜像与 .env.example 的 BASALT_DB_IMAGE 都指回 release/image-lock.json，
// 改任一处即红。
// This file pins deployment sources to the image lock file: A3 requires the
// Dockerfile's ARG BASE_IMAGE, the compose database image and the .env.example
// BASALT_DB_IMAGE to point back at release/image-lock.json, and any drift fails.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	lockFixtureBase = "alpine:3.23@sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	lockFixturePG   = "pgvector/pgvector:0.8.7-pg17-bookworm@sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// repoDeployment 返回真实部署源码与锁文件的路径；本 US 的判定面对就是仓库里这四件。
// repoDeployment returns the real deployment sources and lock file; these four files
// are the judgement surface of this US.
func repoDeployment() (lock, dockerfile, compose, envExample string) {
	root := filepath.Join("..", "..")
	return filepath.Join(root, "release", "image-lock.json"),
		filepath.Join(root, "docker", "Dockerfile"),
		filepath.Join(root, "docker", "compose.yaml"),
		filepath.Join(root, "docker", ".env.example")
}

// lockFixture 是一套可用的临时部署源码；各字段单独替换用于变异测试。
// lockFixture is a usable deployment source set; individual fields are replaced to
// build the mutation cases.
type lockFixture struct {
	lock       ImageLock
	dockerfile string
	compose    string
	env        string
}

// writeLockFixture 在临时目录里造一套部署源码，默认四件自洽。
// writeLockFixture builds a deployment source set in a temp dir; the four files are
// consistent by default.
func writeLockFixture(t *testing.T) lockFixture {
	t.Helper()
	dir := t.TempDir()
	fixture := lockFixture{
		lock:       ImageLock{Schema: 1, AppBase: lockFixtureBase, Postgres: lockFixturePG},
		dockerfile: filepath.Join(dir, "Dockerfile"),
		compose:    filepath.Join(dir, "compose.yaml"),
		env:        filepath.Join(dir, ".env.example"),
	}
	fixture.write(t, lockFixtureBase, lockFixturePG, lockFixturePG)
	return fixture
}

// write 用给定内容重写三件部署源码。
func (f lockFixture) write(t *testing.T, dockerfileBase, composeImage, envImage string) {
	t.Helper()
	files := map[string]string{
		f.dockerfile: "ARG BASE_IMAGE=" + dockerfileBase + "\nFROM ${BASE_IMAGE}\n",
		f.compose:    "services:\n  postgres:\n    image: " + composeImage + "\n",
		f.env:        "BASALT_DB_IMAGE=" + envImage + "\n",
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// 中文验收点（A3）：仓库里四件源码必须与锁文件一致；任一处被改坏都必须点名失败。
// Acceptance (A3): the repository's four files must agree with the lock, and any
// drift fails with the drifted file named.
func TestUS18_Image_LockMatchesSources(t *testing.T) {
	lockPath, dockerfile, compose, envExample := repoDeployment()
	lock, err := ReadImageLock(lockPath)
	if err != nil {
		t.Fatalf("读取 release/image-lock.json：%v", err)
	}
	if err := CheckImageLock(lock, dockerfile, compose, envExample); err != nil {
		t.Fatalf("仓库部署源码与锁文件不一致：%v", err)
	}

	// 自洽样本放行，后面的每一次变红才有意义。
	fixture := writeLockFixture(t)
	if err := CheckImageLock(fixture.lock, fixture.dockerfile, fixture.compose, fixture.env); err != nil {
		t.Fatalf("自洽样本应通过：%v", err)
	}

	// 变异一：Dockerfile 底包 digest 被改。
	fixture.write(t, "alpine:3.23@sha256:"+
		"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", lockFixturePG, lockFixturePG)
	if err := CheckImageLock(fixture.lock, fixture.dockerfile, fixture.compose, fixture.env); err == nil ||
		!strings.Contains(err.Error(), "BASE_IMAGE") {
		t.Fatalf("Dockerfile 底包漂移必须点名 BASE_IMAGE，got %v", err)
	}

	// 变异二：compose 数据库镜像被换掉。
	fixture.write(t, lockFixtureBase, "postgres:17-bookworm", lockFixturePG)
	if err := CheckImageLock(fixture.lock, fixture.dockerfile, fixture.compose, fixture.env); err == nil ||
		!strings.Contains(err.Error(), "数据库镜像") {
		t.Fatalf("compose 数据库镜像漂移必须被拒，got %v", err)
	}

	// 变异三：.env.example 指到另一张镜像。
	fixture.write(t, lockFixtureBase, lockFixturePG, "pgvector/pgvector:0.8.7-pg17-bookworm")
	if err := CheckImageLock(fixture.lock, fixture.dockerfile, fixture.compose, fixture.env); err == nil ||
		!strings.Contains(err.Error(), "BASALT_DB_IMAGE") {
		t.Fatalf(".env.example 漂移必须被拒，got %v", err)
	}

	// 变异四：同一 digest 写两次——复制粘贴常把数据库镜像也塞进应用服务。
	fixture.write(t, lockFixtureBase, lockFixturePG, lockFixturePG)
	duplicated := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(duplicated, []byte(
		"services:\n  basalt:\n    image: "+lockFixturePG+
			"\n  postgres:\n    image: "+lockFixturePG+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckImageLock(fixture.lock, fixture.dockerfile, duplicated, fixture.env); err == nil ||
		!strings.Contains(err.Error(), "出现 2 次") {
		t.Fatalf("重复引用必须被拒，got %v", err)
	}

	// 变异五：锁文件 schema 不是 1。
	badSchema := fixture.lock
	badSchema.Schema = 2
	if err := CheckImageLock(badSchema, fixture.dockerfile, fixture.compose, fixture.env); err == nil {
		t.Fatal("锁文件 schema 非 1 必须被拒")
	}

	// 变异六：锁文件里的引用缺 digest。
	noDigest := filepath.Join(t.TempDir(), "image-lock.json")
	if err := os.WriteFile(noDigest,
		[]byte(`{"schema":1,"app_base":"alpine:3.23","postgres":"`+lockFixturePG+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadImageLock(noDigest); err == nil {
		t.Fatal("缺 digest 的锁文件必须被拒")
	}
}
