package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"pgregory.net/rapid"
)

// guardRequest sends one request with exact headers and an optional Host
// override, and returns the status code and the body.
func guardRequest(t *testing.T, ts *httptest.Server, method, path, host string, headers map[string]string, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const guardStoreBody = `{"project_id":"guard","keypath":"notes.a","content":"c"}`

// A web page can send a POST with a text/plain body and no preflight (a
// "simple" cross-site request), and a page that rebinds its DNS name to the
// daemon address sends its own name as the Host. The daemon must refuse both,
// and must keep serving its real clients, which send JSON and no Origin.
func TestGuardRefusesRequestsFromWebPages(t *testing.T) {
	ts := newTestServer(t)
	json := map[string]string{"Content-Type": "application/json"}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{"Content-Type": "application/json"}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	cases := []struct {
		name    string
		method  string
		path    string
		host    string
		headers map[string]string
		body    string
		want    int
	}{
		// Real clients.
		{"json write", "POST", "/api/v1/memories/store", "", json, guardStoreBody, 200},
		{"json write with charset", "POST", "/api/v1/memories/store", "", map[string]string{"Content-Type": "application/json; charset=utf-8"}, guardStoreBody, 200},
		{"read by loopback name", "GET", "/api/v1/projects", "localhost:8765", nil, "", 200},
		{"read by IPv4 literal", "GET", "/api/v1/projects", "192.168.1.20:8765", nil, "", 200},
		{"read by IPv6 literal", "GET", "/api/v1/projects", "[::1]:8765", nil, "", 200},
		{"read by single-label name", "GET", "/api/v1/projects", "memstate-host:8765", nil, "", 200},
		{"address bar navigation", "GET", "/health", "", map[string]string{"Sec-Fetch-Site": "none"}, "", 200},
		// Cross-site requests from a web page.
		{"simple POST, text/plain", "POST", "/api/v1/memories/store", "", map[string]string{"Content-Type": "text/plain;charset=UTF-8"}, guardStoreBody, 415},
		{"simple POST, form", "POST", "/api/v1/memories/delete", "", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, guardStoreBody, 415},
		{"POST without a content type", "POST", "/api/v1/memories/store", "", nil, guardStoreBody, 415},
		{"foreign origin", "POST", "/api/v1/memories/store", "", with(map[string]string{"Origin": "https://attacker.example"}), guardStoreBody, 403},
		{"loopback origin", "POST", "/api/v1/memories/store", "", with(map[string]string{"Origin": "http://localhost:3000"}), guardStoreBody, 403},
		{"null origin", "GET", "/api/v1/projects", "", map[string]string{"Origin": "null"}, "", 403},
		{"fetch metadata, cross-site", "GET", "/api/v1/projects", "", map[string]string{"Sec-Fetch-Site": "cross-site"}, "", 403},
		{"fetch metadata, same-site", "GET", "/api/v1/projects", "", map[string]string{"Sec-Fetch-Site": "same-site"}, "", 403},
		// DNS rebinding: the page's own name arrives as the Host.
		{"rebound host, read", "GET", "/api/v1/projects", "attacker.example:8765", nil, "", 403},
		{"rebound host, health", "GET", "/health", "attacker.example", nil, "", 403},
		{"rebound host, write", "POST", "/api/v1/memories/store", "attacker.example:8765", json, guardStoreBody, 403},
		{"name that only looks local", "GET", "/api/v1/projects", "localhost.attacker.example", nil, "", 403},
		{"ip-like name", "GET", "/api/v1/projects", "127.0.0.1.nip.io:8765", nil, "", 403},
	}
	for _, c := range cases {
		code, body := guardRequest(t, ts, c.method, c.path, c.host, c.headers, c.body)
		if code != c.want {
			t.Errorf("%s: got %d, want %d (%s)", c.name, code, c.want, strings.TrimSpace(body))
		}
	}
}

// A refused write must not reach the store.
func TestGuardRefusedWriteStoresNothing(t *testing.T) {
	ts := newTestServer(t)
	body := `{"project_id":"forged","keypath":"notes.a","content":"c"}`
	guardRequest(t, ts, "POST", "/api/v1/memories/store", "", map[string]string{"Content-Type": "text/plain"}, body)
	guardRequest(t, ts, "POST", "/api/v1/memories/store", "", map[string]string{"Content-Type": "application/json", "Origin": "https://attacker.example"}, body)
	_, projects := guardRequest(t, ts, "GET", "/api/v1/projects", "", nil, "")
	if strings.Contains(projects, "forged") {
		t.Fatalf("a refused request created a project: %s", projects)
	}
}

// POST /admin/shutdown has no body, so the content type rule does not cover
// it. The Origin rule must.
func TestGuardShutdownRefusesWebPage(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(newRouter(newTestStore(t), func() { calls.Add(1) }, nil))
	t.Cleanup(ts.Close)

	code, _ := guardRequest(t, ts, "POST", "/admin/shutdown", "", map[string]string{"Origin": "https://attacker.example"}, "")
	if code != 403 || calls.Load() != 0 {
		t.Fatalf("shutdown from a web page: code %d, shutdown calls %d", code, calls.Load())
	}
	code, _ = guardRequest(t, ts, "POST", "/admin/shutdown", "", nil, "")
	if code != 202 {
		t.Fatalf("shutdown from a local client: code %d", code)
	}
}

// One request must not make the daemon hold an unbounded body in memory.
func TestGuardLimitsRequestBody(t *testing.T) {
	ts := newTestServer(t)
	json := map[string]string{"Content-Type": "application/json"}
	big := `{"project_id":"big","keypath":"k","content":"` + strings.Repeat("a", 5<<20) + `"}`
	if code, body := guardRequest(t, ts, "POST", "/api/v1/memories/store", "", json, big); code != 413 {
		t.Fatalf("5 MiB body: got %d, want 413 (%.80s)", code, body)
	}
	ok := `{"project_id":"big","keypath":"k","content":"` + strings.Repeat("a", 1<<20) + `"}`
	if code, body := guardRequest(t, ts, "POST", "/api/v1/memories/store", "", json, ok); code != 200 {
		t.Fatalf("1 MiB body: got %d, want 200 (%.80s)", code, body)
	}
}

// A daemon started on a DNS name answers to that name and to no other.
func TestGuardAcceptsTheBindHostName(t *testing.T) {
	daemonBindHost = "memstate.example.net"
	t.Cleanup(func() { daemonBindHost = "" })
	ts := newTestServer(t)
	for host, want := range map[string]int{
		"memstate.example.net:8765": 200,
		"MEMSTATE.example.net":      200,
		"other.example.net:8765":    403,
	} {
		if code, _ := guardRequest(t, ts, "GET", "/api/v1/projects", host, nil, ""); code != want {
			t.Errorf("Host %q: got %d, want %d", host, code, want)
		}
	}
}

func TestBindHostName(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1:8765":            "",
		"[::1]:8765":                "",
		":8765":                     "",
		"0.0.0.0:0":                 "",
		"garbage":                   "",
		"localhost:8765":            "localhost",
		"memstate.example.net:8765": "memstate.example.net",
	} {
		if got := bindHostName(addr); got != want {
			t.Errorf("bindHostName(%q) = %q, want %q", addr, got, want)
		}
	}
}

// hostAllowed accepts every IP address literal with or without a port, and
// accepts a name with a dot only when it is the bind host.
func TestHostAllowedProperties(t *testing.T) {
	label := rapid.StringMatching(`[a-z][a-z0-9-]{0,10}`)
	rapid.Check(t, func(t *rapid.T) {
		suffix := ""
		if rapid.Bool().Draw(t, "with_port") {
			suffix = ":" + strconv.Itoa(rapid.IntRange(1, 65535).Draw(t, "port"))
		}

		var b4 [4]byte
		copy(b4[:], rapid.SliceOfN(rapid.Byte(), 4, 4).Draw(t, "ipv4"))
		if host := netip.AddrFrom4(b4).String() + suffix; !hostAllowed(host, "") {
			t.Fatalf("IPv4 literal %q refused", host)
		}
		var b16 [16]byte
		copy(b16[:], rapid.SliceOfN(rapid.Byte(), 16, 16).Draw(t, "ipv6"))
		if host := "[" + netip.AddrFrom16(b16).String() + "]" + suffix; !hostAllowed(host, "") {
			t.Fatalf("IPv6 literal %q refused", host)
		}

		// Every label starts with a letter, so the name is never an IP address.
		name := strings.Join(rapid.SliceOfN(label, 2, 4).Draw(t, "labels"), ".")
		bind := rapid.SampledFrom([]string{"", "memstate.example.net", name, strings.ToUpper(name)}).Draw(t, "bind")
		if got, want := hostAllowed(name+suffix, bind), strings.EqualFold(name, bind); got != want {
			t.Fatalf("hostAllowed(%q, %q) = %v, want %v", name+suffix, bind, got, want)
		}
	})
}
