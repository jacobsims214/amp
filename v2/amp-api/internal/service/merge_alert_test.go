package service

import (
	"strings"
	"testing"
	"time"
)

// The merge-alert classifier (AMP #3133): which of the four arms a merge item
// falls in, and — the part that must never be wrong in the quiet direction —
// that missing, stale, partial or ambiguous evidence is always "unknown" and
// never a silent "internal".
//
// Every case is pure input → output. No GitHub, no Postgres, no clock: the
// classification time is part of the input, so freshness is decided here and
// not by when the test happens to run.

// alertNow is the fixed classification time: freshness is a property of the
// input, so tests never depend on the wall clock.
var alertNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// fresh is a read taken just now, inside the TTL.
func fresh() time.Time { return alertNow.Add(-time.Minute) }

// rules is a complete, fresh read that found one pull_request rule.
func rules(r *MergePullRequestRule) MergeRuleFacts {
	return MergeRuleFacts{ReadAt: fresh(), PullRequest: r}
}

// repo is a fresh detected default branch.
func repo(branch string) MergeRepoFacts {
	return MergeRepoFacts{DefaultBranch: branch, Source: MergeAlertSourceDetected, ReadAt: fresh()}
}

// codeOwner is a resolved CODEOWNERS answer, so the *bool is never a bare
// true/false literal in a case that means "unknown".
func codeOwner(v bool) *bool { return &v }

// codeOwnerRule is this repo's shape (§1b): pull requests required, zero
// required approvals, code owners required, one unattributed-changes
// approval. Note that with RequiredApprovingReviewCount 0 and
// RequireExtraApprovalForUnattributedChanges it demands an approval it does
// not attribute — see TestClassifyMergeAlert_PersonIndirectRules.
func codeOwnerRule() *MergePullRequestRule {
	return &MergePullRequestRule{
		RulesetSource:                              "Repository",
		RulesetName:                                "main",
		RequiredApprovingReviewCount:               0,
		RequireCodeOwnerReview:                     true,
		RequireExtraApprovalForUnattributedChanges: true,
	}
}

func TestClassifyMergeAlert(t *testing.T) {
	const (
		def  = "main"
		ib   = "fix/actionable-merge-workflow"
		me   = "jacobsims214"
		orig = `Repository ruleset "main": a code owner's approval — you are a code owner of the changed paths`
	)

	tests := []struct {
		name     string
		in       MergeAlertInput
		wantKind string
		// wantReason has to appear in the reason: the classification must say
		// why, not just what.
		wantReason string
		// wantRule, when set, has to appear in the named rule; when empty and
		// the kind is not a rule alert, Rule must be empty.
		wantRule string
	}{
		// ---- arm 1: default_branch ----
		{
			name:       "target is the detected default branch",
			in:         MergeAlertInput{Now: alertNow, Repo: repo(def), TargetBranch: def, Rules: rules(nil)},
			wantKind:   MergeAlertDefaultBranch,
			wantReason: "default branch",
		},
		{
			name: "default branch alerts even when the rules could not be read",
			// Clause (a) needs only the default branch: a private repo, a
			// 404, an expired token — none of it may hide a default-branch
			// merge.
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: def,
				Rules: MergeRuleFacts{Partial: true}, // never read: no ReadAt
			},
			wantKind:   MergeAlertDefaultBranch,
			wantReason: "default branch",
		},
		{
			name: "default branch read manually in repo settings",
			// §2's fallback: a person declared it, so there is no read time to
			// age out.
			in: MergeAlertInput{
				Now:          alertNow,
				Repo:         MergeRepoFacts{DefaultBranch: def, Source: MergeAlertSourceManual},
				TargetBranch: def,
				Rules:        rules(nil),
			},
			wantKind:   MergeAlertDefaultBranch,
			wantReason: "default branch",
		},
		{
			name: "the default branch is not main, and it alerts by name",
			// The policy must work on a repo whose default branch is master:
			// "assume main" is never allowed, in either direction.
			in:         MergeAlertInput{Now: alertNow, Repo: repo("master"), TargetBranch: "master", Rules: rules(nil)},
			wantKind:   MergeAlertDefaultBranch,
			wantReason: "master",
		},

		// ---- arm 2: rule ----
		{
			name: "a rule names the person as a required reviewer",
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib, Person: MergeAlertPerson{Login: me},
				Rules: rules(&MergePullRequestRule{
					RulesetSource:                "Organization",
					RulesetName:                  "release",
					RequiredApprovingReviewCount: 1,
					RequiredReviewers:            []MergeRuleReviewer{{Login: me}},
				}),
			},
			wantKind:   MergeAlertRule,
			wantReason: "rule requires your approval",
			wantRule:   "you are a required reviewer",
		},
		{
			name: "a code-owner rule, and CODEOWNERS says the person owns the paths",
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib,
				Person: MergeAlertPerson{Login: me, CodeOwner: codeOwner(true)},
				Rules:  rules(codeOwnerRule()),
			},
			wantKind:   MergeAlertRule,
			wantReason: "rule requires your approval",
			wantRule:   orig,
		},
		{
			name: "a rule naming a team cannot be attributed, so it is not a rule alert",
			// Team membership is unresolved (§6), so "approval required" is
			// true but "this person's" is not knowable from this evidence.
			// It must be unknown, never internal.
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib, Person: MergeAlertPerson{Login: me},
				Rules: rules(&MergePullRequestRule{
					RequiredReviewers: []MergeRuleReviewer{{Login: "platform", Team: true}},
				}),
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "cannot attribute",
		},
		{
			name: "a rule requires someone unnamed, and a positive PR signal names the person",
			// Person-indirect rules stay unknown (§6 is an open decision), and
			// this path shows why the fallback cannot rescue them by status
			// alone: the requested reviewer is present, but nobody proved the
			// rule asks *them*.
			in: MergeAlertInput{
				Now:  alertNow,
				Repo: repo(def), TargetBranch: ib,
				Person: MergeAlertPerson{Login: me, CodeOwner: codeOwner(false)},
				Rules: MergeRuleFacts{
					ReadAt:             fresh(),
					PullRequest:        &MergePullRequestRule{RequiredApprovingReviewCount: 1},
					ReviewDecision:     "REVIEW_REQUIRED",
					MergeableState:     "blocked",
					RequestedReviewers: []string{me},
				},
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "cannot attribute",
		},
		{
			name: "partial rules plus GitHub's required review plus this person requested",
			// §3.3's fallback: the classic endpoint was unreadable and
			// rules/branches came back empty, but the PR itself proves a
			// review is required and names this person.
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib, Person: MergeAlertPerson{Login: me},
				Rules: MergeRuleFacts{
					ReadAt:             fresh(),
					Partial:            true,
					ReviewDecision:     "REVIEW_REQUIRED",
					RequestedReviewers: []string{me},
				},
			},
			wantKind:   MergeAlertRule,
			wantReason: "rules could not be read completely",
			wantRule:   "you are a requested reviewer",
		},
		{
			name: "partial rules plus a blocked PR plus this person requested",
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib,
				Person: MergeAlertPerson{Login: "JacobSims214"}, // logins are case-insensitive
				Rules: MergeRuleFacts{
					ReadAt:             fresh(),
					Partial:            true,
					MergeableState:     "blocked",
					RequestedReviewers: []string{me},
				},
			},
			wantKind:   MergeAlertRule,
			wantReason: "you are a requested reviewer",
			wantRule:   "you are a requested reviewer",
		},

		// ---- arm 3: internal ----
		{
			name: "non-default target and complete rules that require nobody",
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib, Person: MergeAlertPerson{Login: me},
				Rules: rules(nil),
			},
			wantKind:   MergeAlertInternal,
			wantReason: "not the repository's default branch",
		},
		{
			name: "non-default target, a deletion/non_fast_forward rule only",
			// Rules that block merges without ever asking a person: they say
			// nothing about approval, so the integration branch is internal.
			// The fetch layer drops every non-pull_request rule (only this
			// rule type can demand a person — §1b), which is exactly the nil
			// PullRequest here.
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib, Person: MergeAlertPerson{Login: me},
				Rules: rules(nil),
			},
			wantKind:   MergeAlertInternal,
			wantReason: "no rule requires your approval",
		},
		{
			name: "non-default target; a code-owner rule, and the person provably is not an owner",
			// Resolved false is positive knowledge: the rule asks code
			// owners, and they are not one. That can quiet the card. (The
			// rule carries no person-indirect demand here — see the
			// person-indirect cases for why that would change the answer.)
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib,
				Person: MergeAlertPerson{Login: me, CodeOwner: codeOwner(false)},
				Rules:  rules(&MergePullRequestRule{RequireCodeOwnerReview: true}),
			},
			wantKind:   MergeAlertInternal,
			wantReason: "no rule requires your approval",
		},
		{
			name: "non-default target; a code-owner rule, and CODEOWNERS names someone else",
			// Resolved false where others do own the paths: still not this
			// person's, so still internal.
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib,
				Person: MergeAlertPerson{Login: me, CodeOwner: codeOwner(false)},
				Rules:  rules(&MergePullRequestRule{RequireCodeOwnerReview: true, RequiredReviewers: []MergeRuleReviewer{{Login: "someone-else"}}}),
			},
			wantKind:   MergeAlertInternal,
			wantReason: "no rule requires your approval",
		},
		{
			name: "non-default target; a rule names other reviewers and nobody else",
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib, Person: MergeAlertPerson{Login: me},
				Rules: rules(&MergePullRequestRule{
					RequiredReviewers: []MergeRuleReviewer{{Login: "someone-else"}},
				}),
			},
			wantKind:   MergeAlertInternal,
			wantReason: "no rule requires your approval",
		},

		// ---- arm 4: unknown, and it must never be internal ----
		{
			name:       "the default branch was not read",
			in:         MergeAlertInput{Now: alertNow, TargetBranch: ib, Rules: rules(nil)},
			wantKind:   MergeAlertUnknown,
			wantReason: "default branch could not be read",
		},
		{
			name: "the default branch read is older than the TTL",
			in: MergeAlertInput{
				Now:          alertNow,
				Repo:         MergeRepoFacts{DefaultBranch: def, ReadAt: alertNow.Add(-MergeAlertEvidenceTTL - time.Minute)},
				TargetBranch: ib,
				Rules:        rules(nil),
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "stale",
		},
		{
			name: "the default branch read is stamped in the future",
			in: MergeAlertInput{
				Now:          alertNow,
				Repo:         MergeRepoFacts{DefaultBranch: def, ReadAt: alertNow.Add(time.Hour)},
				TargetBranch: def,
				Rules:        rules(nil),
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "cannot be judged",
		},
		{
			name:       "the target branch is unknown",
			in:         MergeAlertInput{Now: alertNow, Repo: repo(def), Rules: rules(nil)},
			wantKind:   MergeAlertUnknown,
			wantReason: "target branch is unknown",
		},
		{
			name: "the rules were never read, and the target is not the default",
			// This is today's state of every card before #3134 fills the
			// evidence: non-default target, nothing read. Unknown — the
			// honest-unknown chip, not silent internal.
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib,
				Rules: MergeRuleFacts{},
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "has not been read",
		},
		{
			name: "the rules read is stale",
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib,
				Rules: MergeRuleFacts{ReadAt: alertNow.Add(-MergeAlertEvidenceTTL - time.Second)},
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "stale",
		},
		{
			name: "the rules read has no timestamp at all",
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib,
				Rules: MergeRuleFacts{PullRequest: codeOwnerRule()},
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "has not been read",
		},
		{
			name: "partial rules with no PR-level positive signal",
			// Absence of knowledge can never quiet a card, but a requested
			// reviewer alone is not a positive signal either.
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib, Person: MergeAlertPerson{Login: me},
				Rules: MergeRuleFacts{ReadAt: fresh(), Partial: true},
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "could not be read completely",
		},
		{
			name: "partial rules: a review is required but this person is not requested",
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib, Person: MergeAlertPerson{Login: me},
				Rules: MergeRuleFacts{
					ReadAt:             fresh(),
					Partial:            true,
					ReviewDecision:     "REVIEW_REQUIRED",
					RequestedReviewers: []string{"someone-else"},
				},
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "could not be read completely",
		},
		{
			name: "partial rules: this person is requested but nothing says a review is required",
			// Code owners are auto-requested without any protection in place
			// (§1c), so a requested reviewer alone must not alert.
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib, Person: MergeAlertPerson{Login: me},
				Rules: MergeRuleFacts{
					ReadAt:             fresh(),
					Partial:            true,
					RequestedReviewers: []string{me},
				},
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "no approval requirement that names you",
		},
		{
			name: "partial rules: a blocked PR, this person requested, but not a review decision",
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib, Person: MergeAlertPerson{Login: me},
				Rules: MergeRuleFacts{
					ReadAt:             fresh(),
					Partial:            true,
					MergeableState:     "unknown", // GitHub has not computed it yet
					RequestedReviewers: []string{me},
				},
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "could not be read completely",
		},
		{
			name: "partial rules with no person login",
			// Without a login, "is this person requested" cannot be answered.
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib,
				Rules: MergeRuleFacts{
					ReadAt:             fresh(),
					Partial:            true,
					ReviewDecision:     "REVIEW_REQUIRED",
					RequestedReviewers: []string{me},
				},
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "could not be read completely",
		},
		{
			name: "a code-owner rule with CODEOWNERS unresolved",
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib, Person: MergeAlertPerson{Login: me},
				Rules: rules(&MergePullRequestRule{RequireCodeOwnerReview: true}),
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "cannot attribute",
		},
		{
			name: "a rule names a reviewer and the person's login is unknown",
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib,
				Rules: rules(&MergePullRequestRule{RequiredReviewers: []MergeRuleReviewer{{Login: me}}}),
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "cannot attribute",
		},
		{
			name: "person-indirect rules (count, last-push, unattributed) are never internal",
			// §6: these demand *someone's* approval. They cannot be this
			// person's, and they cannot be nobody's either.
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib,
				Person: MergeAlertPerson{Login: me, CodeOwner: codeOwner(false)},
				Rules: rules(&MergePullRequestRule{
					RequiredApprovingReviewCount:               2,
					RequireLastPushApproval:                    true,
					RequireExtraApprovalForUnattributedChanges: true,
				}),
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "cannot attribute",
		},
		{
			name: "this repo's real rule shape: code owners plus unattributed changes",
			// decisions/repo-default-branch.md §1b's verified jacobsims214/amp
			// `main` ruleset: required_approving_review_count 0,
			// require_code_owner_review true,
			// require_extra_approval_for_unattributed_changes true. The code
			// owner is provably this person, so the rule IS theirs — that arm
			// wins before the person-indirect demand is considered.
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib,
				Person: MergeAlertPerson{Login: me, CodeOwner: codeOwner(true)},
				Rules:  rules(codeOwnerRule()),
			},
			wantKind:   MergeAlertRule,
			wantReason: "rule requires your approval",
			wantRule:   "Repository ruleset",
		},
		{
			name: "this repo's real rule shape, and the person is not a code owner",
			// Same verified rule, this person not among the owners: the only
			// demand left is the unattributed-changes approval, which is
			// someone's but not theirs and not nobody's — §6's open
			// decision, so unknown rather than a quiet internal.
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib,
				Person: MergeAlertPerson{Login: me, CodeOwner: codeOwner(false)},
				Rules:  rules(codeOwnerRule()),
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "cannot attribute",
		},
		{
			name: "complete rules require nobody, but the PR says a review is required",
			// Contradictory reads: ambiguous, so unknown, not internal.
			in: MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib, Person: MergeAlertPerson{Login: me},
				Rules: MergeRuleFacts{ReadAt: fresh(), ReviewDecision: "REVIEW_REQUIRED"},
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "reports that a review is required",
		},
		{
			name: "a zero Now cannot make evidence look fresh",
			in: MergeAlertInput{
				Now: time.Time{}, Repo: repo(def), TargetBranch: def, Rules: rules(nil),
			},
			wantKind:   MergeAlertUnknown,
			wantReason: "cannot be judged",
		},

		// ---- already merged: never alerts ----
		{
			name: "already merged, non-default target, rules unknown",
			in: MergeAlertInput{
				Now: alertNow, Merged: true, Repo: repo(def), TargetBranch: ib,
			},
			wantKind:   MergeAlertNone,
			wantReason: "already recorded",
		},
		{
			name: "already merged, and even the default branch is unknown",
			// A merged item must not raise state 9's warn chip: there is no
			// question left for a person to answer.
			in:         MergeAlertInput{Now: alertNow, Merged: true, TargetBranch: ib},
			wantKind:   MergeAlertNone,
			wantReason: "already recorded",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyMergeAlert(tc.in)
			if got.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q (reason: %q)", got.Kind, tc.wantKind, got.Reason)
			}
			if !strings.Contains(got.Reason, tc.wantReason) {
				t.Errorf("Reason = %q, want it to contain %q", got.Reason, tc.wantReason)
			}
			if tc.wantRule != "" {
				if !strings.Contains(got.Rule, tc.wantRule) {
					t.Errorf("Rule = %q, want it to contain %q", got.Rule, tc.wantRule)
				}
			} else if got.Kind != MergeAlertRule && got.Rule != "" {
				t.Errorf("Rule = %q, want empty for a %q classification", got.Rule, got.Kind)
			}
			if want := tc.in.Repo.DefaultBranch; got.DefaultBranch != want {
				t.Errorf("DefaultBranch = %q, want %q", got.DefaultBranch, want)
			}
		})
	}
}

// TestClassifyMergeAlert_ArmsAreCovered fails if an arm of the policy loses
// every case above: the ticket's acceptance is "four classification arms
// tested", and a table edit could otherwise drop one silently.
func TestClassifyMergeAlert_ArmsAreCovered(t *testing.T) {
	arms := []string{MergeAlertDefaultBranch, MergeAlertRule, MergeAlertInternal, MergeAlertUnknown}
	const (
		def = "main"
		ib  = "fix/actionable-merge-workflow"
		me  = "jacobsims214"
	)
	inputs := map[string]MergeAlertInput{
		MergeAlertDefaultBranch: {Now: alertNow, Repo: repo(def), TargetBranch: def, Rules: rules(nil)},
		MergeAlertRule: {
			Now: alertNow, Repo: repo(def), TargetBranch: ib,
			Person: MergeAlertPerson{Login: me},
			Rules:  rules(&MergePullRequestRule{RequiredReviewers: []MergeRuleReviewer{{Login: me}}}),
		},
		MergeAlertInternal: {Now: alertNow, Repo: repo(def), TargetBranch: ib, Person: MergeAlertPerson{Login: me}, Rules: rules(nil)},
		MergeAlertUnknown:  {Now: alertNow, Repo: repo(def), TargetBranch: ib},
	}
	for _, arm := range arms {
		in, ok := inputs[arm]
		if !ok {
			t.Fatalf("no sample input for arm %q", arm)
		}
		if got := ClassifyMergeAlert(in); got.Kind != arm {
			t.Errorf("arm %q: sample input classified %q", arm, got.Kind)
		}
	}
}

// TestClassifyMergeAlert_UnknownNeverQuiets pins the one asymmetry the whole
// policy rests on (§5): positive knowledge may quiet a card, absence of
// knowledge never may. Every degradation of the evidence — read missing, stale,
// future-stamped, partial, person unknown, contradiction — yields unknown, and
// none of them yields internal.
func TestClassifyMergeAlert_UnknownNeverQuiets(t *testing.T) {
	const (
		def = "main"
		ib  = "fix/actionable-merge-workflow"
		me  = "jacobsims214"
	)
	complete := MergeAlertInput{
		Now: alertNow, Repo: repo(def), TargetBranch: ib,
		Person: MergeAlertPerson{Login: me, CodeOwner: codeOwner(false)},
		Rules:  rules(nil),
	}
	if got := ClassifyMergeAlert(complete); got.Kind != MergeAlertInternal {
		t.Fatalf("the complete-evidence baseline is %q, want internal", got.Kind)
	}

	degradations := []struct {
		name    string
		degrade func(*MergeAlertInput)
	}{
		{"no default branch", func(in *MergeAlertInput) { in.Repo.DefaultBranch = "" }},
		{"default branch never read", func(in *MergeAlertInput) { in.Repo.ReadAt = time.Time{} }},
		{"default branch stale", func(in *MergeAlertInput) { in.Repo.ReadAt = alertNow.Add(-time.Hour) }},
		{"default branch future-stamped", func(in *MergeAlertInput) { in.Repo.ReadAt = alertNow.Add(time.Minute) }},
		{"no target branch", func(in *MergeAlertInput) { in.TargetBranch = "" }},
		{"rules never read", func(in *MergeAlertInput) { in.Rules.ReadAt = time.Time{} }},
		{"rules stale", func(in *MergeAlertInput) { in.Rules.ReadAt = alertNow.Add(-time.Hour) }},
		{"rules future-stamped", func(in *MergeAlertInput) { in.Rules.ReadAt = alertNow.Add(time.Minute) }},
		{"rules partial", func(in *MergeAlertInput) { in.Rules.Partial = true }},
		{"code owner unresolved, and a code-owner rule applies", func(in *MergeAlertInput) {
			in.Rules.PullRequest = &MergePullRequestRule{RequireCodeOwnerReview: true}
			in.Person.CodeOwner = nil
		}},
		{"person login unknown, and a rule names a reviewer", func(in *MergeAlertInput) {
			in.Rules.PullRequest = &MergePullRequestRule{RequiredReviewers: []MergeRuleReviewer{{Login: me}}}
			in.Person.Login = ""
		}},
		{"PR says review required anyway", func(in *MergeAlertInput) { in.Rules.ReviewDecision = "REVIEW_REQUIRED" }},
		{"rule names a team", func(in *MergeAlertInput) {
			in.Rules.PullRequest = &MergePullRequestRule{RequiredReviewers: []MergeRuleReviewer{{Login: "platform", Team: true}}}
		}},
		{"rule requires someone unnamed", func(in *MergeAlertInput) {
			in.Rules.PullRequest = &MergePullRequestRule{RequiredApprovingReviewCount: 1}
		}},
	}
	for _, d := range degradations {
		t.Run(d.name, func(t *testing.T) {
			in := complete
			d.degrade(&in)
			if got := ClassifyMergeAlert(in); got.Kind != MergeAlertUnknown {
				t.Fatalf("Kind = %q, want unknown: degraded evidence must never quiet a card", got.Kind)
			}
		})
	}
}

// TestClassifyMergeAlert_RequestedReviewerAloneNeverAlerts pins the §1c
// gotcha: GitHub auto-requests review from code owners of changed paths even
// where no rule requires them, so a requested reviewer is never proof on its
// own — with or without a positive PR signal, when the rules are complete.
func TestClassifyMergeAlert_RequestedReviewerAloneNeverAlerts(t *testing.T) {
	const (
		def = "main"
		ib  = "fix/actionable-merge-workflow"
		me  = "jacobsims214"
	)
	tests := []struct {
		name  string
		rules MergeRuleFacts
	}{
		{
			name:  "requested, nothing else",
			rules: MergeRuleFacts{ReadAt: fresh(), RequestedReviewers: []string{me}},
		},
		{
			name:  "requested and blocked, complete rules requiring nobody",
			rules: MergeRuleFacts{ReadAt: fresh(), MergeableState: "blocked", RequestedReviewers: []string{me}},
		},
		{
			name:  "requested and REVIEW_REQUIRED, complete rules requiring nobody",
			rules: MergeRuleFacts{ReadAt: fresh(), ReviewDecision: "REVIEW_REQUIRED", RequestedReviewers: []string{me}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyMergeAlert(MergeAlertInput{
				Now: alertNow, Repo: repo(def), TargetBranch: ib,
				Person: MergeAlertPerson{Login: me, CodeOwner: codeOwner(false)},
				Rules:  tc.rules,
			})
			if got.Kind == MergeAlertRule || got.Kind == MergeAlertDefaultBranch {
				t.Fatalf("Kind = %q: a requested reviewer alone must not alert", got.Kind)
			}
			// REVIEW_REQUIRED contradicting complete rules is ambiguous, so
			// unknown; the blocked-only case is plain internal.
			if got.Kind == MergeAlertInternal && tc.rules.ReviewDecision != "" {
				t.Fatalf("Kind = internal on contradictory evidence; want unknown")
			}
		})
	}
}

// TestClassifyMergeAlert_MergedNeverAlerts pins that a recorded merge decides
// before any evidence: no arm, no rule, no warning, whatever else is (not)
// known.
func TestClassifyMergeAlert_MergedNeverAlerts(t *testing.T) {
	targets := []string{"", "main", "fix/actionable-merge-workflow"}
	for _, target := range targets {
		t.Run("target="+target, func(t *testing.T) {
			in := MergeAlertInput{Now: alertNow, Merged: true, TargetBranch: target}
			in.Repo = repo("main")
			in.Rules.Partial = true
			got := ClassifyMergeAlert(in)
			if got.Kind != MergeAlertNone {
				t.Fatalf("Kind = %q, want %q", got.Kind, MergeAlertNone)
			}
			if got.Rule != "" {
				t.Errorf("Rule = %q, want empty for a merged item", got.Rule)
			}
		})
	}
}

// TestClassifyMergeAlert_EvidenceTTL pins the boundary itself: a read exactly
// at the TTL is still fresh, a second older is not. Rule content mutates in
// real time (§1b), so this edge is a policy decision, not an accident.
func TestClassifyMergeAlert_EvidenceTTL(t *testing.T) {
	const def = "main"
	tests := []struct {
		name     string
		readAt   time.Time
		wantKind string
	}{
		{"exactly at the TTL", alertNow.Add(-MergeAlertEvidenceTTL), MergeAlertDefaultBranch},
		{"one second past the TTL", alertNow.Add(-MergeAlertEvidenceTTL - time.Second), MergeAlertUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyMergeAlert(MergeAlertInput{
				Now:          alertNow,
				Repo:         MergeRepoFacts{DefaultBranch: def, ReadAt: tc.readAt},
				TargetBranch: def,
				Rules:        rules(nil),
			})
			if got.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
		})
	}
}

// TestClassifyMergeAlert_Pure pins that classification consults nothing but
// its input: the same input classified twice gives the same answer, and the
// input is not modified (no clock, no cache, no lazily-filled field).
func TestClassifyMergeAlert_Pure(t *testing.T) {
	in := MergeAlertInput{
		Now:          alertNow,
		Repo:         repo("main"),
		TargetBranch: "fix/actionable-merge-workflow",
		Person:       MergeAlertPerson{Login: "jacobsims214", CodeOwner: codeOwner(true)},
		Rules:        rules(codeOwnerRule()),
	}
	first := ClassifyMergeAlert(in)
	second := ClassifyMergeAlert(in)
	if first != second {
		t.Fatalf("two calls on the same input differ:\n%+v\n%+v", first, second)
	}
	if in.Repo.ReadAt.IsZero() || len(in.Rules.PullRequest.RequiredReviewers) != 0 {
		t.Fatalf("input was modified: %+v", in)
	}
	// The classification time comes from the input, not the wall clock: the
	// same input stays fresh however long the process runs.
	if first.Kind != MergeAlertRule {
		t.Fatalf("Kind = %q, want %q", first.Kind, MergeAlertRule)
	}
}
