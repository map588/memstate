package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// healthResponse is the JSON shape probed by double-start detection.
// Both fields MUST be present for a sibling daemon to conclude "it's us."
type healthResponse struct {
	Service string `json:"service"`
	Version string `json:"version"`
}

func decodeHealth(r io.Reader) (*healthResponse, error) {
	var h healthResponse
	if err := json.NewDecoder(r).Decode(&h); err != nil {
		return nil, err
	}
	if h.Service == "" || h.Version == "" {
		return nil, errors.New("missing fields")
	}
	return &h, nil
}

func newRouter(store *Store, shutdown func(), embedder *Embedder) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthResponse{
			Service: healthServiceName,
			Version: healthVersion,
		})
	})

	// POST /admin/shutdown — local-only kill switch for a manually-started
	// shared daemon. Loopback binding is the access control; anyone who can
	// already reach 127.0.0.1 on this user's system can also `pkill`.
	mux.HandleFunc("POST /admin/shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "shutting down"})
		// Flush the response before cancelling so the caller gets a clean reply.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if shutdown != nil {
			go shutdown()
		}
	})

	// --- writes -----------------------------------------------------------
	mux.HandleFunc("POST /api/v1/memories/store", handleStore(store, embedder))
	mux.HandleFunc("POST /api/v1/memories/remember", handleRemember(store, embedder))
	mux.HandleFunc("POST /api/v1/memories/delete", handleDelete(store))
	mux.HandleFunc("POST /api/v1/projects/delete", handleDeleteProject(store))

	// --- reads ------------------------------------------------------------
	mux.HandleFunc("POST /api/v1/memories/search", handleSearch(store, embedder))
	mux.HandleFunc("POST /api/v1/memories/history", handleHistory(store))
	mux.HandleFunc("POST /api/v1/keypaths", handleKeypaths(store))
	mux.HandleFunc("GET /api/v1/tree", handleTree(store))
	mux.HandleFunc("GET /api/v1/projects", handleProjects(store))
	mux.HandleFunc("GET /api/v1/memories/{id}", handleMemoryByID(store))

	return logging(mux)
}

// ---------- helpers ----------

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// checkProjectLive rejects soft-deleted projects on reads and writes.
// A project that does not yet exist is NOT deleted; writes are allowed
// (and will create it). Only a row with deleted_at set triggers rejection.
func checkProjectLive(store *Store, projectID string) error {
	deleted, err := store.ProjectDeleted(projectID)
	if err != nil {
		return err
	}
	if deleted {
		return fmt.Errorf("project %q is soft-deleted; write again to revive it", projectID)
	}
	return nil
}

// maybeEmbedKeypath fires a fire-and-forget goroutine that embeds the given
// keypath with the configured model and upserts into keypath_embeddings.
// No-op if the embedder is nil or the row already exists. Errors are logged
// (throttled) and do not surface to the caller — writes succeed regardless.
func maybeEmbedKeypath(store *Store, embedder *Embedder, projectID, keypath string) {
	if embedder == nil {
		return
	}
	embedder.inFlight.Add(1)
	go func() {
		defer embedder.inFlight.Done()
		has, err := store.HasKeypathEmbedding(projectID, keypath, embedder.Model)
		if err != nil {
			embedder.maybeLog(fmt.Sprintf("has-embedding check failed for %s/%s: %v",
				projectID, keypath, err))
			return
		}
		if has {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		vec, err := embedder.Embed(ctx, keypath)
		if err != nil {
			embedder.maybeLog(fmt.Sprintf("embed failed for %s/%s: %v",
				projectID, keypath, err))
			return
		}
		if err := store.UpsertKeypathEmbedding(projectID, keypath, embedder.Model,
			len(vec), packVector(vec)); err != nil {
			embedder.maybeLog(fmt.Sprintf("upsert embedding for %s/%s: %v",
				projectID, keypath, err))
		}
	}()
}

// ---------- write handlers ----------

type storeReq struct {
	ProjectID string   `json:"project_id"`
	Keypath   string   `json:"keypath"`
	Content   string   `json:"content"`
	Source    string   `json:"source,omitempty"`
	Category  string   `json:"category,omitempty"` // accepted but not yet stored
	Topics    []string `json:"topics,omitempty"`   // accepted but not yet stored
}

type writeResp struct {
	Action     string  `json:"action"` // "created" | "superseded"
	Stored     *Memory `json:"stored"`
	Superseded *Memory `json:"superseded,omitempty"`
}

func handleStore(store *Store, embedder *Embedder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in storeReq
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if in.ProjectID == "" || in.Keypath == "" || in.Content == "" {
			writeErr(w, http.StatusBadRequest, "project_id, keypath, content are required")
			return
		}
		if err := checkProjectLive(store, in.ProjectID); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		kp := NormalizeKeypath(in.Keypath)
		stored, prev, err := store.Write(in.ProjectID, kp, in.Content, in.Source, false)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		maybeEmbedKeypath(store, embedder, in.ProjectID, kp)
		writeJSON(w, http.StatusOK, writeResp{
			Action:     classifyWrite(stored, prev),
			Stored:     stored,
			Superseded: prev,
		})
	}
}

type rememberReq struct {
	ProjectID string  `json:"project_id"`
	Keypath   string  `json:"keypath,omitempty"` // optional — extraction runs when empty
	Content   string  `json:"content"`
	Source    string  `json:"source,omitempty"`
	// Root is the prefix applied to every extracted keypath. Absent (nil)
	// means "default to <project_id>". An explicit "" disables the prefix.
	// Any explicit value is used as-is after keypath normalization.
	Root    *string `json:"root,omitempty"`
	Context string  `json:"context,omitempty"` // accepted but not used
}

// extractedItem is one entry in a batch remember response.
type extractedItem struct {
	Keypath    string  `json:"keypath"`
	Action     string  `json:"action"` // "created" | "superseded"
	Stored     *Memory `json:"stored"`
	Superseded *Memory `json:"superseded,omitempty"`
}

// rememberResp is returned from /memories/remember regardless of whether the
// caller supplied an explicit keypath or relied on heading extraction. The
// single-keypath path returns a one-element Items array so callers have one
// shape to parse.
type rememberResp struct {
	Method string          `json:"method"` // "explicit" | "headings"
	Items  []extractedItem `json:"items"`
}

func handleRemember(store *Store, embedder *Embedder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in rememberReq
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if in.ProjectID == "" || in.Content == "" {
			writeErr(w, http.StatusBadRequest, "project_id and content are required")
			return
		}
		if err := checkProjectLive(store, in.ProjectID); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}

		var sections []Section
		method := "explicit"
		if in.Keypath != "" {
			sections = []Section{{Keypath: NormalizeKeypath(in.Keypath), Content: in.Content}}
		} else {
			root := in.ProjectID
			if in.Root != nil {
				root = NormalizeKeypath(*in.Root)
			}
			sections = ExtractHeadings(in.Content, root)
			method = "headings"
			if len(sections) == 0 {
				writeErr(w, http.StatusBadRequest,
					"no keypath provided and no ## headings found in content; "+
						"either pass an explicit keypath or include h2+ headings")
				return
			}
		}

		batch, err := store.WriteBatch(in.ProjectID, sections, in.Source)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := rememberResp{Method: method, Items: make([]extractedItem, len(batch))}
		for i, it := range batch {
			out.Items[i] = extractedItem{
				Keypath:    it.Keypath,
				Action:     classifyWrite(it.Stored, it.Superseded),
				Stored:     it.Stored,
				Superseded: it.Superseded,
			}
			maybeEmbedKeypath(store, embedder, in.ProjectID, it.Keypath)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// classifyWrite names the outcome of a single write:
// "created"    — no prior version existed
// "superseded" — a new version superseded a distinct prior version
// "unchanged"  — identical content to the current version, nothing written
func classifyWrite(stored, prev *Memory) string {
	if prev == nil {
		return "created"
	}
	if stored != nil && stored.ID == prev.ID {
		return "unchanged"
	}
	return "superseded"
}

type deleteReq struct {
	ProjectID string `json:"project_id"`
	Keypath   string `json:"keypath"`
	Recursive bool   `json:"recursive,omitempty"`
}

type deleteResp struct {
	DeletedCount    int      `json:"deleted_count"`
	DeletedKeypaths []string `json:"deleted_keypaths"`
}

func handleDelete(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in deleteReq
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if in.ProjectID == "" || in.Keypath == "" {
			writeErr(w, http.StatusBadRequest, "project_id and keypath are required")
			return
		}
		if err := checkProjectLive(store, in.ProjectID); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		kp := NormalizeKeypath(in.Keypath)

		if in.Recursive {
			killed, err := store.DeleteSubtree(in.ProjectID, kp)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, deleteResp{
				DeletedCount: len(killed), DeletedKeypaths: killed,
			})
			return
		}
		_, _, err := store.Delete(in.ProjectID, kp)
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, deleteResp{
			DeletedCount: 1, DeletedKeypaths: []string{kp},
		})
	}
}

type deleteProjectReq struct {
	ProjectID string `json:"project_id"`
}

func handleDeleteProject(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in deleteProjectReq
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if in.ProjectID == "" {
			writeErr(w, http.StatusBadRequest, "project_id required")
			return
		}
		// Count deleted memories for parity with hosted response shape.
		live, _ := store.List(in.ProjectID, "")
		if err := store.DeleteProject(in.ProjectID); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"project_id":    in.ProjectID,
			"deleted_count": len(live),
		})
	}
}

// ---------- read handlers ----------

type searchReq struct {
	Query     string  `json:"query"`
	ProjectID string  `json:"project_id,omitempty"`
	Limit     int     `json:"limit,omitempty"`
	Mode      string  `json:"mode,omitempty"`      // "fts" (default) | "semantic"
	Threshold float32 `json:"threshold,omitempty"` // semantic only; 0 = use env / default
}

func handleSearch(store *Store, embedder *Embedder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in searchReq
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if in.Query == "" {
			writeErr(w, http.StatusBadRequest, "query required")
			return
		}
		if in.ProjectID != "" {
			if err := checkProjectLive(store, in.ProjectID); err != nil {
				writeErr(w, http.StatusConflict, err.Error())
				return
			}
		}

		mode := in.Mode
		if mode == "" {
			mode = "fts"
		}
		switch mode {
		case "fts":
			hits, err := store.Search(in.ProjectID, in.Query, in.Limit)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"mode":        "fts",
				"query":       in.Query,
				"results":     hits,
				"total_found": len(hits),
			})
		case "semantic":
			if embedder == nil {
				writeErr(w, http.StatusServiceUnavailable,
					"semantic search disabled (no embedder configured)")
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			qvec, err := embedder.Embed(ctx, in.Query)
			if err != nil {
				writeErr(w, http.StatusBadGateway,
					fmt.Sprintf("embed query: %v", err))
				return
			}
			threshold := in.Threshold
			if threshold == 0 {
				threshold = envThreshold()
			}
			hits, err := store.SemanticSearch(in.ProjectID, qvec, embedder.Model, threshold, in.Limit)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"mode":        "semantic",
				"model":       embedder.Model,
				"threshold":   threshold,
				"query":       in.Query,
				"results":     hits,
				"total_found": len(hits),
			})
		default:
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("unknown mode %q (expected fts|semantic)", mode))
		}
	}
}

type historyReq struct {
	ProjectID string `json:"project_id,omitempty"`
	Keypath   string `json:"keypath,omitempty"`
	MemoryID  int64  `json:"memory_id,omitempty"`
}

func handleHistory(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in historyReq
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		var versions []*Memory

		switch {
		case in.MemoryID != 0:
			m, err := store.GetByID(in.MemoryID)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if m == nil {
				writeErr(w, http.StatusNotFound, "memory not found")
				return
			}
			versions, err = store.History(m.ProjectID, m.Keypath)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
		case in.ProjectID != "" && in.Keypath != "":
			var err error
			versions, err = store.History(in.ProjectID, NormalizeKeypath(in.Keypath))
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
		default:
			writeErr(w, http.StatusBadRequest,
				"provide memory_id OR (project_id AND keypath)")
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"versions":       versions,
			"total_versions": len(versions),
		})
	}
}

type keypathsReq struct {
	ProjectID      string `json:"project_id"`
	Keypath        string `json:"keypath,omitempty"`
	Recursive      bool   `json:"recursive,omitempty"`
	IncludeContent bool   `json:"include_content,omitempty"`
	AtRevision     int    `json:"at_revision,omitempty"` // accepted, not implemented
}

func handleKeypaths(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in keypathsReq
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if in.ProjectID == "" {
			writeErr(w, http.StatusBadRequest, "project_id required")
			return
		}
		if err := checkProjectLive(store, in.ProjectID); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		list, err := store.List(in.ProjectID, NormalizeKeypath(in.Keypath))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Strip content when not requested, to match hosted behavior.
		if !in.IncludeContent {
			for _, m := range list {
				m.Content = ""
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"memories":    list,
			"total_count": len(list),
		})
	}
}

func handleTree(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pid := r.URL.Query().Get("project_id")
		if pid == "" {
			writeErr(w, http.StatusBadRequest, "project_id required")
			return
		}
		if err := checkProjectLive(store, pid); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		tree, err := store.Tree(pid)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Flatten top-level children into "domains" for hosted-shape parity.
		domains := tree.Children
		total := 0
		countValues(tree, &total)
		writeJSON(w, http.StatusOK, map[string]any{
			"project_id":     pid,
			"domains":        domains,
			"total_memories": total,
		})
	}
}

func countValues(n *TreeNode, total *int) {
	if n.HasValue {
		*total++
	}
	for _, c := range n.Children {
		countValues(c, total)
	}
}

func handleProjects(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ps, err := store.ListProjects()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"projects": ps})
	}
}

func handleMemoryByID(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := r.PathValue("id")
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "id must be integer")
			return
		}
		m, err := store.GetByID(id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if m == nil {
			writeErr(w, http.StatusNotFound, "memory not found")
			return
		}
		writeJSON(w, http.StatusOK, m)
	}
}
