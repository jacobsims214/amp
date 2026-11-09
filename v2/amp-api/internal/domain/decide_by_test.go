package domain

import (
	"testing"
	"time"
)

func TestRaisedPriority(t *testing.T) {
	for _, c := range []struct {
		in, want string
		changed  bool
	}{{"0", "1", true}, {"1", "2", true}, {"2", "3", true}, {"3", "3", false}, {"", "2", true}, {"-1", "1", true}} {
		got, changed := RaisedPriority(c.in)
		if got != c.want || changed != c.changed {
			t.Errorf("RaisedPriority(%q) = %q, %v; want %q, %v", c.in, got, changed, c.want, c.changed)
		}
	}
}

func TestCheckDecideBy(t *testing.T) {
	at := time.Now()
	if err := CheckDecideBy(PerformerAgent, &at); err != ErrDecideByNotHuman {
		t.Fatalf("agent ticket: %v", err)
	}
	if err := CheckDecideBy(PerformerAgent, nil); err != nil {
		t.Fatalf("clearing on an agent ticket: %v", err)
	}
	if err := CheckDecideBy(PerformerHuman, &at); err != nil {
		t.Fatalf("human ticket: %v", err)
	}
}

func TestDecisionOverdue(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Minute), now.Add(time.Minute)
	for _, c := range []struct {
		name string
		t    Task
		want bool
	}{
		{"past, open, human", Task{Performer: PerformerHuman, State: TaskStateBacklog, DecideBy: &past}, true},
		{"future", Task{Performer: PerformerHuman, State: TaskStateBacklog, DecideBy: &future}, false},
		{"done", Task{Performer: PerformerHuman, State: TaskStateCompleted, DecideBy: &past}, false},
		{"agent", Task{Performer: PerformerAgent, State: TaskStateBacklog, DecideBy: &past}, false},
		{"none", Task{Performer: PerformerHuman, State: TaskStateBacklog}, false},
	} {
		if got := c.t.DecisionOverdue(now); got != c.want {
			t.Errorf("%s: overdue = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestChangesEscalation(t *testing.T) {
	zero := 0
	if (ProjectSettings{}).ChangesEscalation() {
		t.Fatal("empty settings change escalation")
	}
	if !(ProjectSettings{DecisionBackupDecider: &zero}).ChangesEscalation() {
		t.Fatal("removing the backup decider is an escalation change")
	}
}

func TestClearsFieldsTrimmedAndSorted(t *testing.T) {
	c, err := ParseClears([]string{" decide_by", "description "}, TaskClearable)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Fields(); len(got) != 2 || got[0] != "decide_by" || got[1] != "description" {
		t.Fatalf("Fields() = %q", got)
	}
}
