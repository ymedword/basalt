//go:build !linux

package app

import "errors"

func outputTmpfsSize(string) (int64, error) {
	return 0, errors.New("隔离执行需要 Linux 与本实例独占 tmpfs")
}
