package releasekit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ymedword/basalt/internal/isolation"
)

// ImageLock 固定部署用到的两个基础镜像 digest：应用镜像的 alpine 底包与
// PostgreSQL/pgvector 镜像。Go 版本不进锁文件——go.mod 的 toolchain 与 CI 是
// 唯一事实源。
// ImageLock pins the two base-image digests of a deployment: the alpine base of the
// application image and the PostgreSQL/pgvector image. The Go version stays out of
// the lock file: go.mod's toolchain directive and CI are its single source of truth.
type ImageLock struct {
	Schema   int                     `json:"schema"`
	AppBase  string                  `json:"app_base"`
	Postgres string                  `json:"postgres"`
	Sandbox  isolation.SandboxImages `json:"sandbox"`
}

// ReadImageLock 读取并校验锁文件格式。
// ReadImageLock loads the lock file and validates its shape.
func ReadImageLock(path string) (ImageLock, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- 锁文件路径由调用方显式给出
	if err != nil {
		return ImageLock{}, fmt.Errorf("读取镜像锁文件 %s：%w", path, err)
	}
	m, err := isolation.ParseImageManifest(body, false)
	if err != nil {
		return ImageLock{}, fmt.Errorf("解析镜像锁文件 %s：%w", path, err)
	}
	return ImageLock{Schema: m.Schema, AppBase: m.AppBase, Postgres: m.Postgres, Sandbox: m.Sandbox}, nil
}

// CheckImageLock 核对锁文件与部署源码一致：AppBase 只对 Dockerfile 的
// ARG BASE_IMAGE；Postgres 对 compose 的数据库镜像与 .env.example 的
// BASALT_DB_IMAGE。比对面固定，不接受“在别处也写了同一个 digest”的替代证据。
// CheckImageLock reconciles the lock with the deployment sources: AppBase only with
// the Dockerfile's ARG BASE_IMAGE, Postgres with the compose database image and the
// BASALT_DB_IMAGE entry of .env.example. The comparison surface is fixed.
func CheckImageLock(lock ImageLock, dockerfile, compose, envExample string) error {
	if lock.Schema != 2 {
		return fmt.Errorf("镜像锁文件 schema=%d，期望 2", lock.Schema)
	}
	dockerfileBody, err := os.ReadFile(dockerfile) // #nosec G304 -- 部署源码路径由调用方显式给出
	if err != nil {
		return fmt.Errorf("读取 Dockerfile %s：%w", dockerfile, err)
	}
	baseArg, err := dockerfileBaseArg(string(dockerfileBody))
	if err != nil {
		return fmt.Errorf("%s：%w", dockerfile, err)
	}
	if baseArg != lock.AppBase {
		return fmt.Errorf("%s 的 ARG BASE_IMAGE=%q 与锁文件 app_base=%q 不一致", dockerfile, baseArg, lock.AppBase)
	}

	composeBody, err := os.ReadFile(compose) // #nosec G304 -- 部署源码路径由调用方显式给出
	if err != nil {
		return fmt.Errorf("读取 compose %s：%w", compose, err)
	}
	images := composeImages(string(composeBody))
	if len(images) == 0 {
		return fmt.Errorf("%s 里没有 image: 条目", compose)
	}
	postgresFound := false
	for _, image := range images {
		if image == lock.Postgres {
			postgresFound = true
			break
		}
	}
	if !postgresFound {
		return fmt.Errorf("%s 的数据库镜像不在 image: 条目内，锁文件 postgres=%q", compose, lock.Postgres)
	}
	if count := countValue(images, lock.Postgres); count != 1 {
		return fmt.Errorf("%s 里锁定的数据库镜像出现 %d 次，期望 1 次", compose, count)
	}

	envBody, err := os.ReadFile(envExample) // #nosec G304 -- 部署源码路径由调用方显式给出
	if err != nil {
		return fmt.Errorf("读取 %s：%w", envExample, err)
	}
	envImage, err := envValue(string(envBody), "BASALT_DB_IMAGE")
	if err != nil {
		return fmt.Errorf("%s：%w", envExample, err)
	}
	if envImage != lock.Postgres {
		return fmt.Errorf("%s 的 BASALT_DB_IMAGE=%q 与锁文件 postgres=%q 不一致", envExample, envImage, lock.Postgres)
	}
	return nil
}

// MergeSandboxImages 将镜像记录合并到副本，不回写源锁或以本地 ID 冒充发布 digest。
// MergeSandboxImages merges image records into a copy without rewriting the source or substituting local IDs for published digests.
func MergeSandboxImages(source ImageLock, images []ImageRecord, require bool) (ImageLock, error) {
	if len(images) != 1 {
		return ImageLock{}, errors.New("执行镜像清单需要唯一 linux/amd64 镜像记录")
	}
	image := images[0]
	if err := image.Validate(); err != nil {
		return ImageLock{}, err
	}
	source.Sandbox = isolation.SandboxImages{}
	if image.RepoDigest != "" {
		ref := image.Tag + "@" + image.RepoDigest
		source.Sandbox = isolation.SandboxImages{OCI: ref, GVisor: ref}
	}
	body, err := json.Marshal(source)
	if err != nil {
		return ImageLock{}, err
	}
	if _, err := isolation.ParseImageManifest(body, require); err != nil {
		return ImageLock{}, err
	}
	return source, nil
}

// dockerfileBaseArg 取 ARG BASE_IMAGE 的默认值；缺失、重复或空值都拒绝。
// dockerfileBaseArg returns the default of ARG BASE_IMAGE; missing, duplicated or
// empty values are rejected.
func dockerfileBaseArg(body string) (string, error) {
	value := ""
	found := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ARG ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "BASE_IMAGE=") {
			continue
		}
		if found {
			return "", errors.New("ARG BASE_IMAGE 重复声明")
		}
		found = true
		value = strings.TrimSpace(strings.TrimPrefix(fields[1], "BASE_IMAGE="))
	}
	if !found {
		return "", errors.New("缺少 ARG BASE_IMAGE=<冻结的镜像引用>")
	}
	if value == "" {
		return "", errors.New("ARG BASE_IMAGE 为空")
	}
	return value, nil
}

// composeImages 按行提取 image: 的值；compose 是 YAML，但 releasekit 只用标准库，
// 因此只认单行 `image: <value>` 与 `- image: <value>` 两种写法，注释行跳过。
// composeImages extracts image: values line by line. releasekit is standard-library
// only, so it recognises single-line `image: <value>` and `- image: <value>` forms
// and skips comments.
func composeImages(body string) []string {
	var images []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "- ")
		if !strings.HasPrefix(line, "image:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "image:"))
		value = strings.Trim(value, `"'`)
		if value != "" {
			images = append(images, value)
		}
	}
	return images
}

// envValue 读 .env 风格的 KEY=VALUE；键缺失或重复都拒绝。
// envValue reads KEY=VALUE from an envy file; a missing or duplicated key fails.
func envValue(body, key string) (string, error) {
	value := ""
	found := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rest, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		if found {
			return "", fmt.Errorf("%s 重复声明", key)
		}
		found = true
		value = strings.Trim(strings.TrimSpace(rest), `"'`)
	}
	if !found {
		return "", fmt.Errorf("缺少 %s", key)
	}
	return value, nil
}

func countValue(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}
