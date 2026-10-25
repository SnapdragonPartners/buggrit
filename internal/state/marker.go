// Package state holds buggrit's comment markers, the replay that reads each
// finding's state from its thread, and the thread state machine
// (docs/design.md §6).
//
// Comments are buggrit's only store. Every buggrit comment ends with a hidden
// marker carrying the machine-readable part:
//
//	<!-- buggrit:v1 {"id":"f-7f3a2c","fp":"…","sev":"P1","pass":2,"sha":"abc123"} -->
//
// A marker counts only in a comment that is buggrit's own (OwnMarker).
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SnapdragonPartners/buggrit/internal/github"
)

const (
	markerOpen  = "<!-- buggrit:v1 "
	markerClose = " -->"
)

// FindingState is a finding's state in the thread state machine (§6).
type FindingState string

// Finding states. Whether a finding is settled isn't a state: it's read
// from who resolved its thread (Tracked.Settled).
const (
	StateOpen  FindingState = "open"
	StateFixed FindingState = "fixed"
	// StateDismissed is terminal. buggrit no longer writes it, but threads
	// earlier versions dismissed keep it.
	StateDismissed FindingState = "dismissed"
	// StateNeedsHuman is read only, from earlier versions' markers. Replay
	// treats it as open.
	StateNeedsHuman FindingState = "needs_human"
)

// Valid reports whether s is a known state.
func (s FindingState) Valid() bool {
	switch s {
	case StateOpen, StateFixed, StateDismissed, StateNeedsHuman:
		return true
	}
	return false
}

// Kind says why a transition happened. buggrit posts only fixed and
// regressed; the other kinds are read from earlier versions' markers, and
// replay keeps reading them (§6).
type Kind string

// Transition kinds, each with the state it leads to (Kind.To).
const (
	KindFixed     Kind = "fixed"     // open → fixed
	KindRegressed Kind = "regressed" // fixed → open: the defect is back

	// Read only, from earlier versions.
	KindReopened   Kind = "reopened"    // resolved without a fix → open
	KindRebutted   Kind = "rebutted"    // a reply judged invalid → open
	KindNeedsHuman Kind = "needs_human" // rebuttals used up → open
	KindDismissed  Kind = "dismissed"   // a reply judged valid → dismissed
	KindSettled    Kind = "settled"     // needs_human, then a human replied → dismissed
)

// To is the state a transition of kind k leads to, or "" for an unknown kind.
func (k Kind) To() FindingState {
	switch k {
	case KindFixed:
		return StateFixed
	case KindRegressed, KindReopened, KindRebutted:
		return StateOpen
	case KindNeedsHuman:
		return StateNeedsHuman
	case KindDismissed, KindSettled:
		return StateDismissed
	}
	return ""
}

// PassStatus is how a pass ended. Each pass's review body carries a pass
// marker with it (design §6, "Review baseline").
type PassStatus string

// Pass statuses. Only a complete pass advances completed_sha.
const (
	PassComplete   PassStatus = "complete"
	PassIncomplete PassStatus = "incomplete"
)

// Marker is the payload of a buggrit marker. A thread's root comment carries
// a finding marker (FP, Severity, Pass set); each buggrit reply carries a
// transition marker (To, Kind and SHA set, plus Evidence for a fixed
// verdict); each pass's review body carries a pass marker (Status, Pass and
// SHA set, and Verified when the pass re-verified fixed findings). A
// reconcile-only run's review carries SHA and Verified with no Status.
//
// Unknown fields are ignored when decoding, so a marker written by a newer
// buggrit still reads, and so do earlier versions' seen and left_resolved.
type Marker struct {
	// Verified maps each fixed finding re-verified as still fixed at SHA to
	// the evidence files of that verdict (design §6, "Evidence baseline").
	Verified map[string][]string `json:"verified,omitempty"`
	ID       string              `json:"id"`
	FP       string              `json:"fp,omitempty"`
	Severity string              `json:"sev,omitempty"`
	SHA      string              `json:"sha,omitempty"`
	To       FindingState        `json:"to,omitempty"`
	Kind     Kind                `json:"kind,omitempty"`
	Status   PassStatus          `json:"status,omitempty"`
	Evidence []string            `json:"evidence_files,omitempty"`
	Pass     int                 `json:"pass,omitempty"`
}

// IsTransition reports whether m records a state transition.
func (m *Marker) IsTransition() bool { return m.To != "" }

// IsPass reports whether m is a pass marker.
func (m *Marker) IsPass() bool { return m.Status != "" }

func (m *Marker) validate() error {
	switch {
	case m.ID == "":
		return errors.New("marker has no id")
	case m.To != "" && !m.To.Valid():
		return fmt.Errorf("marker %s: unknown state %q", m.ID, m.To)
	case m.Kind != "" && m.Kind.To() != m.To:
		return fmt.Errorf("marker %s: kind %q does not lead to state %q", m.ID, m.Kind, m.To)
	case m.To == StateFixed && len(m.Evidence) == 0:
		// §6: a fixed verdict must list the files it relied on.
		return fmt.Errorf("marker %s: fixed transition has no evidence_files", m.ID)
	case m.Status != "" && m.Status != PassComplete && m.Status != PassIncomplete:
		return fmt.Errorf("marker %s: unknown pass status %q", m.ID, m.Status)
	case m.Status != "" && (m.Pass < 1 || m.SHA == ""):
		return fmt.Errorf("marker %s: pass marker needs pass and sha", m.ID)
	}
	return m.validateVerified()
}

// validateVerified checks a pass's record of re-verified findings: like a
// fixed transition, each needs the files its verdict relied on (§6).
func (m *Marker) validateVerified() error {
	if len(m.Verified) > 0 && m.SHA == "" {
		return fmt.Errorf("marker %s: verified needs sha", m.ID)
	}
	for id, files := range m.Verified {
		if len(files) == 0 {
			return fmt.Errorf("marker %s: verified %s has no evidence files", m.ID, id)
		}
	}
	return nil
}

// Encode renders m as a marker line to append to a comment body.
func Encode(m *Marker) (string, error) {
	if err := m.validate(); err != nil {
		return "", err
	}
	// json.Marshal escapes '<' and '>', so the payload can never contain
	// "-->" and end the HTML comment early.
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encode marker: %w", err)
	}
	return markerOpen + string(b) + markerClose, nil
}

// Append adds m's marker to the end of body.
func Append(body string, m *Marker) (string, error) {
	line, err := Encode(m)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(body, "\n") + "\n" + line, nil
}

// Strip returns body without its trailing marker, for showing a comment to
// the model or quoting it.
func Strip(body string) string {
	if i := strings.LastIndex(body, markerOpen); i >= 0 && strings.Contains(body[i:], markerClose) {
		return strings.TrimRight(body[:i], "\n")
	}
	return body
}

// ErrNoMarker means a comment body has no buggrit marker.
var ErrNoMarker = errors.New("no buggrit marker")

// Parse returns the marker in body. buggrit writes exactly one, at the end,
// so the last one wins; anything quoted earlier in the body is ignored.
func Parse(body string) (*Marker, error) {
	start := strings.LastIndex(body, markerOpen)
	if start < 0 {
		return nil, ErrNoMarker
	}
	rest := body[start+len(markerOpen):]
	end := strings.Index(rest, markerClose)
	if end < 0 {
		return nil, fmt.Errorf("unterminated buggrit marker")
	}
	var m Marker
	if err := json.Unmarshal([]byte(rest[:end]), &m); err != nil {
		return nil, fmt.Errorf("decode buggrit marker: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// SameLogin reports whether two GitHub logins are the same account. Logins
// are case-insensitive.
func SameLogin(a, b string) bool { return a != "" && strings.EqualFold(a, b) }

// OwnMarker returns c's marker if c is buggrit's own comment: it carries a
// valid marker and is authored by botLogin. Other workflows can share
// github-actions[bot], and anyone can paste marker text, so neither check is
// enough alone (design §3). A marker anywhere else is ignored.
func OwnMarker(c *github.Comment, botLogin string) (*Marker, bool) {
	return ownMarker(c.Author, c.Body, botLogin)
}

func ownMarker(author, body, botLogin string) (*Marker, bool) {
	if !SameLogin(author, botLogin) {
		return nil, false
	}
	m, err := Parse(body)
	if err != nil {
		return nil, false
	}
	return m, true
}
