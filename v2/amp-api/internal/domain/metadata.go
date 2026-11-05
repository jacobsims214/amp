package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
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

// Merge-gate metadata keys (AMP #2596), inherited like any metadata
// (project → epic → story → ticket).
const (
	// MetaIntegrationBranch names the branch an epic's or story's work is
	// merged into before one integration PR takes it to main. A run whose
	// branch is that branch doesn't hold its ticket's dependents: the work
	// is where it belongs. The ticket stays unmerged (merge_state none) until
	// the integration branch's merge is reported.
	MetaIntegrationBranch = "integration_branch"
	// MetaWaitsForMerge false, on a dependent, means it waits only for its
	// dependencies to complete, not for their merges (a review of an open
	// PR). Any other value, or none, keeps the merge gate.
	MetaWaitsForMerge = "waits_for_merge"
	// MetaMergeFor, on a human merge step (performer "human"), is the
	// numeric id of the ticket whose PR merge the step tracks: "this person's
	// step is the gate for ticket N's merge". It makes the plan's step and
	// My work's Merge card for that PR one action instead of two (AMP #3009).
	// Read a ticket's OWN metadata for it, never inherited metadata: a story
	// or epic must not point every ticket under it at one PR.
	MetaMergeFor = "merge_for"
)

// IntegrationBranch is m's integration branch, or "".
func (m Metadata) IntegrationBranch() string {
	s, _ := m[MetaIntegrationBranch].(string)
	return strings.TrimSpace(s)
}

// WaitsForMerge reports whether a ticket with effective metadata m waits on
// its dependencies' merges: yes unless waits_for_merge is false.
func (m Metadata) WaitsForMerge() bool {
	switch v := m[MetaWaitsForMerge].(type) {
	case bool:
		return v
	case string:
		return !strings.EqualFold(strings.TrimSpace(v), "false")
	}
	return true
}

// TouchesMergeGate reports whether a metadata change sets or removes a key
// that decides who waits on a merge.
func (p MetadataPatch) TouchesMergeGate() bool {
	_, ib := p[MetaIntegrationBranch]
	_, wm := p[MetaWaitsForMerge]
	return ib || wm
}

// MergeFor is m's merge_for link: the id of the ticket whose PR merge a
// human merge step tracks, and whether one is set. It reports false for a
// value that isn't a positive ticket id, so a malformed link reads as no
// link (ParseMergeFor is the strict reading, for the write paths).
func (m Metadata) MergeFor() (int, bool) {
	id, err := ParseMergeFor(m[MetaMergeFor])
	return id, err == nil && id > 0
}

// ParseMergeFor reads a merge_for value strictly: a positive ticket id as a
// number, a string of digits, or "#123". A nil value is no link (an id of 0
// and no error); anything else is an error, so a write path can refuse it
// instead of storing a link nothing reads.
func ParseMergeFor(v any) (int, error) {
	switch n := v.(type) {
	case nil:
		return 0, nil
	case bool:
		return 0, fmt.Errorf("merge_for must be a ticket id, not %v", n)
	case float64:
		return mergeForID(int64(n), math.Trunc(n) == n, strconv.FormatFloat(n, 'f', -1, 64))
	case float32:
		f := float64(n)
		return mergeForID(int64(n), math.Trunc(f) == f, strconv.FormatFloat(f, 'f', -1, 64))
	case int:
		return mergeForID(int64(n), true, strconv.Itoa(n))
	case int32:
		return mergeForID(int64(n), true, strconv.FormatInt(int64(n), 10))
	case int64:
		return mergeForID(n, true, strconv.FormatInt(n, 10))
	case json.Number:
		i, err := n.Int64()
		return mergeForID(i, err == nil, n.String())
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, nil
		}
		i, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
		return mergeForID(i, err == nil, n)
	}
	return 0, fmt.Errorf("merge_for must be a ticket id (a number), not %T", v)
}

// mergeForID is ParseMergeFor's numeric half: whole, positive, in range.
func mergeForID(n int64, whole bool, shown string) (int, error) {
	if !whole || n <= 0 || n > math.MaxInt32 {
		return 0, fmt.Errorf("merge_for must be a ticket id (a positive whole number), got %s", shown)
	}
	return int(n), nil
}
