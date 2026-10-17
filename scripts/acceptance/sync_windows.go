//go:build windows

package main

// syncDir 在 Windows 上为空操作：目录句柄不支持 FlushFileBuffers，
// rename 的元数据持久化由文件系统保证。
// syncDir is a no-op on Windows; directory handles do not support FlushFileBuffers.
func syncDir(string) error { return nil }
