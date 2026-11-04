//go:build !windows && !linux && !darwin

package tool

import "os"

func flowFileIdentity(_ *os.File, _ os.FileInfo) (string, error) { return "", ErrFlowToolUnavailable }
