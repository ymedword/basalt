//go:build linux

package app

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"syscall"
)

func outputTmpfsSize(path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, errors.New("output_root 必须是非链接目录")
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Type != 0x01021994 {
		return 0, errors.New("output_root 必须位于独占 tmpfs")
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return 0, err
	}
	rootStat, ok := info.Sys().(*syscall.Stat_t)
	parentStat, parentOK := parent.Sys().(*syscall.Stat_t)
	if !ok || !parentOK || rootStat.Dev == parentStat.Dev {
		return 0, errors.New("output_root 必须是本实例独占 tmpfs 挂载点")
	}
	if stat.Bsize <= 0 || stat.Blocks > uint64(math.MaxInt64)/uint64(stat.Bsize) {
		return 0, errors.New("tmpfs 大小无法安全换算")
	}
	return int64(stat.Blocks) * stat.Bsize, nil // #nosec G115 -- 上面已校验 int64 范围
}
