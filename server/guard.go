package main

import (
	"errors"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// The daemon has no login. Its clients are local programs: the MCP proxy,
// the Python scripts, the recall hook and the CLI. A web browser on the same
// machine can also reach the port, and a web page can make the browser send
// requests there. requestGuard refuses the requests that only a browser
// sends, so a web page cannot read, write or stop the daemon.
//
// The guard does not identify the client. A local program of another user,
// or a machine that reaches a daemon on a network address, still has full
// access.

// maxRequestBytes is the largest request body that the daemon reads. A
// memory is text of a few kilobytes. Without a limit, one request can fill
// the memory of the daemon and the disk.
const maxRequestBytes = 4 << 20

// daemonBindHost is the DNS name in the listen address, when the user
// started the daemon on a name and not on an IP address. Clients then send
// this name as the Host, so the guard accepts it. main sets it before the
// router starts. It is empty for an IP address.
var daemonBindHost string

// requestGuard wraps the router with the checks below, in this order.
//
//  1. Host. A page that points its own DNS name at the daemon address (DNS
//     rebinding) is same-origin for the browser, so it sends no Origin. The
//     Host header still carries the name of the page. See hostAllowed.
//  2. Origin and Sec-Fetch-Site. A browser adds them to the requests that a
//     page makes. No client of the daemon sends them.
//  3. Content type. A page can send a POST with a text/plain or a form body
//     to another site without a preflight request. A JSON content type
//     needs a preflight, and the daemon does not answer one.
//  4. Body size. See maxRequestBytes.
func requestGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host, daemonBindHost) {
			writeErr(w, http.StatusForbidden,
				"host not allowed: use an IP address, localhost, a name without a dot, or the name in --addr")
			return
		}
		if _, fromPage := r.Header["Origin"]; fromPage || !fetchSiteAllowed(r.Header.Get("Sec-Fetch-Site")) {
			writeErr(w, http.StatusForbidden, "requests from a web page are not allowed")
			return
		}
		if r.ContentLength != 0 {
			if !isJSONContentType(r.Header.Get("Content-Type")) {
				writeErr(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
			if r.ContentLength > maxRequestBytes {
				writeErr(w, http.StatusRequestEntityTooLarge, bodyTooLarge)
				return
			}
			// A chunked body has no declared length. The reader stops it.
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		}
		next.ServeHTTP(w, r)
	})
}

const bodyTooLarge = "request body is larger than 4 MiB"

// hostAllowed reports whether a Host header value names the daemon in a way
// that a DNS rebinding page cannot use. Such a page needs a DNS name that
// its owner controls. An IP address, localhost and a name without a dot
// (a hosts-file or a tailnet name) are not such names. A name with a dot is
// accepted only when it is bindHost, the name that the user gave in --addr.
// A request without a Host header is accepted.
func hostAllowed(hostport, bindHost string) bool {
	if hostport == "" {
		return true
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if !strings.Contains(host, ".") {
		return isHostLabel(host)
	}
	return bindHost != "" && strings.EqualFold(host, bindHost)
}

// isHostLabel reports whether s is one host name label: letters, digits,
// hyphens and underscores.
func isHostLabel(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// fetchSiteAllowed reports whether a Sec-Fetch-Site value can come from a
// client of the daemon. Programs do not send the header. A browser sends
// "none" when the user types the address. Every other value, "same-origin"
// included, means that a page made the request: the daemon serves no pages,
// so a same-origin page is a rebound one.
func fetchSiteAllowed(value string) bool {
	return value == "" || value == "none"
}

// isJSONContentType reports whether a Content-Type value is application/json,
// with or without parameters.
func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}

// bindHostName returns the DNS name in a listen address, or "" when the
// host part is empty or an IP address.
func bindHostName(listenAddr string) string {
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil || host == "" {
		return ""
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return ""
	}
	return host
}

// decodeStatus maps a request decoding error to an HTTP status: 413 when
// the body passed maxRequestBytes, 400 for a body that is not the expected
// JSON.
func decodeStatus(err error) int {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}
