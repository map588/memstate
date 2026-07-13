package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

func buildDaemon(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "memstated")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// cleanEnv is os.Environ() with all MEMSTATE_* vars stripped, so a developer
// shell that talks to a shared daemon (MEMSTATE_ADDR set) cannot leak that
// config into the hermetic test daemons.
func cleanEnv() []string {
	env := os.Environ()
	out := env[:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "MEMSTATE_") {
			out = append(out, kv)
		}
	}
	return out
}

// runDaemon spawns memstated with the given args + env. Returns the process,
// a reader for everything the daemon emits on stderr (after the banner), and
// the address parsed from the banner. The caller must eventually kill or
// wait on the process.
func runDaemon(
	t *testing.T, bin string, args []string, extraEnv map[string]string,
) (*exec.Cmd, *bufio.Scanner, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(cleanEnv(),
		"MEMSTATE_DB="+filepath.Join(t.TempDir(), "t.db"),
		// Hermetic daemons must never call the GitHub releases API.
		"MEMSTATE_NO_UPDATE_CHECK=1",
	)
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Scan stderr until the READY banner appears.
	scanner := bufio.NewScanner(stderr)
	addrRE := regexp.MustCompile(`MEMSTATE_READY addr=([^ ]+)`)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && scanner.Scan() {
		line := scanner.Text()
		if m := addrRE.FindStringSubmatch(line); m != nil {
			return cmd, scanner, m[1]
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	t.Fatalf("daemon never printed READY banner")
	return nil, nil, ""
}

func getHealth(t *testing.T, addr string) bool {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var h map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&h)
	return h["service"] == "memstate"
}

// --- tests -----------------------------------------------------------------

// TestRandomPortBanner verifies the default path: no --addr, OS picks a port,
// daemon prints it on stderr, /health is reachable on that address.
func TestRandomPortBanner(t *testing.T) {
	bin := buildDaemon(t)
	cmd, _, addr := runDaemon(t, bin, nil, nil)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("expected 127.0.0.1 bind, got %s", addr)
	}
	if !getHealth(t, addr) {
		t.Fatalf("health check failed at %s", addr)
	}
}

// TestOwnerPIDOrphanShutdown: spawn a throwaway "sleep" as the pretend
// parent, pass its pid via --owner-pid, kill the sleep, and verify the
// daemon exits within a few seconds.
func TestOwnerPIDOrphanShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-pid semantics rely on POSIX kill(pid,0)")
	}
	bin := buildDaemon(t)

	// Surrogate parent that stays alive until we kill it.
	surrogate := exec.Command("sleep", "60")
	if err := surrogate.Start(); err != nil {
		t.Fatalf("surrogate: %v", err)
	}
	surrogatePID := surrogate.Process.Pid
	defer func() {
		if surrogate.Process != nil {
			_ = surrogate.Process.Kill()
			_ = surrogate.Wait()
		}
	}()

	cmd, _, _ := runDaemon(t, bin, []string{
		"--owner-pid", intStr(surrogatePID),
	}, nil)
	t.Cleanup(func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	// Kill the surrogate; daemon should notice within 2-3 seconds.
	_ = surrogate.Process.Kill()
	_ = surrogate.Wait()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// ok
	case <-time.After(6 * time.Second):
		t.Fatalf("daemon did not shut down when owner-pid disappeared")
	}
}

// TestShutdownEndpoint: POST /admin/shutdown cleanly terminates the daemon.
func TestShutdownEndpoint(t *testing.T) {
	bin := buildDaemon(t)
	cmd, _, addr := runDaemon(t, bin, nil, nil)
	t.Cleanup(func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	resp, err := http.Post("http://"+addr+"/admin/shutdown", "", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("POST shutdown: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		t.Fatalf("shutdown status: %d", resp.StatusCode)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// ok
	case <-time.After(5 * time.Second):
		t.Fatalf("daemon did not exit after /admin/shutdown")
	}
}

// TestSubcommandStop verifies `memstated stop --addr` calls /admin/shutdown.
func TestSubcommandStop(t *testing.T) {
	bin := buildDaemon(t)

	// Spin up on a fixed free port so we can point `stop` at it.
	addr := reserveAndRelease(t)
	cmd, _, _ := runDaemon(t, bin, []string{"--addr", addr}, nil)
	t.Cleanup(func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	stopCmd := exec.Command(bin, "stop", "--addr", addr)
	out, err := stopCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "shutdown requested") {
		t.Fatalf("expected 'shutdown requested', got: %s", out)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("daemon did not exit after stop subcommand")
	}
}

// TestSubcommandStatus hits /health via the status subcommand.
func TestSubcommandStatus(t *testing.T) {
	bin := buildDaemon(t)
	cmd, _, addr := runDaemon(t, bin, nil, nil)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	out, err := exec.Command(bin, "status", "--addr", addr).CombinedOutput()
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"service": "memstate"`) {
		t.Fatalf("expected service line, got:\n%s", out)
	}
}

// TestExplicitAddrDoubleStart: the old shared-daemon guards are still live
// when someone opts into --addr. Second `memstated --addr X` against a live
// first instance exits 0 with an "already running" diagnostic.
func TestExplicitAddrDoubleStart(t *testing.T) {
	bin := buildDaemon(t)
	addr := reserveAndRelease(t)
	cmd, _, _ := runDaemon(t, bin, []string{"--addr", addr}, nil)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	second := exec.Command(bin, "--addr", addr)
	second.Env = append(cleanEnv(), "MEMSTATE_DB="+filepath.Join(t.TempDir(), "x.db"))
	out, err := second.CombinedOutput()
	if err != nil {
		t.Fatalf("second --addr start should exit 0, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "already running") {
		t.Fatalf("expected 'already running' message, got: %s", out)
	}
}

// TestExplicitAddrAlien: --addr points at a port held by an unrelated HTTP
// server whose /health does NOT look like memstate's. Daemon exits 2 loudly.
func TestExplicitAddrAlien(t *testing.T) {
	bin := buildDaemon(t)
	alien := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"service":"other","version":"1"}`)
	}))
	t.Cleanup(alien.Close)
	addr := strings.TrimPrefix(alien.URL, "http://")

	cmd := exec.Command(bin, "--addr", addr)
	cmd.Env = append(cleanEnv(), "MEMSTATE_DB="+filepath.Join(t.TempDir(), "x.db"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit, got 0.\n%s", out)
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Fatalf("expected exit 2, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "non-memstate process") {
		t.Fatalf("expected alien diagnostic, got: %s", out)
	}
}

// --- small utilities -------------------------------------------------------

func reserveAndRelease(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func intStr(n int) string {
	return (func() string {
		b := make([]byte, 0, 10)
		if n == 0 {
			return "0"
		}
		neg := false
		if n < 0 {
			neg = true
			n = -n
		}
		for n > 0 {
			b = append([]byte{byte('0' + n%10)}, b...)
			n /= 10
		}
		if neg {
			b = append([]byte{'-'}, b...)
		}
		return string(b)
	})()
}
