package domain

import (
	"errors"
	"fmt"
	"strings"
)

// Structured agent questions (#130): amp_block_task may give the person the
// options to choose from and the one it recommends. The person picks one in
// AMP (or writes something else), and the chosen option is what the agent
// receives as the answer.

// Question option bounds: the same as a decision's ask.
const (
	MinQuestionOptions = 2
	MaxQuestionOptions = 4
	maxOptionLabel     = 120
)

// QuestionOption is one answer the person can choose. Its JSON is the
// human-decisions spec's AskOption ({label, consequence}), so the two PRs
// share one shape; once both have landed it can become an alias of it.
type QuestionOption struct {
	Label       string `json:"label"`       // "Pipedrive"
	Consequence string `json:"consequence"` // optional here: trade-offs, cost
}

// Question is the structured part of a ticket's open question: its options
// and the one the agent recommends (an index, or nil for none). The question
// itself is the ticket's block_reason. Recommended only marks a badge: the
// person always chooses.
type Question struct {
	Options     []QuestionOption `json:"options"`
	Recommended *int             `json:"recommended"`
}

// Validate checks a question's options: 2 to 4, each with a distinct label,
// and the recommended one (if any) in range. It trims labels and
// consequences in place.
func (q *Question) Validate() error {
	if q == nil {
		return errors.New("question is missing")
	}
	if n := len(q.Options); n < MinQuestionOptions || n > MaxQuestionOptions {
		return fmt.Errorf("options needs %d to %d options (got %d); leave options out to ask in words only", MinQuestionOptions, MaxQuestionOptions, n)
	}
	seen := map[string]bool{}
	for i := range q.Options {
		o := &q.Options[i]
		o.Label, o.Consequence = strings.TrimSpace(o.Label), strings.TrimSpace(o.Consequence)
		if o.Label == "" {
			return fmt.Errorf("option %d needs a label", i+1)
		}
		if len([]rune(o.Label)) > maxOptionLabel {
			return fmt.Errorf("option %d's label is over %d characters: keep the label short and put the detail in its consequence", i+1, maxOptionLabel)
		}
		k := strings.ToLower(o.Label)
		if seen[k] {
			return fmt.Errorf("option %d (%s) repeats another option's label", i+1, o.Label)
		}
		seen[k] = true
	}
	if q.Recommended != nil && (*q.Recommended < 0 || *q.Recommended >= len(q.Options)) {
		return fmt.Errorf("recommended is %d; it must index one of the %d options (0 to %d)", *q.Recommended, len(q.Options), len(q.Options)-1)
	}
	return nil
}

// Choose returns the label of option i, or an error naming the choices when
// the ticket's question has no such option.
func (q *Question) Choose(i int) (string, error) {
	if q == nil || len(q.Options) == 0 {
		return "", errors.New("this question has no options to choose from: answer it in words")
	}
	if i < 0 || i >= len(q.Options) {
		return "", fmt.Errorf("option %d isn't one of the question's %d options (0 to %d)", i, len(q.Options), len(q.Options)-1)
	}
	return q.Options[i].Label, nil
}

// ChosenAnswer is the comment an answer posts when the person chose option
// label: the choice on its own line, so the agent reads it first, then
// anything they added.
func ChosenAnswer(label, note string) string {
	body := "Chose: " + label
	if note = strings.TrimSpace(note); note != "" {
		body += "\n\n" + note
	}
	return body
}
