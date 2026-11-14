package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
		"shared daemon of this machine to restart after the swap (default MEMSTATE_ADDR or "+defaultAddr+")")
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
	if err := downloadRelease(rel, want, tmp); err != nil {
		return fail(err)
	}
	defer os.Remove(tmp) // no-op after the rename succeeds
	fmt.Printf("checksum matches %s\n", checksumAsset)

	restartAddr := *addr
	if restartAddr == "" {
		restartAddr = os.Getenv("MEMSTATE_ADDR")
	}
	if restartAddr == "" {
		restartAddr = defaultAddr
	}
	if err := installDownload(tmp, exePath, restartAddr); err != nil {
		return fail(err)
	}
	fmt.Printf("upgraded v%s → %s\n", healthVersion, rel.TagName)
	return 0
}

// installDownload puts the downloaded binary tmp at exePath. When a shared
// daemon of this machine listens on addr, it stops that daemon before the
// swap and starts the new binary in its place: Windows refuses to move a
// running executable, and on every platform the stop avoids a window where
// /health reports the old version from the new file. It leaves the daemon
// of another machine alone (locateDaemon).
func installDownload(tmp, exePath, addr string) error {
	// Read the running daemon's flags first, so the restart replays them.
	prev, site := locateDaemon(addr)
	if site == daemonHere {
		fmt.Printf("stopping daemon at %s\n", addr)
		if err := stopAndWait(addr, 5*time.Second); err != nil {
			return err
		}
	}

	if err := replaceBinary(tmp, exePath); err != nil {
		if site == daemonHere {
			fmt.Fprintf(os.Stderr,
				"memstated upgrade: daemon at %s was stopped but not restarted — restart it manually\n",
				addr)
		}
		return err
	}

	switch site {
	case daemonElsewhere:
		fmt.Println(elsewhereNote(addr, prev, "upgrade"))
		return nil
	case daemonAbsent:
		fmt.Println("no running shared daemon found — child-mode daemons pick up the new binary next session")
		return nil
	}
	if err := startDetachedDaemon(exePath, addr, prev); err != nil {
		return fmt.Errorf("binary upgraded but restart failed: %v\n"+
			"start it manually: memstated --addr %s", err, addr)
	}
	fmt.Printf("daemon restarted at %s\n", addr)
	if next, err := fetchHealth(addr); err == nil {
		reportRestartDiff(os.Stdout, prev, next)
	}
	return nil
}

// daemonSite says where the shared daemon of an address runs, as this
// machine sees it.
type daemonSite int

const (
	// daemonAbsent: this machine can listen on the address, and no memstate
	// daemon answers there.
	daemonAbsent daemonSite = iota
	// daemonHere: a memstate daemon of this machine answers.
	daemonHere
	// daemonElsewhere: the daemon of the address does not run on this
	// machine. This machine cannot start it, so it must not stop it.
	daemonElsewhere
)

// locateDaemon reads /health at addr and says where the daemon of that
// address runs. The report is nil when no memstate daemon answers.
//
// `upgrade` and `restart` stop a daemon and start this binary in its place.
// That is correct only for a daemon of this machine. On a machine that is
// a client, MEMSTATE_ADDR names the daemon of another machine: the stop
// works there, the start cannot work here, and each client of that daemon
// loses its store until someone starts it again.
//
// A daemon is here when two facts hold. This machine can listen on the
// address (addrIsLocal). The PID in the report is a process of this
// machine: a forwarded port has a local address and a daemon on the far
// side. A daemon from an old release reports no PID, and then the address
// decides.
func locateDaemon(addr string) (*healthResponse, daemonSite) {
	h, err := fetchHealth(addr)
	if err != nil || h.Service != healthServiceName {
		h = nil
	}
	switch {
	case !addrIsLocal(addr):
		return h, daemonElsewhere
	case h == nil:
		return nil, daemonAbsent
	case h.Pid != 0 && !processAlive(h.Pid):
		return h, daemonElsewhere
	}
	return h, daemonHere
}

// elsewhereNote is the line that `memstated <verb>` prints for a daemon
// that it leaves alone. h is the /health report, nil when none came: the
// note then says nothing about the state of the daemon, because a daemon
// across a slow link can answer later than fetchHealth waits.
func elsewhereNote(addr string, h *healthResponse, verb string) string {
	version := ""
	if h != nil {
		version = " (it runs v" + oneLine(h.Version) + ")"
	}
	return fmt.Sprintf("the daemon at %s does not run on this machine%s. "+
		"This command did not stop it: run `memstated %s` on the machine that runs it.",
		addr, version, verb)
}

// addrIsLocal reports whether this machine can listen on addr.
func addrIsLocal(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return hostIsLocal(host, interfaceAddrs(), net.LookupHost)
}

// interfaceAddrs returns the address of each network interface of this
// machine.
func interfaceAddrs() []netip.Addr {
	addrs, _ := net.InterfaceAddrs()
	var out []netip.Addr
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if parsed, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, parsed.Unmap())
		}
	}
	return out
}

// hostIsLocal reports whether host, the host part of a listen address, is
// an address of this machine: empty (each interface), a loopback or
// unspecified address, or one of local, the addresses of the interfaces.
// A name is local when each address that lookup gives for it is local. A
// name that does not resolve is not local: the caller then leaves the
// daemon alone, which is the safe result. For an IP address this is the
// rule of isRemoteAddr in the proxy, which does not start a daemon for an
// address of another machine.
func hostIsLocal(host string, local []netip.Addr, lookup func(string) ([]string, error)) bool {
	if host == "" {
		return true
	}
	here := func(s string) bool {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return false
		}
		ip = ip.WithZone("").Unmap()
		return ip.IsLoopback() || ip.IsUnspecified() || slices.Contains(local, ip)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return here(host)
	}
	ips, err := lookup(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	return !slices.ContainsFunc(ips, func(s string) bool { return !here(s) })
}

// checksumAsset is the release asset that lists the SHA-256 of every other
// asset, one "<hex>  <name>" line each (`make release` writes it with
// scripts/sha256sums.mjs).
const checksumAsset = "SHA256SUMS"

// assetURL returns the download URL of the release asset called name.
func (r *ghRelease) assetURL(name string) (string, error) {
	var names []string
	for _, a := range r.Assets {
		if a.Name == name {
			return a.BrowserDownloadURL, nil
		}
		names = append(names, a.Name)
	}
	return "", fmt.Errorf("release %s has no asset %q (assets: %s)",
		r.TagName, name, strings.Join(names, ", "))
}

// downloadRelease downloads the asset called name to dest and checks it
// against the release's checksum list. When it returns an error, dest does
// not exist.
//
// The list comes from the same release as the binary. The check finds a
// download that is damaged, cut short or replaced on the way. It does not
// find a release that an attacker published: that needs a signature made
// with a key that is not on GitHub.
func downloadRelease(rel *ghRelease, name, dest string) error {
	url, err := rel.assetURL(name)
	if err != nil {
		return err
	}
	sumsURL, err := rel.assetURL(checksumAsset)
	if err != nil {
		return fmt.Errorf("%w; the download cannot be verified", err)
	}
	sums, err := fetchText(sumsURL, 1<<20)
	if err != nil {
		return err
	}
	if err := downloadTo(url, dest); err != nil {
		return err
	}
	if err := verifyDownload(dest, sums, name); err != nil {
		os.Remove(dest)
		return err
	}
	return nil
}

// fetchText returns the body of url as text. A body longer than limit bytes
// is an error.
func fetchText(url string, limit int64) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > limit {
		return "", fmt.Errorf("GET %s: longer than %d bytes", url, limit)
	}
	return string(b), nil
}

// checksumFor returns the SHA-256 that a checksum list gives for name, as
// lower-case hex. A line is "<64 hex digits>  <name>"; `sha256sum` writes
// "*" before the name of a file that it read in binary mode.
func checksumFor(sums, name string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		digest := strings.ToLower(fields[0])
		if b, err := hex.DecodeString(digest); err != nil || len(b) != sha256.Size {
			return "", fmt.Errorf("%s: the entry for %s is not a SHA-256", checksumAsset, name)
		}
		return digest, nil
	}
	return "", fmt.Errorf("%s has no entry for %s", checksumAsset, name)
}

// verifyDownload reports an error unless the SHA-256 of the file at path is
// the one that the checksum list gives for name.
func verifyDownload(path, sums, name string) error {
	want, err := checksumFor(sums, name)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s; the download is damaged or was replaced",
			name, got, want)
	}
	return nil
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

// restartPlan gives the flags that start the successor of a daemon on addr:
// --addr plus every flag the old daemon was started with (its /health
// `args`, minus any --addr). Nothing else is pinned: the new process reads
// the current environment and config.env itself, so an edited setting
// takes effect on restart while an explicit flag keeps winning, exactly as
// it did for the old daemon. A nil report gives just --addr.
func restartPlan(prev *healthResponse, addr string) []string {
	args := []string{"--addr", addr}
	if prev == nil {
		return args
	}
	skip := false
	for _, a := range prev.Args {
		switch {
		case skip:
			skip = false
		case a == "--addr" || a == "-addr":
			skip = true
		case strings.HasPrefix(a, "--addr=") || strings.HasPrefix(a, "-addr="):
		default:
			args = append(args, a)
		}
	}
	return args
}

// reportRestartDiff prints every embed or idle setting that changed between
// the old daemon's /health and the new one's, with the config.env key that
// pins it. Silent when nothing changed. A restart that reverts the model is
// how settings got "forgotten" before config.env existed; now it is loud.
func reportRestartDiff(w io.Writer, prev, next *healthResponse) {
	if prev == nil || next == nil {
		return
	}
	for _, c := range []struct{ name, key, was, now string }{
		{"embedding_url", "MEMSTATE_EMBEDDING_URL", prev.EmbeddingURL, next.EmbeddingURL},
		{"embed_model", "MEMSTATE_EMBED_MODEL", prev.EmbedModel, next.EmbedModel},
		{"embed_timeout", "MEMSTATE_EMBED_TIMEOUT", prev.EmbedTimeout, next.EmbedTimeout},
		{"semantic_threshold", "MEMSTATE_SEMANTIC_THRESHOLD", fmt.Sprint(prev.SemanticThreshold), fmt.Sprint(next.SemanticThreshold)},
		{"idle_timeout", "MEMSTATE_IDLE_TIMEOUT", prev.IdleTimeout, next.IdleTimeout},
	} {
		if !sameSetting(c.was, c.now) {
			fmt.Fprintf(w, "  %s changed: %q -> %q (set %s in config.env to pin it)\n", c.name, c.was, c.now, c.key)
		}
	}
}

// startDetachedDaemon launches the (new) binary as a shared daemon in its own
// session with the flags prev was started with (see restartPlan), stderr and
// stdout appended to daemonLogPath() — the same file the MCP proxy tees
// child-mode daemons into. The environment is inherited as is.
func startDetachedDaemon(exePath, addr string, prev *healthResponse) error {
	logPath := daemonLogPath()
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	logF, _ := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	cmd := exec.Command(exePath, restartPlan(prev, addr)...)
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
	return fmt.Errorf("daemon did not answer /health at %s within 5s (see %s)", addr, logPath)
}
