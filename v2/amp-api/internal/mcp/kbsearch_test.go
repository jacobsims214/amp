package mcp

import "testing"

func TestKBSearchLimit(t *testing.T) {
	tests := []struct {
		name string
		args map[string]interface{}
		want int
	}{
		{"absent uses default", map[string]interface{}{}, 3},
		{"nil uses default", map[string]interface{}{"limit": nil}, 3},
		{"zero uses default", map[string]interface{}{"limit": float64(0)}, 3},
		{"negative uses default", map[string]interface{}{"limit": float64(-5)}, 3},
		{"in range kept", map[string]interface{}{"limit": float64(7)}, 7},
		{"max kept", map[string]interface{}{"limit": float64(10)}, 10},
		{"over max clamped", map[string]interface{}{"limit": float64(500)}, 10},
		{"int over max clamped", map[string]interface{}{"limit": 11}, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := kbSearchLimit(tt.args); got != tt.want {
				t.Errorf("kbSearchLimit(%v) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}
