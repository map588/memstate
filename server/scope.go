package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// sharedProject is the one reserved project. It holds facts that are true
// in every repository: the user's preferences and profile, each host's
// environment and tool configuration, and the practices and lessons the
// agent carries from one repository to the next. It never collides with a
// repository slug because slugProject trims edge underscores.
const sharedProject = "_shared"

// legacySharedProject is the name sharedProject had before 2026-10-17.
// migrateSharedProject renames the rows on open and Merge maps an export
// that still uses it.
const legacySharedProject = "_user"

// ErrReservedWrite marks a write that the reserved-project gate refused.
// Handlers map it to HTTP 400.
var ErrReservedWrite = errors.New("reserved write")

// sharedAreas are the top-level areas allowed in sharedProject for facts
// that hold on every machine: preferences and profile describe the user,
// practices and lessons describe how the agent works across repositories.
var sharedAreas = []string{"preferences", "profile", "practices", "lessons"}

// hostAreas are the areas allowed under host.<host_slug> for facts that hold
// only on one machine.
var hostAreas = []string{"env", "tools"}

const allowedSharedShapes = "preferences.<topic>, profile.<topic>, " +
	"practices.<topic>, lessons.<topic>, " +
	"host.<host_slug>.env.<topic>, host.<host_slug>.tools.<topic>"

// checkReservedWrite is the line between project memory and shared memory.
// Any project id that starts with "_" is reserved; only sharedProject
// exists, and a keypath there must match one of the allowed shapes. A short
// allowlist is the wall against session summaries, todos and decisions
// landing in a shared store. Normal projects pass untouched.
func checkReservedWrite(projectID, keypath string) error {
	if !strings.HasPrefix(projectID, "_") {
		return nil
	}
	if projectID != sharedProject {
		return fmt.Errorf("%w: project id prefix \"_\" is reserved; the only reserved project is %q",
			ErrReservedWrite, sharedProject)
	}
	seg := strings.Split(keypath, ".")
	for _, s := range seg {
		if s == "" {
			return reservedKeypathErr(keypath)
		}
	}
	switch {
	case len(seg) >= 2 && slices.Contains(sharedAreas, seg[0]):
		return nil
	case len(seg) >= 4 && seg[0] == "host" && slices.Contains(hostAreas, seg[2]):
		return nil
	}
	return reservedKeypathErr(keypath)
}

func reservedKeypathErr(keypath string) error {
	return fmt.Errorf("%w: keypath %q is not allowed in %s; allowed shapes: %s",
		ErrReservedWrite, keypath, sharedProject, allowedSharedShapes)
}

// isOtherHost reports whether a shared-scope keypath describes another
// machine: anything under host.<slug> where slug is not host.
func isOtherHost(keypath, host string) bool {
	seg := strings.SplitN(keypath, ".", 3)
	return len(seg) >= 2 && seg[0] == "host" && seg[1] != host
}

// hostSlug names this machine inside sharedProject: the first label of the
// hostname, slugged like a project id. The TS proxy and the Python skill
// apply the same rule, so all three agree on the segment.
func hostSlug() string {
	h, _ := os.Hostname()
	return slugHost(h)
}

func slugHost(hostname string) string {
	label, _, _ := strings.Cut(hostname, ".")
	return slugProject(label)
}

// legacyProject maps the name the shared project had before 2026-10-17 to
// sharedProject on every request, so a proxy from an older release keeps
// reading and writing the shared scope against this daemon. The mapping
// exists because `memstated upgrade` replaces only the daemon: the proxy
// and the skill scripts of that install still send legacySharedProject
// until the install scripts run again.
func legacyProject(id string) string {
	if id == legacySharedProject {
		return sharedProject
	}
	return id
}
