package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Gentleman-Programming/engram/v2/internal/store"
)

// ─── session end parser (issue #1247, #1084 guarantee) ───────────────────────

func TestParseSessionEndArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    sessionEndArgs
		wantErr string
	}{
		{
			name: "single session id",
			args: []string{"sess-1"},
			want: sessionEndArgs{sessionID: "sess-1"},
		},
		{
			name: "single session id with summary",
			args: []string{"sess-1", "--summary", "done"},
			want: sessionEndArgs{sessionID: "sess-1", summary: "done", hasSummary: true},
		},
		{
			name: "single session id with json",
			args: []string{"sess-1", "--json"},
			want: sessionEndArgs{sessionID: "sess-1", jsonOut: true},
		},
		{
			name:    "no session id",
			args:    []string{},
			wantErr: "session ID",
		},
		{
			name:    "unknown flag",
			args:    []string{"sess-1", "--sumary", "x"},
			wantErr: "unknown flag",
		},
		{
			name:    "extra positional argument",
			args:    []string{"sess-1", "sess-2"},
			wantErr: "unexpected extra argument",
		},
		{
			name:    "--summary missing value",
			args:    []string{"sess-1", "--summary"},
			wantErr: "--summary requires a value",
		},
		{
			name:    "--summary value that looks like a flag",
			args:    []string{"sess-1", "--summary", "--json"},
			wantErr: "--summary requires a value",
		},
		{
			name:    "--summary empty value",
			args:    []string{"sess-1", "--summary", ""},
			wantErr: "--summary requires a value",
		},
		{
			name:    "--summary whitespace-only value",
			args:    []string{"sess-1", "--summary", "   "},
			wantErr: "--summary requires a value",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSessionEndArgs(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseSessionEndArgs(%v) error = %v, want containing %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSessionEndArgs(%v): %v", tc.args, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseSessionEndArgs(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// ─── session end command behavior ────────────────────────────────────────────

// seedEndedSession ends a session through the canonical store contract so the
// CLI tests observe the same terminal state a real end produces.
func seedEndedSession(t *testing.T, cfg store.Config, sessionID, project string) {
	t.Helper()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := s.CreateSession(sessionID, project, "/tmp"); err != nil {
		_ = s.Close()
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.EndSession(sessionID, ""); err != nil {
		_ = s.Close()
		t.Fatalf("EndSession: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

// sessionEndSeamSpy records every injectable-seam call made by one command
// run and scripts the outcomes the seams report.
type sessionEndSeamSpy struct {
	storeOpened   bool
	getSessionIDs []string
	endCalls      []string
	store         *store.Store // optional real store handed out by storeNew
	session       *store.Session
	getErr        error
	endErr        error
}

// withSessionEndStoreSpy stubs both injectable session-end seams and arms
// storeNew, so tests can prove exactly when the command touches the store and
// script what it observes.
func withSessionEndStoreSpy(t *testing.T, spy *sessionEndSeamSpy) {
	t.Helper()
	oldNew := storeNew
	storeNew = func(cfg store.Config) (*store.Store, error) {
		spy.storeOpened = true
		if spy.store != nil {
			return spy.store, nil
		}
		return nil, errors.New("store must not be opened for this invocation")
	}
	t.Cleanup(func() { storeNew = oldNew })

	oldGet := storeGetSession
	storeGetSession = func(s *store.Store, id string) (*store.Session, error) {
		spy.getSessionIDs = append(spy.getSessionIDs, id)
		return spy.session, spy.getErr
	}
	t.Cleanup(func() { storeGetSession = oldGet })

	oldEnd := storeEndSession
	storeEndSession = func(s *store.Store, id, summary string) error {
		spy.endCalls = append(spy.endCalls, id)
		return spy.endErr
	}
	t.Cleanup(func() { storeEndSession = oldEnd })
}

func TestCmdSessionEndSingle(t *testing.T) {
	t.Run("ends an open session", func(t *testing.T) {
		cfg := testConfig(t)
		mustSeedSession(t, cfg, "sess-end-1", "proj-end")

		exited := stubbedExit(t)
		withArgs(t, "engram", "session", "end", "sess-end-1")
		stdout, stderr := captureOutput(t, func() { cmdSession(cfg) })
		if stderr != "" {
			t.Fatalf("expected no stderr, got: %q", stderr)
		}
		if len(*exited) != 0 {
			t.Fatalf("expected exit code 0 (no exitFunc call), got codes %v", *exited)
		}
		if !strings.Contains(stdout, `Session "sess-end-1" ended`) {
			t.Fatalf("expected end confirmation in stdout, got: %q", stdout)
		}
		if got := mustQueryCount(t, cfg, `SELECT COUNT(*) FROM sessions WHERE id = ? AND ended_at IS NOT NULL`, "sess-end-1"); got != 1 {
			t.Fatalf("ended rows = %d, want 1", got)
		}
	})

	t.Run("ends an open session with a summary", func(t *testing.T) {
		cfg := testConfig(t)
		mustSeedSession(t, cfg, "sess-end-summary", "proj-end")

		exited := stubbedExit(t)
		withArgs(t, "engram", "session", "end", "sess-end-summary", "--summary", "shipped the slice")
		stdout, stderr := captureOutput(t, func() { cmdSession(cfg) })
		if stderr != "" {
			t.Fatalf("expected no stderr, got: %q", stderr)
		}
		if len(*exited) != 0 {
			t.Fatalf("expected exit code 0 (no exitFunc call), got codes %v", *exited)
		}
		if !strings.Contains(stdout, `Session "sess-end-summary" ended`) {
			t.Fatalf("expected end confirmation in stdout, got: %q", stdout)
		}
		db, err := sql.Open("sqlite", filepath.Join(cfg.DataDir, "engram.db"))
		if err != nil {
			t.Fatalf("open database: %v", err)
		}
		defer func() { _ = db.Close() }()
		var summary *string
		if err := db.QueryRow(`SELECT summary FROM sessions WHERE id = ?`, "sess-end-summary").Scan(&summary); err != nil {
			t.Fatalf("read summary: %v", err)
		}
		if summary == nil || *summary != "shipped the slice" {
			t.Fatalf("summary = %v, want %q", summary, "shipped the slice")
		}
	})

	t.Run("reports already ended without failing", func(t *testing.T) {
		cfg := testConfig(t)
		seedEndedSession(t, cfg, "sess-end-2", "proj-end")

		exited := stubbedExit(t)
		withArgs(t, "engram", "session", "end", "sess-end-2")
		stdout, stderr := captureOutput(t, func() { cmdSession(cfg) })
		if stderr != "" {
			t.Fatalf("expected no stderr, got: %q", stderr)
		}
		if !strings.Contains(stdout, `Session "sess-end-2" already ended`) {
			t.Fatalf("expected already-ended notice in stdout, got: %q", stdout)
		}
		if len(*exited) != 0 {
			t.Fatalf("expected exit code 0 (no exitFunc call), got codes %v", *exited)
		}
	})

	t.Run("fails on unknown session id", func(t *testing.T) {
		cfg := testConfig(t)

		exited := stubbedExit(t)
		withArgs(t, "engram", "session", "end", "no-such-session")
		_, stderr := captureOutput(t, func() { cmdSession(cfg) })
		if len(*exited) != 1 || (*exited)[0] != 1 {
			t.Fatalf("exit codes = %v, want [1]", *exited)
		}
		if !strings.Contains(stderr, `error: session "no-such-session" not found`) {
			t.Fatalf("expected not-found error in stderr, got: %q", stderr)
		}
	})

	t.Run("busy end fails with the store message", func(t *testing.T) {
		cfg := testConfig(t)
		realStore, err := store.New(cfg)
		if err != nil {
			t.Fatalf("store.New: %v", err)
		}
		t.Cleanup(func() { _ = realStore.Close() })

		spy := &sessionEndSeamSpy{
			store:   realStore,
			session: &store.Session{ID: "sess-busy"},
			endErr:  store.ErrSessionBusy,
		}
		withSessionEndStoreSpy(t, spy)

		exited := stubbedExit(t)
		withArgs(t, "engram", "session", "end", "sess-busy")
		_, stderr := captureOutput(t, func() { cmdSession(cfg) })
		if len(*exited) != 1 || (*exited)[0] != 1 {
			t.Fatalf("exit codes = %v, want [1]", *exited)
		}
		if !strings.Contains(stderr, "session closure is temporarily blocked") {
			t.Fatalf("expected busy message in stderr, got: %q", stderr)
		}
		if len(spy.endCalls) != 1 || spy.endCalls[0] != "sess-busy" {
			t.Fatalf("end seam calls = %v, want exactly [sess-busy]", spy.endCalls)
		}
		if len(spy.getSessionIDs) != 1 {
			t.Fatalf("get seam calls = %v, want exactly the pre-check read", spy.getSessionIDs)
		}
	})

	t.Run("json output reports id status and ended_at", func(t *testing.T) {
		cfg := testConfig(t)
		mustSeedSession(t, cfg, "sess-end-json", "proj-end")

		withArgs(t, "engram", "session", "end", "sess-end-json", "--json")
		stdout, stderr := captureOutput(t, func() { cmdSession(cfg) })
		if stderr != "" {
			t.Fatalf("expected no stderr, got: %q", stderr)
		}
		var payload struct {
			ID      string  `json:"id"`
			Status  string  `json:"status"`
			EndedAt *string `json:"ended_at"`
		}
		if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
			t.Fatalf("invalid single-end JSON %q: %v", stdout, err)
		}
		if payload.ID != "sess-end-json" || payload.Status != "ended" || payload.EndedAt == nil || *payload.EndedAt == "" {
			t.Fatalf("single-end JSON = %+v", payload)
		}
	})

	t.Run("json output for already ended reports original ended_at", func(t *testing.T) {
		cfg := testConfig(t)
		seedEndedSession(t, cfg, "sess-end-json2", "proj-end")
		// Backdate so the JSON must carry the ORIGINAL ended_at, not a fresh one.
		db, err := sql.Open("sqlite", filepath.Join(cfg.DataDir, "engram.db"))
		if err != nil {
			t.Fatalf("open database: %v", err)
		}
		if _, err := db.Exec(`UPDATE sessions SET ended_at = '2020-01-02 03:04:05' WHERE id = ?`, "sess-end-json2"); err != nil {
			_ = db.Close()
			t.Fatalf("backdate ended_at: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close database: %v", err)
		}

		withArgs(t, "engram", "session", "end", "sess-end-json2", "--json")
		stdout, stderr := captureOutput(t, func() { cmdSession(cfg) })
		if stderr != "" {
			t.Fatalf("expected no stderr, got: %q", stderr)
		}
		var payload2 struct {
			ID      string  `json:"id"`
			Status  string  `json:"status"`
			EndedAt *string `json:"ended_at"`
		}
		if err := json.Unmarshal([]byte(stdout), &payload2); err != nil {
			t.Fatalf("invalid single-end JSON %q: %v", stdout, err)
		}
		if payload2.ID != "sess-end-json2" || payload2.Status != "already_ended" || payload2.EndedAt == nil || *payload2.EndedAt != "2020-01-02 03:04:05" {
			t.Fatalf("single-end JSON = %+v, want already_ended with original ended_at", payload2)
		}
	})

	t.Run("unknown id fails even with json", func(t *testing.T) {
		cfg := testConfig(t)

		exited := stubbedExit(t)
		withArgs(t, "engram", "session", "end", "no-such-json", "--json")
		stdout, stderr := captureOutput(t, func() { cmdSession(cfg) })
		if len(*exited) != 1 || (*exited)[0] != 1 {
			t.Fatalf("exit codes = %v, want [1]", *exited)
		}
		if !strings.Contains(stderr, `not found`) {
			t.Fatalf("expected not-found error in stderr, got: %q", stderr)
		}
		if strings.Contains(stdout, "{") {
			t.Fatalf("expected no JSON on stdout for the fatal path, got: %q", stdout)
		}
	})
}

func TestCmdSessionEndRejectsInvalidInvocationsBeforeStoreOpen(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "unknown flag", args: []string{"end", "sess-1", "--sumary", "x"}, wantErr: "unknown flag"},
		{name: "summary missing value", args: []string{"end", "sess-1", "--summary"}, wantErr: "--summary requires a value"},
		{name: "summary value that looks like a flag", args: []string{"end", "sess-1", "--summary", "--json"}, wantErr: "--summary requires a value"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			spy := &sessionEndSeamSpy{}
			withSessionEndStoreSpy(t, spy)
			exited := stubbedExit(t)

			withArgs(t, append([]string{"engram", "session"}, tc.args...)...)
			_, stderr := captureOutput(t, func() { cmdSession(cfg) })
			if len(*exited) != 1 || (*exited)[0] != 1 {
				t.Fatalf("exit codes = %v, want [1]", *exited)
			}
			if !strings.Contains(stderr, tc.wantErr) {
				t.Fatalf("expected %q in stderr, got: %q", tc.wantErr, stderr)
			}
			if !strings.Contains(stderr, "usage: engram session end") {
				t.Fatalf("expected usage in stderr, got: %q", stderr)
			}
			if spy.storeOpened {
				t.Fatal("storeNew must not be called for an invalid invocation")
			}
			if len(spy.getSessionIDs) != 0 || len(spy.endCalls) != 0 {
				t.Fatalf("store seams must not be called for an invalid invocation, got get=%v end=%v", spy.getSessionIDs, spy.endCalls)
			}
		})
	}
}

func TestCmdSessionDispatch(t *testing.T) {
	t.Run("unknown session subcommand is rejected", func(t *testing.T) {
		cfg := testConfig(t)

		exited := stubbedExit(t)
		withArgs(t, "engram", "session", "bogus")
		_, stderr := captureOutput(t, func() { cmdSession(cfg) })
		if len(*exited) != 1 || (*exited)[0] != 1 {
			t.Fatalf("exit codes = %v, want [1]", *exited)
		}
		if !strings.Contains(stderr, "unknown session command") {
			t.Fatalf("expected unknown-subcommand error in stderr, got: %q", stderr)
		}
	})

	t.Run("missing session subcommand prints usage", func(t *testing.T) {
		cfg := testConfig(t)

		exited := stubbedExit(t)
		withArgs(t, "engram", "session")
		_, stderr := captureOutput(t, func() { cmdSession(cfg) })
		if len(*exited) != 1 || (*exited)[0] != 1 {
			t.Fatalf("exit codes = %v, want [1]", *exited)
		}
		if !strings.Contains(stderr, "usage: engram session") {
			t.Fatalf("expected usage in stderr, got: %q", stderr)
		}
	})
}

func TestSessionEndInUsage(t *testing.T) {
	stdout, _ := captureOutput(t, func() { printUsage() })
	if !strings.Contains(stdout, "session end <id>") {
		t.Fatalf("expected %q in usage output, got:\n%s", "session end <id>", stdout)
	}
}
