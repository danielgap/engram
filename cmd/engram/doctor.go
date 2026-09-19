package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Gentleman-Programming/engram/v2/internal/diagnostic"
	"github.com/Gentleman-Programming/engram/v2/internal/store"
)

func cmdDoctor(cfg store.Config) {
	if len(os.Args) > 2 && os.Args[2] == "repair" {
		cmdDoctorRepair(cfg)
		return
	}
	if len(os.Args) > 2 && os.Args[2] == "acknowledge" {
		cmdDoctorAcknowledge(cfg)
		return
	}
	jsonOut := false
	project := ""
	check := ""
	for i := 2; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--json":
			jsonOut = true
		case "--project":
			if i+1 >= len(os.Args) {
				fmt.Fprintln(os.Stderr, "error: --project requires a value")
				exitFunc(1)
				return
			}
			project = os.Args[i+1]
			i++
		case "--check":
			if i+1 >= len(os.Args) {
				fmt.Fprintln(os.Stderr, "error: --check requires a value")
				exitFunc(1)
				return
			}
			check = os.Args[i+1]
			i++
		case "--help", "-h", "help":
			printDoctorUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "error: unknown doctor argument %q\n", os.Args[i])
			printDoctorUsage()
			exitFunc(1)
			return
		}
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	if strings.TrimSpace(project) != "" {
		// Doctor can inspect a pending-sync project before it has an observation
		// bucket, so its explicit diagnostic filter is structurally validated but
		// does not require ProjectExists.
		project, err = resolveCLIProject(s, project, false)
		if err != nil {
			fatal(err)
			return
		}
	}

	report, err := runDiagnostics(context.Background(), s, strings.TrimSpace(project), strings.TrimSpace(check))
	if err != nil {
		report = diagnostic.ErrorReport(project, err)
		if jsonOut {
			writeDoctorJSON(report)
		} else {
			fmt.Fprintf(os.Stderr, "engram doctor failed: %s\n", err)
		}
		if errors.Is(err, diagnostic.ErrInvalidCheck) {
			exitFunc(1)
		}
		return
	}

	if jsonOut {
		writeDoctorJSON(report)
		return
	}
	renderDoctorText(report)
}

func printDoctorUsage() {
	fmt.Fprintln(os.Stdout, "usage: engram doctor [--json] [--project PROJECT] [--check CODE]")
	fmt.Fprintln(os.Stdout, "       engram doctor repair --project PROJECT --check CODE (--plan|--dry-run|--apply)")
	fmt.Fprintln(os.Stdout, "       engram doctor repair [--project PROJECT] --check "+diagnostic.CheckSyncMutationRequiredFields+" [--plan|--dry-run|--apply] (default: --dry-run)")
	fmt.Fprintln(os.Stdout, "       engram doctor acknowledge --check CODE [--project PROJECT] [--note NOTE]")
	fmt.Fprintln(os.Stdout, "       engram doctor acknowledge --check CODE --revoke [--fingerprint HEX]")
	_, _ = fmt.Fprintln(os.Stdout, "note: --project is required for every repair check except "+diagnostic.CheckSyncMutationRequiredFields+", where it optionally scopes title repair, supersession, quarantine, and source-title repair.")
	fmt.Fprintln(os.Stdout, "checks: "+strings.Join(diagnostic.RegisteredCodes(), ", "))
	_, _ = fmt.Fprintln(os.Stdout, "diagnostic-only checks with no repair: "+strings.Join(diagnosticOnlyCheckCodes(), ", "))
	_, _ = fmt.Fprintln(os.Stdout, "acknowledgeable checks: "+strings.Join(diagnostic.AcknowledgeableCodes(), ", "))
}

func printDoctorRepairUsage() {
	_, _ = fmt.Fprintln(os.Stdout, "usage: engram doctor repair --project PROJECT --check CODE (--plan|--dry-run|--apply)")
	_, _ = fmt.Fprintln(os.Stdout, "       engram doctor repair [--project PROJECT] --check "+diagnostic.CheckSyncMutationRequiredFields+" [--plan|--dry-run|--apply] (default: --dry-run)")
	_, _ = fmt.Fprintln(os.Stdout, "note: --project is required for every repair check except "+diagnostic.CheckSyncMutationRequiredFields+", where it optionally scopes title repair, supersession, quarantine, and source-title repair.")
	_, _ = fmt.Fprintln(os.Stdout, "repairable checks: "+strings.Join(diagnostic.RepairableCodes(), ", "))
	_, _ = fmt.Fprintln(os.Stdout, "diagnostic-only checks with no repair: "+strings.Join(diagnosticOnlyCheckCodes(), ", "))
}

func diagnosticOnlyCheckCodes() []string {
	repairable := make(map[string]bool, len(diagnostic.RepairableCodes()))
	for _, code := range diagnostic.RepairableCodes() {
		repairable[code] = true
	}

	registered := diagnostic.RegisteredCodes()
	diagnosticOnly := make([]string, 0, len(registered)-len(repairable))
	for _, code := range registered {
		if !repairable[code] {
			diagnosticOnly = append(diagnosticOnly, code)
		}
	}
	return diagnosticOnly
}

func cmdDoctorRepair(cfg store.Config) {
	project := ""
	check := ""
	mode := diagnostic.RepairMode("")
	modeCount := 0
	for i := 3; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--project":
			if i+1 >= len(os.Args) {
				failDoctorRepair("--project requires a value")
				return
			}
			project = os.Args[i+1]
			i++
		case "--check":
			if i+1 >= len(os.Args) {
				failDoctorRepair("--check requires a value")
				return
			}
			check = os.Args[i+1]
			i++
		case "--plan":
			mode = diagnostic.RepairModePlan
			modeCount++
		case "--dry-run":
			mode = diagnostic.RepairModeDryRun
			modeCount++
		case "--apply":
			mode = diagnostic.RepairModeApply
			modeCount++
		case "--help", "-h", "help":
			printDoctorRepairUsage()
			return
		default:
			failDoctorRepair(fmt.Sprintf("unknown doctor repair argument %q", os.Args[i]))
			return
		}
	}

	project, _ = store.NormalizeProject(project)
	project = strings.TrimSpace(project)
	check = strings.TrimSpace(check)
	if project == "" && check != diagnostic.CheckSyncMutationRequiredFields {
		failDoctorRepair("--project is required")
		return
	}
	if check == "" {
		failDoctorRepair("--check is required")
		return
	}
	if modeCount == 0 && check == diagnostic.CheckSyncMutationRequiredFields {
		mode = diagnostic.RepairModeDryRun
	} else if modeCount != 1 {
		failDoctorRepair("exactly one of --plan, --dry-run, or --apply is required")
		return
	}
	if !diagnostic.IsRepairableCode(check) {
		if _, err := diagnostic.DefaultRegistry().Lookup(check); err == nil {
			failDoctorRepair(check + " is a diagnostic-only check with no repair; run engram doctor --check " + check)
			return
		}
		failDoctorRepair("unsupported repair check " + check)
		return
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	if check == diagnostic.CheckSyncMutationRequiredFields {
		repairs, err := s.RepairObservationMutationTitles(project, mode == diagnostic.RepairModeApply)
		if err != nil {
			failDoctorRepair(err.Error())
			return
		}
		superseded, err := s.SupersedeUnenrolledLegacyMutations(store.DefaultSyncTargetKey, project, mode == diagnostic.RepairModeApply)
		if err != nil {
			failDoctorRepair(err.Error())
			return
		}
		report, err := s.QuarantineIrreparableSyncMutations(store.DefaultSyncTargetKey, project, mode == diagnostic.RepairModeApply)
		if err != nil {
			failDoctorRepair(err.Error())
			return
		}
		if mode != diagnostic.RepairModeApply {
			handledSeqs := make(map[int64]struct{}, len(repairs.Actions)+len(superseded.Actions))
			for _, action := range repairs.Actions {
				handledSeqs[action.Seq] = struct{}{}
			}
			for _, action := range superseded.Actions {
				handledSeqs[action.Seq] = struct{}{}
			}
			remaining := report.Actions[:0]
			for _, action := range report.Actions {
				if _, handled := handledSeqs[action.Seq]; !handled {
					remaining = append(remaining, action)
				}
			}
			report.Actions = remaining
		}
		sourceRepairs, err := s.RepairObservationSourceTitles(project, mode == diagnostic.RepairModeApply)
		if err != nil {
			failDoctorRepair(err.Error())
			return
		}
		if mode == diagnostic.RepairModeApply {
			report.Applied = len(repairs.Actions) > 0 || len(report.Actions) > 0 || len(superseded.Actions) > 0 || len(sourceRepairs.Actions) > 0
		}
		writeDoctorRepairJSON(struct {
			store.SyncMutationQuarantineReport
			Repairs                []store.SyncMutationTitleRepairAction      `json:"repairs"`
			Superseded             []store.SyncMutationSupersedeAction         `json:"superseded"`
			SourceRepairs          []store.ObservationSourceTitleRepairAction `json:"source_repairs"`
			SourceRepairBackupPath string                                     `json:"source_repair_backup_path,omitempty"`
		}{report, repairs.Actions, superseded.Actions, sourceRepairs.Actions, sourceRepairs.BackupPath})
		return
	}

	ctx := context.Background()
	report, err := runDiagnostics(ctx, s, project, check)
	if err != nil {
		failDoctorRepair(err.Error())
		return
	}
	plan, err := diagnostic.BuildRepairPlan(ctx, diagnostic.Scope{Store: s, Project: project}, report, check, mode)
	if err != nil {
		failDoctorRepair(err.Error())
		return
	}
	if check == diagnostic.CheckSyncTargetClosedSpace {
		cleanup, err := s.CleanupForeignSyncTargets(mode == diagnostic.RepairModeApply)
		if err != nil {
			failDoctorRepair(err.Error())
			return
		}
		plan.TargetActions = make([]diagnostic.SyncTargetCleanupAction, 0, len(cleanup.Actions))
		if mode == diagnostic.RepairModeApply && len(cleanup.Actions) > 0 {
			plan.Status = "applied"
		}
		for _, action := range cleanup.Actions {
			plan.TargetActions = append(plan.TargetActions, diagnostic.SyncTargetCleanupAction{TargetKey: action.TargetKey, RetargetedMutations: action.RetargetedMutations, RetainedMutations: action.RetainedMutations, StateRemoved: action.StateRemoved})
			if action.RetainedMutations > 0 && mode == diagnostic.RepairModeApply {
				plan.Status = "blocked"
				if cleanup.Applied {
					plan.Status = "partial"
				}
			}
		}
		writeDoctorRepairJSON(plan)
		return
	}
	if check == diagnostic.CheckOrphanedObservationSession {
		plan.Counts.SessionsPlanned = int64(len(plan.SessionRebuilds))
		for _, action := range plan.SessionRebuilds {
			plan.Counts.ObservationsPlanned += action.ObservationCount
		}
		if mode == diagnostic.RepairModeApply && len(plan.SessionRebuilds) > 0 {
			candidates := make([]store.SessionRebuildCandidate, 0, len(plan.SessionRebuilds))
			for _, action := range plan.SessionRebuilds {
				candidates = append(candidates, store.SessionRebuildCandidate{Project: action.Project, SessionID: action.SessionID, StartedAt: action.StartedAt})
			}
			result, err := s.ApplyOrphanedObservationSessionRepair(candidates)
			if err != nil {
				// The pre-tx backup exists even when the transaction failed, so
				// the failure output must preserve its path for the user.
				message := err.Error()
				if result.BackupPath != "" {
					message += "; pre-repair backup preserved at " + result.BackupPath
				}
				failDoctorRepair(message)
				return
			}
			plan.Status = orphanRepairApplyStatus(result.Counts.SessionsInserted, len(plan.SessionRebuilds))
			plan.BackupPath = result.BackupPath
			plan.Counts.SessionsApplied = result.Counts.SessionsInserted
			plan.Counts.ObservationsApplied = result.Counts.ObservationsLinked
		}
		writeDoctorRepairJSON(plan)
		return
	}
	actions := make([]store.SessionProjectReclassification, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		actions = append(actions, store.SessionProjectReclassification{SessionID: action.SessionID, FromProject: action.FromProject, ToProject: action.ToProject})
	}
	if mode == diagnostic.RepairModeApply && len(actions) > 0 {
		counts, err := s.EstimateSessionProjectReclassification(actions)
		if err != nil {
			failDoctorRepair(err.Error())
			return
		}
		plan.Counts.SessionsPlanned = counts.Sessions
		plan.Counts.ObservationsPlanned = counts.Observations
		plan.Counts.PromptsPlanned = counts.Prompts
		result, err := s.ApplySessionProjectReclassification(actions)
		if err != nil {
			failDoctorRepair(err.Error())
			return
		}
		plan.Status = "applied"
		plan.BackupPath = result.BackupPath
		plan.Counts.SessionsApplied = result.Counts.Sessions
		plan.Counts.ObservationsApplied = result.Counts.Observations
		plan.Counts.PromptsApplied = result.Counts.Prompts
	} else {
		counts, err := s.EstimateSessionProjectReclassification(actions)
		if err != nil {
			failDoctorRepair(err.Error())
			return
		}
		plan.Counts.SessionsPlanned = counts.Sessions
		plan.Counts.ObservationsPlanned = counts.Observations
		plan.Counts.PromptsPlanned = counts.Prompts
	}
	writeDoctorRepairJSON(plan)
}

// orphanRepairApplyStatus derives the honest plan status from what the apply
// actually inserted versus the planner's non-skipped rebuild actions: an apply
// that inserted nothing is a noop, one that rebuilt only part of the plan is
// partial, and only a complete apply is applied.
func orphanRepairApplyStatus(sessionsInserted int64, planned int) string {
	switch {
	case sessionsInserted <= 0:
		return "noop"
	case sessionsInserted < int64(planned):
		return "partial"
	default:
		return "applied"
	}
}

func failDoctorRepair(message string) {
	fmt.Fprintln(os.Stderr, "engram doctor repair failed: "+message)
	printDoctorRepairUsage()
	exitFunc(1)
}

// doctorAcknowledgeJSONResult is the stable JSON envelope of `engram doctor
// acknowledge`. The acknowledge mode lists every fingerprint it upserted; the
// revoke mode reports the deleted row count.
type doctorAcknowledgeJSONResult struct {
	Check        string                       `json:"check"`
	Project      string                       `json:"project,omitempty"`
	Mode         string                       `json:"mode"`
	Count        int64                        `json:"count"`
	Fingerprint  string                       `json:"fingerprint,omitempty"`
	Acknowledged []doctorAcknowledgeEntryJSON `json:"acknowledged,omitempty"`
}

type doctorAcknowledgeEntryJSON struct {
	Fingerprint string `json:"fingerprint"`
	Display     string `json:"display"`
	Note        string `json:"note,omitempty"`
}

func cmdDoctorAcknowledge(cfg store.Config) {
	project := ""
	check := ""
	note := ""
	fingerprint := ""
	revoke := false
	jsonOut := false
	for i := 3; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--project":
			if i+1 >= len(os.Args) {
				failDoctorAcknowledge("--project requires a value")
				return
			}
			project = os.Args[i+1]
			i++
		case "--check":
			if i+1 >= len(os.Args) {
				failDoctorAcknowledge("--check requires a value")
				return
			}
			check = os.Args[i+1]
			i++
		case "--note":
			if i+1 >= len(os.Args) {
				failDoctorAcknowledge("--note requires a value")
				return
			}
			note = os.Args[i+1]
			i++
		case "--fingerprint":
			if i+1 >= len(os.Args) {
				failDoctorAcknowledge("--fingerprint requires a value")
				return
			}
			fingerprint = os.Args[i+1]
			i++
		case "--revoke":
			revoke = true
		case "--json":
			jsonOut = true
		case "--help", "-h", "help":
			printDoctorAcknowledgeUsage()
			return
		default:
			failDoctorAcknowledge(fmt.Sprintf("unknown doctor acknowledge argument %q", os.Args[i]))
			return
		}
	}
	check = strings.TrimSpace(check)
	if check == "" {
		failDoctorAcknowledge("--check is required")
		return
	}
	if !diagnostic.IsAcknowledgeableCode(check) {
		failDoctorAcknowledge(fmt.Sprintf("unsupported acknowledge check %q; acknowledgeable checks: %s", check, strings.Join(diagnostic.AcknowledgeableCodes(), ", ")))
		return
	}
	if revoke && strings.TrimSpace(note) != "" {
		failDoctorAcknowledge("--note cannot be combined with --revoke")
		return
	}
	if !revoke && strings.TrimSpace(fingerprint) != "" {
		failDoctorAcknowledge("--fingerprint requires --revoke")
		return
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	ctx := context.Background()
	if revoke {
		revokeDoctorAcknowledgements(ctx, s, check, fingerprint, jsonOut)
		return
	}
	project, _ = store.NormalizeProject(project)
	project = strings.TrimSpace(project)
	if project != "" {
		// Acknowledge can inspect a check on a pending-sync project before it
		// has an observation bucket, matching the base doctor command.
		project, err = resolveCLIProject(s, project, false)
		if err != nil {
			fatal(err)
			return
		}
	}
	// runDiagnostics routes a --check request through RunOne, which filters
	// already-acknowledged findings and never prunes: acknowledging is a
	// scoped, additive action and must not revoke anything.
	report, err := runDiagnostics(ctx, s, project, check)
	if err != nil {
		failDoctorAcknowledge(err.Error())
		return
	}
	var findings []diagnostic.Finding
	for _, checkResult := range report.Checks {
		if checkResult.CheckID == check {
			findings = checkResult.Findings
		}
	}
	if len(findings) == 0 {
		if jsonOut {
			writeDoctorAcknowledgeJSON(doctorAcknowledgeJSONResult{Check: check, Project: project, Mode: "acknowledge", Count: 0})
			return
		}
		fmt.Printf("no %s findings reported; nothing to acknowledge\n", check)
		return
	}
	entries := make([]store.DoctorAcknowledgementInput, 0, len(findings))
	rendered := make([]doctorAcknowledgeEntryJSON, 0, len(findings))
	for _, finding := range findings {
		fingerprint := diagnostic.FindingFingerprint(finding)
		entries = append(entries, store.DoctorAcknowledgementInput{CheckID: check, EvidenceFingerprint: fingerprint, Note: strings.TrimSpace(note)})
		rendered = append(rendered, doctorAcknowledgeEntryJSON{Fingerprint: fingerprint, Display: doctorAcknowledgeFindingLabel(finding), Note: strings.TrimSpace(note)})
	}
	written, err := s.UpsertDoctorAcknowledgements(ctx, entries)
	if err != nil {
		failDoctorAcknowledge(err.Error())
		return
	}
	if jsonOut {
		writeDoctorAcknowledgeJSON(doctorAcknowledgeJSONResult{Check: check, Project: project, Mode: "acknowledge", Count: written, Acknowledged: rendered})
		return
	}
	for _, entry := range rendered {
		fmt.Printf("%s %s %s\n", check, doctorAcknowledgeShortFingerprint(entry.Fingerprint), entry.Display)
	}
	fmt.Printf("acknowledged %d finding(s) for %s\n", written, check)
}

func revokeDoctorAcknowledgements(ctx context.Context, s *store.Store, check, fingerprint string, jsonOut bool) {
	var fingerprints []string
	if fingerprint = strings.TrimSpace(fingerprint); fingerprint != "" {
		fingerprints = []string{fingerprint}
	}
	revoked, err := s.RevokeDoctorAcknowledgements(ctx, check, fingerprints)
	if err != nil {
		failDoctorAcknowledge(err.Error())
		return
	}
	if jsonOut {
		writeDoctorAcknowledgeJSON(doctorAcknowledgeJSONResult{Check: check, Mode: "revoke", Count: revoked, Fingerprint: fingerprint})
		return
	}
	fmt.Printf("revoked %d acknowledgement(s) for %s\n", revoked, check)
}

// doctorAcknowledgeFindingLabel renders the human-facing tail of an
// acknowledgement line: the affected project when the evidence carries one,
// otherwise the affected session ID, otherwise a short evidence snippet.
func doctorAcknowledgeFindingLabel(finding diagnostic.Finding) string {
	var evidence struct {
		Project   string `json:"project"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(finding.Evidence, &evidence); err == nil {
		if label := strings.TrimSpace(evidence.Project); label != "" {
			return label
		}
		if label := strings.TrimSpace(evidence.SessionID); label != "" {
			return label
		}
	}
	return truncate(string(finding.Evidence), 24)
}

// doctorAcknowledgeShortFingerprint truncates a fingerprint for display only;
// stored acknowledgements always keep the full hex value.
func doctorAcknowledgeShortFingerprint(fingerprint string) string {
	if len(fingerprint) <= 12 {
		return fingerprint
	}
	return fingerprint[:12]
}

func printDoctorAcknowledgeUsage() {
	fmt.Fprintln(os.Stdout, "usage: engram doctor acknowledge --check CODE [--project PROJECT] [--note NOTE]")
	fmt.Fprintln(os.Stdout, "       engram doctor acknowledge --check CODE --revoke [--fingerprint HEX]")
	_, _ = fmt.Fprintln(os.Stdout, "note: acknowledgements persist accepted findings so doctor stops re-reporting them; a full unscoped run prunes acknowledgements whose evidence no longer exists.")
	_, _ = fmt.Fprintln(os.Stdout, "acknowledgeable checks: "+strings.Join(diagnostic.AcknowledgeableCodes(), ", "))
}

func failDoctorAcknowledge(message string) {
	fmt.Fprintln(os.Stderr, "engram doctor acknowledge failed: "+message)
	printDoctorAcknowledgeUsage()
	exitFunc(1)
}

func writeDoctorAcknowledgeJSON(value doctorAcknowledgeJSONResult) {
	out, err := jsonMarshalIndent(value, "", "  ")
	if err != nil {
		fatal(err)
		return
	}
	fmt.Println(string(out))
}

func writeDoctorRepairJSON(value any) {
	out, err := jsonMarshalIndent(value, "", "  ")
	if err != nil {
		fatal(err)
		return
	}
	fmt.Println(string(out))
}

func writeDoctorJSON(report diagnostic.Report) {
	out, err := jsonMarshalIndent(report, "", "  ")
	if err != nil {
		fatal(err)
		return
	}
	fmt.Println(string(out))
}

func renderDoctorText(report diagnostic.Report) {
	fmt.Printf("Engram Doctor: %s\n", report.Status)
	if report.Project != "" {
		fmt.Printf("Project: %s\n", report.Project)
	}
	fmt.Printf("Checks: %d ok=%d warnings=%d blocked=%d errors=%d\n", report.Summary.Total, report.Summary.OK, report.Summary.Warnings, report.Summary.Blocked, report.Summary.Errors)
	if report.AcknowledgementsPruned > 0 {
		fmt.Printf("acknowledged (%d pruned)\n", report.AcknowledgementsPruned)
	}
	fmt.Printf("\n")
	for _, check := range report.Checks {
		fmt.Printf("[%s] %s — %s\n", check.Result, check.CheckID, check.Message)
		if check.Why != "" {
			fmt.Printf("  why: %s\n", check.Why)
		}
		if check.SafeNextStep != "" {
			fmt.Printf("  next: %s\n", check.SafeNextStep)
		}
		for _, finding := range check.Findings {
			fmt.Printf("  - %s: %s\n", finding.ReasonCode, finding.Message)
			if len(finding.Evidence) > 0 {
				fmt.Printf("    evidence: %s\n", string(finding.Evidence))
			}
		}
	}
}
