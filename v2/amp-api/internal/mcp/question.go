package mcp

// Structured agent questions (#130): amp_block_task's options and
// recommended, and amp_answer_question's option.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/simstech/amp-api/internal/domain"
)

// questionProps are amp_block_task's options and recommended fields.
func questionProps() map[string]interface{} {
	return map[string]interface{}{
		"options": map[string]interface{}{
			"type": "array",
			"description": "Optional. The answers the person can choose from, 2 to 4, each {label, consequence}: label is the choice in a few words " +
				"('Pipedrive'), consequence its trade-offs or cost (optional). AMP shows them as one-tap choices beside a free-text answer, " +
				"and the person's choice comes back as the answer ('Chose: <label>'). Leave out for an open question. Example: " +
				`[{"label":"HubSpot","consequence":"Already holds the marketing contacts; $50/seat"},{"label":"Pipedrive","consequence":"What sales uses today; no marketing data"}]`,
			"items": map[string]interface{}{"type": "object", "properties": map[string]interface{}{
				"label":       str("REQUIRED. The choice in a few words (at most 120 characters)."),
				"consequence": str("Optional. What choosing it means: trade-offs, cost."),
			}, "required": []string{"label"}},
		},
		"recommended": map[string]interface{}{
			"type":        "integer",
			"description": "Optional, with options: the 0-based index of the option you recommend. It only marks it as recommended: the person always chooses.",
		},
	}
}

// optQuestion reads amp_block_task's options and recommended: nil when no
// options were given (a question in words, as before), or a validated
// question. recommended without options is refused.
func optQuestion(args map[string]interface{}) (*domain.Question, error) {
	raw, hasOpts := args["options"]
	rec, hasRec := args["recommended"]
	if !hasOpts || raw == nil {
		if hasRec && rec != nil && rec != "" {
			return nil, fmt.Errorf("recommended needs options: give the options it indexes, or leave recommended out")
		}
		return nil, nil
	}
	list, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("options must be an array of {label, consequence}")
	}
	if len(list) == 0 {
		return nil, nil
	}
	q := &domain.Question{Options: make([]domain.QuestionOption, 0, len(list))}
	for i, item := range list {
		switch v := item.(type) {
		case string: // a bare label
			q.Options = append(q.Options, domain.QuestionOption{Label: v})
		case map[string]interface{}:
			label, _ := v["label"].(string)
			cons, _ := v["consequence"].(string)
			q.Options = append(q.Options, domain.QuestionOption{Label: label, Consequence: cons})
		default:
			return nil, fmt.Errorf("options[%d] must be {label, consequence}", i)
		}
	}
	if hasRec && rec != nil && rec != "" {
		i, err := optionIndex(q, rec)
		if err != nil {
			return nil, fmt.Errorf("recommended: %w", err)
		}
		q.Recommended = &i
	}
	if err := q.Validate(); err != nil {
		return nil, err
	}
	return q, nil
}

// optionIndex reads an option reference: a 0-based index (a number, or a
// number in a string) or, against q's options, an exact label.
func optionIndex(q *domain.Question, v interface{}) (int, error) {
	switch n := v.(type) {
	case float64:
		if n != float64(int(n)) {
			return 0, fmt.Errorf("%v isn't a whole number: give the option's 0-based index", n)
		}
		return int(n), nil
	case int:
		return n, nil
	case string:
		s := strings.TrimSpace(n)
		if i, err := strconv.Atoi(s); err == nil {
			return i, nil
		}
		if q != nil {
			for i, o := range q.Options {
				if strings.EqualFold(strings.TrimSpace(o.Label), s) {
					return i, nil
				}
			}
		}
		return 0, fmt.Errorf("%q is neither an option's 0-based index nor one of its labels", s)
	}
	return 0, fmt.Errorf("give the option's 0-based index")
}

// chosenOption reads amp_answer_question's option and option_label. It
// reads nothing from the ticket: the service checks the pair against the
// stored question (after authorization), so a question that changed since
// it was read is refused, not answered with whatever option now has that
// index.
func chosenOption(args map[string]interface{}) (*int, string, error) {
	v, ok := args["option"]
	if !ok || v == nil || v == "" {
		return nil, "", nil
	}
	label := strings.TrimSpace(optStr(args, "option_label", ""))
	if s, isStr := v.(string); isStr {
		if _, err := strconv.Atoi(strings.TrimSpace(s)); err != nil {
			return nil, "", fmt.Errorf("option is the chosen option's 0-based index; give its label as option_label")
		}
	}
	i, err := optionIndex(nil, v)
	if err != nil {
		return nil, "", fmt.Errorf("option: %w", err)
	}
	if label == "" {
		return nil, "", fmt.Errorf("option_label is required with option: the chosen option's label as amp_get_task showed it")
	}
	return &i, label, nil
}
