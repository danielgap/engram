package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// ─── D2: cloud inbox routing alias map (schema v2) ──────────────────────────

func TestSyncProjectAliasTableMigratedWithSchemaV2(t *testing.T) {
	s := newTestStore(t)
	var table string
	if err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'sync_project_aliases'`,
	).Scan(&table); err != nil {
		t.Fatalf("sync_project_aliases table missing after migration: %v", err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version = %d, want %d", version, schemaVersion)
	}
	if schemaVersion < 2 {
		t.Fatalf("schemaVersion = %d, want >= 2 for the inbox routing alias map", schemaVersion)
	}
}

func TestRegisterSyncProjectAliasRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterSyncProjectAlias("old-name", "new-project"); err != nil {
		t.Fatalf("register alias: %v", err)
	}
	if err := s.withTx(func(tx *sql.Tx) error {
		resolved, err := s.lookupSyncProjectAliasTx(tx, "old-name")
		if err != nil {
			return err
		}
		if resolved != "new-project" {
			t.Fatalf("alias lookup = %q, want %q", resolved, "new-project")
		}
		return nil
	}); err != nil {
		t.Fatalf("alias lookup: %v", err)
	}
}

func TestRegisterSyncProjectAliasRejectsReservedInboxTarget(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterSyncProjectAlias("some-key", ReservedInboxProjectName); err == nil {
		t.Fatal("register alias targeting the reserved inbox project name, want error")
	}
}

func TestRegisterSyncProjectAliasNormalizesAndUpserts(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterSyncProjectAlias("  Old-Name  ", "new-project"); err != nil {
		t.Fatalf("register alias: %v", err)
	}
	if err := s.RegisterSyncProjectAlias("old-name", "  Renamed_Project  "); err != nil {
		t.Fatalf("re-register alias: %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sync_project_aliases WHERE alias_kind = ?`, syncProjectAliasKindKey).Scan(&count); err != nil {
		t.Fatalf("count alias rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("alias rows = %d, want 1 (re-registration upserts)", count)
	}
	if err := s.withTx(func(tx *sql.Tx) error {
		resolved, err := s.lookupSyncProjectAliasTx(tx, "old-name")
		if err != nil {
			return err
		}
		if resolved != "renamed_project" {
			t.Fatalf("alias target = %q, want %q", resolved, "renamed_project")
		}
		return nil
	}); err != nil {
		t.Fatalf("lookup alias: %v", err)
	}
}

func TestLookupSyncProjectAliasMissReturnsEmpty(t *testing.T) {
	s := newTestStore(t)
	if err := s.withTx(func(tx *sql.Tx) error {
		resolved, err := s.lookupSyncProjectAliasTx(tx, "never-registered")
		if err != nil {
			return err
		}
		if resolved != "" {
			t.Fatalf("unknown alias resolved to %q, want empty", resolved)
		}
		return nil
	}); err != nil {
		t.Fatalf("lookup unknown alias: %v", err)
	}
}

func TestRegisterSyncProjectCwdRoundTrip(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	if err := s.RegisterSyncProjectCwd(dir, "cwd-project"); err != nil {
		t.Fatalf("register cwd: %v", err)
	}
	if err := s.RegisterSyncProjectCwd(dir+"/", "cwd-project"); err != nil {
		t.Fatalf("re-register cwd with trailing separator: %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sync_project_aliases WHERE alias_kind = ?`, syncProjectAliasKindCwd).Scan(&count); err != nil {
		t.Fatalf("count cwd rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("cwd rows = %d, want 1 (same canonical cwd upserts)", count)
	}
	if err := s.withTx(func(tx *sql.Tx) error {
		resolved, err := s.lookupSyncProjectCwdTx(tx, dir)
		if err != nil {
			return err
		}
		if resolved != "cwd-project" {
			t.Fatalf("cwd lookup = %q, want %q", resolved, "cwd-project")
		}
		return nil
	}); err != nil {
		t.Fatalf("lookup cwd: %v", err)
	}
}

func TestRegisterSyncProjectCwdRejectsEmptyAndReserved(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterSyncProjectCwd("", "project"); err == nil {
		t.Fatal("register empty cwd, want error")
	}
	if err := s.RegisterSyncProjectCwd("   ", "project"); err == nil {
		t.Fatal("register blank cwd, want error")
	}
	if err := s.RegisterSyncProjectCwd(t.TempDir(), ""); err == nil {
		t.Fatal("register cwd with empty project, want error")
	}
	if err := s.RegisterSyncProjectCwd(t.TempDir(), ReservedInboxProjectName); err == nil {
		t.Fatal("register cwd targeting the reserved inbox project name, want error")
	}
}

// ─── D1/D3: two-stage routing at enqueue ────────────────────────────────────

// initGitRepo creates a git repository with a controlled directory name so
// slug derivation is deterministic.
func initGitRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	cmd := exec.Command("git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v: %s", dir, err, out)
	}
	return dir
}

func countSyncMutationsWhere(t *testing.T, s *Store, where string, args ...any) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sync_mutations WHERE `+where, args...).Scan(&count); err != nil {
		t.Fatalf("count sync_mutations: %v", err)
	}
	return count
}

func TestUnenrolledObservationJournalsToCloudInbox(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("inbox-obs-session", "unenrolled-proj", "/tmp/inbox-obs"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	obsID, err := s.AddObservation(AddObservationParams{
		SessionID: "inbox-obs-session",
		Type:      "decision",
		Title:     "inbox routing",
		Content:   "local observation",
		Project:   "unenrolled-proj",
		Scope:     "project",
	})
	if err != nil {
		t.Fatalf("add observation: %v", err)
	}
	if got := countSyncMutationsWhere(t, s, `target_key = ?`, DefaultSyncTargetKey); got != 0 {
		t.Fatalf("legacy cloud rows for unenrolled observation = %d, want 0", got)
	}
	if got := countSyncMutationsWhere(t, s, `target_key = ?`, SyncInboxTargetKey); got != 1 {
		t.Fatalf("inbox rows = %d, want 1", got)
	}
	var entity, op, source, project, payload string
	if err := s.db.QueryRow(`SELECT entity, op, source, project, payload FROM sync_mutations WHERE target_key = ?`, SyncInboxTargetKey).
		Scan(&entity, &op, &source, &project, &payload); err != nil {
		t.Fatalf("read inbox mutation: %v", err)
	}
	if entity != SyncEntityObservation || op != SyncOpUpsert || source != SyncSourceLocal {
		t.Fatalf("inbox mutation = %s/%s/%s, want observation/upsert/local", entity, op, source)
	}
	if project != "unenrolled-proj" {
		t.Fatalf("inbox project column = %q, want %q", project, "unenrolled-proj")
	}
	var decoded struct {
		SyncID   string `json:"sync_id"`
		Title    string `json:"title"`
		Project  string `json:"project"`
		Original bool   `json:"original"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("decode inbox payload: %v", err)
	}
	if decoded.Title != "inbox routing" || decoded.Project != "unenrolled-proj" {
		t.Fatalf("payload was rewritten: %s", payload)
	}
	var syncID string
	if err := s.db.QueryRow(`SELECT sync_id FROM observations WHERE id = ?`, obsID).Scan(&syncID); err != nil {
		t.Fatalf("read observation sync id: %v", err)
	}
	if decoded.SyncID != syncID {
		t.Fatalf("payload sync_id = %q, want %q", decoded.SyncID, syncID)
	}
	// No unenrolled per-project state may be minted, and the inbox lifecycle
	// row must stay untouched.
	var minted int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sync_state WHERE target_key = ?`, syncTargetKeyForProject("unenrolled-proj")).Scan(&minted); err != nil {
		t.Fatal(err)
	}
	if minted != 0 {
		t.Fatal("unenrolled project state row was minted by inbox routing")
	}
	state, err := s.GetSyncState(SyncInboxTargetKey)
	if err != nil {
		t.Fatalf("read inbox state: %v", err)
	}
	if state.Lifecycle != SyncLifecycleInbox {
		t.Fatalf("inbox lifecycle = %q, want %q", state.Lifecycle, SyncLifecycleInbox)
	}
}

func TestUnenrolledTelemetryDoesNotJournalAtAll(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("telemetry-session", "telemetry-proj", "/tmp/telemetry"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := s.AddPrompt(AddPromptParams{SessionID: "telemetry-session", Content: "local prompt", Project: "telemetry-proj"}); err != nil {
		t.Fatalf("add prompt: %v", err)
	}
	if got := countSyncMutationsWhere(t, s, `1=1`); got != 0 {
		t.Fatalf("unenrolled telemetry journaled %d mutations, want 0", got)
	}
}

func TestSessionProjectFallbackRoutesToInbox(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.db.Exec(`INSERT INTO sessions (id, project, directory) VALUES ('fallback-session', 'sess-proj', '/tmp/fallback')`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	err := s.withTx(func(tx *sql.Tx) error {
		return s.enqueueSyncMutationTx(tx, SyncEntityObservation, "fallback-obs", SyncOpUpsert, syncObservationPayload{
			SyncID:    "fallback-obs",
			SessionID: "fallback-session",
			Type:      "decision",
			Title:     "fallback",
			Content:   "payload carries no project",
			Scope:     "project",
		})
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var project string
	if err := s.db.QueryRow(`SELECT project FROM sync_mutations WHERE target_key = ? AND entity_key = ?`, SyncInboxTargetKey, "fallback-obs").Scan(&project); err != nil {
		t.Fatalf("read inbox mutation: %v", err)
	}
	if project != "sess-proj" {
		t.Fatalf("inbox project = %q, want session-derived %q", project, "sess-proj")
	}
}

func TestGitRootSlugRoutesUnenrolledToInbox(t *testing.T) {
	s := newTestStore(t)
	repoDir := initGitRepo(t, "My_Repo Name")
	if _, err := s.db.Exec(`INSERT INTO sessions (id, project, directory) VALUES ('slug-session', '', ?)`, repoDir); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	err := s.withTx(func(tx *sql.Tx) error {
		return s.enqueueSyncMutationTx(tx, SyncEntityObservation, "slug-obs", SyncOpUpsert, syncObservationPayload{
			SyncID:    "slug-obs",
			SessionID: "slug-session",
			Type:      "decision",
			Title:     "slug",
			Content:   "no explicit project anywhere",
			Scope:     "project",
		})
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var project string
	if err := s.db.QueryRow(`SELECT project FROM sync_mutations WHERE target_key = ? AND entity_key = ?`, SyncInboxTargetKey, "slug-obs").Scan(&project); err != nil {
		t.Fatalf("read inbox mutation: %v", err)
	}
	if project != "my-repo-name" {
		t.Fatalf("inbox project = %q, want git-root slug %q", project, "my-repo-name")
	}
}

func TestNonRepoDirectoryRoutesUnsortedToInbox(t *testing.T) {
	s := newTestStore(t)
	plainDir := t.TempDir()
	if _, err := s.db.Exec(`INSERT INTO sessions (id, project, directory) VALUES ('unsorted-session', '', ?)`, plainDir); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	err := s.withTx(func(tx *sql.Tx) error {
		return s.enqueueSyncMutationTx(tx, SyncEntityObservation, "unsorted-obs", SyncOpUpsert, syncObservationPayload{
			SyncID:    "unsorted-obs",
			SessionID: "unsorted-session",
			Type:      "decision",
			Title:     "unsorted",
			Content:   "no repo, no project",
			Scope:     "project",
		})
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var project string
	if err := s.db.QueryRow(`SELECT project FROM sync_mutations WHERE target_key = ? AND entity_key = ?`, SyncInboxTargetKey, "unsorted-obs").Scan(&project); err != nil {
		t.Fatalf("read inbox mutation: %v", err)
	}
	if project != SyncInboxUnsortedProject {
		t.Fatalf("inbox project = %q, want %q", project, SyncInboxUnsortedProject)
	}
}

func TestAliasMapGovernsInboxRouting(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterSyncProjectAlias("old-name", "renamed-project"); err != nil {
		t.Fatalf("register alias: %v", err)
	}
	plainDir := t.TempDir()
	if err := s.RegisterSyncProjectCwd(plainDir, "cwd-project"); err != nil {
		t.Fatalf("register cwd: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO sessions (id, project, directory) VALUES ('alias-session', '', ?)`, plainDir); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Rename alias wins for an explicit candidate key.
	if err := s.withTx(func(tx *sql.Tx) error {
		return s.enqueueSyncMutationTx(tx, SyncEntityObservation, "alias-obs", SyncOpUpsert, syncObservationPayload{
			SyncID: "alias-obs", SessionID: "alias-session", Type: "decision", Title: "renamed", Content: "c", Project: ptrString("old-name"), Scope: "project",
		})
	}); err != nil {
		t.Fatalf("enqueue alias observation: %v", err)
	}
	var project string
	if err := s.db.QueryRow(`SELECT project FROM sync_mutations WHERE target_key = ? AND entity_key = ?`, SyncInboxTargetKey, "alias-obs").Scan(&project); err != nil {
		t.Fatalf("read inbox mutation: %v", err)
	}
	if project != "renamed-project" {
		t.Fatalf("alias-routed project = %q, want %q", project, "renamed-project")
	}

	// cwd alias resolves a project-less write from a non-repo directory.
	if err := s.withTx(func(tx *sql.Tx) error {
		return s.enqueueSyncMutationTx(tx, SyncEntityObservation, "cwd-obs", SyncOpUpsert, syncObservationPayload{
			SyncID: "cwd-obs", SessionID: "alias-session", Type: "decision", Title: "cwd", Content: "c", Scope: "project",
		})
	}); err != nil {
		t.Fatalf("enqueue cwd observation: %v", err)
	}
	if err := s.db.QueryRow(`SELECT project FROM sync_mutations WHERE target_key = ? AND entity_key = ?`, SyncInboxTargetKey, "cwd-obs").Scan(&project); err != nil {
		t.Fatalf("read inbox mutation: %v", err)
	}
	if project != "cwd-project" {
		t.Fatalf("cwd-routed project = %q, want %q", project, "cwd-project")
	}
}

func TestEnrolledCandidateKeepsLegacyDeliveryLane(t *testing.T) {
	s := newTestStore(t)
	if err := s.EnrollProject("enrolled-proj"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if err := s.CreateSession("enrolled-session", "enrolled-proj", "/tmp/enrolled"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "enrolled-session", Type: "decision", Title: "t", Content: "c", Project: "enrolled-proj", Scope: "project",
	}); err != nil {
		t.Fatalf("add observation: %v", err)
	}
	if got := countSyncMutationsWhere(t, s, `target_key = ? AND entity = ?`, DefaultSyncTargetKey, SyncEntityObservation); got != 1 {
		t.Fatalf("legacy cloud observation rows = %d, want 1", got)
	}
	if got := countSyncMutationsWhere(t, s, `target_key = ?`, SyncInboxTargetKey); got != 0 {
		t.Fatalf("inbox rows for enrolled candidate = %d, want 0", got)
	}
	state, err := s.GetSyncState(syncTargetKeyForProject("enrolled-proj"))
	if err != nil {
		t.Fatalf("read project state: %v", err)
	}
	if state.Lifecycle != SyncLifecyclePending {
		t.Fatalf("enrolled project lifecycle = %q, want pending", state.Lifecycle)
	}
}

func TestEnrolledGitSlugRoutesToDeliveryLane(t *testing.T) {
	s := newTestStore(t)
	repoDir := initGitRepo(t, "Slug_Enrolled")
	if err := s.EnrollProject("slug-enrolled"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO sessions (id, project, directory) VALUES ('slug-enrolled-session', '', ?)`, repoDir); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := s.withTx(func(tx *sql.Tx) error {
		return s.enqueueSyncMutationTx(tx, SyncEntityObservation, "slug-enrolled-obs", SyncOpUpsert, syncObservationPayload{
			SyncID: "slug-enrolled-obs", SessionID: "slug-enrolled-session", Type: "decision", Title: "t", Content: "c", Scope: "project",
		})
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var project string
	if err := s.db.QueryRow(`SELECT target_key, project FROM sync_mutations WHERE entity_key = ?`, "slug-enrolled-obs").Scan(&project, &project); err != nil {
		t.Fatalf("read mutation: %v", err)
	}
	if project != "slug-enrolled" {
		t.Fatalf("slug candidate project = %q, want %q", project, "slug-enrolled")
	}
	var targetKey string
	if err := s.db.QueryRow(`SELECT target_key FROM sync_mutations WHERE entity_key = ?`, "slug-enrolled-obs").Scan(&targetKey); err != nil {
		t.Fatal(err)
	}
	if targetKey != DefaultSyncTargetKey {
		t.Fatalf("enrolled slug candidate target = %q, want %q", targetKey, DefaultSyncTargetKey)
	}
	state, err := s.GetSyncState(syncTargetKeyForProject("slug-enrolled"))
	if err != nil {
		t.Fatalf("read slug project state: %v", err)
	}
	if state.Lifecycle != SyncLifecyclePending {
		t.Fatalf("slug project lifecycle = %q, want pending (delivery lane mint)", state.Lifecycle)
	}
}

func TestRemoteSourcePreservesLegacyJournaling(t *testing.T) {
	s := newTestStore(t)
	err := s.withTx(func(tx *sql.Tx) error {
		return s.enqueueSyncMutationWithSourceTx(tx, SyncEntityObservation, "remote-obs", SyncOpUpsert, syncObservationPayload{
			SyncID: "remote-obs", SessionID: "remote-session", Type: "decision", Title: "t", Content: "c", Project: ptrString("remote-unenrolled"), Scope: "project",
		}, SyncSourceRemote)
	})
	if err != nil {
		t.Fatalf("enqueue remote: %v", err)
	}
	var targetKey, project string
	if err := s.db.QueryRow(`SELECT target_key, project FROM sync_mutations WHERE entity_key = ?`, "remote-obs").Scan(&targetKey, &project); err != nil {
		t.Fatalf("read mutation: %v", err)
	}
	if targetKey != DefaultSyncTargetKey || project != "remote-unenrolled" {
		t.Fatalf("remote mutation = %s/%s, want cloud/remote-unenrolled (legacy behavior preserved)", targetKey, project)
	}
	if got := countSyncMutationsWhere(t, s, `target_key = ?`, SyncInboxTargetKey); got != 0 {
		t.Fatalf("remote source routed to inbox: %d rows, want 0", got)
	}
	state, err := s.GetSyncState(syncTargetKeyForProject("remote-unenrolled"))
	if err != nil {
		t.Fatalf("remote unenrolled state must still exist (legacy mint): %v", err)
	}
	if state.Lifecycle != SyncLifecyclePending {
		t.Fatalf("remote state lifecycle = %q, want pending", state.Lifecycle)
	}
}

func ptrString(v string) *string { return &v }

// ─── D3: stop minting unenrolled sync_state rows on read/touch ──────────────

func TestGetSyncStateDoesNotMintUnenrolledProjectTarget(t *testing.T) {
	s := newTestStore(t)
	target := syncTargetKeyForProject("mint-unenrolled")
	if _, err := s.GetSyncState(target); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetSyncState(unenrolled) err = %v, want sql.ErrNoRows", err)
	}
	if got := scalarInt(t, s, `SELECT COUNT(*) FROM sync_state WHERE target_key = ?`, target); got != 0 {
		t.Fatalf("unenrolled target rows after read = %d, want 0", got)
	}
}

func TestGetSyncStateStillMintsReservedAndEnrolledTargets(t *testing.T) {
	s := newTestStore(t)
	if err := s.EnrollProject("mint-enrolled"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	for _, target := range []string{
		DefaultSyncTargetKey,
		SyncInboxTargetKey,
		LocalChunkTargetKey,
		syncTargetKeyForProject("mint-enrolled"),
	} {
		if _, err := s.GetSyncState(target); err != nil {
			t.Fatalf("GetSyncState(%q): %v", target, err)
		}
		if got := scalarInt(t, s, `SELECT COUNT(*) FROM sync_state WHERE target_key = ?`, target); got != 1 {
			t.Fatalf("target %q rows = %d, want 1", target, got)
		}
	}
}

func TestMarkSyncBlockedDoesNotMintUnenrolledTarget(t *testing.T) {
	s := newTestStore(t)
	target := syncTargetKeyForProject("blocked-unenrolled")
	if err := s.MarkSyncBlocked(target, "blocked_unenrolled", "not enrolled"); err != nil {
		t.Fatalf("MarkSyncBlocked on absent unenrolled target: %v", err)
	}
	if got := scalarInt(t, s, `SELECT COUNT(*) FROM sync_state WHERE target_key = ?`, target); got != 0 {
		t.Fatalf("unenrolled target rows after MarkSyncBlocked = %d, want 0", got)
	}
}

func TestMarkSyncBlockedStillMintsEnrolledTarget(t *testing.T) {
	s := newTestStore(t)
	if err := s.EnrollProject("blocked-enrolled"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	target := syncTargetKeyForProject("blocked-enrolled")
	if err := s.MarkSyncBlocked(target, "blocked_unenrolled", "message"); err != nil {
		t.Fatalf("MarkSyncBlocked on enrolled target: %v", err)
	}
	state, err := s.GetSyncState(target)
	if err != nil {
		t.Fatalf("read enrolled state: %v", err)
	}
	if state.Lifecycle != SyncLifecycleDegraded {
		t.Fatalf("enrolled lifecycle = %q, want degraded", state.Lifecycle)
	}
}

func TestRefreshProjectSyncStateSkipsAbsentUnenrolled(t *testing.T) {
	s := newTestStore(t)
	target := syncTargetKeyForProject("refresh-unenrolled")
	if err := s.withTx(func(tx *sql.Tx) error {
		return s.refreshProjectSyncStateTx(tx, "refresh-unenrolled")
	}); err != nil {
		t.Fatalf("refresh absent unenrolled target: %v", err)
	}
	if got := scalarInt(t, s, `SELECT COUNT(*) FROM sync_state WHERE target_key = ?`, target); got != 0 {
		t.Fatalf("refresh minted unenrolled state rows = %d, want 0", got)
	}
}

func TestApplyPulledMutationRequiresExistingStateRow(t *testing.T) {
	s := newTestStore(t)
	target := syncTargetKeyForProject("pull-unenrolled")
	err := s.ApplyPulledMutation(target, SyncMutation{
		Seq: 1, Entity: SyncEntityObservation, EntityKey: "pull-key", Op: SyncOpUpsert, Payload: "{}",
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ApplyPulledMutation without state row = %v, want sql.ErrNoRows", err)
	}
	if got := scalarInt(t, s, `SELECT COUNT(*) FROM sync_state WHERE target_key = ?`, target); got != 0 {
		t.Fatalf("ApplyPulledMutation minted rows = %d, want 0", got)
	}
}

// ─── D4: unenrolled telemetry exclusion & legacy empty-project contracts ────

func TestLegacyBlankProjectTelemetryDoesNotJournal(t *testing.T) {
	s := newTestStore(t)
	// Legacy shape: sessions rows that identify no project — no migration ever
	// rewrites them. New local writes for them must not journal at all.
	if _, err := s.db.Exec(`INSERT INTO sessions (id, project, directory) VALUES ('blank-session', '', '/tmp/blank')`); err != nil {
		t.Fatalf("seed legacy session: %v", err)
	}
	if err := s.EndSession("blank-session", "done"); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if err := s.DeleteSession("blank-session"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if got := countSyncMutationsWhere(t, s, `1=1`); got != 0 {
		t.Fatalf("blank-project telemetry journaled %d mutations, want 0", got)
	}
}

func TestLegacyEmptyProjectCloudRowsKeepDeliveryContract(t *testing.T) {
	s := newTestStore(t)
	// Already-journaled legacy rows in the delivery lane keep today's
	// empty-project-always-syncs contract: ListPendingSyncMutations is untouched.
	if _, err := s.db.Exec(`INSERT INTO sync_mutations (target_key, entity, entity_key, op, payload, source, project) VALUES (?, 'observation', 'legacy-obs', 'upsert', '{}', 'local', '')`, DefaultSyncTargetKey); err != nil {
		t.Fatalf("seed legacy empty-project row: %v", err)
	}
	mutations, err := s.ListPendingSyncMutations(DefaultSyncTargetKey, 10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(mutations) != 1 || mutations[0].EntityKey != "legacy-obs" {
		t.Fatalf("pending = %+v, want the legacy empty-project row", mutations)
	}
}
