//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// detachSysProcAttr puts the restarted daemon in its own session so it
// survives the upgrade process (and its terminal) exiting.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// watchOwner polls the parent PID and triggers shutdown when it vanishes.
// Signal 0 is the canonical Unix "does this pid exist AND can I signal it"
// probe; ESRCH means the owner is gone.
func watchOwner(pid int, shutdown func()) {
	for {
		time.Sleep(2 * time.Second)
		if err := syscall.Kill(pid, 0); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				fmt.Fprintf(os.Stderr,
					"memstated: owner pid %d vanished — shutting down\n", pid)
				shutdown()
				return
			}
			// EPERM ("process exists, you can't signal it") is still alive.
			// Anything else: be conservative and keep running.
		}
	}
}
