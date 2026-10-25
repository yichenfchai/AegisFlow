//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

func procAttr() *syscall.SysProcAttr {
	// Own process group: required for GenerateConsoleCtrlEvent to reach only
	// the server, and mapped to Go's SIGTERM delivery on Windows.
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// terminate delivers the real termination signal on Windows: Go maps
// CTRL_BREAK_EVENT to syscall.SIGTERM.
func terminate(cmd *exec.Cmd) error {
	const ctrlBreakEvent = 1
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	generate := kernel32.NewProc("GenerateConsoleCtrlEvent")
	r1, _, callErr := generate.Call(ctrlBreakEvent, uintptr(cmd.Process.Pid))
	if r1 == 0 {
		return callErr
	}
	return nil
}
