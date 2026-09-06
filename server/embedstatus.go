package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// modelStatus is one row of `memstated embed status`: how far one model's
// vector set covers the current keypaths, and what it costs on disk.
type modelStatus struct {
	Model      string
	Configured bool
	Rows       int   // vectors stored, stale rows included
	Missing    int   // current keypaths with no vector under this model
	Dim        int   // vector length
	Bytes      int64 // vector storage
}

// simStats describes the cosine-similarity landscape of one model's
// vectors. It shows whether the semantic threshold fits the model: each
// model family spreads its scores differently, and a threshold tuned for
// one can keep everything or nothing for another.
type simStats struct {
	Vectors int     // vectors in the sample
	Sampled bool    // true when the set was larger than simSampleMax
	Pairs   int     // pairs compared
	Hist    [10]int // pairwise cosine, bins of width 0.1 over [0, 1); negatives land in bin 0
	// Nearest-neighbour cosine per vector, as percentiles over the sample.
	NNp10, NNp50, NNp90 float32
	// KeepAtThreshold is the share of vectors whose nearest neighbour
	// scores at or above the threshold: the share a semantic query near
	// an existing memory can expect to find.
	KeepAtThreshold float64
	// NoiseAtThreshold is the share of all pairs at or above the
	// threshold. Most pairs are unrelated memories, so this approximates
	// the false-positive rate of the threshold for this model.
	NoiseAtThreshold float64
}

// simSampleMax caps the pairwise comparison. 500 vectors give 124,750
// pairs, enough for a stable histogram at any dimension.
const simSampleMax = 500

// probeResult is one timed Ollama embed call.
type probeResult struct {
	Latency time.Duration
	Dim     int
	Err     error
}

// embedStatusReport is everything `embed status` prints.
type embedStatusReport struct {
	Configured string
	OllamaURL  string
	Timeout    time.Duration
	Threshold  float32
	Total      int // current live keypaths
	ContentP50 int // bytes
	ContentMax int // bytes
	Truncated  int // keypaths whose content exceeds embedMaxBytes
	Models     []modelStatus
	Sim        *simStats // configured model only; nil below two vectors
	Probe      *probeResult
}

// collectEmbedStatus gathers the report from the store and, when probe is
// set, one live Ollama call. The similarity section covers emb.Model.
func collectEmbedStatus(store *Store, emb *Embedder, threshold float32, probe bool) (*embedStatusReport, error) {
	r := &embedStatusReport{
		Configured: emb.Model,
		OllamaURL:  emb.URL,
		Timeout:    emb.timeout(),
		Threshold:  threshold,
	}

	// No vector matches model "", so this lists every current keypath.
	all, err := store.ListMissingEmbeddings("")
	if err != nil {
		return nil, fmt.Errorf("list keypaths: %w", err)
	}
	r.Total = len(all)
	sizes := make([]int, 0, len(all))
	for _, m := range all {
		sizes = append(sizes, len(m.Content))
		if len(m.Content) > embedMaxBytes {
			r.Truncated++
		}
	}
	sort.Ints(sizes)
	if len(sizes) > 0 {
		r.ContentP50 = sizes[len(sizes)/2]
		r.ContentMax = sizes[len(sizes)-1]
	}

	stats, err := store.EmbeddingModelStats()
	if err != nil {
		return nil, fmt.Errorf("model stats: %w", err)
	}
	seen := false
	for _, st := range stats {
		missing, err := store.ListMissingEmbeddings(st.Model)
		if err != nil {
			return nil, fmt.Errorf("missing for %s: %w", st.Model, err)
		}
		ms := modelStatus{
			Model: st.Model, Rows: st.Rows, Dim: st.Dim, Bytes: st.Bytes,
			Missing: len(missing), Configured: st.Model == emb.Model,
		}
		seen = seen || ms.Configured
		r.Models = append(r.Models, ms)
	}
	if !seen {
		// The configured model has no vectors yet: show it with an empty bar.
		r.Models = append(r.Models, modelStatus{
			Model: emb.Model, Configured: true, Missing: r.Total,
		})
	}

	rows, err := store.ListKeypathEmbeddings("", emb.Model)
	if err != nil {
		return nil, fmt.Errorf("vectors for %s: %w", emb.Model, err)
	}
	vecs := make([][]float32, 0, len(rows))
	for _, row := range rows {
		vecs = append(vecs, row.Vector)
	}
	r.Sim = similarityStats(vecs, threshold, simSampleMax)

	if probe {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), emb.timeout())
		vec, err := emb.EmbedQuery(ctx, "memstate embed status probe")
		cancel()
		r.Probe = &probeResult{Latency: time.Since(start), Dim: len(vec), Err: err}
	}
	return r, nil
}

// similarityStats computes the pairwise cosine histogram and the
// nearest-neighbour percentiles over at most maxN vectors. It returns nil
// below two vectors, where no pair exists.
func similarityStats(vecs [][]float32, threshold float32, maxN int) *simStats {
	if len(vecs) < 2 {
		return nil
	}
	s := &simStats{Vectors: len(vecs)}
	if len(vecs) > maxN {
		vecs = vecs[:maxN]
		s.Sampled = true
		s.Vectors = maxN
	}
	nearest := make([]float32, len(vecs))
	for i := range nearest {
		nearest[i] = -1
	}
	noisy := 0
	for i := 0; i < len(vecs); i++ {
		for j := i + 1; j < len(vecs); j++ {
			c := cosine(vecs[i], vecs[j])
			s.Pairs++
			if c >= threshold {
				noisy++
			}
			bin := int(c * 10)
			if bin < 0 {
				bin = 0
			}
			if bin > 9 {
				bin = 9
			}
			s.Hist[bin]++
			if c > nearest[i] {
				nearest[i] = c
			}
			if c > nearest[j] {
				nearest[j] = c
			}
		}
	}
	kept := 0
	for _, nn := range nearest {
		if nn >= threshold {
			kept++
		}
	}
	s.KeepAtThreshold = float64(kept) / float64(len(nearest))
	s.NoiseAtThreshold = float64(noisy) / float64(s.Pairs)
	sort.Slice(nearest, func(i, j int) bool { return nearest[i] < nearest[j] })
	s.NNp10 = percentile(nearest, 0.10)
	s.NNp50 = percentile(nearest, 0.50)
	s.NNp90 = percentile(nearest, 0.90)
	return s
}

// percentile returns the value at fraction p of a sorted slice.
func percentile(sorted []float32, p float64) float32 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Round(p * float64(len(sorted)-1)))
	return sorted[idx]
}

// bar renders done/total as a fixed-width block bar. A zero total renders
// as empty.
func bar(done, total, width int) string {
	if total <= 0 || done < 0 {
		return strings.Repeat("░", width)
	}
	if done > total {
		done = total
	}
	filled := done * width / total
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// humanBytes renders a byte count with a binary unit.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// renderEmbedStatus writes the report as a terminal dashboard.
func renderEmbedStatus(w io.Writer, r *embedStatusReport) {
	fmt.Fprintf(w, "model      %s\n", r.Configured)
	fmt.Fprintf(w, "ollama     %s   timeout %s   threshold %.2f\n", r.OllamaURL, r.Timeout, r.Threshold)
	if r.Probe != nil {
		if r.Probe.Err != nil {
			fmt.Fprintf(w, "probe      unreachable: %v\n", r.Probe.Err)
		} else {
			fmt.Fprintf(w, "probe      %s per query embed, dim %d\n",
				r.Probe.Latency.Round(time.Millisecond), r.Probe.Dim)
		}
	}
	fmt.Fprintf(w, "keypaths   %d current   content p50 %s, max %s   %d over the %s embed cap (head only)\n",
		r.Total, humanBytes(int64(r.ContentP50)), humanBytes(int64(r.ContentMax)),
		r.Truncated, humanBytes(embedMaxBytes))
	fmt.Fprintln(w)

	fmt.Fprintf(w, "  %-28s %-30s %14s  %5s  %8s  %s\n", "model", "coverage", "done/total", "dim", "size", "rows")
	for _, m := range r.Models {
		mark := " "
		if m.Configured {
			mark = "*"
		}
		done := r.Total - m.Missing
		pct := 0
		if r.Total > 0 {
			pct = done * 100 / r.Total
		}
		fmt.Fprintf(w, "%s %-28s %s %6d/%-6d %3d%%  %5d  %8s  %d\n",
			mark, m.Model, bar(done, r.Total, 30), done, r.Total, pct, m.Dim, humanBytes(m.Bytes), m.Rows)
	}
	fmt.Fprintln(w)

	if r.Sim == nil {
		fmt.Fprintf(w, "similarity  fewer than two %s vectors; nothing to compare yet\n", r.Configured)
		return
	}
	s := r.Sim
	sampled := ""
	if s.Sampled {
		sampled = " (sampled)"
	}
	fmt.Fprintf(w, "pairwise cosine, %d vectors%s, %d pairs\n", s.Vectors, sampled, s.Pairs)
	maxBin := 0
	for _, n := range s.Hist {
		if n > maxBin {
			maxBin = n
		}
	}
	for i, n := range s.Hist {
		width := 0
		if maxBin > 0 {
			width = n * 30 / maxBin
		}
		fmt.Fprintf(w, "  %.1f-%.1f %-30s %d\n", float64(i)/10, float64(i+1)/10, strings.Repeat("▇", width), n)
	}
	fmt.Fprintf(w, "nearest neighbour   p10 %.2f   p50 %.2f   p90 %.2f\n", s.NNp10, s.NNp50, s.NNp90)
	fmt.Fprintf(w, "threshold %.2f keeps the nearest neighbour of %.0f%% of vectors; %.1f%% of all pairs pass it\n",
		r.Threshold, s.KeepAtThreshold*100, s.NoiseAtThreshold*100)
	fmt.Fprintln(w, "  read: raise the threshold until few pairs pass but the nearest-neighbour share stays high.")
	fmt.Fprintln(w, "        set it with MEMSTATE_SEMANTIC_THRESHOLD or per request.")
}
