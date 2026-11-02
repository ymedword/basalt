// Package releasekit 实现 Basalt 的四平台发布构建、确定性打包、哈希清单、
// Ed25519 签名、SBOM、发布记录与单机升级编排。它只依赖 Go 标准库，不进入产品
// 运行时二进制。
// Package releasekit implements the release pipeline: cross-target builds,
// deterministic archives, checksums, Ed25519 signatures, SBOMs, release records
// and single-machine upgrade orchestration. It uses the standard library only.
package releasekit

import "time"

// Mode 是一次发布的模式：预演可无签名，正式必须签名且 tag 指向 HEAD。
// Mode is the release mode: rehearsal may be unsigned; final must be signed.
type Mode string

const (
	// ModeRehearsal 允许无 tag、无签名，产物只能落在 rehearsal 目录。
	ModeRehearsal Mode = "rehearsal"
	// ModeFinal 要求 tag、签名密钥与完整宿主证据。
	ModeFinal Mode = "final"
)

// Tier 是归档档位：full 含 runtime/，binary 只含程序。
// Tier selects the archive contents: full bundles runtime/, binary does not.
type Tier string

const (
	// TierFull 含运行时组件，只能在匹配宿主构建。
	TierFull Tier = "full"
	// TierBinary 只含程序，可交叉编译。
	TierBinary Tier = "binary"
)

// Target 是一个构建目标。
// Target is one build target.
type Target struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	Tier   Tier   `json:"tier"`
}

// String 返回 `goos/goarch`，用于日志与错误文案。
// String renders the target as `goos/goarch`.
func (t Target) String() string { return t.GOOS + "/" + t.GOARCH }

// Valid 报告目标是否在批准的四平台清单内。
// Valid reports whether the target belongs to the approved four-platform set.
func (t Target) Valid() bool {
	switch t.String() {
	case "windows/amd64", "linux/amd64", "linux/arm64", "darwin/arm64":
		return true
	default:
		return false
	}
}

// DefaultTargets 返回四个批准目标及其档位。
// DefaultTargets returns the four approved targets with their tiers.
func DefaultTargets() []Target {
	return []Target{
		{GOOS: "windows", GOARCH: "amd64", Tier: TierFull},
		{GOOS: "linux", GOARCH: "amd64", Tier: TierFull},
		{GOOS: "linux", GOARCH: "arm64", Tier: TierBinary},
		{GOOS: "darwin", GOARCH: "arm64", Tier: TierBinary},
	}
}

// ArchiveName 返回归档文件名。
// ArchiveName returns the release archive name.
func (t Target) ArchiveName(version string) string {
	ext := ".tar.gz"
	if t.GOOS == "windows" {
		ext = ".zip"
	}
	return "basalt-" + version + "-" + t.GOOS + "-" + t.GOARCH + ext
}

// BinaryName 返回目标平台的可执行文件名。
// BinaryName returns the target executable name.
func (t Target) BinaryName() string {
	if t.GOOS == "windows" {
		return "basalt.exe"
	}
	return "basalt"
}

// Inputs 是发布输入的冻结快照。
// Inputs is the frozen release input snapshot.
type Inputs struct {
	Version   string    `json:"version"`
	Tag       string    `json:"tag"`
	Mode      Mode      `json:"mode"`
	Commit    string    `json:"commit"`
	Repo      string    `json:"-"`
	StartedAt time.Time `json:"started_at"`
}

// BuildResult 描述一次构建。
// BuildResult describes one build.
type BuildResult struct {
	Target      Target `json:"target"`
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	GoVersion   string `json:"go_version"`
	CGOEnabled  string `json:"cgo_enabled"`
	Binary      string `json:"-"`
	BinarySHA   string `json:"binary_sha256"`
	GoModNewDep bool   `json:"go_mod_new_dependency"`
}

// PackageResult 描述一个归档。
// PackageResult describes one archive.
type PackageResult struct {
	Name         string `json:"name"`
	Archive      string `json:"-"`
	Target       Target `json:"target"`
	SHA256       string `json:"sha256"`
	BinarySHA256 string `json:"binary_sha256"`
	FileCount    int    `json:"file_count"`
}

// Evidence 是某个宿主的实际运行证据。
// Evidence records one host's actual run.
type Evidence struct {
	GOOS          string        `json:"goos"`
	GOARCH        string        `json:"goarch"`
	Tier          Tier          `json:"tier"`
	Status        string        `json:"status"`
	Checks        []string      `json:"checks"`
	Reason        string        `json:"reason,omitempty"`
	ArchiveSHA256 string        `json:"archive_sha256"`
	Reproducible  *Reproducible `json:"reproducible,omitempty"`
}

// 证据状态三档。
// The three evidence statuses.
const (
	StatusTested       = "tested"
	StatusCompiledOnly = "compiled-only"
	StatusNotTested    = "not-tested"
)

// SupportRow 是支持矩阵的一行。Kind 为空表示归档行（旧记录语义不变），
// image 表示镜像行；同平台归档与镜像按 (goos, goarch, kind) 三元组区分。
// SupportRow is one row of the support matrix. An empty Kind keeps the archive-row
// semantics of older records; Kind=image marks an image row, matched by
// (goos, goarch, kind) instead of goos/goarch alone.
type SupportRow struct {
	GOOS   string   `json:"goos"`
	GOARCH string   `json:"goarch"`
	Tier   Tier     `json:"tier"`
	Kind   string   `json:"kind,omitempty"`
	Status string   `json:"status"`
	Checks []string `json:"checks,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

// SupportMatrix 是发布目录里的支持矩阵。
// SupportMatrix is the published support matrix.
type SupportMatrix struct {
	Schema int          `json:"schema"`
	Rows   []SupportRow `json:"rows"`
}

// ArtifactRecord 是发布记录里的单个制品。
// ArtifactRecord is one artifact in the release record.
type ArtifactRecord struct {
	Name             string `json:"name"`
	GOOS             string `json:"goos"`
	GOARCH           string `json:"goarch"`
	Tier             Tier   `json:"tier"`
	SHA256           string `json:"sha256"`
	SBOMSHA256       string `json:"sbom_sha256"`
	ProvenanceSHA256 string `json:"provenance_sha256"`
}

// Toolchain 记录构建工具链。
// Toolchain records the build toolchain.
type Toolchain struct {
	GoVersion         string `json:"go_version"`
	ReleasekitVersion string `json:"releasekit_version"`
}

// SigningInfo 记录签名状态。
// SigningInfo records the signing state.
type SigningInfo struct {
	Status               string   `json:"status"`
	Algorithm            string   `json:"algorithm"`
	PublicKeyFingerprint string   `json:"public_key_fingerprint"`
	SignedFiles          []string `json:"signed_files"`
}

// Reproducible 记录双构建比较结果。
// Reproducible records the double-build comparison.
type Reproducible struct {
	Checked           bool   `json:"checked"`
	Equal             bool   `json:"equal"`
	SecondBuildSHA256 string `json:"second_build_sha256"`
}

// ReleaseRecord 是发布目录的权威记录。
// ReleaseRecord is the authoritative record of a release directory.
type ReleaseRecord struct {
	Schema              int              `json:"schema"`
	RecordID            string           `json:"record_id"`
	Version             string           `json:"version"`
	Tag                 string           `json:"tag"`
	Mode                Mode             `json:"mode"`
	Commit              string           `json:"commit"`
	StartedAt           time.Time        `json:"started_at"`
	FinishedAt          time.Time        `json:"finished_at"`
	Toolchain           Toolchain        `json:"toolchain"`
	Artifacts           []ArtifactRecord `json:"artifacts"`
	Images              []ImageRecord    `json:"images,omitempty"`
	Evidence            []SupportRow     `json:"evidence"`
	Signing             SigningInfo      `json:"signing"`
	Reproducible        Reproducible     `json:"reproducible"`
	SupportMatrixSHA256 string           `json:"support_matrix_sha256"`
	NotesSHA256         string           `json:"notes_sha256"`
	Limitations         []string         `json:"limitations"`
}

// ReleaseManifest 是发布目录的文件清单。
// ReleaseManifest is the release-directory file manifest.
type ReleaseManifest struct {
	Schema    int              `json:"schema"`
	Version   string           `json:"version"`
	Tag       string           `json:"tag"`
	Mode      Mode             `json:"mode"`
	Commit    string           `json:"commit"`
	Artifacts []ArtifactRecord `json:"artifacts"`
	Images    []ImageRecord    `json:"images,omitempty"`
}

// VerifyPolicy 控制校验严格程度。
// VerifyPolicy controls verification strictness.
type VerifyPolicy struct {
	PublicKey         string `json:"public_key,omitempty"`
	TrustFile         string `json:"trust_file,omitempty"`
	RequireSignature  bool   `json:"require_signature"`
	RequireRepoDigest bool   `json:"require_repo_digest,omitempty"`
}

// VerifyReport 是一次校验的读数。
// VerifyReport is the verification reading.
type VerifyReport struct {
	VerifiedFiles      []string `json:"verified_files"`
	VerifiedSignatures []string `json:"verified_signatures"`
	Signed             bool     `json:"signed"`
}

// ReleaseOptions 是 `release build/package` 的输入。
// ReleaseOptions carries the inputs of a single-target release run.
type ReleaseOptions struct {
	Repo        string
	Version     string
	Tag         string
	Mode        Mode
	Target      Target
	WorkDir     string
	OutDir      string
	RuntimeDir  string
	Manifest    string
	Readme      string
	NotesPath   string
	Format      string
	EvidenceDir string
	SigningKey  string
	PublicKey   string
	TrustFile   string
	// 冒烟与升级探针的宿主输入：工作目录（含 basalt.yaml）、DSN、Agent 与监听地址。
	// Host inputs for the smoke and upgrade probe: the workspace holding basalt.yaml,
	// the DSN, the agent and the listen address.
	Workspace     string
	DSN           string
	Agent         string
	Addr          string
	AdminTokenEnv string
	Upgrade       UpgradeOptions
}

// AssembleOptions 是四平台静态汇总的输入。
// AssembleOptions carries the inputs of the static assembly step.
type AssembleOptions struct {
	ArtifactDir string
	EvidenceDir string
	// Images 是 image build 产出的 image-record.json 路径列表；assemble 在写校验和
	// 之前读取、校验并复制镜像 SBOM / provenance，再把记录与矩阵镜像行一并签名。
	// Images lists the image-record.json files produced by image build; assemble
	// loads, validates and copies their SBOM / provenance before checksums are
	// written, then signs the records and matrix rows together.
	Images []string
	// ImageEvidence 是本次发布实际执行过的镜像验收检查 ID（如 stage 的检查名）。
	// 只有给出它，矩阵镜像行才能是 tested；不给出时按记录里的扫描状态写 not-tested
	// 并注明原因，**不得**把“没扫描”写成“没测试”。
	// ImageEvidence lists the image acceptance checks actually run for this release.
	// Supplying it makes the matrix image row tested; without it the row stays
	// not-tested with the record's scan reason.
	ImageEvidence []string
	OutDir        string
	NotesPath     string
	SigningKey    string
	PublicKey     string
	TrustFile     string
	Inputs        Inputs
}

// Backup 是一次升级前的三对象备份。
// Backup describes the three-object pre-upgrade backup.
type Backup struct {
	Root         string
	PostgresDump string
	Workspace    string
	Config       string
	MasterKey    string
	AdminToken   string
	DatabaseDSN  string
}

// UpgradeOptions 是单机升级/恢复的输入。
// UpgradeOptions carries the single-machine upgrade inputs.
type UpgradeOptions struct {
	OldArchive string
	NewArchive string
	// Commit 记录被升级到的新版本提交号，写进 backup.json 供恢复后对账。
	// Commit records the commit being upgraded to; it lands in backup.json.
	Commit     string
	WorkDir    string
	InstallDir string
	Workspace  string
	ConfigPath string
	MasterKey  string
	AdminToken string
	DSN        string
	// Schema 限定 pg_dump 只导出本实例对象；空值表示整库（单实例部署的常见形态）。
	// Schema restricts pg_dump to this schema; empty means the whole database.
	Schema       string
	PGDump       string
	PGRestore    string
	StopCommand  []string
	StartCommand []string
	HealthURL    string
}
