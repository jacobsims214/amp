package repository

// Repository-facts cache persistence (AMP #3153, epic 183 story 453).
//
// This file is the storage half of the merge-alert policy
// (decisions/repo-default-branch.md §2). Migration 044 (#3152) created four
// project-scoped tables — project_github_credentials, github_repo_facts,
// github_branch_facts, github_pull_facts — and this file is the only Go code
// that reads or writes the three facts tables. It contains no network calls,
// no classifier and no projection: the reader is internal/githubfacts (#3151),
// the refresh policy (TTLs, negative cache, singleflight) is the coordinator
// (#3156), and the decision about who gets told is the classifier (#3133).
//
// Three properties this layer exists to keep true:
//
//  1. NOTHING IS CROSS-PROJECT. Every statement filters by project_id AND host
//     AND lower(repo_slug); branch and PR facts reach their repository row
//     through a join on those same three columns rather than trusting a
//     repo_facts_id the caller supplies. A repository fact is a function of the
//     credential that read it — GitHub answers a private repo with a 404 to a
//     token that cannot see it — so the same owner/repo in two projects is two
//     independent rows and one project's token-derived visibility can never be
//     read through another. The schema enforces this too (the composite
//     credential foreign key, 044); these queries keep the reads honest.
//
//  2. A FAILURE NEVER DESTROYS THE LAST GOOD FACTS. "Not knowing" is a
//     representable state and staleness is visible, which is what the policy's
//     safety rule needs: positive knowledge may quiet a card, absence of
//     knowledge never may. So a failed refresh records its failure CLASS and
//     time and leaves fetched_at, completeness, default branch and credential
//     exactly as the last successful read left them. A reader can then tell
//     the three states apart without any extra column:
//
//       fetched_at NULL                      never fetched
//       fetched_at set, no last_error_class  verified at fetched_at
//       fetched_at set, last_error_class set verified at fetched_at, the
//                                            latest refresh failed since
//
//     A verified refresh that is OLDER than what is stored (an out-of-order
//     retry) is ignored rather than applied, so a slow duplicate cannot walk
//     the facts backwards. To correct a bad timestamp, invalidate and rewrite.
//
//  3. A PERSON'S MANUAL DEFAULT BRANCH WINS. default_branch_source 'manual' is
//     the project owner's override and outranks detection (#3130 §2, #3133).
//     A verified read of a row whose source is 'manual' updates the read's
//     completeness and time but leaves the branch and its provenance alone; a
//     manual declaration replaces whatever is stored. A detected read never
//     silently overwrites an override, and an override never claims to be a
//     detection.
//
// Failures are stored as a class from a closed list — never a message — so a
// raw error string carrying a token cannot reach a row even if a caller
// passes err.Error(). The list here mirrors githubfacts.Kind and 044's CHECKs;
// if the client gains a Kind, all three move in the same change. The database
// has the last word, and this layer translates its CHECK/FK violations into
// messages that name the offending value's SHAPE, never its content.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// GitHubFactsHost is the API host facts are read from when a caller names
// none. Enterprise hosts are explicitly unsupported until there is a
// host/credential decision (#3130 §6), but the host is a column so facts from
// two hosts never collide. (#3154's credential layer calls its twin
// GitHubCredentialHost; both mean "github.com", and neither is "main".)
const GitHubFactsHost = "github.com"

// A cache miss is not a 404. These errors say "this project holds no cached
// facts for that repository/branch/PR", which the refresh coordinator turns
// into "unknown" and a background refresh — never into an error a person sees.
var (
	// ErrNoGitHubRepoFacts: the project has no cached facts for the repository.
	ErrNoGitHubRepoFacts = errors.New("no cached facts for this project and repository")
	// ErrNoGitHubBranchFacts: the project's repository facts exist but this
	// branch has never been read.
	ErrNoGitHubBranchFacts = errors.New("no cached facts for this branch")
	// ErrNoGitHubPullFacts: the project's repository facts exist but this pull
	// request has never been read.
	ErrNoGitHubPullFacts = errors.New("no cached facts for this pull request")
)

// GitHubCompleteness says how much of a requested fact set a read could
// actually establish. It is the same closed vocabulary as githubfacts
// .Completeness and 044's CHECK, so the cache stores a client answer verbatim
// with no mapping layer that can drift.
//
//	unknown  nothing was read successfully — the default, and the honest state
//	         for "never checked", "no token" and "the read failed"
//	partial  some reads answered and some did not (rules read, classic branch
//	         protection unreadable with this token): a positive signal for a
//	         rule, never proof that none applies
//	complete every read answered; only here may an empty rule list be read as
//	         "no rule applies"
type GitHubCompleteness string

const (
	GitHubCompletenessUnknown  GitHubCompleteness = "unknown"
	GitHubCompletenessPartial  GitHubCompleteness = "partial"
	GitHubCompletenessComplete GitHubCompleteness = "complete"
)

// GitHubDefaultBranchSource records where a default branch came from. 'manual'
// is the project owner's override and wins; 'unknown' means not detected and is
// never guessed at — 'manual' and 'unknown' are not interchangeable.
type GitHubDefaultBranchSource string

const (
	GitHubBranchSourceUnknown    GitHubDefaultBranchSource = "unknown"
	GitHubBranchSourceREST       GitHubDefaultBranchSource = "rest"
	GitHubBranchSourceGraphQL    GitHubDefaultBranchSource = "graphql"
	GitHubBranchSourceOriginHead GitHubDefaultBranchSource = "origin_head"
	GitHubBranchSourceManual     GitHubDefaultBranchSource = "manual"
)

// A failed read's class. These mirror githubfacts.Kind exactly and 044's
// last_error_class CHECK; a class here that the database refuses (or one the
// client can report that is missing here) is a bug in this list.
const (
	GitHubFactNotFound        = "not_found"
	GitHubFactForbidden       = "forbidden"
	GitHubFactRateLimited     = "rate_limited"
	GitHubFactTimeout         = "timeout"
	GitHubFactCanceled        = "canceled"
	GitHubFactNetwork         = "network"
	GitHubFactServer          = "server"
	GitHubFactMoved           = "moved"
	GitHubFactInvalid         = "invalid"
	GitHubFactNoCredential    = "no_credential"
	GitHubFactNoTarget        = "no_target"
	GitHubFactUnsupportedHost = "unsupported_host"
)

// githubFactFailureClasses is every storable failure class, plus "" for "it
// didn't fail". A closed set on purpose: widening it needs a migration.
var githubFactFailureClasses = map[string]bool{
	"":                        true,
	GitHubFactNotFound:        true,
	GitHubFactForbidden:       true,
	GitHubFactRateLimited:     true,
	GitHubFactTimeout:         true,
	GitHubFactCanceled:        true,
	GitHubFactNetwork:         true,
	GitHubFactServer:          true,
	GitHubFactMoved:           true,
	GitHubFactInvalid:         true,
	GitHubFactNoCredential:    true,
	GitHubFactNoTarget:        true,
	GitHubFactUnsupportedHost: true,
}

// githubFactsSlug is the same shape 044 CHECKs: owner/repo, never a URL and
// never a path. Checked here so a malformed slug is a clear error rather than a
// constraint violation.
var githubFactsSlug = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// GitHubFactsKey names one repository within one project. Every read, write and
// invalidation in this file starts from it, so no statement can be issued
// without a project. Host defaults to GitHubFactsHost; Slug is stored as the
// reader reported it and matched case-insensitively (GitHub treats Owner/Repo
// and owner/repo as one repository).
type GitHubFactsKey struct {
	ProjectID int
	Host      string
	Slug      string
}

// normalize fills in the default host and rejects a key that cannot name a row.
func (k GitHubFactsKey) normalize() (GitHubFactsKey, error) {
	if k.ProjectID <= 0 {
		return GitHubFactsKey{}, fmt.Errorf("repository facts key: a project id is required")
	}
	host := strings.TrimSpace(k.Host)
	if host == "" {
		host = GitHubFactsHost
	}
	slug := strings.TrimSpace(k.Slug)
	if !githubFactsSlug.MatchString(slug) || len(slug) > 200 {
		// The value is not echoed: a URL pasted by mistake can carry a token
		// in its userinfo, and this error reaches logs.
		return GitHubFactsKey{}, fmt.Errorf("repository facts key: the repository must be named owner/repo, " +
			"not a URL or a path")
	}
	return GitHubFactsKey{ProjectID: k.ProjectID, Host: host, Slug: slug}, nil
}

// GitHubRepoFacts is one project's cached facts about one repository: its
// identity and its default branch, and how much of that was verified when.
//
// FetchedAt is nil until a read actually succeeds. Completeness, FetchedAt and
// LastErrorClass/At are what make "not knowing" representable — see the file
// comment for the three states they distinguish.
type GitHubRepoFacts struct {
	ID        int
	ProjectID int
	Host      string
	// Slug is the spelling the row was first created with. Identity is
	// case-insensitive (044's unique index is on lower(repo_slug)), so a later
	// read that spells it differently finds this row rather than creating one.
	Slug string
	// CredentialID is the project credential these facts were read with, nil
	// when they were read without one (a public repository, or no credential
	// attached yet) — a weaker claim the classifier must treat as such. The
	// schema's composite foreign key means a row cannot name another project's
	// credential.
	CredentialID        *int
	DefaultBranch       string
	DefaultBranchSource GitHubDefaultBranchSource
	Completeness        GitHubCompleteness
	// FetchedAt is when these facts were last VERIFIED. A failed refresh leaves
	// it alone, so stale facts stay visibly stale rather than looking freshly
	// checked.
	FetchedAt *time.Time
	// LastErrorClass is why the latest refresh failed ("" when it didn't), as a
	// class — never a message. LastErrorAt is when.
	LastErrorClass string
	LastErrorAt    *time.Time
	UpdatedAt      time.Time
}

// Verified reports whether these facts came from a read that actually
// succeeded. Only verified facts may be evidence; the rest are "unknown".
func (f *GitHubRepoFacts) Verified() bool {
	return f != nil && f.FetchedAt != nil && f.Completeness != GitHubCompletenessUnknown
}

// LastRefreshFailed reports whether the most recent refresh failed while the
// last good facts were kept. Those facts are still dated by FetchedAt, which is
// how a reader tells "stale since then" from "just checked" — a failed refresh
// must never look fresh.
func (f *GitHubRepoFacts) LastRefreshFailed() bool {
	return f != nil && f.LastErrorClass != ""
}

// GitHubRepoFactsWrite is what one refresh established about a repository.
//
// VerifiedAt decides the write's meaning, so a caller cannot accidentally store
// a failure as a success:
//
//   - VerifiedAt set: a successful read. It replaces completeness, the
//     verification time and the credential, and clears any previous failure. It
//     replaces the default branch and its source too — except that a detected
//     read leaves a 'manual' override (and its provenance) in place. A
//     verification older than the stored one is ignored.
//   - VerifiedAt nil with FailureClass set: a failed read. It records the class
//     and leaves every good fact as it was.
//   - VerifiedAt nil with Source 'manual': a person's declaration. It replaces
//     the branch and its source, records no read time, and leaves the last
//     failure visible (a declaration is not a read).
type GitHubRepoFactsWrite struct {
	CredentialID        *int
	DefaultBranch       string
	DefaultBranchSource GitHubDefaultBranchSource
	Completeness        GitHubCompleteness
	VerifiedAt          *time.Time
	FailureClass        string
}

// validate checks the write against the schema's rules, so a bad value is a
// clear error instead of a constraint violation. Messages name the shape of a
// value, never the value itself where it could be sensitive.
func (w GitHubRepoFactsWrite) validate() (GitHubRepoFactsWrite, error) {
	completeness := w.Completeness
	if completeness == "" {
		completeness = GitHubCompletenessUnknown
	}
	switch completeness {
	case GitHubCompletenessUnknown, GitHubCompletenessPartial, GitHubCompletenessComplete:
	default:
		return GitHubRepoFactsWrite{}, fmt.Errorf("repository facts: completeness %q is not one of unknown, partial, complete", completeness)
	}
	source := w.DefaultBranchSource
	if source == "" {
		source = GitHubBranchSourceUnknown
	}
	switch source {
	case GitHubBranchSourceUnknown, GitHubBranchSourceREST, GitHubBranchSourceGraphQL,
		GitHubBranchSourceOriginHead, GitHubBranchSourceManual:
	default:
		return GitHubRepoFactsWrite{}, fmt.Errorf("repository facts: default branch source %q is not one of unknown, rest, graphql, origin_head, manual", source)
	}
	if source != GitHubBranchSourceUnknown && strings.TrimSpace(w.DefaultBranch) == "" {
		return GitHubRepoFactsWrite{}, fmt.Errorf("repository facts: a default branch source other than unknown must name the branch")
	}
	if !githubFactFailureClasses[w.FailureClass] {
		return GitHubRepoFactsWrite{}, fmt.Errorf("repository facts: failure class %q is not one githubfacts reports", w.FailureClass)
	}
	// A verified state is always dated, and "unknown" is the never-verified
	// state — so each implies the other. A read that answered only part of what
	// it asked for is 'partial', not 'unknown' with a timestamp.
	if completeness != GitHubCompletenessUnknown && w.VerifiedAt == nil {
		return GitHubRepoFactsWrite{}, fmt.Errorf("repository facts: completeness %q needs the time it was verified", completeness)
	}
	if completeness == GitHubCompletenessUnknown && w.VerifiedAt != nil {
		return GitHubRepoFactsWrite{}, fmt.Errorf("repository facts: a read with a verification time cannot be 'unknown' completeness")
	}
	// One write is either a read that succeeded or one that failed. A row can
	// hold both over time (a success at T1, a failed refresh at T2); a single
	// write cannot.
	if w.VerifiedAt != nil && w.FailureClass != "" {
		return GitHubRepoFactsWrite{}, fmt.Errorf("repository facts: a write cannot be both verified and a failure; " +
			"a read that answered some of what it asked for is 'partial'")
	}
	out := w
	out.Completeness = completeness
	out.DefaultBranchSource = source
	return out, nil
}

const githubRepoFactsCols = `id, project_id, host, repo_slug, credential_id, default_branch,
	default_branch_source, completeness, fetched_at, last_error_class, last_error_at, updated_at`

func scanGitHubRepoFacts(row rowScanner, f *GitHubRepoFacts) error {
	return row.Scan(&f.ID, &f.ProjectID, &f.Host, &f.Slug, &f.CredentialID, &f.DefaultBranch,
		&f.DefaultBranchSource, &f.Completeness, &f.FetchedAt, &f.LastErrorClass, &f.LastErrorAt, &f.UpdatedAt)
}

// The SQL fragments that decide whether a write takes each group of columns.
// They are constants (never built from input) and refer to EXCLUDED (the
// incoming row) and to the row alias named in each statement's ON CONFLICT.
const (
	// ghFactsReadVerified: the write carries a verification time, so it is a
	// read that succeeded.
	ghFactsReadVerified = `(EXCLUDED.fetched_at IS NOT NULL)`
	// ghFactsDetectionTaken: a verified read replaces the DEFAULT BRANCH and its
	// provenance only when the stored row is not a person's override ("a
	// detection never silently overwrites an override"), and a person's
	// declaration always replaces them. The credential follows the detection for
	// the same reason: "which token produced this answer" belongs to the answer,
	// so it must not change while the answer does not.
	ghFactsDetectionTaken = `((EXCLUDED.fetched_at IS NOT NULL AND %[1]s.default_branch_source <> 'manual')` +
		` OR EXCLUDED.default_branch_source = 'manual')`
	// ghFactsCredentialTaken: which credential produced the verified facts, so it
	// follows any read that succeeded rather than the branch specifically.
	ghFactsCredentialTaken = `(EXCLUDED.fetched_at IS NOT NULL)`
	// ghFactsErrorTaken: a verified read clears the previous failure; a failed
	// read records it. A manual declaration is neither, so it leaves the last
	// failure visible.
	ghFactsErrorTaken = `(EXCLUDED.fetched_at IS NOT NULL OR EXCLUDED.last_error_class <> '')`
)

// ghFactsDetectionTakenFor renders ghFactsDetectionTaken for a row alias.
func ghFactsDetectionTakenFor(alias string) string {
	return fmt.Sprintf(ghFactsDetectionTaken, alias)
}

// GetGitHubRepoFacts returns one project's cached facts for one repository, or
// ErrNoGitHubRepoFacts. The slug is matched case-insensitively, as GitHub treats
// owner and repository names.
func (r *Repo) GetGitHubRepoFacts(ctx context.Context, key GitHubFactsKey) (*GitHubRepoFacts, error) {
	k, err := key.normalize()
	if err != nil {
		return nil, err
	}
	f := &GitHubRepoFacts{}
	err = scanGitHubRepoFacts(r.db.QueryRow(ctx, `SELECT `+githubRepoFactsCols+`
		FROM github_repo_facts WHERE project_id = $1 AND host = $2 AND lower(repo_slug) = lower($3)`,
		k.ProjectID, k.Host, k.Slug), f)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoGitHubRepoFacts
	}
	if err != nil {
		return nil, fmt.Errorf("read repository facts for %s: %w", k.Slug, err)
	}
	return f, nil
}

// ListGitHubRepoFacts returns a project's cached repositories, by slug. It never
// spans projects: the query is project-scoped like every other one here.
func (r *Repo) ListGitHubRepoFacts(ctx context.Context, projectID int) ([]GitHubRepoFacts, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("list repository facts: a project id is required")
	}
	rows, err := r.db.Query(ctx, `SELECT `+githubRepoFactsCols+`
		FROM github_repo_facts WHERE project_id = $1 ORDER BY lower(repo_slug)`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list repository facts for project %d: %w", projectID, err)
	}
	return collect(rows, scanGitHubRepoFacts)
}

// UpsertGitHubRepoFacts records one refresh's outcome and returns the row as it
// is now stored — which, for an out-of-order verification, may be the newer row
// the write was not allowed to overwrite. See GitHubRepoFactsWrite for what each
// kind of write changes.
//
// A failed refresh therefore never replaces a verified timestamp: the row keeps
// its FetchedAt and Completeness and gains LastErrorClass/LastErrorAt, so stale
// facts stay distinguishable from freshly checked ones.
func (r *Repo) UpsertGitHubRepoFacts(ctx context.Context, key GitHubFactsKey, w GitHubRepoFactsWrite) (*GitHubRepoFacts, error) {
	k, err := key.normalize()
	if err != nil {
		return nil, err
	}
	w, err = w.validate()
	if err != nil {
		return nil, err
	}
	f := &GitHubRepoFacts{}
	err = scanGitHubRepoFacts(r.db.QueryRow(ctx, `
		INSERT INTO github_repo_facts AS f (
		    project_id, host, repo_slug, credential_id, default_branch, default_branch_source,
		    completeness, fetched_at, last_error_class, last_error_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, CASE WHEN $9 = '' THEN NULL ELSE NOW() END, NOW())
		ON CONFLICT (project_id, host, lower(repo_slug)) DO UPDATE SET
		    credential_id = CASE WHEN `+ghFactsCredentialTaken+` THEN EXCLUDED.credential_id ELSE f.credential_id END,
		    default_branch = CASE WHEN `+ghFactsDetectionTakenFor("f")+` THEN EXCLUDED.default_branch ELSE f.default_branch END,
		    default_branch_source = CASE WHEN `+ghFactsDetectionTakenFor("f")+` THEN EXCLUDED.default_branch_source ELSE f.default_branch_source END,
		    completeness = CASE WHEN `+ghFactsReadVerified+` THEN EXCLUDED.completeness ELSE f.completeness END,
		    fetched_at = COALESCE(EXCLUDED.fetched_at, f.fetched_at),
		    last_error_class = CASE WHEN `+ghFactsErrorTaken+` THEN EXCLUDED.last_error_class ELSE f.last_error_class END,
		    last_error_at = CASE WHEN `+ghFactsErrorTaken+`
		        THEN CASE WHEN EXCLUDED.last_error_class = '' THEN NULL ELSE NOW() END
		        ELSE f.last_error_at END,
		    updated_at = NOW()
		WHERE EXCLUDED.fetched_at IS NULL OR f.fetched_at IS NULL OR EXCLUDED.fetched_at >= f.fetched_at
		RETURNING `+githubRepoFactsCols,
		k.ProjectID, k.Host, k.Slug, w.CredentialID, w.DefaultBranch, w.DefaultBranchSource,
		w.Completeness, w.VerifiedAt, w.FailureClass), f)
	if errors.Is(err, pgx.ErrNoRows) {
		// The stored row is newer than this verification: return what is
		// actually stored rather than pretending the older read won.
		return r.GetGitHubRepoFacts(ctx, k)
	}
	if err != nil {
		return nil, githubFactsWriteError("repository facts for "+k.Slug, err)
	}
	return f, nil
}

// InvalidateGitHubRepoFacts drops a project's cached facts for a repository,
// with its branch and PR facts (044's ON DELETE CASCADE). It reports whether
// there was a row. A webhook delivery that changes a repository, its default
// branch or a pull request calls this: dropping the rows is honest — AMP no
// longer claims to know — and the next refresh repopulates them. A miss is not
// an error.
func (r *Repo) InvalidateGitHubRepoFacts(ctx context.Context, key GitHubFactsKey) (bool, error) {
	k, err := key.normalize()
	if err != nil {
		return false, err
	}
	tag, err := r.db.Exec(ctx, `DELETE FROM github_repo_facts
		WHERE project_id = $1 AND host = $2 AND lower(repo_slug) = lower($3)`,
		k.ProjectID, k.Host, k.Slug)
	if err != nil {
		return false, fmt.Errorf("invalidate repository facts for %s: %w", k.Slug, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ---- Branch facts ----

// GitHubBranchFacts is what one project's cache holds for one branch of one
// repository: the rules evaluated against it and how completely they were read.
//
// Rules is the reduced array 044 stores (rule type, approving review count,
// code-owner review, required reviewers, last-push approval, thread resolution,
// ruleset id/source) — an array, never a response envelope. It stays as JSON
// because the classifier's own types (#3133) own its interpretation; this layer
// only guarantees it is an array of rules.
type GitHubBranchFacts struct {
	RepoFactsID    int
	Branch         string
	Rules          []byte
	Completeness   GitHubCompleteness
	FetchedAt      *time.Time
	LastErrorClass string
	LastErrorAt    *time.Time
	UpdatedAt      time.Time
}

// Verified reports whether the rules came from a read that succeeded. Only
// verified rules are evidence; an empty list is "no rules" only when this is
// true and Completeness is complete.
func (b *GitHubBranchFacts) Verified() bool {
	return b != nil && b.FetchedAt != nil && b.Completeness != GitHubCompletenessUnknown
}

// LastRefreshFailed reports whether the latest refresh failed while the last
// good rules were kept, so a reader sees them as stale rather than fresh.
func (b *GitHubBranchFacts) LastRefreshFailed() bool {
	return b != nil && b.LastErrorClass != ""
}

// GitHubBranchFactsWrite is one refresh's outcome for a branch. VerifiedAt has
// the same meaning as on GitHubRepoFactsWrite: set for a successful read (which
// replaces the rules, completeness and time), nil for a failure (which keeps
// them and records the class).
type GitHubBranchFactsWrite struct {
	Rules        []byte
	Completeness GitHubCompleteness
	VerifiedAt   *time.Time
	FailureClass string
}

func (w GitHubBranchFactsWrite) validate() (GitHubBranchFactsWrite, error) {
	completeness := w.Completeness
	if completeness == "" {
		completeness = GitHubCompletenessUnknown
	}
	switch completeness {
	case GitHubCompletenessUnknown, GitHubCompletenessPartial, GitHubCompletenessComplete:
	default:
		return GitHubBranchFactsWrite{}, fmt.Errorf("branch facts: completeness %q is not one of unknown, partial, complete", completeness)
	}
	if !githubFactFailureClasses[w.FailureClass] {
		return GitHubBranchFactsWrite{}, fmt.Errorf("branch facts: failure class %q is not one githubfacts reports", w.FailureClass)
	}
	if completeness != GitHubCompletenessUnknown && w.VerifiedAt == nil {
		return GitHubBranchFactsWrite{}, fmt.Errorf("branch facts: completeness %q needs the time it was verified", completeness)
	}
	if completeness == GitHubCompletenessUnknown && w.VerifiedAt != nil {
		return GitHubBranchFactsWrite{}, fmt.Errorf("branch facts: a read with a verification time cannot be 'unknown' completeness")
	}
	if w.VerifiedAt != nil && w.FailureClass != "" {
		return GitHubBranchFactsWrite{}, fmt.Errorf("branch facts: a write cannot be both verified and a failure; " +
			"a read that answered some of what it asked for is 'partial'")
	}
	rules, err := checkGitHubRuleArray(w.Rules)
	if err != nil {
		return GitHubBranchFactsWrite{}, err
	}
	out := w
	out.Completeness, out.Rules = completeness, rules
	return out, nil
}

// checkGitHubRuleArray keeps "rules" an array of rules. An empty array is a real
// answer — the read happened and nothing applies — so it is kept as '[]' rather
// than NULL.
func checkGitHubRuleArray(rules []byte) ([]byte, error) {
	if len(rules) == 0 {
		return []byte("[]"), nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(rules, &arr); err != nil {
		return nil, fmt.Errorf("branch facts: rules must be a JSON array of rules: %w", err)
	}
	return rules, nil
}

const githubBranchFactsCols = `bf.repo_facts_id, bf.branch, bf.rules, bf.completeness, bf.fetched_at,
	bf.last_error_class, bf.last_error_at, bf.updated_at`

// githubBranchFactsFrom joins a branch fact to its repository row so the read is
// scoped by the project's own key, not by a repo_facts_id the caller supplied: a
// branch fact cannot be reached through another project's repository row.
const githubBranchFactsFrom = ` FROM github_branch_facts bf
	JOIN github_repo_facts f ON f.id = bf.repo_facts_id`

const githubFactsScope = ` WHERE f.project_id = $1 AND f.host = $2 AND lower(f.repo_slug) = lower($3)`

func scanGitHubBranchFacts(row rowScanner, b *GitHubBranchFacts) error {
	return row.Scan(&b.RepoFactsID, &b.Branch, &b.Rules, &b.Completeness, &b.FetchedAt,
		&b.LastErrorClass, &b.LastErrorAt, &b.UpdatedAt)
}

// GetGitHubBranchFacts returns the cached rules for one branch, or
// ErrNoGitHubBranchFacts when that branch was never read. ErrNoGitHubRepoFacts
// means the project has no repository facts at all.
func (r *Repo) GetGitHubBranchFacts(ctx context.Context, key GitHubFactsKey, branch string) (*GitHubBranchFacts, error) {
	k, err := key.normalize()
	if err != nil {
		return nil, err
	}
	branch, err = normalizeGitHubBranch(branch)
	if err != nil {
		return nil, err
	}
	b := &GitHubBranchFacts{}
	err = scanGitHubBranchFacts(r.db.QueryRow(ctx, `SELECT `+githubBranchFactsCols+githubBranchFactsFrom+
		githubFactsScope+` AND bf.branch = $4`, k.ProjectID, k.Host, k.Slug, branch), b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoGitHubBranchFacts
	}
	if err != nil {
		return nil, fmt.Errorf("read branch facts for %s@%s: %w", k.Slug, branch, err)
	}
	return b, nil
}

// ListGitHubBranchFacts returns every branch this project has cached for the
// repository, by branch name. ErrNoGitHubRepoFacts when the repository itself is
// not cached; an empty list means the repository is cached with no branch read
// yet.
func (r *Repo) ListGitHubBranchFacts(ctx context.Context, key GitHubFactsKey) ([]GitHubBranchFacts, error) {
	k, err := key.normalize()
	if err != nil {
		return nil, err
	}
	if _, err := r.GetGitHubRepoFacts(ctx, k); err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx, `SELECT `+githubBranchFactsCols+githubBranchFactsFrom+
		githubFactsScope+` ORDER BY bf.branch`, k.ProjectID, k.Host, k.Slug)
	if err != nil {
		return nil, fmt.Errorf("list branch facts for %s: %w", k.Slug, err)
	}
	return collect(rows, scanGitHubBranchFacts)
}

// UpsertGitHubBranchFacts records one refresh's outcome for a branch and returns
// the row as stored. The repository's facts must exist first: a branch fact
// hangs off the repository row and must inherit its project and credential
// context (044), so writing one without it is ErrNoGitHubRepoFacts rather than a
// row with no scope. As with repository facts, a failed refresh keeps the last
// good rules and verification time and only records the failure.
func (r *Repo) UpsertGitHubBranchFacts(ctx context.Context, key GitHubFactsKey, branch string, w GitHubBranchFactsWrite) (*GitHubBranchFacts, error) {
	k, err := key.normalize()
	if err != nil {
		return nil, err
	}
	branch, err = normalizeGitHubBranch(branch)
	if err != nil {
		return nil, err
	}
	w, err = w.validate()
	if err != nil {
		return nil, err
	}
	b := &GitHubBranchFacts{}
	err = scanGitHubBranchFacts(r.db.QueryRow(ctx, `
		INSERT INTO github_branch_facts AS bf (
		    repo_facts_id, branch, rules, completeness, fetched_at, last_error_class, last_error_at, updated_at)
		SELECT f.id, $4, $5, $6, $7, $8, CASE WHEN $8 = '' THEN NULL ELSE NOW() END, NOW()
		  FROM github_repo_facts f`+githubFactsScope+`
		ON CONFLICT (repo_facts_id, branch) DO UPDATE SET
		    rules = CASE WHEN `+ghFactsReadVerified+` THEN EXCLUDED.rules ELSE bf.rules END,
		    completeness = CASE WHEN `+ghFactsReadVerified+` THEN EXCLUDED.completeness ELSE bf.completeness END,
		    fetched_at = COALESCE(EXCLUDED.fetched_at, bf.fetched_at),
		    last_error_class = CASE WHEN `+ghFactsErrorTaken+` THEN EXCLUDED.last_error_class ELSE bf.last_error_class END,
		    last_error_at = CASE WHEN `+ghFactsErrorTaken+`
		        THEN CASE WHEN EXCLUDED.last_error_class = '' THEN NULL ELSE NOW() END
		        ELSE bf.last_error_at END,
		    updated_at = NOW()
		WHERE EXCLUDED.fetched_at IS NULL OR bf.fetched_at IS NULL OR EXCLUDED.fetched_at >= bf.fetched_at
		RETURNING `+githubBranchFactsCols,
		k.ProjectID, k.Host, k.Slug, branch, w.Rules, w.Completeness, w.VerifiedAt, w.FailureClass), b)
	if errors.Is(err, pgx.ErrNoRows) {
		// Either the repository row does not exist (nothing inserted) or the
		// stored branch facts are newer. Tell the two apart by reading.
		if _, gerr := r.GetGitHubRepoFacts(ctx, k); gerr != nil {
			return nil, gerr
		}
		return r.GetGitHubBranchFacts(ctx, k, branch)
	}
	if err != nil {
		return nil, githubFactsWriteError("branch facts for "+k.Slug+"@"+branch, err)
	}
	return b, nil
}

// InvalidateGitHubBranchFacts drops the cached rules for one branch, so the next
// read re-fetches them. It reports whether there was a row; a miss is not an
// error. Nothing else about the repository's facts changes.
func (r *Repo) InvalidateGitHubBranchFacts(ctx context.Context, key GitHubFactsKey, branch string) (bool, error) {
	k, err := key.normalize()
	if err != nil {
		return false, err
	}
	branch, err = normalizeGitHubBranch(branch)
	if err != nil {
		return false, err
	}
	tag, err := r.db.Exec(ctx, `DELETE FROM github_branch_facts bf
		USING github_repo_facts f
		WHERE f.id = bf.repo_facts_id AND f.project_id = $1 AND f.host = $2
		  AND lower(f.repo_slug) = lower($3) AND bf.branch = $4`,
		k.ProjectID, k.Host, k.Slug, branch)
	if err != nil {
		return false, fmt.Errorf("invalidate branch facts for %s@%s: %w", k.Slug, branch, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ---- Pull request facts ----

// GitHubPullFacts is what one project's cache holds for one pull request. PR
// facts change on every reviewer action, so the coordinator's TTL for them is
// much shorter than for repository or branch facts (#3130 §2); the storage layer
// keeps the same "verified when" discipline either way.
//
// RequestedReviewers is an over-approximation on its own — GitHub auto-requests
// code owners whether or not a rule requires them — which is why it is stored
// beside the rules read rather than instead of it.
type GitHubPullFacts struct {
	RepoFactsID        int
	Number             int
	BaseBranch         string
	State              string
	ReviewDecision     string
	RequestedReviewers []string
	RequestedTeams     []string
	MergeableState     string
	Completeness       GitHubCompleteness
	FetchedAt          *time.Time
	LastErrorClass     string
	LastErrorAt        *time.Time
	UpdatedAt          time.Time
}

// Verified reports whether the PR facts came from a read that succeeded.
func (p *GitHubPullFacts) Verified() bool {
	return p != nil && p.FetchedAt != nil && p.Completeness != GitHubCompletenessUnknown
}

// LastRefreshFailed reports whether the latest refresh failed while the last
// good PR facts were kept.
func (p *GitHubPullFacts) LastRefreshFailed() bool {
	return p != nil && p.LastErrorClass != ""
}

// GitHubPullFactsWrite is one refresh's outcome for a pull request. VerifiedAt
// has the same meaning as on GitHubRepoFactsWrite. The closed vocabularies below
// mirror 044's CHECKs; anything GitHub adds later is refused at write time until
// the migration is widened, on purpose.
type GitHubPullFactsWrite struct {
	BaseBranch         string
	State              string
	ReviewDecision     string
	RequestedReviewers []string
	RequestedTeams     []string
	MergeableState     string
	Completeness       GitHubCompleteness
	VerifiedAt         *time.Time
	FailureClass       string
}

var (
	githubPullStates = map[string]bool{
		"unknown": true, "open": true, "closed": true, "merged": true,
	}
	githubPullReviewDecisions = map[string]bool{
		"unknown": true, "none": true, "review_required": true, "approved": true, "changes_requested": true,
	}
	githubPullMergeableStates = map[string]bool{
		"unknown": true, "blocked": true, "clean": true, "dirty": true, "unstable": true,
		"behind": true, "draft": true, "has_hooks": true,
	}
)

func (w GitHubPullFactsWrite) validate() (GitHubPullFactsWrite, error) {
	completeness := w.Completeness
	if completeness == "" {
		completeness = GitHubCompletenessUnknown
	}
	switch completeness {
	case GitHubCompletenessUnknown, GitHubCompletenessPartial, GitHubCompletenessComplete:
	default:
		return GitHubPullFactsWrite{}, fmt.Errorf("pull request facts: completeness %q is not one of unknown, partial, complete", completeness)
	}
	if !githubFactFailureClasses[w.FailureClass] {
		return GitHubPullFactsWrite{}, fmt.Errorf("pull request facts: failure class %q is not one githubfacts reports", w.FailureClass)
	}
	if completeness != GitHubCompletenessUnknown && w.VerifiedAt == nil {
		return GitHubPullFactsWrite{}, fmt.Errorf("pull request facts: completeness %q needs the time it was verified", completeness)
	}
	if completeness == GitHubCompletenessUnknown && w.VerifiedAt != nil {
		return GitHubPullFactsWrite{}, fmt.Errorf("pull request facts: a read with a verification time cannot be 'unknown' completeness")
	}
	if w.VerifiedAt != nil && w.FailureClass != "" {
		return GitHubPullFactsWrite{}, fmt.Errorf("pull request facts: a write cannot be both verified and a failure")
	}
	state := strings.TrimSpace(w.State)
	if state == "" {
		state = "unknown"
	}
	if !githubPullStates[state] {
		return GitHubPullFactsWrite{}, fmt.Errorf("pull request facts: state %q is not one github_pull_facts accepts", state)
	}
	decision := strings.TrimSpace(w.ReviewDecision)
	if decision == "" {
		decision = "unknown"
	}
	if !githubPullReviewDecisions[decision] {
		return GitHubPullFactsWrite{}, fmt.Errorf("pull request facts: review decision %q is not one github_pull_facts accepts", decision)
	}
	mergeable := strings.TrimSpace(w.MergeableState)
	if mergeable == "" {
		mergeable = "unknown"
	}
	if !githubPullMergeableStates[mergeable] {
		return GitHubPullFactsWrite{}, fmt.Errorf("pull request facts: mergeable state %q is not one github_pull_facts accepts", mergeable)
	}
	out := w
	out.Completeness = completeness
	out.State, out.ReviewDecision, out.MergeableState = state, decision, mergeable
	out.RequestedReviewers = nonNilStrings(w.RequestedReviewers)
	out.RequestedTeams = nonNilStrings(w.RequestedTeams)
	return out, nil
}

// nonNilStrings turns a nil slice into the empty array the column defaults to,
// so "no reviewers" is stored as '{}' rather than NULL.
func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

const githubPullFactsCols = `pf.repo_facts_id, pf.number, pf.base_branch, pf.state, pf.review_decision,
	pf.requested_reviewers, pf.requested_teams, pf.mergeable_state, pf.completeness, pf.fetched_at,
	pf.last_error_class, pf.last_error_at, pf.updated_at`

const githubPullFactsFrom = ` FROM github_pull_facts pf
	JOIN github_repo_facts f ON f.id = pf.repo_facts_id`

func scanGitHubPullFacts(row rowScanner, p *GitHubPullFacts) error {
	return row.Scan(&p.RepoFactsID, &p.Number, &p.BaseBranch, &p.State, &p.ReviewDecision,
		&p.RequestedReviewers, &p.RequestedTeams, &p.MergeableState, &p.Completeness, &p.FetchedAt,
		&p.LastErrorClass, &p.LastErrorAt, &p.UpdatedAt)
}

// GetGitHubPullFacts returns the cached facts for one pull request, or
// ErrNoGitHubPullFacts when that PR was never read. ErrNoGitHubRepoFacts means
// the project has no repository facts at all.
func (r *Repo) GetGitHubPullFacts(ctx context.Context, key GitHubFactsKey, number int) (*GitHubPullFacts, error) {
	k, err := key.normalize()
	if err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("pull request facts: the pull request number must be positive")
	}
	p := &GitHubPullFacts{}
	err = scanGitHubPullFacts(r.db.QueryRow(ctx, `SELECT `+githubPullFactsCols+githubPullFactsFrom+
		githubFactsScope+` AND pf.number = $4`, k.ProjectID, k.Host, k.Slug, number), p)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoGitHubPullFacts
	}
	if err != nil {
		return nil, fmt.Errorf("read pull request facts for %s#%d: %w", k.Slug, number, err)
	}
	return p, nil
}

// UpsertGitHubPullFacts records one refresh's outcome for a pull request and
// returns the row as stored. Like branch facts, the repository's facts must exist
// first (ErrNoGitHubRepoFacts), and a failed refresh keeps the last good facts and
// verification time while recording the failure class.
func (r *Repo) UpsertGitHubPullFacts(ctx context.Context, key GitHubFactsKey, number int, w GitHubPullFactsWrite) (*GitHubPullFacts, error) {
	k, err := key.normalize()
	if err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("pull request facts: the pull request number must be positive")
	}
	w, err = w.validate()
	if err != nil {
		return nil, err
	}
	p := &GitHubPullFacts{}
	err = scanGitHubPullFacts(r.db.QueryRow(ctx, `
		INSERT INTO github_pull_facts AS pf (
		    repo_facts_id, number, base_branch, state, review_decision, requested_reviewers,
		    requested_teams, mergeable_state, completeness, fetched_at, last_error_class,
		    last_error_at, updated_at)
		SELECT f.id, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
		       CASE WHEN $13 = '' THEN NULL ELSE NOW() END, NOW()
		  FROM github_repo_facts f`+githubFactsScope+`
		ON CONFLICT (repo_facts_id, number) DO UPDATE SET
		    base_branch = CASE WHEN `+ghFactsReadVerified+` THEN EXCLUDED.base_branch ELSE pf.base_branch END,
		    state = CASE WHEN `+ghFactsReadVerified+` THEN EXCLUDED.state ELSE pf.state END,
		    review_decision = CASE WHEN `+ghFactsReadVerified+` THEN EXCLUDED.review_decision ELSE pf.review_decision END,
		    requested_reviewers = CASE WHEN `+ghFactsReadVerified+` THEN EXCLUDED.requested_reviewers ELSE pf.requested_reviewers END,
		    requested_teams = CASE WHEN `+ghFactsReadVerified+` THEN EXCLUDED.requested_teams ELSE pf.requested_teams END,
		    mergeable_state = CASE WHEN `+ghFactsReadVerified+` THEN EXCLUDED.mergeable_state ELSE pf.mergeable_state END,
		    completeness = CASE WHEN `+ghFactsReadVerified+` THEN EXCLUDED.completeness ELSE pf.completeness END,
		    fetched_at = COALESCE(EXCLUDED.fetched_at, pf.fetched_at),
		    last_error_class = CASE WHEN `+ghFactsErrorTaken+` THEN EXCLUDED.last_error_class ELSE pf.last_error_class END,
		    last_error_at = CASE WHEN `+ghFactsErrorTaken+`
		        THEN CASE WHEN EXCLUDED.last_error_class = '' THEN NULL ELSE NOW() END
		        ELSE pf.last_error_at END,
		    updated_at = NOW()
		WHERE EXCLUDED.fetched_at IS NULL OR pf.fetched_at IS NULL OR EXCLUDED.fetched_at >= pf.fetched_at
		RETURNING `+githubPullFactsCols,
		k.ProjectID, k.Host, k.Slug, number, w.BaseBranch, w.State, w.ReviewDecision,
		w.RequestedReviewers, w.RequestedTeams, w.MergeableState, w.Completeness, w.VerifiedAt,
		w.FailureClass), p)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := r.GetGitHubRepoFacts(ctx, k); gerr != nil {
			return nil, gerr
		}
		return r.GetGitHubPullFacts(ctx, k, number)
	}
	if err != nil {
		return nil, githubFactsWriteError(fmt.Sprintf("pull request facts for %s#%d", k.Slug, number), err)
	}
	return p, nil
}

// InvalidateGitHubPullFacts drops the cached facts for one pull request. It
// reports whether there was a row; a miss is not an error. The webhook path calls
// it on a review or a review dismissal, where the PR facts (requested reviewers,
// review decision) are exactly what went stale.
func (r *Repo) InvalidateGitHubPullFacts(ctx context.Context, key GitHubFactsKey, number int) (bool, error) {
	k, err := key.normalize()
	if err != nil {
		return false, err
	}
	if number <= 0 {
		return false, fmt.Errorf("pull request facts: the pull request number must be positive")
	}
	tag, err := r.db.Exec(ctx, `DELETE FROM github_pull_facts pf
		USING github_repo_facts f
		WHERE f.id = pf.repo_facts_id AND f.project_id = $1 AND f.host = $2
		  AND lower(f.repo_slug) = lower($3) AND pf.number = $4`,
		k.ProjectID, k.Host, k.Slug, number)
	if err != nil {
		return false, fmt.Errorf("invalidate pull request facts for %s#%d: %w", k.Slug, number, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ---- shared helpers ----

// normalizeGitHubBranch trims a branch name and refuses one the schema cannot
// store. Branch names are free-form (slashes, dots) up to 255 characters; an
// empty name is never "the default branch".
func normalizeGitHubBranch(branch string) (string, error) {
	b := strings.TrimSpace(branch)
	if b == "" || len(b) > 255 {
		return "", fmt.Errorf("repository facts: a branch name is required and at most 255 characters")
	}
	return b, nil
}

// isCheckViolation reports whether err is a Postgres CHECK violation (23514).
func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

// isForeignKeyViolation reports whether err is a Postgres foreign key violation
// (23503).
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// githubFactsWriteError turns the two constraint failures a facts write can
// produce into messages that name what went wrong without echoing a value.
// Anything else is returned as it is. A CHECK violation means a closed
// vocabulary was widened in the client but not in the migration (or the other
// way round); a foreign key violation means the credential does not belong to
// this project and host — the schema's cross-project guard, working as designed.
func githubFactsWriteError(what string, err error) error {
	switch {
	case isCheckViolation(err):
		return fmt.Errorf("%s: a value is outside the closed vocabulary migration 044 stores: %w", what, err)
	case isForeignKeyViolation(err):
		return fmt.Errorf("%s: the credential named does not belong to this project and host: %w", what, err)
	default:
		return fmt.Errorf("%s: %w", what, err)
	}
}
