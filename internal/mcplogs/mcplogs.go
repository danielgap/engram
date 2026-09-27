// Package mcplogs parses and classifies Claude Code MCP client logs for the
// connect / close / never-reconnect failure class tracked in gentle-ai#1019.
//
// Claude Code writes per-server cache fragments as directories named
// mcp-logs-<server>, each containing per-session *.jsonl files where every
// line is a JSON object with a debug message and a timestamp. ScanLifecycles
// groups those lines into per-(server, session) SessionLifecycle records in
// deterministic server-then-session order; ScanDir classifies them.
//
// The classifier implements the surviving-witness standard proposed by
// Denver2828 (2026-08-23) and validated against jjeg1979's negative control
// (2026-08-24): a 4-clause candidate (declared close of 2-5s, cache-clear,
// zero completed tool calls, no later reconnection) only becomes a
// defect-suspect when another MCP server in the same session demonstrably
// outlived the close. A candidate whose peers all closed within ~10s is
// ordinary session teardown; a candidate with no witness either way is
// indeterminate.
//
// No confirmed defect-positive log exists in the wild yet: the
// defect-suspect path is exercised only by the labeled synthetic fixture in
// testdata/synthetic-defect.
package mcplogs

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	closePattern   = regexp.MustCompile(`connection closed after (\d+)s`)
	toolPattern    = regexp.MustCompile(`Tool '([^']+)' completed successfully`)
	startPattern   = regexp.MustCompile(`Starting connection with timeout`)
	connectPattern = regexp.MustCompile(`Successfully connected`)
	clearPattern   = regexp.MustCompile(`Cleared connection cache for reconnection`)
	sigintPattern  = regexp.MustCompile(`Sending SIGINT to MCP server process`)
)

// Verdict is the classification of one (server, session) log group.
type Verdict string

// Verdict values, from Denver2828's 2026-08-23 analysis and jjeg1979's
// 2026-08-24 negative control.
const (
	// VerdictHealthy: no 4-clause candidate match (includes ordinary SIGINT
	// teardown and sessions that reconnected after a close).
	VerdictHealthy Verdict = "healthy"
	// VerdictTeardown: candidate match, but peer servers closed within ~10s —
	// the whole session ended; not this defect.
	VerdictTeardown Verdict = "teardown"
	// VerdictIndeterminate: candidate match, but no peer can witness either way.
	VerdictIndeterminate Verdict = "indeterminate"
	// VerdictDefectSuspect: candidate match plus a surviving witness.
	VerdictDefectSuspect Verdict = "defect-suspect"
)

// Candidate lifetime bounds, in seconds, from the issue's reported pattern.
const (
	candidateMinLifetimeSec = 2
	candidateMaxLifetimeSec = 5
	// peerCloseWindowSec is Denver2828's session-ended bound: peers closing
	// within this window of our close indicate the whole session ended.
	peerCloseWindowSec = 10 * time.Second
)

// SessionVerdict classifies one server's connection lifecycle in one session.
type SessionVerdict struct {
	Server         string
	SessionID      string
	Candidate      bool
	Verdict        Verdict
	Reason         string
	ClosedAfterSec int
	ClearedCache   bool
	SIGINTSent     bool
	ToolCalls      int
}

type logLine struct {
	Debug     string    `json:"debug"`
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"sessionId"`
}

// SessionLifecycle is the parsed connection history of one (server, session)
// log group.
type SessionLifecycle struct {
	Server         string
	SessionID      string
	Starts         []time.Time
	EverConnected  bool
	ClosedAt       time.Time
	ClosedAfterSec int
	HasClose       bool
	ClearedCache   bool
	SIGINTSent     bool
	ToolCalls      []time.Time
}

// logDirPrefix is the Claude Code cache directory prefix for per-server logs.
const logDirPrefix = "mcp-logs-"

// ScanLifecycles walks a Claude Code cache fragment (directories named
// mcp-logs-<server>, each containing per-session *.jsonl files) and returns
// the parsed SessionLifecycle for every (server, session) group, sorted by
// server then session id. Malformed lines are skipped.
func ScanLifecycles(root string) ([]SessionLifecycle, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("mcplogs: %s is not a directory", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	groups := make(map[string]*SessionLifecycle)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), logDirPrefix) {
			continue
		}
		server := strings.TrimPrefix(entry.Name(), logDirPrefix)
		files, err := os.ReadDir(filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".jsonl") {
				continue
			}
			if err := scanFile(filepath.Join(root, entry.Name(), file.Name()), server, groups); err != nil {
				return nil, err
			}
		}
	}
	lifecycles := make([]SessionLifecycle, 0, len(groups))
	for _, group := range groups {
		lifecycles = append(lifecycles, *group)
	}
	sort.Slice(lifecycles, func(i, j int) bool {
		if lifecycles[i].Server != lifecycles[j].Server {
			return lifecycles[i].Server < lifecycles[j].Server
		}
		return lifecycles[i].SessionID < lifecycles[j].SessionID
	})
	return lifecycles, nil
}

func scanFile(path, server string, groups map[string]*SessionLifecycle) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// bufio.Reader instead of bufio.Scanner: scanner tokens are capped, so one
	// oversized line would abort the whole scan via ErrTooLong and discard
	// every lifecycle parsed so far. ReadString tolerates lines of any size;
	// malformed or oversized lines are skipped by the json.Unmarshal failure
	// path below.
	reader := bufio.NewReader(f)
	for {
		raw, readErr := reader.ReadString('\n')
		line := strings.TrimSuffix(raw, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line != "" {
			var entry logLine
			if err := json.Unmarshal([]byte(line), &entry); err == nil && entry.SessionID != "" && entry.Debug != "" {
				key := server + "\x00" + entry.SessionID
				group := groups[key]
				if group == nil {
					group = &SessionLifecycle{Server: server, SessionID: entry.SessionID}
					groups[key] = group
				}
				applyLine(group, entry)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil // final line without trailing newline was still processed
			}
			return readErr
		}
	}
}

func applyLine(group *SessionLifecycle, line logLine) {
	debug, at := line.Debug, line.Timestamp
	switch {
	case startPattern.MatchString(debug):
		group.Starts = append(group.Starts, at)
	case connectPattern.MatchString(debug):
		group.EverConnected = true
	case closePattern.MatchString(debug):
		if m := closePattern.FindStringSubmatch(debug); len(m) == 2 {
			if secs, err := strconv.Atoi(m[1]); err == nil {
				// Log fragments may arrive out of order; keep the newest close.
				if !group.HasClose || at.After(group.ClosedAt) {
					group.HasClose = true
					group.ClosedAfterSec = secs
					group.ClosedAt = at
				}
			}
		}
	case clearPattern.MatchString(debug):
		group.ClearedCache = true
	case sigintPattern.MatchString(debug):
		group.SIGINTSent = true
	case toolPattern.MatchString(debug):
		group.ToolCalls = append(group.ToolCalls, at)
	}
}

// ScanDir walks a Claude Code cache fragment (directories named
// mcp-logs-<server>, each containing per-session *.jsonl files) and
// classifies every (server, session) group. Malformed lines are skipped.
func ScanDir(root string) ([]SessionVerdict, error) {
	lifecycles, err := ScanLifecycles(root)
	if err != nil {
		return nil, err
	}
	return classify(lifecycles), nil
}

// classify maps the sorted lifecycles to verdicts; peers are grouped by
// session id, and ScanLifecycles' server-then-session order is preserved.
func classify(lifecycles []SessionLifecycle) []SessionVerdict {
	bySession := make(map[string][]SessionLifecycle)
	for _, lc := range lifecycles {
		bySession[lc.SessionID] = append(bySession[lc.SessionID], lc)
	}
	verdicts := make([]SessionVerdict, 0, len(lifecycles))
	for _, lc := range lifecycles {
		verdicts = append(verdicts, verdictFor(lc, bySession[lc.SessionID]))
	}
	return verdicts
}

func verdictFor(group SessionLifecycle, peers []SessionLifecycle) SessionVerdict {
	v := SessionVerdict{
		Server:         group.Server,
		SessionID:      group.SessionID,
		ClosedAfterSec: group.ClosedAfterSec,
		ClearedCache:   group.ClearedCache,
		SIGINTSent:     group.SIGINTSent,
		ToolCalls:      len(group.ToolCalls),
		Verdict:        VerdictHealthy,
		Reason:         "not-a-candidate",
	}
	v.Candidate = isCandidate(group)
	if !v.Candidate {
		return v
	}
	for _, peer := range peers {
		if peer.Server == group.Server {
			continue
		}
		if witnessByToolCall(peer, group.ClosedAt) {
			v.Verdict = VerdictDefectSuspect
			v.Reason = "peer-tool-call-after-close"
			return v
		}
	}
	for _, peer := range peers {
		if peer.Server == group.Server {
			continue
		}
		if witnessByOutliving(peer, group.ClosedAt) {
			v.Verdict = VerdictDefectSuspect
			v.Reason = "peer-outlived"
			return v
		}
	}
	anyPeerConnected := false
	for _, peer := range peers {
		if peer.Server != group.Server && peer.EverConnected {
			anyPeerConnected = true
			break
		}
	}
	switch {
	case len(peers) <= 1:
		v.Verdict = VerdictIndeterminate
		v.Reason = "no-peer"
	case anyPeerConnected:
		// Peers connected but nothing survived our close: they closed within
		// the window (or stopped logging without declaring a close), which is
		// ordinary short-session teardown.
		v.Verdict = VerdictTeardown
		v.Reason = "peer-closed-within-10s"
	default:
		v.Verdict = VerdictIndeterminate
		v.Reason = "peer-never-connected"
	}
	return v
}

// isCandidate implements the 4-clause conjunction: declared close of 2-5
// seconds, cache-clear, zero completed tool calls, and no reconnection
// attempt after the close.
func isCandidate(group SessionLifecycle) bool {
	if !group.HasClose {
		return false
	}
	if group.ClosedAfterSec < candidateMinLifetimeSec || group.ClosedAfterSec > candidateMaxLifetimeSec {
		return false
	}
	if !group.ClearedCache {
		return false
	}
	if len(group.ToolCalls) > 0 {
		return false
	}
	for _, start := range group.Starts {
		if start.After(group.ClosedAt) {
			return false
		}
	}
	return true
}

// witnessByToolCall: a peer completed a tool call after our close, so the
// session demonstrably continued using MCP without us.
func witnessByToolCall(peer SessionLifecycle, ourClose time.Time) bool {
	for _, call := range peer.ToolCalls {
		if call.After(ourClose) {
			return true
		}
	}
	return false
}

// witnessByOutliving: a peer declared its own close more than the window
// after ours. A peer that never declared a close proves nothing (the
// unauthenticated teardown logs stop without close lines for some servers).
func witnessByOutliving(peer SessionLifecycle, ourClose time.Time) bool {
	return peer.HasClose && peer.ClosedAt.Sub(ourClose) > peerCloseWindowSec
}
