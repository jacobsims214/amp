package domain

import (
	"fmt"
	"sort"
	"strings"
)

// Metadata is free-form key/value data on a project, epic, story, or task,
// for integrations and reporting: "jira_board": "PLAT", "initiative":
// "Q3 growth", "capitalizable": true. Values are strings, numbers, or
// booleans. A story's effective metadata is its project's, then its epic's,
// then its own (later wins); a task's adds the task's own on top.
type Metadata map[string]any

// Metadata limits: enough for integration fields, small enough to stay out
// of the way of everything else.
const (
	MaxMetadataKeys     = 50
	MaxMetadataKeyLen   = 64
	MaxMetadataValueLen = 1000
)

// MetadataPatch is a change to metadata: keys with a value are set, keys
// with null are removed, other keys are kept.
type MetadataPatch map[string]any

// Split validates a patch and splits it into the values to set and the
// keys to remove.
func (p MetadataPatch) Split() (set Metadata, remove []string, err error) {
	if len(p) > MaxMetadataKeys {
		return nil, nil, fmt.Errorf("metadata: at most %d keys per change", MaxMetadataKeys)
	}
	set = Metadata{}
	for k, v := range p {
		key := strings.TrimSpace(k)
		if key == "" || len(key) > MaxMetadataKeyLen {
			return nil, nil, fmt.Errorf("metadata: keys must be 1-%d characters, got %q", MaxMetadataKeyLen, k)
		}
		if key != k {
			return nil, nil, fmt.Errorf("metadata: key %q has leading or trailing spaces", k)
		}
		switch val := v.(type) {
		case nil:
			remove = append(remove, key)
		case string:
			if len(val) > MaxMetadataValueLen {
				return nil, nil, fmt.Errorf("metadata: %q is longer than %d characters", key, MaxMetadataValueLen)
			}
			set[key] = val
		case bool, float64, float32, int, int64, int32:
			set[key] = val
		default:
			return nil, nil, fmt.Errorf("metadata: %q must be a string, number, or boolean (null removes it)", key)
		}
	}
	sort.Strings(remove)
	return set, remove, nil
}

// Clean validates metadata given whole (on create, or in an import), where
// null values are simply dropped.
func (m Metadata) Clean() (Metadata, error) {
	set, _, err := MetadataPatch(m).Split()
	return set, err
}

// Merge returns base with over's keys on top (over wins).
func (m Metadata) Merge(over Metadata) Metadata {
	out := Metadata{}
	for k, v := range m {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}
