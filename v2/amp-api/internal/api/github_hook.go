package api

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/reqctx"
	"github.com/simstech/amp-api/internal/service"
)

// Inbound GitHub webhook (AMP #2479; rules in internal/service/github_hook.go):
//
//	POST   /api/hooks/github/:project_id       GitHub's deliveries. No AMP session:
//	                                          X-Hub-Signature-256 is the only auth
//	GET    /api/projects/:id/github-hook        project owners: URL, status, recent deliveries
//	POST   /api/projects/:id/github-hook/secret project owners: generate or rotate; the secret is returned once
//	DELETE /api/projects/:id/github-hook        project owners: turn it off
//
// The public route must reach amp-api without a login session: main leaves
// it out of the auth middleware (GitHubHookPrefix), and the Helm chart's
// Envoy route for it turns ext_authz off (envoy.inboundHooks).

// GitHubHookPrefix is the public route's path prefix.
const GitHubHookPrefix = "/api/hooks/github/"

// Limits on the public route. A pull_request payload is tens of kilobytes;
// GitHub retries nothing on its own, so a refused delivery is redelivered
// by hand from GitHub's webhook page.
const (
	githubHookMaxBody   = 2 << 20 // bytes
	githubHookPerMinute = 300     // requests per client address per minute
)

// hookLimiter is a fixed-window counter per client address (per replica).
type hookLimiter struct {
	mu     sync.Mutex
	window time.Time
	counts map[string]int
	limit  int
}

func (l *hookLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w := now.Truncate(time.Minute); !w.Equal(l.window) || l.counts == nil {
		l.window, l.counts = w, map[string]int{}
	}
	l.counts[key]++
	return l.counts[key] <= l.limit
}

var githubLimiter = &hookLimiter{limit: githubHookPerMinute}

// clientKey is the address a request is rate-limited by: the one the
// request context resolved (Envoy's view of the client), else the peer.
func clientKey(r *http.Request) string {
	if info, ok := reqctx.FromContext(r.Context()); ok && info.SourceIP.IsValid() {
		return info.SourceIP.String()
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// githubHook serves POST /api/hooks/github/:project_id.
func (h *RestHandler) githubHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		jsonErr(w, errorf("POST required"), http.StatusMethodNotAllowed)
		return
	}
	projectID, err := strconv.Atoi(strings.Trim(strings.TrimPrefix(r.URL.Path, GitHubHookPrefix), "/"))
	if err != nil || projectID <= 0 {
		jsonErr(w, errorf("no GitHub webhook is set up for this project"), http.StatusNotFound)
		return
	}
	if !githubLimiter.allow(clientKey(r), time.Now()) {
		w.Header().Set("Retry-After", "60")
		jsonErr(w, errorf("too many requests"), http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, githubHookMaxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			jsonErr(w, errorf("payload too large"), http.StatusRequestEntityTooLarge)
			return
		}
		jsonErr(w, errorf("can't read the body"), http.StatusBadRequest)
		return
	}
	res := h.svc.HandleGitHubDelivery(r.Context(), service.GitHubDelivery{
		ProjectID:  projectID,
		Event:      r.Header.Get("X-GitHub-Event"),
		DeliveryID: r.Header.Get("X-GitHub-Delivery"),
		Signature:  r.Header.Get("X-Hub-Signature-256"),
		Body:       body,
	})
	if res.Status == http.StatusNoContent {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if res.Status >= 400 {
		jsonErr(w, errorf(res.Result), res.Status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(res.Status)
	_ = json.NewEncoder(w).Encode(res)
}

// projectGitHubHook serves /api/projects/:id/github-hook[/secret].
func (h *RestHandler) projectGitHubHook(w http.ResponseWriter, r *http.Request, pr authz.Principal, projectID int, sub string) {
	ctx := r.Context()
	switch {
	case sub == "" && r.Method == http.MethodGet:
		info, err := h.svc.GitHubHook(ctx, pr, projectID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, info)
	case sub == "secret" && r.Method == http.MethodPost:
		info, err := h.svc.RotateGitHubHookSecret(ctx, pr, projectID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, info)
	case sub == "" && r.Method == http.MethodDelete:
		had, err := h.svc.DeleteGitHubHook(ctx, pr, projectID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]any{"deleted": had})
	default:
		http.NotFound(w, r)
	}
}
