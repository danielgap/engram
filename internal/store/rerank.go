package store

import (
	"math"
	"sort"
	"time"
)

// sqliteTimeLayout is the format produced by SQLite datetime('now'): UTC,
// space-separated, second precision.
const sqliteTimeLayout = "2006-01-02 15:04:05"

// Composite ranking knobs. Multiplicative factors keep the score scale-free
// relative to BM25 magnitude; no cross-magnitude calibration is needed.
const (
	// recencyFloor is the minimum weight an old observation retains, so
	// lexically superior old memories still surface instead of vanishing.
	recencyFloor = 0.35
	// recencyHalfLifeDays is the age at which the recency blend reaches its
	// midpoint (0.35 + 0.65*0.5 ≈ 0.675).
	recencyHalfLifeDays = 90.0
	// pinnedFactor rewards explicit user curation.
	pinnedFactor = 1.5
	// revisionBoostPerCount and revisionCapBound bound reinforcement so
	// self-referenced memories cannot loop their way to the top.
	revisionBoostPerCount = 0.05
	revisionCapBound      = 5
)

// compositeScore blends BM25 lexical relevance with lifecycle signals.
//
// rank is the raw FTS5 bm25() value (negative; more negative = better match).
// The returned score is a positive relevance where higher = better:
//
//	score = (-rank) × recencyFactor × pinnedFactor × revisionFactor
//
// updatedAt is parsed with sqliteTimeLayout; an unparseable or empty value is
// treated as neutral (factor 1.0) so a format hiccup never demotes results.
func compositeScore(rank float64, updatedAt string, pinned bool, revisions int, now time.Time) float64 {
	relevance := -rank

	var recency float64
	if t, err := time.ParseInLocation(sqliteTimeLayout, updatedAt, time.UTC); err == nil {
		ageDays := now.Sub(t).Hours() / 24
		if ageDays < 0 {
			ageDays = 0 // future timestamps (clock skew) count as fresh
		}
		recency = recencyFloor + (1-recencyFloor)*math.Pow(2, -ageDays/recencyHalfLifeDays)
	} else {
		recency = 1.0
	}

	pin := 1.0
	if pinned {
		pin = pinnedFactor
	}

	if revisions > revisionCapBound {
		revisions = revisionCapBound
	}
	revision := 1.0 + float64(revisions)*revisionBoostPerCount

	return relevance * recency * pin * revision
}

// rerankSearchResults computes the composite score for each FTS result row,
// orders them best-first, and rewrites Rank to the negative composite so the
// stored convention (lower rank = better) stays intact for callers.
// Sorting is stable: ties keep their incoming BM25 order.
func rerankSearchResults(rows []SearchResult, now time.Time) []SearchResult {
	if len(rows) < 2 {
		if len(rows) == 1 {
			rows[0].Rank = -compositeScore(rows[0].Rank, rows[0].UpdatedAt, rows[0].Pinned, rows[0].RevisionCount, now)
		}
		return rows
	}

	type scored struct {
		row   SearchResult
		score float64
	}
	scoredRows := make([]scored, len(rows))
	for i, r := range rows {
		scoredRows[i] = scored{row: r, score: compositeScore(r.Rank, r.UpdatedAt, r.Pinned, r.RevisionCount, now)}
	}

	sort.SliceStable(scoredRows, func(i, j int) bool {
		return scoredRows[i].score > scoredRows[j].score
	})

	for i := range scoredRows {
		scoredRows[i].row.Rank = -scoredRows[i].score
		rows[i] = scoredRows[i].row
	}
	return rows
}
