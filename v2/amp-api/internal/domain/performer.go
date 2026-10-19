package domain

import (
	"fmt"
	"slices"
	"strings"
)

// Who does a ticket's work.
const (
	PerformerAgent = "agent"
	PerformerHuman = "human"
)

// HumanAgentID is a human ticket's agent_id once its owner starts it, and
// the agent_id of the run recorded when they finish it.
const HumanAgentID = "human"

// HumanReasons are why a person, not an agent, does a ticket. Reports group
// human work by them, to show what kind of work people do instead of agents.
var HumanReasons = []string{"review", "approval", "access", "judgment", "external", "physical", "other"}

// HumanReasonText describes each reason, for tool descriptions and the UI.
var HumanReasonText = map[string]string{
	"review":   "a person reviews or merges (code review, PR merge)",
	"approval": "a sign-off only a person can give (release, spend, legal, security)",
	"access":   "needs access or credentials agents don't have (prod, a vendor console, accounts)",
	"judgment": "a decision or judgment call (priorities, design direction, trade-offs)",
	"external": "depends on someone outside the team (a customer, vendor, another team)",
	"physical": "physical or offline work (hardware, a site visit, a phone call)",
	"other":    "anything else a person has to do",
}

// NormalizePerformer validates a performer and its reason. Empty performer
// means agent; a reason only goes with "human" and must be a known one.
func NormalizePerformer(performer, reason string) (string, string, error) {
	performer = strings.ToLower(strings.TrimSpace(performer))
	reason = strings.ToLower(strings.TrimSpace(reason))
	switch performer {
	case "", PerformerAgent:
		if reason != "" {
			return "", "", fmt.Errorf("human_reason only goes with performer \"human\"")
		}
		return PerformerAgent, "", nil
	case PerformerHuman, "person":
		if reason == "" {
			reason = "other"
		}
		if !slices.Contains(HumanReasons, reason) {
			return "", "", fmt.Errorf("human_reason must be one of %s", strings.Join(HumanReasons, ", "))
		}
		return PerformerHuman, reason, nil
	}
	return "", "", fmt.Errorf("performer must be \"agent\" or \"human\"")
}
