package main

import "runtime/debug"

// buildID identifies the code of this binary beyond healthVersion: the
// short git revision that Go records at build time, with "+dirty" when
// the working tree had changes. It is empty for a binary built outside a
// git checkout, such as a go test binary. Two binaries with one version
// string but different code, a daemon still running the previous build
// after `make install` for example, differ here.
func buildID() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" && dirty {
		rev += "+dirty"
	}
	return rev
}
