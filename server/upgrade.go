package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"context"
)

// Self-upgrade from GitHub releases. `memstated upgrade` is CLI-only, like
// export/import; the daemon's role is limited to noticing that a newer
// release exists and saying so (stderr log line + latest_available in
// /health) — it never replaces itself.

// releaseRepo is the GitHub repo whose releases carry the platform binaries
// built by CI (.github/workflows/build.yml, `make release` naming).
const releaseRepo = "map588/memstate"

// latestAvailable holds the newest release version seen by watchUpdates when
// it is strictly newer than this build; empty means up to date (or unchecked).
var latestAvailable atomic.Value // string

func updateAvailable() string {
	s, _ := latestAvailable.Load().(string)
	return s
}

// watchUpdates polls the GitHub releases API once at startup and then daily
// (shared daemons can live for weeks). Failures are silent — being offline
// is normal and must not spam the log. Disabled entirely by
// MEMSTATE_NO_UPDATE_CHECK (tests set it so hermetic daemons never touch
// the network).
func watchUpdates(ctx context.Context) {
	check := func() {
		rel, err := fetchLatestRelease(10 * time.Second)
		if err != nil {
			return
		}
		latest := strings.TrimPrefix(rel.TagName, "v")
		if !versionNewer(latest, healthVersion) || updateAvailable() == latest {
			return
		}
		latestAvailable.Store(latest)
		fmt.Fprintf(os.Stderr,
			"memstated: v%s is available (running v%s) — run `memstated upgrade` to update\n",
			latest, healthVersion)
	}
	check()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func fetchLatestRelease(timeout time.Duration) (*ghRelease, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet,
		"https://api.github.com/repos/"+releaseRepo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET releases/latest: HTTP %d", resp.StatusCode)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("release has no tag_name")
	}
	return &rel, nil
}

// parseVersion reads up to three dotted numeric segments, tolerating a
// leading "v" and a pre-release suffix ("1.2.3-rc1" → [1 2 3]). Non-numeric
// segments count as 0.
func parseVersion(s string) [3]int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	var v [3]int
	for i, part := range strings.SplitN(s, ".", 3) {
		n, _ := strconv.Atoi(part)
		v[i] = n
	}
	return v
}

// versionNewer reports whether a is strictly newer than b.
func versionNewer(a, b string) bool {
	va, vb := parseVersion(a), parseVersion(b)
	for i := range va {
		if va[i] != vb[i] {
			return va[i] > vb[i]
		}
	}
	return false
}

// releaseAssetName matches the naming in the Makefile's release target.
func releaseAssetName() string {
	name := "memstated-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func cmdUpgrade(args []string) int {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	addr := fs.String("addr", "",
		"shared daemon to restart after the swap (default MEMSTATE_ADDR or "+defaultAddr+")")
	_ = fs.Parse(args)
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: memstated upgrade [--addr HOST:PORT]")
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "memstated upgrade: %v\n", err)
		return 1
	}

	rel, err := fetchLatestRelease(15 * time.Second)
	if err != nil {
		return fail(err)
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	if !versionNewer(latest, healthVersion) {
		fmt.Printf("already up to date (running v%s, latest release %s)\n",
			healthVersion, rel.TagName)
		return 0
	}

	want := releaseAssetName()
	var url string
	var names []string
	for _, a := range rel.Assets {
		names = append(names, a.Name)
		if a.Name == want {
			url = a.BrowserDownloadURL
		}
	}
	if url == "" {
		return fail(fmt.Errorf("release %s has no asset %q (assets: %s)",
			rel.TagName, want, strings.Join(names, ", ")))
	}

	exePath, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	// Resolve symlinks (e.g. a GOBIN shim) so we replace the real file, not
	// the link.
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	fmt.Printf("downloading %s %s → %s\n", rel.TagName, want, exePath)
	tmp := exePath + ".download"
	if err := downloadTo(url, tmp); err != nil {
		return fail(err)
	}
	defer os.Remove(tmp) // no-op after the rename succeeds

	// Stop a running shared daemon BEFORE swapping the binary: Windows
	// refuses to move a running executable, and on every platform it avoids
	// a window where /health reports the old version from the new file.
	restartAddr := *addr
	if restartAddr == "" {
		restartAddr = os.Getenv("MEMSTATE_ADDR")
	}
	if restartAddr == "" {
		restartAddr = defaultAddr
	}
	wasRunning := looksLikeOurDaemon(restartAddr)
	if wasRunning {
		fmt.Printf("stopping daemon at %s\n", restartAddr)
		if err := stopAndWait(restartAddr, 5*time.Second); err != nil {
			return fail(err)
		}
	}

	if err := replaceBinary(tmp, exePath); err != nil {
		if wasRunning {
			fmt.Fprintf(os.Stderr,
				"memstated upgrade: daemon at %s was stopped but not restarted — restart it manually\n",
				restartAddr)
		}
		return fail(err)
	}

	if wasRunning {
		if err := startDetachedDaemon(exePath, restartAddr); err != nil {
			fmt.Fprintf(os.Stderr,
				"memstated upgrade: binary upgraded but restart failed: %v\n"+
					"start it manually: memstated --addr %s\n", err, restartAddr)
			return 1
		}
		fmt.Printf("daemon restarted at %s\n", restartAddr)
	} else {
		fmt.Println("no running shared daemon found — child-mode daemons pick up the new binary next session")
	}
	fmt.Printf("upgraded v%s → %s\n", healthVersion, rel.TagName)
	return 0
}

func downloadTo(url, dest string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	_, cpErr := io.Copy(f, resp.Body)
	if cerr := f.Close(); cpErr == nil {
		cpErr = cerr
	}
	if cpErr != nil {
		os.Remove(dest)
		return cpErr
	}
	return nil
}

// replaceBinary swaps tmp into place. The old binary is renamed aside first
// (renaming a running executable is legal even on Windows, deleting or
// overwriting it is not) and then removed best-effort.
func replaceBinary(tmp, exePath string) error {
	old := exePath + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exePath, old); err != nil {
		return err
	}
	if err := os.Rename(tmp, exePath); err != nil {
		_ = os.Rename(old, exePath) // roll back
		return err
	}
	_ = os.Remove(old) // may fail on Windows while still mapped; harmless leftover
	return nil
}

// stopAndWait shuts the daemon down and polls until the port stops answering.
func stopAndWait(addr string, timeout time.Duration) error {
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/admin/shutdown", nil)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("stop %s: %w", addr, err)
	}
	resp.Body.Close()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r, err := client.Get("http://" + addr + "/health"); err != nil {
			return nil // nothing listening — it's down
		} else {
			r.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon at %s did not exit within %v", addr, timeout)
}

// startDetachedDaemon launches the (new) binary as a shared daemon in its own
// session, stderr/stdout appended to ~/.memstate/memstated.log — the same
// file the MCP proxy tees child-mode daemons into.
func startDetachedDaemon(exePath, addr string) error {
	var logF *os.File
	if home, err := os.UserHomeDir(); err == nil {
		dir := filepath.Join(home, ".memstate")
		_ = os.MkdirAll(dir, 0o755)
		logF, _ = os.OpenFile(filepath.Join(dir, "memstated.log"),
			os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	}
	cmd := exec.Command(exePath, "--addr", addr)
	cmd.SysProcAttr = detachSysProcAttr()
	if logF != nil {
		cmd.Stdout = logF
		cmd.Stderr = logF
	}
	err := cmd.Start()
	if logF != nil {
		logF.Close() // the child holds its own descriptor after Start
	}
	if err != nil {
		return err
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if looksLikeOurDaemon(addr) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not answer /health at %s within 5s (see ~/.memstate/memstated.log)", addr)
}
