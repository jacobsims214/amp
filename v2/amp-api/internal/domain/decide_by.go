package domain

import (
	"errors"
	"strconv"
	"time"
)

// Decide-by dates on a person's tickets (#146). A judgment ticket (or any
// human ticket) can say when it should be decided by; My Work flags it once
// that passes, and a project that turns escalation on reminds its owner,
// optionally raises its priority and hands it to a backup decider. Nothing
// here ever records a decision: escalation reaches a person.

// ErrDecideByNotHuman refuses a decide-by date on agent work.
var ErrDecideByNotHuman = errors.New("decide_by is for a person's ticket (performer 'human'): an agent's ticket has no decision to wait on")

// CheckDecideBy refuses a decide-by date on a ticket a person doesn't do.
// A nil date (none, or a clear) is always fine.
func CheckDecideBy(performer string, at *time.Time) error {
	if at != nil && performer != PerformerHuman {
		return ErrDecideByNotHuman
	}
	return nil
}

// DecisionOverdue reports whether t is a person's open ticket past its
// decide-by date at now.
func (t *Task) DecisionOverdue(now time.Time) bool {
	return t.IsHuman() && t.State != TaskStateCompleted && t.DecideBy != nil && now.After(*t.DecideBy)
}

// MaxPriority is the highest ticket priority ("3", critical).
const MaxPriority = 3

// RaisedPriority is p one step up ("0" low .. "3" critical), and whether it
// changed. An unreadable priority is treated as normal ("1").
func RaisedPriority(p string) (string, bool) {
	n, err := strconv.Atoi(p)
	if err != nil {
		n = 1
	}
	if n >= MaxPriority {
		return p, false
	}
	if n < 0 {
		n = 0
	}
	return strconv.Itoa(n + 1), true
}

// ChangesEscalation reports whether u changes the decision escalation.
func (u ProjectSettings) ChangesEscalation() bool {
	return u.DecisionEscalationMinutes != nil || u.DecisionEscalationRaisePriority != nil || u.DecisionBackupDecider != nil
}
