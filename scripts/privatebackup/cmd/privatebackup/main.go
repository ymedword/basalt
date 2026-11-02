// Command privatebackup 备份与恢复未入库私有资产。
// Command privatebackup snapshots and restores private assets kept out of Git.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ymedword/basalt/scripts/privatebackup"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "snapshot":
		return snapshot(args[1:])
	case "verify":
		return verify(args[1:])
	case "restore":
		return restore(args[1:])
	case "inspect":
		return inspect(args[1:])
	default:
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `privatebackup - 私有资产快照
  privatebackup snapshot --repo . --out <tar.gz> [<path>...]
    （.git/info/exclude 里的路径自动纳入；--out 必须在工作区之外）
  privatebackup verify --archive <tar.gz>
  privatebackup restore --archive <tar.gz> --dest <dir>
  privatebackup inspect --archive <tar.gz>`)
}

func snapshot(args []string) int {
	flags := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repo := flags.String("repo", ".", "仓库根")
	out := flags.String("out", "", "快照路径")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	manifest, err := privatebackup.CreateSnapshot(*repo, *out, flags.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "privatebackup:", err)
		return 1
	}
	fmt.Printf("archive=%s files=%d\n", *out, len(manifest.Entries))
	return 0
}

func verify(args []string) int {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	archive := flags.String("archive", "", "快照路径")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	manifest, err := privatebackup.Verify(*archive)
	if err != nil {
		fmt.Fprintln(os.Stderr, "privatebackup:", err)
		return 1
	}
	fmt.Printf("archive=%s files=%d verified=true\n", *archive, len(manifest.Entries))
	return 0
}

// inspect 只读校验并打印清单，不写目标目录。
// inspect verifies the archive and prints its inventory without extracting.
func inspect(args []string) int {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	archive := flags.String("archive", "", "快照路径")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	manifest, err := privatebackup.Verify(*archive)
	if err != nil {
		fmt.Fprintln(os.Stderr, "privatebackup:", err)
		return 1
	}
	for _, entry := range manifest.Entries {
		fmt.Printf("%s %d %s\n", entry.SHA256, entry.Size, entry.Path)
	}
	return 0
}

func restore(args []string) int {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	archive := flags.String("archive", "", "快照路径")
	dest := flags.String("dest", "", "恢复目录")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	manifest, err := privatebackup.RestoreSnapshot(*archive, *dest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "privatebackup:", err)
		return 1
	}
	fmt.Printf("dest=%s files=%d restored=true\n", *dest, len(manifest.Entries))
	return 0
}
