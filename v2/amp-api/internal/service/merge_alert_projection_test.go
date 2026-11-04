package service

// AMP #3158: the classifier (#3133) applied to the waiting projection.
//
// Two layers are tested here, and the split matters:
//
//   - mergeAlertInputFromView is the MAPPING from cached facts to the
//     classifier's input. It is pure, so every arm and every degradation is
//     reachable without Postgres, GitHub or a clock — which is how the safety
//     rules are pinned: a hint never becomes a verified target, an untrusted
//     fact never becomes evidence, and a rule the server cannot parse never
//     becomes "no rule".
//   - dropNonAlertMerges is the projection decision: which arms reach a person.
//     internal (agent organization) and none (already merged) are dropped BEFORE
//     the plan-step fold, so a person's merged step reappears as its own Do
//     item rather than vanishing with the card.
//
// What is deliberately NOT here: the gate. Nothing in this file can change
// merge_state or release a dependent, and the ticket-level test
// (cmd/server TestMergeAlertPolicy_*) pins that a suppressed alert leaves the
// gate exactly as it was.

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/githubfacts"
	"github.com/simstech/amp-api/internal/repository"
)

// view is a helper for building a GitHubFactsView the way the coordinator
// does: every fact's judgement is set explicitly, never inferred.
type factsView struct {
	repoTrusted   bool
	defaultBranch string
	repoSource    repository.GitHubDefaultBranchSource
	repoReadAt    time.Time

	target       string
	targetSource githubfacts.Source

	branchTrusted bool
	branchPartial bool
	branchRules   []byte
	branchReadAt  time.Time

	pullTrusted  bool
	pullBase     string
	pullState    string
	reviewState  string
	mergeable    string
	requestedBy  []string
	pullReadAt   time.Time
	anyPullFacts bool
}

// view builds a GitHubFactsView the way the coordinator does. The important
// fidelity detail: the coordinator fills each fact's VALUE whether or not it is
// Trusted (a stale row keeps the branch it last read, and a stale rules read keeps
// its rules), and Trusted is the separate judgement. A helper that blanked the
// value when untrusted would let the Trusted guard be deleted with no test
// noticing — which is exactly the bug this mapping must not have.
func (f factsView) view() GitHubFactsView {
	v := GitHubFactsView{ProjectID: 1, Slug: "acme/rocket", PR: 7,
		Target: f.target, TargetSource: f.targetSource}
	if f.defaultBranch == "" && !f.repoTrusted {
		v.Repo = GitHubRepoFactsView{Unknown: "the repository's default branch has not been read"}
	} else {
		v.Repo = GitHubRepoFactsView{DefaultBranch: f.defaultBranch, Source: f.repoSource,
			ReadAt: f.repoReadAt, Fresh: f.repoTrusted, Trusted: f.repoTrusted}
		if !f.repoTrusted {
			v.Repo.Unknown = "the repository's default branch was read longer ago than the TTL"
		}
	}
	if f.branchRules == nil && !f.branchTrusted {
		v.Branch = GitHubBranchFactsView{Unknown: "the rules of the target branch have not been read"}
	} else {
		rules := f.branchRules
		if rules == nil {
			rules = []byte("[]")
		}
		v.Branch = GitHubBranchFactsView{Rules: rules, ReadAt: f.branchReadAt, Fresh: f.branchTrusted,
			Trusted: f.branchTrusted, Complete: f.branchTrusted && !f.branchPartial,
			Partial: f.branchTrusted && f.branchPartial}
		if !f.branchTrusted {
			v.Branch.Unknown = "the rules of the target branch were read longer ago than the TTL"
		}
	}
	if !f.pullTrusted && f.pullBase == "" {
		v.Pull = GitHubPullFactsView{Unknown: "the pull request's facts have not been read"}
	} else {
		v.Pull = GitHubPullFactsView{BaseBranch: f.pullBase, RequestedReviewers: f.requestedBy,
			ReadAt: f.pullReadAt, Fresh: f.pullTrusted, Trusted: f.pullTrusted,
			Facts: &repository.GitHubPullFacts{Number: 7, BaseBranch: f.pullBase, State: f.pullState,
				ReviewDecision: f.reviewState, MergeableState: f.mergeable,
				RequestedReviewers: f.requestedBy, FetchedAt: &f.pullReadAt}}
		if !f.pullTrusted {
			v.Pull.Unknown = "the pull request's facts were read longer ago than the TTL"
		}
	}
	return v
}

// ruleJSON is one stored rule row in 044's reduced shape (what
// githubFactRulesJSON writes), so the mapping is tested against the bytes the
// cache actually holds rather than against a hand-built struct.
type ruleJSON struct {
	Type              string         `json:"type"`
	RulesetID         int64          `json:"ruleset_id,omitempty"`
	RulesetSource     string         `json:"ruleset_source,omitempty"`
	RulesetSourceType string         `json:"ruleset_source_type,omitempty"`
	Parameters        map[string]any `json:"parameters,omitempty"`
}

func rulesOf(t *testing.T, rules ...ruleJSON) []byte {
	t.Helper()
	b, err := json.Marshal(rules)
	if err != nil {
		t.Fatalf("marshal rules: %v", err)
	}
	return b
}

// pullRule is a pull_request rule with the parameters under test.
func pullRule(sourceType, source string, params map[string]any) ruleJSON {
	return ruleJSON{Type: "pull_request", RulesetSource: source, RulesetSourceType: sourceType, Parameters: params}
}

var nowForView = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// TestMergeAlertProjectionFourArmsAndMerged pins, for each arm of the policy,
// that the projection actually produces it from cached facts — and that the
// merged exclusion wins over everything.
func TestMergeAlertProjectionFourArmsAndMerged(t *testing.T) {
	// The repository's ruleset demands one approving review from a named user,
	// who is NOT the ticket's owner (AMP stores no GitHub login, so the policy
	// cannot attribute it): that is the honest unknown, not a rule alert.
	namedReviewer := rulesOf(t, pullRule("Repository", "main-protection", map[string]any{
		"required_approving_review_count": 1,
		"required_reviewers":              []map[string]any{{"type": "User", "login": "someone-else"}},
	}))

	tests := []struct {
		name       string
		view       factsView
		merged     bool
		wantKind   string
		wantAsks   bool
		wantTarget string
	}{
		{
			name: "default_branch: the PR targets the detected default branch",
			view: factsView{repoTrusted: true, defaultBranch: "main", repoSource: repository.GitHubBranchSourceREST,
				repoReadAt: nowForView, target: "main", targetSource: githubfacts.SourcePull,
				pullTrusted: true, pullBase: "main", pullReadAt: nowForView},
			wantKind: MergeAlertDefaultBranch, wantAsks: true, wantTarget: "main",
		},
		{
			name: "rule: a rule requires nobody in particular, but the names are unreadable to AMP",
			// required_approving_review_count > 0 is person-indirect (§6), so even
			// with fresh complete rules this is unknown — never internal.
			view: factsView{repoTrusted: true, defaultBranch: "main", repoSource: repository.GitHubBranchSourceREST,
				repoReadAt: nowForView, target: "feat/x", targetSource: githubfacts.SourcePull,
				branchTrusted: true, branchReadAt: nowForView, branchRules: namedReviewer,
				pullTrusted: true, pullBase: "feat/x", pullReadAt: nowForView},
			wantKind: MergeAlertUnknown, wantAsks: true, wantTarget: "feat/x",
		},
		{
			name: "internal: non-default target, complete fresh rules, no rule requires anyone",
			view: factsView{repoTrusted: true, defaultBranch: "main", repoSource: repository.GitHubBranchSourceREST,
				repoReadAt: nowForView, target: "fix/integration", targetSource: githubfacts.SourcePull,
				branchTrusted: true, branchReadAt: nowForView, branchRules: []byte("[]"),
				pullTrusted: true, pullBase: "fix/integration", pullReadAt: nowForView},
			wantKind: MergeAlertInternal, wantAsks: false, wantTarget: "fix/integration",
		},
		{
			name: "unknown: nothing has been read",
			view: factsView{},
			// No facts, no target: the floor.
			wantKind: MergeAlertUnknown, wantAsks: true, wantTarget: "",
		},
		{
			name: "unknown: the target is only the run's hint, never the PR's own base",
			// A hint cannot decide anything: the default branch was read and the
			// rule set looks complete, but nothing verified what the PR targets.
			view: factsView{repoTrusted: true, defaultBranch: "main", repoSource: repository.GitHubBranchSourceREST,
				repoReadAt: nowForView, target: "fix/integration", targetSource: githubfacts.SourceHint,
				branchTrusted: true, branchReadAt: nowForView, branchRules: []byte("[]")},
			wantKind: MergeAlertUnknown, wantAsks: true, wantTarget: "",
		},
		{
			name: "none: the merge is already recorded, whatever the evidence says",
			view: factsView{repoTrusted: true, defaultBranch: "main", repoSource: repository.GitHubBranchSourceREST,
				repoReadAt: nowForView, target: "main", targetSource: githubfacts.SourcePull,
				pullTrusted: true, pullBase: "main", pullState: "merged", pullReadAt: nowForView},
			wantKind: MergeAlertNone, wantAsks: false, wantTarget: "main",
		},
		{
			name: "none: merged beats an unreadable repository too",
			view: factsView{pullTrusted: true, pullBase: "main", pullState: "merged", pullReadAt: nowForView},
			// The pull read is trusted but nothing names the target as the PR's
			// base, so no target is reported; merged still decides first.
			wantKind: MergeAlertNone, wantAsks: false, wantTarget: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := mergeAlertInputFromView(tt.view.view(), MergeAlertInput{Now: nowForView})
			if tt.merged {
				in.Merged = true
			}
			alert := ClassifyMergeAlert(in)
			if alert.Kind != tt.wantKind {
				t.Fatalf("arm = %q (%s), want %q", alert.Kind, alert.Reason, tt.wantKind)
			}
			if got := mergeAlertAsksPerson(alert.Kind); got != tt.wantAsks {
				t.Fatalf("asks a person = %v for arm %q, want %v", got, alert.Kind, tt.wantAsks)
			}
			if alert.Reason == "" {
				t.Error("every arm must say why")
			}
			if got := mergeAlertVerifiedTarget(tt.view.view()); got != tt.wantTarget {
				t.Fatalf("verified target = %q, want %q", got, tt.wantTarget)
			}
			// A rule alert must name the rule; nothing else may.
			if (alert.Kind == MergeAlertRule) != (alert.Rule != "") {
				t.Fatalf("rule-box text %q for arm %q", alert.Rule, alert.Kind)
			}
		})
	}
}

// TestMergeAlertProjectionMergedWinsOverEverything is the merged exclusion as
// its own guard: GitHub saying "merged" outranks every other fact, including
// facts that would otherwise raise the state-9 warn chip (already-merged work
// must never ask for acknowledgement — user direction 2026-11-02).
func TestMergeAlertProjectionMergedWinsOverEverything(t *testing.T) {
	partial := factsView{repoTrusted: true, defaultBranch: "main", repoSource: repository.GitHubBranchSourceREST,
		repoReadAt: nowForView, target: "feat/x", targetSource: githubfacts.SourcePull,
		branchTrusted: true, branchPartial: true, branchReadAt: nowForView,
		pullTrusted: true, pullBase: "feat/x", pullState: "merged", pullReadAt: nowForView}
	in := mergeAlertInputFromView(partial.view(), MergeAlertInput{Now: nowForView})
	if !in.Merged {
		t.Fatal("a pull read that says merged did not reach the classifier")
	}
	alert := ClassifyMergeAlert(in)
	if alert.Kind != MergeAlertNone {
		t.Fatalf("merged + partial rules = %q, want none", alert.Kind)
	}
	if mergeAlertAsksPerson(alert.Kind) {
		t.Fatal("already-merged work asks a person")
	}
}

// TestMergeAlertProjectionUntrustedFactsAreNotEvidence degrades the one input
// that reaches each arm, one fact at a time, and asserts nothing is ever read
// as *better* than it is: an untrusted default branch, an untrusted rules read
// and an unverified target each fall back to unknown, never to internal and
// never to an alert the evidence does not support.
func TestMergeAlertProjectionUntrustedFactsAreNotEvidence(t *testing.T) {
	base := factsView{repoTrusted: true, defaultBranch: "main", repoSource: repository.GitHubBranchSourceREST,
		repoReadAt: nowForView, target: "fix/integration", targetSource: githubfacts.SourcePull,
		branchTrusted: true, branchReadAt: nowForView, branchRules: []byte("[]"),
		pullTrusted: true, pullBase: "fix/integration", pullReadAt: nowForView}

	// Sanity: the base really is internal, so each degradation below is a real
	// loss of evidence rather than a test of an already-unknown input.
	if got := ClassifyMergeAlert(mergeAlertInputFromView(base.view(), MergeAlertInput{Now: nowForView})).Kind; got != MergeAlertInternal {
		t.Fatalf("baseline arm = %q, want internal", got)
	}

	degradations := []struct {
		name string
		mut  func(*factsView)
	}{
		{"default branch not trusted", func(f *factsView) { f.repoTrusted = false }},
		{"rules not trusted", func(f *factsView) { f.branchTrusted = false }},
		{"target is a hint rather than the PR's base", func(f *factsView) { f.targetSource = githubfacts.SourceHint }},
		{"target empty", func(f *factsView) { f.target = "" }},
	}
	for _, d := range degradations {
		t.Run(d.name, func(t *testing.T) {
			f := base
			d.mut(&f)
			got := ClassifyMergeAlert(mergeAlertInputFromView(f.view(), MergeAlertInput{Now: nowForView})).Kind
			if got == MergeAlertInternal {
				t.Fatalf("%s quieted the item to internal: absence of evidence can never do that", d.name)
			}
			if got == MergeAlertDefaultBranch {
				t.Fatalf("%s produced a default-branch alert it cannot support", d.name)
			}
		})
	}
}

// TestMergeAlertProjectionStalePullFactsDoNotDisarmCompleteRules is the
// deliberate exception to the rule above, written down so it is not "fixed"
// into a bug later. Losing the pull request's own facts (they are stale, so not
// Trusted) does NOT unseat a complete, trusted rules read: the item still quiets
// to internal, because the evidence that quiets it — "no rule on this branch
// requires your approval" — is positive knowledge about the branch, not an
// absence inferred from the missing PR facts.
//
// What the pull facts govern is the *contradiction* check (§3.3: complete rules
// that require nothing, while the PR reports a review is required) and the
// partial-rules fallback. Those are the two places where missing PR facts must
// and do stay unknown — see the partial case below.
func TestMergeAlertProjectionStalePullFactsDoNotDisarmCompleteRules(t *testing.T) {
	stale := factsView{repoTrusted: true, defaultBranch: "main", repoSource: repository.GitHubBranchSourceREST,
		repoReadAt: nowForView, target: "fix/integration", targetSource: githubfacts.SourcePull,
		branchTrusted: true, branchReadAt: nowForView, branchRules: []byte("[]"),
		pullTrusted: false, pullBase: "fix/integration"}
	alert := ClassifyMergeAlert(mergeAlertInputFromView(stale.view(), MergeAlertInput{Now: nowForView}))
	if alert.Kind != MergeAlertInternal {
		t.Fatalf("complete trusted rules + stale pull facts = %q (%s), want internal", alert.Kind, alert.Reason)
	}
	// The target is still the PR's own base (a verified read does not stop
	// naming what the PR targeted), so the card can still say where it goes.
	if got := mergeAlertVerifiedTarget(stale.view()); got != "fix/integration" {
		t.Fatalf("target = %q, want the pull read's base", got)
	}
}

// TestMergeAlertProjectionPartialRulesStillFallBackToPRSignals: a partial rules
// read cannot be "no rule", and it does not throw away the PR-level signals that
// may legitimately stand in for it (§3.3). With complete-but-partial evidence
// and a requested reviewer who is the person, the alert fires; without the
// person among the reviewers it stays unknown.
func TestMergeAlertProjectionPartialRulesStillFallBackToPRSignals(t *testing.T) {
	base := factsView{repoTrusted: true, defaultBranch: "main", repoSource: repository.GitHubBranchSourceREST,
		repoReadAt: nowForView, target: "feat/x", targetSource: githubfacts.SourcePull,
		branchTrusted: true, branchPartial: true, branchReadAt: nowForView, branchRules: []byte("[]"),
		pullTrusted: true, pullBase: "feat/x", reviewState: "review_required", pullReadAt: nowForView}

	// Nobody can be matched: AMP has no GitHub login for the owner, so a
	// partial read stays unknown rather than alerting.
	in := mergeAlertInputFromView(base.view(), MergeAlertInput{Now: nowForView})
	if got := ClassifyMergeAlert(in).Kind; got != MergeAlertUnknown {
		t.Fatalf("partial rules with no attributable person = %q, want unknown", got)
	}

	// With a person whose login IS requested, the same facts are a rule alert.
	withPerson := base
	withPerson.requestedBy = []string{"octocat"}
	in = mergeAlertInputFromView(withPerson.view(), MergeAlertInput{Now: nowForView})
	in.Person = MergeAlertPerson{Login: "octocat"}
	alert := ClassifyMergeAlert(in)
	if alert.Kind != MergeAlertRule {
		t.Fatalf("partial rules + required review + requested reviewer = %q (%s), want rule", alert.Kind, alert.Reason)
	}
	if alert.Rule == "" {
		t.Error("a rule alert must name what is required")
	}
}

// TestMergeAlertProjectionCompleteRulesNameAPersonWhoMustApprove: when the
// evidence CAN attribute a rule, a rule alert fires and the rule-box names the
// ruleset. This is the state-3 card.
func TestMergeAlertProjectionCompleteRulesNameAPersonWhoMustApprove(t *testing.T) {
	rules := rulesOf(t,
		ruleJSON{Type: "deletion"},
		pullRule("Organization", "org-protection", map[string]any{
			"required_approving_review_count": 1,
			"required_reviewers":              []map[string]any{{"type": "User", "login": "octocat"}},
		}),
	)
	f := factsView{repoTrusted: true, defaultBranch: "main", repoSource: repository.GitHubBranchSourceREST,
		repoReadAt: nowForView, target: "feat/billing", targetSource: githubfacts.SourcePull,
		branchTrusted: true, branchReadAt: nowForView, branchRules: rules,
		pullTrusted: true, pullBase: "feat/billing", pullReadAt: nowForView}
	in := mergeAlertInputFromView(f.view(), MergeAlertInput{Now: nowForView})
	in.Person = MergeAlertPerson{Login: "octocat"}
	alert := ClassifyMergeAlert(in)
	if alert.Kind != MergeAlertRule {
		t.Fatalf("arm = %q (%s), want rule", alert.Kind, alert.Reason)
	}
	for _, want := range []string{"Organization", "org-protection", "@octocat"} {
		if !contains(alert.Rule, want) {
			t.Fatalf("rule-box %q does not name %q", alert.Rule, want)
		}
	}
	if alert.DefaultBranch != "main" {
		t.Fatalf("repo chip = %q, want main", alert.DefaultBranch)
	}
}

// contains is a tiny substring test, kept local so this file needs no import
// for it.
func contains(hay, needle string) bool {
	return len(needle) == 0 || len(hay) >= len(needle) && indexOf(hay, needle) >= 0
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestMergeAlertPullRequestRuleMergesEveryRuleset: two rulesets can cover one
// branch, and the classifier judges ONE rule. Narrowing to the first would
// under-ask — dropping a reviewer or a code-owner demand another rule makes — so
// the demands are unioned. Over-asking is recoverable; silently not asking is
// the failure the policy exists to prevent.
func TestMergeAlertPullRequestRuleMergesEveryRuleset(t *testing.T) {
	rules := rulesOf(t,
		// The organization's rule demands a code owner.
		pullRule("Organization", "org-protection", map[string]any{"require_code_owner_review": true}),
		// The repository's demands three approvals from nobody in particular.
		pullRule("Repository", "repo-protection", map[string]any{"required_approving_review_count": 3}),
		// A non-pull_request rule must not contribute a demand.
		ruleJSON{Type: "required_status_checks"},
	)
	rule, found, unreadable := mergeAlertPullRequestRule(rules)
	if unreadable {
		t.Fatal("valid rules reported as unreadable")
	}
	if !found {
		t.Fatal("no pull_request rule found in a list that has two")
	}
	if !rule.RequireCodeOwnerReview {
		t.Error("the organization's code-owner demand was dropped")
	}
	if rule.RequiredApprovingReviewCount != 3 {
		t.Errorf("approval count = %d, want the largest (3)", rule.RequiredApprovingReviewCount)
	}
	// The rule-box names the first rule that demands something.
	if rule.RulesetSource != "Organization" || rule.RulesetName != "org-protection" {
		t.Errorf("named rule = %q/%q, want the organization's", rule.RulesetSource, rule.RulesetName)
	}
}

// TestMergeAlertPullRequestRuleOnlyPullRequestRulesCount: a branch protected
// only by rules that block merges without asking a person reduces to "no rule",
// which is a real answer (complete evidence) and never an alert.
func TestMergeAlertPullRequestRuleOnlyPullRequestRulesCount(t *testing.T) {
	for _, tt := range []struct {
		name  string
		rules []byte
		found bool
	}{
		{"empty array", []byte("[]"), false},
		{"nothing stored", nil, false},
		{"only blocking rules", rulesOf(t, ruleJSON{Type: "deletion"}, ruleJSON{Type: "non_fast_forward"}), false},
		{"a pull_request rule", rulesOf(t, pullRule("Repository", "p", nil)), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, found, unreadable := mergeAlertPullRequestRule(tt.rules)
			if unreadable {
				t.Fatal("valid rules reported as unreadable")
			}
			if found != tt.found {
				t.Fatalf("found = %v, want %v", found, tt.found)
			}
		})
	}
}

// TestMergeAlertProjectionUnreadableRulesNeverQuiet: a rules column this server
// cannot parse proves nothing about the absence of a rule, so it degrades to
// partial — the classifier may alert or stay unknown, and may never say internal.
func TestMergeAlertProjectionUnreadableRulesNeverQuiet(t *testing.T) {
	f := factsView{repoTrusted: true, defaultBranch: "main", repoSource: repository.GitHubBranchSourceREST,
		repoReadAt: nowForView, target: "fix/integration", targetSource: githubfacts.SourcePull,
		branchTrusted: true, branchReadAt: nowForView, branchRules: []byte(`{"not":"an array"}`),
		pullTrusted: true, pullBase: "fix/integration", pullReadAt: nowForView}
	in := mergeAlertInputFromView(f.view(), MergeAlertInput{Now: nowForView})
	if !in.Rules.Partial {
		t.Fatal("unreadable rules were not marked partial: they could then quiet the item")
	}
	if got := ClassifyMergeAlert(in).Kind; got == MergeAlertInternal {
		t.Fatalf("unreadable rules quieted the item to internal")
	}
}

// TestMergeAlertTeamReviewerKeepsItsSlug: GitHub returns users and teams in one
// array. A team entry is carried as its slug and marked Team, so the classifier
// refuses to guess its membership instead of matching the slug as a login.
func TestMergeAlertTeamReviewerKeepsItsSlug(t *testing.T) {
	rules := rulesOf(t, pullRule("Repository", "p", map[string]any{
		"required_reviewers": []map[string]any{{"type": "Team", "slug": "platform"}},
	}))
	rule, found, _ := mergeAlertPullRequestRule(rules)
	if !found || len(rule.RequiredReviewers) != 1 {
		t.Fatalf("reviewers = %+v (found %v)", rule, found)
	}
	rev := rule.RequiredReviewers[0]
	if rev.Login != "platform" || !rev.Team {
		t.Fatalf("team reviewer = %+v, want login platform marked as a team", rev)
	}
	// And a person whose login happens to be "platform" is still not matched:
	// team membership is unresolved (§6), so this is unknown, never internal.
	f := factsView{repoTrusted: true, defaultBranch: "main", repoSource: repository.GitHubBranchSourceREST,
		repoReadAt: nowForView, target: "feat/x", targetSource: githubfacts.SourcePull,
		branchTrusted: true, branchReadAt: nowForView, branchRules: rules,
		pullTrusted: true, pullBase: "feat/x", pullReadAt: nowForView}
	in := mergeAlertInputFromView(f.view(), MergeAlertInput{Now: nowForView})
	in.Person = MergeAlertPerson{Login: "platform"}
	if got := ClassifyMergeAlert(in).Kind; got != MergeAlertUnknown {
		t.Fatalf("a team's slug matched as a person's login: arm = %q", got)
	}
}

// TestDropNonAlertMergesKeepsOnlyHumanAlerts is the projection decision itself:
// internal and already-merged items disappear, every arm that asks a person
// stays, and nothing that is not a merge item is touched.
func TestDropNonAlertMergesKeepsOnlyHumanAlerts(t *testing.T) {
	items := []WaitItem{
		{Kind: WaitMerge, TaskID: 1, Alert: MergeAlertDefaultBranch},
		{Kind: WaitMerge, TaskID: 2, Alert: MergeAlertRule},
		{Kind: WaitMerge, TaskID: 3, Alert: MergeAlertUnknown},
		{Kind: WaitMerge, TaskID: 4, Alert: MergeAlertInternal},
		{Kind: WaitMerge, TaskID: 5, Alert: MergeAlertNone},
		{Kind: WaitMerge, TaskID: 6}, // no classification made: kept, as before
		{Kind: WaitDo, TaskID: 7},
		{Kind: WaitApprove, TaskID: 8},
	}
	var kept []int
	for _, it := range dropNonAlertMerges(items) {
		kept = append(kept, it.TaskID)
	}
	if want := []int{1, 2, 3, 6, 7, 8}; !slices.Equal(kept, want) {
		t.Fatalf("kept %v, want %v", kept, want)
	}
}
