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
	args, env := restartPlan(nil, "127.0.0.1:1")
	if strings.Join(args, " ") != "--addr 127.0.0.1:1" || len(env) != 0 {
		t.Fatalf("nil report: %v %v", args, env)
	}
	prev := &healthResponse{
		EmbedModel: "qwen3-embedding:4b", SemanticThreshold: 0.45,
		OllamaURL: "http://127.0.0.1:11434", EmbedTimeout: "1m0s", IdleTimeout: "30m0s",
	}
	args, env = restartPlan(prev, "127.0.0.1:8765")
	want := "--addr 127.0.0.1:8765 --embed-model qwen3-embedding:4b --ollama-url http://127.0.0.1:11434 --embed-timeout 1m0s --idle-timeout 30m0s"
	if strings.Join(args, " ") != want {
		t.Fatalf("args: %q", strings.Join(args, " "))
	}
	if len(env) != 1 || env[0] != "MEMSTATE_SEMANTIC_THRESHOLD=0.45" {
		t.Fatalf("env: %v", env)
	}
}

// TestUpgradeRestartKeepsConfig runs the real binary the way `memstated
// upgrade` restarts it and checks that /health on the new daemon reports
// the config of the old one.
func TestUpgradeRestartKeepsConfig(t *testing.T) {
	bin := buildDaemon(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MEMSTATE_DB", filepath.Join(t.TempDir(), "up.db"))
	t.Setenv("MEMSTATE_NO_UPDATE_CHECK", "1")
	t.Setenv("MEMSTATE_SEMANTIC_THRESHOLD", "")
	addr := reserveAndRelease(t)
	prev := &healthResponse{
		Service: healthServiceName, Version: healthVersion,
		EmbedModel: "keep-me", SemanticThreshold: 0.37,
		OllamaURL: "http://127.0.0.1:9", EmbedTimeout: "7s", IdleTimeout: "20m0s",
	}
	if err := startDetachedDaemon(bin, addr, prev); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stopAndWait(addr, 5*time.Second) })
	h, err := fetchHealth(addr)
	if err != nil {
		t.Fatal(err)
	}
	if h.EmbedModel != "keep-me" || h.SemanticThreshold != 0.37 || h.OllamaURL != "http://127.0.0.1:9" ||
		h.EmbedTimeout != "7s" || h.IdleTimeout != "20m0s" {
		t.Fatalf("restarted daemon lost config: %+v", h)
	}
}
