package api

// Request attachment downloads over REST (epic 202, story 549, AMP #3963; build
// slice G3 of discoveries/team-inbox-approved-build-slices.md; approved product
// contract artifact 36 v2 §1 and decision D3).
//
//	POST /api/teams/{team_id}/requests/{request_id}/attachments/{attachment_id}/download
//	GET  /api/request-downloads/{token}                           serve the bytes
//
// This is the thin REST adapter over internal/service/request_downloads.go — the
// same methods an MCP tool or the UI's own client would call. It decides nothing: who
// may download, which files may be served, whether a grant is still good, expiry,
// single-use and revocation are all the service's, so the statuses a client branches
// on are the same wherever the call came from.
//
// It owns exactly the HTTP facts the service cannot:
//
//   - **the headers of a download**, and they are the contract's posture:
//     Content-Disposition: attachment (domain.RequestAttachmentDisposition, never
//     inline: an uploaded HTML or SVG file must never run with the viewer's session)
//     and X-Content-Type-Options: nosniff, so a polyglot file cannot be re-typed by
//     the browser. The type written out is the row's sniffed type, never the
//     client's; the file name comes from the row, never from the URL.
//   - **the filename encoding**. A name with a quote, a newline or a non-ASCII letter
//     would either break the header or be mangled, so the ASCII fallback is stripped
//     down and the real name travels in RFC 5987's filename*. The domain already
//     refuses names that are paths or contain control characters; this is the second
//     half of the same promise, at the layer that writes the header.
//   - **no caching**. Authorization is re-checked on every use and a URL is single-use,
//     so a shared cache must never hold the bytes: private, no-store.
//
// What this file deliberately does NOT do:
//
//   - no route without a principal. Both routes are registered through deniedMux like
//     every other /api route, so the auth verifier and the auth proxy in front of AMP
//     apply unchanged, the submit-only scope gate refuses a narrow credential, and a
//     403 is recorded as access.denied. There is no "signed URL means no sign-in" path:
//     the URL is an additional capability, never a replacement for the auth proxy.
//   - no public or absolute URL. The minted URL is a path on AMP's own origin; nothing
//     here reaches object storage, and there is no bucket address to leak.
//   - no token in any response but the mint's, and none in any log line. The handler
//     logs nothing about a download at all — the service's audit row already records
//     it, against ids rather than the credential.

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// requestAttachmentDownloadSuffix is the sub-path a mint is requested on, under a
// request's own route: POST /api/requests/{id}/attachments/{attachment_id}/download.
const requestAttachmentDownloadSuffix = "/attachments/"

// downloadGrantPath is where a minted token is used: service.RequestDownloadGrantPrefix
// + the token. Both are read from the service package so the mint and this handler
// cannot drift.

// requestDownloadMint handles one mint: it resolves the attachment, tells the service
// which request and tenant it belongs to, and answers the URL plus the metadata a panel
// shows.
//
// The tenant comes from the route's team segment (the same way the queue and detail routes
// name a request: a request id alone is not a lookup this product has, because an id-only
// read is the cross-tenant enumeration primitive the request surface exists without). The
// team segment resolves the org, and the service's tenant-scoped read refuses any pair the
// request does not actually have — so a path that names team 4 and request 7 from team 9 is
// the surface's single 404, not a read.
// requestTeamDownloadMint is the mint's route handler: it reads the team id from the
// route's own path value and hands the rest to requestDownloadMint.
func (h *RestHandler) requestTeamDownloadMint(w http.ResponseWriter, r *http.Request) {
	teamID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || teamID <= 0 {
		jsonErr(w, authz.RequestNotFound(), 404)
		return
	}
	h.requestDownloadMint(w, r, teamID)
}

func (h *RestHandler) requestDownloadMint(w http.ResponseWriter, r *http.Request, teamID int) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		jsonErr(w, errorf("POST required to mint a download link"), http.StatusMethodNotAllowed)
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}

	requestID, attachmentID, ok := downloadMintIDs(r.URL.Path)
	if !ok {
		jsonErr(w, authz.RequestNotFound(), 404)
		return
	}
	// The team's org, read from the team row. A team that does not exist is the
	// surface's not-found, exactly like a request that does not.
	team, err := h.svc.RequestTeamOrg(r.Context(), teamID)
	if err != nil {
		fail(w, err, 500)
		return
	}
	grant, err := h.svc.CreateRequestDownloadGrant(r.Context(), p, service.RequestDownloadGrantInput{
		RequestID:    requestID,
		AttachmentID: attachmentID,
		OrgID:        team.OrgID,
		TeamID:       teamID,
	})
	if err != nil {
		fail(w, err, 500)
		return
	}
	url := grant.URL
	if appURL := strings.TrimSuffix(h.svc.AppURL, "/"); appURL != "" {
		// Absolute only when the deployment knows its own public origin; otherwise the
		// path is relative to the page and still reaches AMP's own surface.
		url = appURL + grant.URL
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonOK(w, map[string]any{"download": map[string]any{
		"url":           url,
		"expires_at":    grant.ExpiresAt.UTC().Format(time.RFC3339),
		"attachment_id": grant.AttachmentID,
		"request_id":    grant.RequestID,
		"file_name":     grant.FileName,
		"size_bytes":    grant.SizeBytes,
	}})
}

// requestDownloadServe handles GET /api/request-downloads/{token}: it spends the
// grant (single use) and writes the bytes with the download posture.
func (h *RestHandler) requestDownloadServe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		jsonErr(w, errorf("GET required"), http.StatusMethodNotAllowed)
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	token := service.RequestDownloadGrantToken(r.URL.Path)
	if token == "" {
		jsonErr(w, authz.RequestNotFound(), 404)
		return
	}
	_, att, err := h.svc.ServeRequestDownload(r.Context(), p, token)
	if err != nil {
		fail(w, err, 500)
		return
	}
	writeRequestAttachmentDownload(w, r, att)
}

// writeRequestAttachmentDownload writes one attachment's bytes with the only headers
// an attachment may ever be served with. See the file header: attachment, never inline;
// nosniff; the row's sniffed type; no caching.
func writeRequestAttachmentDownload(w http.ResponseWriter, r *http.Request, att *domain.RequestAttachment) {
	hd := w.Header()
	hd.Set("Content-Type", att.ContentType)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Disposition", requestAttachmentDisposition(att.FileName))
	// Defense in depth under the disposition: even if a browser ignored the attachment
	// header (an old one, or a proxy rewriting headers), the document cannot run
	// scripts or forms with AMP's origin.
	hd.Set("Content-Security-Policy", "sandbox")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("Cross-Origin-Resource-Policy", "same-origin")
	// The bytes' digest as a strong validator: a resumed or conditioned retry gets the
	// same file or a 304, and never a stale body from a cache (there is none — see below).
	hd.Set("ETag", `"`+att.SHA256+`"`)
	// Authorization is re-checked on every use and a grant is single-use, so nothing
	// between AMP and the browser may keep the bytes.
	hd.Set("Cache-Control", "private, no-store")
	hd.Set("Content-Length", strconv.Itoa(len(att.Content)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(att.Content)
}

// requestAttachmentDisposition is the Content-Disposition value for one downloaded
// file, and the one place a file name reaches a header.
//
// domain.RequestAttachmentDisposition ("attachment") is the only disposition; the domain
// has already refused names that are paths or contain control characters, and this
// function does the remaining header work:
//
//   - the ASCII fallback (filename=) is stripped to printable ASCII with quotes,
//     backslashes, semicolons and commas removed, so it can never break out of its
//     quoted value or add a parameter;
//   - the real name travels in filename* (RFC 5987) percent-encoded as UTF-8, which is
//     what makes a name like "Rechnung März.pdf" arrive intact instead of mangled or,
//     worse, refused by a browser.
func requestAttachmentDisposition(fileName string) string {
	fallback := asciiFileNameFallback(fileName)
	value := domain.RequestAttachmentDisposition + `; filename="` + fallback + `"`
	if encoded := rfc5987Encode(fileName); encoded != "" && encoded != fallback {
		value += "; filename*=UTF-8''" + encoded
	}
	return value
}

// asciiFileNameFallback reduces a display name to something that is always safe inside a
// quoted Content-Disposition parameter: printable ASCII, with the characters that end or
// re-parse a quoted string removed. An empty result falls back to "download", because a
// header with an empty file name invites browsers to substitute the URL's last segment —
// which is the token.
func asciiFileNameFallback(name string) string {
	var b strings.Builder
	for _, c := range name {
		switch {
		case c < 0x20 || c == 0x7f:
			// Control characters: never in a header.
			continue
		case c > 0x7e:
			// Non-ASCII: the filename* parameter carries the real name.
			continue
		case c == '"' || c == '\\' || c == ';' || c == ',':
			// Would end or re-parse the quoted value, or add a parameter.
			b.WriteRune('_')
		default:
			b.WriteRune(c)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" || out == "." || out == ".." {
		return "download"
	}
	return out
}

// rfc5987Encode percent-encodes a name for the filename* parameter: UTF-8 bytes, with
// only the RFC 5987 attribute-char set left alone. An empty name encodes to "".
func rfc5987Encode(name string) string {
	if name == "" {
		return ""
	}
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '!', c == '#', c == '$', c == '&', c == '+', c == '-', c == '.', c == '^',
			c == '_', c == '`', c == '|', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}

// downloadMintIDs pulls the attachment id (and the request id it must belong to) out
// of a mint path: /api/teams/{team}/requests/{request}/attachments/{attachment}/download.
// The team id is the route's own segment, parsed where the route dispatches. A malformed
// id is the surface's not-found: an id that cannot exist names nothing.
func downloadMintIDs(path string) (requestID int, attachmentID int64, ok bool) {
	rest, found := strings.CutPrefix(path, "/api/teams/")
	if !found {
		return 0, 0, false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	// {team}/requests/{request}/attachments/{attachment}/download
	if len(parts) != 6 || parts[1] != "requests" || parts[3] != "attachments" || parts[5] != "download" {
		return 0, 0, false
	}
	rid, err := strconv.Atoi(parts[2])
	if err != nil || rid <= 0 {
		return 0, 0, false
	}
	aid, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil || aid <= 0 {
		return 0, 0, false
	}
	return rid, aid, true
}
