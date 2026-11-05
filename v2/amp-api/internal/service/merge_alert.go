package service

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The merge-alert classifier (AMP #3133, decisions/repo-default-branch.md §3,
// approved artifact #27 v2).
//
// A Merge item reaches a person only if (a) its PR targets the repository's
// DETECTED default branch, or (b) a repository rule actually requires that
// person's approval. Everything else is agent organization — a worktree or an
// integration branch — and must not become a card.
//
// This file is the policy and nothing else. ClassifyMergeAlert is pure: no
// host calls, no database, no UI, and no clock of its own (Now arrives in the
// input). That is what lets all four arms be tested without GitHub, Postgres
// or a browser. What it deliberately does NOT do matters as much:
//
//   - It never touches dependency gating. mergeGateFor (merge.go) alone
//     decides merge_state and which tickets wait for a merge; this classifier
//     only decides whether a human is told about a merge that is already gated
//     (user direction 2026-11-02: "Separate dependency readiness from human
//     alerting").
//   - It never assumes "main". A default branch that could not be read is
//     unknown, and so is a target that could not be verified.
//   - It never infers a required approval from a requested reviewer alone:
//     GitHub auto-requests review from code owners of changed paths even where
//     no rule requires them (§1c and the gotchas).
//
// Degradation is asymmetric on purpose (§5). Positive knowledge can quiet a
// card; absence of knowledge never can. Unknown, stale, partial or ambiguous
// evidence is always MergeAlertUnknown — the honest-unknown state 9 — and
// never MergeAlertInternal.

// The classification of one merge item: whether a person is asked about it,
// and why.
const (
	// MergeAlertDefaultBranch: the PR targets the repository's detected
	// default branch. States 1/2 — the merge card as built. Safe even when
	// the rules could not be read: clause (a) needs only the default branch.
	MergeAlertDefaultBranch = "default_branch"
	// MergeAlertRule: the PR targets another branch, and a repository rule
	// requires this person's approval. State 3.
	MergeAlertRule = "rule"
	// MergeAlertInternal: the PR targets another branch, and complete, fresh
	// evidence shows no rule requires this person. State 6 — at most an
	// informational row that clears when the integration PR's merge is
	// recorded, never a card.
	MergeAlertInternal = "internal"
	// MergeAlertUnknown: the evidence is missing, stale, partial or
	// ambiguous. State 9 — the warn chip with its resolution path, never
	// silence.
	MergeAlertUnknown = "unknown"
	// MergeAlertNone: there is nothing to classify, because the merge is
	// already recorded. Already-merged work never asks a person anything, so
	// this is not one of the four policy arms: reusing one of them would
	// misdescribe the branch, and "unknown" in particular would raise the
	// state-9 warn chip for work that is finished.
	MergeAlertNone = "none"
)

// MergeAlertEvidenceTTL is how long a repository fact or a rules read counts
// as fresh (decisions/repo-default-branch.md §2). Rule content mutates in real
// time — this repo's default-branch ruleset was created and edited within
// hours of the research — so an older read is not evidence of anything.
const MergeAlertEvidenceTTL = 15 * time.Minute

// MergeAlertInput is every fact the classifier may use. The fetch layer fills
// it from repo_facts, branch_rules and the PR's own state; the classifier
// fetches nothing, so every arm is tested against the policy alone.
type MergeAlertInput struct {
	// Now is when the classification is made. Freshness is therefore a pure
	// function of the input (tests pass a fixed clock). A zero Now makes every
	// read look future-stamped, which fails safe: unknown.
	Now time.Time
	// Merged: the merge is already recorded. It decides before anything else:
	// work that is already merged never alerts, whatever the other evidence
	// says — including when that evidence is missing.
	Merged bool
	// Repo: the repository's detected default branch and when it was read.
	Repo MergeRepoFacts
	// TargetBranch: the branch the PR actually targets, as verified by the
	// fetch layer. Empty means the target is unknown — never "main".
	TargetBranch string
	// Rules: what was read about the target branch, and the PR-level review
	// signals that go with it.
	Rules MergeRuleFacts
	// Person: the ticket's owner: who would be asked to review.
	Person MergeAlertPerson
}

// Where a repository fact came from (MergeRepoFacts.Source).
const (
	// MergeAlertSourceDetected: read from the repository (§1a: REST
	// `.default_branch` server-side). It expires on the evidence TTL.
	MergeAlertSourceDetected = "detected"
	// MergeAlertSourceManual: set by a person in AMP's repo settings
	// (`github_default_branch`, §2's fallback/manual path, artifact #27
	// state 9's resolution). A person's declaration does not go stale 15
	// minutes later; it is replaced or cleared by another declaration.
	MergeAlertSourceManual = "manual"
)

// MergeRepoFacts is the repository's default branch (§1a: REST
// `.default_branch` server-side, a person's setting as the fallback). AMP's
// server has no checkout, so neither a guess nor `origin/HEAD` read somewhere
// else is acceptable here.
type MergeRepoFacts struct {
	// DefaultBranch is the default branch. Empty means it is not known:
	// never "main", never a guess.
	DefaultBranch string
	// Source: MergeAlertSourceDetected or MergeAlertSourceManual. Unknown
	// source is treated as detected, so it ages out rather than living
	// forever.
	Source string
	// ReadAt is when it was read. Zero — or, for a detected fact, older than
	// the evidence TTL — means the fact is not evidence. A manual fact has no
	// read time to age.
	ReadAt time.Time
}

// MergeRuleFacts is the evaluated rules of the PR's target branch
// (`GET /repos/{owner}/{repo}/rules/branches/{branch}`, §1b) plus the PR-level
// signals that can stand in for a rules read that could not be complete
// (§1c, §3.3).
type MergeRuleFacts struct {
	// ReadAt is when the rules were read for this target. Zero or stale means
	// the rules are not evidence, and nothing about the target can be
	// decided.
	ReadAt time.Time
	// Partial: the read answered but cannot prove the ABSENCE of a rule that
	// needs this person. Typical causes: rules/branches returned [] while
	// classic branch protection was unreadable (a member token gets 404 for
	// both "not protected" and "no permission"), the request failed, or the
	// repository is on a host whose rule model AMP does not read. Partial is
	// never read as "no rule" (§1b, §5).
	Partial bool
	// PullRequest is the `pull_request` rule that applies to the target, nil
	// when none does. Only this rule type can demand a person's approval; the
	// others (deletion, non_fast_forward, required_status_checks,
	// commit_message_pattern, …) block merges without asking anyone.
	PullRequest *MergePullRequestRule
	// ReviewDecision is the PR's GitHub review decision: "REVIEW_REQUIRED",
	// "APPROVED", "CHANGES_REQUESTED", or "" when GitHub has not computed it
	// yet (it does so asynchronously right after a PR opens — gotcha in §6).
	//
	// It reaches this struct through the real pipeline, not only from tests:
	// the reader fetches it over GraphQL (REST `pulls/{n}` does not carry it),
	// the cached pull write stores it in 044's closed vocabulary, and
	// mergeAlertInputFromView copies it here. It is "" when that read failed,
	// which is unknown and never "no review required" (AMP #3219).
	ReviewDecision string
	// MergeableState is the PR's mergeable state: "blocked", "clean", … "" or
	// "unknown" when it is not computed yet. Blocked does not by itself mean
	// "a person is required" — required status checks block too — so it is
	// only ever a positive signal paired with a requested reviewer. It comes
	// from the same REST pull read as the base and reviewers, and "unknown"
	// is what a failed or uncomputed read leaves.
	MergeableState string
	// RequestedReviewers are the PR's requested user reviewers (logins).
	// GitHub over-approximates this on purpose: code owners are auto-requested
	// whether or not a rule requires them.
	RequestedReviewers []string
}

// MergePullRequestRule is the subset of a `pull_request` rule's parameters
// that decides whether a person is required (§1b).
type MergePullRequestRule struct {
	// RulesetSource is where the rule is configured: "Repository" or
	// "Organization" (GitHub's `ruleset_source_type`).
	RulesetSource string
	// RulesetName is the ruleset's name (GitHub's `ruleset_source`), so the
	// rule-box can name the exact rule artifact #27 state 3 shows.
	RulesetName string
	// RequiredApprovingReviewCount: 0 means pull requests are required but
	// nobody in particular must approve.
	RequiredApprovingReviewCount int
	// RequireCodeOwnerReview: an approval from a code owner of the changed
	// paths is required.
	RequireCodeOwnerReview bool
	// RequiredReviewers are the users and teams the ruleset names
	// ("Review required").
	RequiredReviewers []MergeRuleReviewer
	// RequireLastPushApproval: an approval from someone other than the last
	// pusher is required. It demands *someone's* approval, not a named
	// person's.
	RequireLastPushApproval bool
	// RequireExtraApprovalForUnattributedChanges: one more approval when
	// commits can't be attributed. Also not person-specific.
	RequireExtraApprovalForUnattributedChanges bool
}

// MergeRuleReviewer is one entry of a rule's required_reviewers.
type MergeRuleReviewer struct {
	// Login is the user's login, or the team's slug when Team is set.
	Login string
	// Team: the entry names a team, so which people it requires depends on
	// team membership.
	Team bool
}

// MergeAlertPerson is the ticket's owner, seen through what decides an alert:
// their GitHub login, and whether CODEOWNERS makes them an owner of the PR's
// changed paths.
type MergeAlertPerson struct {
	// Login is the person's GitHub login. Empty means it is not known, so a
	// rule that names users cannot be judged against them.
	Login string
	// CodeOwner: whether CODEOWNERS makes them an owner of the changed paths.
	// nil means unresolved (CODEOWNERS not read, or GitHub's resolution not
	// available) — which can never quiet an item.
	CodeOwner *bool
}

// MergeAlert is the classification of one merge item.
type MergeAlert struct {
	// Kind is one of MergeAlertDefaultBranch, MergeAlertRule,
	// MergeAlertInternal, MergeAlertUnknown, MergeAlertNone.
	Kind string
	// Reason is one line saying why, in the terms the card or chip shows
	// (state 3's "why you're asked" line, state 9's warning). It is always
	// set, including for MergeAlertNone.
	Reason string
	// Rule is set for a rule alert only: which rule requires the person — the
	// ruleset it comes from and what it demands (artifact #27 state 3's
	// rule-box). Presentation copy belongs to the UI ticket; this is the
	// factual detail behind it. Empty for every other kind, which has no rule
	// to name.
	Rule string
	// DefaultBranch is the detected default branch this item was classified
	// against — the repo chip's "default: main · detected from …" — and ""
	// when it could not be read.
	DefaultBranch string
}

// ClassifyMergeAlert decides which of the four arms a merge item falls in,
// and says why. It is pure: the same input always gives the same answer, and
// nothing outside the input is consulted or changed.
func ClassifyMergeAlert(in MergeAlertInput) MergeAlert {
	db := in.Repo.DefaultBranch

	// Already merged: nothing waits on it, so nobody is asked (§3: "Already
	// merged never alerts"). Checked before any evidence, so a merged item can
	// never raise the state-9 warn chip either.
	if in.Merged {
		return MergeAlert{
			Kind:          MergeAlertNone,
			Reason:        "the merge is already recorded; nothing waits on it",
			DefaultBranch: db,
		}
	}

	// Clause (a) needs the detected default branch, and clause (b) needs it to
	// know the target is not the default. Without it nothing can be decided.
	if db == "" {
		return mergeAlertUnknown("the repository's default branch could not be read", db)
	}
	if in.Repo.Source != MergeAlertSourceManual {
		if why := mergeAlertStaleReason("the repository's default branch", in.Repo.ReadAt, in.Now); why != "" {
			return mergeAlertUnknown(why, db)
		}
	}
	if in.TargetBranch == "" {
		return mergeAlertUnknown("the PR's target branch is unknown", db)
	}

	// Clause (a): the default branch. Safe even when the rules are unreadable.
	if in.TargetBranch == db {
		return MergeAlert{
			Kind:          MergeAlertDefaultBranch,
			Reason:        fmt.Sprintf("the PR targets %s, the repository's default branch", db),
			DefaultBranch: db,
		}
	}

	// From here the PR targets a branch that is not the default, so only a
	// rule that actually requires this person may alert — and only complete,
	// fresh evidence may quiet it.
	if why := mergeAlertStaleReason("the rules on "+in.TargetBranch, in.Rules.ReadAt, in.Now); why != "" {
		return mergeAlertUnknown(why, db)
	}

	required, unresolved, detail := mergeAlertRuleStanding(in.Rules.PullRequest, in.Person)
	switch {
	case required:
		return MergeAlert{
			Kind:          MergeAlertRule,
			Reason:        fmt.Sprintf("the PR targets %s, and a rule requires your approval", in.TargetBranch),
			Rule:          detail,
			DefaultBranch: db,
		}
	case in.Rules.Partial:
		// The rules could not be read completely. Fall back to the PR-level
		// positive signals (§3.3): GitHub saying reviews are required, or the
		// PR being blocked, *plus* this person among the requested reviewers.
		// Anything less is unknown — never "internal", and never an alert
		// from a requested reviewer alone.
		if mergeAlertPositiveSignal(in.Rules) && mergeAlertRequested(in.Rules.RequestedReviewers, in.Person.Login) {
			return MergeAlert{
				Kind:          MergeAlertRule,
				Reason:        fmt.Sprintf("the PR targets %s; the rules could not be read completely, but GitHub reports a required review and you are a requested reviewer", in.TargetBranch),
				Rule:          "the branch's rules could not be read completely; GitHub reports a review is required, and you are a requested reviewer",
				DefaultBranch: db,
			}
		}
		return mergeAlertUnknown(fmt.Sprintf("the rules on %s could not be read completely, and the PR shows no approval requirement that names you", in.TargetBranch), db)
	case unresolved:
		// A rule demands an approval this person might have to give, and the
		// evidence cannot say whose: a team whose membership is not resolved
		// (§6), an owner list AMP cannot check, or an approval that must come
		// from *someone* (required_approving_review_count,
		// require_last_push_approval,
		// require_extra_approval_for_unattributed_changes — §6's
		// person-indirect rules, whose alert policy is a product decision).
		// A human approval is required, so this cannot be "internal"; which
		// human is not knowable here, so it cannot be a rule alert either.
		return mergeAlertUnknown(fmt.Sprintf("the PR targets %s and a rule requires a human approval that the evidence cannot attribute to you or anyone else", in.TargetBranch), db)
	case strings.EqualFold(in.Rules.ReviewDecision, "REVIEW_REQUIRED"):
		// Complete rules that require nothing, and a PR GitHub says needs a
		// review: the two reads contradict each other. Ambiguous evidence is
		// unknown, not internal.
		return mergeAlertUnknown(fmt.Sprintf("the rules on %s require no approval, but the PR reports that a review is required", in.TargetBranch), db)
	default:
		return MergeAlert{
			Kind:          MergeAlertInternal,
			Reason:        fmt.Sprintf("%s is not the repository's default branch (%s), and no rule requires your approval", in.TargetBranch, db),
			DefaultBranch: db,
		}
	}
}

// mergeAlertUnknown is the honest-unknown arm, with the default branch it was
// judged against ("" when that too is unknown).
func mergeAlertUnknown(reason, defaultBranch string) MergeAlert {
	return MergeAlert{Kind: MergeAlertUnknown, Reason: reason, DefaultBranch: defaultBranch}
}

// mergeAlertStaleReason says why a read cannot be trusted as evidence, or ""
// when it can: never read, read at an unknown time, older than the TTL, or
// stamped in the future (a clock this classifier cannot reason about — a zero
// Now lands here).
func mergeAlertStaleReason(what string, readAt, now time.Time) string {
	if readAt.IsZero() {
		return what + " has not been read"
	}
	age := now.Sub(readAt)
	if age < 0 {
		return what + " is stamped in the future, so its age cannot be judged"
	}
	if age > MergeAlertEvidenceTTL {
		return fmt.Sprintf("%s was read %s ago, older than the %s TTL, so it is stale", what, age.Round(time.Second), MergeAlertEvidenceTTL)
	}
	return ""
}

// mergeAlertRuleStanding judges a pull_request rule against the person (§1c):
// required when the rule names them, or names a code owner they provably are;
// unresolved when it demands an approval they might have to give but the
// evidence cannot attribute; not required when it provably does not concern
// them. detail is the rule-box line for a required standing.
func mergeAlertRuleStanding(r *MergePullRequestRule, p MergeAlertPerson) (required, unresolved bool, detail string) {
	if r == nil {
		return false, false, ""
	}

	if r.RequireCodeOwnerReview {
		switch {
		case p.CodeOwner == nil:
			// CODEOWNERS or GitHub's path→owner resolution is missing, so
			// whether this rule names the person is unknown.
			unresolved = true
		case *p.CodeOwner:
			return true, false, mergeAlertRuleDetail(r, "you are a code owner of the changed paths")
		}
	}

	for _, rev := range r.RequiredReviewers {
		switch {
		case rev.Team:
			// Which people a team names depends on a membership map AMP does
			// not resolve yet (§6: team-based matching is an open decision).
			unresolved = true
		case rev.Login == "" || p.Login == "":
			// A named reviewer, and no way to compare them to the person.
			unresolved = true
		case strings.EqualFold(rev.Login, p.Login):
			return true, false, mergeAlertRuleDetail(r, "you are a required reviewer")
		}
	}

	// An approval someone must give, without naming who. See the unresolved
	// arm above: a human approval is required, so this is never "internal",
	// and "rule" would claim it is yours.
	//
	// RequiredApprovingReviewCount > 0 belongs here with the two §6 rules: it
	// demands one approval from anyone who can give it, and every collaborator
	// could. Pairing it with the PR's requested reviewers to guess that this
	// person is the one would be inferring a required approval from a reviewer
	// request — which §1c warns against (code owners are auto-requested with
	// no protection in place) and this ticket forbids. The result is the
	// honest one: a real, pending human approval that cannot be attributed.
	//
	// PENDING USER DECISION (§6, "person-indirect rules"): if the user decides
	// "you could give this approval" is enough, this branch turns into a rule
	// alert instead. Until then it is unknown.
	//
	// The cost is bounded, and deliberately not paid on the common case: it
	// only bites when the NON-DEFAULT target is itself protected with one of
	// these rules — a branch where a human approval really is required to
	// merge — so state 9 there is truthful, not noise. An unprotected or
	// plain-rule integration branch still quiets to internal (§1b verified
	// this repo's ruleset is scoped to ~DEFAULT_BRANCH, so it returns [] for
	// the integration branch and never reaches this line).
	if r.RequiredApprovingReviewCount > 0 || r.RequireLastPushApproval || r.RequireExtraApprovalForUnattributedChanges {
		unresolved = true
	}

	return false, unresolved, ""
}

// mergeAlertRuleDetail names the rule the way artifact #27's rule-box does:
// where it is configured, what it demands, then why it is this person's.
func mergeAlertRuleDetail(r *MergePullRequestRule, why string) string {
	demands := make([]string, 0, 3)
	if n := r.RequiredApprovingReviewCount; n > 0 {
		demands = append(demands, fmt.Sprintf("%d approving %s", n, mergeAlertPlural(n, "review", "reviews")))
	}
	if r.RequireCodeOwnerReview {
		demands = append(demands, "a code owner's approval")
	}
	if len(r.RequiredReviewers) > 0 {
		names := make([]string, 0, len(r.RequiredReviewers))
		for _, rev := range r.RequiredReviewers {
			name := "@" + rev.Login
			if rev.Team {
				name += " (team)"
			}
			names = append(names, name)
		}
		demands = append(demands, "a review from "+strings.Join(names, ", "))
	}
	if len(demands) == 0 {
		// Only reachable through require_code_owner_review resolving true
		// with no other demand; keep the sentence whole rather than empty.
		demands = append(demands, "a review")
	}
	origin := mergeAlertRuleOrigin(r)
	if why == "" {
		return origin + ": " + strings.Join(demands, ", ")
	}
	return origin + ": " + strings.Join(demands, ", ") + " — " + why
}

// mergeAlertRuleOrigin names where a rule is configured, for the rule-box.
func mergeAlertRuleOrigin(r *MergePullRequestRule) string {
	switch {
	case r.RulesetSource != "" && r.RulesetName != "":
		return fmt.Sprintf("%s ruleset %q", r.RulesetSource, r.RulesetName)
	case r.RulesetSource != "":
		return r.RulesetSource + " ruleset"
	case r.RulesetName != "":
		return fmt.Sprintf("ruleset %q", r.RulesetName)
	default:
		return "the branch's pull request rule"
	}
}

// mergeAlertPositiveSignal reports a PR-level sign that a review is genuinely
// required: GitHub says so, or the PR is blocked from merging. Blocked alone
// can mean required status checks, so this only ever pairs with the requested
// reviewers — never on its own (§1c, §3.3).
//
// Both inputs are populated by the production read path (AMP #3219); until
// then this was true only of hand-built test inputs, so the §3.3 arm could not
// fire for a real merge. merge_alert_pipeline_test.go pins the live chain.
func mergeAlertPositiveSignal(f MergeRuleFacts) bool {
	return strings.EqualFold(f.ReviewDecision, "REVIEW_REQUIRED") ||
		strings.EqualFold(f.MergeableState, "blocked")
}

// mergeAlertRequested reports whether the person is among the PR's requested
// reviewers. GitHub logins are case-insensitive; an unknown login matches
// nothing.
func mergeAlertRequested(requested []string, login string) bool {
	if login == "" {
		return false
	}
	return slices.ContainsFunc(requested, func(r string) bool { return strings.EqualFold(r, login) })
}

// mergeAlertPlural picks the word for n, in the style the rest of the API's
// copy uses.
func mergeAlertPlural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
