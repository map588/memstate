package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Writers to one keypath from many goroutines must all succeed and produce
// one version each. With a connection pool and a deferred BEGIN, the
// read-then-insert in Write would fail with SQLITE_BUSY on lock upgrade;
// _txlock=immediate makes the writers queue instead.
func TestConcurrentWritersOneKeypath(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const n = 64
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, _, err := store.Write("p", "k", fmt.Sprintf("value %d", i), WriteMeta{}, false); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("write: %v", err)
	}
	versions, err := store.History("p", "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != n || versions[0].Version != n {
		t.Fatalf("want %d versions ending at v%d, got %d ending at v%d", n, n, len(versions), versions[0].Version)
	}
}

// Reads on other connections proceed while a writer holds the write lock.
func TestReadsRunDuringWrites(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, _, err := store.Write("p", "seed", "x", WriteMeta{}, false); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, _, err := store.Write("p", fmt.Sprintf("w%d", i), fmt.Sprintf("%d", j), WriteMeta{}, false); err != nil {
					failures.Add(1)
				}
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := store.List("p", ""); err != nil {
					failures.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d operations failed under concurrency", failures.Load())
	}
}

// No more than the budget's calls reach the server at once: embedMaxQuery
// for queries, embedMaxContent for documents.
func TestEmbedConcurrencyBounded(t *testing.T) {
	for _, c := range []struct {
		name  string
		cap   int
		embed func(e *Embedder) ([]float32, error)
	}{
		{"query", embedMaxQuery, func(e *Embedder) ([]float32, error) { return e.EmbedQuery(context.Background(), "q") }},
		{"content", embedMaxContent, func(e *Embedder) ([]float32, error) { return e.EmbedDocument(context.Background(), "d") }},
	} {
		t.Run(c.name, func(t *testing.T) { checkEmbedBound(t, c.cap, c.embed) })
	}
}

func checkEmbedBound(t *testing.T, cap int, embed func(e *Embedder) ([]float32, error)) {
	var inFlight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{1, 0}})
	}))
	defer srv.Close()
	e := &Embedder{URL: srv.URL, Model: "m", Client: &http.Client{Timeout: 5 * time.Second}}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := embed(e); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if p := peak.Load(); int(p) > cap || p == 0 {
		t.Fatalf("peak in-flight embeds = %d, want 1..%d", p, cap)
	}
}
