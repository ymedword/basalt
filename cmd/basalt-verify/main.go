// Command basalt-verify 只依赖下载目录校验发布物。
// Command basalt-verify verifies a release using only the download directory.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/ymedword/basalt/scripts/releasekit"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("basalt-verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	releaseDir := flags.String("release-dir", "", "发布目录")
	publicKey := flags.String("public-key", "basalt-release.pub", "Ed25519 公钥")
	trust := flags.String("trust", "", "信任文件（可选）")
	require := flags.Bool("require-signature", true, "要求签名")
	requireImageDigest := flags.Bool("require-image-digest", false,
		"要求发布记录里的镜像带 repo_digest（对外发布镜像时使用）")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *releaseDir == "" {
		fmt.Fprintln(os.Stderr, "basalt-verify: --release-dir 必填")
		return 2
	}
	report, err := releasekit.VerifyRelease(ctx, *releaseDir, releasekit.VerifyPolicy{
		PublicKey: *publicKey, TrustFile: *trust, RequireSignature: *require,
		RequireRepoDigest: *requireImageDigest,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "basalt-verify:", err)
		return 1
	}
	fmt.Printf("release=%s files=%d signatures=%d signed=%t\n",
		*releaseDir, len(report.VerifiedFiles), len(report.VerifiedSignatures), report.Signed)
	return 0
}
