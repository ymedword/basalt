package main

import (
	"errors"
	"math"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func prepareProcess(cmd *exec.Cmd) {
	// Suspend before job assignment so descendants cannot escape that interval.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW}
}

func trackProcess(cmd *exec.Cmd) (func() error, error) {
	pid := cmd.Process.Pid
	if pid < 1 || pid > math.MaxUint32 {
		return nil, errors.New("invalid Windows process ID")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return nil, errors.Join(err, windows.CloseHandle(job))
	}
	assignErr := windows.AssignProcessToJobObject(job, process)
	closeErr := windows.CloseHandle(process)
	if err := errors.Join(assignErr, closeErr); err != nil {
		return nil, errors.Join(err, windows.CloseHandle(job))
	}
	stop := func() error { return errors.Join(windows.TerminateJobObject(job, 1), windows.CloseHandle(job)) }
	if err := resumePrimaryThread(uint32(pid)); err != nil {
		return nil, errors.Join(err, stop())
	}
	return stop, nil
}

// resumePrimaryThread 在 Job 关联后恢复挂起进程，避开启动到隔离之间的逃逸窗口。
// os/exec closes the initial thread handle. A suspended process has one thread;
// find it with Toolhelp after job assignment and resume it with typed APIs.
func resumePrimaryThread(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry)) // constant size; no pointer conversion
	for err := windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return err
		}
		_, resumeErr := windows.ResumeThread(thread)
		return errors.Join(resumeErr, windows.CloseHandle(thread))
	}
	return errors.New("suspended process thread not found")
}
