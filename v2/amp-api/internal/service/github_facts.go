package service

// The repository-facts refresh coordinator (AMP #3156, epic 183, story 453).
//
// Three layers meet here, and this file is the only place that knows all of
// them: the reader (`internal/githubfacts`, #3151) makes the GitHub calls, the
// cache (`internal/repository/github_facts.go`, #3153) stores what a read
// established, and the credential service (#3154) supplies the token. The
// reader deliberately has no cache, the cache deliberately has no network, and
// the credential store deliberately answers only "which token"; the policy that
// joins them is this file's:
//
//   - **TTLs.** A repository's default branch and the rules of a target branch
//     are evidence for 15 minutes (DefaultGitHubFactsRepoTTL). Rule content
//     mutates in real time — this repository's ruleset was created and edited
//     within hours of the research (decisions/repo-default-branch.md §1b) — so
//     an older read is not evidence of anything, whatever it says. A pull
//     request's facts change on every reviewer action, so they are evidence for
//     60 seconds (DefaultGitHubFactsPullTTL).
//   - **A negative cache.** After a refresh fails, nothing retries that
//     (project, repository, target) for 60 seconds
//     (DefaultGitHubFactsNegativeTTL). Without it a repository no token can read
//     turns every My work load into another round of doomed GitHub calls.
//   - **Singleflight, scoped by project/repository/target.** Concurrent readers
//     that want the same facts share one refresh. The scope is part of the key,
//     not a detail: a refresh keyed by slug alone would let project A's request
//     answer project B's read with facts read under A's credential — the
//     cross-project leak #3153 went to some trouble to make impossible in the
//     statements.
//
// The rule that shapes the hot path: **a My work read never makes an HTTP
// call.** Lookup answers from the cache only, says plainly which facts are
// unknown, and starts a background refresh bounded by
// DefaultGitHubFactsRefreshTimeout. A miss is not an error and not a 404 — it
// is "unknown", which is a state a card can render (#3153's gotcha).
//
// "Unknown" is not one flag. Each fact carries its own judgement, because the
// merge-alert policy (#3133) leans on the difference: only a *complete* rules
// read may be read as "nothing applies", and a stale default branch may never
// be compared to a PR's target and found different, which would silently
// classify a merge as internal agent organization. Both are represented
// positively below (Complete, Trusted) rather than as an absence a caller could
// mistake for a negative answer.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/simstech/amp-api/internal/githubfacts"
	"github.com/simstech/amp-api/internal/repository"
)

// Defaults for the coordinator's policy. They mirror the TTLs recorded in
// decisions/repo-default-branch.md §2 so the research and the code cannot drift
// apart silently.
const (
	// DefaultGitHubFactsRepoTTL is how long a repository's default branch and
	// the rules of a target branch count as evidence.
	DefaultGitHubFactsRepoTTL = 15 * time.Minute
	// DefaultGitHubFactsPullTTL is how long a pull request's own facts (its
	// actual target, its requested reviewers, whether it merged) count.
	DefaultGitHubFactsPullTTL = 60 * time.Second
	// DefaultGitHubFactsNegativeTTL is how long a failed refresh keeps its
	// (project, repository, target) from being retried.
	DefaultGitHubFactsNegativeTTL = 60 * time.Second
	// DefaultGitHubFactsRefreshTimeout bounds one refresh end to end. The
	// reader bounds each request and each Facts call too; this is the outer
	// bound, so a background refresh cannot outlive the policy that spawned it.
	DefaultGitHubFactsRefreshTimeout = 20 * time.Second
	// DefaultGitHubFactsMaxRefresh bounds how many background refreshes may run
	// at once across every project. A page of merge items must not open one
	// GitHub call per item; a refresh beyond the bound is deferred (and the
	// next hot read tries again) rather than queued.
	DefaultGitHubFactsMaxRefresh = 8
)

// ErrGitHubFactsRefreshBound is returned to a synchronous caller (Refresh) whose
// refresh was deferred by the concurrency bound: nothing was read, and the next
// call will try again. A hot read (Lookup) never waits, so it reports Deferred
// instead of returning this.
var ErrGitHubFactsRefreshBound = errors.New("github facts: too many refreshes are already running; this one was deferred")

// githubFactsSlug mirrors repository's own check, so a malformed slug is refused
// before it reaches the store or a URL. It is deliberately duplicated here: the
// store's error is the backstop, but a hot read must be able to say "that isn't
// a repository name" without a database round trip. Like the store's, the error
// never echoes the value — a pasted URL can carry a token in its userinfo.
var githubFactsSlug = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// GitHubFactStore is the slice of the cache this coordinator uses. It is an
// interface so the tests can drive the policy with an in-memory cache and no
// Postgres; *repository.Repo is the real implementation.
type GitHubFactStore interface {
	GetGitHubRepoFacts(ctx context.Context, key repository.GitHubFactsKey) (*repository.GitHubRepoFacts, error)
	UpsertGitHubRepoFacts(ctx context.Context, key repository.GitHubFactsKey, w repository.GitHubRepoFactsWrite) (*repository.GitHubRepoFacts, error)
	GetGitHubBranchFacts(ctx context.Context, key repository.GitHubFactsKey, branch string) (*repository.GitHubBranchFacts, error)
	UpsertGitHubBranchFacts(ctx context.Context, key repository.GitHubFactsKey, branch string, w repository.GitHubBranchFactsWrite) (*repository.GitHubBranchFacts, error)
	GetGitHubPullFacts(ctx context.Context, key repository.GitHubFactsKey, number int) (*repository.GitHubPullFacts, error)
	UpsertGitHubPullFacts(ctx context.Context, key repository.GitHubFactsKey, number int, w repository.GitHubPullFactsWrite) (*repository.GitHubPullFacts, error)
	// InvalidateGitHubPullFacts drops one pull request's cached facts. It is the
	// webhook's write (AMP #3160): a delivery is new evidence, and the row it
	// contradicts must go rather than wait out the TTL.
	InvalidateGitHubPullFacts(ctx context.Context, key repository.GitHubFactsKey, number int) (bool, error)
}

// GitHubFactCredentials supplies the project's GitHub credential. Secret's
// contract is the reader's: an empty token with a nil error means "this project
// has no credential", which is a real state (a public repository, or one
// attached later) and not a failure — the reads still run and a private
// repository answers 404, which stays unknown.
type GitHubFactCredentials interface {
	// CredentialID names the credential a read used, so the cached facts record
	// which token produced the answer. ok is false when there is none.
	CredentialID(ctx context.Context, projectID int, host string) (id int, ok bool, err error)
	// Secret returns the token in plaintext. Nothing here logs it or returns it
	// to a caller.
	Secret(ctx context.Context, projectID int, host string) (token string, err error)
}

// GitHubFactReader reads one repository's facts. *githubfacts.Client is the
// real implementation; a test passes a fake so every arm of the policy is
// exercised without a socket.
type GitHubFactReader interface {
	Facts(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error)
}

// GitHubFactReaderFactory builds a reader bound to one refresh's credential.
// The reader takes its token source as an option, and the token differs per
// project, so a fresh reader per refresh is how a project's credential reaches
// the client. The production value is
//
//	func(tok githubfacts.TokenSource) GitHubFactReader {
//		return githubfacts.New(githubfacts.Options{Token: tok})
//	}
type GitHubFactReaderFactory func(token githubfacts.TokenSource) GitHubFactReader

// GitHubFactsConfig configures a coordinator. Store, Credentials and Readers are
// the injections; every duration is optional and falls back to the default
// above.
type GitHubFactsConfig struct {
	Store       GitHubFactStore
	Credentials GitHubFactCredentials
	Readers     GitHubFactReaderFactory

	// Now is the clock, for deterministic tests; time.Now when nil.
	Now func() time.Time
	// RepoTTL, PullTTL and NegativeTTL are the three windows described above.
	RepoTTL     time.Duration
	PullTTL     time.Duration
	NegativeTTL time.Duration
	// RefreshTimeout bounds one refresh end to end.
	RefreshTimeout time.Duration
	// MaxConcurrentRefresh bounds the background refreshes in flight.
	MaxConcurrentRefresh int
	// Logger receives refresh failures. nil means slog.Default().
	Logger *slog.Logger
}

// GitHubFactsRequest names the facts wanted: one repository of one project, and
// optionally one pull request and the branch it is believed to target.
//
// Target is a HINT (a run's reported base_branch). The pull request's own base,
// once read, is authoritative and replaces it — GitHub's answer for what a PR
// targets is the fact, and the run's report is what a person typed or the
// harness guessed.
type GitHubFactsRequest struct {
	ProjectID int
	Host      string // "" means github.com
	Slug      string // owner/repo, never a URL
	PR        int    // 0 reads no pull request
	Target    string // the target branch hint; "" means none
}

// factsKey normalises the request into the cache key, refusing a key that cannot
// name a row.
func (r GitHubFactsRequest) factsKey() (repository.GitHubFactsKey, error) {
	if r.ProjectID <= 0 {
		return repository.GitHubFactsKey{}, fmt.Errorf("github facts: a project id is required")
	}
	slug := strings.TrimSpace(r.Slug)
	if !githubFactsSlug.MatchString(slug) || len(slug) > 200 {
		return repository.GitHubFactsKey{}, fmt.Errorf("github facts: the repository must be named owner/repo, not a URL or a path")
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		host = repository.GitHubFactsHost
	}
	if r.PR < 0 {
		return repository.GitHubFactsKey{}, fmt.Errorf("github facts: a pull request number cannot be negative")
	}
	return repository.GitHubFactsKey{ProjectID: r.ProjectID, Host: host, Slug: slug}, nil
}

// GitHubFactsView is what a hot read may use: cached facts, each with an explicit
// judgement of how far it can be trusted, and what the read did about refreshing
// them.
//
// Every *View zero value means "nothing is known": the embedded Facts pointer is
// nil, Trusted is false and Unknown says why. That is the state a card renders
// as the honest-unknown chip, and it is never an error.
type GitHubFactsView struct {
	ProjectID int
	Host      string
	Slug      string
	PR        int

	// Target is the branch the facts below belong to, and TargetSource says
	// where it came from: githubfacts.SourcePull when a pull request read
	// confirmed it, githubfacts.SourceHint when it is only the caller's hint,
	// "" when it is not known at all.
	Target       string
	TargetSource githubfacts.Source

	Repo   GitHubRepoFactsView
	Branch GitHubBranchFactsView
	Pull   GitHubPullFactsView

	// Refresh says what the hot read did about refreshing these facts. It never
	// describes an HTTP call this read made, because it made none.
	Refresh GitHubFactsRefresh
}

// GitHubRepoFactsView is one repository's cached default branch, and how far it
// is evidence.
type GitHubRepoFactsView struct {
	// Facts is the cached row, nil when nothing was ever read. It is for
	// display (the repository chip, the last failure class) and is not itself
	// evidence: Trusted is.
	Facts *repository.GitHubRepoFacts

	DefaultBranch string
	Source        repository.GitHubDefaultBranchSource
	// ReadAt is when the default branch was last VERIFIED — the zero time when
	// it never was. It is the row's FetchedAt, not "now": a failed refresh
	// leaves it alone so staleness shows.
	ReadAt time.Time

	// Fresh says the read happened inside the TTL.
	Fresh bool
	// Trusted says this is the repository's default branch as far as the policy
	// is concerned: it came from a read that succeeded and it is still fresh.
	// Only Trusted facts may be compared to a PR's target.
	Trusted bool
	// Unknown is why the default branch is not evidence, in terms a person can
	// read; "" when Trusted.
	Unknown string
}

// GitHubBranchFactsView is the cached rules of one branch, and how far they are
// evidence.
type GitHubBranchFactsView struct {
	Facts *repository.GitHubBranchFacts

	// Rules is the reduced rule array 044 stores. It is meaningful as an ANSWER
	// only when Complete: an empty list read by a member token, with classic
	// branch protection unreadable, is partial and proves nothing about the
	// absence of a rule.
	Rules        []byte
	Completeness repository.GitHubCompleteness
	ReadAt       time.Time

	Fresh   bool
	Trusted bool
	// Complete says every read this needed answered, so an empty rule list may
	// be read as "no rule applies". It is the only completeness that may quiet a
	// merge as internal agent organization.
	Complete bool
	// Partial says the rules answered but cannot prove the ABSENCE of a rule
	// that needs a person — a positive signal only, never "no rule".
	Partial bool
	// Unknown is why the rules are not evidence; "" when Trusted.
	Unknown string
}

// GitHubPullFactsView is one pull request's cached facts, and how far they are
// evidence.
type GitHubPullFactsView struct {
	Facts *repository.GitHubPullFacts

	BaseBranch         string
	RequestedReviewers []string
	RequestedTeams     []string
	// State is the closed vocabulary 044 accepts. "merged" is the only state the
	// reader can establish positively; "" and "unknown" both mean it did not.
	State  string
	ReadAt time.Time

	Fresh   bool
	Trusted bool
	// Unknown is why the pull request's facts are not evidence; "" when Trusted.
	Unknown string
}

// GitHubFactsRefresh says what a hot read did about refreshing. A hot read never
// waits for one.
type GitHubFactsRefresh struct {
	// Started: this call began a background refresh.
	Started bool
	// Joined: an identical refresh was already running, so this call did not
	// start a second one. This is the singleflight collapsing.
	Joined bool
	// Deferred: a refresh was wanted, but the concurrency bound was full. The
	// next hot read tries again.
	Deferred bool
	// Skipped is why no refresh was started: the cached facts are fresh, or the
	// last attempt failed inside the negative-cache window. "" when one was
	// started or joined.
	Skipped string
	// Failure is the class of the failure that negative-cached this scope, so a
	// caller can log or show it without re-reading the row. "" when the skip was
	// not the negative cache.
	Failure string
}

// GitHubFactsSkippedFresh is Refresh.Skipped when everything the request needed
// is cached and inside its TTL.
const GitHubFactsSkippedFresh = "the cached facts are fresh"

// negativeSkipReason is Refresh.Skipped when the negative cache is holding a
// scope quiet, naming the failure and how long it has left. Both are safe to
// show: a failure class is a closed-set name, never a message or a token.
func negativeSkipReason(class string, wait time.Duration) string {
	return fmt.Sprintf("the last refresh failed (%s); not retrying for %s", class, wait.Round(time.Second))
}

// GitHubFactsCoordinator reads repository facts for the merge-alert policy,
// between a reader that must not cache and a cache that must not call GitHub.
//
// It is safe for concurrent use. One coordinator serves every project: the keys
// carry the project, so nothing is shared between them by accident.
type GitHubFactsCoordinator struct {
	store   GitHubFactStore
	creds   GitHubFactCredentials
	readers GitHubFactReaderFactory
	now     func() time.Time
	log     *slog.Logger

	repoTTL     time.Duration
	pullTTL     time.Duration
	negativeTTL time.Duration
	timeout     time.Duration

	// slots bounds the background refreshes in flight; nil when unbounded.
	slots chan struct{}
	// flights is the singleflight, and also the "is anything in flight" signal
	// WaitForIdle uses.
	flights factsFlights
	// negative keeps a failed scope quiet for NegativeTTL.
	negative githubFactsNegativeCache
}

// NewGitHubFactsCoordinator returns a coordinator. Store, Credentials and Readers
// are required: a coordinator without one of them cannot do its job, and failing
// here is better than a nil dereference inside a hot read.
func NewGitHubFactsCoordinator(cfg GitHubFactsConfig) (*GitHubFactsCoordinator, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("github facts coordinator: a cache is required")
	case cfg.Credentials == nil:
		return nil, errors.New("github facts coordinator: a credential source is required")
	case cfg.Readers == nil:
		return nil, errors.New("github facts coordinator: a reader factory is required")
	}
	c := &GitHubFactsCoordinator{
		store:       cfg.Store,
		creds:       cfg.Credentials,
		readers:     cfg.Readers,
		now:         cfg.Now,
		log:         cfg.Logger,
		repoTTL:     cfg.RepoTTL,
		pullTTL:     cfg.PullTTL,
		negativeTTL: cfg.NegativeTTL,
		timeout:     cfg.RefreshTimeout,
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	if c.repoTTL <= 0 {
		c.repoTTL = DefaultGitHubFactsRepoTTL
	}
	if c.pullTTL <= 0 {
		c.pullTTL = DefaultGitHubFactsPullTTL
	}
	if c.negativeTTL <= 0 {
		c.negativeTTL = DefaultGitHubFactsNegativeTTL
	}
	if c.timeout <= 0 {
		c.timeout = DefaultGitHubFactsRefreshTimeout
	}
	max := cfg.MaxConcurrentRefresh
	if max == 0 {
		max = DefaultGitHubFactsMaxRefresh
	}
	if max > 0 {
		c.slots = make(chan struct{}, max)
	}
	return c, nil
}

// Lookup answers a hot read from the cache, and starts a background refresh when
// the cached facts are missing or stale. It makes no HTTP call and does not wait
// for one: the returned view says which facts are unknown, and the refresh
// (if any) fills the cache for the next read.
//
// A miss is not an error — it is the unknown state. An error means the request
// itself is unusable (a project id or a repository name that cannot name a row)
// or the cache could not be read at all; a cache *outage* is not a fact about the
// repository, so it is returned rather than dressed up as "unknown".
func (c *GitHubFactsCoordinator) Lookup(ctx context.Context, req GitHubFactsRequest) (GitHubFactsView, error) {
	key, err := req.factsKey()
	if err != nil {
		return GitHubFactsView{}, err
	}
	now := c.now().UTC()
	view := GitHubFactsView{ProjectID: key.ProjectID, Host: key.Host, Slug: key.Slug, PR: req.PR}

	repoFacts, err := c.store.GetGitHubRepoFacts(ctx, key)
	if err != nil && !errors.Is(err, repository.ErrNoGitHubRepoFacts) {
		return view, fmt.Errorf("github facts: reading the cached facts for %s: %w", key.Slug, err)
	}
	view.Repo = c.repoView(repoFacts, now)

	var pullFacts *repository.GitHubPullFacts
	if req.PR > 0 {
		var perr error
		pullFacts, perr = c.store.GetGitHubPullFacts(ctx, key, req.PR)
		if perr != nil && !errors.Is(perr, repository.ErrNoGitHubPullFacts) {
			return view, fmt.Errorf("github facts: reading the cached facts for %s#%d: %w", key.Slug, req.PR, perr)
		}
		view.Pull = c.pullView(pullFacts, now)
	}

	view.Target, view.TargetSource = resolveGitHubFactsTarget(req.Target, pullFacts)

	var branchFacts *repository.GitHubBranchFacts
	if view.Target == "" {
		// No target, so no branch was read and there is nothing to look up. The
		// reason is stated here rather than left as a zero value, because a
		// caller must not have to guess whether "no rules" means "no rules" or
		// "nobody asked".
		view.Branch.Unknown = "the branch the change targets is unknown, so its rules were not read"
	} else {
		var berr error
		branchFacts, berr = c.store.GetGitHubBranchFacts(ctx, key, view.Target)
		if berr != nil && !errors.Is(berr, repository.ErrNoGitHubBranchFacts) {
			return view, fmt.Errorf("github facts: reading the cached rules for %s@%s: %w", key.Slug, view.Target, berr)
		}
		view.Branch = c.branchView(branchFacts, now)
	}

	view.Refresh = c.refreshIfNeeded(ctx, req, key, view, now)
	return view, nil
}

// refreshIfNeeded decides whether a refresh is warranted and starts one if so.
// It never blocks on network I/O.
func (c *GitHubFactsCoordinator) refreshIfNeeded(ctx context.Context, req GitHubFactsRequest, key repository.GitHubFactsKey, view GitHubFactsView, now time.Time) GitHubFactsRefresh {
	needRepo := !view.Repo.Trusted
	// A target that is not known cannot be refreshed: there is no branch to read
	// rules for, and inventing one (the default branch, "main") is exactly what
	// the policy forbids. The view already says the rules are unknown.
	needBranch := view.Target != "" && !view.Branch.Trusted
	needPull := req.PR > 0 && !view.Pull.Trusted
	if !needRepo && !needBranch && !needPull {
		return GitHubFactsRefresh{Skipped: GitHubFactsSkippedFresh}
	}

	// The negative cache: a scope whose last attempt failed inside the window is
	// not hammered. If everything still needed is covered by it, the hot read
	// starts nothing — and the per-fact Unknown strings in the view already say
	// why those facts are missing, so the card keeps explaining itself.
	if remaining, class := c.negative.remaining(c.flightKey(key, view.Target, req.PR), now, c.negativeTTL); remaining > 0 {
		return GitHubFactsRefresh{Skipped: negativeSkipReason(class, remaining), Failure: class}
	}

	joined, started, deferred := c.startRefresh(ctx, req, view.Target)
	return GitHubFactsRefresh{Started: started, Joined: joined, Deferred: deferred}
}

// flightKey is the scope of one refresh, shared by the singleflight and the
// negative cache so "who is already asking for this" and "what recently failed"
// can never disagree.
func (c *GitHubFactsCoordinator) flightKey(key repository.GitHubFactsKey, target string, pr int) factsFlightKey {
	return factsFlightKey{ProjectID: key.ProjectID, Host: key.Host, Slug: key.Slug, Target: strings.TrimSpace(target), PR: pr}
}

// Refresh performs one refresh and waits for it, sharing a refresh already in
// flight for the same facts. It is for a handler or a webhook that wants the
// cache filled before it answers — never for a hot read, which must not wait.
//
// joined says the result came from a refresh another caller started (the
// singleflight collapsed the two). An error means nothing was read or the cache
// could not be written; the returned Facts may still be set when only the write
// failed, so a caller can use what was read.
func (c *GitHubFactsCoordinator) Refresh(ctx context.Context, req GitHubFactsRequest) (facts *githubfacts.Facts, joined bool, err error) {
	key, err := req.factsKey()
	if err != nil {
		return nil, false, err
	}
	flightKey := c.flightKey(key, req.Target, req.PR)

	f, mine := c.flights.begin(flightKey)
	if !mine {
		select {
		case <-f.done:
			return f.facts, true, f.err
		case <-ctx.Done():
			return nil, true, ctx.Err()
		}
	}
	defer c.flights.finish(flightKey, f)
	f.facts, f.err = c.refresh(ctx, req)
	c.recordOutcome(flightKey, f.facts, f.err)
	return f.facts, false, f.err
}

// InvalidatePull drops the cached facts for one pull request, so the next read
// re-reads them from GitHub instead of trusting a row a webhook delivery has just
// contradicted. It reports whether there was a row; a miss is not an error, and
// neither is a project that has never cached this repository.
//
// This is the webhook's entry point (AMP #3160), and it is deliberately narrow.
// It does NOT drop the repository or branch rows: a pull-request event says
// nothing about the default branch or a branch's rules, and dropping them would
// throw away good evidence and make the 15m TTL meaningless. A default-branch
// rename is a `repository` event, which this hook is not subscribed to, so that
// stays the TTL's job — the correctness backstop when no delivery arrives.
//
// The scope's negative-cache entry is forgotten too. Without that, a delivery
// arriving inside a 60s negative window would drop the row and then have the next
// read refuse to refill it: the invalidation would be invisible for exactly as
// long as the failure it was meant to clear.
func (c *GitHubFactsCoordinator) InvalidatePull(ctx context.Context, req GitHubFactsRequest) (bool, error) {
	key, err := req.factsKey()
	if err != nil {
		return false, err
	}
	if req.PR <= 0 {
		return false, fmt.Errorf("github facts: a pull request number is required to invalidate its facts")
	}
	had, err := c.store.InvalidateGitHubPullFacts(ctx, key, req.PR)
	if err != nil {
		return false, err
	}
	c.negative.forgetPull(key, req.PR)
	return had, nil
}

// recordOutcome feeds one attempt to the negative cache: a failed attempt silences
// its scope for NegativeTTL, a successful one forgets the previous failure.
func (c *GitHubFactsCoordinator) recordOutcome(key factsFlightKey, facts *githubfacts.Facts, err error) {
	if failed, class := refreshOutcome(facts, err); failed {
		c.negative.fail(key, class, c.now().UTC(), c.negativeTTL)
		return
	}
	c.negative.succeed(key)
}

// WaitForIdle waits until no refresh is in flight, or ctx ends. Reflection-free
// and bounded: it is how a shutdown drains the background work a hot read
// started, and how a test knows a background refresh has landed.
func (c *GitHubFactsCoordinator) WaitForIdle(ctx context.Context) error {
	return c.flights.wait(ctx)
}

// InFlight reports how many refreshes are running. For operational visibility and
// tests; the policy never branches on it.
func (c *GitHubFactsCoordinator) InFlight() int { return c.flights.count() }

// startRefresh begins a background refresh, collapsing into an identical one
// already in flight and deferring when the concurrency bound is full.
func (c *GitHubFactsCoordinator) startRefresh(ctx context.Context, req GitHubFactsRequest, target string) (joined, started, deferred bool) {
	key, err := req.factsKey()
	if err != nil {
		return false, false, false // Lookup already refused it
	}
	flightKey := c.flightKey(key, target, req.PR)

	f, mine := c.flights.begin(flightKey)
	if !mine {
		return true, false, false
	}
	if c.slots != nil {
		select {
		case c.slots <- struct{}{}:
		default:
			// Bound reached. A joined synchronous caller gets a clear error
			// rather than a silent "someone else did it".
			f.err = ErrGitHubFactsRefreshBound
			c.flights.finish(flightKey, f)
			return false, false, true
		}
	}
	go func() {
		if c.slots != nil {
			defer func() { <-c.slots }()
		}
		defer c.flights.finish(flightKey, f)
		// The request that asked for this refresh is over by the time it runs: a
		// closed connection must not cancel a refresh the next read wants. The
		// refresh's own deadline (RefreshTimeout, set in refresh) is the bound.
		f.facts, f.err = c.refresh(context.WithoutCancel(ctx), req)
		c.recordOutcome(flightKey, f.facts, f.err)
	}()
	return false, true, false
}

// refresh reads the repository's facts and stores what they established,
// attaching the project's credential and bounding the whole call. Whether the
// attempt is remembered as a failure — and so kept quiet for NegativeTTL — is the
// caller's business, because that decision is about the refresh scope, not about
// one read.
func (c *GitHubFactsCoordinator) refresh(ctx context.Context, req GitHubFactsRequest) (*githubfacts.Facts, error) {
	key, err := req.factsKey()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// The credential follows the read: which token produced an answer belongs to
	// the answer (#3153). A failure to name it (as opposed to there being none)
	// is not fatal — the facts are stored as anonymous, which is the weaker claim
	// the classifier must treat as such, never a stronger one.
	var credentialID *int
	if id, ok, cerr := c.creds.CredentialID(ctx, key.ProjectID, key.Host); cerr != nil {
		c.log.Warn("github facts: couldn't identify the credential a refresh used", "project_id", key.ProjectID, "host", key.Host, "err", cerr)
	} else if ok {
		credentialID = &id
	}

	facts, err := c.readers(c.tokenSource(key)).Facts(ctx, githubfacts.Request{
		Slug:   key.Slug,
		PR:     req.PR,
		Target: strings.TrimSpace(req.Target),
	})
	if err != nil {
		// The request itself was unusable: nothing was read, so nothing is
		// written and no cached fact changes.
		return nil, err
	}
	if serr := c.storeFacts(ctx, key, req, facts, credentialID); serr != nil {
		c.log.Warn("github facts: caching a refresh failed", "project_id", key.ProjectID, "slug", key.Slug, "err", serr)
		return facts, serr
	}
	for _, r := range facts.Reads {
		if !r.OK {
			c.log.Debug("github facts: a read stayed unknown", "project_id", key.ProjectID, "slug", key.Slug, "op", r.Op,
				"class", failureClass(facts, r.Op))
			break
		}
	}
	return facts, nil
}

// refreshOutcome is what one attempt did, for the negative cache: nothing at all
// succeeded, or it did. A failed *read* with the request itself fine is not the
// same as a failed attempt — the reader answers a partial read with a Facts and no
// error, and only the second is worth silencing.
func refreshOutcome(facts *githubfacts.Facts, err error) (failed bool, class string) {
	if err != nil {
		var gf *githubfacts.Error
		if errors.As(err, &gf) && gf.Kind != "" {
			return true, string(gf.Kind)
		}
		return true, repository.GitHubFactInvalid
	}
	if facts == nil {
		return true, repository.GitHubFactInvalid
	}
	// The whole attempt failed when no read answered at all (a token the store
	// could not produce, a credential the repo store cannot open). A read that
	// answered and one that did not is partial knowledge, not a failed attempt: it
	// is already stored, and it is exactly the case the classifier must see.
	if facts.Completeness != githubfacts.Unknown {
		return false, ""
	}
	for _, op := range []githubfacts.Op{githubfacts.OpRepo, githubfacts.OpRules, githubfacts.OpPull, githubfacts.OpProtection} {
		if r, ok := facts.ReadFor(op); ok && !r.OK {
			return true, failureClass(facts, op)
		}
	}
	return true, repository.GitHubFactInvalid
}

// tokenSource binds the project's credential to the reader's contract: an empty
// token with a nil error means "no credential", and any other error is the
// reader's KindNoCredential.
func (c *GitHubFactsCoordinator) tokenSource(key repository.GitHubFactsKey) githubfacts.TokenSource {
	return func(ctx context.Context) (string, error) {
		token, err := c.creds.Secret(ctx, key.ProjectID, key.Host)
		if err != nil {
			// Never wrap this error with the value: the credential store's error
			// may quote the ciphertext or the key, and this string reaches logs.
			return "", err
		}
		return token, nil
	}
}

// storeFacts writes what one read established. Each row is written from the read
// that produced it, so a failure is recorded as a failure on exactly the fact it
// left unknown:
//
//   - the repository row from the repo read (the default branch),
//   - the branch row from the rules and protection reads,
//   - the pull row from the pull read.
//
// Completeness is never optimistic: the repository row is complete only when the
// default branch and the rules both answered, and the branch row only when the
// rules AND classic protection both answered. A rules list read by a token that
// could not see protection is partial, and partial never means "no rule".
//
// The verification time is the COORDINATOR's clock, not the reader's ReadAt. They
// are the same clock in production, and they must be the same clock for the TTL to
// mean anything: the freshness check compares this stamp against c.now(), so a
// stamp from another clock could land in the future and leave the facts
// permanently untrusted, or in the past and make them expire at once. The moment
// that matters to the policy is when the coordinator recorded the answer.
func (c *GitHubFactsCoordinator) storeFacts(ctx context.Context, key repository.GitHubFactsKey, req GitHubFactsRequest, facts *githubfacts.Facts, credentialID *int) error {
	verifiedAt := c.now().UTC()

	repoWrite := repository.GitHubRepoFactsWrite{CredentialID: credentialID}
	if read, ok := facts.ReadFor(githubfacts.OpRepo); ok && read.OK {
		repoWrite.VerifiedAt = &verifiedAt
		repoWrite.Completeness = repoFactsCompleteness(facts)
		repoWrite.DefaultBranch = facts.DefaultBranch
		if facts.DefaultBranch != "" {
			// REST `.default_branch`: the server-side source the research
			// recommends (§1a). origin/HEAD is a harness-side hint, not this.
			repoWrite.DefaultBranchSource = repository.GitHubBranchSourceREST
		}
	} else {
		repoWrite.FailureClass = failureClass(facts, githubfacts.OpRepo)
	}
	if _, err := c.store.UpsertGitHubRepoFacts(ctx, key, repoWrite); err != nil {
		return err
	}

	if facts.Target != "" {
		branchWrite := repository.GitHubBranchFactsWrite{}
		if read, ok := facts.ReadFor(githubfacts.OpRules); ok && read.OK {
			rules, err := githubFactRulesJSON(facts.Rules.Rules)
			if err != nil {
				return err
			}
			branchWrite.Rules = rules
			branchWrite.Completeness = branchFactsCompleteness(facts)
			branchWrite.VerifiedAt = &verifiedAt
		} else {
			branchWrite.FailureClass = failureClass(facts, githubfacts.OpRules)
		}
		if _, err := c.store.UpsertGitHubBranchFacts(ctx, key, facts.Target, branchWrite); err != nil {
			return err
		}
	}

	if req.PR > 0 {
		if read, ok := facts.ReadFor(githubfacts.OpPull); ok {
			pullWrite := repository.GitHubPullFactsWrite{}
			if read.OK {
				pullWrite.BaseBranch = facts.Pull.Base
				pullWrite.State = pullFactsState(facts.Pull)
				pullWrite.RequestedReviewers = facts.Pull.RequestedReviewers
				pullWrite.RequestedTeams = facts.Pull.RequestedTeams
				// Complete: this row is about what the pull read established, and
				// that read answered in full. A failed rules read says nothing about
				// the pull request's own facts.
				pullWrite.Completeness = repository.GitHubCompletenessComplete
				pullWrite.VerifiedAt = &verifiedAt
			} else {
				pullWrite.FailureClass = failureClass(facts, githubfacts.OpPull)
			}
			if _, err := c.store.UpsertGitHubPullFacts(ctx, key, req.PR, pullWrite); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- the views ----

func (c *GitHubFactsCoordinator) repoView(f *repository.GitHubRepoFacts, now time.Time) GitHubRepoFactsView {
	v := GitHubRepoFactsView{Facts: f}
	if f == nil {
		v.Unknown = "the repository's default branch has not been read"
		return v
	}
	v.DefaultBranch, v.Source = f.DefaultBranch, f.DefaultBranchSource
	if f.FetchedAt != nil {
		v.ReadAt = f.FetchedAt.UTC()
	}
	v.Fresh = freshFor(f.FetchedAt, now, c.repoTTL)
	// A branch with no source cannot have come from a read that named it
	// (repository refuses that combination), so an empty branch is a miss even
	// when the row is dated.
	v.Trusted = f.Verified() && v.Fresh && f.DefaultBranch != ""
	v.Unknown = unknownReason("the repository's default branch", f.Verified(), v.Fresh, f.FetchedAt, f.LastErrorClass, now, c.repoTTL)
	return v
}

func (c *GitHubFactsCoordinator) branchView(f *repository.GitHubBranchFacts, now time.Time) GitHubBranchFactsView {
	v := GitHubBranchFactsView{Facts: f}
	if f == nil {
		v.Unknown = "the rules of the target branch have not been read"
		return v
	}
	v.Rules, v.Completeness = f.Rules, f.Completeness
	if f.FetchedAt != nil {
		v.ReadAt = f.FetchedAt.UTC()
	}
	v.Fresh = freshFor(f.FetchedAt, now, c.repoTTL)
	v.Trusted = f.Verified() && v.Fresh
	v.Partial = v.Trusted && f.Completeness == repository.GitHubCompletenessPartial
	v.Complete = v.Trusted && f.Completeness == repository.GitHubCompletenessComplete
	v.Unknown = unknownReason("the rules of the target branch", f.Verified(), v.Fresh, f.FetchedAt, f.LastErrorClass, now, c.repoTTL)
	return v
}

func (c *GitHubFactsCoordinator) pullView(f *repository.GitHubPullFacts, now time.Time) GitHubPullFactsView {
	v := GitHubPullFactsView{Facts: f}
	if f == nil {
		v.Unknown = "the pull request's facts have not been read"
		return v
	}
	v.BaseBranch, v.State = f.BaseBranch, f.State
	v.RequestedReviewers, v.RequestedTeams = f.RequestedReviewers, f.RequestedTeams
	if f.FetchedAt != nil {
		v.ReadAt = f.FetchedAt.UTC()
	}
	v.Fresh = freshFor(f.FetchedAt, now, c.pullTTL)
	v.Trusted = f.Verified() && v.Fresh
	v.Unknown = unknownReason("the pull request's facts", f.Verified(), v.Fresh, f.FetchedAt, f.LastErrorClass, now, c.pullTTL)
	return v
}

// TargetIsDefault says whether the pull request targets the repository's default
// branch — clause (a) of the merge-alert policy.
//
// nil means it CANNOT BE DECIDED: the default branch is not trusted (never read,
// stale, or a failed refresh), the target is not known, or the target is only the
// caller's hint rather than the PR's own base. That nil is the point: a stale
// default branch must never be compared to a target and found different, which
// would classify the merge as internal agent organization and quietly stop
// asking a person about a real merge. Absence of evidence is never a negative
// answer.
func (v GitHubFactsView) TargetIsDefault() *bool {
	if !v.Repo.Trusted || v.Target == "" || v.TargetSource != githubfacts.SourcePull {
		return nil
	}
	same := v.Target == v.Repo.DefaultBranch
	return &same
}

// AnyUnknown reports whether any fact the view carries is unknown, for a caller
// that wants one flag. The per-fact Unknown strings say which.
func (v GitHubFactsView) AnyUnknown() bool {
	return v.Repo.Unknown != "" || v.Branch.Unknown != "" || v.Pull.Unknown != ""
}

// resolveGitHubFactsTarget picks the branch the facts belong to. GitHub's answer
// for what the PR targets (the pull read's base) overrides the caller's hint, and
// a hint is never presented as GitHub's answer: TargetSource stays SourceHint, so
// TargetIsDefault refuses to decide on it.
//
// The base of a *verified* pull read is used even when the pull facts are no
// longer fresh: freshness governs whether the facts are refreshed, not what the
// PR's target was, and a retargeted PR is a webhook's job to invalidate.
func resolveGitHubFactsTarget(hint string, pull *repository.GitHubPullFacts) (string, githubfacts.Source) {
	if pull != nil && pull.Verified() && pull.BaseBranch != "" {
		return pull.BaseBranch, githubfacts.SourcePull
	}
	if hint = strings.TrimSpace(hint); hint != "" {
		return hint, githubfacts.SourceHint
	}
	return "", ""
}

// freshFor says a read that happened at *at is inside ttl. A read stamped in the
// future cannot be judged and is not fresh: an unknown clock is not evidence.
func freshFor(at *time.Time, now time.Time, ttl time.Duration) bool {
	if at == nil {
		return false
	}
	age := now.Sub(at.UTC())
	return age >= 0 && age < ttl
}

// unknownReason words why a fact is not evidence. Empty when it is.
func unknownReason(what string, verified, fresh bool, at *time.Time, class string, now time.Time, ttl time.Duration) string {
	switch {
	case at == nil && class != "":
		return fmt.Sprintf("%s could not be read (%s)", what, class)
	case at == nil:
		return what + " has not been read"
	case !verified:
		return fmt.Sprintf("%s was never verified", what)
	case !fresh:
		why := fmt.Sprintf("%s was read %s ago, older than the %s TTL", what, now.Sub(at.UTC()).Round(time.Second), ttl)
		if class != "" {
			why += fmt.Sprintf(", and the latest refresh failed (%s)", class)
		}
		return why
	}
	return ""
}

// githubFactsNegativeCache remembers, per refresh scope, when the last attempt
// failed. It is the negative cache's whole state: a scope whose failure is younger
// than the TTL is not retried, so a repository no token can read does not turn
// every My work load into another round of doomed GitHub calls
// (decisions/repo-default-branch.md §2).
//
// It is keyed by the refresh scope rather than by a cached row on purpose. A row
// remembers the last failure of the fact it holds, which is not the same thing: a
// rules read that failed leaves the *repository* row's failure untouched, so a row
// keyed cache would keep retrying it, and one that succeeded long ago would keep
// suppressing it. What the policy bounds is attempts, so attempts are what it
// counts.
//
// The entries are in memory and per process: a restart forgets a failure and tries
// once more, which is the safe direction (one extra call, no stale suppression).
// Write it down only if a deployment ever shows the retry rate mattering.
type githubFactsNegativeCache struct {
	mu      sync.Mutex
	until   map[factsFlightKey]time.Time
	failure map[factsFlightKey]string
}

// remaining reports how long a scope has left in its negative window, and the
// failure class that put it there. remaining <= 0 means the scope may be tried.
// It also forgets entries that have expired, so the map cannot grow without bound.
func (n *githubFactsNegativeCache) remaining(key factsFlightKey, now time.Time, ttl time.Duration) (time.Duration, string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	until, ok := n.until[key]
	if !ok {
		return 0, ""
	}
	if !now.Before(until) {
		delete(n.until, key)
		delete(n.failure, key)
		return 0, ""
	}
	return until.Sub(now), n.failure[key]
}

// fail records that a refresh of key failed now, silencing it for ttl.
func (n *githubFactsNegativeCache) fail(key factsFlightKey, class string, now time.Time, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.until == nil {
		n.until = make(map[factsFlightKey]time.Time)
		n.failure = make(map[factsFlightKey]string)
	}
	n.until[key] = now.Add(ttl)
	n.failure[key] = class
}

// forgetPull drops every negative-cache entry for one pull request, whatever
// target it was recorded under. A webhook delivery is new evidence about that
// pull request, so a failure recorded before it must not keep the scope quiet:
// the delivery exists precisely because something changed.
func (n *githubFactsNegativeCache) forgetPull(key repository.GitHubFactsKey, pr int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for k := range n.until {
		if k.ProjectID == key.ProjectID && k.Host == key.Host && strings.EqualFold(k.Slug, key.Slug) && k.PR == pr {
			delete(n.until, k)
			delete(n.failure, k)
		}
	}
}

// succeed clears a scope's negative window: the next failure is a new fact.
func (n *githubFactsNegativeCache) succeed(key factsFlightKey) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.until, key)
	delete(n.failure, key)
}

// ---- the singleflight ----

// factsFlightKey scopes one refresh. Project and repository keep two projects'
// identical slugs apart (the cross-project rule #3153 enforces in SQL); Target
// keeps two branches of one repository apart (their rules differ); PR keeps two
// pull requests apart, because a refresh reads exactly one PR's facts and
// collapsing two would strand the second one's cache forever.
type factsFlightKey struct {
	ProjectID int
	Host      string
	Slug      string
	Target    string
	PR        int
}

// factsFlight is one in-flight refresh. done is closed after facts and err are
// written, so a waiter that receives from done sees both.
type factsFlight struct {
	done  chan struct{}
	facts *githubfacts.Facts
	err   error
}

// factsFlights collapses concurrent refreshes of the same key, and tracks whether
// any is in flight so a shutdown (or a test) can wait for them.
type factsFlights struct {
	mu    sync.Mutex
	calls map[factsFlightKey]*factsFlight
	// idle is closed whenever calls is empty, and is a fresh open channel as
	// soon as one refresh begins, so waiters can select on it without polling.
	idle chan struct{}
}

// begin returns the flight for key, and whether this caller owns it (and must run
// and finish it). When it does not, the caller should wait on the flight's done.
func (g *factsFlights) begin(key factsFlightKey) (*factsFlight, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f, ok := g.calls[key]; ok {
		return f, false
	}
	if g.calls == nil {
		g.calls = make(map[factsFlightKey]*factsFlight)
	}
	if len(g.calls) == 0 {
		g.idle = make(chan struct{})
	}
	f := &factsFlight{done: make(chan struct{})}
	g.calls[key] = f
	return f, true
}

// finish removes the flight and publishes it to waiters, and marks the group idle
// when it was the last one. It must be called exactly once by the begin that
// owned the flight, after facts and err are set, so a waiter receiving from done
// sees both.
func (g *factsFlights) finish(key factsFlightKey, f *factsFlight) {
	g.mu.Lock()
	if cur, ok := g.calls[key]; ok && cur == f {
		delete(g.calls, key)
	}
	if len(g.calls) == 0 && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
	g.mu.Unlock()
	close(f.done)
}

// wait blocks until nothing is in flight, or ctx ends.
func (g *factsFlights) wait(ctx context.Context) error {
	for {
		g.mu.Lock()
		if len(g.calls) == 0 {
			g.mu.Unlock()
			return nil
		}
		ch := g.idle
		g.mu.Unlock()
		if ch == nil {
			continue
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *factsFlights) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

// ---- mapping the reader's answer onto the cache ----

// repoFactsCompleteness: the repository row covers the default branch and the
// rules, so it is complete only when both answered. Protection counts too: a
// rules list read without it cannot prove the absence of a rule.
func repoFactsCompleteness(f *githubfacts.Facts) repository.GitHubCompleteness {
	if !f.ReadOK(githubfacts.OpRepo) {
		return repository.GitHubCompletenessUnknown
	}
	if f.Target == "" || !f.ReadOK(githubfacts.OpRules) || !f.ReadOK(githubfacts.OpProtection) {
		return repository.GitHubCompletenessPartial
	}
	return repository.GitHubCompletenessComplete
}

// branchFactsCompleteness: the branch row is complete only when the rules AND
// classic branch protection both answered. "Rules read, protection not readable
// with this token" is partial — a positive signal for a rule, never proof that
// none applies.
func branchFactsCompleteness(f *githubfacts.Facts) repository.GitHubCompleteness {
	if !f.ReadOK(githubfacts.OpRules) {
		return repository.GitHubCompletenessUnknown
	}
	if !f.ReadOK(githubfacts.OpProtection) {
		return repository.GitHubCompletenessPartial
	}
	return repository.GitHubCompletenessComplete
}

// failureClass names why a read stayed unknown, as one of the closed set 044 and
// the repository layer both check. A failed read the reader did not classify
// would be a reader bug; storing it as invalid keeps the write valid and the
// failure visible rather than silently dropping the negative cache.
func failureClass(f *githubfacts.Facts, op githubfacts.Op) string {
	read, ok := f.ReadFor(op)
	if !ok {
		return repository.GitHubFactInvalid
	}
	if read.Err != nil && read.Err.Kind != "" {
		return string(read.Err.Kind)
	}
	return repository.GitHubFactInvalid
}

// pullFactsState maps the reader's answer onto 044's closed vocabulary. Only
// "merged" is knowable from this read: REST's merged_at is null for an open PR
// *and* for a closed-unmerged one, and the read does not ask for `state`, so
// anything else stays unknown rather than being guessed as open.
func pullFactsState(p githubfacts.PullFacts) string {
	if p.Merged {
		return "merged"
	}
	return "unknown"
}

// githubFactRule is one rule as 044 stores it: the reduced array the migration
// documents (type, the pull_request parameters the classifier reads, the ruleset
// it came from). The classifier's own types own its interpretation; this is only
// the reduction.
type githubFactRule struct {
	Type              string                    `json:"type"`
	RulesetID         int64                     `json:"ruleset_id,omitempty"`
	RulesetSource     string                    `json:"ruleset_source,omitempty"`
	RulesetSourceType string                    `json:"ruleset_source_type,omitempty"`
	Parameters        *githubFactRuleParameters `json:"parameters,omitempty"`
}

type githubFactRuleParameters struct {
	RequiredApprovingReviewCount               int                  `json:"required_approving_review_count"`
	RequireCodeOwnerReview                     bool                 `json:"require_code_owner_review"`
	RequiredReviewers                          []githubFactReviewer `json:"required_reviewers"`
	RequireLastPushApproval                    bool                 `json:"require_last_push_approval"`
	RequireExtraApprovalForUnattributedChanges bool                 `json:"require_extra_approval_for_unattributed_changes"`
	RequireReviewThreadResolution              bool                 `json:"required_review_thread_resolution"`
}

type githubFactReviewer struct {
	Type  string `json:"type,omitempty"`
	ID    int64  `json:"id,omitempty"`
	Login string `json:"login,omitempty"`
	Slug  string `json:"slug,omitempty"`
}

// githubFactRulesJSON reduces the reader's rules to the array 044 CHECKs. An empty
// list becomes "[]" through the repository's own normalisation, so "the read
// happened and nothing applies" stays distinguishable from "never read".
func githubFactRulesJSON(rules []githubfacts.Rule) ([]byte, error) {
	out := make([]githubFactRule, 0, len(rules))
	for _, r := range rules {
		gr := githubFactRule{
			Type:              r.Type,
			RulesetID:         r.RulesetID,
			RulesetSource:     r.RulesetSource,
			RulesetSourceType: r.RulesetSourceType,
		}
		if r.Type == "pull_request" {
			p := r.Parameters
			gp := &githubFactRuleParameters{
				RequiredApprovingReviewCount:               p.RequiredApprovingReviewCount,
				RequireCodeOwnerReview:                     p.RequireCodeOwnerReview,
				RequireLastPushApproval:                    p.RequireLastPushApproval,
				RequireExtraApprovalForUnattributedChanges: p.RequireExtraApprovalForUnattributedChanges,
				RequireReviewThreadResolution:              p.RequireReviewThreadResolution,
				RequiredReviewers:                          make([]githubFactReviewer, 0, len(p.RequiredReviewers)),
			}
			for _, rv := range p.RequiredReviewers {
				gp.RequiredReviewers = append(gp.RequiredReviewers, githubFactReviewer{
					Type: rv.Type, ID: rv.ID, Login: rv.Login, Slug: rv.Slug,
				})
			}
			gr.Parameters = gp
		}
		out = append(out, gr)
	}
	return json.Marshal(out)
}

// ---- the production adapters ----

// RepoGitHubFactCredentials adapts the repository's sealed credential store to the
// coordinator's credential port. Nothing here returns or logs the token: Secret's
// caller (the reader's token source) puts it in a request header and nowhere else.
type RepoGitHubFactCredentials struct {
	Repo *repository.Repo
}

// CredentialID reports which credential a project has for host, without its
// token.
func (a RepoGitHubFactCredentials) CredentialID(ctx context.Context, projectID int, host string) (int, bool, error) {
	c, err := a.Repo.GetGitHubCredential(ctx, projectID, host)
	if errors.Is(err, repository.ErrNoGitHubCredential) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return c.ID, true, nil
}

// Secret returns the project's token, or "" with a nil error when there is none —
// the reader's own contract for "this read is anonymous".
func (a RepoGitHubFactCredentials) Secret(ctx context.Context, projectID int, host string) (string, error) {
	token, err := a.Repo.GitHubCredentialSecret(ctx, projectID, host)
	if errors.Is(err, repository.ErrNoGitHubCredential) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return token, nil
}

// GitHubClientReaderFactory returns the production reader factory: a
// githubfacts.Client bound to the credential the coordinator resolved.
func GitHubClientReaderFactory(opts githubfacts.Options) GitHubFactReaderFactory {
	return func(token githubfacts.TokenSource) GitHubFactReader {
		opts.Token = token
		return githubfacts.New(opts)
	}
}
