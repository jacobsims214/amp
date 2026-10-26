package service

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/simstech/amp-api/internal/auditchain"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// Audit log integrity (KB decisions/audit-log-v2.md section 4, #2429). The
// database makes audit_log append-only and hash-chains every row (migration
// 033); this file signs checkpoints of the chains, purges old rows through
// the anchored path, and verifies.

// AuditKeys signs checkpoints and checks their signatures. main sets it
// from the environment (auditchain.LoadKeyring); nil, or a keyring without a
// signing key, writes unsigned checkpoints.
var AuditKeys *auditchain.Keyring

// DefaultAuditRetention is how long audit entries are kept when
// AUDIT_RETENTION_DAYS is unset.
const DefaultAuditRetention = 365 * 24 * time.Hour

// MinAuditRetention is the shortest retention: audit_purge() (migration
// 035) refuses rows younger than this.
const MinAuditRetention = 30 * 24 * time.Hour

// AuditRetention is the configured retention (AUDIT_RETENTION_DAYS; main
// sets it). Verify flags an anchor whose row was younger than this when
// it was purged. 0: entries are kept forever, and anchors aren't checked.
var AuditRetention time.Duration

// systemPrincipal is AMP itself, for entries its jobs write.
var systemPrincipal = authz.Principal{Email: "AMP"}

// VerifyAudit verifies the chain of org orgID (org owners and admins, and
// global admins), or every chain (orgID 0: global admins only).
func (s *Service) VerifyAudit(ctx context.Context, p authz.Principal, orgID int) (auditchain.Report, error) {
	var chains []string
	switch {
	case orgID != 0:
		if _, err := s.Authz.RequireOrgAdmin(ctx, p, orgID); err != nil {
			return auditchain.Report{}, err
		}
		chains = []string{fmt.Sprintf("org:%d", orgID)}
	case p.Super:
		keys, err := s.Repo.AuditChainKeys(ctx)
		if err != nil {
			return auditchain.Report{}, err
		}
		chains = keys
	default:
		return auditchain.Report{}, authz.Forbidden("verifying every audit chain is for global admins; pass an org you administer")
	}
	return VerifyAuditChains(ctx, s.Repo, AuditKeys, auditchain.Policy{Retention: AuditRetention}, chains)
}

// VerifyAuditChains recomputes each chain from its latest anchor and checks
// its checkpoints against keys and policy. It reads only; problems are in
// the report, and err is for failing to read.
func VerifyAuditChains(ctx context.Context, repo *repository.Repo, keys *auditchain.Keyring, policy auditchain.Policy, chains []string) (auditchain.Report, error) {
	var reports []auditchain.ChainReport
	for _, key := range chains {
		var v *auditchain.ChainVerifier
		err := repo.ReadAuditChain(ctx, key,
			func(head *auditchain.Head, cps []auditchain.Checkpoint) {
				v = auditchain.NewChainVerifier(key, head, cps, keys, policy)
			},
			func(r auditchain.Row) { v.Add(r) })
		if err != nil {
			return auditchain.Report{}, err
		}
		reports = append(reports, v.Finish())
	}
	return auditchain.NewReport(keys, reports), nil
}

// WriteAuditCheckpoints signs the head of every chain that has moved since
// its last checkpoint, and records each as an audit.checkpoint entry, so the
// signed hash also leaves the database through the audit webhooks. It
// returns how many it wrote.
func (s *Service) WriteAuditCheckpoints(ctx context.Context) (int, error) {
	chains, err := s.Repo.AuditChainKeys(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, key := range chains {
		head, err := s.Repo.AuditChainHead(ctx, key)
		if err != nil {
			return n, err
		}
		if head == nil || head.Seq == 0 {
			continue
		}
		last, err := s.Repo.LastAuditCheckpoint(ctx, key)
		if err != nil {
			return n, err
		}
		if last != nil && last.UptoSeq >= head.Seq {
			continue
		}
		if last != nil && head.Seq == last.UptoSeq+1 {
			// The only new row is the entry announcing the last
			// checkpoint: don't checkpoint the announcement forever.
			if a, err := s.Repo.AuditActionAt(ctx, key, head.Seq); err != nil {
				return n, err
			} else if a == "audit.checkpoint" {
				continue
			}
		}
		cp := &auditchain.Checkpoint{ChainKey: key, UptoSeq: head.Seq, Hash: head.Hash, At: time.Now(), Kind: auditchain.KindPeriodic}
		AuditKeys.Sign(cp)
		if err := s.Repo.InsertAuditCheckpoint(ctx, cp); err != nil {
			return n, err
		}
		n++
		s.audit(ctx, systemPrincipal, checkpointEntry(cp, ""))
	}
	return n, nil
}

// RecordSystem writes an audit entry made by AMP itself (actor system),
// e.g. the webhook dispatcher turning a failing webhook off (#2439 M4).
func (s *Service) RecordSystem(ctx context.Context, e domain.AuditEntry) {
	s.audit(ctx, systemPrincipal, e)
}

// checkpointEntry is the audit entry announcing a checkpoint.
func checkpointEntry(cp *auditchain.Checkpoint, summary string) domain.AuditEntry {
	details := map[string]any{"chain_key": cp.ChainKey, "upto_seq": cp.UptoSeq, "hash": hex.EncodeToString(cp.Hash),
		"kind": cp.Kind, "checkpoint_id": cp.ID, "at": cp.At.UTC().Format("2006-01-02T15:04:05.000000Z"), "signed": len(cp.Signature) > 0}
	if len(cp.Signature) > 0 {
		details["key_id"] = cp.KeyID
		details["signature"] = base64.StdEncoding.EncodeToString(cp.Signature)
	}
	if summary == "" {
		summary = fmt.Sprintf("%s checkpoint of %s at seq %d", cp.Kind, cp.ChainKey, cp.UptoSeq)
	}
	e := domain.AuditEntry{Action: "audit.checkpoint", TargetType: "audit_chain", TargetID: cp.ChainKey, Summary: summary, Details: details}
	if org := auditchain.OrgOfChain(cp.ChainKey); org != 0 {
		e.OrgID = intRef(org)
	}
	return e
}

// PurgeAudit deletes audit entries written before cutoff, chain by chain,
// through the anchored path (repository.PurgeAuditChain), batch by batch
// (each with its own anchor, so writers are never held up for long), and
// records an audit.purge entry with what went. It returns the rows deleted
// per chain.
func (s *Service) PurgeAudit(ctx context.Context, cutoff time.Time, retention time.Duration) (map[string]int64, error) {
	chains, err := s.Repo.AuditChainKeys(ctx)
	if err != nil {
		return nil, err
	}
	deleted := map[string]int64{}
	perChain := map[string]any{}
	var total int64
	var firstErr error
	for _, key := range chains {
		var last *auditchain.Checkpoint
		var n, batches int64
		for ctx.Err() == nil {
			cp, k, err := s.Repo.PurgeAuditChain(ctx, key, cutoff, repository.AuditPurgeBatch, AuditKeys.Sign)
			if err != nil {
				slog.Error("audit retention: purging a chain failed", "chain", key, "err", err)
				if firstErr == nil {
					firstErr = err
				}
				break
			}
			if cp == nil {
				break
			}
			last, n, batches = cp, n+k, batches+1
		}
		if last == nil {
			continue
		}
		deleted[key] = n
		total += n
		perChain[key] = map[string]any{"deleted": n, "batches": batches, "anchor_seq": last.UptoSeq, "anchor_id": last.ID,
			"anchor_hash": hex.EncodeToString(last.Hash), "signed": len(last.Signature) > 0}
		// The last anchor's signature goes out with the chain's own
		// webhooks (each batch's anchor is in audit_checkpoints).
		s.audit(ctx, systemPrincipal, checkpointEntry(last, fmt.Sprintf("retention anchor of %s at seq %d (%d older entries deleted)", key, last.UptoSeq, n)))
	}
	if total > 0 {
		s.audit(ctx, systemPrincipal, domain.AuditEntry{Action: "audit.purge", TargetType: "audit_log",
			Summary: fmt.Sprintf("retention deleted %d audit entries older than %s", total, cutoff.UTC().Format(time.RFC3339)),
			Details: map[string]any{"cutoff": cutoff.UTC().Format(time.RFC3339), "retention_days": int(retention / (24 * time.Hour)), "chains": perChain}})
	}
	return deleted, firstErr
}

// TryLock takes a cluster-wide named lock without waiting
// (repository.Repo.TryNamedLock).
type TryLock func(ctx context.Context, key string) (release func(), ok bool, err error)

// RunAuditCheckpoints writes checkpoints every interval until ctx ends, on
// one replica at a time.
func (s *Service) RunAuditCheckpoints(ctx context.Context, interval time.Duration, lock TryLock) {
	runLocked(ctx, interval, lock, "audit-checkpoint", func() {
		if n, err := s.WriteAuditCheckpoints(ctx); err != nil {
			slog.Error("writing audit checkpoints failed", "err", err)
		} else if n > 0 {
			slog.Info("audit checkpoints written", "count", n, "signed", AuditKeys.CanSign())
		}
	})
}

// RunAuditRetention deletes entries older than retention every interval
// until ctx ends, on one replica at a time. retention <= 0 never runs.
func (s *Service) RunAuditRetention(ctx context.Context, retention, interval time.Duration, lock TryLock) {
	if retention <= 0 {
		return
	}
	runLocked(ctx, interval, lock, "audit-retention", func() {
		deleted, err := s.PurgeAudit(ctx, time.Now().Add(-retention), retention)
		if err != nil {
			slog.Error("audit retention failed", "err", err)
		}
		if len(deleted) > 0 {
			slog.Info("audit retention deleted old entries", "per_chain", deleted)
		}
	})
}

func runLocked(ctx context.Context, interval time.Duration, lock TryLock, name string, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		release, ok, err := lock(ctx, name)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				slog.Warn("job lock failed", "job", name, "err", err)
			}
		case ok:
			fn()
			release()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
