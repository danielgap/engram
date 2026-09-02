package store

import (
	"math"
	"sort"
	"strconv"
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
// nowDay is the day ordinal of "now" (dayOrdinalOfTime), computed once per
// query; updatedAt's ordinal is derived by pure digit arithmetic — no
// time.Parse per row. Age granularity is one day, irrelevant against a 90-day
// half-life. An unparseable or empty updatedAt is treated as neutral (factor
// 1.0) so a format hiccup never demotes results.
func compositeScore(rank float64, updatedAt string, pinned bool, revisions int, nowDay int) float64 {
	relevance := -rank

	var recency float64
	if rowDay, ok := dayOrdinal(updatedAt); ok {
		ageDays := float64(nowDay - rowDay)
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

// dayOrdinalOfTime converts a wall-clock time to the same day ordinal
// dayOrdinal produces for SQLite timestamp strings. Called once per query.
func dayOrdinalOfTime(t time.Time) int {
	ordinal, _ := dayOrdinal(t.UTC().Format(sqliteTimeLayout))
	return ordinal
}

// dayOrdinal converts a SQLite datetime('now') string ("YYYY-MM-DD HH:MM:SS",
// UTC, fixed-width, zero-padded) into a proleptic-Gregorian day ordinal using
// digit arithmetic only — no time.Parse, no per-row allocations. The returned
// bool is false for malformed input (callers treat that as neutral).
func dayOrdinal(s string) (int, bool) {
	if len(s) < 10 || s[4] != '-' || s[7] != '-' {
		return 0, false
	}
	y, errY := strconv.Atoi(s[0:4])
	m, errM := strconv.Atoi(s[5:7])
	d, errD := strconv.Atoi(s[8:10])
	if errY != nil || errM != nil || errD != nil {
		return 0, false
	}
	if m < 1 || m > 12 || d < 1 || d > daysInMonth(y, m) {
		return 0, false
	}
	return daysFromCivil(y, m, d), true
}

// daysInMonth returns the day count of a month, honoring leap years.
func daysInMonth(y, m int) int {
	switch m {
	case 1, 3, 5, 7, 8, 10, 12:
		return 31
	case 4, 6, 9, 11:
		return 30
	case 2:
		if y%4 == 0 && (y%100 != 0 || y%400 == 0) {
			return 29
		}
		return 28
	}
	return 0
}

// daysFromCivil maps a civil date to its day ordinal (Howard Hinnant's
// algorithm; day 0 = 1970-01-01). Validated against time.AddDate in
// TestDayOrdinalParityWithTime.
func daysFromCivil(y, m, d int) int {
	if m <= 2 {
		y--
		m += 12
	}
	era := y / 400
	yoe := y - era*400
	doy := (153*(m-3)+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// rerankSearchResults computes the composite score for each FTS result row,
// orders them best-first, and rewrites Rank to the negative composite so the
// stored convention (lower rank = better) stays intact for callers.
// Sorting is stable: ties keep their incoming BM25 order.
func rerankSearchResults(rows []SearchResult, now time.Time) []SearchResult {
	nowDay := dayOrdinalOfTime(now)
	if len(rows) < 2 {
		if len(rows) == 1 {
			rows[0].Rank = -compositeScore(rows[0].Rank, rows[0].UpdatedAt, rows[0].Pinned, rows[0].RevisionCount, nowDay)
		}
		return rows
	}

	type scored struct {
		row   SearchResult
		score float64
	}
	scoredRows := make([]scored, len(rows))
	for i, r := range rows {
		scoredRows[i] = scored{row: r, score: compositeScore(r.Rank, r.UpdatedAt, r.Pinned, r.RevisionCount, nowDay)}
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
