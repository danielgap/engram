package store

import (
	"testing"
)

// TestDecayExtendedTypesGetReviewAfter pins the Phase 2 decay map: bugfix,
// discovery, pattern, and architecture observations must enter the review
// lifecycle on insert. Types without a configured offset (manual) keep the
// Phase 1 NULL behavior.
func TestDecayExtendedTypesGetReviewAfter(t *testing.T) {
	s := newTestStore(t)

	if err := s.CreateSession("decay-session", "engram", t.TempDir()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	cases := []struct {
		typ      string
		wantNULL bool
	}{
		{typ: "bugfix"},
		{typ: "discovery"},
		{typ: "pattern"},
		{typ: "architecture"},
		{typ: "decision"},   // Phase 1 coverage must survive
		{typ: "preference"}, // Phase 1 coverage must survive
		{typ: "manual", wantNULL: true},
	}

	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			id, err := s.AddObservation(AddObservationParams{
				SessionID: "decay-session",
				Type:      tc.typ,
				Title:     "decay probe " + tc.typ,
				Content:   "content for " + tc.typ,
				Project:   "engram",
				Scope:     "project",
			})
			if err != nil {
				t.Fatalf("AddObservation(%s): %v", tc.typ, err)
			}

			var reviewAfter *string
			if err := s.db.QueryRow(
				`SELECT review_after FROM observations WHERE id = ?`, id,
			).Scan(&reviewAfter); err != nil {
				t.Fatalf("query review_after: %v", err)
			}
			if tc.wantNULL {
				if reviewAfter != nil {
					t.Fatalf("type %q must keep NULL review_after, got %q", tc.typ, *reviewAfter)
				}
				return
			}
			if reviewAfter == nil {
				t.Fatalf("type %q must enter the review lifecycle (non-NULL review_after)", tc.typ)
			}
		})
	}
}
