// Command memstated is the HTTP backend for versioned agent memory.
//
// It speaks REST — NOT MCP. The MCP surface lives in the TypeScript proxy
// under ../client/, which spawns this binary as a child and forwards MCP
// tool calls here over HTTP loopback.
//
// Lifetime model:
//   - Default ("child mode"): listen on :0 (OS-picked port), print a banner
//     "MEMSTATE_READY addr=127.0.0.1:<port>" on stderr, watch --owner-pid
//     via kill(pid, 0) every 2s and shut down if it vanishes. The parent
//     reads the banner to learn our address, and kills us on its way out.
//   - "Shared daemon" mode (explicit --addr or MEMSTATE_ADDR): bind to the
//     named address. If EADDRINUSE and /health says it's us, exit 0 quietly.
//     If EADDRINUSE and the occupant is alien, exit 2 loudly.
//
// Subcommands:
//   - memstated          — run the daemon (default)
//   - memstated stop     — POST /admin/shutdown at the configured addr
//   - memstated status   — GET /health and print the response
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"
)

const (
	defaultAddr       = "127.0.0.1:8765" // used only in --addr / stop / status
	healthServiceName = "memstate"
	healthVersion     = "0.6.2"
	readyBanner       = "MEMSTATE_READY addr="
)

func defaultDBPath() string {
	if v := os.Getenv("MEMSTATE_DB"); v != "" {
		return expandHome(v)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".memstate", "memstate.db")
}

// expandHome resolves a leading ~/ or bare ~ to the current user's home dir.
// Shells normally do this, but MEMSTATE_DB is commonly set in config files
// (e.g. an MCP server JSON entry) where the shell never touches it.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		if p == "~" {
			return home
		}
		return filepath.Join(home, p[2:])
	}
	return p
}

func main() {
	// Dispatch subcommands before flag parsing; the subcommands have their
	// own simple arg handling and don't spin up the server.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "stop":
			os.Exit(cmdStop(os.Args[2:]))
		case "status":
			os.Exit(cmdStatus(os.Args[2:]))
		case "export":
			os.Exit(cmdExport(os.Args[2:]))
		case "import":
			os.Exit(cmdImport(os.Args[2:]))
		case "projects":
			os.Exit(cmdProjects(os.Args[2:]))
		case "dump":
			os.Exit(cmdDump(os.Args[2:]))
		case "search":
			os.Exit(cmdSearch(os.Args[2:]))
		case "upgrade":
			os.Exit(cmdUpgrade(os.Args[2:]))
		case "embed":
			os.Exit(cmdEmbed(os.Args[2:]))
		case "-h", "--help", "help":
			printUsage()
			os.Exit(0)
		}
	}

	// --- server mode -----------------------------------------------------
	fs := flag.NewFlagSet("memstated", flag.ExitOnError)
	addrFlag := fs.String("addr", "",
		"bind address (default: random port via :0). "+
			"Set explicitly (e.g. 127.0.0.1:8765) for shared-daemon mode.")
	ownerPIDFlag := fs.Int("owner-pid", 0,
		"parent PID to monitor — daemon exits when the PID disappears (0 = disabled)")
	idleTimeoutFlag := fs.Duration("idle-timeout", 0,
		"shut down after this duration with no HTTP requests (0 = disabled). "+
			"Ignored when --owner-pid is set. Env: MEMSTATE_IDLE_TIMEOUT.")
	embedModelFlag := fs.String("embed-model", "",
		"Ollama model for semantic search (default nomic-embed-text). Env: MEMSTATE_EMBED_MODEL.")
	ollamaURLFlag := fs.String("ollama-url", "",
		"Ollama base URL (default http://127.0.0.1:11434), or an OpenAI-compatible API base URL that ends in /v1. Env: MEMSTATE_OLLAMA_URL.")
	embedTimeoutFlag := fs.Duration("embed-timeout", 0,
		"max time for one Ollama embed call, must cover a cold model load (default 60s). "+
			"Env: MEMSTATE_EMBED_TIMEOUT.")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	idleTimeout := *idleTimeoutFlag
	if idleTimeout == 0 {
		if v := os.Getenv("MEMSTATE_IDLE_TIMEOUT"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				log.Fatalf("memstated: bad MEMSTATE_IDLE_TIMEOUT %q: %v", v, err)
			}
			idleTimeout = d
		}
	}

	resolved := *addrFlag
	if resolved == "" {
		resolved = os.Getenv("MEMSTATE_ADDR")
	}
	explicitAddr := resolved != ""
	if !explicitAddr {
		resolved = "127.0.0.1:0" // random port
	}

	ln, err := net.Listen("tcp", resolved)
	if err != nil {
		if explicitAddr && isAddrInUse(err) {
			handleBusyPort(resolved)
		}
		log.Fatalf("memstated: listen %s: %v", resolved, err)
	}

	actualAddr := ln.Addr().String()

	dbPath := defaultDBPath()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		ln.Close()
		log.Fatalf("memstated: mkdir %s: %v", filepath.Dir(dbPath), err)
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		ln.Close()
		log.Fatalf("memstated: open store %s: %v", dbPath, err)
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Wire the admin shutdown route so POST /admin/shutdown cleanly ends
	// the server by cancelling the root context.
	shutdownFn := func() { stop() }

	embedder := NewEmbedder(*ollamaURLFlag, *embedModelFlag, *embedTimeoutFlag)
	// Eagerly repair any missing vectors (post-migration wipe, model switch,
	// writes made while Ollama was down). Non-blocking; failures just log.
	embedder.BackfillEmbeddings(store)

	// Idle-exit is only meaningful for long-lived detached daemons; when
	// --owner-pid is set the parent already owns our lifetime.
	if *ownerPIDFlag == 0 {
		daemonIdleTimeout = idleTimeout
	}
	handler := newRouter(store, shutdownFn, embedder)
	if idleTimeout > 0 && *ownerPIDFlag == 0 {
		var lastActivity atomic.Int64
		lastActivity.Store(time.Now().UnixNano())
		handler = activityMiddleware(handler, &lastActivity)
		go watchIdle(ctx, &lastActivity, idleTimeout, shutdownFn)
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	// Parent-death watchdog. kill(pid, 0) returns ESRCH when the pid no
	// longer names a live process. We poll because Unix does not give Go
	// an "exit when parent dies" primitive that works cross-platform.
	if *ownerPIDFlag > 0 {
		go watchOwner(*ownerPIDFlag, shutdownFn)
	}

	// Nudge toward `memstated upgrade` when a newer release exists: one
	// stderr line + latest_available in /health. Never blocks startup.
	if os.Getenv("MEMSTATE_NO_UPDATE_CHECK") == "" {
		go watchUpdates(ctx)
	}

	// Announce our bind address on stderr so the spawning parent can find us.
	// Print the banner BEFORE Serve blocks so the parent's stderr reader
	// sees it deterministically.
	fmt.Fprintf(os.Stderr, "%s%s db=%s\n", readyBanner, actualAddr, dbPath)

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("memstated: serve: %v", err)
	}
}

// handleBusyPort is only reached in --addr mode. It distinguishes
// "another memstated" (exit 0 — benign double-start) from "an unrelated
// process squatting on the port" (exit 2 — loud human-visible failure).
func handleBusyPort(addr string) {
	if looksLikeOurDaemon(addr) {
		fmt.Fprintf(os.Stderr, "memstated: already running on %s\n", addr)
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr,
		"memstated: port %s is occupied by a non-memstate process; refusing to start\n",
		addr)
	os.Exit(2)
}

func isAddrInUse(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && errors.Is(opErr.Err, syscall.EADDRINUSE) {
		return true
	}
	return strings.Contains(err.Error(), "address already in use")
}

func looksLikeOurDaemon(addr string) bool {
	h, err := fetchHealth(addr)
	return err == nil && h.Service == healthServiceName
}

// fetchHealth returns the /health document of the daemon at addr, or an
// error when nothing answers within 500ms or the reply is not ours.
func fetchHealth(addr string) (*healthResponse, error) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health: HTTP %d", resp.StatusCode)
	}
	return decodeHealth(resp.Body)
}

// activityMiddleware stamps lastActivity on every incoming request so the
// idle watchdog can tell whether anyone is still using us.
func activityMiddleware(next http.Handler, lastActivity *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastActivity.Store(time.Now().UnixNano())
		next.ServeHTTP(w, r)
	})
}

// watchIdle polls lastActivity and triggers shutdown once no request has
// arrived for `timeout`. The check interval is timeout/4 bounded to [5s, 60s]
// so short timeouts stay responsive without burning wakeups on long ones.
func watchIdle(ctx context.Context, lastActivity *atomic.Int64, timeout time.Duration, shutdown func()) {
	interval := min(max(timeout/4, 5*time.Second), 60*time.Second)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			last := time.Unix(0, lastActivity.Load())
			if time.Since(last) >= timeout {
				fmt.Fprintf(os.Stderr,
					"memstated: idle for %v (limit %v) — shutting down\n",
					time.Since(last).Round(time.Second), timeout)
				shutdown()
				return
			}
		}
	}
}

// watchOwner lives in platform_unix.go / platform_windows.go: liveness
// probing of a foreign PID has no portable primitive.

// ---------- subcommands ----------

func printUsage() {
	fmt.Fprint(os.Stderr,
		`Usage:
  memstated                        run the daemon (random port by default)
  memstated --addr HOST:PORT       run on an explicit address (shared mode)
  memstated --owner-pid N          shut down when process N disappears
  memstated --idle-timeout 30m     shut down after N of no-request idleness
  memstated --embed-model NAME     Ollama model for semantic search
  memstated --ollama-url URL       Ollama base URL, or an OpenAI-compatible base URL ending in /v1
  memstated --embed-timeout 60s    max time per Ollama embed call (cold load included)
  memstated stop   [--addr HOST:PORT]   send a shutdown request to a running daemon
  memstated status [--addr HOST:PORT]   query /health

  memstated projects [--db PATH]   list live projects with memory counts
  memstated dump [--keys] [--db PATH] PROJECT [KEYPATH]
                                   pretty-print a project's memories (or the
                                   subtree under KEYPATH); --keys for the
                                   keypath tree only
  memstated search [--project ID] [--limit N] [--db PATH] QUERY...
                                   full-text search across memories
  memstated upgrade [--addr HOST:PORT]
                                   download the latest release binary over this
                                   one and restart the shared daemon if running
  memstated export --project ID | --all [--out FILE] [--db PATH] [--overwrite]
                                   write project memory (full history) to a JSON file
  memstated import [--project ID] [--force] [--db PATH] FILE
                                   timestamp-merge an export file into the local DB
                                   (newer keys win; new keys keep their history)
  memstated embed status [--watch] [--probe] [--model NAME] [--addr HOST:PORT] [--db PATH]
                                   coverage bar per model, missing counts, content
                                   sizes, cosine histogram and threshold fit;
                                   --watch redraws every 2s, --probe times Ollama;
                                   --model defaults to the running daemon's model
  memstated embed rebuild [--model NAME] [--ollama-url URL] [--embed-timeout D] [--db PATH]
                                   drop and recompute every vector for one model
  memstated embed prune [--keep NAME] [--db PATH]
                                   delete vectors of every model except --keep
                                   (default: the configured model)

Environment:
  MEMSTATE_ADDR           default for --addr
  MEMSTATE_DB             SQLite file path (default ~/.memstate/memstate.db)
  MEMSTATE_IDLE_TIMEOUT   default for --idle-timeout (e.g. 30m)
  MEMSTATE_EMBED_MODEL    default for --embed-model (nomic-embed-text)
  MEMSTATE_OLLAMA_URL     default for --ollama-url (http://127.0.0.1:11434; a /v1 URL is OpenAI-compatible)
  MEMSTATE_EMBED_TIMEOUT  default for --embed-timeout (60s)
  MEMSTATE_NO_UPDATE_CHECK  set to disable the daemon's daily release check
`)
}

func subAddr(args []string) string {
	fs := flag.NewFlagSet("subcmd", flag.ExitOnError)
	addr := fs.String("addr", "", "")
	_ = fs.Parse(args)
	if *addr != "" {
		return *addr
	}
	if v := os.Getenv("MEMSTATE_ADDR"); v != "" {
		return v
	}
	return defaultAddr
}

// openStoreCLI opens the SQLite store for an offline subcommand:
// --db flag beats MEMSTATE_DB beats ~/.memstate/memstate.db. Safe to run
// next to a live daemon — WAL plus busy_timeout serialize the writers, and
// the daemon reads every query fresh from SQLite.
func openStoreCLI(dbFlag string) (*Store, string, error) {
	path := dbFlag
	if path == "" {
		path = defaultDBPath()
	} else {
		path = expandHome(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, "", err
	}
	s, err := OpenStore(path)
	if err != nil {
		return nil, "", err
	}
	return s, path, nil
}

func cmdProjects(args []string) int {
	fs := flag.NewFlagSet("projects", flag.ExitOnError)
	db := fs.String("db", "", "SQLite file (default MEMSTATE_DB or ~/.memstate/memstate.db)")
	_ = fs.Parse(args)
	store, dbPath, err := openStoreCLI(*db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated projects: %v\n", err)
		return 1
	}
	defer store.Close()
	ps, err := store.ListProjects()
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated projects: %v\n", err)
		return 1
	}
	if len(ps) == 0 {
		fmt.Printf("no live projects in %s\n", dbPath)
		return 0
	}
	fmt.Printf("%d live project(s) in %s\n\n", len(ps), dbPath)
	tw := tabwriter.NewWriter(os.Stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tMEMORIES\tLAST UPDATED")
	for _, p := range ps {
		last := "-"
		if p.LastUpdatedAt > 0 {
			last = time.Unix(p.LastUpdatedAt, 0).Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\n", p.ID, p.MemoryCount, last)
	}
	_ = tw.Flush()
	return 0
}

func cmdExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	project := fs.String("project", "", "project id to export")
	all := fs.Bool("all", false, "export every live project")
	out := fs.String("out", "",
		"destination file (default ~/.memstate/exports/<name>_<timestamp>.json)")
	db := fs.String("db", "", "SQLite file (default MEMSTATE_DB or ~/.memstate/memstate.db)")
	overwrite := fs.Bool("overwrite", false, "replace the destination file if it exists")
	_ = fs.Parse(args)
	if *all == (*project != "") {
		fmt.Fprintln(os.Stderr, "memstated export: pass exactly one of --project ID or --all")
		return 2
	}
	store, dbPath, err := openStoreCLI(*db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated export: %v\n", err)
		return 1
	}
	defer store.Close()

	label, ids := *project, []string{*project}
	if *all {
		label = "all"
		ps, err := store.ListProjects()
		if err != nil {
			fmt.Fprintf(os.Stderr, "memstated export: %v\n", err)
			return 1
		}
		ids = ids[:0]
		for _, p := range ps {
			ids = append(ids, p.ID)
		}
		if len(ids) == 0 {
			fmt.Fprintf(os.Stderr, "memstated export: no live projects in %s\n", dbPath)
			return 1
		}
	}
	data, err := store.Export(ids)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated export: %v\n", err)
		return 1
	}

	dest := *out
	if dest == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "memstated export: no home directory; pass --out")
			return 1
		}
		dest = filepath.Join(home, ".memstate", "exports",
			fmt.Sprintf("%s_%s.json", safeFilename(label),
				time.Now().UTC().Format("20060102T150405Z")))
	} else {
		dest = expandHome(dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "memstated export: %v\n", err)
		return 1
	}
	blob, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated export: %v\n", err)
		return 1
	}
	blob = append(blob, '\n')
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if *overwrite {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(dest, flags, 0o644)
	if err != nil {
		if os.IsExist(err) {
			fmt.Fprintf(os.Stderr,
				"memstated export: %s already exists; pass --overwrite or a different --out\n", dest)
			return 1
		}
		fmt.Fprintf(os.Stderr, "memstated export: %v\n", err)
		return 1
	}
	_, werr := f.Write(blob)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		fmt.Fprintf(os.Stderr, "memstated export: write %s: %v\n", dest, werr)
		return 1
	}
	keypaths, versions := 0, 0
	for _, pe := range data.Projects {
		versions += len(pe.Memories)
		for i := range pe.Memories {
			if i == 0 || pe.Memories[i-1].Keypath != pe.Memories[i].Keypath {
				keypaths++
			}
		}
	}
	fmt.Printf("exported %d project(s), %d keypaths, %d versions (%d bytes)\n",
		len(data.Projects), keypaths, versions, len(blob))
	fmt.Printf("from %s\nto   %s\n", dbPath, dest)
	return 0
}

func cmdImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	project := fs.String("project", "",
		"import a single-project file under this id instead of the one recorded in the file")
	force := fs.Bool("force", false,
		"take the file's latest value even when the local version is newer")
	db := fs.String("db", "", "SQLite file (default MEMSTATE_DB or ~/.memstate/memstate.db)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: memstated import [--project ID] [--force] [--db PATH] FILE (flags before FILE)")
		return 2
	}
	src := expandHome(fs.Arg(0))
	blob, err := os.ReadFile(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated import: %v\n", err)
		return 1
	}
	var data ExportData
	if err := json.Unmarshal(blob, &data); err != nil {
		fmt.Fprintf(os.Stderr, "memstated import: %s is not a memstate export: %v\n", src, err)
		return 1
	}
	store, dbPath, err := openStoreCLI(*db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated import: %v\n", err)
		return 1
	}
	defer store.Close()

	stats, err := store.Merge(&data, *project, *force)
	for _, st := range stats {
		fmt.Printf("%s: %d restored, %d updated, %d deleted, %d unchanged, %d kept (local newer)\n",
			st.ProjectID, st.Restored, st.Updated, st.Deleted, st.Unchanged, st.SkippedOlder)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated import: %v\n", err)
		return 1
	}
	if len(stats) == 0 {
		fmt.Println("nothing to merge: file has no projects")
		return 0
	}
	fmt.Printf("merged into %s\n", dbPath)
	// Vectors were dropped for changed keys and never existed for restored
	// ones; rebuild synchronously so semantic search works immediately.
	// If Ollama is down this logs and moves on — the daemon's startup
	// backfill is the safety net.
	emb := NewEmbedder("", "", 0)
	emb.BackfillEmbeddings(store)
	emb.WaitForPending()
	return 0
}

// cmdEmbed manages the stored vector sets: `status` lists models with row
// counts, `rebuild` recomputes one model from scratch, `prune` drops every
// other model. Direct SQLite access, like dump and export; safe next to a
// live daemon because the daemon reads every vector fresh from the DB.
func cmdEmbed(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: memstated embed status|rebuild|prune [flags]")
		return 2
	}
	verb, rest := args[0], args[1:]
	fs := flag.NewFlagSet("embed "+verb, flag.ExitOnError)
	db := fs.String("db", "", "SQLite file (default MEMSTATE_DB or ~/.memstate/memstate.db)")
	model := fs.String("model", "", "embed model (default MEMSTATE_EMBED_MODEL or nomic-embed-text)")
	keep := fs.String("keep", "", "prune: model to keep (default MEMSTATE_EMBED_MODEL or nomic-embed-text)")
	ollamaURL := fs.String("ollama-url", "", "Ollama base URL, or an OpenAI-compatible base URL ending in /v1 (default MEMSTATE_OLLAMA_URL or http://127.0.0.1:11434)")
	embedTimeout := fs.Duration("embed-timeout", 0, "max time per Ollama call (default MEMSTATE_EMBED_TIMEOUT or 60s)")
	watch := fs.Bool("watch", false, "status: redraw every 2s until interrupted")
	probe := fs.Bool("probe", false, "status: time one live Ollama embed call")
	addr := fs.String("addr", "", "running daemon to read the model from (default MEMSTATE_ADDR or 127.0.0.1:8765)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	// The daemon's own model and threshold are the truth for what search
	// uses. Without --model, prefer them over the shell environment, which
	// may be empty when the model was given as a daemon flag.
	liveModel, liveThreshold := runningEmbedConfig(*addr)
	if *model == "" {
		*model = liveModel
	}
	threshold := envThreshold()
	if liveThreshold > 0 && os.Getenv("MEMSTATE_SEMANTIC_THRESHOLD") == "" {
		threshold = liveThreshold
	}
	store, _, err := openStoreCLI(*db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated embed: %v\n", err)
		return 1
	}
	defer store.Close()

	switch verb {
	case "status":
		emb := NewEmbedder(*ollamaURL, *model, *embedTimeout)
		for {
			report, err := collectEmbedStatus(store, emb, threshold, *probe)
			if err != nil {
				fmt.Fprintf(os.Stderr, "memstated embed status: %v\n", err)
				return 1
			}
			if *watch {
				fmt.Print("\033[H\033[2J") // clear screen, cursor home
			}
			renderEmbedStatus(os.Stdout, report)
			if !*watch {
				return 0
			}
			fmt.Printf("\n%s  refresh 2s, Ctrl-C to stop\n", time.Now().Format("15:04:05"))
			time.Sleep(2 * time.Second)
		}
	case "rebuild":
		emb := NewEmbedder(*ollamaURL, *model, *embedTimeout)
		dropped, done, skipped, err := emb.Rebuild(store, os.Stdout)
		fmt.Printf("model %s: dropped %d, embedded %d, skipped %d\n",
			emb.Model, dropped, done, skipped)
		if err != nil {
			fmt.Fprintf(os.Stderr, "memstated embed rebuild: %v\n", err)
			return 1
		}
		return 0
	case "prune":
		keepModel := NewEmbedder("", *keep, 0).Model
		n, err := store.DeleteEmbeddingsExcept(keepModel)
		if err != nil {
			fmt.Fprintf(os.Stderr, "memstated embed prune: %v\n", err)
			return 1
		}
		fmt.Printf("kept %s, dropped %d rows of other models\n", keepModel, n)
		return 0
	}
	fmt.Fprintf(os.Stderr, "memstated embed: unknown verb %q (expected status|rebuild|prune)\n", verb)
	return 2
}

// runningEmbedConfig returns the embed_model and semantic_threshold a
// live daemon reports in /health, or zero values when none answers within
// 500ms. addr "" means MEMSTATE_ADDR, then the default shared address.
func runningEmbedConfig(addr string) (string, float32) {
	if addr == "" {
		addr = os.Getenv("MEMSTATE_ADDR")
	}
	if addr == "" {
		addr = defaultAddr
	}
	h, err := fetchHealth(addr)
	if err != nil {
		return "", 0
	}
	return h.EmbedModel, h.SemanticThreshold
}

func cmdStop(args []string) int {
	addr := subAddr(args)
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/admin/shutdown",
		bytes.NewReader(nil))
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated stop: could not reach %s: %v\n", addr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "memstated stop: HTTP %d %s\n", resp.StatusCode, body)
		return 1
	}
	fmt.Fprintf(os.Stderr, "memstated stop: shutdown requested at %s\n", addr)
	return 0
}

func cmdStatus(args []string) int {
	addr := subAddr(args)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated status: %s unreachable: %v\n", addr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "memstated status: HTTP %d %s\n", resp.StatusCode, body)
		return 1
	}
	var h map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		fmt.Fprintf(os.Stderr, "memstated status: decode error: %v\n", err)
		return 1
	}
	out, _ := json.MarshalIndent(h, "", "  ")
	fmt.Println(string(out))
	if h["service"] != healthServiceName {
		return 2
	}
	return 0
}
