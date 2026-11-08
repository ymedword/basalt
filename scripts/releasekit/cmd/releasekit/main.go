// Command releasekit 是 Basalt 发布工具的命令行入口。
// Command releasekit is the command-line entry point of the release tool.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ymedword/basalt/scripts/releasekit"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printUsage(stdout)
		return 0
	}
	command, rest := args[0], args[1:]
	var err error
	switch command {
	case "preflight":
		err = runPreflight(ctx, rest, stdout)
	case "build":
		err = runBuild(ctx, rest, stdout)
	case "package":
		err = runPackage(ctx, rest, stdout)
	case "release":
		err = runRelease(ctx, rest, stdout)
	case "verify-package":
		err = runVerifyPackage(rest, stdout)
	case "record":
		err = runRecord(ctx, rest, stdout)
	case "assemble":
		err = runAssemble(ctx, rest, stdout)
	case "image":
		err = runImage(ctx, rest, stdout)
	case "verify":
		err = runVerify(ctx, rest, stdout)
	case "repro":
		err = runRepro(ctx, rest, stdout)
	case "smoke":
		err = runSmoke(ctx, rest, stdout)
	case "upgrade":
		err = runUpgrade(ctx, rest, stdout, stderr)
	case "sbom":
		err = runSBOM(ctx, rest, stdout)
	case "provenance":
		err = runProvenance(ctx, rest, stdout)
	case "sign":
		err = runSign(rest, stdout)
	default:
		fmt.Fprintf(stderr, "releasekit: 未知子命令 %q\n\n", command)
		printUsage(stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "releasekit:", err)
		return 1
	}
	return 0
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, `releasekit - Basalt 发布工具

用法:
  releasekit preflight --repo . --version X.Y.Z --tag vX.Y.Z --mode rehearsal|final [--notes <md>] [--out <dir>]
  releasekit build --repo . --version X.Y.Z --tag vX.Y.Z --mode final --target linux/amd64 --tier full --out dist/build
  releasekit package --repo . --binary dist/build/basalt --version X.Y.Z --target linux/amd64 --tier full [--format tar.gz] --runtime release/runtime --out dist/artifacts
  releasekit release --repo . --version X.Y.Z --target linux/amd64 --tier full --runtime release/runtime [--workspace <dir> --dsn <dsn> --old <归档> --config <path>] --evidence-dir dist/evidence --out dist/artifacts
  releasekit verify-package --archive dist/artifacts/basalt-X.Y.Z-linux-amd64.tar.gz --version X.Y.Z [--target linux/amd64 --tier full]
  releasekit record --artifact-dir dist/artifacts --evidence-dir dist/evidence --version X.Y.Z --tag vX.Y.Z --mode rehearsal --out dist/record
  releasekit assemble --artifact-dir dist/artifacts --evidence-dir dist/evidence --version X.Y.Z --tag vX.Y.Z --mode final --out dist/release/X.Y.Z --notes release/notes/X.Y.Z.md --signing-key <key> --public-key <pub> [--image-record <image-record.json>]...
  releasekit image build --repo . --version X.Y.Z --binary <path> --base-ref <name@sha256:…> [--dockerfile docker/Dockerfile --entrypoint docker/entrypoint.sh --config docker/basalt.yaml --out dist/images --record dist/images/image-record.json]
  releasekit image verify --record dist/images/image-record.json [--require-local-image]
  releasekit verify --release-dir dist/release/X.Y.Z --public-key <pub> --require-signature [--require-image-digest]
  releasekit repro --release-dir dist/release/X.Y.Z --public-key <pub>
  releasekit smoke --archive <archive> --version X.Y.Z --target goos/goarch [--tier full] [--workspace <dir> --dsn <dsn> --agent <name> --addr <host:port> --admin-token-env <VAR>] [--evidence-dir <dir>]
  releasekit upgrade --old <old-archive> --new <new-archive> --dsn <dsn> [--schema <name>] --workspace <dir> --config <path>
      （受限入口：未完成真实宿主升级／失败恢复闭环验证；生产升级见 docs/Operations.md §5.1）
  releasekit sbom --repo . --version X.Y.Z --artifact-dir <dir> --out <dir>
  releasekit provenance --version X.Y.Z --artifact-dir <dir> --out <dir>
  releasekit sign --key <path> --dir <dir> --name <file>`)
}

func parseTarget(value, tier string) (releasekit.Target, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return releasekit.Target{}, fmt.Errorf("target %q 必须是 goos/goarch", value)
	}
	target := releasekit.Target{GOOS: parts[0], GOARCH: parts[1], Tier: releasekit.Tier(tier)}
	if target.Tier != releasekit.TierFull && target.Tier != releasekit.TierBinary {
		return releasekit.Target{}, fmt.Errorf("tier %q 必须是 full 或 binary", tier)
	}
	if !target.Valid() {
		return releasekit.Target{}, fmt.Errorf("target %s 不在四平台清单内", target.String())
	}
	return target, nil
}

func runPreflight(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("preflight", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repo := flags.String("repo", ".", "仓库根")
	version := flags.String("version", "", "版本 X.Y.Z")
	tag := flags.String("tag", "", "tag vX.Y.Z")
	mode := flags.String("mode", "rehearsal", "rehearsal / final")
	signingKey := flags.String("signing-key", "", "Ed25519 私钥")
	publicKey := flags.String("public-key", "", "Ed25519 公钥")
	notes := flags.String("notes", "", "发布说明（可选，给出即校验模板）")
	outDir := flags.String("out", "", "输出目录（可选，给出即检查可创建）")
	if err := flags.Parse(args); err != nil {
		return err
	}
	in, err := releasekit.LoadInputs(*repo, *version, *tag, releasekit.Mode(*mode))
	if err != nil {
		return err
	}
	if err := releasekit.Preflight(ctx, in, releasekit.ReleaseOptions{
		SigningKey: *signingKey, PublicKey: *publicKey,
		NotesPath: *notes, OutDir: *outDir,
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "version=%s tag=%s mode=%s commit=%s\n", in.Version, in.Tag, in.Mode, in.Commit)
	return nil
}

func runBuild(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repo := flags.String("repo", ".", "仓库根")
	version := flags.String("version", "", "版本 X.Y.Z")
	tag := flags.String("tag", "", "tag vX.Y.Z")
	mode := flags.String("mode", "rehearsal", "rehearsal / final")
	targetValue := flags.String("target", "", "goos/goarch")
	tier := flags.String("tier", "binary", "full / binary")
	outDir := flags.String("out", "dist/build", "构建输出目录")
	if err := flags.Parse(args); err != nil {
		return err
	}
	target, err := parseTarget(*targetValue, *tier)
	if err != nil {
		return err
	}
	in, err := releasekit.LoadInputs(*repo, *version, *tag, releasekit.Mode(*mode))
	if err != nil {
		return err
	}
	build, err := releasekit.BuildTarget(ctx, in, target, *outDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "binary=%s sha256=%s go_version=%s cgo_enabled=%s\n",
		build.Binary, build.BinarySHA, build.GoVersion, build.CGOEnabled)
	return nil
}

func runPackage(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("package", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repo := flags.String("repo", ".", "仓库根")
	binary := flags.String("binary", "", "已构建二进制")
	version := flags.String("version", "", "版本 X.Y.Z")
	targetValue := flags.String("target", "", "goos/goarch")
	tier := flags.String("tier", "binary", "full / binary")
	runtimeDir := flags.String("runtime", "", "runtime 目录（full 必填）")
	format := flags.String("format", "", "归档格式：windows 必须 zip，其余 tar.gz（缺省按目标平台）")
	outDir := flags.String("out", "dist/artifacts", "输出目录")
	if err := flags.Parse(args); err != nil {
		return err
	}
	target, err := parseTarget(*targetValue, *tier)
	if err != nil {
		return err
	}
	if *binary == "" {
		return errors.New("--binary 必填")
	}
	inputs := releasekit.Inputs{Repo: *repo, Version: *version, Mode: releasekit.ModeRehearsal}
	sum, err := releasekit.FileSHA256(*binary)
	if err != nil {
		return err
	}
	build := releasekit.BuildResult{Target: target, Version: *version, Binary: *binary, BinarySHA: sum}
	pkg, err := releasekit.PackageTarget(ctx, inputs, target, build, releasekit.ReleaseOptions{
		RuntimeDir: *runtimeDir, Format: *format, OutDir: *outDir,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "archive=%s sha256=%s files=%d\n", pkg.Archive, pkg.SHA256, pkg.FileCount)
	return nil
}

// runRelease 是单目标的完整发布流程：构建、打包、静态校验，匹配宿主时再做双构建
// 比较与冒烟 / 升级探针，并写出可汇总的宿主证据。四平台发布在各自宿主上各跑一次。
// runRelease drives one target end to end: build, package, static verification and,
// on a matching host, the reproducibility comparison plus smoke and upgrade probes.
func runRelease(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("release", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repo := flags.String("repo", ".", "仓库根")
	version := flags.String("version", "", "版本 X.Y.Z")
	tag := flags.String("tag", "", "tag vX.Y.Z")
	mode := flags.String("mode", "rehearsal", "rehearsal / final")
	targetValue := flags.String("target", "", "goos/goarch")
	tier := flags.String("tier", "binary", "full / binary")
	buildDir := flags.String("build-dir", "", "构建输出目录（缺省用临时目录）")
	outDir := flags.String("out", "dist/artifacts", "归档输出目录")
	runtimeDir := flags.String("runtime", "release/runtime", "runtime 目录（full 档）")
	format := flags.String("format", "", "归档格式（缺省按目标平台）")
	evidenceDir := flags.String("evidence-dir", "dist/evidence", "宿主证据目录")
	notes := flags.String("notes", "", "发布说明（给出即校验模板）")
	signingKey := flags.String("signing-key", "", "Ed25519 私钥（final 必填）")
	publicKey := flags.String("public-key", "", "Ed25519 公钥（final 必填）")
	trust := flags.String("trust", "", "信任文件")
	workspace := flags.String("workspace", "", "含 basalt.yaml 的部署工作目录（宿主冒烟用）")
	dsn := flags.String("dsn", "", "PostgreSQL DSN（宿主冒烟用）")
	agent := flags.String("agent", "demo", "冒烟任务用的 Agent 名")
	addr := flags.String("addr", "127.0.0.1:8080", "服务监听地址")
	tokenEnv := flags.String("admin-token-env", "BASALT_SMOKE_ADMIN_TOKEN", "管理面 token 的环境变量名")
	oldArchive := flags.String("old", "", "上一版归档（full 档升级探针）")
	schema := flags.String("schema", "", "升级备份限定的 schema（可选）")
	configPath := flags.String("config", "", "升级探针用的配置文件")
	masterKey := flags.String("master-key", "", "主密钥文件（可选，给出即备份与恢复）")
	adminToken := flags.String("admin-token", "", "admin token 文件（可选，给出即备份与恢复）")
	installDir := flags.String("install-dir", "", "安装目录（缺省取配置文件所在目录）")
	backupDir := flags.String("backup-dir", "", "升级备份目录（缺省落在配置目录下）")
	if err := flags.Parse(args); err != nil {
		return err
	}
	target, err := parseTarget(*targetValue, *tier)
	if err != nil {
		return err
	}
	opts := releasekit.ReleaseOptions{
		Repo: *repo, Version: *version, Tag: *tag, Mode: releasekit.Mode(*mode),
		Target: target, WorkDir: *buildDir, OutDir: *outDir, RuntimeDir: *runtimeDir,
		Format: *format, EvidenceDir: *evidenceDir, NotesPath: *notes,
		SigningKey: *signingKey, PublicKey: *publicKey, TrustFile: *trust,
		Workspace: *workspace, DSN: *dsn, Agent: *agent, Addr: *addr, AdminTokenEnv: *tokenEnv,
		Upgrade: releasekit.UpgradeOptions{
			OldArchive: *oldArchive, Schema: *schema, ConfigPath: *configPath,
			MasterKey: *masterKey, AdminToken: *adminToken, InstallDir: *installDir, WorkDir: *backupDir,
		},
	}
	if err := releasekit.RunRelease(ctx, opts); err != nil {
		return err
	}
	fmt.Fprintf(out, "release_ok=true target=%s tier=%s out=%s evidence=%s\n",
		target.String(), target.Tier, *outDir, *evidenceDir)
	return nil
}

func runVerifyPackage(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("verify-package", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	archive := flags.String("archive", "", "归档路径")
	version := flags.String("version", "", "版本 X.Y.Z")
	targetValue := flags.String("target", "", "goos/goarch（给出即核对包内目标）")
	tier := flags.String("tier", "", "full / binary（与 --target 同时给出）")
	if err := flags.Parse(args); err != nil {
		return err
	}
	in := releasekit.Inputs{Version: *version}
	var err error
	if *targetValue != "" {
		var target releasekit.Target
		target, err = parseTarget(*targetValue, *tier)
		if err == nil {
			err = releasekit.VerifyPackageForTarget(*archive, in, target)
		}
	} else {
		err = releasekit.VerifyPackageStatic(*archive, in)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "archive=%s verified=true\n", *archive)
	return nil
}

// runRecord 单步重建发布记录，用于调试汇总输入；不签名、不写校验和。
// runRecord rebuilds the release record alone for debugging; it does not sign or
// write checksums.
// writeJSONFile 写稳定缩进 JSON，供单步调试子命令复用。
// writeJSONFile writes stable indented JSON for the single-step debug commands.
func writeJSONFile(path string, value any) error {
	body, err := releasekit.MarshalIndentJSON(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644) // #nosec G306 -- 调试产物需要可读
}

func runRecord(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("record", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repo := flags.String("repo", ".", "仓库根")
	artifactDir := flags.String("artifact-dir", "", "四平台归档与 SBOM 目录")
	evidenceDir := flags.String("evidence-dir", "", "宿主证据目录")
	version := flags.String("version", "", "版本 X.Y.Z")
	tag := flags.String("tag", "", "tag vX.Y.Z")
	mode := flags.String("mode", "rehearsal", "rehearsal / final")
	outDir := flags.String("out", "", "记录输出目录")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	in, err := releasekit.LoadInputs(*repo, *version, *tag, releasekit.Mode(*mode))
	if err != nil {
		return err
	}
	pkgs, err := releasekit.LoadPackages(*artifactDir, in)
	if err != nil {
		return err
	}
	matrix, err := releasekit.BuildSupportMatrix(in, *evidenceDir)
	if err != nil {
		return err
	}
	reproducible, err := releasekit.AggregateReproducible(*evidenceDir)
	if err != nil {
		return err
	}
	manifest, err := releasekit.BuildReleaseManifest(in, pkgs)
	if err != nil {
		return err
	}
	record, err := releasekit.BuildReleaseRecord(in, pkgs, matrix, manifest, reproducible)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil { // #nosec G301 -- 调试输出目录
		return err
	}
	matrixPath := filepath.Join(*outDir, "support-matrix.json")
	if err := writeJSONFile(matrixPath, matrix); err != nil {
		return err
	}
	matrixSum, err := releasekit.FileSHA256(matrixPath)
	if err != nil {
		return err
	}
	record.SupportMatrixSHA256 = matrixSum
	if err := writeJSONFile(filepath.Join(*outDir, "release-record.json"), record); err != nil {
		return err
	}
	fmt.Fprintf(out, "record_id=%s out=%s\n", record.RecordID, *outDir)
	return nil
}

// stringList 收集可重复给出的 flag，如 assemble 的 --image-record。
// stringList collects flags that may be repeated, such as assemble's --image-record.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func runAssemble(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("assemble", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	artifactDir := flags.String("artifact-dir", "", "四平台归档目录")
	evidenceDir := flags.String("evidence-dir", "", "宿主证据目录")
	version := flags.String("version", "", "版本 X.Y.Z")
	tag := flags.String("tag", "", "tag vX.Y.Z")
	mode := flags.String("mode", "rehearsal", "rehearsal / final")
	outDir := flags.String("out", "", "发布目录")
	notes := flags.String("notes", "", "发布说明")
	signingKey := flags.String("signing-key", "", "Ed25519 私钥")
	publicKey := flags.String("public-key", "", "Ed25519 公钥")
	trust := flags.String("trust", "", "信任文件")
	commit := flags.String("commit", "", "40 位提交号（缺省取 HEAD，并与仓库核对）")
	var imageRecords stringList
	flags.Var(&imageRecords, "image-record", "镜像记录路径（可重复）")
	var imageEvidence stringList
	flags.Var(&imageEvidence, "image-evidence", "镜像验收检查 ID（可重复；只写实际执行过的检查）")
	if err := flags.Parse(args); err != nil {
		return err
	}
	// 证据必须落在至少一条镜像记录上；没有镜像记录时单独给证据是无意义的。
	if len(imageEvidence) > 0 && len(imageRecords) == 0 {
		return errors.New("--image-evidence 必须与 --image-record 一起使用")
	}
	// 汇总也必须走与构建相同的输入冻结：脏工作区 / tag 不指向 HEAD / 版本非法都要拒绝，
	// 否则记录里的 commit 与实际产物对不上。
	in, err := releasekit.LoadInputs(".", *version, *tag, releasekit.Mode(*mode))
	if err != nil {
		return err
	}
	if *commit != "" && *commit != in.Commit {
		return fmt.Errorf("--commit %s 与仓库 HEAD %s 不一致", *commit, in.Commit)
	}
	options := releasekit.ReleaseOptions{
		SigningKey: *signingKey, PublicKey: *publicKey, TrustFile: *trust,
		NotesPath: *notes, OutDir: *outDir,
	}
	if err := releasekit.Preflight(ctx, in, options); err != nil {
		return err
	}
	if err := releasekit.AssembleRelease(ctx, releasekit.AssembleOptions{
		ArtifactDir: *artifactDir, EvidenceDir: *evidenceDir, Images: imageRecords,
		ImageEvidence: imageEvidence, OutDir: *outDir,
		NotesPath: *notes, SigningKey: *signingKey, PublicKey: *publicKey,
		TrustFile: *trust, Inputs: in,
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "release_dir=%s mode=%s verified=true\n", *outDir, in.Mode)
	return nil
}

// runImage 处理镜像子命令：build 构建镜像并写记录，verify 按记录核对本地镜像与文件。
// runImage handles the image subcommands: build creates the image and its record,
// verify checks the local image and the recorded files against the record.
func runImage(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return errors.New("用法：releasekit image build|verify …")
	}
	switch args[0] {
	case "build":
		return runImageBuild(ctx, args[1:], out)
	case "verify":
		return runImageVerify(ctx, args[1:], out)
	default:
		return fmt.Errorf("image 未知子命令 %q（支持 build / verify）", args[0])
	}
}

func runImageBuild(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("image build", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repo := flags.String("repo", ".", "仓库根")
	version := flags.String("version", "", "版本 X.Y.Z")
	binary := flags.String("binary", "", "linux/amd64 二进制")
	baseRef := flags.String("base-ref", "", "基础镜像引用 name@sha256:…（必须与 release/image-lock.json 一致）")
	dockerfile := flags.String("dockerfile", "docker/Dockerfile", "Dockerfile")
	entrypoint := flags.String("entrypoint", "docker/entrypoint.sh", "entrypoint.sh")
	configFile := flags.String("config", "docker/basalt.yaml", "basalt.yaml 模板")
	workerConfig := flags.String("worker-config", "docker/basalt.worker.yaml", "Worker 配置模板")
	exportKeyScript := flags.String("export-key-script", "docker/export-master-key.sh", "主密钥导出脚本")
	seedScript := flags.String("seed-script", "docker/seed-workspace.sh", "种子导出 / 导入脚本")
	outDir := flags.String("out", "dist/images", "镜像 SBOM / provenance / 记录输出目录")
	recordPath := flags.String("record", "", "镜像记录路径（缺省 <out>/image-record.json）")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*baseRef) == "" {
		return errors.New("--base-ref 必填：镜像必须以冻结的 digest 引用为基础（见 release/image-lock.json）")
	}
	record := *recordPath
	if record == "" {
		record = filepath.Join(*outDir, "image-record.json")
	}
	outAbs, err := filepath.Abs(*outDir)
	if err != nil {
		return err
	}
	recordAbs, err := filepath.Abs(record)
	if err != nil {
		return err
	}
	// 记录里的 SBOM / provenance 只写文件名，核对时相对记录所在目录解析；分放两处
	// 会让“记录与其工件”的绑定关系在目录层面就断掉。
	if filepath.Dir(recordAbs) != outAbs {
		return fmt.Errorf("--record 必须与 --out 同目录（记录目录 %s，产物目录 %s）",
			filepath.Dir(recordAbs), outAbs)
	}
	image, err := releasekit.BuildImage(ctx, releasekit.ImageInputs{
		Repo: *repo, Version: *version,
		Target: releasekit.Target{GOOS: "linux", GOARCH: "amd64", Tier: releasekit.TierBinary},
		Binary: *binary, BaseRef: *baseRef,
		Dockerfile: *dockerfile, Entrypoint: *entrypoint, ConfigFile: *configFile,
		License:          filepath.Join(*repo, "LICENSE"),
		WorkerConfigFile: *workerConfig, ExportKeyScript: *exportKeyScript, SeedScript: *seedScript,
		OutDir: *outDir,
	})
	if err != nil {
		return err
	}
	if err := writeJSONFile(recordAbs, image); err != nil {
		return err
	}
	fmt.Fprintf(out, "image=%s tag=%s local_id=%s record=%s\n", image.Name, image.Tag, image.LocalID, recordAbs)
	return nil
}

func runImageVerify(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("image verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	recordPath := flags.String("record", "", "镜像记录路径")
	requireLocal := flags.Bool("require-local-image", false, "要求本地镜像存在且 ID 与记录一致")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*recordPath) == "" {
		return errors.New("--record 必填")
	}
	body, err := os.ReadFile(*recordPath) // #nosec G304 -- 记录路径由调用方显式给出
	if err != nil {
		return err
	}
	var record releasekit.ImageRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return fmt.Errorf("解析 %s：%w", *recordPath, err)
	}
	// 先核记录自洽与 SBOM / provenance 哈希，再核本地镜像；任何一步失败都意味着
	// 记录不能代表当前镜像。
	if err := record.Validate(); err != nil {
		return err
	}
	if err := releasekit.CheckImageRecordFiles(record, filepath.Dir(*recordPath)); err != nil {
		return err
	}
	if _, err := releasekit.VerifyImage(ctx, record, releasekit.VerifyImageOptions{
		RequireLocalImage: *requireLocal,
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "image=%s verified=true local_id=%s\n", record.Tag, record.LocalID)
	return nil
}

func runVerify(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	releaseDir := flags.String("release-dir", "", "发布目录")
	publicKey := flags.String("public-key", "", "Ed25519 公钥")
	trust := flags.String("trust", "", "信任文件")
	require := flags.Bool("require-signature", false, "要求签名")
	requireImageDigest := flags.Bool("require-image-digest", false, "要求镜像记录带 repo_digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	report, err := releasekit.VerifyRelease(ctx, *releaseDir, releasekit.VerifyPolicy{
		PublicKey: *publicKey, TrustFile: *trust, RequireSignature: *require,
		RequireRepoDigest: *requireImageDigest,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "files=%d signatures=%d signed=%t\n",
		len(report.VerifiedFiles), len(report.VerifiedSignatures), report.Signed)
	return nil
}

func runRepro(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("repro", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	releaseDir := flags.String("release-dir", "", "发布目录")
	publicKey := flags.String("public-key", "", "Ed25519 公钥")
	require := flags.Bool("require-signature", false, "要求签名")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if _, err := releasekit.VerifyRelease(ctx, *releaseDir, releasekit.VerifyPolicy{
		PublicKey: *publicKey, RequireSignature: *require,
	}); err != nil {
		return err
	}
	record, err := releasekit.ReadReleaseRecord(*releaseDir)
	if err != nil {
		return err
	}
	if !record.Reproducible.Checked {
		return errors.New("记录未做双构建比较（reproducible.checked=false），不能声明可复现")
	}
	if !record.Reproducible.Equal {
		return fmt.Errorf("双构建哈希不一致：second_build_sha256=%s", record.Reproducible.SecondBuildSHA256)
	}
	fmt.Fprintf(out, "repro_checked=true equal=true second_build_sha256=%s\n", record.Reproducible.SecondBuildSHA256)
	return nil
}

func runSmoke(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("smoke", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	archive := flags.String("archive", "", "归档路径")
	version := flags.String("version", "", "版本 X.Y.Z")
	targetValue := flags.String("target", "", "goos/goarch")
	tier := flags.String("tier", "full", "full / binary")
	workspace := flags.String("workspace", "", "含 basalt.yaml 的部署工作目录（full 必填）")
	dsn := flags.String("dsn", "", "PostgreSQL DSN（需与 basalt.yaml 一致）")
	agent := flags.String("agent", "demo", "跑 mock 任务用的 Agent 名")
	addr := flags.String("addr", "127.0.0.1:8080", "服务监听地址")
	tokenEnv := flags.String("admin-token-env", "BASALT_SMOKE_ADMIN_TOKEN", "管理面 token 的环境变量名")
	evidenceDir := flags.String("evidence-dir", "", "证据输出目录；给出即写 evidence-<goos>-<goarch>.json")
	if err := flags.Parse(args); err != nil {
		return err
	}
	target, err := parseTarget(*targetValue, *tier)
	if err != nil {
		return err
	}
	pkg := releasekit.PackageResult{
		Archive: *archive, Name: target.ArchiveName(*version), Target: target,
	}
	sum, err := releasekit.FileSHA256(*archive)
	if err != nil {
		return err
	}
	pkg.SHA256 = sum
	opts := releasekit.ReleaseOptions{
		Workspace: *workspace, DSN: *dsn, Agent: *agent, Addr: *addr, AdminTokenEnv: *tokenEnv,
	}
	if err := releasekit.RunSmoke(ctx, pkg, opts); err != nil {
		return err
	}
	if *evidenceDir != "" {
		if err := releasekit.WriteEvidence(*evidenceDir,
			releasekit.Inputs{Version: *version}, pkg, nil); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "smoke_ok=true")
	return nil
}

func runUpgrade(ctx context.Context, args []string, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	oldArchive := flags.String("old", "", "旧归档")
	newArchive := flags.String("new", "", "新归档")
	dsn := flags.String("dsn", "", "PostgreSQL DSN")
	schema := flags.String("schema", "", "限定备份的 schema（可选）")
	workspace := flags.String("workspace", "", "workspace")
	configPath := flags.String("config", "", "配置文件")
	masterKey := flags.String("master-key", "", "主密钥文件")
	adminToken := flags.String("admin-token", "", "admin token 文件")
	installDir := flags.String("install-dir", "", "安装目录")
	backupDir := flags.String("backup-dir", "", "备份目录")
	if err := flags.Parse(args); err != nil {
		return err
	}
	// 自动升级尚未在真实宿主完成「停服 → 升级 → 启动 → 失败恢复」闭环验证，
	// 只作为后续首发验证入口；生产升级必须先按人工步骤执行。
	fmt.Fprintln(errOut, "releasekit upgrade: 受限入口，未完成真实宿主升级／失败恢复闭环验证；"+
		"生产升级请按 docs/Operations.md §5.1 人工执行，失败按 §5.2 / §4.4 恢复。")
	backup, err := releasekit.RunUpgrade(ctx, releasekit.UpgradeOptions{
		OldArchive: *oldArchive, NewArchive: *newArchive, DSN: *dsn, Schema: *schema,
		Workspace: *workspace, ConfigPath: *configPath, MasterKey: *masterKey,
		AdminToken: *adminToken, InstallDir: *installDir, WorkDir: *backupDir,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "backup=%s\n", backup.Root)
	return nil
}

func runSBOM(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("sbom", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repo := flags.String("repo", ".", "仓库根")
	version := flags.String("version", "", "版本 X.Y.Z")
	artifactDir := flags.String("artifact-dir", "", "四平台归档目录")
	outDir := flags.String("out", "", "输出目录")
	if err := flags.Parse(args); err != nil {
		return err
	}
	in := releasekit.Inputs{Repo: *repo, Version: *version}
	pkgs, err := releasekit.LoadPackages(*artifactDir, in)
	if err != nil {
		return err
	}
	if err := releasekit.BuildSBOM(in, pkgs, *outDir); err != nil {
		return err
	}
	fmt.Fprintln(out, "sbom_ok=true")
	return nil
}

func runProvenance(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("provenance", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	version := flags.String("version", "", "版本 X.Y.Z")
	commit := flags.String("commit", "", "40 位提交号")
	artifactDir := flags.String("artifact-dir", "", "四平台归档目录")
	outDir := flags.String("out", "", "输出目录")
	if err := flags.Parse(args); err != nil {
		return err
	}
	in := releasekit.Inputs{Version: *version, Commit: *commit}
	pkgs, err := releasekit.LoadPackages(*artifactDir, in)
	if err != nil {
		return err
	}
	if err := releasekit.BuildProvenance(in, pkgs, *outDir); err != nil {
		return err
	}
	fmt.Fprintln(out, "provenance_ok=true")
	return nil
}

func runSign(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("sign", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	key := flags.String("key", "", "Ed25519 私钥")
	dir := flags.String("dir", "", "文件目录")
	name := flags.String("name", "", "文件名")
	if err := flags.Parse(args); err != nil {
		return err
	}
	info, err := releasekit.SignFile(*key, *dir, *name)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signature=%s fingerprint=%s\n", info.Signature, info.Fingerprint)
	return nil
}
