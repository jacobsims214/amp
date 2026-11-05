package domain

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func validAsk() *Ask {
	return &Ask{
		Question: "Which north-star metric do we commit to for the next 90 days?",
		Options: []AskOption{
			{Label: "A: Weekly active teams", Consequence: "Easy to measure; rewards seats over value"},
			{Label: "B: Activation rate (40%, floor 30%)", Consequence: "Needs the onboarding event (#41)"},
			{Label: "C: Net revenue retention", Consequence: "Lags 90 days; can't steer this quarter"},
		},
		Recommended: 1,
		Reason:      "B is the only one we can move and measure inside the quarter.",
		Evidence:    []string{"artifact #1", "#3"},
	}
}

func TestAskValidate(t *testing.T) {
	if err := validAsk().Validate(); err != nil {
		t.Fatalf("valid ask: %v", err)
	}
	cases := map[string]struct {
		edit func(a *Ask)
		want string
	}{
		"one option": {func(a *Ask) { a.Options = a.Options[:1]; a.Recommended = 0 }, "2 to 4 options (got 1)"},
		"five options": {func(a *Ask) {
			a.Options = append(a.Options, AskOption{"D", "d"}, AskOption{"E", "e"})
		}, "2 to 4 options (got 5)"},
		"recommended out of range": {func(a *Ask) { a.Recommended = 3 }, "recommended is 3"},
		"recommended negative":     {func(a *Ask) { a.Recommended = -1 }, "recommended is -1"},
		"missing consequence":      {func(a *Ask) { a.Options[2].Consequence = "  " }, "option 3 (C: Net revenue retention) needs a consequence"},
		"missing label":            {func(a *Ask) { a.Options[0].Label = "" }, "option 1 needs a label"},
		"no question":              {func(a *Ask) { a.Question = " " }, "needs a question"},
		"no reason":                {func(a *Ask) { a.Reason = "" }, "recommendation_reason"},
	}
	for name, c := range cases {
		a := validAsk()
		c.edit(a)
		err := a.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	var nilAsk *Ask
	if nilAsk.Validate() == nil {
		t.Error("nil ask validated")
	}
}

func TestAskStoredKeepsOnlyTheStructuredPart(t *testing.T) {
	a := validAsk()
	a.Options[0].Label = "  A: Weekly active teams  "
	d := a.Stored()
	if d.Question != a.Question || d.Recommended != 1 || len(d.Options) != 3 || d.Options[0].Label != "A: Weekly active teams" {
		t.Fatalf("stored = %+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("stored validate: %v", err)
	}
}

func TestHumanBriefMissing(t *testing.T) {
	if m := HumanBriefMissing("do it", "done", "review", nil); m != nil {
		t.Fatalf("full brief: %v", m)
	}
	if m := HumanBriefMissing(" ", "", "physical", nil); !slices.Equal(m, []string{"description", "acceptance_criteria"}) {
		t.Fatalf("empty brief: %v", m)
	}
	if m := HumanBriefMissing("do it", "done", "judgment", nil); !slices.Equal(m, []string{"ask"}) {
		t.Fatalf("judgment without ask: %v", m)
	}
	if m := HumanBriefMissing("do it", "done", "judgment", validAsk()); m != nil {
		t.Fatalf("judgment with ask: %v", m)
	}
	bad := validAsk()
	bad.Options = bad.Options[:1]
	bad.Recommended = 0
	m := HumanBriefMissing("", "done", "judgment", bad)
	if len(m) != 2 || m[0] != "description" || !strings.HasPrefix(m[1], "ask (") || !strings.Contains(m[1], "2 to 4 options") {
		t.Fatalf("judgment with an invalid ask: %v", m)
	}
}

func TestDecisionDocClaimed(t *testing.T) {
	var d *DecisionDoc
	if d.Claimed() {
		t.Fatal("nil doc claimed")
	}
	d = &DecisionDoc{}
	if d.Claimed() {
		t.Fatal("empty doc claimed")
	}
	v := 2
	d.DecidedVersion = &v
	if d.Claimed() {
		t.Fatal("version without a claim time counts as claimed")
	}
	now := time.Now()
	d.RecordingStartedAt = &now
	if !d.Claimed() {
		t.Fatal("claimed doc not claimed")
	}
}
