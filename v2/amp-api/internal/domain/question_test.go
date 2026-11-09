package domain

import (
	"strings"
	"testing"
)

func TestQuestionValidate(t *testing.T) {
	one, five := 1, 5
	ok := Question{Options: []QuestionOption{{Label: " HubSpot "}, {Label: "Pipedrive", Consequence: "cheaper"}}, Recommended: &one}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid question refused: %v", err)
	}
	if ok.Options[0].Label != "HubSpot" {
		t.Fatalf("labels are trimmed: %q", ok.Options[0].Label)
	}
	for name, q := range map[string]Question{
		"one option":   {Options: []QuestionOption{{Label: "A"}}},
		"five options": {Options: []QuestionOption{{Label: "A"}, {Label: "B"}, {Label: "C"}, {Label: "D"}, {Label: "E"}}},
		"no label":     {Options: []QuestionOption{{Label: "A"}, {Label: " "}}},
		"repeat":       {Options: []QuestionOption{{Label: "A"}, {Label: "a"}}},
		"out of range": {Options: []QuestionOption{{Label: "A"}, {Label: "B"}}, Recommended: &five},
		"long label":   {Options: []QuestionOption{{Label: "A"}, {Label: strings.Repeat("x", 121)}}},
	} {
		if err := q.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestQuestionChoose(t *testing.T) {
	q := &Question{Options: []QuestionOption{{Label: "HubSpot"}, {Label: "Pipedrive"}}}
	if l, err := q.Choose(1); err != nil || l != "Pipedrive" {
		t.Fatalf("Choose(1) = %q, %v", l, err)
	}
	if _, err := q.Choose(2); err == nil {
		t.Fatal("Choose(2) of 2 options accepted")
	}
	var none *Question
	if _, err := none.Choose(0); err == nil {
		t.Fatal("choosing on a question without options accepted")
	}
	if got := ChosenAnswer("Pipedrive", " it's what sales uses "); got != "Chose: Pipedrive\n\nit's what sales uses" {
		t.Fatalf("ChosenAnswer = %q", got)
	}
	if got := ChosenAnswer("Pipedrive", ""); got != "Chose: Pipedrive" {
		t.Fatalf("ChosenAnswer without a note = %q", got)
	}
}
