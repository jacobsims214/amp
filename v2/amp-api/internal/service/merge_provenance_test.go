package service

import (
	"testing"

	"github.com/simstech/amp-api/internal/domain"
)

// latestOpeningRun picks the run a done ticket's merge gate follows: the
// last one that put work up. Which run that is decides whether a correction
// releases the gate (AMP #3002).
func TestLatestOpeningRun(t *testing.T) {
	const ib = "fix/actionable-merge-workflow"
	run := func(branch, pr, base string) domain.Run {
		return domain.Run{Branch: branch, PRURL: pr, BaseBranch: base}
	}
	tests := []struct {
		name string
		runs []domain.Run
		want domain.Run
		// landed is whether the chosen run puts the ticket's work on ib.
		landed bool
	}{
		{
			name: "no runs at all",
			runs: nil,
		},
		{
			name: "no run put work up (usage only)",
			runs: []domain.Run{{Model: "m"}, {}},
		},
		{
			name: "#2966: the worktree-branch completion is the only work run",
			runs: []domain.Run{run("amp-2966", "", "")},
			want: run("amp-2966", "", ""),
		},
		{
			name:   "#2966 corrected: the later integration run decides",
			runs:   []domain.Run{run("amp-2966", "", ""), run(ib, "", ib)},
			want:   run(ib, "", ib),
			landed: true,
		},
		{
			name:   "a later PR to main gates again, latest wins",
			runs:   []domain.Run{run("amp-1", "", ""), run(ib, "", ib), run(ib, "https://pr/1", "main")},
			want:   run(ib, "https://pr/1", "main"),
			landed: false, // an integration branch's own PR to main isn't landing on it
		},
		{
			name:   "a PR into the integration branch landed too",
			runs:   []domain.Run{run("amp-1", "", ""), run("feat/x", "https://pr/2", ib)},
			want:   run("feat/x", "https://pr/2", ib),
			landed: true,
		},
		{
			name:   "a PR whose base was not reported lands nowhere",
			runs:   []domain.Run{run("amp-1", "", ""), run(ib, "https://pr/3", "")},
			want:   run(ib, "https://pr/3", ""),
			landed: false,
		},
		{
			name:   "a later PR-elsewhere run is what the gate follows",
			runs:   []domain.Run{run("amp-1", "", ""), run("amp-1", "https://pr/4", "main")},
			want:   run("amp-1", "https://pr/4", "main"),
			landed: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := latestOpeningRun(tt.runs)
			if got.Branch != tt.want.Branch || got.PRURL != tt.want.PRURL || got.BaseBranch != tt.want.BaseBranch {
				t.Fatalf("latestOpeningRun = %+v, want %+v", got, tt.want)
			}
			if landed := got.LandsOn(ib); landed != tt.landed {
				t.Fatalf("chosen run LandsOn(%q) = %v, want %v", ib, landed, tt.landed)
			}
		})
	}
}
