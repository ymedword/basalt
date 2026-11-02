package releasekit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const releasekitVersion = "1"

// BuildReleaseRecord 生成发布记录；不写绝对路径、凭据或私有文档内容。
// BuildReleaseRecord builds the release record without absolute paths, credentials
// or private document content.
func BuildReleaseRecord(in Inputs, pkgs []PackageResult, matrix SupportMatrix, manifest ReleaseManifest, reproducible Reproducible) (ReleaseRecord, error) {
	if len(pkgs) == 0 {
		return ReleaseRecord{}, fmt.Errorf("发布记录至少需要一个制品")
	}
	artifacts, err := artifactRecords(pkgs, filepathDirOf(pkgs[0].Archive), in)
	if err != nil {
		// 调用方在 assemble 中已写好 SBOM/provenance；直接从制品同级目录读取。
		return ReleaseRecord{}, err
	}
	finished := time.Now().UTC()
	record := ReleaseRecord{
		Schema:       1,
		RecordID:     in.Version + "-" + string(in.Mode) + "-" + shortCommit(in.Commit) + "-" + finished.Format("20060102T150405Z"),
		Version:      in.Version,
		Tag:          in.Tag,
		Mode:         in.Mode,
		Commit:       in.Commit,
		StartedAt:    in.StartedAt,
		FinishedAt:   finished,
		Toolchain:    Toolchain{GoVersion: runtime.Version(), ReleasekitVersion: releasekitVersion},
		Artifacts:    artifacts,
		Evidence:     matrix.Rows,
		Signing:      SigningInfo{Status: "not_signed", Algorithm: "none"},
		Reproducible: reproducible,
	}
	record.Limitations = recordLimitations(in.Mode, matrix.Rows)
	return record, nil
}

// recordLimitations 把未验证平台与未授权外部项写进记录，final 也不例外：
// 限制为空会被读成"全部平台都已实测"。
// recordLimitations records unverified platforms and out-of-scope external checks;
// an empty list would read as "every platform was tested".
func recordLimitations(mode Mode, rows []SupportRow) []string {
	limitations := make([]string, 0, len(rows)+2)
	if mode != ModeFinal {
		limitations = append(limitations, "rehearsal 产物不可对外发布")
	}
	for _, row := range rows {
		if row.Status == StatusTested {
			continue
		}
		platform := row.GOOS + "/" + row.GOARCH
		if row.Kind != "" {
			platform += "(" + row.Kind + ")"
		}
		limitations = append(limitations, fmt.Sprintf("%s 未实测：%s", platform, row.Reason))
	}
	limitations = append(limitations, "外部模型、真实 webhook 与通知渠道未由发布流程验证")
	sort.Strings(limitations)
	return limitations
}

// ReadReleaseRecord 读取发布目录里的记录。
// ReadReleaseRecord loads the record from a release directory.
func ReadReleaseRecord(dir string) (ReleaseRecord, error) {
	body, err := os.ReadFile(filepath.Join(dir, releaseRecordName)) // #nosec G304 -- 发布目录由调用方显式给出
	if err != nil {
		return ReleaseRecord{}, err
	}
	var record ReleaseRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return ReleaseRecord{}, fmt.Errorf("解析 %s：%w", releaseRecordName, err)
	}
	return record, nil
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func filepathDirOf(path string) string {
	index := strings.LastIndexAny(path, `/\`)
	if index < 0 {
		return "."
	}
	return path[:index]
}
