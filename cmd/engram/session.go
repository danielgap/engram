package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Gentleman-Programming/engram/v2/internal/store"
)

// Injectable store entry points for the session end command, following the
// package-var pattern used across main.go so tests can stub every store touch.
var (
	storeEndSession = func(s *store.Store, id, summary string) error {
		return s.EndSession(id, summary)
	}
	storeGetSession = func(s *store.Store, id string) (*store.Session, error) {
		return s.GetSession(id)
	}
)

// sessionEndArgs carries the parsed form of one "engram session end"
// invocation. This slice only supports the single-session grammar; bulk
// filters (--by-age/--project/--apply) land in a later slice.
type sessionEndArgs struct {
	sessionID  string
	summary    string
	hasSummary bool
	jsonOut    bool
}

// isMissingFlagValue reports whether the token after a value-taking flag fails
// to name a usable value: it is blank or is itself a long flag. Accepting such
// a token as the value (e.g. summary="--json") would consume the next option
// as data and run the end operation under an argument the operator never
// typed, so it is rejected with the same error as an absent value.
func isMissingFlagValue(value string) bool {
	return strings.TrimSpace(value) == "" || strings.HasPrefix(value, "--")
}

// parseSessionEndArgs validates the tokens that follow "engram session end"
// and rejects anything undocumented BEFORE the store is opened (#1084
// guarantee): silently ignoring an unsupported option would let an end
// operation run under assumptions the operator never made. Flag values are
// held to the same bar, so a value that looks like the next flag
// (--summary --json) errors instead of being swallowed.
func parseSessionEndArgs(args []string) (sessionEndArgs, error) {
	var parsed sessionEndArgs
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--summary":
			if i+1 >= len(args) || isMissingFlagValue(args[i+1]) {
				return sessionEndArgs{}, errors.New("--summary requires a value")
			}
			parsed.summary = args[i+1]
			parsed.hasSummary = true
			i++
		case "--json":
			parsed.jsonOut = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return sessionEndArgs{}, fmt.Errorf("unknown flag %q", args[i])
			}
			if parsed.sessionID != "" {
				return sessionEndArgs{}, fmt.Errorf("unexpected extra argument %q", args[i])
			}
			parsed.sessionID = args[i]
		}
	}
	if parsed.sessionID == "" {
		return sessionEndArgs{}, errors.New("a session ID is required")
	}
	return parsed, nil
}

func printSessionEndUsage() {
	fmt.Fprintln(os.Stderr, "usage: engram session end <id> [--summary TEXT] [--json]")
}

func cmdSession(cfg store.Config) {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: engram session end <id> [--summary TEXT] [--json]")
		exitFunc(1)
		return
	}

	switch os.Args[2] {
	case "end":
		cmdSessionEnd(cfg)
	default:
		fmt.Fprintf(os.Stderr, "unknown session command: %s\n\n", os.Args[2])
		printSessionEndUsage()
		exitFunc(1)
	}
}

func cmdSessionEnd(cfg store.Config) {
	parsed, err := parseSessionEndArgs(os.Args[3:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		printSessionEndUsage()
		exitFunc(1)
		return
	}
	cmdSessionEndSingle(cfg, parsed)
}

// sessionEndStatus values name the two terminal verdicts of one end run in
// the --json payload. They mirror the canonical store contract: EndSession
// either performs the real transition ("ended") or the session was already
// terminal ("already_ended").
const (
	sessionEndStatusEnded        = "ended"
	sessionEndStatusAlreadyEnded = "already_ended"
)

// sessionEndJSON is the --json payload of one session end run. EndedAt always
// carries a store-read timestamp: the original ended_at for an already-ended
// session, the authoritative new value after a committed end. It is omitted
// only if the store somehow reports none, so a committed end can still lose
// the field to a failed read without corrupting the payload.
type sessionEndJSON struct {
	ID      string  `json:"id"`
	Status  string  `json:"status"`
	EndedAt *string `json:"ended_at,omitempty"`
}

// writeSessionEndJSON prints one session end JSON payload to stdout.
func writeSessionEndJSON(value sessionEndJSON) {
	out, err := jsonMarshalIndent(value, "", "  ")
	if err != nil {
		fatal(err)
		return
	}
	fmt.Println(string(out))
}

// cmdSessionEndSingle ends exactly one session by ID, composing the canonical
// store primitives. A read-only GetSession pre-check renders the two
// display-only verdicts the single-shot store call cannot express: an unknown
// ID is a hard error (the CLI must not inherit the store's wrapped-not-found
// error as an afterthought — it is the operator's primary feedback), and an
// already-ended session is a normal no-op notice with exit code 0, without
// calling EndSession at all. Otherwise EndSession performs the real
// transition, and a fresh GetSession supplies the authoritative ended_at for
// the confirmation line and the --json payload, so every reported timestamp
// is read back from the store rather than assumed. The pre-check + end
// composition is display-only bookkeeping between two store calls; it needs
// no retries or locking beyond what the store already provides.
func cmdSessionEndSingle(cfg store.Config, parsed sessionEndArgs) {
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer func() { _ = s.Close() }()

	session, err := storeGetSession(s, parsed.sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrSessionNotFound) {
			fmt.Fprintf(os.Stderr, "error: session %q not found\n", parsed.sessionID)
			exitFunc(1)
			return
		}
		fatal(err)
		return
	}
	if session.EndedAt != nil && strings.TrimSpace(*session.EndedAt) != "" {
		if parsed.jsonOut {
			writeSessionEndJSON(sessionEndJSON{
				ID:      parsed.sessionID,
				Status:  sessionEndStatusAlreadyEnded,
				EndedAt: session.EndedAt,
			})
			return
		}
		fmt.Printf("Session %q already ended\n", parsed.sessionID)
		return
	}

	var summary string
	if parsed.hasSummary {
		summary = parsed.summary
	}
	if err := storeEndSession(s, parsed.sessionID, summary); err != nil {
		fatal(err)
		return
	}
	final, err := storeGetSession(s, parsed.sessionID)
	if err != nil {
		fatal(err)
		return
	}
	if parsed.jsonOut {
		writeSessionEndJSON(sessionEndJSON{
			ID:      parsed.sessionID,
			Status:  sessionEndStatusEnded,
			EndedAt: final.EndedAt,
		})
		return
	}
	fmt.Printf("Session %q ended\n", parsed.sessionID)
}
