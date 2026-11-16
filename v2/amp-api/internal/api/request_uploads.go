package api

// Team request attachments over REST: putting a requester's private file on a
// request, listing the files it has, pinning one past the retention window,
// withdrawing one, and asking for the short-lived download URL (epic 202, story
// 549, AMP #3974; build slices F5 and G2–G4 of
// discoveries/team-inbox-approved-build-slices.md; approved product contract
// artifact 36 v2 §1 and decision D3).
//
//	GET    /api/teams/:id/requests/:request/attachments                     the panel: every file and its state
//	POST   /api/teams/:id/requests/:request/attachments                     one upload (base64 bytes in the body)
//	POST   /api/teams/:id/requests/:request/attachments/:attachment/keep    pin it past the window, or unpin it
//	DELETE /api/teams/:id/requests/:request/attachments/:attachment         withdraw it
//
// (The download mint stays request_download.go's — POST .../attachments/:id/download
// and the URL it produces — and is reused unchanged, because minting a URL and
// serving bytes are one gate and must not have two adapters.)
//
// Every one of them is a thin adapter over the reviewed service method that already
// owns its decision:
//
//	panel    Service.ListRequestAttachmentsForDownload   (G3, #3963)
//	upload   Service.AddRequestAttachment                (G2, #3961)
//	keep     Service.KeepRequestAttachment               (G4, #3964)
//	delete   Service.DeleteRequestAttachment             (G4, #3964)
//
// They decide nothing: who may upload, keep, delete or download; how many files a
// request may carry and how big they may be; what a file's stored content type is;
// what its scan state is; whether the hold applies; when its retention window ends;
// and every audit row are the services' and the domain's, so a refusal is the same
// bytes on this surface as in the web UI.
//
// # The tenant comes from the path's team row, never from the body
//
// Every route here names its team as well as its request, and the org is read from
// that team's row (`RequestTeamOrg`). The service then re-verifies the (org, team)
// pair against the request's own columns, so a path naming team 4 and a request from
// team 9 is the surface's single 404 rather than a cross-tenant read or write. This
// is the same resolution the download mint makes, and it is why the routes cannot
// take a request id alone: an id-only lookup is the cross-tenant enumeration
// primitive the request surface exists without.
//
// The `:request` segment is the serial here, like every other request write (#3971's
// rule): a public reference on a write path is refused by name rather than searched
// for. The panel (a read) addresses the same segment the same way, because a
// requester who holds only their reference has their own `/api/requests` surface for
// what they may read, and resolving a reference here would put a second
// tenant-scoped lookup in an adapter.
//
// # Bytes travel in the body, and only as bytes
//
// An upload carries `content_base64` (and its `file_name`) — never a URL, a path or a
// place to fetch. There is deliberately no argument or field that names a location:
// a column or a parameter like that is how an upload starts being a request-forgery
// primitive (056's header makes the same argument for the table itself), and AMP
// never fetches a caller-supplied address. A payload that names one of the
// location-shaped keys is refused **by name** rather than ignored, because a dropped
// `url` leaves the caller believing a file was attached from a link AMP opened.
//
// Two bounds stand in front of the decoder:
//
//   - `maxRequestUploadBody` caps the whole body before it is read, so a caller
//     cannot make AMP allocate a payload the domain will refuse anyway. It is the
//     base64 length of one 25 MiB file plus room for the name and JSON framing, so a
//     file of exactly the per-file limit still fits.
//   - the per-file bound itself (`domain.MaxRequestAttachmentBytes`, 25 MiB) is
//     checked on the encoded length **before** decoding and again on the decoded
//     length, because the encoded length is what an attacker controls.
//
// # The panel is a read of one request's files, not of the request
//
// The listing answers each file's metadata plus the one claim a listing may make
// about it (ready, or the reason it is not), computed by the same function the mint
// and the serve gate use — so a panel's "waiting for a lead" and the URL's refusal
// cannot disagree. The bytes are never loaded by the listing.
//
// # What is deliberately NOT here
//
//   - no release of a held file. A restricted upload waits for a team lead, and
//     releasing one is a lead-only act with its own path; this file has no verb for
//     it and an upload can never set it (the input type has no field).
//   - no scan verdict. AMP ships no scanner in v1, and nothing here can set a scan
//     state: the upload service chooses it explicitly.
//   - no retention clock. The purge is a service job (#3964); a keep or a delete here
//     is a person's act and is audited as one.
//   - no artifact. An attachment is private and separate from an artifact (contract
//     §1): nothing here reuses artifact storage, artifact validation or artifact URLs.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// maxRequestUploadBody bounds an upload before the JSON decoder reads it.
//
// It is derived from the per-file limit rather than invented: one file at exactly
// `domain.MaxRequestAttachmentBytes` encodes to `base64.StdEncoding.EncodedLen` of
// that, and the remaining kilobyte is the name and the JSON framing. A body that
// reaches this bound cannot be a legal upload, so refusing it costs no valid caller
// anything — and the domain's own bound is still the real validation, checked after
// decoding.
var maxRequestUploadBody int64 = base64EncodedAttachmentLimit + 4<<10

// base64EncodedAttachmentLimit is the encoded length of one file at the per-file
// limit. It is a named variable because two readers use it (the body bound above and
// the pre-decode check below) and they must not drift from each other or from the
// domain's number; it is not a constant because base64.StdEncoding.EncodedLen is a
// function.
var base64EncodedAttachmentLimit int64 = int64(base64.StdEncoding.EncodedLen(domain.MaxRequestAttachmentBytes))

// requestUploadBody is what one upload may send: the file's display name and its
// bytes, base64-encoded.
//
// There is deliberately no content-type field, no scan-state field, no size field and
// no location field. The type is sniffed by the service from the bytes that arrived
// (so a client's Content-Type header cannot reach the column — the decoder does not
// even look at it), the scan state is the service's own decision, the size is the
// length of the decoded bytes, and a location is refused by name below. A payload
// naming one of them is answered rather than dropped.
type requestUploadBody struct {
	FileName      string `json:"file_name"`
	ContentBase64 string `json:"content_base64"`
}

// requestAttachmentFacts is one file as a listing answers it: the metadata a panel
// shows, never the bytes, plus what the gate would say about serving it right now.
//
// The fields are the domain row's display half — name, sniffed type, size, digest,
// scan state, uploader, time, retention and hold — because every one of them is
// something the approving triager or the requester who sent it is entitled to see.
// There is no `content` key: the repository's listing read does not load bytes, and
// the download is a separate, single-use capability.
type requestAttachmentFacts struct {
	ID          int64                                  `json:"id"`
	RequestID   int                                    `json:"request_id"`
	FileName    string                                 `json:"file_name"`
	ContentType string                                 `json:"content_type"`
	SizeBytes   int64                                  `json:"size_bytes"`
	SHA256      string                                 `json:"sha256"`
	ScanState   domain.RequestScanState                `json:"scan_state"`
	UploadedBy  *int                                   `json:"uploaded_by"`
	CreatedAt   time.Time                              `json:"created_at"`
	Keep        bool                                   `json:"keep"`
	KeptBy      *int                                   `json:"kept_by"`
	KeptAt      *time.Time                             `json:"kept_at"`
	ExpiresAt   *time.Time                             `json:"expires_at"`
	ReleasedAt  *time.Time                             `json:"released_at"`
	ReleasedBy  *int                                   `json:"released_by"`
	Held        bool                                   `json:"held"`
	State       service.RequestAttachmentDownloadState `json:"state"`
}

// requestAttachmentPanelAnswer is the listing: the request's identity and its files.
//
// The count is beside the list so a caller rendering "3 of 5 files" does not have to
// count, and the bounds travel with it so the same caller can say why a sixth upload
// would be refused without guessing AMP's numbers.
type requestAttachmentPanelAnswer struct {
	RequestID int                      `json:"request_id"`
	PublicRef string                   `json:"public_ref"`
	Items     []requestAttachmentFacts `json:"items"`
	Count     int                      `json:"count"`
	Limits    requestAttachmentLimits  `json:"limits"`
}

// requestAttachmentLimits is what a panel tells a client about AMP's upload bounds:
// per file, per request, and the retention window a keep pins past.
type requestAttachmentLimits struct {
	MaxFileBytes  int `json:"max_file_bytes"`
	MaxFiles      int `json:"max_files"`
	MaxTotalBytes int `json:"max_total_bytes"`
	RetentionDays int `json:"retention_days"`
}

// requestUploadAnswer is one upload's answer: the stored file and whether it is held.
//
// `held` is stated at the top level as well as inside the file's state because it is
// the one fact a UI branches on immediately ("waiting for a team lead" instead of
// "attached"), and a caller should not have to reach into a nested object to learn
// that its file is not readable yet.
type requestUploadAnswer struct {
	Attachment requestAttachmentFacts `json:"attachment"`
	Held       bool                   `json:"held"`
	// Next is one sentence about what the caller can and cannot do next, mirroring
	// the MCP tool's own note: an upload grants nothing about reading the file back,
	// and a held file waits for a lead.
	Next string `json:"next"`
}

// requestAttachmentKeepAnswer is one keep or unkeep's answer: the file's retention
// columns as they now stand.
//
// It carries the whole row rather than just the flag because a keep and an unkeep
// write three different columns (keep, kept_by/kept_at, expires_at) and the expiry an
// unkeep computes from the request's close moment is not something a client could
// derive. There is no `changed` flag: unlike an attach, a keep always writes — an
// unkeep of a file whose window has already passed still stamps the expiry, and
// pinning an already-pinned file re-stamps who and when — so a flag saying "nothing
// happened" would be a claim this path cannot make.
type requestAttachmentKeepAnswer struct {
	Attachment requestAttachmentFacts `json:"attachment"`
	Keep       bool                   `json:"keep"`
}

// requestAttachmentDeleteAnswer is one delete's answer: which row this call removed.
//
// A retry of a delete that already happened is the surface's 404, not a 200 with a
// false: the service reads the row before deleting it (it needs the stored facts for
// the audit entry), so a second delete finds nothing to name and answers
// `request.not_found` — the same bytes a file that never existed gets, which is the
// honest answer to "delete attachment 91" when there is no attachment 91. `Deleted`
// is therefore true on every 200 this route writes, and it is reported anyway
// because the answer should say what it did rather than imply it.
type requestAttachmentDeleteAnswer struct {
	Deleted      bool  `json:"deleted"`
	AttachmentID int64 `json:"attachment_id"`
}

// remoteFetchFields are the body fields a caller uses when it believes an upload can
// name where the bytes live. Nothing here fetches: the bytes arrived in this request
// or they did not arrive.
//
// The list is internal/mcp/request_planning.go's remoteFetchArgs, restated for the
// body rather than the arguments — a caller must not be able to do over REST what the
// tool refuses over MCP. `file_name` is deliberately absent: it is a display name,
// and the domain refuses one that is a path (see NormalizeRequestAttachmentFileName).
var remoteFetchFields = map[string]string{
	"url": "an upload's bytes travel inside this request", "uri": "an upload's bytes travel inside this request",
	"href": "an upload's bytes travel inside this request", "link": "an upload's bytes travel inside this request",
	"source_url": "an upload's bytes travel inside this request", "content_url": "an upload's bytes travel inside this request",
	"file_url": "an upload's bytes travel inside this request", "download_url": "an upload's bytes travel inside this request",
	"fetch": "an upload's bytes travel inside this request", "fetch_url": "an upload's bytes travel inside this request",
	"remote": "an upload's bytes travel inside this request", "remote_url": "an upload's bytes travel inside this request",
	"path": "an upload's bytes travel inside this request", "file_path": "an upload's bytes travel inside this request",
	"content_type": "AMP sniffs a file's type from the bytes it received, so a content type is not an upload field",
	"scan_state":   "a scan verdict is AMP's to record, never a client's to claim",
	"size_bytes":   "a file's size is the length of the bytes it arrived with, not a field",
}

// ---- The panel ----

// teamRequestAttachmentPanel serves GET /api/teams/:id/requests/:request/attachments:
// the file panel.
func (h *RestHandler) teamRequestAttachmentPanel(w http.ResponseWriter, r *http.Request) {
	teamID, requestID, ok := requestLifecycleTarget(w, r)
	if !ok {
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	team, err := h.svc.RequestTeamOrg(r.Context(), teamID)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	req, entries, err := h.svc.ListRequestAttachmentsForDownload(r.Context(), p, requestID, team.OrgID, team.ID)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	if req == nil {
		fail(w, errorf("the attachment panel answered no request"), http.StatusInternalServerError)
		return
	}
	out := requestAttachmentPanelAnswer{
		RequestID: req.ID,
		PublicRef: req.PublicRef,
		Items:     make([]requestAttachmentFacts, 0, len(entries)),
		Count:     len(entries),
		Limits: requestAttachmentLimits{
			MaxFileBytes:  domain.MaxRequestAttachmentBytes,
			MaxFiles:      domain.MaxRequestAttachments,
			MaxTotalBytes: domain.MaxRequestAttachmentsTotalBytes,
			RetentionDays: domain.RequestAttachmentRetentionDays,
		},
	}
	for _, e := range entries {
		if e.Attachment == nil {
			continue
		}
		out.Items = append(out.Items, requestAttachmentView(e.Attachment, e.State))
	}
	jsonOK(w, out)
}

// ---- The upload ----

// teamRequestAttachmentUpload serves POST on the same path: one file onto an existing
// request.
//
// The body is read and bounded before the service is called, so a payload that cannot
// be an upload costs no storage and no audit row. 201 when this call stored the file:
// an upload is not idempotent (there is no replay token, and a second identical upload
// is a second file, bounded only by the request's count) and answering 200 would claim
// a convergence that does not exist.
func (h *RestHandler) teamRequestAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	teamID, requestID, ok := requestLifecycleTarget(w, r)
	if !ok {
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	body, ok := decodeRequestUpload(w, r)
	if !ok {
		return
	}
	content, err := decodeAttachmentBytes(body.ContentBase64)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	team, err := h.svc.RequestTeamOrg(r.Context(), teamID)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	att, err := h.svc.AddRequestAttachment(r.Context(), p, service.AddRequestAttachmentInput{
		RequestID: requestID,
		OrgID:     team.OrgID,
		TeamID:    team.ID,
		FileName:  body.FileName,
		Content:   content,
	})
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	if att == nil {
		fail(w, errorf("the upload answered no attachment"), http.StatusInternalServerError)
		return
	}
	// The stored row is metadata only (the repository's insert returns no bytes), so
	// this answer cannot be turned into a read path.
	facts := requestAttachmentView(att, service.RequestAttachmentDownloadStateAt(att, time.Now()))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, requestUploadAnswer{
		Attachment: facts,
		Held:       facts.Held,
		Next:       requestUploadNext(facts),
	})
}

// requestUploadNext is the one sentence an upload's answer carries about what
// happens next. It is the adapter's copy of the MCP tool's note, deliberately worded
// the same way: the two surfaces must not teach different rules.
func requestUploadNext(facts requestAttachmentFacts) string {
	if facts.Held {
		return "Stored, and HELD: an archive, an executable or a name that says code waits for a team lead to release it before " +
			"anyone can download it. The requester can be told that is why it is not readable yet."
	}
	return "Stored as a private file on the request; its content type is what AMP sniffed from the bytes. Reading it back is a " +
		"separate decision: a download is a short-lived single-use link, and having uploaded a file grants nothing."
}

// ---- Keep and delete ----

// teamRequestAttachmentKeep serves POST .../attachments/:attachment/keep: pin a file
// past its retention window (`keep: true`) or let the request's own clock apply again
// (`keep: false`).
//
// Pinning and unpinning are the same decision applied twice, so they are one route
// and one flag: a separate `unkeep` path would be a second name for it.
func (h *RestHandler) teamRequestAttachmentKeep(w http.ResponseWriter, r *http.Request) {
	in, p, ok := h.attachmentRetentionTarget(w, r)
	if !ok {
		return
	}
	var body requestAttachmentKeepBody
	if err := decodeRequestMutation(w, r, &body, "attachment keep"); err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	if body.Keep == nil {
		// No default is invented: `{}` would otherwise mean "unkeep", which is the
		// destructive half, and a client that forgot the field would silently start a
		// file's retention clock.
		fail(w, authz.RequestInvalid("keep is required: send {\"keep\": true} to pin the file past its window, or "+
			"{\"keep\": false} to let the request's own clock apply again"), http.StatusBadRequest)
		return
	}
	after, err := h.svc.KeepRequestAttachment(r.Context(), p, in, *body.Keep)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	if after == nil {
		fail(w, errorf("the keep answered no attachment"), http.StatusInternalServerError)
		return
	}
	facts := requestAttachmentView(after, service.RequestAttachmentDownloadStateAt(after, time.Now()))
	jsonOK(w, requestAttachmentKeepAnswer{Attachment: facts, Keep: after.Keep})
}

// requestAttachmentKeepBody is what a keep may send: the flag and nothing else.
//
// There is deliberately no expires_at, no kept_by and no id: the window is the
// request's (D3's rule, computed from its close moment by the repository), the person
// is the authenticated caller, and the file is the path's.
type requestAttachmentKeepBody struct {
	Keep *bool `json:"keep"`
}

// teamRequestAttachmentDelete serves DELETE .../attachments/:attachment: withdraw one
// file, and with it its bytes (056 puts the bytes in the row, so the delete is the
// physical cleanup).
func (h *RestHandler) teamRequestAttachmentDelete(w http.ResponseWriter, r *http.Request) {
	in, p, ok := h.attachmentRetentionTarget(w, r)
	if !ok {
		return
	}
	if err := decodeRequestMutation(w, r, &struct{}{}, "attachment delete"); err != nil {
		// A DELETE carries no fields. A body that names one is refused rather than
		// ignored, and an empty body is the normal shape.
		fail(w, err, http.StatusBadRequest)
		return
	}
	changed, err := h.svc.DeleteRequestAttachment(r.Context(), p, in)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, requestAttachmentDeleteAnswer{Deleted: changed, AttachmentID: in.AttachmentID})
}

// attachmentRetentionTarget resolves a keep or delete's target: the team, the request
// serial, the attachment id and the caller.
//
// It is one function so the two acts cannot resolve their path differently — the
// refusal for a malformed segment is the same on both, and a path that names team 4
// and request 7 from team 9 is the same not-found for either.
func (h *RestHandler) attachmentRetentionTarget(w http.ResponseWriter, r *http.Request) (service.RequestAttachmentRetentionInput, authz.Principal, bool) {
	teamID, requestID, ok := requestLifecycleTarget(w, r)
	if !ok {
		return service.RequestAttachmentRetentionInput{}, authz.Principal{}, false
	}
	p, ok := h.principal(w, r)
	if !ok {
		return service.RequestAttachmentRetentionInput{}, authz.Principal{}, false
	}
	attachmentID, err := requestAttachmentTarget(r)
	if err != nil {
		fail(w, err, http.StatusBadRequest)
		return service.RequestAttachmentRetentionInput{}, authz.Principal{}, false
	}
	team, err := h.svc.RequestTeamOrg(r.Context(), teamID)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return service.RequestAttachmentRetentionInput{}, authz.Principal{}, false
	}
	return service.RequestAttachmentRetentionInput{
		RequestID:    requestID,
		OrgID:        team.OrgID,
		TeamID:       team.ID,
		AttachmentID: attachmentID,
	}, p, true
}

// requestAttachmentTarget reads the :attachment path segment: one file's id, and
// nothing else.
//
// A value that is not a positive whole number is the caller's mistake and says so,
// rather than being answered 404: an id that could not be an id names nothing, and a
// 404 for it would claim a file might have existed.
func requestAttachmentTarget(r *http.Request) (int64, error) {
	segment := strings.TrimSpace(r.PathValue("attachment"))
	if segment == "" {
		return 0, authz.RequestInvalid("name the attachment by its id")
	}
	id, err := strconv.ParseInt(segment, 10, 64)
	if err != nil || id <= 0 {
		return 0, authz.RequestInvalid("an attachment is addressed by its id, a whole number (got %q)", segment)
	}
	return id, nil
}

// ---- Decoding ----

// decodeRequestUpload reads one upload: bounded before it is read, strict about
// unknown fields, and explicit about the two kinds of field a caller might send that
// this path refuses — a location to fetch and a claim about the file.
func decodeRequestUpload(w http.ResponseWriter, r *http.Request) (requestUploadBody, bool) {
	var body requestUploadBody
	err := decodeBoundedStrictJSON(w, r, &body, maxRequestUploadBody, "request upload")
	if err != nil {
		if field := unknownField(err); field != "" {
			if why, refused := remoteFetchFields[field]; refused {
				fail(w, authz.RequestInvalid("%q is not an upload field and is refused rather than ignored: %s, as "+
					"content_base64, and AMP never fetches a location a caller names — a URL here would make an upload a "+
					"request-forgery primitive and a way to pull bytes from somewhere AMP cannot vouch for", field, why),
					http.StatusBadRequest)
				return body, false
			}
		}
		fail(w, err, http.StatusBadRequest)
		return body, false
	}
	return body, true
}

// decodeBoundedStrictJSON is one body's read with the three transport refusals every
// request write here shares: a bound applied before the bytes are buffered, unknown
// fields refused rather than dropped, and exactly one JSON value.
//
// It differs from decodeRequestMutation in the bound it takes, because an upload's
// body is measured in megabytes rather than kilobytes and its refusal must name that
// number. Everything else — the empty body being "no fields", the second-value check,
// the `MaxBytesError` classification — is the same code path, so the two cannot
// drift.
func decodeBoundedStrictJSON(w http.ResponseWriter, r *http.Request, into any, limit int64, what string) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return oversizeUploadRefusal(limit)
		}
		return authz.RequestInvalid("the body is not a %s: %s", what, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return oversizeUploadRefusal(limit)
		}
		return authz.RequestInvalid("the body holds more than one JSON value: send one")
	}
	return nil
}

// oversizeUploadRefusal is the one refusal for an upload body over the transport's
// bound. It names the domain's per-file limit (the number a person can act on) rather
// than the transport's, because the transport's is derived from it and the caller's
// mistake is always "the file is too big".
func oversizeUploadRefusal(limit int64) error {
	return authz.RequestInvalid("the body is over %d bytes, and one file may be at most the %d-byte per-file limit "+
		"(%d MiB) once base64-encoded: send a smaller file, or ask the team for a different format",
		limit, domain.MaxRequestAttachmentBytes, domain.MaxRequestAttachmentBytes>>20)
}

// decodeAttachmentBytes turns the base64 field into the file's bytes, refusing an
// over-size payload BEFORE it is decoded and an unreadable one instead of guessing.
//
// The order matters and is the MCP reader's: the encoded length is checked first so a
// caller cannot make AMP allocate a payload the domain will refuse anyway, the decoded
// length is checked again because that is the real bound, and an unreadable payload
// names the encoding — never the content, because a base64 string is a file and an
// error message is not a place to echo one.
func decodeAttachmentBytes(raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, authz.RequestInvalid("content_base64 is empty: an attachment needs bytes, and an empty file has nothing to attach")
	}
	if int64(len(trimmed)) > base64EncodedAttachmentLimit {
		return nil, authz.RequestInvalid("content_base64 holds %d base64 characters, which is more than the per-file limit of "+
			"%d bytes (%d MiB) allows (%d characters): send a smaller file, or ask the team for a different format",
			len(trimmed), domain.MaxRequestAttachmentBytes, domain.MaxRequestAttachmentBytes>>20, base64EncodedAttachmentLimit)
	}
	content, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, authz.RequestInvalid("content_base64 is not valid standard base64 (RFC 4648, no line breaks): encode the " +
			"file's bytes with base64.StdEncoding and send them on one line")
	}
	if len(content) > domain.MaxRequestAttachmentBytes {
		return nil, authz.RequestInvalid("content_base64 decodes to %d bytes, over the %d-byte per-file limit (%d MiB): send a "+
			"smaller file", len(content), domain.MaxRequestAttachmentBytes, domain.MaxRequestAttachmentBytes>>20)
	}
	return content, nil
}

// requestAttachmentView projects a stored attachment and the gate's claim about it
// onto the answer's whitelist.
//
// `Held` and `State` are both reported: the hold says "a lead must release this
// before anyone can read it" and the state says "may these bytes be served at all
// right now" (scan, hold, purge). They overlap on purpose — a held file's state's
// refusal is `held_for_release` — and a UI that shows only one of them would have to
// re-derive the other.
func requestAttachmentView(att *domain.RequestAttachment, state service.RequestAttachmentDownloadState) requestAttachmentFacts {
	return requestAttachmentFacts{
		ID:          att.ID,
		RequestID:   att.RequestID,
		FileName:    att.FileName,
		ContentType: att.ContentType,
		SizeBytes:   att.SizeBytes,
		SHA256:      att.SHA256,
		ScanState:   att.ScanState,
		UploadedBy:  att.UploadedBy,
		CreatedAt:   att.CreatedAt,
		Keep:        att.Keep,
		KeptBy:      att.KeptBy,
		KeptAt:      att.KeptAt,
		ExpiresAt:   att.ExpiresAt,
		ReleasedAt:  att.ReleasedAt,
		ReleasedBy:  att.ReleasedBy,
		Held:        att.Held(),
		State:       state,
	}
}
