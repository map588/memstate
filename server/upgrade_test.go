package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
