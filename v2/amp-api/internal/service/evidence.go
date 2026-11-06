package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/receipts"
	"github.com/simstech/amp-api/internal/repository"
)

// Receipt evidence (#126, Phase 3; v2/docs/specs/verification-receipts.md,
// "Evidence store", "HTTP routes", "Retention"). A worker declares the files
// it recorded (amp_add_evidence) and gets one-time upload URLs; amp-api
// checks each upload's sha256 and length and stores the bytes in the
// receipt store, never in Postgres and never as an artifact (D3). The sweep
// expires unkept evidence after the retention period (D13).

// EvidenceStore is the instance's receipt store and its limits. A nil
// *EvidenceStore, or one with a nil Store, means evidence storage isn't
// configured: evidence calls are refused with Err, and visual checks can't
// count (they need stored evidence), so Require holds them for a person.
type EvidenceStore struct {
	Store receipts.Store
	// Err is why the store isn't available (when Store is nil).
	Err string
	// MaxBytes is the per-file cap (receipts.DefaultMaxBytes when <= 0).
	MaxBytes int64
	// Retention is how long unkept evidence lives (90 days when <= 0).
	Retention time.Duration
}

// Limits of one amp_add_evidence call and one receipt.
const (
	MaxEvidenceFilesPerCall = 20
	MaxEvidencePerReceipt   = 50
)

// pendingGrace is how long a pending row outlives its expired upload URL
// before the sweep deletes it.
const pendingGrace = 24 * time.Hour

func (e *EvidenceStore) store() receipts.Store {
	if e == nil {
		return nil
	}
	return e.Store
}

// storeErr is "" when evidence can be stored, else why not.
func (s *Service) storeErr() string {
	if s.Evidence.store() != nil {
		return ""
	}
	if s.Evidence != nil && s.Evidence.Err != "" {
		return s.Evidence.Err
	}
	return "no receipt store is set up on this AMP server"
}

// MaxEvidenceBytes is the per-file cap (RECEIPT_MAX_BYTES).
func (s *Service) MaxEvidenceBytes() int64 { return s.maxEvidenceBytes() }

func (s *Service) maxEvidenceBytes() int64 {
	if s.Evidence != nil && s.Evidence.MaxBytes > 0 {
		return s.Evidence.MaxBytes
	}
	return receipts.DefaultMaxBytes
}

// EvidenceRetention is how long unkept evidence lives.
func (s *Service) EvidenceRetention() time.Duration {
	if s.Evidence != nil && s.Evidence.Retention > 0 {
		return s.Evidence.Retention
	}
	return receipts.DefaultRetentionDays * 24 * time.Hour
}

func (s *Service) notConfigured() error {
	return &authz.Error{Status: 503, Msg: "evidence storage isn't configured: " + s.storeErr() +
		". Complete without evidence; a visual check then counts as cant_tell"}
}

// EvidenceFileIn is one file amp_add_evidence declares.
type EvidenceFileIn struct {
	Name       string
	Bytes      int64
	SHA256     string
	DurationMS *int
	Frames     *int
}

// EvidenceUpload is one declared file's upload: URL is empty when the file
// is already stored with the same sha256 (nothing to upload).
type EvidenceUpload struct {
	Name      string     `json:"name"`
	Key       string     `json:"key"`
	URL       string     `json:"url,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Stored    bool       `json:"stored,omitempty"`
}

var (
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitRe = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)
)

// UploadPath is the upload route's prefix; the token follows it.
const UploadPath = "/api/receipt-uploads/"

// AddEvidence declares evidence files for draft receipt receiptID, as the
// ticket's worker on its current dispatch, and mints a one-time upload URL
// for each (amp_add_evidence). It records the receipt's commit and tree.
func (s *Service) AddEvidence(ctx context.Context, p authz.Principal, receiptID int64, dispatchID, commit, treeSHA string, files []EvidenceFileIn) ([]EvidenceUpload, error) {
	rc, err := s.Repo.GetReceipt(ctx, receiptID)
	if err != nil {
		return nil, authz.NotFound("receipt %d not found: pass the receipt_id amp_open_receipt returned", receiptID)
	}
	t, _, err := s.LoadTask(ctx, p, rc.TaskID, authz.TaskWork)
	if err != nil {
		if authz.StatusOf(err, 0) == 404 {
			// Name only what the caller asked for.
			return nil, authz.NotFound("receipt %d not found: pass the receipt_id amp_open_receipt returned", receiptID)
		}
		return nil, err
	}
	dispatchID = strings.TrimSpace(dispatchID)
	if t.State != domain.TaskStateInProgress || dispatchID == "" || !t.CurrentDispatch(dispatchID) || rc.DispatchID != dispatchID {
		return nil, authz.Forbidden("receipt %d: evidence is added by ticket %d's worker with the dispatch_id that opened the receipt; "+
			"%q isn't it (or isn't the ticket's current dispatch)", receiptID, t.ID, dispatchID)
	}
	if rc.State != domain.ReceiptDraft {
		return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf("receipt %d is already final: evidence is added before amp_complete_task", rc.ID)}
	}
	if s.Evidence.store() == nil {
		return nil, s.notConfigured()
	}
	treeSHA = strings.ToLower(strings.TrimSpace(treeSHA))
	commit = strings.TrimSpace(commit)
	if !receipts.ValidTreeSHA(treeSHA) {
		return nil, authz.BadRequest("tree_sha is required (12 to 64 hex digits): the git tree of the revision you recorded on (git rev-parse HEAD^{tree})")
	}
	if commit != "" && !commitRe.MatchString(commit) {
		return nil, authz.BadRequest("commit must be a git commit SHA")
	}
	if len(files) == 0 || len(files) > MaxEvidenceFilesPerCall {
		return nil, authz.BadRequest("files: 1 to %d per call", MaxEvidenceFilesPerCall)
	}
	max := s.maxEvidenceBytes()
	seen := map[string]bool{}
	for i, f := range files {
		if _, ok := receipts.ContentType(f.Name); !ok {
			return nil, authz.BadRequest("files[%d].name %q isn't an evidence name: use one of %s", i, f.Name, receipts.AllowedNames)
		}
		if seen[f.Name] {
			return nil, authz.BadRequest("files[%d].name %q is listed twice", i, f.Name)
		}
		seen[f.Name] = true
		if f.Bytes <= 0 || f.Bytes > max {
			return nil, authz.BadRequest("files[%d] (%s): bytes must be 1 to %d (RECEIPT_MAX_BYTES)", i, f.Name, max)
		}
		if !sha256Re.MatchString(f.SHA256) {
			return nil, authz.BadRequest("files[%d] (%s): sha256 must be 64 lowercase hex digits (sha256sum)", i, f.Name)
		}
		if (f.DurationMS != nil && *f.DurationMS < 0) || (f.Frames != nil && *f.Frames < 0) {
			return nil, authz.BadRequest("files[%d] (%s): duration_ms and frames can't be negative", i, f.Name)
		}
	}
	have, err := s.Repo.CountReceiptEvidence(ctx, rc.ID)
	if err != nil {
		return nil, err
	}
	// Re-added keys don't count twice; an upper bound is enough here.
	newKeys := 0
	for _, f := range files {
		e, err := s.Repo.EvidenceByKey(ctx, receipts.Key(t.ProjectID, t.ID, rc.ID, treeSHA, f.Name))
		if err != nil {
			return nil, err
		}
		if e == nil {
			newKeys++
		}
	}
	if have+newKeys > MaxEvidencePerReceipt {
		return nil, authz.BadRequest("receipt %d: at most %d evidence files", rc.ID, MaxEvidencePerReceipt)
	}
	if err := s.Repo.SetReceiptRevision(ctx, rc.ID, commit, treeSHA); err != nil {
		return nil, err
	}
	base := strings.TrimRight(s.AppURL, "/")
	out := make([]EvidenceUpload, 0, len(files))
	for _, f := range files {
		ct, _ := receipts.ContentType(f.Name)
		key := receipts.Key(t.ProjectID, t.ID, rc.ID, treeSHA, f.Name)
		if err := receipts.ValidateKey(key); err != nil {
			return nil, authz.BadRequest("%v", err)
		}
		cur, err := s.Repo.EvidenceByKey(ctx, key)
		if err != nil {
			return nil, err
		}
		if cur != nil && cur.Status == domain.EvidenceStored {
			if cur.SHA256 == f.SHA256 && cur.Bytes == f.Bytes {
				out = append(out, EvidenceUpload{Name: f.Name, Key: key, Stored: true})
				continue
			}
			return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf("%s is already stored for this revision with a different sha256; "+
				"evidence is written once: record on the revision you complete (a new tree_sha), or keep the stored file", f.Name)}
		}
		token, hash, err := receipts.NewUploadToken()
		if err != nil {
			return nil, err
		}
		expires := time.Now().Add(receipts.UploadTokenTTL).UTC().Truncate(time.Second)
		ok, err := s.Repo.PutPendingEvidence(ctx, domain.ReceiptEvidence{ReceiptID: rc.ID, Key: key, Name: f.Name, TreeSHA: treeSHA,
			ContentType: ct, Bytes: f.Bytes, SHA256: f.SHA256, DurationMS: f.DurationMS, Frames: f.Frames}, t.ProjectID, hash, expires)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf("%s was stored meanwhile; call amp_add_evidence again", f.Name)}
		}
		out = append(out, EvidenceUpload{Name: f.Name, Key: key, URL: base + UploadPath + token, ExpiresAt: &expires})
	}
	return out, nil
}

// StoreUpload stores an upload to a one-time URL (PUT
// /api/receipt-uploads/<token>, no session: the token is the credential).
// Unknown or already used token: 404. Expired: 410. Another upload with the
// same token still running: 409. Bytes that don't match the declared length
// or sha256: 422, and nothing is stored (the token stays valid until it
// expires, so the upload can be retried).
//
// The row is claimed first (its token hash extended with a one-off claim,
// in one UPDATE), so two requests with the same token can't both delete, write
// and mark the same key: the second gets 409 and touches nothing.
func (s *Service) StoreUpload(ctx context.Context, token string, body io.Reader) (*domain.ReceiptEvidence, error) {
	if !receipts.ValidTokenShape(token) {
		return nil, authz.NotFound("upload URL not found")
	}
	hash := receipts.HashToken(token)
	row, err := s.Repo.PendingEvidenceByToken(ctx, hash)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, authz.NotFound("upload URL not found or already used: call amp_add_evidence again for a new one")
	}
	if row.UploadExpiresAt == nil || time.Now().After(*row.UploadExpiresAt) {
		return nil, &authz.Error{Status: 410, Msg: "upload URL expired: call amp_add_evidence again for a new one"}
	}
	store := s.Evidence.store()
	if store == nil {
		return nil, s.notConfigured()
	}
	if err := receipts.ValidateKey(row.Key); err != nil {
		return nil, authz.BadRequest("%v", err)
	}
	if row.TokenHash != hash {
		return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf("%s is being uploaded by another request with this URL", row.Name)}
	}
	_, claim, err := receipts.NewUploadToken()
	if err != nil {
		return nil, err
	}
	claim = hash + ":claim:" + claim
	claimed, err := s.Repo.ClaimPendingEvidence(ctx, row.ID, hash, claim)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf("%s is being uploaded by another request with this URL", row.Name)}
	}
	stored := false
	defer func() {
		if stored {
			return
		}
		// Not stored: the token works again (a retry), unless the row was
		// re-minted meanwhile.
		if err := s.Repo.ReleasePendingEvidence(context.WithoutCancel(ctx), row.ID, claim, hash); err != nil {
			slog.Warn("receipt upload: release the claim", "key", row.Key, "err", err)
		}
	}()
	// A pending row's object is from an earlier attempt that didn't finish
	// (or a re-mint): it is replaced.
	if err := store.Delete(ctx, row.Key); err != nil {
		return nil, err
	}
	sum, n, err := store.Put(ctx, row.Key, row.ContentType, body, row.Bytes)
	switch {
	case errors.Is(err, receipts.ErrTooLarge):
		return nil, &authz.Error{Status: 422, Msg: fmt.Sprintf("%s: the upload is longer than the declared %d bytes; nothing was stored", row.Name, row.Bytes)}
	case errors.Is(err, receipts.ErrExists):
		return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf("%s is being uploaded by another request", row.Name)}
	case err != nil:
		return nil, err
	}
	if n != row.Bytes || sum != row.SHA256 {
		if derr := store.Delete(ctx, row.Key); derr != nil {
			slog.Warn("receipt upload: delete a mismatched object", "key", row.Key, "err", derr)
		}
		what := "sha256"
		if n != row.Bytes {
			what = fmt.Sprintf("length (%d bytes, declared %d)", n, row.Bytes)
		}
		return nil, &authz.Error{Status: 422, Msg: fmt.Sprintf("%s: the upload's %s doesn't match what amp_add_evidence declared; nothing was stored", row.Name, what)}
	}
	ok, err := s.Repo.MarkEvidenceStored(ctx, row.ID, claim)
	if err != nil {
		return nil, err
	}
	if !ok {
		// Re-minted while uploading: the new URL's upload replaces this one.
		return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf("%s was declared again while uploading: use the newer URL", row.Name)}
	}
	stored = true
	ev := row.ReceiptEvidence
	ev.Status = domain.EvidenceStored
	now := time.Now()
	ev.StoredAt = &now
	// Only what changed: the receipt's status and hold aren't re-derived by
	// an upload, so they are left out rather than guessed.
	if s.Notify != nil {
		s.Notify(domain.Event{Type: domain.EventReceiptUpdated, ProjectID: row.ProjectID, At: now,
			Payload: map[string]any{"task_id": row.TaskID, "receipt_id": row.ReceiptID, "evidence_key": row.Key}})
	}
	return &ev, nil
}

// ServableEvidence is what GET /api/receipts/<key> serves.
type ServableEvidence struct {
	Evidence domain.ReceiptEvidence
	Store    receipts.Store
}

// EvidenceForServe checks p may see the evidence at key and that it is
// stored: an invalid key is 400, a caller with no access to its project
// 403, an unknown key 404, expired evidence 410.
func (s *Service) EvidenceForServe(ctx context.Context, p authz.Principal, key string) (*ServableEvidence, error) {
	if err := receipts.ValidateKey(key); err != nil {
		return nil, authz.BadRequest("%v", err)
	}
	projectID := receipts.KeyProject(key)
	if projectID == 0 {
		return nil, authz.NotFound("evidence not found")
	}
	// Membership first, so a non-member learns nothing about which keys
	// exist.
	lvl, err := s.Authz.ProjectLevel(ctx, p, projectID)
	if err != nil {
		return nil, err
	}
	if lvl == authz.None {
		return nil, authz.Forbidden("evidence of project %d: you aren't a member of the project", projectID)
	}
	row, err := s.Repo.EvidenceByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if row == nil || row.ProjectID != projectID {
		return nil, authz.NotFound("evidence not found")
	}
	t, err := s.Repo.GetTask(ctx, row.TaskID)
	if err != nil {
		return nil, authz.NotFound("evidence not found: its ticket was deleted")
	}
	if err := authz.CheckTask(lvl, t, p.UserID, authz.TaskRead); err != nil {
		return nil, err
	}
	switch row.Status {
	case domain.EvidenceExpired:
		return nil, &authz.Error{Status: 410, Msg: fmt.Sprintf("%s expired: unkept evidence is deleted after %d days", row.Name, int(s.EvidenceRetention().Hours()/24))}
	case domain.EvidenceStored:
	default:
		return nil, authz.NotFound("%s hasn't been uploaded", row.Name)
	}
	store := s.Evidence.store()
	if store == nil {
		return nil, s.notConfigured()
	}
	return &ServableEvidence{Evidence: row.ReceiptEvidence, Store: store}, nil
}

// withExpiry sets each unkept receipt's evidence ExpiresAt.
func (s *Service) withExpiry(rc domain.Receipt, ev []domain.ReceiptEvidence) {
	if rc.Kept {
		return
	}
	at := rc.CreatedAt.Add(s.EvidenceRetention())
	for i := range ev {
		ev[i].ExpiresAt = &at
	}
}

// ---- Retention ----

// sweepBatch bounds one sweep pass's rows of each kind.
const sweepBatch = 500

// tmpMaxAge is how old a store's temporary file (an upload a crash cut off)
// must be before the sweep removes it.
const tmpMaxAge = time.Hour

// SweepReceiptEvidence deletes the stored evidence of unkept receipts past
// retention (the rows stay, marked expired, so the drawer can say
// "evidence expired"), deletes pending rows whose upload expired more than a
// day ago (their object first), and removes the store's stale temporary
// files. Each object is deleted while its row (and receipt) is locked, and
// the row changes only after the delete worked: a receipt kept, or an upload
// re-minted, meanwhile waits for the lock and is then skipped, and a failed
// delete leaves the row as it was, for the next pass.
func (s *Service) SweepReceiptEvidence(ctx context.Context) (expired, dropped int, err error) {
	store := s.Evidence.store()
	if store == nil {
		return 0, 0, nil
	}
	failed := map[int64]bool{}
	for {
		rows, err := s.Repo.ExpirableEvidence(ctx, time.Now().Add(-s.EvidenceRetention()), sweepBatch)
		if err != nil {
			return expired, dropped, err
		}
		n := 0
		for _, e := range rows {
			if failed[e.ID] {
				continue
			}
			ok, err := s.Repo.ExpireEvidenceLocked(ctx, e.ID, func(key string) error { return store.Delete(ctx, key) })
			if err != nil {
				slog.Warn("receipt sweep: expire evidence (retried next pass)", "key", e.Key, "err", err)
				failed[e.ID] = true
				continue
			}
			if ok {
				n++
			}
		}
		expired += n
		if len(rows) < sweepBatch || n == 0 {
			break
		}
	}
	for {
		rows, err := s.Repo.StalePendingEvidence(ctx, time.Now().Add(-pendingGrace), sweepBatch)
		if err != nil {
			return expired, dropped, err
		}
		n := 0
		for _, e := range rows {
			if failed[e.ID] {
				continue
			}
			ok, err := s.Repo.DeleteStalePendingLocked(ctx, e.ID, e.TokenHash, time.Now().Add(-pendingGrace),
				func(key string) error { return store.Delete(ctx, key) })
			if err != nil {
				slog.Warn("receipt sweep: delete an unfinished upload (retried next pass)", "key", e.Key, "err", err)
				failed[e.ID] = true
				continue
			}
			if ok {
				n++
			}
		}
		dropped += n
		if len(rows) < sweepBatch || n == 0 {
			break
		}
	}
	if c, ok := store.(interface {
		CleanTemp(olderThan time.Duration) (int, error)
	}); ok {
		if n, err := c.CleanTemp(tmpMaxAge); err != nil {
			slog.Warn("receipt sweep: clean temporary files", "err", err)
		} else if n > 0 {
			slog.Info("receipt sweep: removed stale temporary files", "count", n)
		}
	}
	return expired, dropped, nil
}

// RunReceiptSweep runs SweepReceiptEvidence every interval until ctx ends,
// on one replica at a time (an advisory lock).
func (s *Service) RunReceiptSweep(ctx context.Context, interval time.Duration, lock TryLock) {
	runLocked(ctx, interval, lock, "receipt-sweep", func() {
		expired, dropped, err := s.SweepReceiptEvidence(ctx)
		if err != nil {
			slog.Warn("receipt evidence sweep failed", "err", err)
		}
		if expired > 0 || dropped > 0 {
			slog.Info("receipt evidence sweep", "expired", expired, "unfinished_uploads_deleted", dropped)
		}
	})
}

// deleteEvidenceObjects deletes rows' objects from the store. It reports
// false when one couldn't be deleted, or there is no store while a row is
// stored or pending (the caller then keeps the rows, so a later pass, or a
// retry once the store is back, deletes them).
func (s *Service) deleteEvidenceObjects(ctx context.Context, rows []repository.EvidenceRow) bool {
	store := s.Evidence.store()
	ok := true
	for _, e := range rows {
		if e.Status == domain.EvidenceExpired {
			continue // its object is already gone
		}
		if store == nil {
			// The object can't be deleted, and once the rows go nothing could
			// ever find it: refuse, so the caller keeps the rows and retries.
			slog.Warn("receipt evidence can't be deleted: no receipt store is configured", "key", e.Key)
			ok = false
			continue
		}
		if err := store.Delete(ctx, e.Key); err != nil {
			slog.Warn("delete receipt evidence", "key", e.Key, "err", err)
			ok = false
		}
	}
	return ok
}

// PurgeTrashedReceipts deletes the receipts and evidence of tickets the
// trash purge is about to delete (receipts have no FK to tasks).
func (s *Service) PurgeTrashedReceipts(ctx context.Context) (int64, error) {
	ids, err := s.Repo.PurgeableTrashTaskIDs(ctx)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	rows, err := s.Repo.TasksEvidence(ctx, ids)
	if err != nil {
		return 0, err
	}
	if !s.deleteEvidenceObjects(ctx, rows) {
		return 0, fmt.Errorf("some evidence of purged tickets couldn't be deleted; retrying next pass")
	}
	return s.Repo.DeleteTasksReceipts(ctx, ids)
}

// DeleteProjectReceipts deletes a project's receipts and their evidence:
// before a reset deletes its tickets.
func (s *Service) DeleteProjectReceipts(ctx context.Context, projectID int) error {
	rows, err := s.Repo.ProjectEvidence(ctx, projectID)
	if err != nil {
		return err
	}
	if !s.deleteEvidenceObjects(ctx, rows) {
		return fmt.Errorf("couldn't delete project %d's receipt evidence; nothing was reset, try again", projectID)
	}
	return s.Repo.DeleteProjectReceipts(ctx, projectID)
}
