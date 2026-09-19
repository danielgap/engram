package diagnostic

import (
	"reflect"
	"strings"
	"testing"
)

func TestAcknowledgeableCodesContents(t *testing.T) {
	want := []string{CheckAmbiguousActiveRuntimeSessions, CheckOrphanedObservationSession}
	got := AcknowledgeableCodes()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AcknowledgeableCodes()=%v, want %v", got, want)
	}
	registered := make(map[string]bool)
	for _, code := range RegisteredCodes() {
		registered[code] = true
	}
	for _, code := range got {
		if !registered[code] {
			t.Fatalf("acknowledgeable code %q is not a registered check", code)
		}
	}
}

func TestIsAcknowledgeableCodeNegatives(t *testing.T) {
	cases := []struct {
		code string
		want bool
	}{
		{code: CheckOrphanedObservationSession, want: true},
		{code: CheckAmbiguousActiveRuntimeSessions, want: true},
		{code: "  " + CheckOrphanedObservationSession + "  ", want: true},
		// Repairable checks must stay honest: their exit is repair.
		{code: CheckSyncMutationRequiredFields, want: false},
		{code: CheckOrphanedObservationSession + "-nope", want: false},
		// Diagnostic-only does not mean acknowledgeable by default.
		{code: CheckSQLiteLockContention, want: false},
		{code: CheckUnownedSessionProject, want: false},
		{code: "", want: false},
		{code: "   ", want: false},
		{code: "unknown_check", want: false},
	}
	for _, tc := range cases {
		if got := IsAcknowledgeableCode(tc.code); got != tc.want {
			t.Fatalf("IsAcknowledgeableCode(%q)=%v, want %v", tc.code, got, tc.want)
		}
	}
	if strings.Join(AcknowledgeableCodes(), ",") == "" {
		t.Fatalf("AcknowledgeableCodes() must never be empty")
	}
}
