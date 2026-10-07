package main

import "testing"

func TestResolveListenAddr(t *testing.T) {
	t.Setenv("MEMSTATE_ADDR", "127.0.0.1:8765")
	cases := []struct {
		flag     string
		ownerPID int
		want     string
		explicit bool
	}{
		{"", 0, "127.0.0.1:8765", true},         // shared daemon: env names the port
		{"127.0.0.1:1", 0, "127.0.0.1:1", true}, // flag beats env
		{"", 42, "127.0.0.1:0", false},          // child daemon: env ignored, private
		{"127.0.0.1:1", 42, "127.0.0.1:1", true},
	}
	for _, c := range cases {
		addr, explicit := resolveListenAddr(c.flag, c.ownerPID)
		if addr != c.want || explicit != c.explicit {
			t.Errorf("flag=%q owner=%d: got (%q, %v), want (%q, %v)", c.flag, c.ownerPID, addr, explicit, c.want, c.explicit)
		}
	}
	t.Setenv("MEMSTATE_ADDR", "")
	if addr, explicit := resolveListenAddr("", 0); addr != "127.0.0.1:0" || explicit {
		t.Fatalf("no flag, no env: got (%q, %v)", addr, explicit)
	}
}
