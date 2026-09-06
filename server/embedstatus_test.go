package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestBar(t *testing.T) {
	cases := []struct {
		done, total int
		want        string
	}{
		{0, 0, "░░░░"},
		{0, 10, "░░░░"},
		{5, 10, "██░░"},
		{10, 10, "████"},
		{12, 10, "████"},
		{-1, 10, "░░░░"},
	}
	for _, c := range cases {
		if got := bar(c.done, c.total, 4); got != c.want {
			t.Errorf("bar(%d,%d): got %q want %q", c.done, c.total, got, c.want)
		}
	}
}

func TestSimilarityStats(t *testing.T) {
	if similarityStats(nil, 0.5, 10) != nil || similarityStats([][]float32{{1, 0}}, 0.5, 10) != nil {
		t.Fatal("fewer than two vectors must give nil")
	}
	// Two identical vectors, one orthogonal to both.
	vecs := [][]float32{{1, 0}, {1, 0}, {0, 1}}
	s := similarityStats(vecs, 0.5, 10)
	if s.Vectors != 3 || s.Pairs != 3 || s.Sampled {
		t.Fatalf("shape: %+v", s)
	}
	// Pairs: (a,b)=1.0 -> bin 9, (a,c)=0 -> bin 0, (b,c)=0 -> bin 0.
	if s.Hist[9] != 1 || s.Hist[0] != 2 {
		t.Fatalf("hist: %v", s.Hist)
	}
	// Nearest neighbours: a=1, b=1, c=0. Two of three clear a 0.5 threshold.
	if s.NNp10 != 0 || s.NNp50 != 1 || s.NNp90 != 1 {
		t.Fatalf("percentiles: p10 %v p50 %v p90 %v", s.NNp10, s.NNp50, s.NNp90)
	}
	if s.KeepAtThreshold < 0.66 || s.KeepAtThreshold > 0.67 {
		t.Fatalf("keep: %v", s.KeepAtThreshold)
	}
	// One of three pairs (a,b) scores >= 0.5.
	if s.NoiseAtThreshold < 0.33 || s.NoiseAtThreshold > 0.34 {
		t.Fatalf("noise: %v", s.NoiseAtThreshold)
	}
	// Sampling cap.
	many := make([][]float32, 0, 20)
	for i := 0; i < 20; i++ {
		many = append(many, []float32{float32(i), 1})
	}
	s = similarityStats(many, 0.5, 5)
	if !s.Sampled || s.Vectors != 5 || s.Pairs != 10 {
		t.Fatalf("sampling: %+v", s)
	}
}

func TestPercentile(t *testing.T) {
	sorted := []float32{0.1, 0.2, 0.3, 0.4, 0.5}
	if percentile(sorted, 0) != 0.1 || percentile(sorted, 0.5) != 0.3 || percentile(sorted, 1) != 0.5 {
		t.Fatal("percentile endpoints and midpoint")
	}
	if percentile(nil, 0.5) != 0 {
		t.Fatal("empty slice must give 0")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 1536: "1.5 KB", 5 << 20: "5.0 MB"}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d): got %q want %q", n, got, want)
		}
	}
}

func TestEmbedStatusReport(t *testing.T) {
	ollama := mockOllama(t)
	defer ollama.Close()
	store := newTestStore(t)
	_, _, _ = store.Write("p", "a", "alpha content", WriteMeta{}, false)
	_, _, _ = store.Write("p", "b", "bravo content", WriteMeta{}, false)
	_, _, _ = store.Write("p", "big", strings.Repeat("x", embedMaxBytes+1), WriteMeta{}, false)
	_, _, _ = store.Write("p", "dead", "gone", WriteMeta{}, false)
	_, _, _ = store.Delete("p", "dead")

	emb := newTestEmbedder(t, ollama)

	// Before any vector exists the configured model still shows, fully missing.
	r, err := collectEmbedStatus(store, emb, 0.5, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 3 || r.Truncated != 1 || r.ContentMax != embedMaxBytes+1 {
		t.Fatalf("keypath stats: %+v", r)
	}
	if len(r.Models) != 1 || r.Models[0].Model != "mock" || r.Models[0].Missing != 3 || !r.Models[0].Configured {
		t.Fatalf("models before backfill: %+v", r.Models)
	}
	if r.Sim != nil {
		t.Fatal("no vectors: similarity must be nil")
	}

	// Partial coverage plus a foreign model.
	if _, _, err := emb.Backfill(store, bytes.NewBuffer(nil)); err != nil {
		t.Fatal(err)
	}
	_ = store.UpsertKeypathEmbedding("p", "a", "other", 2, packVector([]float32{1, 0}))
	_, _ = store.DeleteEmbeddingsForModel("") // no-op, keeps the API honest
	r, err = collectEmbedStatus(store, emb, 0.5, true)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]modelStatus{}
	for _, m := range r.Models {
		byName[m.Model] = m
	}
	if m := byName["mock"]; m.Missing != 0 || m.Rows != 3 || m.Dim != 8 || m.Bytes != 3*8*4 {
		t.Fatalf("mock status: %+v", m)
	}
	if m := byName["other"]; m.Missing != 2 || m.Rows != 1 || m.Configured {
		t.Fatalf("other status: %+v", m)
	}
	if r.Sim == nil || r.Sim.Vectors != 3 || r.Sim.Pairs != 3 {
		t.Fatalf("sim: %+v", r.Sim)
	}
	if r.Probe == nil || r.Probe.Err != nil || r.Probe.Dim != 8 {
		t.Fatalf("probe: %+v", r.Probe)
	}

	var out bytes.Buffer
	renderEmbedStatus(&out, r)
	text := out.String()
	for _, want := range []string{
		"model      mock",
		"* mock",
		"3/3", "100%",
		"  other",
		"1/3", " 33%",
		"1 over the 5.9 KB embed cap",
		"pairwise cosine, 3 vectors, 3 pairs",
		"nearest neighbour",
		"probe      ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("render missing %q in:\n%s", want, text)
		}
	}
}

func TestRunningEmbedConfig(t *testing.T) {
	t.Setenv("MEMSTATE_SEMANTIC_THRESHOLD", "0.6")
	ts := newTestServerWithEmbedder(t, &Embedder{Model: "live-model"})
	addr := strings.TrimPrefix(ts.URL, "http://")
	if model, th := runningEmbedConfig(addr); model != "live-model" || th != 0.6 {
		t.Fatalf("live daemon: got %q %v", model, th)
	}
	if model, th := runningEmbedConfig("127.0.0.1:9"); model != "" || th != 0 {
		t.Fatalf("closed port must give zero values, got %q %v", model, th)
	}
}
