package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// ---- Inbound GitHub webhook (AMP #2479, KB decisions/human-work-ux.md §4) ----
//
// GitHub posts a project's pull_request events to
// POST /api/hooks/github/{project_id}. The request has no AMP session and is
// authenticated only by X-Hub-Signature-256: an HMAC-SHA256 of the body
// under the project's secret. A merged PR records the merge (source
// "github") on the project's tickets whose runs carry the PR URL, its head
// branch or one of its commits. A PR closed without merging marks the
// waiting ones abandoned. Each delivery is handled once (X-GitHub-Delivery).
//
// The handler acts as githubPrincipal, a global-admin principal, and that
// is safe only because every lookup it makes is limited to the project in
// the URL, whose secret signed the request: it never uses Trace, which
// spans every project the principal can see.

// githubPrincipal is who the webhook acts as: history and audit rows show
// "github".
var githubPrincipal = authz.Principal{Email: domain.MergeSourceGitHub, Super: true}

// GitHubHookInfo is a project's GitHub webhook as the settings page shows it.
type GitHubHookInfo struct {
	Configured bool                        `json:"configured"`
	URL        string                      `json:"url"`
	Hook       *domain.GitHubHook          `json:"hook,omitempty"`
	Deliveries []domain.GitHubHookDelivery `json:"deliveries"`
	// Secret is set only in the answer to generating or rotating it: it is
	// shown once and can't be read back.
	Secret string `json:"secret,omitempty"`
	// Encrypted says whether the secret is stored encrypted (the server has
	// an encryption key).
	Encrypted bool `json:"encrypted"`
	// Setup is what to enter in GitHub's webhook form.
	Setup map[string]string `json:"setup"`
}

// GitHubHookURL is the address GitHub posts projectID's events to.
func (s *Service) GitHubHookURL(projectID int) string {
	return fmt.Sprintf("%s/api/hooks/github/%d", strings.TrimRight(s.AppURL, "/"), projectID)
}

func (s *Service) githubHookInfo(ctx context.Context, projectID int) (*GitHubHookInfo, error) {
	info := &GitHubHookInfo{URL: s.GitHubHookURL(projectID), Deliveries: []domain.GitHubHookDelivery{}, Encrypted: s.Repo.CanSealSecrets(),
		Setup: map[string]string{"payload_url": s.GitHubHookURL(projectID), "content_type": "application/json", "events": "Pull requests"}}
	h, err := s.Repo.GetGitHubHook(ctx, projectID)
	if errors.Is(err, repository.ErrNoGitHubHook) {
		return info, nil
	}
	if err != nil {
		return nil, err
	}
	info.Configured, info.Hook = true, h
	if info.Deliveries, err = s.Repo.ListGitHubDeliveries(ctx, projectID); err != nil {
		return nil, err
	}
	return info, nil
}

// GitHubHook is projectID's GitHub webhook: whether it is set up, its URL
// and its recent deliveries (project owners).
func (s *Service) GitHubHook(ctx context.Context, p authz.Principal, projectID int) (*GitHubHookInfo, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Owner); err != nil {
		return nil, err
	}
	return s.githubHookInfo(ctx, projectID)
}

// RotateGitHubHookSecret generates projectID's GitHub webhook secret, or
// replaces it, and returns it this once (project owners). GitHub's webhook
// must be updated with the new one: deliveries signed with the old secret
// are refused from now on.
func (s *Service) RotateGitHubHookSecret(ctx context.Context, p authz.Principal, projectID int) (*GitHubHookInfo, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Owner); err != nil {
		return nil, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	secret := hex.EncodeToString(b)
	prev, _ := s.Repo.GetGitHubHook(ctx, projectID)
	if _, err := s.Repo.SetGitHubHookSecret(ctx, projectID, secret, p.UserRef()); err != nil {
		return nil, err
	}
	action, summary := "project.github_hook.create", "set up the GitHub merge webhook"
	if prev != nil {
		action, summary = "project.github_hook.rotate_secret", "rotated the GitHub merge webhook's secret"
	}
	s.audit(ctx, p, domain.AuditEntry{Action: action, ProjectID: intRef(projectID), TargetType: "project", TargetID: idStr(projectID),
		Summary: p.Label() + " " + summary})
	info, err := s.githubHookInfo(ctx, projectID)
	if err != nil {
		return nil, err
	}
	info.Secret = secret
	return info, nil
}

// DeleteGitHubHook turns projectID's GitHub webhook off: its secret and
// delivery log are deleted, and GitHub's deliveries are refused (project
// owners).
func (s *Service) DeleteGitHubHook(ctx context.Context, p authz.Principal, projectID int) (bool, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Owner); err != nil {
		return false, err
	}
	had, err := s.Repo.DeleteGitHubHook(ctx, projectID)
	if err != nil || !had {
		return had, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "project.github_hook.delete", ProjectID: intRef(projectID), TargetType: "project",
		TargetID: idStr(projectID), Summary: p.Label() + " turned the GitHub merge webhook off"})
	return true, nil
}

// GitHubDelivery is one request to the webhook.
type GitHubDelivery struct {
	ProjectID  int
	Event      string // X-GitHub-Event
	DeliveryID string // X-GitHub-Delivery
	Signature  string // X-Hub-Signature-256
	Body       []byte
}

// GitHubResult is the webhook's answer.
type GitHubResult struct {
	Status   int    `json:"-"`
	Result   string `json:"result"`
	Replayed bool   `json:"replayed,omitempty"`
	TaskIDs  []int  `json:"task_ids,omitempty"`
	// Invalidated says this delivery dropped the cached repository facts it
	// contradicted (AMP #3160), so the next My work read re-reads them instead
	// of waiting out the TTL.
	Invalidated bool `json:"invalidated,omitempty"`
}

// verifyGitHubSignature checks X-Hub-Signature-256 ("sha256=<hex>") against
// body under secret, in constant time.
func verifyGitHubSignature(secret string, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(strings.TrimSpace(header), "sha256=")
	if !ok || secret == "" {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// SignGitHubPayload is the X-Hub-Signature-256 GitHub sends for body.
func SignGitHubPayload(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ghPullRequestEvent is the part of GitHub's pull_request payload AMP reads.
type ghPullRequestEvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Number         int    `json:"number"`
		HTMLURL        string `json:"html_url"`
		Merged         bool   `json:"merged"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		Head           ghRef  `json:"head"`
		Base           ghRef  `json:"base"`
	} `json:"pull_request"`
	// Repository is the repository the hook is installed on: the base side of
	// the pull request, and the identity the facts cache is keyed by.
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// pullNumber is the pull request's number, from whichever of the two places
// GitHub put it. Both are present on a real delivery; a payload with neither is
// not about a pull request AMP can name, and 0 means "don't invalidate".
func (e ghPullRequestEvent) pullNumber() int {
	if e.PullRequest.Number > 0 {
		return e.PullRequest.Number
	}
	return e.Number
}

// repoSlug is the repository this delivery is about, as the facts cache names
// it. The top-level `repository` is the repo the hook belongs to (the base side
// of a fork PR); base.repo is the same repository and is the fallback for a
// payload that omits it. A fork's head repo is deliberately never used: its
// rules are not the ones the merge is judged by.
func (e ghPullRequestEvent) repoSlug() string {
	if slug := strings.ToLower(strings.TrimSpace(e.Repository.FullName)); slug != "" {
		return slug
	}
	return e.PullRequest.Base.repo()
}

type ghRef struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

func (r ghRef) repo() string {
	if r.Repo == nil {
		return ""
	}
	return strings.ToLower(r.Repo.FullName)
}

// HandleGitHubDelivery authenticates and handles one webhook request. It
// never logs or returns the secret or the signature.
//
//   - no hook for the project: 404; a missing or wrong signature: 401, and
//     nothing changes;
//   - a delivery already handled: 200, a no-op that returns the first
//     result;
//   - ping: 200; pull_request closed: 200 with the tickets it recorded;
//     anything else: 204.
func (s *Service) HandleGitHubDelivery(ctx context.Context, d GitHubDelivery) GitHubResult {
	secret, err := s.Repo.GitHubHookSecret(ctx, d.ProjectID)
	if errors.Is(err, repository.ErrNoGitHubHook) {
		return GitHubResult{Status: http.StatusNotFound, Result: "no GitHub webhook is set up for this project"}
	}
	if err != nil {
		slog.Error("github webhook: read secret", "project_id", d.ProjectID, "err", err)
		return GitHubResult{Status: http.StatusInternalServerError, Result: "can't check the signature"}
	}
	if !verifyGitHubSignature(secret, d.Body, d.Signature) {
		if err := s.Repo.NoteGitHubHookRejected(ctx, d.ProjectID); err != nil {
			slog.Warn("github webhook: count rejection", "project_id", d.ProjectID, "err", err)
		}
		slog.Warn("github webhook: signature missing or invalid", "project_id", d.ProjectID, "signature_present", d.Signature != "")
		return GitHubResult{Status: http.StatusUnauthorized, Result: "signature missing or invalid"}
	}
	if d.Event == "" || d.DeliveryID == "" || len(d.DeliveryID) > 100 {
		return GitHubResult{Status: http.StatusBadRequest, Result: "X-GitHub-Event and X-GitHub-Delivery are required"}
	}
	var ev ghPullRequestEvent
	if d.Event == "pull_request" {
		if err := json.Unmarshal(d.Body, &ev); err != nil {
			return GitHubResult{Status: http.StatusBadRequest, Result: "invalid pull_request payload"}
		}
	}
	claimed, prev, err := s.Repo.ClaimGitHubDelivery(ctx, d.ProjectID, d.DeliveryID, d.Event, ev.Action)
	if err != nil {
		slog.Error("github webhook: claim delivery", "project_id", d.ProjectID, "err", err)
		return GitHubResult{Status: http.StatusInternalServerError, Result: "can't record the delivery"}
	}
	if !claimed {
		res := GitHubResult{Status: http.StatusOK, Replayed: true, TaskIDs: prev.TaskIDs,
			Result: "already handled this delivery: " + nameOrDash(prev.Result)}
		return res
	}
	// The delivery is new evidence about this pull request, so the cached facts it
	// contradicts go now rather than at the end of their TTL (AMP #3160). This runs
	// after the claim (a replay changes nothing) and before the event is acted on,
	// because the delivery is a real change on GitHub whether or not AMP can act
	// on it: a merge recording that fails is redelivered as a replay, which is a
	// no-op, so invalidating afterwards would leave the cache wrong until the TTL.
	invalidated := s.invalidateFactsForDelivery(ctx, d.ProjectID, d.Event, ev)
	res := s.handleGitHubEvent(ctx, d.ProjectID, d.Event, ev)
	if invalidated {
		res.Invalidated = true
		// Said in the result as well as the field: a 204 has no body to read, and
		// the delivery log is where an operator asks whether a delivery reached
		// the cached facts.
		res.Result += "; dropped the cached repository facts it contradicts"
	}
	if err := s.Repo.FinishGitHubDelivery(ctx, d.ProjectID, d.DeliveryID, res.Status, res.Result, res.TaskIDs); err != nil {
		slog.Warn("github webhook: record delivery", "project_id", d.ProjectID, "err", err)
	}
	return res
}

// invalidateFactsForDelivery drops the cached repository facts a delivery has
// just contradicted, and reports whether it dropped any. It is the webhook's
// whole write to the facts cache (AMP #3160), and it is deliberately narrow:
//
//   - Only a `pull_request` delivery invalidates anything. The hook is subscribed
//     to pull requests alone (githubHookInfo's setup), and an unsupported event
//     stays harmless: it changes no cache and no ticket.
//   - Only THAT pull request's facts are dropped. A pull-request event says
//     nothing about the default branch or a branch's rules, so the repository and
//     branch rows are left alone — dropping them would discard good evidence and
//     make the 15m TTL meaningless. A default-branch rename is a `repository`
//     event, which this hook does not receive, so that stays the TTL's job: the
//     correctness backstop when no delivery arrives.
//   - Only the project in the URL is touched, and only for the repository the
//     payload names. The key carries the project, so a delivery to one project's
//     hook cannot reach another project's rows even when both cache the same
//     repository; the other project's copy stays stale until its own TTL, which
//     is the safe direction (stale, never wrong).
//
// A failure here is logged and swallowed: the delivery's own work (recording a
// merge) must not fail because a cache row could not be dropped, and the TTL
// still bounds how long the stale row can be believed.
func (s *Service) invalidateFactsForDelivery(ctx context.Context, projectID int, event string, ev ghPullRequestEvent) bool {
	if s.Facts == nil || event != "pull_request" {
		return false
	}
	slug, pr := ev.repoSlug(), ev.pullNumber()
	if slug == "" || pr <= 0 {
		// Nothing to name: a payload without a repository or a number cannot
		// identify a cached row, and guessing one would drop another PR's facts.
		return false
	}
	had, err := s.Facts.InvalidatePull(ctx, GitHubFactsRequest{ProjectID: projectID, Slug: slug, PR: pr})
	if err != nil {
		slog.Warn("github webhook: invalidate cached repository facts", "project_id", projectID, "slug", slug, "pr", pr, "err", err)
		return false
	}
	return had
}

func (s *Service) handleGitHubEvent(ctx context.Context, projectID int, event string, ev ghPullRequestEvent) GitHubResult {
	switch {
	case event == "ping":
		return GitHubResult{Status: http.StatusOK, Result: "pong: AMP receives this project's pull request events"}
	case event != "pull_request":
		return GitHubResult{Status: http.StatusNoContent, Result: "ignored: AMP only reads pull_request events"}
	case ev.Action != "closed":
		return GitHubResult{Status: http.StatusNoContent, Result: "ignored: pull_request " + nameOrDash(ev.Action)}
	}
	pr := ev.PullRequest
	// The head branch identifies the work only when it lives in the same
	// repository: a fork's "main" is not this project's "main".
	branch := ""
	if pr.Head.repo() != "" && pr.Head.repo() == pr.Base.repo() {
		branch = strings.TrimSpace(pr.Head.Ref)
	}
	var commits []string
	for _, c := range []string{pr.Head.SHA, pr.MergeCommitSHA} {
		if c = strings.ToLower(strings.TrimSpace(c)); len(c) >= 7 {
			commits = append(commits, c)
		}
	}
	runs, err := s.Repo.FindRunsForPR(ctx, projectID, commits, branch, strings.TrimSpace(pr.HTMLURL))
	if err != nil {
		slog.Error("github webhook: find runs", "project_id", projectID, "err", err)
		return GitHubResult{Status: http.StatusInternalServerError, Result: "can't look up the tickets"}
	}
	hits := s.traceHits(ctx, runs)
	var recs []MergeRecord
	var into []int
	if pr.Merged {
		// A PR merged into a ticket's integration branch isn't its merge to
		// main: the ticket stops holding its dependents and stays unmerged
		// until the integration branch is merged (AMP #2596).
		hits, into = s.releaseIntegrationHits(ctx, hits, strings.TrimSpace(pr.Base.Ref))
	}
	if pr.Merged {
		recs, err = s.markHitsMerged(ctx, githubPrincipal, hits, strings.ToLower(pr.MergeCommitSHA), domain.MergeSourceGitHub)
	} else {
		recs, err = s.markHitsAbandoned(ctx, githubPrincipal, hits, domain.MergeSourceGitHub)
	}
	if err != nil {
		slog.Error("github webhook: record merge", "project_id", projectID, "merged", pr.Merged, "err", err)
		return GitHubResult{Status: http.StatusInternalServerError, Result: "recording the merge failed; redeliver to retry"}
	}
	var changed, already []int
	for _, r := range recs {
		if r.AlreadyRecorded {
			already = append(already, r.TaskID)
		} else {
			changed = append(changed, r.TaskID)
		}
	}
	what := "merged"
	if !pr.Merged {
		what = "closed without merging"
	}
	changed = append(changed, into...)
	res := GitHubResult{Status: http.StatusOK, TaskIDs: changed}
	switch {
	case len(into) > 0 && len(recs) == 0:
		res.Result = "PR merged into integration branch " + pr.Base.Ref + ": released the tickets waiting on " + ticketList(into) + " (still unmerged)"
	case len(recs) == 0 && len(hits) == 0:
		res.Result = "PR " + what + ": no ticket in this project has its PR, branch or commits"
	case len(changed) == 0:
		res.Result = "PR " + what + ": nothing to change (" + ticketList(already) + " already recorded)"
		if len(already) == 0 {
			res.Result = "PR " + what + ": no matching ticket was waiting on its merge"
		}
	default:
		verb := "recorded merged on "
		if !pr.Merged {
			verb = "marked abandoned: "
		}
		res.Result = "PR " + what + ": " + verb + ticketList(changed)
		if len(already) > 0 {
			res.Result += " (" + ticketList(already) + " already recorded)"
		}
	}
	return res
}

func ticketList(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf("#%d", id)
	}
	return strings.Join(parts, ", ")
}

// releaseIntegrationHits splits off the hits whose ticket's integration
// branch is base: their work just landed where it belongs. A ticket waiting
// on that merge goes back to merge_state none (still unmerged), releasing
// its dependents. It returns the other hits, and the tickets it released.
func (s *Service) releaseIntegrationHits(ctx context.Context, hits []TraceHit, base string) ([]TraceHit, []int) {
	if base == "" {
		return hits, nil
	}
	var rest []TraceHit
	var released []int
	no := false
	for _, h := range hits {
		t := h.Task
		if t.IsHuman() || s.integrationBranch(ctx, &t) != base {
			rest = append(rest, h)
			continue
		}
		if t.MergeState != domain.MergeNeeded {
			continue // nothing held; leave it unmerged for the integration PR
		}
		if _, err := s.setMergeState(ctx, githubPrincipal, &t, actor.MsgSetMergeState{State: domain.MergeNone, Required: &no,
			From: []string{domain.MergeNeeded}, Detail: "Its PR was merged into its integration branch (" + base +
				"): the tickets that depend on it no longer wait; it stays unmerged until that branch is merged"}); err != nil {
			slog.Warn("github webhook: release integration merge", "task_id", t.ID, "err", err)
			continue
		}
		released = append(released, t.ID)
	}
	return rest, released
}
