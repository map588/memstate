package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
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

func newRouter(store *Store, shutdown func()) http.Handler {
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
	mux.HandleFunc("POST /api/v1/memories/store", handleStore(store))
	mux.HandleFunc("POST /api/v1/memories/remember", handleRemember(store))
	mux.HandleFunc("POST /api/v1/memories/delete", handleDelete(store))
	mux.HandleFunc("POST /api/v1/projects/delete", handleDeleteProject(store))

	// --- reads ------------------------------------------------------------
	mux.HandleFunc("POST /api/v1/memories/search", handleSearch(store))
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

func handleStore(store *Store) http.HandlerFunc {
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
		action := "created"
		if prev != nil {
			action = "superseded"
		}
		writeJSON(w, http.StatusOK, writeResp{Action: action, Stored: stored, Superseded: prev})
	}
}

type rememberReq struct {
	ProjectID string `json:"project_id"`
	Keypath   string `json:"keypath"`
	Content   string `json:"content"`
	Source    string `json:"source,omitempty"`
	Context   string `json:"context,omitempty"` // accepted but not used
}

func handleRemember(store *Store) http.HandlerFunc {
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
		if in.Keypath == "" {
			writeErr(w, http.StatusBadRequest,
				"keypath is required in local mode (auto-extraction not supported)")
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
		action := "created"
		if prev != nil {
			action = "superseded"
		}
		writeJSON(w, http.StatusOK, writeResp{Action: action, Stored: stored, Superseded: prev})
	}
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
	Query     string `json:"query"`
	ProjectID string `json:"project_id,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

func handleSearch(store *Store) http.HandlerFunc {
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
		hits, err := store.Search(in.ProjectID, in.Query, in.Limit)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error()) // usually bad FTS syntax
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"query":       in.Query,
			"results":     hits,
			"total_found": len(hits),
		})
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
