package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
		Args:    []string{"--addr", "127.0.0.1:1", "--embed-model", "keep-me", "--embed-timeout", "7s", "--idle-timeout", "20m"},
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
