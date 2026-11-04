// Package githubfacts reads the GitHub repository facts AMP's merge-alert
// policy needs: the repository's default branch, a pull request's actual
// target and requested reviewers, the rules evaluated against a target
// branch, and whether classic branch protection could be read at all
// (AMP #3151; the mechanism is recorded in
// decisions/repo-default-branch.md, verified live against jacobsims214/amp
// on 2026-11-03).
//
// It is a reader and nothing else: no cache, no database, no classifier and
// no application wiring. Two guarantees shape it.
//
//   - Absence of knowledge is never reported as absence of a rule. A read
//     that fails — 404 for a private repo the token cannot see, 403 for a
//     scope the token lacks, a timeout, an unparseable body — leaves the
//     fact it was fetching unknown, and Completeness says the facts are
//     Partial or Unknown. Only Complete may be read as "nothing on this
//     branch requires a review" (decisions/repo-default-branch.md §5).
//   - The credential never leaves the client. It is sent as a header, never
//     in the URL; an error carries a failure class and GitHub's own message
//     field, never the request or its headers, and the token is scrubbed
//     from that message in case a body ever echoes it. Nothing here logs.
//
// Every call is bounded. One deadline covers a single request, another
// covers the whole Facts read, and at most one retry happens — only for a
// rate-limit answer whose advertised wait is short enough to fit
// (Options.MaxRetryWait), never a backoff loop.
package githubfacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is GitHub's public REST API. The base is an option so
	// tests can point at a stub; GitHub Enterprise Server is explicitly out
	// of scope (decisions/repo-default-branch.md §6) and needs its own host
	// and credential decision.
	DefaultBaseURL = "https://api.github.com"

	// DefaultTimeout bounds one HTTP request; DefaultTotal bounds one
	// Facts read. DefaultMaxBody caps how much of a response is decoded.
	DefaultTimeout      = 10 * time.Second
	DefaultTotal        = 30 * time.Second
	DefaultMaxBody      = 4 << 20
	DefaultMaxRetryWait = 2 * time.Second

	// maxAttempts is the whole retry budget: one retry, and only for the
	// rate-limit case in retryWait. There is no backoff loop.
	maxAttempts = 2

	// apiVersion pins the REST API version so a GitHub-side default change
	// cannot move the response shapes under us.
	apiVersion = "2022-11-28"
	userAgent  = "AMP-GitHubFacts/1"
)

// Op names one read. Completeness is computed from these, and a Read's Op
// says which fact its failure left unknown.
type Op string

// The four reads Facts makes. OpPull happens only when a PR number is given;
// OpRules and OpProtection happen only when a target branch is known.
const (
	OpRepo       Op = "repo"       // GET /repos/{owner}/{repo} → .default_branch
	OpPull       Op = "pull"       // GET /repos/{owner}/{repo}/pulls/{n}
	OpRules      Op = "rules"      // GET /repos/{owner}/{repo}/rules/branches/{branch}
	OpProtection Op = "protection" // GET /repos/{owner}/{repo}/branches/{branch}/protection
)

// Source says where the branch the rules were read for came from.
type Source string

const (
	// SourcePull means the PR's own base.ref: GitHub's answer for what the
	// PR targets, which always overrides a run's reported hint.
	SourcePull Source = "pull"
	// SourceHint means only a hint (the run's base_branch) was available,
	// so which branch the PR targets is still unconfirmed.
	SourceHint Source = "hint"
)

// Completeness says how much of the requested fact set could actually be
// read. It is the guard on the merge policy's safety rule:
//
//   - Complete: every read the request needed answered. Only here may an
//     empty rule list be read as "no rule applies".
//   - Partial: some reads answered, some did not. Rules read plus classic
//     protection unreadable is the common case (a member token cannot read
//     the protection endpoint), and it must stay unknown.
//   - Unknown: no read answered at all — no token, a private repo, an
//     outage, a non-GitHub host.
type Completeness string

const (
	Complete Completeness = "complete"
	Partial  Completeness = "partial"
	Unknown  Completeness = "unknown"
)

// Kind classifies a failed read without exposing what was sent. Callers
// branch on it; the message is for a person to read and never carries the
// credential.
type Kind string

const (
	// KindNotFound is a 404: the resource is missing *or* the credential
	// cannot see it — GitHub does not distinguish the two, so neither may
	// we. Never "no rule here".
	KindNotFound Kind = "not_found"
	// KindForbidden is a 401/403: an expired, revoked or under-scoped
	// token, or no access to the repository.
	KindForbidden Kind = "forbidden"
	// KindRateLimited is a 429, or a 403 whose rate-limit headers say the
	// budget is exhausted.
	KindRateLimited Kind = "rate_limited"
	// KindTimeout is a deadline: Options.Timeout on one request, the whole
	// read's budget, or the caller's context.
	KindTimeout Kind = "timeout"
	// KindCanceled is the caller giving up before the read finished.
	KindCanceled Kind = "canceled"
	// KindNetwork is a transport failure: DNS, connection refused, TLS, a
	// malformed response.
	KindNetwork Kind = "network"
	// KindServer is a 5xx from GitHub.
	KindServer Kind = "server"
	// KindMoved is a redirect (301/302/307/308) — a renamed or transferred
	// repository. Following it would report the new repository's facts
	// under the old slug, so it is reported instead.
	KindMoved Kind = "moved"
	// KindInvalid is an unreadable response body, or a request that cannot
	// be built (a malformed slug or branch).
	KindInvalid Kind = "invalid"
	// KindNoCredential is a token that could not be supplied at all: the
	// project's credential is missing, unreadable or undecryptable.
	KindNoCredential Kind = "no_credential"
	// KindNoTarget is a rule read that could not be attempted because no
	// target branch was known — no PR base and no hint.
	KindNoTarget Kind = "no_target"
	// KindUnsupportedHost is a pull request URL that does not name
	// github.com. GitHub Enterprise Server is out of scope
	// (decisions/repo-default-branch.md §6).
	KindUnsupportedHost Kind = "unsupported_host"
)

// Error is one failed read, or a request that could not be made. It is a
// typed value so a caller can classify a failure (errors.As, or the Kind
// field) and render "unknown" honestly; it never carries the credential,
// the request headers or a raw response body.
type Error struct {
	Op      Op     // the read that failed
	Kind    Kind   // the failure class
	Status  int    // HTTP status, 0 when no response was received
	Message string // a short, redacted explanation; safe to log and show
	// err is the underlying cause (a transport or context error), kept for
	// errors.Is/errors.As; it never contains the token because it comes
	// from net/http's request layer, not from the headers.
	err error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	op := string(e.Op)
	if op == "" {
		op = "request"
	}
	switch {
	case e.Status > 0:
		return fmt.Sprintf("github facts: %s: HTTP %d (%s): %s", op, e.Status, e.Kind, e.Message)
	case e.Message != "":
		return fmt.Sprintf("github facts: %s: %s: %s", op, e.Kind, e.Message)
	default:
		return fmt.Sprintf("github facts: %s: %s", op, e.Kind)
	}
}

func (e *Error) Unwrap() error { return e.err }

// Read is one read's outcome. A Read that is not OK says which fact stayed
// unknown and why.
type Read struct {
	Op     Op
	OK     bool
	Status int // HTTP status, 0 if the request never got a response
	Err    *Error
}

// Facts is what the reads established about one repository, and how much of
// it could be read. A fact that could not be read keeps its zero value and
// its Op is not OK in Reads: an empty rule list means "no rules" only when
// Completeness is Complete.
type Facts struct {
	Slug   string // owner/repo, as validated from the request
	PR     int    // pull request number, 0 when none was asked for
	ReadAt time.Time

	// DefaultBranch is the repository's default branch, "" when unknown.
	DefaultBranch string
	Pull          PullFacts
	Rules         RulesFacts

	// Target is the branch the rules were read for; TargetSource says
	// whether GitHub confirmed it (the PR's base) or it is still a hint.
	Target       string
	TargetSource Source

	Completeness Completeness
	Reads        []Read // every read attempted, in call order
}

// ReadFor returns the Read for op, if it was attempted.
func (f *Facts) ReadFor(op Op) (Read, bool) {
	for _, r := range f.Reads {
		if r.Op == op {
			return r, true
		}
	}
	return Read{}, false
}

// ReadOK reports whether op was attempted and answered.
func (f *Facts) ReadOK(op Op) bool {
	r, ok := f.ReadFor(op)
	return ok && r.OK
}

// RulesPartial is the merge-alert classifier's MergeRuleFacts.Partial: true
// when the rules read answered but cannot prove the ABSENCE of a rule that
// needs someone. That is the case whenever the branch's evaluated rules
// answered but classic protection did not (a member token gets 404 for "not
// protected" and "no permission" alike), or when either read failed. Partial
// is never read as "no rule": the classifier turns it into unknown unless a
// positive PR-level signal stands in for it (decisions/repo-default-branch.md
// §1b, §5).
//
// This lives here, next to the reads, so the mapping onto the classifier's
// evidence is written once and no caller can invert the safety rule by hand.
func (f *Facts) RulesPartial() bool {
	return !f.ReadOK(OpRules) || !f.ReadOK(OpProtection)
}

// PullFacts is what the PR read established. Known says the read answered;
// nothing in it is usable until it does.
type PullFacts struct {
	Known              bool
	Base               string   // base.ref: the PR's actual target
	RequestedReviewers []string // requested user logins
	RequestedTeams     []string // requested team slugs
	Merged             bool     // merged_at was set
}

// RulesFacts is what the target branch's rules read established.
//
// Known is GitHub's own answer for "all active rules that apply to this
// branch", evaluated across repository and organization rulesets. If the
// read failed, Known is false and Rules is empty: that is unknown, not
// "no rules". Protection carries the older mechanism, which only an admin
// credential can read.
type RulesFacts struct {
	Known      bool
	Rules      []Rule
	Protection ClassicProtection
}

// PullRequestRules returns the rules of type pull_request — the only ones
// that can demand a person's review. Other types (deletion,
// non_fast_forward, required_status_checks, commit_message_pattern, …) never
// require an approval.
func (r RulesFacts) PullRequestRules() []Rule {
	out := make([]Rule, 0, len(r.Rules))
	for _, rule := range r.Rules {
		if rule.Type == "pull_request" {
			out = append(out, rule)
		}
	}
	return out
}

// Rule is one active rule evaluated for a branch. Parameters is filled only
// for Type "pull_request"; the other rule types carry rule type-specific
// parameters this package does not model.
type Rule struct {
	Type              string
	RulesetID         int64
	RulesetSource     string // the ruleset's name
	RulesetSourceType string // "Repository" or "Organization"
	Parameters        RuleParameters
}

// RuleParameters is the subset of a pull_request rule's parameters the merge
// policy reads (decisions/repo-default-branch.md §1b).
type RuleParameters struct {
	RequiredApprovingReviewCount  int
	DismissStaleReviewsOnPush     bool
	RequireCodeOwnerReview        bool
	RequireLastPushApproval       bool
	RequireReviewThreadResolution bool
	// RequireExtraApprovalForUnattributedChanges demands an extra approval
	// when commits cannot be attributed. It is not person-specific.
	RequireExtraApprovalForUnattributedChanges bool
	AllowedMergeMethods                        []string
	RequiredReviewers                          []Reviewer
}

// Reviewer is one entry of a pull_request rule's required_reviewers. GitHub
// returns users and teams in one array, distinguished by Type: a user has a
// Login, a team a Slug.
//
// This package deliberately does not expand a team into its members: "does
// this rule require this person" needs an org-membership lookup that is a
// separate decision (decisions/repo-default-branch.md §6), so an
// unresolved team membership stays unknown rather than being guessed.
type Reviewer struct {
	Type  string // "User" or "Team"
	ID    int64
	Login string // set for a user
	Slug  string // set for a team
}

// ClassicProtection is GitHub's older branch-protection setting for the
// target branch. Reading it needs Administration: read, and GitHub answers
// 404 both for "this branch is not protected" and for "you may not look",
// so a 404 leaves Protected nil — unknown, never false
// (decisions/repo-default-branch.md §1b, §5).
type ClassicProtection struct {
	// Known says the endpoint answered with a readable body. Only then is
	// Protected meaningful.
	Known     bool
	Protected *bool
	// RequiredApprovingReviewCount, RequireCodeOwnerReviews and
	// RequireLastPushApproval are filled when the branch has a
	// required-pull-request-reviews setting.
	RequiredApprovingReviewCount *int
	RequireCodeOwnerReviews      *bool
	RequireLastPushApproval      *bool
}

// TokenSource supplies the credential for a read. It is called once per
// Facts call, so a rotated token is picked up without rebuilding the client.
// An empty token with a nil error means "no credential": the reads still
// run, and a private repository answers 404 — which stays unknown, exactly
// as it should.
type TokenSource func(context.Context) (string, error)

// Options configures a Client. Transport and Token are the injections: a
// test passes an httptest transport and a fixture token, production passes
// a real transport and the project's sealed credential.
type Options struct {
	// BaseURL is the REST API root; DefaultBaseURL when empty.
	BaseURL string
	// Transport carries the requests. http.DefaultTransport when nil, which
	// is what a test replaces to capture the wire.
	Transport http.RoundTripper
	// Token supplies the credential. When nil every read is unauthenticated.
	Token TokenSource
	// Timeout bounds one request; Total bounds one Facts call. Zero uses
	// the defaults; a negative value means "no extra bound beyond the
	// caller's context".
	Timeout time.Duration
	Total   time.Duration
	// MaxBody caps a decoded response body.
	MaxBody int64
	// MaxRetryWait is the longest advertised rate-limit wait the client will
	// sit out for its single retry. A longer wait is reported instead of
	// waited for, so a read can never become a background backoff loop.
	MaxRetryWait time.Duration
	// Now is the clock, for deterministic tests; time.Now when nil.
	Now func() time.Time
}

// Client reads repository facts over HTTP. It is safe for concurrent use:
// it holds no mutable state.
type Client struct {
	baseURL      string
	http         *http.Client
	token        TokenSource
	timeout      time.Duration
	total        time.Duration
	maxBody      int64
	maxRetryWait time.Duration
	now          func() time.Time
}

// New returns a client. Nothing here does I/O.
func New(opts Options) *Client {
	c := &Client{
		baseURL:      strings.TrimRight(opts.BaseURL, "/"),
		token:        opts.Token,
		timeout:      opts.Timeout,
		total:        opts.Total,
		maxBody:      opts.MaxBody,
		maxRetryWait: opts.MaxRetryWait,
		now:          opts.Now,
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	if c.timeout == 0 {
		c.timeout = DefaultTimeout
	}
	if c.total == 0 {
		c.total = DefaultTotal
	}
	if c.maxBody <= 0 {
		c.maxBody = DefaultMaxBody
	}
	if c.maxRetryWait <= 0 {
		c.maxRetryWait = DefaultMaxRetryWait
	}
	if c.now == nil {
		c.now = time.Now
	}
	transport := opts.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	c.http = &http.Client{
		Transport: transport,
		// A redirect is a fact about the repository being gone from this
		// slug, not a fact about the new one; report it (KindMoved).
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c
}

// Request is what to read.
type Request struct {
	// Slug is owner/repo. It is validated and escaped before it reaches a
	// URL, so a value like "../org/repo" cannot escape the path.
	Slug string
	// PR is the pull request number; 0 skips the pull read (and leaves the
	// target unconfirmed unless Target is set).
	PR int
	// Target is a hint: the target branch a run reported. The PR's actual
	// base overwrites it when the pull read answers, because GitHub's
	// answer for what the PR targets is authoritative.
	Target string
}

// Facts reads the repository's merge-relevant facts.
//
// It returns an error only when the request itself is unusable (a malformed
// slug, a negative PR number) — nothing was read in that case. A failed read
// is not an error: it is recorded in Facts.Reads, the fact it was fetching
// stays at its zero value, and Completeness drops to Partial or Unknown, so
// the caller can tell unknown from "nothing applies".
func (c *Client) Facts(ctx context.Context, req Request) (*Facts, error) {
	slug, err := validSlug(req.Slug)
	if err != nil {
		return nil, err
	}
	if req.PR < 0 {
		return nil, &Error{Kind: KindInvalid, Message: "a pull request number cannot be negative"}
	}
	if c.total > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.total)
		defer cancel()
	}

	owner, repo, _ := strings.Cut(slug, "/")
	repoPath := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	f := &Facts{Slug: slug, PR: req.PR, ReadAt: c.now().UTC()}

	f.Reads = append(f.Reads, c.readRepo(ctx, repoPath, f))

	if req.PR > 0 {
		f.Reads = append(f.Reads, c.readPull(ctx, repoPath, req.PR, f))
	}

	f.Target, f.TargetSource = targetFor(f, req.Target)
	if f.Target == "" {
		// Nothing to evaluate rules against. Record it rather than silently
		// reading some other branch: the caller must see why rules are
		// unknown, and Completeness stays below Complete.
		f.Reads = append(f.Reads, Read{Op: OpRules, Err: &Error{
			Op:      OpRules,
			Kind:    KindNoTarget,
			Message: "no target branch: the pull request's base was not read and no hint was given",
		}})
		f.Completeness = completeness(f, req)
		return f, nil
	}

	f.Reads = append(f.Reads, c.readRules(ctx, repoPath, f.Target, f))
	f.Reads = append(f.Reads, c.readProtection(ctx, repoPath, f.Target, f))
	f.Completeness = completeness(f, req)
	return f, nil
}

// targetFor resolves the branch rules are read for: GitHub's own answer
// when the pull read gave one, else the caller's hint.
func targetFor(f *Facts, hint string) (string, Source) {
	if f.Pull.Known && f.Pull.Base != "" {
		return f.Pull.Base, SourcePull
	}
	if hint = strings.TrimSpace(hint); hint != "" {
		return hint, SourceHint
	}
	return "", ""
}

// completeness requires every read the request asked for to have answered.
// A read that was never attempted (no target) is not in Reads, so it counts
// as missing — which is the point: an unread rule set can never look like a
// rule-free one.
func completeness(f *Facts, req Request) Completeness {
	required := []Op{OpRepo, OpRules, OpProtection}
	if req.PR > 0 {
		required = append(required, OpPull)
	}
	answered := 0
	for _, op := range required {
		if f.ReadOK(op) {
			answered++
		}
	}
	switch {
	case answered == len(required):
		return Complete
	case answered == 0:
		return Unknown
	default:
		return Partial
	}
}

// ---- the reads ----

func (c *Client) readRepo(ctx context.Context, repoPath string, f *Facts) Read {
	var body struct {
		DefaultBranch string `json:"default_branch"`
	}
	r := c.get(ctx, OpRepo, repoPath, &body)
	if r.OK {
		f.DefaultBranch = body.DefaultBranch
	}
	return r
}

func (c *Client) readPull(ctx context.Context, repoPath string, number int, f *Facts) Read {
	var body struct {
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		RequestedReviewers []struct {
			Login string `json:"login"`
		} `json:"requested_reviewers"`
		RequestedTeams []struct {
			Slug string `json:"slug"`
		} `json:"requested_teams"`
		// merged_at is null until the PR merges; a merged PR requires
		// nobody's approval.
		MergedAt *string `json:"merged_at"`
	}
	r := c.get(ctx, OpPull, repoPath+"/pulls/"+strconv.Itoa(number), &body)
	if !r.OK {
		return r
	}
	f.Pull.Known = true
	f.Pull.Base = body.Base.Ref
	f.Pull.Merged = body.MergedAt != nil
	for _, rv := range body.RequestedReviewers {
		if rv.Login != "" {
			f.Pull.RequestedReviewers = append(f.Pull.RequestedReviewers, rv.Login)
		}
	}
	for _, tm := range body.RequestedTeams {
		if tm.Slug != "" {
			f.Pull.RequestedTeams = append(f.Pull.RequestedTeams, tm.Slug)
		}
	}
	return r
}

func (c *Client) readRules(ctx context.Context, repoPath, branch string, f *Facts) Read {
	// A branch name with a slash must reach the wire as one escaped path
	// segment (fix%2Factionable-merge-workflow): unencoded, GitHub answers
	// 404 or routes somewhere else
	// (decisions/repo-default-branch.md, Gotchas).
	var body []struct {
		Type              string          `json:"type"`
		RulesetID         int64           `json:"ruleset_id"`
		RulesetSource     string          `json:"ruleset_source"`
		RulesetSourceType string          `json:"ruleset_source_type"`
		Parameters        json.RawMessage `json:"parameters"`
	}
	r := c.get(ctx, OpRules, repoPath+"/rules/branches/"+url.PathEscape(branch), &body)
	if !r.OK {
		return r
	}
	f.Rules.Known = true
	f.Rules.Rules = make([]Rule, 0, len(body))
	for _, raw := range body {
		rule := Rule{
			Type:              raw.Type,
			RulesetID:         raw.RulesetID,
			RulesetSource:     raw.RulesetSource,
			RulesetSourceType: raw.RulesetSourceType,
		}
		if raw.Type == "pull_request" && len(raw.Parameters) > 0 {
			// A pull_request rule whose parameters do not parse leaves the
			// rule reported with zero parameters; the classifier must not
			// read "0 required reviews" out of that, so say so rather than
			// failing the whole read (the rules list itself is a fact).
			var p struct {
				RequiredApprovingReviewCount               int        `json:"required_approving_review_count"`
				DismissStaleReviewsOnPush                  bool       `json:"dismiss_stale_reviews_on_push"`
				RequireCodeOwnerReview                     bool       `json:"require_code_owner_review"`
				RequireLastPushApproval                    bool       `json:"require_last_push_approval"`
				RequireExtraApprovalForUnattributedChanges bool       `json:"require_extra_approval_for_unattributed_changes"`
				RequireReviewThreadResolution              bool       `json:"required_review_thread_resolution"`
				AllowedMergeMethods                        []string   `json:"allowed_merge_methods"`
				RequiredReviewers                          []Reviewer `json:"required_reviewers"`
			}
			if err := json.Unmarshal(raw.Parameters, &p); err == nil {
				rule.Parameters = RuleParameters{
					RequiredApprovingReviewCount:               p.RequiredApprovingReviewCount,
					DismissStaleReviewsOnPush:                  p.DismissStaleReviewsOnPush,
					RequireCodeOwnerReview:                     p.RequireCodeOwnerReview,
					RequireLastPushApproval:                    p.RequireLastPushApproval,
					RequireExtraApprovalForUnattributedChanges: p.RequireExtraApprovalForUnattributedChanges,
					RequireReviewThreadResolution:              p.RequireReviewThreadResolution,
					AllowedMergeMethods:                        p.AllowedMergeMethods,
					RequiredReviewers:                          p.RequiredReviewers,
				}
			}
		}
		f.Rules.Rules = append(f.Rules.Rules, rule)
	}
	return r
}

func (c *Client) readProtection(ctx context.Context, repoPath, branch string, f *Facts) Read {
	var body struct {
		RequiredPullRequestReviews *struct {
			RequiredApprovingReviewCount int  `json:"required_approving_review_count"`
			RequireCodeOwnerReviews      bool `json:"require_code_owner_reviews"`
			RequireLastPushApproval      bool `json:"require_last_push_approval"`
		} `json:"required_pull_request_reviews"`
	}
	r := c.get(ctx, OpProtection, repoPath+"/branches/"+url.PathEscape(branch)+"/protection", &body)
	// A failure here — most often the 404 GitHub returns both for "not
	// protected" and for "a member token may not look" — leaves
	// Protection.Known false. Never set Protected=false from a failure.
	if !r.OK {
		return r
	}
	protected := true
	f.Rules.Protection = ClassicProtection{Known: true, Protected: &protected}
	if p := body.RequiredPullRequestReviews; p != nil {
		count, codeOwners, lastPush := p.RequiredApprovingReviewCount, p.RequireCodeOwnerReviews, p.RequireLastPushApproval
		f.Rules.Protection.RequiredApprovingReviewCount = &count
		f.Rules.Protection.RequireCodeOwnerReviews = &codeOwners
		f.Rules.Protection.RequireLastPushApproval = &lastPush
	}
	return r
}

// ---- the HTTP layer ----

// get performs one read, with the single bounded retry for a short
// rate-limit answer. On success it decodes into out.
func (c *Client) get(ctx context.Context, op Op, path string, out any) Read {
	r := Read{Op: op}

	var token string
	if c.token != nil {
		var err error
		token, err = c.token(ctx)
		if err != nil {
			// The credential store could not answer. Report the class, not
			// the store's error: it may quote the ciphertext or the key.
			r.Err = &Error{Op: op, Kind: KindNoCredential, Message: "the GitHub credential could not be read", err: err}
			return r
		}
	}

	for attempt := 1; ; attempt++ {
		status, header, body, err := c.do(ctx, token, path)
		r.Status = status
		if err != nil {
			r.Err = transportError(op, err)
			return r
		}
		if status >= 200 && status <= 299 {
			if out != nil {
				if derr := json.Unmarshal(body, out); derr != nil {
					r.Err = &Error{Op: op, Kind: KindInvalid, Status: status,
						Message: "the response body could not be decoded"}
					return r
				}
			}
			r.OK = true
			return r
		}

		e := c.statusError(op, status, header, body, token)
		if e.Kind == KindRateLimited && attempt < maxAttempts {
			if wait, ok := c.retryWait(ctx, header); ok {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					r.Err = transportError(op, ctx.Err())
					return r
				case <-timer.C:
				}
				continue
			}
		}
		r.Err = e
		return r
	}
}

// do sends one request and reads at most MaxBody of the answer.
func (c *Client) do(ctx context.Context, token, path string) (int, http.Header, []byte, error) {
	reqCtx := ctx
	if c.timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if token != "" {
		// The token travels in a header, never in the URL, so it cannot
		// reach a log, a redirect Location or an error message by accident.
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody))
	if err != nil {
		return resp.StatusCode, resp.Header, nil, err
	}
	return resp.StatusCode, resp.Header, body, nil
}

// transportError classifies a request that got no usable answer.
func transportError(op Op, err error) *Error {
	e := &Error{Op: op, Kind: KindNetwork, Message: "the request failed", err: err}
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		e.Kind, e.Message = KindTimeout, "the request timed out"
	case errors.Is(err, context.Canceled):
		e.Kind, e.Message = KindCanceled, "the request was canceled"
	case errors.As(err, &netErr) && netErr.Timeout():
		e.Kind, e.Message = KindTimeout, "the request timed out"
	}
	// Keep the cause's own words out of Message: a *url.Error quotes the
	// URL, and while the URL holds no credential, a short fixed class is
	// what a person needs. The cause is still available via Unwrap.
	return e
}

// statusError classifies a non-2xx answer. The message is GitHub's own
// message field (never the raw body, which could echo anything), with the
// credential scrubbed out of it.
func (c *Client) statusError(op Op, status int, header http.Header, body []byte, token string) *Error {
	e := &Error{Op: op, Status: status, Message: redact(apiMessage(body, header, status), token)}
	switch {
	case status == http.StatusNotFound:
		e.Kind = KindNotFound
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		if status == http.StatusForbidden && rateLimited(header) {
			e.Kind = KindRateLimited
		} else {
			e.Kind = KindForbidden
		}
	case status == http.StatusTooManyRequests:
		e.Kind = KindRateLimited
	case status >= 300 && status <= 399:
		e.Kind = KindMoved
		if loc := header.Get("Location"); loc != "" {
			e.Message = "moved: " + redact(loc, token)
		}
	case status >= 500:
		e.Kind = KindServer
	default:
		e.Kind = KindInvalid
	}
	return e
}

// apiMessage pulls GitHub's message field out of an error body. It never
// returns the body itself.
func apiMessage(body []byte, header http.Header, status int) string {
	var payload struct {
		Message string `json:"message"`
	}
	if len(body) > 0 && json.Unmarshal(body, &payload) == nil && payload.Message != "" {
		return payload.Message
	}
	if text := http.StatusText(status); text != "" {
		return text
	}
	return "unexpected response"
}

// rateLimited reports whether a 403 is a rate-limit answer rather than a
// permission one.
func rateLimited(header http.Header) bool {
	if header.Get("Retry-After") != "" {
		return true
	}
	return header.Get("X-RateLimit-Remaining") == "0"
}

// retryWait reports how long to wait before the single retry, and whether
// the wait is short enough and leaves the read enough time. Anything longer
// than Options.MaxRetryWait is not waited for: a read reports the rate limit
// instead of turning into a backoff loop.
func (c *Client) retryWait(ctx context.Context, header http.Header) (time.Duration, bool) {
	wait, ok := advertisedWait(header)
	if !ok || wait < 0 || wait > c.maxRetryWait {
		return 0, false
	}
	if deadline, has := ctx.Deadline(); has {
		// Leave the retry itself some room; a wait that eats the whole
		// budget would only convert a rate-limit error into a timeout.
		remaining := time.Until(deadline)
		if remaining <= wait {
			return 0, false
		}
	}
	return wait, true
}

// advertisedWait is the wait GitHub asked for: Retry-After in seconds,
// else the gap to X-RateLimit-Reset.
func advertisedWait(header http.Header) (time.Duration, bool) {
	if ra := header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second, true
		}
	}
	if reset := header.Get("X-RateLimit-Reset"); reset != "" {
		if unix, err := strconv.ParseInt(strings.TrimSpace(reset), 10, 64); err == nil {
			return time.Until(time.Unix(unix, 0)), true
		}
	}
	return 0, false
}

// redact removes a credential from text that a person will read.
func redact(text, token string) string {
	if token == "" || text == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "[redacted]")
}

// ---- identity ----

// slugPart is what GitHub allows in an owner or repository name. Anything
// else is rejected before it can reach a URL, which is what keeps a slug
// like "../../org/repo" out of the path.
var slugPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// validSlug checks and normalizes an owner/repo slug.
func validSlug(slug string) (string, *Error) {
	slug = strings.TrimSpace(slug)
	owner, repo, ok := strings.Cut(slug, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") ||
		!slugPart.MatchString(owner) || !slugPart.MatchString(repo) {
		return "", &Error{Kind: KindInvalid, Message: "not an owner/repo slug"}
	}
	return owner + "/" + repo, nil
}

// ParsePullURL returns the owner/repo slug and the number of a pull request
// URL of the form https://github.com/{owner}/{repo}/pull/{n} — the identity
// a run's pr_url carries, which needs no project configuration
// (decisions/repo-default-branch.md §2).
//
// Only github.com is accepted. GitHub Enterprise Server and other hosts have
// their own API host, token and rule model and need a separate decision, so
// they are reported (KindUnsupportedHost) rather than guessed at.
func ParsePullURL(raw string) (string, int, *Error) {
	bad := func(kind Kind, msg string) (string, int, *Error) {
		return "", 0, &Error{Kind: kind, Message: msg}
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return bad(KindInvalid, "not a pull request URL")
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && host != "www.github.com" {
		return bad(KindUnsupportedHost, "not a github.com URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return bad(KindInvalid, "not a pull request URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return bad(KindInvalid, "not a pull request URL")
	}
	slug, verr := validSlug(parts[0] + "/" + parts[1])
	if verr != nil {
		return bad(KindInvalid, "not a pull request URL")
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return bad(KindInvalid, "the pull request number is not a number")
	}
	return slug, n, nil
}
