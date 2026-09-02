package store

import (
	"fmt"
	"math/rand"
	"testing"
)

// seedSearchCorpusTB seeds a deterministic observation/prompt corpus for
// benchmarks and query-plan tests. Works with both *testing.T and *testing.B
// via the common testing.TB interface.
func seedSearchCorpusTB(tb testing.TB, s *Store, n int) {
	tb.Helper()

	rng := rand.New(rand.NewSource(42)) //nolint:gosec — deterministic fixture, not crypto
	types := []string{"decision", "bugfix", "pattern", "discovery", "architecture"}
	projects := []string{"engram", "gentle-pi", "cloud"}
	subjects := []string{
		"auth token rotation",
		"fts query sanitization",
		"sync mutation journal",
		"project name drift",
		"session lifecycle cleanup",
	}
	filler := []string{
		"wrapped each search term in quotes before passing to match",
		"generation fence prevents stale reads after replacement",
		"normalize project on every write path to avoid fragmentation",
		"deferred loading keeps eager tool context small",
		"backoff and lease keep push pull loops honest",
	}

	sessionCount := 5
	for i := 0; i < sessionCount; i++ {
		if err := s.CreateSession(fmt.Sprintf("bench-session-%d", i), "engram", tb.TempDir()); err != nil {
			tb.Fatalf("seed session %d: %v", i, err)
		}
	}

	for i := 0; i < n; i++ {
		subject := subjects[rng.Intn(len(subjects))]
		p := AddObservationParams{
			SessionID: fmt.Sprintf("bench-session-%d", rng.Intn(sessionCount)),
			Type:      types[rng.Intn(len(types))],
			Title:     fmt.Sprintf("%s case %d", subject, i),
			Content:   fmt.Sprintf("%s %s %d", subject, filler[rng.Intn(len(filler))], i),
			Project:   projects[rng.Intn(len(projects))],
			Scope:     "project",
		}
		if i%10 == 0 {
			p.TopicKey = fmt.Sprintf("sdd/case-%d/task", i)
		}
		if _, err := s.AddObservation(p); err != nil {
			tb.Fatalf("seed observation %d: %v", i, err)
		}
	}

	for i := 0; i < n/10; i++ {
		prompt := AddPromptParams{
			SessionID: fmt.Sprintf("bench-session-%d", rng.Intn(sessionCount)),
			Content:   fmt.Sprintf("how does %s work case %d", subjects[rng.Intn(len(subjects))], i),
			Project:   projects[rng.Intn(len(projects))],
		}
		if _, err := s.AddPrompt(prompt); err != nil {
			tb.Fatalf("seed prompt %d: %v", i, err)
		}
	}
}

func benchStore(b *testing.B) *Store {
	b.Helper()
	cfg, err := DefaultConfig()
	if err != nil {
		b.Fatalf("DefaultConfig: %v", err)
	}
	cfg.DataDir = b.TempDir()
	cfg.DedupeWindow = 0 // dedupe off: fixture rows share shapes

	s, err := New(cfg)
	if err != nil {
		b.Fatalf("new store: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

func BenchmarkSearchFTS_All(b *testing.B) {
	s := benchStore(b)
	b.StopTimer()
	seedSearchCorpusTB(b, s, 2000)
	b.StartTimer()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Search("auth token rotation", SearchOptions{Project: "engram", Limit: 10}); err != nil {
			b.Fatalf("search: %v", err)
		}
	}
}

func BenchmarkSearchFTS_Any(b *testing.B) {
	s := benchStore(b)
	b.StopTimer()
	seedSearchCorpusTB(b, s, 2000)
	b.StartTimer()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Search("auth token rotation", SearchOptions{Project: "engram", Limit: 10, MatchMode: "any"}); err != nil {
			b.Fatalf("search: %v", err)
		}
	}
}

func BenchmarkSearchTopicKey(b *testing.B) {
	s := benchStore(b)
	b.StopTimer()
	seedSearchCorpusTB(b, s, 2000)
	b.StartTimer()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Search("sdd/case-1000/task", SearchOptions{Project: "engram", Limit: 10}); err != nil {
			b.Fatalf("search: %v", err)
		}
	}
}

func BenchmarkSearchPrompts(b *testing.B) {
	s := benchStore(b)
	b.StopTimer()
	seedSearchCorpusTB(b, s, 2000)
	b.StartTimer()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.SearchPrompts("auth token", "engram", 10); err != nil {
			b.Fatalf("search prompts: %v", err)
		}
	}
}
