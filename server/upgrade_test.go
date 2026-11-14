package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// sumsLine is one line of a checksum list, as scripts/sha256sums.mjs and
// `sha256sum` write it.
func sumsLine(content []byte, name string) string {
	return fmt.Sprintf("%x  %s\n", sha256.Sum256(content), name)
}

func TestVerifyDownload(t *testing.T) {
	const name = "memstated-linux-amd64"
	content := []byte("memstated binary")
	path := filepath.Join(t.TempDir(), "download")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	good := sumsLine(content, name)
	digest := strings.Fields(good)[0]
	cases := []struct {
		name string
		sums string
		ok   bool
	}{
		{"match", good, true},
		{"match among other entries", sumsLine([]byte("x"), "memstate-mcp.tar.gz") + good, true},
		{"binary mode marker", digest + " *" + name + "\n", true},
		{"upper case digest", strings.ToUpper(digest) + "  " + name + "\n", true},
		{"windows line ends", digest + "  " + name + "\r\n", true},
		{"digest of other content", sumsLine([]byte("other"), name), false},
		{"entry for another asset only", sumsLine(content, "memstated-darwin-arm64"), false},
		{"entry for a longer name only", sumsLine(content, name+".exe"), false},
		{"digest cut short", digest[:10] + "  " + name + "\n", false},
		{"digest is not hex", strings.Repeat("z", 64) + "  " + name + "\n", false},
		{"empty list", "", false},
	}
	for _, c := range cases {
		if err := verifyDownload(path, c.sums, name); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok = %v", c.name, err, c.ok)
		}
	}
}

// A file passes against the list made from its own bytes, and fails after a
// change to any one byte.
func TestVerifyDownloadProperties(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asset")
	rapid.Check(t, func(t *rapid.T) {
		content := rapid.SliceOfN(rapid.Byte(), 1, 2048).Draw(t, "content")
		sums := sumsLine(content, "asset")
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyDownload(path, sums, "asset"); err != nil {
			t.Fatalf("the file that the list describes was refused: %v", err)
		}
		changed := slices.Clone(content)
		changed[rapid.IntRange(0, len(content)-1).Draw(t, "index")] ^= byte(rapid.IntRange(1, 255).Draw(t, "flip"))
		if err := os.WriteFile(path, changed, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyDownload(path, sums, "asset"); err == nil {
			t.Fatal("a changed file was accepted")
		}
	})
}

// downloadRelease keeps a download only when the release's checksum list
// describes it.
func TestDownloadReleaseVerifiesTheChecksum(t *testing.T) {
	name := releaseAssetName()
	binary := []byte("#!/bin/sh\necho memstated\n")
	cases := []struct {
		name    string
		sums    string // "" means the release has no checksum list
		wantErr string // "" means the download is kept
	}{
		{"list matches", sumsLine(binary, name), ""},
		{"list describes other bytes", sumsLine([]byte("the real release"), name), "checksum mismatch"},
		{"list has no entry for the asset", sumsLine(binary, "memstate-mcp.tar.gz"), "no entry"},
		{"release has no list", "", "cannot be verified"},
	}
	for _, c := range cases {
		mux := http.NewServeMux()
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(binary) })
		mux.HandleFunc("/"+checksumAsset, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(c.sums)) })
		ts := httptest.NewServer(mux)

		assets := []map[string]string{{"name": name, "browser_download_url": ts.URL + "/" + name}}
		if c.sums != "" {
			assets = append(assets, map[string]string{"name": checksumAsset, "browser_download_url": ts.URL + "/" + checksumAsset})
		}
		blob, _ := json.Marshal(map[string]any{"tag_name": "v9.9.9", "assets": assets})
		var rel ghRelease
		if err := json.Unmarshal(blob, &rel); err != nil {
			t.Fatal(err)
		}

		dest := filepath.Join(t.TempDir(), "memstated.download")
		err := downloadRelease(&rel, name, dest)
		ts.Close()
		_, statErr := os.Stat(dest)
		switch {
		case c.wantErr == "" && (err != nil || statErr != nil):
			t.Errorf("%s: err = %v, file: %v; want a kept download", c.name, err, statErr)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: err = %v, want one that says %q", c.name, err, c.wantErr)
		case c.wantErr != "" && statErr == nil:
			t.Errorf("%s: the refused download was left at %s", c.name, dest)
		}
	}
}

func TestVersionNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.5.1", "0.5.0", true},
		{"v0.5.1", "0.5.0", true},
		{"0.5.0", "0.5.0", false},
		{"0.5.0", "0.5.1", false},
		{"0.10.0", "0.9.9", true},
		{"1.0.0", "0.99.99", true},
		{"0.6.0-rc1", "0.5.1", true},
		{"garbage", "0.5.1", false},
		{"0.5", "0.5.0", false},
	}
	for _, c := range cases {
		if got := versionNewer(c.a, c.b); got != c.want {
			t.Errorf("versionNewer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestReleaseAssetName(t *testing.T) {
	// Sanity: matches the Makefile naming scheme memstated-<os>-<arch>.
	name := releaseAssetName()
	if len(name) == 0 || name[:10] != "memstated-" {
		t.Errorf("unexpected asset name %q", name)
	}
}

func TestRestartPlan(t *testing.T) {
	if got := strings.Join(restartPlan(nil, "127.0.0.1:1"), " "); got != "--addr 127.0.0.1:1" {
		t.Fatalf("nil report: %q", got)
	}
	// The old daemon's flags come back verbatim, on the new addr, and
	// nothing from its reported values is pinned: those come from env and
	// config.env at the next start.
	prev := &healthResponse{
		Args:       []string{"--addr", "127.0.0.1:1", "--embed-model", "qwen3-embedding:4b", "-addr=x", "--idle-timeout", "30m"},
		EmbedModel: "qwen3-embedding:4b", SemanticThreshold: 0.45, EmbeddingURL: "http://127.0.0.1:11434",
	}
	want := "--addr 127.0.0.1:8765 --embed-model qwen3-embedding:4b --idle-timeout 30m"
	if got := strings.Join(restartPlan(prev, "127.0.0.1:8765"), " "); got != want {
		t.Fatalf("args: %q", got)
	}
}

func TestReportRestartDiff(t *testing.T) {
	prev := &healthResponse{EmbedModel: "a", EmbeddingURL: "u", EmbedTimeout: "1m0s", SemanticThreshold: 0.5}
	next := &healthResponse{EmbedModel: "b", EmbeddingURL: "u", EmbedTimeout: "60s", SemanticThreshold: 0.5}
	var out strings.Builder
	reportRestartDiff(&out, prev, next)
	if got := out.String(); !strings.Contains(got, `embed_model changed: "a" -> "b"`) ||
		!strings.Contains(got, "MEMSTATE_EMBED_MODEL") || strings.Contains(got, "embed_timeout") {
		t.Fatalf("diff: %q", got)
	}
}

// TestUpgradeRestartKeepsConfig runs the real binary the way `memstated
// upgrade` and `memstated restart` start it: the old daemon's flags are
// replayed, and env (here standing in for config.env, which main exports
// into env) supplies the rest.
func TestUpgradeRestartKeepsConfig(t *testing.T) {
	bin := buildDaemon(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MEMSTATE_CONFIG", "off")
	t.Setenv("MEMSTATE_DB", filepath.Join(t.TempDir(), "up.db"))
	t.Setenv("MEMSTATE_NO_UPDATE_CHECK", "1")
	t.Setenv("MEMSTATE_SEMANTIC_THRESHOLD", "0.37")
	t.Setenv("MEMSTATE_EMBEDDING_URL", "http://127.0.0.1:9")
	addr := reserveAndRelease(t)
	prev := &healthResponse{
		Service: healthServiceName, Version: healthVersion,
		Args: []string{"--addr", "127.0.0.1:1", "--embed-model", "keep-me", "--embed-timeout", "7s", "--idle-timeout", "20m"},
	}
	if err := startDetachedDaemon(bin, addr, prev); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stopAndWait(addr, 5*time.Second) })
	h, err := fetchHealth(addr)
	if err != nil {
		t.Fatal(err)
	}
	if h.EmbedModel != "keep-me" || h.SemanticThreshold != 0.37 || h.EmbeddingURL != "http://127.0.0.1:9" ||
		h.EmbedTimeout != "7s" || h.IdleTimeout != "20m0s" || h.Pid == 0 ||
		strings.Join(h.Args, " ") != "--addr "+addr+" --embed-model keep-me --embed-timeout 7s --idle-timeout 20m" {
		t.Fatalf("restarted daemon lost config: %+v", h)
	}
}

// daemonOfAnotherMachine answers /health as a memstate daemon does, with a
// PID that is the PID of no process on this machine, and counts the
// shutdown requests. It stands for a daemon that this machine reaches
// through a forwarded port: the address is local, the process is not.
func daemonOfAnotherMachine(t *testing.T) (addr string, stops *atomic.Int32) {
	t.Helper()
	// The home directory holds the daemon log. Keep the test out of the
	// real one.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MEMSTATE_DB", filepath.Join(t.TempDir(), "memstate.db"))
	t.Setenv("MEMSTATE_CONFIG", "off")

	// A process that ended: its PID is free.
	gone := exec.Command(os.Args[0], "-test.run=^$")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	pid := gone.Process.Pid

	stops = new(atomic.Int32)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthResponse{
			Service: healthServiceName, Version: "0.9.1", Pid: pid,
			Args: []string{"--addr", "0.0.0.0:8765"},
		})
	})
	mux.HandleFunc("POST /admin/shutdown", func(w http.ResponseWriter, r *http.Request) {
		stops.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return strings.TrimPrefix(ts.URL, "http://"), stops
}

// The bug: on a machine that is a client of a daemon on another machine,
// MEMSTATE_ADDR names that daemon. `memstated upgrade` stopped it, and
// could not start it again, because only the other machine can.
func TestUpgradeLeavesADaemonOfAnotherMachine(t *testing.T) {
	addr, stops := daemonOfAnotherMachine(t)
	dir := t.TempDir()
	exe, download := filepath.Join(dir, "memstated"), filepath.Join(dir, "memstated.download")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(download, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := installDownload(download, exe, addr); err != nil {
		t.Errorf("installDownload: %v", err)
	}
	if n := stops.Load(); n != 0 {
		t.Errorf("the daemon of another machine got %d shutdown requests, want 0", n)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new" {
		t.Errorf("binary holds %q, want the download", got)
	}
}

// `memstated restart` has the same rule. It cannot start the daemon again,
// so it must not stop it.
func TestRestartLeavesADaemonOfAnotherMachine(t *testing.T) {
	addr, stops := daemonOfAnotherMachine(t)
	if code := cmdRestart([]string{"--addr", addr}); code == 0 {
		t.Error("restart of a daemon of another machine reported success")
	}
	if n := stops.Load(); n != 0 {
		t.Errorf("the daemon of another machine got %d shutdown requests, want 0", n)
	}
}

func TestHostIsLocal(t *testing.T) {
	// The machine of the bug: 192.168.1.196, a client of the daemon at
	// 192.168.1.200.
	local := []netip.Addr{netip.MustParseAddr("192.168.1.196"), netip.MustParseAddr("fe80::1c2")}
	names := map[string][]string{
		"localhost":   {"127.0.0.1", "::1"},
		"this-box":    {"192.168.1.196"},
		"desktop":     {"192.168.1.200"},
		"two-homes":   {"192.168.1.196", "192.168.1.200"},
		"no-address":  {},
		"not-address": {"desktop"},
	}
	lookup := func(name string) ([]string, error) {
		ips, ok := names[name]
		if !ok {
			return nil, fmt.Errorf("lookup %s: no such host", name)
		}
		return ips, nil
	}
	cases := []struct {
		host string
		want bool
	}{
		{"192.168.1.200", false}, // the bug input
		{"100.64.0.10", false},
		{"", true},
		{"127.0.0.1", true},
		{"127.8.9.10", true},
		{"::1", true},
		{"0.0.0.0", true},
		{"::", true},
		{"192.168.1.196", true},
		{"::ffff:192.168.1.196", true},
		{"::ffff:192.168.1.200", false},
		{"fe80::1c2%eth0", true},
		{"fe80::1c3%eth0", false},
		{"localhost", true},
		{"this-box", true},
		{"desktop", false},
		{"two-homes", false},
		{"no-address", false},
		{"not-address", false},
		{"unknown.example", false},
	}
	for _, c := range cases {
		if got := hostIsLocal(c.host, local, lookup); got != c.want {
			t.Errorf("hostIsLocal(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// An IP address is local exactly when it is a loopback address, the
// unspecified address, or an address of an interface. The form of the
// address (IPv4, IPv4 in IPv6, with a zone) does not change the answer.
func TestHostIsLocalProperty(t *testing.T) {
	noLookup := func(name string) ([]string, error) {
		return nil, fmt.Errorf("lookup of %q: an IP address needs none", name)
	}
	ipv4 := rapid.Custom(func(t *rapid.T) netip.Addr {
		// A small range, so that an address is often one of the interfaces.
		return netip.AddrFrom4([4]byte{
			rapid.SampledFrom([]byte{0, 10, 127, 192}).Draw(t, "a"), 0, 0,
			byte(rapid.IntRange(0, 3).Draw(t, "d")),
		})
	})
	ipv6 := rapid.Custom(func(t *rapid.T) netip.Addr {
		var b [16]byte
		b[0] = rapid.SampledFrom([]byte{0, 0xfd, 0xfe}).Draw(t, "first")
		b[15] = byte(rapid.IntRange(0, 3).Draw(t, "last"))
		return netip.AddrFrom16(b)
	})
	addr := rapid.OneOf(ipv4, ipv6)
	rapid.Check(t, func(t *rapid.T) {
		local := rapid.SliceOfN(addr, 0, 4).Draw(t, "interfaces")
		ip := addr.Draw(t, "ip")
		want := ip.IsLoopback() || ip.IsUnspecified() || slices.Contains(local, ip)

		forms := []string{ip.String()}
		if ip.Is4() {
			forms = append(forms, netip.AddrFrom16(ip.As16()).String())
		} else {
			forms = append(forms, ip.WithZone("eth0").String())
		}
		for _, host := range forms {
			if got := hostIsLocal(host, local, noLookup); got != want {
				t.Fatalf("hostIsLocal(%q) with interfaces %v = %v, want %v", host, local, got, want)
			}
		}
	})
}

func TestLocateDaemon(t *testing.T) {
	// A daemon of this machine: the real router reports the PID of this
	// process.
	ts := httptest.NewServer(newRouter(newTestStore(t), nil, nil))
	t.Cleanup(ts.Close)
	here := strings.TrimPrefix(ts.URL, "http://")
	if h, site := locateDaemon(here); site != daemonHere || h == nil || h.Pid != os.Getpid() {
		t.Errorf("a daemon of this machine: site %d, report %+v", site, h)
	}

	// The address is local and nothing listens there.
	if h, site := locateDaemon(reserveAndRelease(t)); site != daemonAbsent || h != nil {
		t.Errorf("a free local port: site %d, report %+v", site, h)
	}

	// The address is local and the process is not: a forwarded port.
	forwarded, _ := daemonOfAnotherMachine(t)
	if h, site := locateDaemon(forwarded); site != daemonElsewhere || h == nil {
		t.Errorf("a daemon behind a forwarded port: site %d, report %+v", site, h)
	}

	// The address is not one of this machine. 192.0.2.0/24 is the
	// documentation range (RFC 5737): no interface has it and nothing
	// answers there.
	if h, site := locateDaemon("192.0.2.1:8765"); site != daemonElsewhere || h != nil {
		t.Errorf("an address of another machine: site %d, report %+v", site, h)
	}

	// An old daemon reports no PID. Then the address decides.
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthResponse{Service: healthServiceName, Version: "0.5.0"})
	}))
	t.Cleanup(old.Close)
	if _, site := locateDaemon(strings.TrimPrefix(old.URL, "http://")); site != daemonHere {
		t.Errorf("a local daemon that reports no PID: site %d, want daemonHere", site)
	}
}

// A restart with the address of another machine must not start a daemon
// here either: the daemon cannot listen on that address.
func TestRestartRefusesAnAddressOfAnotherMachine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MEMSTATE_DB", filepath.Join(t.TempDir(), "memstate.db"))
	t.Setenv("MEMSTATE_CONFIG", "off")
	start := time.Now()
	if code := cmdRestart([]string{"--addr", "192.0.2.1:8765"}); code == 0 {
		t.Error("restart at an address of another machine reported success")
	}
	// startDetachedDaemon waits 5 seconds for /health. A refusal does not.
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("restart took %v: it tried to start a daemon", took)
	}
}

// Each address of an interface of this machine is local, in the form that
// a user writes in --addr or MEMSTATE_ADDR.
func TestAddrIsLocalOnThisMachine(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) == 0 {
		t.Skip("this machine reports no interface address")
	}
	for _, a := range addrs {
		host, _, _ := strings.Cut(a.String(), "/")
		if addr := net.JoinHostPort(host, "8765"); !addrIsLocal(addr) {
			t.Errorf("addrIsLocal(%q) = false for an address of this machine", addr)
		}
	}
	for _, addr := range []string{"192.0.2.1:8765", "[2001:db8::1]:8765", "192.0.2.1", ""} {
		if addrIsLocal(addr) {
			t.Errorf("addrIsLocal(%q) = true", addr)
		}
	}
	for _, addr := range []string{":8765", "127.0.0.1:8765", "localhost:8765", "[::1]:8765", "0.0.0.0:8765"} {
		if !addrIsLocal(addr) {
			t.Errorf("addrIsLocal(%q) = false", addr)
		}
	}
}
