package domain

import "testing"

// AMP #2596: which runs land on an integration branch, and who opts out of
// waiting on merges.
func TestRunLandsOn(t *testing.T) {
	tests := []struct {
		name string
		run  Run
		ib   string
		want bool
	}{
		{"no integration branch", Run{Branch: "harness-m5"}, "", false},
		{"merged into it, no PR", Run{Branch: "harness-m5"}, "harness-m5", true},
		{"PR targeting it", Run{Branch: "feat/x", PRURL: "u", BaseBranch: "harness-m5"}, "harness-m5", true},
		{"its own PR to main", Run{Branch: "harness-m5", PRURL: "u", BaseBranch: "main"}, "harness-m5", false},
		{"its PR, base not said", Run{Branch: "harness-m5", PRURL: "u"}, "harness-m5", false},
		{"other branch", Run{Branch: "feat/x"}, "harness-m5", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.run.LandsOn(tt.ib); got != tt.want {
				t.Fatalf("LandsOn(%q) = %v, want %v", tt.ib, got, tt.want)
			}
		})
	}
}

func TestMetadataMergeGateKeys(t *testing.T) {
	tests := []struct {
		meta  Metadata
		waits bool
	}{
		{Metadata{}, true},
		{Metadata{MetaWaitsForMerge: false}, false},
		{Metadata{MetaWaitsForMerge: "False"}, false},
		{Metadata{MetaWaitsForMerge: true}, true},
		{Metadata{MetaWaitsForMerge: "no"}, true},
	}
	for _, tt := range tests {
		if got := tt.meta.WaitsForMerge(); got != tt.waits {
			t.Fatalf("%v.WaitsForMerge() = %v, want %v", tt.meta, got, tt.waits)
		}
	}
	if ib := (Metadata{MetaIntegrationBranch: " harness-m5 "}).IntegrationBranch(); ib != "harness-m5" {
		t.Fatalf("IntegrationBranch = %q", ib)
	}
	if !(MetadataPatch{MetaWaitsForMerge: nil}).TouchesMergeGate() || (MetadataPatch{"jira": "X"}).TouchesMergeGate() {
		t.Fatal("TouchesMergeGate")
	}
}
