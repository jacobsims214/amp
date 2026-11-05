package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// Project GitHub credentials over REST (AMP #3155; the rules live in
// internal/service/github_credentials.go, the storage in
// internal/repository/github_credentials.go):
//
//	GET    /api/projects/:id/github-credential          project owners: whether one is attached, for which host, its metadata, and the last problem worth showing — never the token
//	PUT    /api/projects/:id/github-credential          project owners: attach a token, or replace the one attached for that host
//	POST   /api/projects/:id/github-credential/rotate   project owners: replace an attached token; refused when there is nothing to rotate
//	DELETE /api/projects/:id/github-credential          project owners: remove it, and with it every repository fact it read
//
// THE TOKEN IS WRITE-ONLY. It arrives in one body field, is sealed by the
// storage layer, and comes back out through nothing: the read shape
// (domain.GitHubCredential) has no token field, this file has no code path
// that opens one, and no response, error or log line here can carry it.
// Two consequences shape the code below:
//
//   - the token is accepted in the request body only. A token in a query
//     string would be written to every proxy and access log on the way in,
//     so it is refused with a 400 rather than quietly ignored;
//   - a body that fails to decode is answered with a fixed message rather
//     than json's own error. Today's decoder errors don't quote a value (only
//     a field path and an offending character), but the value here is a
//     token: nothing about a body that holds one is echoed back, and a
//     future error path that did would have to be added deliberately.
//
// Cross-site writes never reach this handler: RejectCrossSiteWrites
// (main.go, in front of every REST route) refuses a state-changing request
// a browser marks as another origin's or a sandboxed page's. Nothing here
// needs a token of its own for that.
//
// Every route is project-owner-only, enforced by the service on each call
// (AttachGitHubCredential / RotateGitHubCredential / DeleteGitHubCredential /
// GitHubCredential all require authz.Owner). The project check in
// projectSub (Member) runs first, so someone outside the project gets the
// same "not found" a missing project gets, and a member or team lead gets a
// 403 — never a quiet success.

// githubCredentialMaxBody bounds the request body of an attach or rotate. A
// GitHub token is at most 512 bytes and the label a few dozen; 8 KiB leaves
// room for a JSON envelope without letting a stray paste buffer anything.
const githubCredentialMaxBody = 8 << 10

// githubCredentialMaxLabel bounds a credential's label. The column has no
// length check of its own, so the API is where "a label is a name, not a
// document" is enforced.
const githubCredentialMaxLabel = 120

// githubCredentialStatus is a project's credential as a settings surface
// shows it: whether one is attached, its metadata, whether this server can
// hold one at all, and the last problem worth putting in front of a person.
//
// There is deliberately no token field, and nothing here could fill one: the
// metadata comes from domain.GitHubCredential, which has no token field
// either (AMP #3154). A test marshals this whole answer and asserts the
// token is not in it.
type githubCredentialStatus struct {
	Configured bool                     `json:"configured"`
	Host       string                   `json:"host"`
	Credential *domain.GitHubCredential `json:"credential,omitempty"`
	// Encrypted says whether this server has a key to seal a token with.
	// Without one, attaching and rotating are refused (400 naming
	// AMP_SECRETS_KEY) instead of storing a token in the clear.
	Encrypted bool `json:"encrypted"`
	// LastError is the problem that makes this credential unusable or
	// unconfigurable right now, as a class from a closed list — never a
	// message, and never anything derived from the token. "" means nothing
	// is wrong.
	//
	//	"no_encryption_key"  this server has no AMP_SECRETS_KEY, so a token
	//	                     can't be attached, and one already stored can't
	//	                     be opened by the facts reader.
	//
	// The classes the facts refresh will add (a token GitHub refuses, a rate
	// limit, a repository that can't be read) belong to #3156's coordinator
	// and #3158's projection, and land in this field then.
	LastError string `json:"last_error,omitempty"`
}

// lastErrorOf is the credential's current problem, as a class.
func lastErrorOf(info *domain.GitHubCredentialInfo) string {
	if info == nil || info.Encrypted {
		return ""
	}
	return "no_encryption_key"
}

// githubCredentialStatusOf projects the service's read into the settings
// answer.
func githubCredentialStatusOf(info *domain.GitHubCredentialInfo) githubCredentialStatus {
	st := githubCredentialStatus{
		Configured: info.Configured,
		Host:       info.Host,
		Credential: info.Credential,
		Encrypted:  info.Encrypted,
		LastError:  lastErrorOf(info),
	}
	return st
}

// projectGitHubCredential serves /api/projects/:id/github-credential[/rotate].
func (h *RestHandler) projectGitHubCredential(w http.ResponseWriter, r *http.Request, pr authz.Principal, projectID int, sub string) {
	ctx := r.Context()
	host, bad := checkCredentialHost(r.URL.Query().Get("host"))
	if bad != "" {
		jsonErr(w, errors.New(bad), 400)
		return
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		info, err := h.svc.GitHubCredential(ctx, pr, projectID, host)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, githubCredentialStatusOf(info))
	case sub == "" && r.Method == http.MethodPut:
		h.writeGitHubCredential(w, r, pr, projectID, host, false)
	case sub == "rotate" && r.Method == http.MethodPost:
		h.writeGitHubCredential(w, r, pr, projectID, host, true)
	case sub == "" && r.Method == http.MethodDelete:
		had, err := h.svc.DeleteGitHubCredential(ctx, pr, projectID, host)
		if err != nil {
			fail(w, err, 500)
			return
		}
		if host == "" {
			host = repository.GitHubCredentialHost
		}
		jsonOK(w, map[string]any{"deleted": had, "host": host})
	case sub == "":
		// The route exists; this method doesn't. 405 with Allow, as the
		// rest of the API answers a wrong method.
		w.Header().Set("Allow", "GET, PUT, DELETE")
		jsonErr(w, errorf("GET, PUT or DELETE required"), http.StatusMethodNotAllowed)
	case sub == "rotate":
		w.Header().Set("Allow", "POST")
		jsonErr(w, errorf("POST required"), http.StatusMethodNotAllowed)
	default:
		http.NotFound(w, r)
	}
}

// writeGitHubCredential attaches (rotate=false) or rotates (rotate=true) a
// project's token from the request body. queryHost is the host from the
// query string ("" for the default); a host in the body wins over it.
func (h *RestHandler) writeGitHubCredential(w http.ResponseWriter, r *http.Request, pr authz.Principal, projectID int, queryHost string, rotate bool) {
	if q := r.URL.Query(); q.Has("token") || q.Has("secret") {
		jsonErr(w, errorf("send the token in the request body, never in the URL: a query string is written to every proxy and access log on the way in"), 400)
		return
	}
	body, ok := readGitHubCredentialBody(w, r)
	if !ok {
		return
	}
	label := strings.TrimSpace(body.Label)
	if len(label) > githubCredentialMaxLabel {
		jsonErr(w, fmt.Errorf("a credential label is at most %d characters", githubCredentialMaxLabel), 400)
		return
	}
	host, bad := checkCredentialHost(body.Host)
	if bad != "" {
		jsonErr(w, errors.New(bad), 400)
		return
	}
	if host == "" {
		host = queryHost
	}

	var (
		info *domain.GitHubCredentialInfo
		err  error
	)
	if rotate {
		info, err = h.svc.RotateGitHubCredential(r.Context(), pr, projectID, host, label, body.Token)
	} else {
		info, err = h.svc.AttachGitHubCredential(r.Context(), pr, projectID, host, label, body.Token)
	}
	if err != nil {
		// 400 is the fallback: authz errors keep their 403/404, and the
		// service's own errors (a missing token, no encryption key, an
		// unusable token shape, a redacted storage failure) are all the
		// caller's to fix. None of them contains the token (#3154).
		fail(w, err, 400)
		return
	}
	jsonOK(w, githubCredentialStatusOf(info))
}

// githubCredentialBody is what an attach or rotate accepts. Token is the
// only secret; the rest is metadata.
type githubCredentialBody struct {
	Token string `json:"token"`
	Label string `json:"label"`
	Host  string `json:"host"`
}

// readGitHubCredentialBody decodes the body, answering the request itself
// when it can't. It never wraps the decoder's error: json quotes the input
// it failed on, and here that input is the token.
func readGitHubCredentialBody(w http.ResponseWriter, r *http.Request) (githubCredentialBody, bool) {
	var body githubCredentialBody
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, githubCredentialMaxBody)).Decode(&body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			jsonErr(w, fmt.Errorf("the request body is too large for a credential (at most %d bytes)", githubCredentialMaxBody), http.StatusRequestEntityTooLarge)
			return body, false
		}
		jsonErr(w, errorf(`want a JSON body {"token": "...", "label": "...", "host": "..."}`), 400)
		return body, false
	}
	return body, true
}

// checkCredentialHost trims the host and rejects what can't be one, so a
// pasted URL or a 500-character host is a clear 400 instead of a database
// constraint violation reported as "the database refused the write". It
// returns the host to store ("" means the default, github.com) or the
// message to answer with.
func checkCredentialHost(host string) (string, string) {
	h := strings.TrimSpace(host)
	switch {
	case h == "":
		return "", ""
	case len(h) > 253:
		return "", "a GitHub host is at most 253 characters: give the host name (github.com), not a URL"
	case strings.ContainsAny(h, "/ \t\r\n"):
		return "", "a GitHub host is a host name like github.com, not a URL or a path"
	}
	return h, ""
}
