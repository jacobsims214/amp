package service

// Wiring the merge-alert classifier into the waiting projection (AMP #3158,
// epic 183, story 453).
//
// merge_alert.go is the policy (pure, #3133); github_facts.go is where the
// cached repository facts come from (#3156). This file is the join between
// them and the REST/MCP projections: it turns one cached GitHubFactsView into
// the classifier's input, and reports which arms a person is actually asked
// about.
//
// Two rules shape everything here.
//
//  1. THE READ PATH MAKES NO HTTP CALL. The facts come from
//     GitHubFactsCoordinator.Lookup, which reads the cache and may *start* a
//     bounded background refresh — it never waits for one. A My work read is
//     therefore as cheap as its cache, and a slow or broken GitHub can never
//     make the page slower. A miss is not an error: it is the classifier's
//     unknown arm, which is the honest floor (#3156's own contract).
//
//  2. THE PROJECTION NEVER INVENTS EVIDENCE. Facts are copied into the
//     classifier's input only when the coordinator says they are Trusted, and
//     the target is copied only when GitHub's own pull read established it
//     (SourcePull) — a run's reported base_branch is a hint, and a hint may
//     never decide that a merge is internal agent organization
//     (decisions/repo-default-branch.md §5). Whenever something is missing the
//     input stays zero and the classifier answers unknown, by construction.
//
// And one rule about what the projection must NOT do: it never touches the
// merge gate. Suppressing a human alert is a statement about who is asked; it
// is not a claim that the work merged, and it releases no dependent
// (domain.MergeGates and Service.mergeGateFor are the only things that decide
// that — user direction 2026-11-02, "separate dependency readiness from human
// alerting").

import (
	"context"
	"encoding/json"
	"time"

	"github.com/simstech/amp-api/internal/githubfacts"
	"github.com/simstech/amp-api/internal/repository"
)

// MergeAlertPersonSource answers who a person is to the merge-alert policy:
// their GitHub login, and whether CODEOWNERS makes them an owner of the PR's
// changed paths.
//
// It is nil in production, and that is not a placeholder for a forgotten wire:
// AMP stores no GitHub login for a user today and resolves no CODEOWNERS
// ownership, so "does this rule require this person" cannot be answered for a
// rule that names someone (decisions/repo-default-branch.md §6 lists both as
// open decisions). A nil source leaves MergeAlertPerson zero, which the
// classifier reads as unresolved — a rule that names a reviewer, a code-owner
// rule, or an approval that must come from *someone* becomes the honest
// unknown arm instead of an alert addressed to the wrong person, and never
// becomes "internal" either (§5: absence of knowledge never quiets an item).
//
// The seam exists so the projection does not change when the mapping lands:
// whatever supplies it (project metadata, a users column, a GitHub App) is set
// here and every surface picks it up, because REST and MCP both read
// Service.WaitingOn.
type MergeAlertPersonSource func(ctx context.Context, userID int) MergeAlertPerson

// mergeAlertClassification is one merge item's answer from the policy, plus
// the target GitHub's own pull read established ("" when it established none).
type mergeAlertClassification struct {
	Alert  MergeAlert
	Target string
}

// classifyMerge runs the merge-alert policy for one merge item, from cached
// facts only.
//
// Merged starts false: a Merge item exists only while the merge is NOT
// recorded (domain.MergeGates decides that, and this function does not touch
// it). GitHub's own answer for a PR it already sees merged arrives through the
// pull read and sets it — which is the case that matters, AMP being behind the
// repository — and the policy then answers MergeAlertNone, asking nobody
// anything.
func (s *Service) classifyMerge(ctx context.Context, projectID int, ref repository.MergeRef, owner *int) mergeAlertClassification {
	in := MergeAlertInput{Now: time.Now().UTC()}
	if s.Facts == nil || ref.PRURL == "" {
		// No cache wired, or no PR to name a repository and a number: nothing
		// is known, so the policy answers unknown. Never a guess from a branch
		// name.
		return mergeAlertClassification{Alert: ClassifyMergeAlert(in)}
	}
	slug, pr, perr := githubfacts.ParsePullURL(ref.PRURL)
	if perr != nil {
		// A branch-only run, a non-github.com host, or a malformed URL: the
		// repository identity the policy needs is not there. Unknown.
		return mergeAlertClassification{Alert: ClassifyMergeAlert(in)}
	}
	view, err := s.Facts.Lookup(ctx, GitHubFactsRequest{ProjectID: projectID, Slug: slug, PR: pr, Target: ref.BaseBranch})
	if err != nil {
		// The cache itself could not be read. That is not a fact about the
		// repository, so it is not "no rule applies" either: unknown, and the
		// item keeps asking the person, which is the safe direction.
		return mergeAlertClassification{Alert: ClassifyMergeAlert(in)}
	}
	in = mergeAlertInputFromView(view, in)
	if owner != nil && s.MergeAlertPerson != nil {
		in.Person = s.MergeAlertPerson(ctx, *owner)
	}
	return mergeAlertClassification{Alert: ClassifyMergeAlert(in), Target: mergeAlertVerifiedTarget(view)}
}

// mergeAlertInputFromView copies the cached facts the coordinator trusts into
// the classifier's input. It is pure, so every arm and every degradation is
// testable without Postgres, GitHub or a clock.
//
// The asymmetry is deliberate and mirrors the policy's: a fact is copied only
// when it is Trusted, and Trusted is the coordinator's own affirmative
// statement (verified, fresh, and — for rules — complete or partial). Nothing
// is inferred from a fact being *absent*.
func mergeAlertInputFromView(v GitHubFactsView, in MergeAlertInput) MergeAlertInput {
	if v.Repo.Trusted && v.Repo.DefaultBranch != "" {
		source := MergeAlertSourceDetected
		if v.Repo.Source == repository.GitHubBranchSourceManual {
			// A person's declaration is not a read and does not age out on the
			// evidence TTL; it is replaced or cleared by another declaration
			// (§2). Nothing writes this source yet — the settings surface is a
			// separate ticket — but the mapping is here so it cannot be
			// forgotten when it does.
			source = MergeAlertSourceManual
		}
		in.Repo = MergeRepoFacts{DefaultBranch: v.Repo.DefaultBranch, Source: source, ReadAt: v.Repo.ReadAt}
	}

	// Clause (a) may only compare the PR's own base. A hint (the run's
	// base_branch) is not evidence of where the PR targets — it is what
	// somebody typed or the harness guessed — and comparing it to the default
	// branch is how a real merge silently stops being asked about a person
	// (GitHubFactsView.TargetIsDefault makes the same refusal).
	if v.TargetSource == githubfacts.SourcePull {
		in.TargetBranch = v.Target
	}

	if v.Branch.Trusted {
		in.Rules.ReadAt = v.Branch.ReadAt
		// Partial is carried through unchanged: it is a positive signal for a
		// rule, never proof that none applies.
		in.Rules.Partial = v.Branch.Partial
		rule, ok, unreadable := mergeAlertPullRequestRule(v.Branch.Rules)
		if ok {
			in.Rules.PullRequest = rule
		}
		if unreadable {
			// The rules were read, but this server cannot read what they say
			// (a writer bug or a hand-edited row — the column is CHECKed as an
			// array of rules). Unreadable content cannot prove the ABSENCE of a
			// rule, so it degrades exactly like an incomplete read: the
			// classifier may alert or stay unknown, and may never quiet the item.
			in.Rules.Partial = true
		}
	}

	// The PR-level signals stand in for a rules read that could not be
	// complete, so they are only evidence when the pull read itself was
	// verified and fresh. They come from two reads of their own (mergeable
	// state from REST `pulls/{n}`, the review decision from GraphQL), so each
	// can independently be 044's "unknown" while the other is a real answer —
	// which the classifier handles: unknown is not a positive signal.
	if v.Pull.Trusted && v.Pull.Facts != nil {
		in.Rules.ReviewDecision = v.Pull.ReviewDecision
		in.Rules.MergeableState = v.Pull.MergeableState
		in.Rules.RequestedReviewers = v.Pull.RequestedReviewers
		if v.Pull.Facts.State == "merged" {
			in.Merged = true
		}
	}
	return in
}

// mergeAlertVerifiedTarget is the branch GitHub's own pull read says the PR
// targets, or "" — a hint is never returned, because it is not a verification
// and a card that presents one as the target is the overstatement the whole
// policy exists to remove.
func mergeAlertVerifiedTarget(v GitHubFactsView) string {
	if v.TargetSource != githubfacts.SourcePull {
		return ""
	}
	return v.Target
}

// The stored-rule shape 044 keeps (internal/service/github_facts.go's
// githubFactRule, written by githubFactRulesJSON). It is read here rather than
// reused so the storage shape and the classifier's input stay independently
// named: renaming a key in the writer must break a test, not silently change
// the policy. repository-facts-refresh.md's gotcha says exactly this.
type mergeAlertStoredRule struct {
	Type              string                          `json:"type"`
	RulesetSource     string                          `json:"ruleset_source"`
	RulesetSourceType string                          `json:"ruleset_source_type"`
	Parameters        *mergeAlertStoredRuleParameters `json:"parameters"`
}

type mergeAlertStoredRuleParameters struct {
	RequiredApprovingReviewCount               int                        `json:"required_approving_review_count"`
	RequireCodeOwnerReview                     bool                       `json:"require_code_owner_review"`
	RequiredReviewers                          []mergeAlertStoredReviewer `json:"required_reviewers"`
	RequireLastPushApproval                    bool                       `json:"require_last_push_approval"`
	RequireExtraApprovalForUnattributedChanges bool                       `json:"require_extra_approval_for_unattributed_changes"`
	RequireReviewThreadResolution              bool                       `json:"required_review_thread_resolution"`
}

type mergeAlertStoredReviewer struct {
	Type  string `json:"type"`
	Login string `json:"login"`
	Slug  string `json:"slug"`
}

// mergeAlertPullRequestRule reduces the cached rules array to the one
// pull_request rule the classifier judges, and reports whether there was one.
//
// Only `pull_request` can demand a person: the other rule types (deletion,
// non_fast_forward, required_status_checks, commit_message_pattern, …) block
// merges without asking anyone (§1b). An empty array is a real answer — the
// read happened and nothing applies — and it reduces to nil with false, which
// the classifier reads as "no rule requires you" *given complete evidence*.
//
// SEVERAL pull_request RULES ARE MERGED, NEVER NARROWED. A branch can be
// covered by more than one ruleset (an organization's and the repository's),
// and the classifier judges one rule. Taking the first would under-ask — it
// would drop a reviewer another rule requires, or a code-owner demand — so the
// demands are unioned (flags ORed, the largest approval count, every named
// reviewer) and the rule-box names the first rule that demands anything. Too
// much alerting is recoverable (a person sees one card they could ignore);
// too little silently drops a real merge, which is the failure this policy
// exists to make impossible.
// unreadable is true when the array could not be parsed: the caller must treat
// that as partial evidence rather than as "no rule", because content this
// server cannot read proves nothing about the absence of a rule.
func mergeAlertPullRequestRule(rules []byte) (rule *MergePullRequestRule, found, unreadable bool) {
	if len(rules) == 0 {
		return nil, false, false
	}
	var stored []mergeAlertStoredRule
	if err := json.Unmarshal(rules, &stored); err != nil {
		return nil, false, true
	}
	var out *MergePullRequestRule
	var origin *MergePullRequestRule
	for _, r := range stored {
		if r.Type != "pull_request" {
			continue
		}
		pr := MergePullRequestRule{RulesetSource: r.RulesetSourceType, RulesetName: r.RulesetSource}
		demands := false
		if p := r.Parameters; p != nil {
			pr.RequiredApprovingReviewCount = p.RequiredApprovingReviewCount
			pr.RequireCodeOwnerReview = p.RequireCodeOwnerReview
			pr.RequireLastPushApproval = p.RequireLastPushApproval
			pr.RequireExtraApprovalForUnattributedChanges = p.RequireExtraApprovalForUnattributedChanges
			for _, rv := range p.RequiredReviewers {
				pr.RequiredReviewers = append(pr.RequiredReviewers, MergeRuleReviewer{
					Login: mergeAlertReviewerLogin(rv),
					Team:  rv.Team(),
				})
			}
			demands = p.RequiredApprovingReviewCount > 0 || p.RequireCodeOwnerReview || p.RequireLastPushApproval ||
				p.RequireExtraApprovalForUnattributedChanges || len(p.RequiredReviewers) > 0
		}
		if demands && origin == nil {
			// The first rule that demands something names the rule-box: it is
			// the rule a person is most likely being asked by, and every demand
			// of every rule is still carried in the union below.
			named := pr
			origin = &named
		}
		if out == nil {
			first := pr
			out = &first
			continue
		}
		out.RequiredApprovingReviewCount = max(out.RequiredApprovingReviewCount, pr.RequiredApprovingReviewCount)
		out.RequireCodeOwnerReview = out.RequireCodeOwnerReview || pr.RequireCodeOwnerReview
		out.RequireLastPushApproval = out.RequireLastPushApproval || pr.RequireLastPushApproval
		out.RequireExtraApprovalForUnattributedChanges = out.RequireExtraApprovalForUnattributedChanges || pr.RequireExtraApprovalForUnattributedChanges
		out.RequiredReviewers = append(out.RequiredReviewers, pr.RequiredReviewers...)
	}
	if out == nil {
		// No pull_request rule applies to this branch: a real answer, and the
		// classifier's "no rule requires your approval" given complete
		// evidence.
		return nil, false, false
	}
	if origin != nil {
		out.RulesetSource, out.RulesetName = origin.RulesetSource, origin.RulesetName
	}
	return out, true, false
}

// mergeAlertReviewerLogin is the reviewer's name in the policy's terms. GitHub
// returns users and teams in one array distinguished by Type: a user has a
// login, a team a slug. A team entry keeps its slug as the Login so the
// rule-box can name it, and is marked Team so the classifier refuses to guess
// its membership.
func mergeAlertReviewerLogin(r mergeAlertStoredReviewer) string {
	if r.Team() {
		return r.Slug
	}
	return r.Login
}

// Team reports whether this entry names a team rather than a user.
func (r mergeAlertStoredReviewer) Team() bool {
	return r.Type == "Team" || (r.Type == "" && r.Login == "" && r.Slug != "")
}

// mergeAlertAsksPerson reports whether an arm is a human alert. The four
// policy arms minus the two that ask nobody: internal is agent organization
// (artifact #27 state 6 — at most an informational row, never a card or a
// question), and none is work that is already merged.
func mergeAlertAsksPerson(kind string) bool {
	switch kind {
	case MergeAlertDefaultBranch, MergeAlertRule, MergeAlertUnknown:
		return true
	default:
		return false
	}
}

// dropNonAlertMerges removes the Merge items that are not human alerts,
// leaving every other kind and every other ticket's item alone.
//
// This is where "only actionable merge requests reach people" becomes true for
// REST, MCP and the badge counts at once, because every one of them reads
// Service.WaitingOn. It runs BEFORE dedupFoldedSteps on purpose: a plan's human
// merge step (a person's own ticket, folded onto the card it tracks) must not
// disappear with the card. Once the card is gone the step is an ordinary Do
// item again, which is the truthful outcome — the merge is still somebody's
// ticket, it just is not a *merge alert*.
//
// Nothing about the gate changes here. The ticket keeps merge_state needed and
// its dependents keep waiting; suppressing the alert says "nobody needs to be
// told about this merge", not "this merged" (see the file comment).
func dropNonAlertMerges(items []WaitItem) []WaitItem {
	out := make([]WaitItem, 0, len(items))
	for _, it := range items {
		if it.Kind == WaitMerge && it.Alert != "" && !mergeAlertAsksPerson(it.Alert) {
			continue
		}
		out = append(out, it)
	}
	return out
}
