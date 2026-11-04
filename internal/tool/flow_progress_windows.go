//go:build windows

package tool

import (
	"fmt"
	"os"
	"syscall"
)

func flowFileIdentity(file *os.File, _ os.FileInfo) (string, error) {
	var data syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &data); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%d", data.VolumeSerialNumber, data.FileIndexHigh, data.FileIndexLow), nil
}
