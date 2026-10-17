//go:build !windows

package main

import (
	"errors"
	"os"
)

// syncDir 在 rename 之后同步父目录，保证崩溃后目录项仍指向新文件。
// 目录经 os.Root 打开（不是 os.Open）：路径来自报告输出参数，os.Open 以变量路径
// 打开会被 gosec G304 判为潜在文件包含；Root 把访问限定在已打开目录内。
// syncDir fsyncs the parent directory so a crash cannot lose the rename. The
// directory is opened through os.Root, which scopes the access and avoids G304.
func syncDir(root *os.Root) error {
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
