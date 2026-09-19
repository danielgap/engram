package diagnostic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Gentleman-Programming/engram/v2/internal/store"
)

const (
	StatusOK      = "ok"
	StatusWarning = "warning"
	StatusBlocked = "blocked"
	StatusError   = "error"

	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityError    = "error"
	SeverityBlocking = "blocking"
)

type Scope struct {
	Store                  *store.Store
	Project                string
	Now                    time.Time
	ReadSQLiteLockSnapshot func(context.Context) (store.SQLiteLockSnapshot, error)
	DetectProject          func(string) (DetectedProject, bool)
}

type DetectedProject struct {
	Project string `json:"project"`
	Source  string `json:"source"`
	Path    string `json:"path,omitempty"`
}

type Finding struct {
	CheckID              string          `json:"check_id"`
	Severity             string          `json:"severity"`
	ReasonCode           string          `json:"reason_code"`
	Message              string          `json:"message"`
	Why                  string          `json:"why"`
	Evidence             json.RawMessage `json:"evidence"`
	SafeNextStep         string          `json:"safe_next_step"`
	RequiresConfirmation bool            `json:"requires_confirmation"`
}

// FindingFingerprint returns the stable sha256 hex fingerprint of a finding's
// evidence. It is the identity a persisted acknowledgement is keyed on: the
// same evidence fingerprints identically on every run, while any change to the
// evidence value produces a new, unacknowledged fingerprint. The canonical
// encoding is encoding/json Marshal of the Evidence value — map keys are
// sorted and struct fields follow declaration order, so the bytes are
// deterministic. Display forms may truncate (for example the first 12 hex
// characters); stored acknowledgements always keep the full 64-character hex.
func FindingFingerprint(finding Finding) string {
	encoded, err := json.Marshal(finding.Evidence)
	if err != nil {
		// Evidence in this package is always produced by encoding/json, so this
		// is unreachable in practice; fall back to a stable placeholder instead
		// of failing acknowledgement bookkeeping.
		encoded = []byte(`null`)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

type CheckResult struct {
	CheckID              string          `json:"check_id"`
	Result               string          `json:"result"`
	Severity             string          `json:"severity"`
	ReasonCode           string          `json:"reason_code"`
	Message              string          `json:"message"`
	Why                  string          `json:"why"`
	Evidence             json.RawMessage `json:"evidence"`
	SafeNextStep         string          `json:"safe_next_step"`
	RequiresConfirmation bool            `json:"requires_confirmation"`
	Findings             []Finding       `json:"findings,omitempty"`
	// AcknowledgedCount reports findings of this check whose evidence a human
	// has accepted and which are therefore excluded from Findings and from the
	// severity roll-up. Omitted when zero so unaffected reports keep their shape.
	AcknowledgedCount int `json:"acknowledged_count,omitempty"`
}

type Summary struct {
	Total    int `json:"total"`
	OK       int `json:"ok"`
	Warnings int `json:"warnings"`
	Blocked  int `json:"blocked"`
	Errors   int `json:"errors"`
}

type Report struct {
	Status  string        `json:"status"`
	Project string        `json:"project,omitempty"`
	Summary Summary       `json:"summary"`
	Checks  []CheckResult `json:"checks"`
	// AcknowledgementsPruned reports stale acknowledgement rows the full run
	// removed because their evidence no longer exists. Set only by RunAll and
	// omitted when zero.
	AcknowledgementsPruned int `json:"acknowledgements_pruned,omitempty"`
}

type DiagnosticCheck interface {
	Code() string
	Run(context.Context, Scope) (CheckResult, error)
}

type Runner struct {
	registry Registry
}

func NewRunner() Runner                       { return Runner{registry: DefaultRegistry()} }
func NewRunnerWithRegistry(r Registry) Runner { return Runner{registry: r} }

func (r Runner) RunAll(ctx context.Context, scope Scope) (Report, error) {
	checks := r.registry.Checks()
	results := make([]CheckResult, 0, len(checks))
	pruned := 0
	for _, check := range checks {
		result, seenFingerprints, err := runCheck(ctx, scope, check)
		if err != nil {
			return Report{}, err
		}
		results = append(results, result)
		n, err := pruneAcknowledgementsForCheck(ctx, scope, check.Code(), seenFingerprints)
		if err != nil {
			return Report{}, err
		}
		pruned += int(n)
	}
	report := buildReport(scope.Project, results)
	report.AcknowledgementsPruned = pruned
	return report, nil
}

// pruneAcknowledgementsForCheck enforces the doctor self-prune rule: a FULL
// RunAll collects every acknowledgeable check's findings, so the fingerprints
// it saw prove which acknowledgements still match live evidence and rows for
// vanished evidence are deleted. RunOne never prunes: a single-check run —
// especially one scoped to a project — does not prove the absence of evidence
// other scopes would still find, so revocation there would silently drop
// acknowledgements that are still valid.
func pruneAcknowledgementsForCheck(ctx context.Context, scope Scope, checkID string, seenFingerprints []string) (int64, error) {
	if scope.Store == nil || !IsAcknowledgeableCode(checkID) {
		return 0, nil
	}
	pruned, err := scope.Store.PruneDoctorAcknowledgements(ctx, checkID, seenFingerprints)
	if err != nil {
		return 0, fmt.Errorf("prune doctor acknowledgements for %s: %w", checkID, err)
	}
	return pruned, nil
}

func (r Runner) RunOne(ctx context.Context, scope Scope, code string) (Report, error) {
	check, err := r.registry.Lookup(code)
	if err != nil {
		return Report{}, err
	}
	result, _, err := runCheck(ctx, scope, check)
	if err != nil {
		return Report{}, err
	}
	return buildReport(scope.Project, []CheckResult{result}), nil
}

// runCheck normalizes one check's result and splits acknowledged findings from
// active ones. It returns the filtered result together with the fingerprints
// of ALL findings the check collected (acknowledged and active), so a full
// RunAll can prune acknowledgements whose evidence disappeared. Findings whose
// fingerprint is acknowledged are removed from the result: active findings
// alone drive the severity roll-up, and a check left with zero active findings
// but at least one acknowledged becomes ok with acknowledged_count set.
func runCheck(ctx context.Context, scope Scope, check DiagnosticCheck) (CheckResult, []string, error) {
	result, err := check.Run(ctx, scope)
	if err != nil {
		return CheckResult{}, nil, err
	}
	result.CheckID = strings.TrimSpace(result.CheckID)
	if result.CheckID == "" {
		result.CheckID = check.Code()
	}
	if result.Result == "" {
		result.Result = StatusOK
	}
	if result.Severity == "" {
		result.Severity = SeverityInfo
	}
	if result.ReasonCode == "" {
		result.ReasonCode = result.CheckID + "_ok"
	}
	if result.Evidence == nil {
		result.Evidence = mustJSON(map[string]any{"evaluated": true})
	}
	if scope.Store == nil || !IsAcknowledgeableCode(result.CheckID) {
		return result, nil, nil
	}
	seenFingerprints := make([]string, 0, len(result.Findings))
	for _, finding := range result.Findings {
		seenFingerprints = append(seenFingerprints, FindingFingerprint(finding))
	}
	filtered, err := filterAcknowledgedFindings(ctx, scope, result)
	if err != nil {
		return CheckResult{}, nil, err
	}
	return filtered, seenFingerprints, nil
}

// filterAcknowledgedFindings splits a check's collected findings into active
// and acknowledged by evidence fingerprint and rebuilds the result so severity
// comes from the active findings only. A store read failure is an operational
// error and fails the run loudly instead of silently reporting acknowledged
// findings as active.
func filterAcknowledgedFindings(ctx context.Context, scope Scope, result CheckResult) (CheckResult, error) {
	if len(result.Findings) == 0 {
		return result, nil
	}
	acknowledged, err := scope.Store.AcknowledgedFingerprints(ctx, result.CheckID)
	if err != nil {
		return CheckResult{}, fmt.Errorf("read doctor acknowledgements for %s: %w", result.CheckID, err)
	}
	if len(acknowledged) == 0 {
		return result, nil
	}
	active := make([]Finding, 0, len(result.Findings))
	for _, finding := range result.Findings {
		if _, ok := acknowledged[FindingFingerprint(finding)]; ok {
			continue
		}
		active = append(active, finding)
	}
	if len(active) == len(result.Findings) {
		return result, nil
	}
	acknowledgedCount := len(result.Findings) - len(active)
	if len(active) == 0 {
		// Every finding is acknowledged: the check is ok for severity purposes
		// but stays in the report with the accepted count visible.
		filtered := okResult(result.CheckID, map[string]any{"acknowledged_count": acknowledgedCount})
		filtered.Message = fmt.Sprintf("%d finding(s) acknowledged.", acknowledgedCount)
		filtered.AcknowledgedCount = acknowledgedCount
		return filtered, nil
	}
	// okEvidence is unused by resultFromFindings when findings are present, so
	// the roll-up below is driven purely by the active findings.
	filtered := resultFromFindings(result.CheckID, map[string]any{}, active)
	filtered.AcknowledgedCount = acknowledgedCount
	return filtered, nil
}

func buildReport(project string, checks []CheckResult) Report {
	report := Report{Status: StatusOK, Project: project, Checks: checks}
	report.Summary.Total = len(checks)
	for _, check := range checks {
		switch check.Result {
		case StatusError:
			report.Summary.Errors++
		case StatusBlocked:
			report.Summary.Blocked++
		case StatusWarning:
			report.Summary.Warnings++
		default:
			report.Summary.OK++
		}
	}
	switch {
	case report.Summary.Errors > 0:
		report.Status = StatusError
	case report.Summary.Blocked > 0:
		report.Status = StatusBlocked
	case report.Summary.Warnings > 0:
		report.Status = StatusWarning
	default:
		report.Status = StatusOK
	}
	return report
}

func ErrorReport(project string, err error) Report {
	code := "diagnostic_error"
	if errors.Is(err, ErrInvalidCheck) {
		code = "invalid_check"
	}
	return Report{
		Status:  StatusError,
		Project: project,
		Summary: Summary{Total: 1, Errors: 1},
		Checks: []CheckResult{{
			CheckID:              code,
			Result:               StatusError,
			Severity:             SeverityError,
			ReasonCode:           code,
			Message:              err.Error(),
			Why:                  "Doctor could not run the requested diagnostic safely.",
			Evidence:             mustJSON(map[string]any{"error": err.Error()}),
			SafeNextStep:         "Run `engram doctor` without --check to list registered diagnostics.",
			RequiresConfirmation: false,
		}},
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func okResult(checkID string, evidence any) CheckResult {
	return CheckResult{
		CheckID:              checkID,
		Result:               StatusOK,
		Severity:             SeverityInfo,
		ReasonCode:           checkID + "_ok",
		Message:              "No issues detected.",
		Why:                  "The evaluated store evidence matches expected operational invariants.",
		Evidence:             mustJSON(evidence),
		SafeNextStep:         "No action required.",
		RequiresConfirmation: false,
	}
}

func resultFromFindings(checkID string, okEvidence any, findings []Finding) CheckResult {
	if len(findings) == 0 {
		return okResult(checkID, okEvidence)
	}
	result := CheckResult{
		CheckID:              checkID,
		Result:               StatusWarning,
		Severity:             SeverityWarning,
		ReasonCode:           findings[0].ReasonCode,
		Message:              fmt.Sprintf("%d finding(s) detected.", len(findings)),
		Why:                  findings[0].Why,
		Evidence:             mustJSON(map[string]any{"finding_count": len(findings)}),
		SafeNextStep:         findings[0].SafeNextStep,
		RequiresConfirmation: false,
		Findings:             findings,
	}
	hasActionableFinding := false
	for _, f := range findings {
		switch f.Severity {
		case SeverityBlocking:
			result.Result = StatusBlocked
			result.Severity = SeverityBlocking
			return result
		case SeverityError:
			hasActionableFinding = true
			if result.Result != StatusBlocked {
				result.Result = StatusError
				result.Severity = SeverityError
			}
		case SeverityWarning:
			hasActionableFinding = true
		}
	}
	if !hasActionableFinding {
		result.Result = StatusOK
		result.Severity = SeverityInfo
		result.Message = "No actionable issues detected."
		result.SafeNextStep = "No action required."
	}
	return result
}
