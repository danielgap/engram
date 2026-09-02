package store

import (
	"fmt"
	"strings"
	"testing"
)

// TestSearchRelevanceProjectFilterUsesIndex verifies the LOWER(project)
// filters on scan paths (stats, recents, compaction) are sargable through the
// idx_obs_project_lower / idx_prompts_project_lower expression indexes.
// Regression guard: without these indexes every project-filtered query pays
// a full-table scan plus a LOWER() call per row.
// Shares seedSearchCorpusTB with search_bench_test.go (same package).
func TestSearchRelevanceProjectFilterUsesIndex(t *testing.T) {
	s := newTestStore(t)

	seedSearchCorpusTB(t, s, 50)

	cases := []struct {
		name  string
		query string
		args  []any
		want  string
	}{
		{
			name:  "observations count by project",
			query: `SELECT COUNT(*) FROM observations WHERE LOWER(project) = ? AND deleted_at IS NULL`,
			args:  []any{"engram"},
			want:  "idx_obs_project_lower",
		},
		{
			name:  "user_prompts count by project",
			query: `SELECT COUNT(*) FROM user_prompts WHERE LOWER(project) = ?`,
			args:  []any{"engram"},
			want:  "idx_prompts_project_lower",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := s.db.Query("EXPLAIN QUERY PLAN "+tc.query, tc.args...)
			if err != nil {
				t.Fatalf("explain query plan: %v", err)
			}
			defer rows.Close()

			var plan strings.Builder
			for rows.Next() {
				// EXPLAIN QUERY PLAN columns: id, parent, notused, detail.
				var cols [4]any
				if err := rows.Scan(&cols[0], &cols[1], &cols[2], &cols[3]); err != nil {
					t.Fatalf("scan explain row: %v", err)
				}
				fmt.Fprintf(&plan, "%v ", cols[3])
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("explain rows: %v", err)
			}

			if !strings.Contains(plan.String(), tc.want) {
				t.Fatalf("query plan does not use %s:\nplan: %s", tc.want, plan.String())
			}
		})
	}
}

// TestSearchRerankPromotesCuratedFreshMemory is the end-to-end composite
// ranking regression: given two lexically equal observations, the one that is
// recent, pinned, and reinforced must outrank the stale twin.
func TestSearchRerankPromotesCuratedFreshMemory(t *testing.T) {
	s := newTestStore(t)

	if err := s.CreateSession("rerank-session", "engram", t.TempDir()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	staleID, err := s.AddObservation(AddObservationParams{
		SessionID: "rerank-session",
		Type:      "pattern",
		Title:     "composite ranking approach legacy",
		Content:   "composite ranking blends lexical relevance with lifecycle signals",
		Project:   "engram",
		Scope:     "project",
	})
	if err != nil {
		t.Fatalf("add stale observation: %v", err)
	}
	freshID, err := s.AddObservation(AddObservationParams{
		SessionID: "rerank-session",
		Type:      "pattern",
		Title:     "composite ranking approach current",
		Content:   "composite ranking blends lexical relevance with lifecycle signals",
		Project:   "engram",
		Scope:     "project",
	})
	if err != nil {
		t.Fatalf("add fresh observation: %v", err)
	}

	// Backdate the stale twin two years; curate the fresh one: pin + reinforce.
	if _, err := s.execHook(s.db,
		`UPDATE observations SET updated_at = datetime('now', '-730 days') WHERE id = ?`, staleID); err != nil {
		t.Fatalf("backdate stale: %v", err)
	}
	if _, err := s.execHook(s.db,
		`UPDATE observations SET revision_count = 5 WHERE id = ?`, freshID); err != nil {
		t.Fatalf("reinforce fresh: %v", err)
	}
	if err := s.PinObservation(freshID); err != nil {
		t.Fatalf("pin fresh: %v", err)
	}

	results, err := s.Search("composite ranking", SearchOptions{Project: "engram", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("want both observations in results, got %d", len(results))
	}
	if results[0].ID != freshID {
		t.Fatalf("top result = stale ID %d, want curated fresh ID %d", results[0].ID, freshID)
	}
	if results[0].Rank >= results[1].Rank {
		t.Fatalf("curated fresh rank (%v) must beat stale rank (%v): stored Rank is negative composite, more negative = better", results[0].Rank, results[1].Rank)
	}
}

// TestSearchPorterStemming verifies the FTS tables use the porter tokenizer so
// morphological variants match: "configuring" finds "configured" content,
// "rotating" finds "rotated" content. (Porter handles suffixes, not prefixes:
// "auth" matching "authentication" would need explicit prefix queries, out of
// scope for this change.)
func TestSearchPorterStemming(t *testing.T) {
	s := newTestStore(t)

	if err := s.CreateSession("stem-session", "engram", t.TempDir()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	seed := []struct{ title, content string }{
		{"fts tokenizer setup", "the pipeline configured porter stemming for search"},
		{"sync loop pacing", "the push pull loop rotated leadership after each round"},
	}
	for i, obs := range seed {
		if _, err := s.AddObservation(AddObservationParams{
			SessionID: "stem-session",
			Type:      "pattern",
			Title:     obs.title,
			Content:   obs.content,
			Project:   "engram",
			Scope:     "project",
			ToolName:  fmt.Sprintf("seed-%d", i),
		}); err != nil {
			t.Fatalf("seed %q: %v", obs.title, err)
		}
	}

	cases := []struct {
		name  string
		query string
		want  string
	}{
		{name: "gerund finds past tense", query: "configuring", want: "fts tokenizer setup"},
		{name: "gerund finds past tense again", query: "rotating", want: "sync loop pacing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results, err := s.Search(tc.query, SearchOptions{Project: "engram", Limit: 10})
			if err != nil {
				t.Fatalf("search %q: %v", tc.query, err)
			}
			if len(results) == 0 {
				t.Fatalf("search %q returned no results; stemming not active", tc.query)
			}
			if results[0].Title != tc.want {
				t.Fatalf("search %q top = %q, want %q", tc.query, results[0].Title, tc.want)
			}
		})
	}
}

// TestSearchExactTokensStillMatch guards the quoted-token sanitization path:
// after the porter rebuild, identical literal tokens must still match.
func TestSearchExactTokensStillMatch(t *testing.T) {
	s := newTestStore(t)

	if err := s.CreateSession("exact-session", "engram", t.TempDir()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "exact-session",
		Type:      "pattern",
		Title:     "generation fence behavior",
		Content:   "the generation fence prevents stale reads",
		Project:   "engram",
		Scope:     "project",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	results, err := s.Search("generation fence", SearchOptions{Project: "engram", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 || results[0].Title != "generation fence behavior" {
		t.Fatalf("exact-token search broken: got %v", results)
	}
}
