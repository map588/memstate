// Command memstated is a local HTTP backend for versioned agent memory.
//
// It speaks REST — NOT MCP. The MCP surface lives in the TypeScript proxy
// under ../client/, which spawns this binary as a child and forwards MCP
// tool calls here over HTTP loopback.
//
// Lifetime model:
//   - Default ("child mode"): listen on :0 (OS-picked port), print a banner
//     "MEMSTATE_READY addr=127.0.0.1:<port>" on stderr, watch --owner-pid
//     via kill(pid, 0) every 2s and shut down if it vanishes. The parent
//     reads the banner to learn our address, and kills us on its way out.
//   - "Shared daemon" mode (explicit --addr or MEMSTATE_ADDR): bind to the
//     named address. If EADDRINUSE and /health says it's us, exit 0 quietly.
//     If EADDRINUSE and the occupant is alien, exit 2 loudly.
//
// Subcommands:
//   - memstated          — run the daemon (default)
//   - memstated stop     — POST /admin/shutdown at the configured addr
//   - memstated status   — GET /health and print the response
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	defaultAddr       = "127.0.0.1:8765" // used only in --addr / stop / status
	healthServiceName = "memstate"
	healthVersion     = "0.1.0"
	readyBanner       = "MEMSTATE_READY addr="
)

func defaultDBPath() string {
	if v := os.Getenv("MEMSTATE_DB"); v != "" {
		return expandHome(v)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".memstate", "memstate.db")
}

// expandHome resolves a leading ~/ or bare ~ to the current user's home dir.
// Shells normally do this, but MEMSTATE_DB is commonly set in config files
// (e.g. an MCP server JSON entry) where the shell never touches it.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		if p == "~" {
			return home
		}
		return filepath.Join(home, p[2:])
	}
	return p
}

func main() {
	// Dispatch subcommands before flag parsing; the subcommands have their
	// own simple arg handling and don't spin up the server.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "stop":
			os.Exit(cmdStop(os.Args[2:]))
		case "status":
			os.Exit(cmdStatus(os.Args[2:]))
		case "-h", "--help", "help":
			printUsage()
			os.Exit(0)
		}
	}

	// --- server mode -----------------------------------------------------
	fs := flag.NewFlagSet("memstated", flag.ExitOnError)
	addrFlag := fs.String("addr", "",
		"bind address (default: random port via :0). "+
			"Set explicitly (e.g. 127.0.0.1:8765) for shared-daemon mode.")
	ownerPIDFlag := fs.Int("owner-pid", 0,
		"parent PID to monitor — daemon exits when the PID disappears (0 = disabled)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	resolved := *addrFlag
	if resolved == "" {
		resolved = os.Getenv("MEMSTATE_ADDR")
	}
	explicitAddr := resolved != ""
	if !explicitAddr {
		resolved = "127.0.0.1:0" // random port
	}

	ln, err := net.Listen("tcp", resolved)
	if err != nil {
		if explicitAddr && isAddrInUse(err) {
			handleBusyPort(resolved)
		}
		log.Fatalf("memstated: listen %s: %v", resolved, err)
	}

	actualAddr := ln.Addr().String()

	dbPath := defaultDBPath()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		ln.Close()
		log.Fatalf("memstated: mkdir %s: %v", filepath.Dir(dbPath), err)
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		ln.Close()
		log.Fatalf("memstated: open store %s: %v", dbPath, err)
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Wire the admin shutdown route so POST /admin/shutdown cleanly ends
	// the server by cancelling the root context.
	shutdownFn := func() { stop() }

	embedder := NewEmbedder()

	srv := &http.Server{
		Handler:           newRouter(store, shutdownFn, embedder),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	// Parent-death watchdog. kill(pid, 0) returns ESRCH when the pid no
	// longer names a live process. We poll because Unix does not give Go
	// an "exit when parent dies" primitive that works cross-platform.
	if *ownerPIDFlag > 0 {
		go watchOwner(*ownerPIDFlag, shutdownFn)
	}

	// Announce our bind address on stderr so the spawning parent can find us.
	// Print the banner BEFORE Serve blocks so the parent's stderr reader
	// sees it deterministically.
	fmt.Fprintf(os.Stderr, "%s%s db=%s\n", readyBanner, actualAddr, dbPath)

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("memstated: serve: %v", err)
	}
}

// handleBusyPort is only reached in --addr mode. It distinguishes
// "another memstated" (exit 0 — benign double-start) from "an unrelated
// process squatting on the port" (exit 2 — loud human-visible failure).
func handleBusyPort(addr string) {
	if looksLikeOurDaemon(addr) {
		fmt.Fprintf(os.Stderr, "memstated: already running on %s\n", addr)
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr,
		"memstated: port %s is occupied by a non-memstate process; refusing to start\n",
		addr)
	os.Exit(2)
}

func isAddrInUse(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && errors.Is(opErr.Err, syscall.EADDRINUSE) {
		return true
	}
	return strings.Contains(err.Error(), "address already in use")
}

func looksLikeOurDaemon(addr string) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil || resp == nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	h, err := decodeHealth(resp.Body)
	if err != nil {
		return false
	}
	return h.Service == healthServiceName
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

// ---------- subcommands ----------

func printUsage() {
	fmt.Fprint(os.Stderr,
		`Usage:
  memstated                        run the daemon (random port by default)
  memstated --addr HOST:PORT       run on an explicit address (shared mode)
  memstated --owner-pid N          shut down when process N disappears
  memstated stop   [--addr HOST:PORT]   send a shutdown request to a running daemon
  memstated status [--addr HOST:PORT]   query /health

Environment:
  MEMSTATE_ADDR   default for --addr
  MEMSTATE_DB     SQLite file path (default ~/.memstate/memstate.db)
`)
}

func subAddr(args []string) string {
	fs := flag.NewFlagSet("subcmd", flag.ExitOnError)
	addr := fs.String("addr", "", "")
	_ = fs.Parse(args)
	if *addr != "" {
		return *addr
	}
	if v := os.Getenv("MEMSTATE_ADDR"); v != "" {
		return v
	}
	return defaultAddr
}

func cmdStop(args []string) int {
	addr := subAddr(args)
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/admin/shutdown",
		bytes.NewReader(nil))
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated stop: could not reach %s: %v\n", addr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "memstated stop: HTTP %d %s\n", resp.StatusCode, body)
		return 1
	}
	fmt.Fprintf(os.Stderr, "memstated stop: shutdown requested at %s\n", addr)
	return 0
}

func cmdStatus(args []string) int {
	addr := subAddr(args)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated status: %s unreachable: %v\n", addr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "memstated status: HTTP %d %s\n", resp.StatusCode, body)
		return 1
	}
	var h map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		fmt.Fprintf(os.Stderr, "memstated status: decode error: %v\n", err)
		return 1
	}
	out, _ := json.MarshalIndent(h, "", "  ")
	fmt.Println(string(out))
	if h["service"] != healthServiceName {
		return 2
	}
	return 0
}
