package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #58 — the numbers that decided the design, measured rather than guessed.
//
// The choice between SQLite FTS and a plain inverted index turns entirely on
// the size of the corpus and on how long a cold build costs, and neither is
// knowable from a unit test with three fabricated cells. This one runs
// against the real notebooks on a machine.
//
// It never writes to them. The corpus is copied into a temp directory first,
// because the whole point of the index is that it lands *beside* the log —
// and a measurement is not worth putting a derived file into someone's real
// notebook directory to get.
//
//	COLLECTIF_NOTEBOOK_CORPUS=~/proj/.collectif/notebooks \
//	  go test ./src -run TestLive_SearchCorpus -v
func TestLive_SearchCorpus(t *testing.T) {
	src := os.Getenv("COLLECTIF_NOTEBOOK_CORPUS")
	if src == "" {
		t.Skip("set COLLECTIF_NOTEBOOK_CORPUS to a real notebooks directory to measure against it")
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Skipf("corpus unreadable: %v", err)
	}

	dir := withTempNotebooks(t)
	var logBytes, snapBytes int64
	notebooks := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".snap.json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatalf("copy %s: %v", name, err)
		}
		if strings.HasSuffix(name, ".jsonl") {
			logBytes += int64(len(b))
			notebooks++
		} else {
			snapBytes += int64(len(b))
		}
	}
	if notebooks == 0 {
		t.Skip("no notebooks in the corpus")
	}

	// Cold: no index on disk, no index in memory. Every log is folded.
	cold := time.Now()
	first, err := searchNotebooks(searchQuery{Text: "snapshot"})
	if err != nil {
		t.Fatalf("cold search: %v", err)
	}
	coldFor := time.Since(cold)

	var indexBytes int64
	dirEntries, _ := os.ReadDir(dir)
	for _, e := range dirEntries {
		if strings.HasSuffix(e.Name(), searchIndexSuffix) {
			if fi, err := e.Info(); err == nil {
				indexBytes += fi.Size()
			}
		}
	}

	// Warm: the indexes are in memory and only their log stamps are re-read.
	var warmTotal time.Duration
	queries := []string{"snapshot", "git push", "projection format", "cache bug", "notebook"}
	for _, q := range queries {
		start := time.Now()
		if _, err := searchNotebooks(searchQuery{Text: q}); err != nil {
			t.Fatalf("warm search %q: %v", q, err)
		}
		warmTotal += time.Since(start)
	}

	// Cold-from-disk: a fresh process, indexes already written. This is what
	// the file on disk actually buys, and it is the number that says whether
	// persisting the index was worth doing at all.
	searchCacheMu.Lock()
	searchCache = map[string]*searchIndex{}
	searchCacheMu.Unlock()
	restart := time.Now()
	if _, err := searchNotebooks(searchQuery{Text: "snapshot"}); err != nil {
		t.Fatalf("restart search: %v", err)
	}
	restartFor := time.Since(restart)

	t.Logf("corpus: %d notebooks, %.2f MB of logs, %.2f MB of snapshots",
		notebooks, mb(logBytes), mb(snapBytes))
	t.Logf("index:  %.2f MB on disk (%.0f%% of the logs)", mb(indexBytes),
		100*float64(indexBytes)/float64(logBytes))
	t.Logf("cold build + query: %v (%d hits, %d total)", coldFor.Round(time.Millisecond),
		first.Count, first.Total)
	t.Logf("from disk, no memo:  %v", restartFor.Round(time.Millisecond))
	t.Logf("warm query:          %v average over %d queries",
		(warmTotal / time.Duration(len(queries))).Round(time.Microsecond), len(queries))
}

func mb(n int64) float64 { return float64(n) / (1024 * 1024) }
