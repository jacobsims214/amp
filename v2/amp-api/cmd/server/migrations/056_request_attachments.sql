-- Team requests: private requester uploads (epic 202, story 521, AMP #3759;
-- approved product contract = artifact 36 v2 §1, decision D3; build slice G1 of
-- discoveries/team-inbox-approved-build-slices.md).
--
-- 053 added the request record, 054 its discussion and history, 055 its
-- lineage. This migration adds the last thing that hangs off a request: the
-- files the requester sent with it.
--
-- AN ATTACHMENT IS NOT AN ARTIFACT, AND DOES NOT REUSE ONE. The contract says
-- attachments are "private and separate from artifacts": an artifact is a
-- reviewable deliverable an agent publishes to a project for people to look at,
-- an attachment is a requester's private file on one request, visible to that
-- requester and the team it was sent to. They differ on every axis that
-- matters — audience, retention (D3 vs artifact_versions' 45-day expiry), the
-- scan gate, the hold, and the download posture — so this is its own table and
-- never a flag on artifact_versions, never a row in artifacts.
--
-- Consequently there is NO public URL and NOTHING TO FETCH. Bytes live in this
-- row's `content` column and are only ever reached through the authorized
-- download path (G3), which mints a short-lived signed URL and serves them with
-- Content-Disposition: attachment (domain.RequestAttachmentDisposition). The
-- table has no `url`/`uri`/`storage_key` column at all, because a column like
-- that is how an upload starts being fetched from somewhere AMP does not
-- control (an SSRF and a leak in one). The catalogue test asserts the absence.
--
-- The row is the domain record. Every column here is a field of
-- domain.RequestAttachment and nothing else:
--   file_name, content_type (the SNIFFED type, never the client's header),
--   size_bytes, sha256, scan_state, content, uploaded_by, created_at,
--   keep/kept_by/kept_at/expires_at (D3), released_at/released_by (the hold).
--
-- WHAT THE DATABASE ENFORCES (single-row invariants, whatever wrote the row):
--   * the request exists and owns the attachment — the FK is ON DELETE CASCADE,
--     see DELETION below;
--   * content_type is one of the types Go's http.DetectContentType actually
--     returns (domain.requestAttachmentTypeKinds). A client cannot choose a
--     stored type the sniffer could not have produced, and a type with
--     parameters (`text/plain; charset=utf-8`) is refused, because the domain
--     drops the parameter before storing;
--   * size_bytes is positive, no larger than domain.MaxRequestAttachmentBytes
--     (25 MiB), and EQUAL to octet_length(content): a row cannot claim a size
--     its bytes do not have. That is the cheapest way to stop the classic
--     upload bug — storing the length the client declared instead of the length
--     that arrived — and it stays cheap because it is a length, not a hash;
--   * sha256 is 64 lower-case hex characters (the download's ETag; the domain
--     lower-cases it, so the schema stores only that form). Its agreement with
--     the bytes is NOT re-proved here on purpose: a CHECK is evaluated on every
--     UPDATE of the row, so rehashing up to 25 MiB would run on every metadata
--     write (each expires_at stamp when a request closes, every keep, every
--     release) for information the upload path (G2) already has — it computes
--     the digest from the exact bytes it inserts — and `content` is never
--     updated after the insert. (The size equality below does run on those
--     updates; it is a length, and those three rewrites per file are the whole
--     of a row's post-insert life.)
--   * file_name is a display name, not a path and not a header injection:
--     non-blank, no "/" or "\", not "." or "..", no control characters, at most
--     domain.MaxRequestAttachmentNameLen (255) characters;
--   * scan_state is the contract's closed set (pending, clean, infected,
--     unsupported) and defaults to 'unsupported', never to 'clean'. AMP ships
--     no malware scanner in v1 (the contract ships the interface, not the
--     service), so a write path that forgets a state must land on "no verdict".
--     Defaulting to 'clean' would be a security claim the product cannot back,
--     and defaulting to 'pending' would hold every download behind a scanner
--     that never runs — see domain.NormalizeRequestScanState;
--   * keep/release columns are one-way implications, never biconditionals:
--     keep requires kept_at, kept_by requires kept_at, released_by requires
--     released_at — but not the reverse, because every user FK is
--     ON DELETE SET NULL and a row keeps the moment after the person's row is
--     gone (the 053/054 rule);
--   * a release is evidence a hold existed: released_at is only storable on a
--     file that is actually held — a restricted sniffed type (archive or
--     executable) or a code-like name — exactly
--     domain.RequestAttachment.Validate's rule. The two sides are the schema
--     twin of domain.RequestAttachmentRestricted and
--     domain.RequestAttachmentHeldByName, and the test reads both out of the
--     domain so they cannot drift silently.
--
-- WHAT IS NOT ENFORCED HERE, BY DESIGN:
--   * The count and total bounds (domain.MaxRequestAttachments = 5,
--     MaxRequestAttachmentsTotalBytes = 60 MiB) are properties of the REQUEST,
--     not of one row. A CHECK cannot count sibling rows without a trigger, so
--     intake/service validation (domain.ValidateRequestAttachmentSet) owns them.
--     The per-file bound is here because it is one row's promise.
--   * "An attachment on an active request has no expiry" is also cross-row: it
--     reads team_requests.closed_at. expires_at is stamped by the service when a
--     request closes and cleared while it is open (domain.RequestAttachmentExpiresAt
--     is the pure function), so the rule lives there.
--   * Whether a scan is honest (a `clean` state with no scanner behind it) is
--     not something a string CHECK can see; domain.RequestScanState.SaysClean
--     and RequestAttachmentDownloadRefusal are the one place that decides.
--   * `held` is deliberately NOT a stored column. It is derived from the sniffed
--     type, the name and released_at (domain.RequestAttachment.Held). A stored
--     flag could disagree with the type it was derived from and would let a
--     surface serve a binary as ordinary; the only stored fact is the release.
--
-- DELETION — the approved semantics, and no orphan bytes. Attachments are part
-- of the request's record, so they follow team_requests' lifecycle: deleting the
-- request (or its team, or its org, by cascade) deletes its attachments. The
-- bytes live in the row, so deleting the row deletes the bytes — there is no
-- second store to leak behind (the catalogue test asserts the single bytea
-- column and proves the space is actually reclaimed). Deleting a USER never
-- deletes an attachment: uploaded_by/kept_by/released_by are ON DELETE SET NULL,
-- and the request keeps the requester's contact snapshot, so "the requester sent
-- this" stays readable. The retention job (G4) deletes expired, unkept rows with
-- `DELETE ... WHERE NOT keep AND expires_at <= NOW()`, which this migration's
-- expiry index serves.
--
-- Additive and idempotent: one new table and its indexes, nothing existing is
-- altered or re-indexed. Re-running this file by hand is a no-op.

CREATE TABLE IF NOT EXISTS request_attachments (
    id           BIGSERIAL PRIMARY KEY,
    -- The request this file was sent with. It dies with the request (see
    -- DELETION): an attachment has no life of its own, and no bytes may outlive
    -- the record they belong to.
    request_id   INT NOT NULL REFERENCES team_requests(id) ON DELETE CASCADE,

    -- The name the requester gave it, for display only. It never decides the
    -- content type (at most it can ESCALATE a hold, see the release CHECK) and
    -- is refused as a path so no later layer has to think about zip-slip or a
    -- Content-Disposition that breaks out of its header.
    file_name    TEXT NOT NULL
        CONSTRAINT request_attachments_file_name_not_blank CHECK (btrim(file_name) <> '')
        CONSTRAINT request_attachments_file_name_no_slash CHECK (position('/' in file_name) = 0)
        CONSTRAINT request_attachments_file_name_no_backslash CHECK (position('\' in file_name) = 0)
        CONSTRAINT request_attachments_file_name_not_dot CHECK (btrim(file_name) NOT IN ('.', '..'))
        CONSTRAINT request_attachments_file_name_no_control CHECK (file_name !~ '[[:cntrl:]]')
        CONSTRAINT request_attachments_file_name_length CHECK (char_length(file_name) <= 255),

    -- The type AMP sniffed from the bytes, never the client's Content-Type
    -- header (contract §1). Every value is a type http.DetectContentType really
    -- returns — see domain.requestAttachmentTypeKinds and its sniffer test — so
    -- a client-declared type a sniffer never produces (application/json,
    -- text/csv, image/svg+xml, video/mp4 …) cannot reach this column. An
    -- office file sniffs as application/zip and an unnamed binary as
    -- application/octet-stream: both are stored, and both are then HELD.
    content_type TEXT NOT NULL
        CONSTRAINT request_attachments_content_type CHECK (content_type IN (
            'application/pdf',
            'text/html',
            'text/xml',
            'text/plain',
            'image/png',
            'image/jpeg',
            'image/gif',
            'image/webp',
            'image/bmp',
            'application/zip',
            'application/x-gzip',
            'application/octet-stream',
            'application/wasm')),

    -- How big the bytes are, capped per file and required to be the truth about
    -- the row's own bytes (see the header). INT is plenty: the cap is 25 MiB.
    size_bytes   INT NOT NULL
        CONSTRAINT request_attachments_size_positive CHECK (size_bytes > 0)
        CONSTRAINT request_attachments_size_within_limit CHECK (size_bytes <= 26214400),

    -- The bytes' digest, and the download's ETag (as for artifact_versions).
    -- Lower-case hex only, which is the form the domain normalizes to, so the
    -- same bytes always yield the same ETag string.
    sha256       TEXT NOT NULL
        CONSTRAINT request_attachments_sha256_shape CHECK (sha256 ~ '^[0-9a-f]{64}$'),

    -- Whether malware scanning reached a verdict. Closed set, and the default is
    -- "no verdict" rather than "clean" — AMP ships the interface, not a scanner.
    scan_state   TEXT NOT NULL DEFAULT 'unsupported'
        CONSTRAINT request_attachments_scan_state CHECK (scan_state IN (
            'pending', 'clean', 'infected', 'unsupported')),

    -- Who uploaded it. A connector or service client may have no AMP user, so it
    -- is nullable; deleting the user nulls the link and leaves the file and its
    -- timestamp intact (the request keeps the requester's contact snapshot).
    uploaded_by  INT REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- D3 retention: an attachment is kept while its request is active, then 90
    -- days after the request closes, then purged — unless a team member pins it.
    -- expires_at is NULL both for a file on an active request and for a kept one
    -- (domain.RequestAttachmentExpiresAt), which is why "no expiry" means "may
    -- not be purged" and never "expire immediately".
    keep         BOOLEAN NOT NULL DEFAULT FALSE,
    kept_by      INT REFERENCES users(id) ON DELETE SET NULL,
    kept_at      TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,

    -- The restricted-file hold. A team lead releases a held file after checking
    -- it by hand; only the service's lead-only release path writes these. The
    -- authorization decision ("is this person a lead?") is not here — the row
    -- records that a release happened.
    released_at  TIMESTAMPTZ,
    released_by  INT REFERENCES users(id) ON DELETE SET NULL,

    -- The private bytes. The download path loads them; a list or an event never
    -- carries them. Inline BYTEA on purpose: PostgreSQL moves a large value out
    -- of line into the table's own TOAST relation, so a 25 MiB file costs the
    -- row no more than a pointer, and deleting the row deletes the bytes.
    content      BYTEA NOT NULL,

    -- A kept file records when it was pinned. One-way implication: the timestamp
    -- may outlive the actor (kept_by is ON DELETE SET NULL), never the reverse.
    CONSTRAINT request_attachments_kept_at
        CHECK (NOT keep OR kept_at IS NOT NULL),
    CONSTRAINT request_attachments_kept_by
        CHECK (kept_by IS NULL OR kept_at IS NOT NULL),
    CONSTRAINT request_attachments_released_by
        CHECK (released_by IS NULL OR released_at IS NOT NULL),

    -- A release is evidence a hold existed: only a held file is releasable. The
    -- two escape hatches mirror domain.RequestAttachmentRestricted (archive and
    -- executable) and domain.RequestAttachmentHeldByName (a code-like extension,
    -- which can ESCALATE a hold and can never lift one — sniffing cannot tell a
    -- shell script from a shopping list, both are text/plain). The extension is
    -- extracted the way Go's path.Ext does it: the suffix from the last dot of
    -- the trimmed name, lower-cased, '' when there is none.
    CONSTRAINT request_attachments_release_needs_hold
        CHECK (released_at IS NULL
            OR content_type IN (
                'application/zip', 'application/x-gzip',
                'application/octet-stream', 'application/wasm')
            OR COALESCE(lower(substring(btrim(file_name) from '\.[^.]*$')), '') IN (
                '.sh', '.bash', '.zsh', '.fish', '.ksh',
                '.ps1', '.psm1', '.bat', '.cmd', '.com',
                '.exe', '.dll', '.scr', '.msi',
                '.jar', '.vbs', '.js', '.mjs', '.py', '.rb', '.pl', '.php')),

    -- The row's size is the bytes' size. See the header for why this is the one
    -- measured promise (and why the digest is not re-hashed here).
    CONSTRAINT request_attachments_size_matches_content
        CHECK (octet_length(content) = size_bytes)
);

-- The attachments panel: one request's files, in upload order. Ordered by id,
-- not created_at, so rows written in one transaction keep a stable order (the
-- request_comments read rule) and the read is keyset-friendly.
CREATE INDEX IF NOT EXISTS idx_request_attachments_request
    ON request_attachments (request_id, id);

-- The retention job (G4): "which files may I purge?" — expired and not pinned.
-- Partial, because a kept or still-active file has no expires_at and must never
-- appear in that scan (the idx_artifacts_expiry shape).
CREATE INDEX IF NOT EXISTS idx_request_attachments_expiry
    ON request_attachments (expires_at) WHERE NOT keep;

-- A held file waiting for a lead's release: the triage "needs a release" badge
-- and the release action both read exactly this. Partial over the two held
-- kinds, because a document or a text file is never held by its type.
CREATE INDEX IF NOT EXISTS idx_request_attachments_held
    ON request_attachments (request_id, id)
    WHERE released_at IS NULL
      AND content_type IN ('application/zip', 'application/x-gzip',
                           'application/octet-stream', 'application/wasm');
