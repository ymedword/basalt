//go:build linux || darwin

package tool

import (
	"fmt"
	"os"
	"syscall"
)

func flowFileIdentity(_ *os.File, info os.FileInfo) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", ErrFlowToolUnavailable
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), nil
}
