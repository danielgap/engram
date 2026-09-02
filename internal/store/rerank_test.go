package store

import (
	"math"
	"testing"
	"time"
)

var rerankRefTime = time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

// nowDayOf mirrors how rerankSearchResults derives the reference day ordinal.
func nowDayOf(t time.Time) int {
	ordinal, _ := dayOrdinal(t.UTC().Format(sqliteTimeLayout))
	return ordinal
}

func TestCompositeScoreFactors(t *testing.T) {
	const tolerance = 1e-9
	now := rerankRefTime
	nowDay := nowDayOf(rerankRefTime)

	cases := []struct {
		name string
		rank float64
		age  time.Duration
		pin  bool
		rev  int
		want float64
	}{
		{
			name: "fresh unpinned no revisions is pure relevance",
			rank: -4.0, age: 0, pin: false, rev: 0,
			want: 4.0 * 1.0 * 1.0 * 1.0,
		},
		{
			name: "one half-life old decays to floor blend midpoint",
			rank: -4.0, age: 90 * 24 * time.Hour, pin: false, rev: 0,
			want: 4.0 * (0.35 + 0.65*0.5),
		},
		{
			name: "ancient memory keeps floor weight",
			rank: -4.0, age: 10 * 365 * 24 * time.Hour, pin: false, rev: 0,
			want: 4.0 * (0.35 + 0.65*math.Pow(2, -float64(10*365)/90)),
		},
		{
			name: "pinned multiplies by 1.5",
			rank: -4.0, age: 0, pin: true, rev: 0,
			want: 4.0 * 1.0 * 1.5 * 1.0,
		},
		{
			name: "revision reinforcement caps at five",
			rank: -4.0, age: 0, pin: false, rev: 5,
			want: 4.0 * 1.0 * 1.0 * 1.25,
		},
		{
			name: "revision count above five adds nothing",
			rank: -4.0, age: 0, pin: false, rev: 50,
			want: 4.0 * 1.0 * 1.0 * 1.25,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updatedAt := now.Add(-tc.age).UTC().Format(sqliteTimeLayout)
			got := compositeScore(tc.rank, updatedAt, tc.pin, tc.rev, nowDay)
			if math.Abs(got-tc.want) > tolerance {
				t.Fatalf("compositeScore(%v, %v, %v, %d) = %v, want %v", tc.rank, updatedAt, tc.pin, tc.rev, got, tc.want)
			}
		})
	}
}

func TestCompositeScoreNeutralOnUnparseableTimestamp(t *testing.T) {
	cases := []string{"", "not-a-timestamp", "2026-13-45 99:99:99"}
	for _, ts := range cases {
		t.Run("timestamp="+ts, func(t *testing.T) {
			got := compositeScore(-4.0, ts, false, 0, nowDayOf(rerankRefTime))
			if math.Abs(got-4.0) > 1e-9 {
				t.Fatalf("compositeScore with unparseable %q = %v, want neutral 4.0", ts, got)
			}
		})
	}
}

func TestCompositeScoreFreshBeatsStaleAtEqualRelevance(t *testing.T) {
	now := rerankRefTime
	nowDay := nowDayOf(now)
	fresh := compositeScore(-4.0, now.Add(-24*time.Hour).UTC().Format(sqliteTimeLayout), false, 0, nowDay)
	stale := compositeScore(-4.0, now.Add(-730*24*time.Hour).UTC().Format(sqliteTimeLayout), false, 0, nowDay)
	if fresh <= stale {
		t.Fatalf("fresh (%v) must outrank stale (%v)", fresh, stale)
	}
}

func TestCompositeScorePinnedFreshBeatsUnpinnedStale(t *testing.T) {
	now := rerankRefTime
	nowDay := nowDayOf(now)
	pinnedFresh := compositeScore(-4.0, now.Add(-24*time.Hour).UTC().Format(sqliteTimeLayout), true, 5, nowDay)
	stale := compositeScore(-9.0, now.Add(-730*24*time.Hour).UTC().Format(sqliteTimeLayout), false, 0, nowDay)
	if pinnedFresh <= stale {
		t.Fatalf("pinned fresh (%v) must outrank lexically stronger stale (%v)", pinnedFresh, stale)
	}
}

func TestRerankSearchResultsOrdersAndRanks(t *testing.T) {
	now := rerankRefTime
	freshTS := now.Add(-1 * 24 * time.Hour).UTC().Format(sqliteTimeLayout)
	staleTS := now.Add(-730 * 24 * time.Hour).UTC().Format(sqliteTimeLayout)

	rows := []SearchResult{
		{Observation: Observation{ID: 1, Title: "stale lexically stronger", UpdatedAt: staleTS, Pinned: false, RevisionCount: 0}, Rank: -8.0},
		{Observation: Observation{ID: 2, Title: "fresh pinned reinforced", UpdatedAt: freshTS, Pinned: true, RevisionCount: 4}, Rank: -4.0},
		{Observation: Observation{ID: 3, Title: "stale lexically equal", UpdatedAt: staleTS, Pinned: false, RevisionCount: 0}, Rank: -4.0},
	}

	got := rerankSearchResults(rows, now)

	if len(got) != 3 {
		t.Fatalf("rerank changed result count: got %d, want 3", len(got))
	}
	if got[0].ID != 2 {
		t.Fatalf("top result = ID %d, want 2 (fresh pinned reinforced)", got[0].ID)
	}
	if got[1].ID != 1 {
		t.Fatalf("second result = ID %d, want 1 (equal-age rows keep lexical order: stronger BM25 first)", got[1].ID)
	}
	if got[2].ID != 3 {
		t.Fatalf("third result = ID %d, want 3 (weakest composite)", got[2].ID)
	}
	for _, r := range got {
		if r.Rank >= 0 {
			t.Fatalf("reranked Rank must stay a bm25-style negative composite, got %v for ID %d", r.Rank, r.ID)
		}
	}
}

func TestRerankSearchResultsStableForTies(t *testing.T) {
	now := rerankRefTime
	ts := now.Add(-24 * time.Hour).UTC().Format(sqliteTimeLayout)
	rows := []SearchResult{
		{Observation: Observation{ID: 10, UpdatedAt: ts}, Rank: -4.0},
		{Observation: Observation{ID: 11, UpdatedAt: ts}, Rank: -4.0},
		{Observation: Observation{ID: 12, UpdatedAt: ts}, Rank: -4.0},
	}
	got := rerankSearchResults(rows, now)
	for i, r := range got {
		if r.ID != int64(10+i) {
			t.Fatalf("tie order not stable: position %d = ID %d", i, r.ID)
		}
	}
}

// TestDayOrdinalFromSQLite pins the parse-free date arithmetic: fixed-width
// digit extraction, boundary rollovers, and rejection of malformed input.
func TestDayOrdinalFromSQLite(t *testing.T) {
	cases := []struct {
		name   string
		value  string
		want   int
		wantOK bool
	}{
		{name: "canonical", value: "2026-07-09 12:34:56", wantOK: true},
		{name: "time of day ignored", value: "2026-07-09 00:00:01", wantOK: true},
		{name: "empty", value: "", wantOK: false},
		{name: "garbage", value: "not-a-timestamp", wantOK: false},
		{name: "too short", value: "2026", wantOK: false},
		{name: "invalid month", value: "2026-13-01 00:00:00", wantOK: false},
		{name: "invalid day", value: "2026-02-30 00:00:00", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := dayOrdinal(tc.value)
			if ok != tc.wantOK {
				t.Fatalf("dayOrdinal(%q) ok = %v, want %v", tc.value, ok, tc.wantOK)
			}
			if ok && tc.want != 0 && got != tc.want {
				t.Fatalf("dayOrdinal(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}

	// Boundary diffs must be exactly one day.
	dayMinus1 := "2025-12-31 23:59:59"
	dayPlus1 := "2026-01-01 00:00:00"
	a, _ := dayOrdinal(dayMinus1)
	b, _ := dayOrdinal(dayPlus1)
	if b-a != 1 {
		t.Fatalf("year boundary: ordinal diff = %d, want 1", b-a)
	}

	leapA, _ := dayOrdinal("2024-02-28 12:00:00")
	leapB, _ := dayOrdinal("2024-02-29 12:00:00")
	leapC, _ := dayOrdinal("2024-03-01 12:00:00")
	if leapB-leapA != 1 || leapC-leapB != 1 {
		t.Fatalf("leap boundaries: 28->29 = %d, 29->01 = %d, want 1 and 1", leapB-leapA, leapC-leapB)
	}
}

// TestDayOrdinalParityWithTime cross-checks the arithmetic ordinals against
// time.AddDate across a wide date range: differences in whole days must match.
func TestDayOrdinalParityWithTime(t *testing.T) {
	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	base, ok := dayOrdinal(start.Format(sqliteTimeLayout))
	if !ok {
		t.Fatal("base ordinal")
	}
	for offset := 0; offset <= 1500; offset++ {
		want := base + offset
		got, ok := dayOrdinal(start.AddDate(0, 0, offset).Format(sqliteTimeLayout))
		if !ok || got != want {
			t.Fatalf("offset %d: got %d (ok=%v), want %d", offset, got, ok, want)
		}
	}
}
