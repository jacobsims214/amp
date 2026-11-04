package service

import "testing"

func TestAwaitsArtifactReview(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		id     int
		want   bool
	}{
		{"designer asks with the url", "Review the Settings mockup: https://amp.example.com/artifacts/12. Save at the top or bottom?", 12, true},
		{"url at the end", "Please review https://amp.example.com/artifacts/12", 12, true},
		{"url with a version", "Review https://amp.example.com/artifacts/12?version=3", 12, true},
		{"by number, no public url", "Review artifact #12?", 12, true},
		{"by number without #", "Please look at Artifact 12 and approve", 12, true},
		{"another artifact's url", "Review https://amp.example.com/artifacts/13", 12, false},
		{"longer id with the same prefix", "Review https://amp.example.com/artifacts/123", 12, false},
		{"another artifact by number", "Review artifact #120?", 12, false},
		{"an unrelated question", "Avalara or Stripe Tax?", 12, false},
		{"unrelated question with a number", "Which of the 12 regions first?", 12, false},
		{"a scheduled hold", "scheduled: review artifact #12 on Monday", 12, false},
		{"no reason", "", 12, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := awaitsArtifactReview(tt.reason, tt.id); got != tt.want {
				t.Errorf("awaitsArtifactReview(%q, %d) = %v, want %v", tt.reason, tt.id, got, tt.want)
			}
		})
	}
}
