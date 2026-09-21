//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// isPlatformAddrInUse reports the Windows form of "address already in use".
// Winsock returns WSAEADDRINUSE (10048), which is not syscall.EADDRINUSE.
func isPlatformAddrInUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE)
}

// detachSysProcAttr detaches the restarted daemon from the console so it
// survives the upgrade process exiting.
// 0x00000008 = DETACHED_PROCESS, 0x00000200 = CREATE_NEW_PROCESS_GROUP.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200}
}

// watchOwner blocks on the parent process handle and triggers shutdown when
// it exits — Windows' equivalent of the Unix kill(pid, 0) poll.
func watchOwner(pid int, shutdown func()) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// Can't watch (already gone or no access): assume gone — a child-mode
		// daemon without a live owner must not linger.
		fmt.Fprintf(os.Stderr,
			"memstated: cannot watch owner pid %d (%v) — shutting down\n", pid, err)
		shutdown()
		return
	}
	defer windows.CloseHandle(h)
	_, _ = windows.WaitForSingleObject(h, windows.INFINITE)
	fmt.Fprintf(os.Stderr,
		"memstated: owner pid %d vanished — shutting down\n", pid)
	shutdown()
}
