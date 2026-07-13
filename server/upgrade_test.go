package main

import "testing"

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
