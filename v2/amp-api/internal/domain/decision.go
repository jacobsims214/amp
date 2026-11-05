package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Decision docs: a judgment ticket carries a structured ask (a question, 2 to
// 4 options with consequences, a recommended option with its reasoning) that
// renders its decision doc, a markdown artifact the owner decides on. See
// docs/specs/human-decision-docs.md.

// UpstreamDecision is what a person decided on a completed human ticket that
// another ticket depends on. It reaches the dependent's agent (MCP
// upstream_decisions) and the dependent's drawer, with no extra call. See
// docs/specs/human-decision-docs.md.
type UpstreamDecision struct {
	TaskID   int       `json:"task_id"`
	Name     string    `json:"name"`
	Reason   string    `json:"human_reason"`
	Decision string    `json:"decision"`         // the note, or option + decided text
	Option   string    `json:"option,omitempty"` // chosen option label (doc tickets)
	DocURL   string    `json:"doc_url,omitempty"`
	Version  int       `json:"version,omitempty"`
	By       *int      `json:"decided_by"`
	ByName   string    `json:"decided_by_name"`
	At       time.Time `json:"decided_at"`
}

// ArtifactRoleDecision marks an artifact as a ticket's decision doc.
const ArtifactRoleDecision = "decision"

// Who wrote an artifact version, fixed by its write path.
const (
	AuthorPerson = "person"
	AuthorAgent  = "agent"
)

// Ask option bounds.
const (
	MinAskOptions = 2
	MaxAskOptions = 4
)

// AskOption is one answer a person can choose. The agent questions of #130
// reuse it.
type AskOption struct {
	Label       string `json:"label"`       // "B: Activation rate (40%, floor 30%)"
	Consequence string `json:"consequence"` // trade-offs, cost
}

// Ask is what a judgment ticket asks its owner. It is tool input, not
// storage: it renders the decision doc's v1, whose metadata keeps the
// question, options and recommended index (DecisionAsk) and whose markdown
// keeps the reason and evidence.
type Ask struct {
	Question    string      `json:"question"`
	Options     []AskOption `json:"options"`     // 2..4
	Recommended int         `json:"recommended"` // index into Options
	Reason      string      `json:"recommendation_reason"`
	Evidence    []string    `json:"evidence,omitempty"` // URLs, "#N", "artifact #N"
}

// DecisionAsk is a decision doc version's stored options: the part of an ask
// that is structured. The prose (reason, evidence, context) is the version's
// markdown.
type DecisionAsk struct {
	Question    string      `json:"question"`
	Options     []AskOption `json:"options"`
	Recommended int         `json:"recommended"`
}

// Validate checks the options every decision doc version must have: a
// question, 2 to 4 options each with a label and a consequence, and the
// recommended one in range.
func (a *DecisionAsk) Validate() error {
	if a == nil {
		return errors.New("ask is missing")
	}
	if strings.TrimSpace(a.Question) == "" {
		return errors.New("ask needs a question")
	}
	if n := len(a.Options); n < MinAskOptions || n > MaxAskOptions {
		return fmt.Errorf("ask needs %d to %d options (got %d)", MinAskOptions, MaxAskOptions, n)
	}
	for i, o := range a.Options {
		if strings.TrimSpace(o.Label) == "" {
			return fmt.Errorf("ask option %d needs a label", i+1)
		}
		if strings.TrimSpace(o.Consequence) == "" {
			return fmt.Errorf("ask option %d (%s) needs a consequence: its trade-offs or cost", i+1, strings.TrimSpace(o.Label))
		}
	}
	if a.Recommended < 0 || a.Recommended >= len(a.Options) {
		return fmt.Errorf("ask's recommended is %d; it must index one of its %d options (0 to %d)", a.Recommended, len(a.Options), len(a.Options)-1)
	}
	return nil
}

// Validate checks an ask: its stored part (DecisionAsk.Validate) and the
// reason for the recommendation.
func (a *Ask) Validate() error {
	if a == nil {
		return errors.New("ask is missing")
	}
	stored := a.Stored()
	if err := stored.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(a.Reason) == "" {
		return errors.New("ask needs a recommendation_reason: why the recommended option")
	}
	return nil
}

// Stored is the part of the ask a version keeps as metadata, trimmed.
func (a *Ask) Stored() DecisionAsk {
	d := DecisionAsk{Question: strings.TrimSpace(a.Question), Recommended: a.Recommended, Options: make([]AskOption, len(a.Options))}
	for i, o := range a.Options {
		d.Options[i] = AskOption{Label: strings.TrimSpace(o.Label), Consequence: strings.TrimSpace(o.Consequence)}
	}
	return d
}

// HumanBriefMissing lists what a human ticket's brief lacks: "description"
// and "acceptance_criteria" for every reason, and for judgment "ask" (none
// given) or "ask (<why it's invalid>)". The caller decides whether a missing
// ask counts: a person's judgment ticket made in the web UI has no ask editor.
// Nil when nothing is missing.
func HumanBriefMissing(desc, criteria, reason string, ask *Ask) []string {
	var missing []string
	if strings.TrimSpace(desc) == "" {
		missing = append(missing, "description")
	}
	if strings.TrimSpace(criteria) == "" {
		missing = append(missing, "acceptance_criteria")
	}
	if ask != nil {
		if err := ask.Validate(); err != nil {
			missing = append(missing, "ask ("+err.Error()+")")
		}
	} else if reason == "judgment" {
		missing = append(missing, "ask")
	}
	return missing
}

// DecisionDocVersion is one version of a decision doc with its content and
// options (ArtifactVersion's JSON carries neither).
type DecisionDocVersion struct {
	Version    int          `json:"version"`
	AuthorKind string       `json:"author_kind"`
	Text       string       `json:"text"`
	Ask        *DecisionAsk `json:"ask"`
	CreatedBy  *int         `json:"created_by"`
	CreatedAt  time.Time    `json:"created_at"`
}

// DecisionDoc is a judgment ticket's decision doc: its artifact, the current
// version's options and markdown, and (once recorded) what was decided.
type DecisionDoc struct {
	TaskID         int          `json:"task_id"`
	ArtifactID     int          `json:"artifact_id"`
	DocURL         string       `json:"doc_url,omitempty"`
	CurrentVersion int          `json:"current_version"`
	AuthorKind     string       `json:"author_kind"` // the current version's
	Ask            *DecisionAsk `json:"ask"`         // the current version's options
	Text           string       `json:"text"`        // the current version's markdown
	// The record (Phase 3); empty while undecided.
	DecidedVersion     *int       `json:"decided_version"`
	ChosenOption       *int       `json:"chosen_option"`
	DecisionText       string     `json:"decision_text,omitempty"`
	DecidedBy          *int       `json:"decided_by"`
	DecidedByName      string     `json:"decided_by_name,omitempty"`
	RecordingStartedAt *time.Time `json:"recording_started_at"`
	DecidedAt          *time.Time `json:"decided_at"`
	// Versions, oldest first, when asked for.
	Versions []DecisionDocVersion `json:"versions,omitempty"`
}

// Claimed reports whether Record decision has claimed (or finished) the
// record: from then on the doc can't change, and dependents read the record.
func (d *DecisionDoc) Claimed() bool {
	return d != nil && d.DecidedVersion != nil && (d.RecordingStartedAt != nil || d.DecidedAt != nil)
}
