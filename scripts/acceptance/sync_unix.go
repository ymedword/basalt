//go:build !windows

package main

import (
	"errors"
	"os"
)

// syncDir 在 rename 之后同步父目录，保证崩溃后目录项仍指向新文件。
// syncDir fsyncs the parent directory so a crash cannot lose the rename.
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
