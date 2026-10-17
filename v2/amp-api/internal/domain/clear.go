package domain

import (
	"fmt"
	"slices"
	"strings"
)

// Clearing fields: in an update an empty string means "keep", so that
// callers can send only what changes. To blank a field on purpose, name it
// in the request's Clear list.

// Fields each kind of update can clear. Names and priorities can't be blank.
var (
	TaskClearable  = []string{"description", "acceptance_criteria", "assigned_to", "agent_id"}
	EpicClearable  = []string{"description"}
	StoryClearable = []string{"description", "acceptance_criteria"}
)

// Clears is the set of fields an update blanks.
type Clears map[string]bool

// ParseClears validates a Clear list against the fields that can be cleared.
func ParseClears(fields []string, allowed []string) (Clears, error) {
	c := Clears{}
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if !slices.Contains(allowed, f) {
			return nil, fmt.Errorf("clear: %q can't be cleared; clearable fields are %s", f, strings.Join(allowed, ", "))
		}
		c[f] = true
	}
	return c, nil
}

// Apply returns the new value of a text field: blank when cleared, value
// when one was given, otherwise current.
func (c Clears) Apply(field, value, current string) string {
	switch {
	case c[field]:
		return ""
	case value != "":
		return value
	}
	return current
}
