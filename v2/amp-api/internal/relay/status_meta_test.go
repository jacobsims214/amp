package relay

import (
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
)

// statusMeta keeps only the model and the current ticket from a status
// (#2389), and treats anything else as null.
func TestStatusMeta(t *testing.T) {
	long := strings.Repeat("é", 150) // 300 bytes
	tests := []struct {
		name     string
		status   map[string]any
		model    string
		ticketID int64
		ticketNm string
	}{
		{"empty", map[string]any{}, "", 0, ""},
		{"all", map[string]any{"model": " m ", "ticket_id": int64(7), "ticket_name": " Ship it "}, "m", 7, "Ship it"},
		{"no name (older harness)", map[string]any{"model": "m", "ticket_id": int64(7)}, "m", 7, ""},
		{"name without a ticket", map[string]any{"ticket_id": nil, "ticket_name": "x"}, "", 0, ""},
		{"wrong types", map[string]any{"model": 3, "ticket_id": "7", "ticket_name": 1}, "", 0, ""},
		{"non-positive ticket", map[string]any{"ticket_id": int64(0)}, "", 0, ""},
		{"blank model", map[string]any{"model": "  "}, "", 0, ""},
		{"clipped", map[string]any{"model": long, "ticket_id": int64(1), "ticket_name": long}, strings.Repeat("é", 100), 1, strings.Repeat("é", 100)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := statusMeta(tc.status)
			if got := deref(m.Model); (tc.model == "" && got != nil) || (tc.model != "" && got != tc.model) {
				t.Errorf("model = %v, want %q", got, tc.model)
			}
			if got := deref(m.TicketID); (tc.ticketID == 0 && got != nil) || (tc.ticketID != 0 && got != tc.ticketID) {
				t.Errorf("ticket_id = %v, want %d", got, tc.ticketID)
			}
			if got := deref(m.TicketName); (tc.ticketNm == "" && got != nil) || (tc.ticketNm != "" && got != tc.ticketNm) {
				t.Errorf("ticket_name = %v, want %q", got, tc.ticketNm)
			}
		})
	}
}

// setWorkers takes the worker count from a status for the sessions list
// (#4898): a missing or wrong-typed member reads as zero, and a hostile count
// is bounded.
func TestSetWorkers(t *testing.T) {
	tests := []struct {
		name   string
		status map[string]any
		want   int
	}{
		{"missing (an older harness)", map[string]any{"model": "m"}, 0},
		{"counted", map[string]any{"workers_running": int64(3)}, 3},
		{"none running", map[string]any{"workers_running": int64(0)}, 0},
		{"wrong type", map[string]any{"workers_running": "3"}, 0},
		{"negative", map[string]any{"workers_running": int64(-1)}, 0},
		{"absurd", map[string]any{"workers_running": int64(1) << 40}, maxWorkersRunning},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newLive(&domain.RemoteSession{ID: "rs_x", HarnessPub: "x"})
			l.mu.Lock()
			l.setWorkers(tc.status)
			l.mu.Unlock()
			if got := l.Workers(); got != tc.want {
				t.Errorf("Workers() = %d, want %d", got, tc.want)
			}
		})
	}
}
