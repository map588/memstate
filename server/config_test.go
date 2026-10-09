package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigFileLoadPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.env")
	text := "# comment\nexport MEMSTATE_EMBED_MODEL='qwen3-embedding-4b'\nMEMSTATE_EMBEDDING_URL = \"http://127.0.0.1:8081/v1\"\njunk line\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMSTATE_CONFIG", path)
	t.Setenv("MEMSTATE_EMBEDDING_URL", "http://env:1") // env wins over the file
	t.Setenv("MEMSTATE_EMBED_MODEL", "")
	configFileLoaded, configFileValues, configApplied = "", map[string]string{}, map[string]bool{}
	t.Cleanup(func() {
		configFileLoaded, configFileValues, configApplied = "", map[string]string{}, map[string]bool{}
	})

	if n := loadConfigFile(); n != 1 {
		t.Fatalf("applied %d settings, want 1 (the model; the URL is set in env)", n)
	}
	if v, src := configSource("MEMSTATE_EMBED_MODEL", "x"); v != "qwen3-embedding-4b" || src != "config.env" {
		t.Fatalf("model: %q %q", v, src)
	}
	if v, src := configSource("MEMSTATE_EMBEDDING_URL", "x"); v != "http://env:1" || !strings.HasPrefix(src, "env (config.env says") {
		t.Fatalf("url: %q %q", v, src)
	}
	if v, src := configSource("MEMSTATE_EMBED_TIMEOUT", "60s"); v != "60s" || src != "default" {
		t.Fatalf("timeout: %q %q", v, src)
	}
	var warn bytes.Buffer
	if _, err := parseConfigFile(path, &warn); err != nil || !strings.Contains(warn.String(), "junk line") {
		t.Fatalf("bad line must warn and be skipped: err=%v warn=%q", err, warn.String())
	}
}

func TestWriteConfigValueKeepsOtherLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.env")
	if err := writeConfigValue(path, "MEMSTATE_EMBED_MODEL", "a"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# keep me\nMEMSTATE_EMBED_MODEL=a\nMEMSTATE_ADDR=127.0.0.1:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigValue(path, "MEMSTATE_EMBED_MODEL", "b"); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigValue(path, "MEMSTATE_SEMANTIC_THRESHOLD", "0.6"); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigValue(path, "MEMSTATE_ADDR", ""); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	want := "# keep me\nMEMSTATE_EMBED_MODEL=b\nMEMSTATE_SEMANTIC_THRESHOLD=0.6\n"
	if string(b) != want {
		t.Fatalf("got %q want %q", b, want)
	}
}

func TestSameSetting(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"1m0s", "60s", true}, {"0.6", "0.60", true}, {"", "", true},
		{"x", "y", false}, {"7s", "8s", false}, {"", "30m", false},
	} {
		if got := sameSetting(c.a, c.b); got != c.want {
			t.Errorf("sameSetting(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}
