package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// Verification receipts (#126, v2/docs/specs/verification-receipts.md).
//
// A project's receipts setting is Off (the org default), Inform or Require.
// Off records nothing and changes nothing. Inform records a receipt per
// completing run and shows its status. Require also holds a completion whose
// receipt is below the project's minimum level: the ticket keeps its state
// (in progress), its dispatch ends, and it waits for a person to Accept
// (which completes it) or Reject (which returns it to ready). Only a person
// in the web UI changes the setting, accepts or rejects; an agent may make a
// ticket's override stricter, never weaker.

// ReceiptIn is what a completion says about its receipt: the draft it
// opened (amp_open_receipt), the checks it ran, and what it changed. The
// zero value adopts the ticket's newest draft, or creates one, with no
// checks.
type ReceiptIn struct {
	ReceiptID    int64
	DispatchID   string
	Checks       []domain.ReceiptCheck
	FilesChanged []domain.FileChange // nil keeps the draft's
	TreeSHA      string
	Summary      string
	// skip: the completion is a person's Accept of a held receipt, which is
	// already final; no new receipt is made.
	skip bool
}

// receiptPlan is a completion's receipt, decided before the ticket moves:
// the draft it finalizes (nil: one is created), its checks, its status, and
// whether Require holds it.
type receiptPlan struct {
	mode, minLevel string
	draft          *domain.Receipt
	in             ReceiptIn
	status         string
	hold           bool
	reason         string
}

// ReceiptSettings is a ticket's (or a project's) effective receipts setting.
type ReceiptSettings struct {
	Mode     string `json:"mode"`
	MinLevel string `json:"min_level"`
	// ProjectMode is the project's own value ("" uses the org's) and
	// OrgMode the org's default, for Project settings' "Use org default
	// (Off)".
	ProjectMode string `json:"project_mode"`
	OrgMode     string `json:"org_mode"`
}

// projectReceipts is a project's setting before any metadata override.
func (s *Service) projectReceipts(ctx context.Context, proj *domain.Project) ReceiptSettings {
	rs := ReceiptSettings{ProjectMode: proj.ReceiptsMode, OrgMode: domain.ReceiptsOff, MinLevel: proj.ReceiptsMinLevel}
	if proj.OrgID != nil {
		if org, err := s.Repo.GetOrg(ctx, *proj.OrgID); err == nil && domain.ValidReceiptsMode(org.ReceiptsMode) {
			rs.OrgMode = org.ReceiptsMode
		}
	}
	rs.Mode = rs.OrgMode
	if domain.ValidReceiptsMode(proj.ReceiptsMode) {
		rs.Mode = proj.ReceiptsMode
	}
	if !domain.ValidMinLevel(rs.MinLevel) {
		rs.MinLevel = domain.StatusSelfChecked
	}
	return rs
}

// withOverride applies effective metadata m's receipts keys on top.
func withOverride(rs ReceiptSettings, m domain.Metadata) ReceiptSettings {
	mode, min := m.ReceiptsOverride()
	if mode != "" {
		rs.Mode = mode
	}
	if min != "" {
		rs.MinLevel = min
	}
	return rs
}

// ProjectReceiptSettings is a project's effective setting, its own
// metadata included.
func (s *Service) ProjectReceiptSettings(ctx context.Context, projectID int) (ReceiptSettings, error) {
	proj, err := s.Repo.GetProject(ctx, projectID)
	if err != nil {
		return ReceiptSettings{}, err
	}
	return withOverride(s.projectReceipts(ctx, proj), proj.Metadata), nil
}

// EffectiveReceipts is ticket t's receipts setting: its metadata (project →
// epic → story → ticket), then the project's setting, then the org's. A
// person's ticket never has receipts (D15).
// For display, an unreadable inheritance falls back to the project's own
// metadata; completions use effectiveReceiptsStrict, which fails closed.
func (s *Service) EffectiveReceipts(ctx context.Context, t *domain.Task) (ReceiptSettings, error) {
	return s.effectiveReceipts(ctx, t, false)
}

// effectiveReceiptsStrict is EffectiveReceipts that returns an
// InheritedMetadata error instead of falling back: a completion must not
// miss an epic's or story's Require because a read failed.
func (s *Service) effectiveReceiptsStrict(ctx context.Context, t *domain.Task) (ReceiptSettings, error) {
	return s.effectiveReceipts(ctx, t, true)
}

func (s *Service) effectiveReceipts(ctx context.Context, t *domain.Task, strict bool) (ReceiptSettings, error) {
	proj, err := s.Repo.GetProject(ctx, t.ProjectID)
	if err != nil {
		return ReceiptSettings{}, err
	}
	rs := s.projectReceipts(ctx, proj)
	inherited, err := s.Repo.InheritedMetadata(ctx, "task", t.ID)
	if err != nil {
		if strict {
			return ReceiptSettings{}, err
		}
		inherited = proj.Metadata
	}
	rs = withOverride(rs, inherited.Merge(t.Metadata))
	if t.IsHuman() {
		rs.Mode = domain.ReceiptsOff
	}
	return rs, nil
}

// ---- Opening a draft (amp_open_receipt) ----

// OpenReceipt opens (or updates) ticket taskID's draft receipt for the
// caller's current dispatch, so evidence can upload before completion. In an
// Off project it stores nothing and returns a nil receipt.
func (s *Service) OpenReceipt(ctx context.Context, p authz.Principal, taskID int, dispatchID, summary string, files []domain.FileChange) (*domain.Receipt, string, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskWork)
	if err != nil {
		return nil, "", err
	}
	if t.IsHuman() {
		return nil, domain.ReceiptsOff, nil
	}
	rs, err := s.EffectiveReceipts(ctx, t)
	if err != nil {
		return nil, "", err
	}
	if rs.Mode == domain.ReceiptsOff {
		return nil, domain.ReceiptsOff, nil
	}
	dispatchID = strings.TrimSpace(dispatchID)
	if t.State != domain.TaskStateInProgress || dispatchID == "" || !t.CurrentDispatch(dispatchID) {
		return nil, "", authz.Forbidden("ticket %d: a receipt is opened by the ticket's worker with its current dispatch_id; "+
			"this call presents %q, which isn't the ticket's current dispatch", taskID, dispatchID)
	}
	if len(summary) > domain.MaxReceiptSummary {
		return nil, "", authz.BadRequest("summary: at most %d characters", domain.MaxReceiptSummary)
	}
	rc, err := s.Repo.OpenReceiptDraft(ctx, t.ID, t.ProjectID, dispatchID, rs.Mode, t.AgentID, strings.TrimSpace(summary), files)
	if err != nil {
		return nil, "", err
	}
	return rc, rs.Mode, nil
}

// ---- Completion: plan, hold, finalize ----

// prepareReceipt decides completion's receipt before ticket t moves. nil
// with no error means nothing to record: Off, a person's ticket, a ticket
// already done (completing twice leaves one receipt), or a person's Accept.
// An error (a receipt of another ticket or dispatch) refuses the completion.
func (s *Service) prepareReceipt(ctx context.Context, p authz.Principal, t *domain.Task, in *ReceiptIn) (*receiptPlan, error) {
	if t.IsHuman() || t.State == domain.TaskStateCompleted || (in != nil && in.skip) {
		return nil, nil
	}
	// A held ticket waits for a person (D10): off the web UI nothing
	// completes it, not its worker trying again with new checks, nor a
	// closeout. Rework goes through a new dispatch, which ends the hold.
	if !p.InUI() && t.State == domain.TaskStateInProgress && t.DispatchID == "" {
		rc, err := s.latestFinal(ctx, t.ID)
		if err != nil && authz.StatusOf(err, 0) != 404 {
			return nil, &authz.Error{Status: 503, Msg: fmt.Sprintf("ticket %d: couldn't read its receipt (%v); nothing changed, try again", t.ID, err)}
		}
		if err == nil && liveHeld(*rc, t.State, t.DispatchID) {
			return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf("ticket %d is held: its verification receipt couldn't be verified and it "+
				"waits for a person to Accept (which completes it) or Reject it in the AMP web UI: %s. Don't complete it again; "+
				"rework needs a new dispatch.", t.ID, s.TaskURL(t.ProjectID, t.ID))}
		}
	}
	rs, err := s.effectiveReceiptsStrict(ctx, t)
	if err != nil {
		return nil, &authz.Error{Status: 503, Msg: fmt.Sprintf("ticket %d: couldn't read its receipts setting (%v); nothing changed, try again", t.ID, err)}
	}
	if rs.Mode == domain.ReceiptsOff {
		return nil, nil
	}
	plan := &receiptPlan{mode: rs.Mode, minLevel: rs.MinLevel}
	if in != nil {
		plan.in = *in
	}
	if plan.in.ReceiptID != 0 {
		rc, err := s.Repo.GetReceipt(ctx, plan.in.ReceiptID)
		if err != nil || rc.TaskID != t.ID {
			return nil, authz.BadRequest("receipt %d isn't ticket %d's: pass the receipt_id amp_open_receipt returned for this ticket", plan.in.ReceiptID, t.ID)
		}
		if rc.State != domain.ReceiptDraft {
			return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf("receipt %d is already final; open a new one with amp_open_receipt", rc.ID)}
		}
		if !p.InUI() && rc.DispatchID != "" && rc.DispatchID != strings.TrimSpace(plan.in.DispatchID) {
			return nil, authz.BadRequest("receipt %d was opened by another dispatch of ticket %d; pass your own dispatch_id and receipt_id", rc.ID, t.ID)
		}
		plan.draft = rc
	} else if rc, err := s.Repo.NewestReceiptDraft(ctx, t.ID); err == nil && rc != nil {
		plan.draft = rc
	}
	s.downgradeVisual(ctx, plan)
	agent := t.AgentID
	if plan.draft != nil && plan.draft.AgentID != "" {
		agent = plan.draft.AgentID
	}
	plan.status = domain.DeriveStatus(domain.Receipt{AgentID: agent}, plan.in.Checks)
	if rs.Mode == domain.ReceiptsRequire && !domain.MeetsMinimum(plan.status, rs.MinLevel) {
		plan.hold = true
		plan.reason = holdReason(plan.in.Checks, plan.status, rs.MinLevel)
		// A held ticket waits in progress with no dispatch (that is what
		// "held" means everywhere): a ticket that isn't in progress has no
		// run to hold, so its completion below the minimum is refused.
		if t.State != domain.TaskStateInProgress {
			return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf("ticket %d is %s and its project requires a verification receipt (%s); "+
				"this completion's is below it (%s). Dispatch it so its worker completes it with checks, or a person in the AMP web UI "+
				"lowers the requirement: %s", t.ID, t.State, describeReceipts(rs), plan.reason, s.SettingsURL(t.ProjectID))}
		}
	}
	return plan, nil
}

// downgradeVisual makes cant_tell each passing visual check of plan that
// its draft's stored evidence doesn't back (domain.DowngradeVisualChecks):
// before the status is derived, so Require holds on it. A completion with
// no draft has no evidence; evidence that can't be read counts as missing.
func (s *Service) downgradeVisual(ctx context.Context, plan *receiptPlan) {
	visual := false
	for _, c := range plan.in.Checks {
		visual = visual || (c.Kind == domain.CheckKindVisual && c.Verdict == domain.VerdictPass)
	}
	if !visual {
		return
	}
	var ev []domain.ReceiptEvidence
	tree := plan.in.TreeSHA
	if plan.draft != nil {
		if rows, err := s.Repo.ReceiptEvidence(ctx, plan.draft.ID); err == nil {
			ev = rows
		} else {
			slog.Warn("receipt: read evidence for visual checks", "receipt_id", plan.draft.ID, "err", err)
		}
		if tree == "" {
			tree = plan.draft.TreeSHA
		}
	}
	plan.in.Checks = domain.DowngradeVisualChecks(plan.in.Checks, ev, tree, s.storeErr())
}

// holdReason says why a receipt is below its minimum, in a person's words.
func holdReason(checks []domain.ReceiptCheck, status, min string) string {
	var failed, cant []string
	for _, c := range checks {
		ref := c.CriterionRef
		if ref == "" {
			ref = c.Kind
		}
		switch c.Verdict {
		case domain.VerdictFail:
			failed = append(failed, ref)
		case domain.VerdictCantTell:
			cant = append(cant, ref)
		}
	}
	switch {
	case len(checks) == 0:
		return "no checks were recorded"
	case len(failed) > 0:
		return fmt.Sprintf("%d of %d checks failed (%s)", len(failed), len(checks), strings.Join(failed, ", "))
	case status == domain.StatusSelfChecked && min == domain.StatusIndependentlyVerified:
		return "it needs independent verification"
	case len(cant) > 0:
		return fmt.Sprintf("no check passed; %d couldn't tell (%s)", len(cant), strings.Join(cant, ", "))
	}
	return "its checks don't reach " + strings.ReplaceAll(min, "_", " ")
}

// holdCompletion holds t for a person in its actor (MsgCompleteTask.Hold:
// every check a completion makes, then the dispatch ends instead) and posts
// "Couldn't verify, needs you" on it. It reports completed when the actor
// completed the ticket instead (a replica that predates Hold, during a
// rolling deploy): the caller then finalizes the receipt as completed, and
// nothing is posted.
func (s *Service) holdCompletion(ctx context.Context, p authz.Principal, t *domain.Task, msg actor.MsgCompleteTask, plan *receiptPlan) (completed bool, err error) {
	replyCh := make(chan actor.ReplyComplete, 1)
	msg.TaskID, msg.By, msg.ByName, msg.InUI, msg.Hold, msg.HoldReason, msg.ReplyCh =
		t.ID, p.UserRef(), p.Label(), p.InUI(), true, plan.reason, replyCh
	if err := s.Registry.Send(t.ProjectID, &msg); err != nil {
		return false, err
	}
	reply := actor.Await(ctx, replyCh)
	if reply.Err != nil {
		return false, &authz.Error{Status: 409, Msg: reply.Err.Error()}
	}
	if reply.Task != nil && reply.Task.State == domain.TaskStateCompleted {
		slog.Error("receipt hold ignored: the ticket's actor completed it (a replica without Hold?)", "task_id", t.ID, "reason", plan.reason)
		return true, nil
	}
	s.comment(t.ProjectID, t.ID, p, fmt.Sprintf("Couldn't verify, needs you: %s. A person accepts it (which completes the ticket) "+
		"or rejects it (back to ready) in the AMP web UI: %s", plan.reason, s.TaskURL(t.ProjectID, t.ID)))
	return false, nil
}

// FinalizeReceipt makes completion closed's receipt final: after a real
// transition into completed, or after a hold. run is the run the completion
// recorded (nil for a UI or REST completion). It does nothing when the
// completion recorded no receipt (Off, a repeat, a person's Accept).
func (s *Service) FinalizeReceipt(ctx context.Context, p authz.Principal, taskID int, run *domain.Run, closed Closeout) {
	plan := closed.plan
	if plan == nil || (!closed.Completed && !closed.Held) {
		return
	}
	// A held completion with no final receipt has no Accept and no ask: the
	// ticket would wait unseen, so that failure is an error.
	logFail := func(msg string, args ...any) {
		args = append(args, "task_id", taskID, "held", closed.Held)
		if closed.Held {
			slog.Error(msg+": the ticket is held with no receipt; a person can still reject or complete it on the Board", args...)
			return
		}
		slog.Warn(msg, args...)
	}
	t, err := s.Repo.GetTask(ctx, taskID)
	if err != nil {
		logFail("finalize receipt: load ticket", "err", err)
		return
	}
	if closed.Completed {
		// Done now: no earlier attempt still waits for a person.
		if err := s.Repo.ClearTaskHeld(ctx, taskID); err != nil {
			slog.Warn("finalize receipt: clear earlier holds", "task_id", taskID, "err", err)
		}
	}
	rc := plan.draft
	if rc == nil {
		created, err := s.Repo.CreateReceipt(ctx, domain.Receipt{TaskID: t.ID, ProjectID: t.ProjectID, Mode: plan.mode, AgentID: t.AgentID})
		if err != nil {
			logFail("finalize receipt: create", "err", err)
			return
		}
		rc = created
	}
	fin := *rc
	fin.Mode, fin.Held = plan.mode, closed.Held
	if run != nil && run.AgentID != "" {
		fin.AgentID = run.AgentID
	} else if fin.AgentID == "" {
		fin.AgentID = t.AgentID
	}
	if run != nil && run.ID != 0 {
		id := run.ID
		fin.RunID = &id
	}
	s.copyRunDetails(ctx, t.ID, run, &fin)
	if plan.in.TreeSHA != "" {
		fin.TreeSHA = plan.in.TreeSHA
	}
	if plan.in.FilesChanged != nil {
		fin.FilesChanged = plan.in.FilesChanged
	}
	if plan.in.Summary != "" {
		fin.Summary = plan.in.Summary
	}
	fin.Kept = t.MergeState == domain.MergeMerged
	checks := make([]domain.ReceiptCheck, len(plan.in.Checks))
	for i, c := range plan.in.Checks {
		c.Role, c.CheckedBy, c.UserID = domain.CheckRoleWorker, fin.AgentID, p.UserRef()
		checks[i] = c
	}
	ok, err := s.Repo.FinalizeReceipt(ctx, fin, checks)
	if err != nil {
		logFail("finalize receipt", "receipt_id", fin.ID, "err", err)
		return
	}
	if !ok {
		return // another completion finalized it first
	}
	s.notifyReceipt(t.ProjectID, t.ID, fin.ID, plan.status, closed.Held)
}

// copyRunDetails fills rc's commit, branch, base branch and PR from run, or
// else from the latest run of the ticket that has them.
func (s *Service) copyRunDetails(ctx context.Context, taskID int, run *domain.Run, rc *domain.Receipt) {
	pick := func(r domain.Run) bool {
		if r.Commit == "" && r.Branch == "" && r.PRURL == "" {
			return false
		}
		rc.CommitSHA, rc.Branch, rc.BaseBranch, rc.PRURL = r.Commit, r.Branch, r.BaseBranch, r.PRURL
		return true
	}
	if run != nil && pick(*run) {
		return
	}
	if runs, err := s.Repo.TaskRuns(ctx, taskID); err == nil {
		for i := len(runs) - 1; i >= 0; i-- {
			if pick(runs[i]) {
				return
			}
		}
	}
}

// notifyReceipt publishes receipt.updated through Notify, when set.
func (s *Service) notifyReceipt(projectID, taskID int, receiptID int64, status string, held bool) {
	if s.Notify == nil {
		return
	}
	s.Notify(domain.Event{Type: domain.EventReceiptUpdated, ProjectID: projectID, At: time.Now(),
		Payload: map[string]any{"task_id": taskID, "receipt_id": receiptID, "status": status, "held": held}})
}

// ---- Reading ----

// ReceiptView is a receipt with its checks, evidence and derived status.
type ReceiptView struct {
	Receipt  domain.Receipt           `json:"receipt"`
	Checks   []domain.ReceiptCheck    `json:"checks"`
	Evidence []domain.ReceiptEvidence `json:"evidence"`
	Status   string                   `json:"status"`
	Held     bool                     `json:"held"`
	// HoldReason is why a held receipt is below its minimum.
	HoldReason string `json:"hold_reason,omitempty"`
}

// TaskReceipt is GET /api/tasks/:id/receipt: the latest final receipt, or
// nil with the ticket's setting.
type TaskReceipt struct {
	*ReceiptView
	Attempts int             `json:"attempts"` // earlier final receipts
	Settings ReceiptSettings `json:"settings"`
}

// liveHeld reports whether receipt rc still holds its ticket: it says held,
// and the ticket is still in progress with no dispatch. A ticket moved,
// dispatched or completed some other way isn't waiting on a person.
func liveHeld(rc domain.Receipt, state domain.TaskState, dispatchID string) bool {
	return rc.Held && state == domain.TaskStateInProgress && dispatchID == ""
}

// receiptViews derives each receipt's status, and whether it holds ticket t
// (nil: none of them does); minLevel words a held one's reason.
func (s *Service) receiptViews(ctx context.Context, list []domain.Receipt, withEvidence bool, minLevel string, t *domain.Task) ([]ReceiptView, error) {
	ids := make([]int64, len(list))
	for i, rc := range list {
		ids[i] = rc.ID
	}
	checks, err := s.Repo.ReceiptChecks(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]ReceiptView, len(list))
	for i, rc := range list {
		cs := checks[rc.ID]
		if cs == nil {
			cs = []domain.ReceiptCheck{}
		}
		held := t != nil && liveHeld(rc, t.State, t.DispatchID)
		rc.Held = held
		out[i] = ReceiptView{Receipt: rc, Checks: cs, Evidence: []domain.ReceiptEvidence{}, Status: domain.DeriveStatus(rc, cs), Held: held}
		if held {
			out[i].HoldReason = workerHoldReason(rc, cs, minLevel)
		}
		if withEvidence {
			if ev, err := s.Repo.ReceiptEvidence(ctx, rc.ID); err == nil {
				s.withExpiry(rc, ev)
				out[i].Evidence = ev
			}
		}
	}
	return out, nil
}

// workerHoldReason is holdReason over a receipt's worker checks.
func workerHoldReason(rc domain.Receipt, cs []domain.ReceiptCheck, minLevel string) string {
	worker := []domain.ReceiptCheck{}
	for _, c := range cs {
		if c.Role == domain.CheckRoleWorker {
			worker = append(worker, c)
		}
	}
	return holdReason(worker, domain.DeriveStatus(rc, worker), minLevel)
}

// heldReason is why ticket t's latest receipt holds it ("" when it can't
// be read), for My work's verify items.
func (s *Service) heldReason(ctx context.Context, t *domain.Task) string {
	rc, err := s.latestFinal(ctx, t.ID)
	if err != nil {
		return ""
	}
	checks, err := s.Repo.ReceiptChecks(ctx, []int64{rc.ID})
	if err != nil {
		return ""
	}
	rs, err := s.EffectiveReceipts(ctx, t)
	if err != nil {
		return ""
	}
	return workerHoldReason(*rc, checks[rc.ID], rs.MinLevel)
}

// GetTaskReceipt is ticket taskID's latest final receipt, for anyone who can
// read the ticket.
func (s *Service) GetTaskReceipt(ctx context.Context, p authz.Principal, taskID int) (*TaskReceipt, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskRead)
	if err != nil {
		return nil, err
	}
	rs, err := s.EffectiveReceipts(ctx, t)
	if err != nil {
		return nil, err
	}
	out := &TaskReceipt{Settings: rs}
	list, err := s.Repo.TaskReceipts(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return out, nil
	}
	views, err := s.receiptViews(ctx, list[:1], true, rs.MinLevel, t)
	if err != nil {
		return nil, err
	}
	out.ReceiptView, out.Attempts = &views[0], len(list)-1
	return out, nil
}

// ListTaskReceipts is every final receipt of ticket taskID, newest first.
func (s *Service) ListTaskReceipts(ctx context.Context, p authz.Principal, taskID int) ([]ReceiptView, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskRead)
	if err != nil {
		return nil, err
	}
	rs, err := s.EffectiveReceipts(ctx, t)
	if err != nil {
		return nil, err
	}
	list, err := s.Repo.TaskReceipts(ctx, taskID)
	if err != nil {
		return nil, err
	}
	// Only the latest receipt can hold the ticket.
	views, err := s.receiptViews(ctx, list, true, rs.MinLevel, nil)
	if err == nil && len(views) > 0 {
		if v, err := s.receiptViews(ctx, list[:1], true, rs.MinLevel, t); err == nil {
			views[0] = v[0]
		}
	}
	return views, err
}

// ReceiptSummaryRow is one ticket's line in the project summary.
type ReceiptSummaryRow struct {
	TaskID   int    `json:"task_id"`
	Status   string `json:"status"`
	Held     bool   `json:"held"`
	HasMedia bool   `json:"has_media"`
	// Reason is why a held one waits for a person.
	Reason string `json:"reason,omitempty"`
}

// ReceiptSummary is GET /api/projects/:id/receipts/summary: the project's
// setting, every ticket's latest receipt status (Board badges, Mission's
// unverified count, My work's held asks), and how many agent tickets
// completed in the last 7 days Require would have held (Project settings'
// warning before saving Require).
type ReceiptSummary struct {
	ReceiptSettings
	Tasks       []ReceiptSummaryRow `json:"tasks"`
	WouldHold7d int                 `json:"would_hold_last_7_days"`
}

// ProjectReceiptSummary is the summary for a project member.
func (s *Service) ProjectReceiptSummary(ctx context.Context, p authz.Principal, projectID int) (*ReceiptSummary, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member); err != nil {
		return nil, err
	}
	rs, err := s.ProjectReceiptSettings(ctx, projectID)
	if err != nil {
		return nil, err
	}
	list, media, err := s.Repo.ProjectLatestReceipts(ctx, projectID)
	if err != nil {
		return nil, err
	}
	receipts := make([]domain.Receipt, len(list))
	for i, lr := range list {
		receipts[i] = lr.Receipt
	}
	views, err := s.receiptViews(ctx, receipts, false, rs.MinLevel, nil)
	if err != nil {
		return nil, err
	}
	out := &ReceiptSummary{ReceiptSettings: rs, Tasks: make([]ReceiptSummaryRow, len(views))}
	byTask := map[int]string{}
	for i, v := range views {
		row := ReceiptSummaryRow{TaskID: v.Receipt.TaskID, Status: v.Status, HasMedia: media[v.Receipt.ID]}
		if liveHeld(list[i].Receipt, list[i].TaskState, list[i].TaskDispatch) {
			row.Held, row.Reason = true, workerHoldReason(list[i].Receipt, v.Checks, rs.MinLevel)
		}
		out.Tasks[i] = row
		byTask[v.Receipt.TaskID] = v.Status
	}
	if recent, err := s.Repo.RecentAgentCompletions(ctx, projectID, time.Now().Add(-7*24*time.Hour)); err == nil {
		for _, id := range recent {
			if st, ok := byTask[id]; !ok || !domain.MeetsMinimum(st, rs.MinLevel) {
				out.WouldHold7d++
			}
		}
	}
	return out, nil
}

// ---- A person's verdict: Accept and Reject ----

// latestFinal is ticket taskID's latest final receipt, or a 404.
func (s *Service) latestFinal(ctx context.Context, taskID int) (*domain.Receipt, error) {
	list, err := s.Repo.TaskReceipts(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, authz.NotFound("ticket %d has no receipt", taskID)
	}
	return &list[0], nil
}

// AcceptReceipt records a person's Accept of ticket taskID's latest receipt
// (web UI only, someone who can work the ticket): a human pass verdict, and
// the receipt is kept. A held receipt's ticket is completed by it.
func (s *Service) AcceptReceipt(ctx context.Context, p authz.Principal, taskID int, note string) (*ReceiptView, error) {
	return s.verdict(ctx, p, taskID, domain.VerdictPass, note)
}

// RejectReceipt records a person's Reject (with a note): a human fail
// verdict. A held receipt's ticket goes back to ready, with the note on it.
func (s *Service) RejectReceipt(ctx context.Context, p authz.Principal, taskID int, note string) (*ReceiptView, error) {
	return s.verdict(ctx, p, taskID, domain.VerdictFail, note)
}

func (s *Service) verdict(ctx context.Context, p authz.Principal, taskID int, verdict, note string) (*ReceiptView, error) {
	what := "Accepting a receipt"
	action := "receipt.accept"
	if verdict == domain.VerdictFail {
		what, action = "Rejecting a receipt", "receipt.reject"
	}
	// Authorized first, so a caller who can't see the ticket learns nothing
	// about it.
	cur, _, err := s.LoadTask(ctx, p, taskID, authz.TaskRead)
	if err != nil {
		return nil, err
	}
	if err := authz.RequireUI(p, what, s.TaskURL(cur.ProjectID, taskID)); err != nil {
		s.auditTaskDenied(ctx, p, action, cur, err, nil)
		return nil, err
	}
	note = strings.TrimSpace(note)
	if verdict == domain.VerdictFail && note == "" {
		return nil, authz.BadRequest("a Reject needs a note: what's wrong, for whoever picks the ticket up next")
	}
	if len(note) > domain.MaxCheckNoteBytes {
		return nil, authz.BadRequest("note: at most %d bytes", domain.MaxCheckNoteBytes)
	}
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskWork)
	if err != nil {
		return nil, err
	}
	rc, err := s.latestFinal(ctx, taskID)
	if err != nil {
		return nil, err
	}
	held := liveHeld(*rc, t.State, t.DispatchID)
	// The verdict first, then the move: a completed ticket never lacks the
	// Accept that completed it. If the move fails, the verdict is taken
	// back, so nothing is recorded and a retry starts clean.
	check, err := s.Repo.AddReceiptCheck(ctx, domain.ReceiptCheck{ReceiptID: rc.ID, Role: domain.CheckRoleHuman, Kind: domain.CheckKindVerdict,
		Verdict: verdict, Note: note, CheckedBy: p.Label(), UserID: p.UserRef()})
	if err != nil {
		return nil, err
	}
	if held {
		if err := s.releaseHeld(ctx, p, t, rc, verdict, note); err != nil {
			if derr := s.Repo.DeleteReceiptCheck(context.WithoutCancel(ctx), check.ID); derr != nil {
				slog.Error("receipt verdict: the move failed and the verdict couldn't be taken back", "task_id", taskID,
					"check_id", check.ID, "err", derr)
			}
			return nil, err
		}
	}
	if verdict == domain.VerdictPass {
		// Accepted work's receipts are all kept (D13).
		if err := s.Repo.KeepTaskReceipts(ctx, taskID); err != nil {
			return nil, err
		}
	}
	summary := fmt.Sprintf("accepted the receipt of ticket %d", taskID)
	if verdict == domain.VerdictFail {
		summary = fmt.Sprintf("rejected the receipt of ticket %d: %s", taskID, note)
	}
	s.audit(ctx, p, domain.AuditEntry{Action: action, ProjectID: intRef(t.ProjectID), TargetType: "task", TargetID: idStr(taskID),
		TargetName: t.Name, Summary: summary, Details: map[string]any{"receipt_id": rc.ID, "held": held, "note": note}})
	if cur, err := s.Repo.GetReceipt(ctx, rc.ID); err == nil {
		rc = cur
	}
	if now, err := s.Repo.GetTask(ctx, taskID); err == nil {
		t = now
	}
	rs, _ := s.EffectiveReceipts(ctx, t)
	views, err := s.receiptViews(ctx, []domain.Receipt{*rc}, true, rs.MinLevel, t)
	if err != nil {
		return nil, err
	}
	v := views[0]
	s.notifyReceipt(t.ProjectID, taskID, rc.ID, v.Status, v.Held)
	return &v, nil
}

// releaseHeld acts on a person's verdict on a held receipt: Accept
// completes the ticket (with the held run, so a PR still waits on its
// merge), Reject returns it to ready with the note on it. The hold ends
// only once that worked.
func (s *Service) releaseHeld(ctx context.Context, p authz.Principal, t *domain.Task, rc *domain.Receipt, verdict, note string) error {
	if verdict == domain.VerdictPass {
		var run *domain.Run
		if rc.RunID != nil {
			if runs, err := s.Repo.TaskRuns(ctx, t.ID); err == nil {
				for i := range runs {
					if runs[i].ID == *rc.RunID {
						run = &runs[i]
					}
				}
			}
		}
		if t.State != domain.TaskStateCompleted {
			if _, _, err := s.completeTask(ctx, p, t.ID, run, CompleteClaim{}, &ReceiptIn{skip: true}); err != nil {
				return err
			}
		}
	} else {
		if t.State != domain.TaskStateBacklog {
			if _, err := s.SetTaskState(ctx, p, t.ID, domain.TaskStateBacklog, ""); err != nil {
				return err
			}
		}
		s.comment(t.ProjectID, t.ID, p, "Receipt rejected: "+note)
	}
	return s.Repo.ClearTaskHeld(ctx, t.ID)
}

// KeepReceipts keeps every receipt of a ticket whose merge was just
// recorded (D13). A failure is logged: the sweep would only expire evidence.
func (s *Service) KeepReceipts(ctx context.Context, taskID int) {
	if err := s.Repo.KeepTaskReceipts(ctx, taskID); err != nil {
		slog.Warn("keep receipts on merge", "task_id", taskID, "err", err)
	}
}

// ---- The setting ----

// SetOrgReceiptsMode sets an org's default receipts mode: org owners and
// admins, in the web UI only (D9). Audited with before and after.
func (s *Service) SetOrgReceiptsMode(ctx context.Context, p authz.Principal, orgID int, mode string) (*domain.Org, error) {
	if err := authz.RequireUI(p, "Changing the org's verification receipts default", s.link(fmt.Sprintf("/orgs/%d", orgID))); err != nil {
		return nil, err
	}
	if _, err := s.Authz.RequireOrgAdmin(ctx, p, orgID); err != nil {
		return nil, err
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if !domain.ValidReceiptsMode(mode) {
		return nil, authz.BadRequest("mode must be off, inform or require")
	}
	before, err := s.Repo.GetOrg(ctx, orgID)
	if err != nil {
		return nil, authz.NotFound("org %d not found", orgID)
	}
	if err := s.Repo.SetOrgReceiptsMode(ctx, orgID, mode); err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{OrgID: intRef(orgID), Action: "org.receipts.set", TargetType: "org", TargetID: idStr(orgID),
		TargetName: before.Name, Summary: fmt.Sprintf("set the verification receipts default to %s", mode),
		Before: map[string]any{"receipts_mode": before.ReceiptsMode}, After: map[string]any{"receipts_mode": mode}})
	return s.Repo.GetOrg(ctx, orgID)
}

// checkReceiptsSettings validates a project settings change's receipts
// fields and refuses them off the web UI (D9).
func (s *Service) checkReceiptsSettings(p authz.Principal, projectID int, u *domain.ProjectSettings) error {
	if u.ReceiptsMode == nil && u.ReceiptsMinLevel == nil {
		return nil
	}
	if err := authz.RequireUI(p, "Changing the verification receipts setting", s.SettingsURL(projectID)); err != nil {
		return err
	}
	if u.ReceiptsMode != nil {
		m := strings.ToLower(strings.TrimSpace(*u.ReceiptsMode))
		if m != "" && !domain.ValidReceiptsMode(m) {
			return authz.BadRequest("receipts_mode must be \"\" (the org's default), off, inform or require")
		}
		u.ReceiptsMode = &m
	}
	if u.ReceiptsMinLevel != nil {
		l := strings.ToLower(strings.TrimSpace(*u.ReceiptsMinLevel))
		if err := v1MinLevel(l); err != nil {
			return err
		}
		u.ReceiptsMinLevel = &l
	}
	return nil
}

// ---- Metadata overrides ----

// applyPatch is own with patch applied.
func applyPatch(own domain.Metadata, patch domain.MetadataPatch) domain.Metadata {
	out := domain.Metadata{}
	for k, v := range own {
		out[k] = v
	}
	for k, v := range patch {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

// guardReceiptsMeta refuses a metadata change that sets a receipts key to
// an invalid value (anyone), or that weakens the effective setting at that
// item (off the web UI, D11): a lower mode or minimum level than before.
// inherited is what the item inherits, own its metadata before the change.
func (s *Service) guardReceiptsMeta(ctx context.Context, p authz.Principal, projectID int, inherited, own domain.Metadata, patch domain.MetadataPatch, where string) error {
	if !patch.TouchesReceipts() {
		return nil
	}
	if err := validReceiptsPatch(patch); err != nil {
		return err
	}
	if p.InUI() {
		return nil
	}
	// An item's own values only rise off the web UI, whatever its effective
	// mode: lowering an epic's or story's minimum while it inherits Inform
	// would otherwise weaken a ticket under it that sets Require itself.
	if err := ownValuesRise(own, patch); err != "" {
		return authz.UIOnly(fmt.Sprintf("Lowering or removing %s's own %s", where, err), s.SettingsURL(projectID))
	}
	proj, err := s.Repo.GetProject(ctx, projectID)
	if err != nil {
		return &authz.Error{Status: 503, Msg: fmt.Sprintf("couldn't load project %d to check its receipts setting (%v); nothing changed, try again", projectID, err)}
	}
	base := s.projectReceipts(ctx, proj)
	if inherited == nil {
		inherited = domain.Metadata{}
	}
	before := withOverride(base, inherited.Merge(own))
	after := withOverride(base, inherited.Merge(applyPatch(own, patch)))
	if weakens(before, after) {
		return authz.UIOnly(fmt.Sprintf("Weakening the verification receipts requirement on %s (from %s to %s)", where,
			describeReceipts(before), describeReceipts(after)), s.SettingsURL(projectID))
	}
	return nil
}

// validReceiptsPatch refuses invalid receipts values in a metadata change.
func validReceiptsPatch(patch domain.MetadataPatch) error {
	if v, ok := patch[domain.MetaReceipts]; ok && v != nil {
		if str, isStr := v.(string); !isStr || !domain.ValidReceiptsMode(strings.ToLower(strings.TrimSpace(str))) {
			return authz.BadRequest("metadata %s must be off, inform or require (null removes it)", domain.MetaReceipts)
		}
	}
	if v, ok := patch[domain.MetaReceiptsMinLevel]; ok && v != nil {
		str, isStr := v.(string)
		if !isStr {
			return authz.BadRequest("metadata %s must be self_checked (null removes it)", domain.MetaReceiptsMinLevel)
		}
		if err := v1MinLevel(strings.ToLower(strings.TrimSpace(str))); err != nil {
			return err
		}
	}
	return nil
}

// v1MinLevel refuses a minimum level v1 can't reach without a person:
// independently_verified arrives with the verifier, so until then only
// self_checked can be set (owner decision).
func v1MinLevel(l string) error {
	switch {
	case l == domain.StatusSelfChecked:
		return nil
	case l == domain.StatusIndependentlyVerified:
		return authz.BadRequest("receipts_min_level independently_verified isn't available yet: it arrives with the independent " +
			"verifier. v1 supports self_checked only")
	}
	return authz.BadRequest("receipts_min_level must be self_checked (v1 supports self_checked only)")
}

// ownValuesRise checks that patch only raises (or first sets) the receipts
// values own already has: removing one, or setting a lower mode or minimum
// level, is refused. It returns the key that would fall ("" when none).
func ownValuesRise(own domain.Metadata, patch domain.MetadataPatch) string {
	norm := func(v any) string {
		str, _ := v.(string)
		return strings.ToLower(strings.TrimSpace(str))
	}
	for key, rank := range map[string]func(string) int{domain.MetaReceipts: domain.ModeRank, domain.MetaReceiptsMinLevel: domain.StatusRank} {
		nv, touched := patch[key]
		if !touched {
			continue
		}
		old := norm(own[key])
		if rank(old) < 0 {
			continue // no valid own value: nothing to lower
		}
		if nv == nil || rank(norm(nv)) < rank(old) {
			return fmt.Sprintf("metadata %s (%s)", key, old)
		}
	}
	return ""
}

// weakens reports whether after is a weaker receipts requirement than
// before: a lower mode, or under Require a lower minimum level.
func weakens(before, after ReceiptSettings) bool {
	return domain.ModeRank(after.Mode) < domain.ModeRank(before.Mode) ||
		(after.Mode == domain.ReceiptsRequire && domain.StatusRank(after.MinLevel) < domain.StatusRank(before.MinLevel))
}

// inheritedForGuard is InheritedMetadata for a receipts guard. It fails
// closed: a read error refuses the change (retryable, nothing changed)
// rather than judging it against nothing inherited, which could let a
// weakening through.
func (s *Service) inheritedForGuard(ctx context.Context, kind string, id int) (domain.Metadata, error) {
	m, err := s.Repo.InheritedMetadata(ctx, kind, id)
	if err != nil {
		return nil, guardReadError(fmt.Sprintf("%s %d's inherited metadata", kind, id), err)
	}
	if m == nil {
		m = domain.Metadata{}
	}
	return m, nil
}

// guardReadError is the retryable refusal when a receipts guard can't read
// what it needs.
func guardReadError(what string, err error) error {
	return &authz.Error{Status: 503, Msg: fmt.Sprintf("couldn't read %s to check the receipts setting (%v); nothing changed, try again", what, err)}
}

// GuardReceiptsMoveTask refuses, off the web UI, moving ticket t under
// story when that weakens its effective receipts setting (D11): its own
// metadata stays, what it inherits changes.
func (s *Service) GuardReceiptsMoveTask(ctx context.Context, p authz.Principal, t *domain.Task, story *domain.Story) error {
	if p.InUI() {
		return nil
	}
	proj, err := s.Repo.GetProject(ctx, t.ProjectID)
	if err != nil {
		return &authz.Error{Status: 503, Msg: fmt.Sprintf("couldn't load project %d to check its receipts setting (%v); nothing changed, try again", t.ProjectID, err)}
	}
	base := s.projectReceipts(ctx, proj)
	fromInh, err := s.inheritedForGuard(ctx, "task", t.ID)
	if err != nil {
		return err
	}
	toInh, err := s.inheritedForGuard(ctx, "story", story.ID)
	if err != nil {
		return err
	}
	before := withOverride(base, fromInh.Merge(t.Metadata))
	after := withOverride(base, toInh.Merge(story.Metadata).Merge(t.Metadata))
	if weakens(before, after) {
		return authz.UIOnly(fmt.Sprintf("Moving ticket %d to story %d, which weakens its verification receipts requirement (from %s to %s),",
			t.ID, story.ID, describeReceipts(before), describeReceipts(after)), s.SettingsURL(t.ProjectID))
	}
	return nil
}

// GuardReceiptsMoveStory is GuardReceiptsMoveTask for a story moving under
// epic: the story itself and every ticket in it.
func (s *Service) GuardReceiptsMoveStory(ctx context.Context, p authz.Principal, story *domain.Story, epic *domain.Epic) error {
	if p.InUI() {
		return nil
	}
	proj, err := s.Repo.GetProject(ctx, story.ProjectID)
	if err != nil {
		return &authz.Error{Status: 503, Msg: fmt.Sprintf("couldn't load project %d to check its receipts setting (%v); nothing changed, try again", story.ProjectID, err)}
	}
	base := s.projectReceipts(ctx, proj)
	epicInh, err := s.inheritedForGuard(ctx, "epic", epic.ID)
	if err != nil {
		return err
	}
	under := epicInh.Merge(epic.Metadata).Merge(story.Metadata)
	refuse := func(what string, before, after ReceiptSettings) error {
		return authz.UIOnly(fmt.Sprintf("Moving story %d to epic %d, which weakens the verification receipts requirement of %s (from %s to %s),",
			story.ID, epic.ID, what, describeReceipts(before), describeReceipts(after)), s.SettingsURL(story.ProjectID))
	}
	storyInh, err := s.inheritedForGuard(ctx, "story", story.ID)
	if err != nil {
		return err
	}
	before := withOverride(base, storyInh.Merge(story.Metadata))
	if after := withOverride(base, under); weakens(before, after) {
		return refuse(fmt.Sprintf("story %d", story.ID), before, after)
	}
	ids, err := s.Repo.TaskIDsInStories(ctx, []int{story.ID})
	if err != nil {
		return guardReadError(fmt.Sprintf("story %d's tickets", story.ID), err)
	}
	for _, id := range ids {
		// Every ticket is checked; one that can't be read refuses the move.
		t, err := s.Repo.GetTask(ctx, id)
		if err != nil {
			return guardReadError(fmt.Sprintf("ticket %d", id), err)
		}
		taskInh, err := s.inheritedForGuard(ctx, "task", id)
		if err != nil {
			return err
		}
		before := withOverride(base, taskInh.Merge(t.Metadata))
		if after := withOverride(base, under.Merge(t.Metadata)); weakens(before, after) {
			return refuse(fmt.Sprintf("its ticket %d", id), before, after)
		}
	}
	return nil
}

func describeReceipts(rs ReceiptSettings) string {
	if rs.Mode == domain.ReceiptsRequire {
		return "require " + rs.MinLevel
	}
	return rs.Mode
}

// GuardReceiptsUpdate is guardReceiptsMeta for a change to an existing
// project, epic, story or ticket's metadata.
func (s *Service) GuardReceiptsUpdate(ctx context.Context, p authz.Principal, kind string, id int, patch domain.MetadataPatch) error {
	if !patch.TouchesReceipts() {
		return nil
	}
	var projectID int
	var own domain.Metadata
	switch kind {
	case "project":
		proj, err := s.Repo.GetProject(ctx, id)
		if err != nil {
			return authz.NotFound("project %d not found", id)
		}
		projectID, own = proj.ID, proj.Metadata
	case "epic":
		e, err := s.Repo.GetEpic(ctx, id)
		if err != nil {
			return authz.NotFound("epic %d not found", id)
		}
		projectID, own = e.ProjectID, e.Metadata
	case "story":
		st, err := s.Repo.GetStory(ctx, id)
		if err != nil {
			return authz.NotFound("story %d not found", id)
		}
		projectID, own = st.ProjectID, st.Metadata
	case "task":
		t, err := s.Repo.GetTask(ctx, id)
		if err != nil {
			return authz.NotFound("task %d not found", id)
		}
		projectID, own = t.ProjectID, t.Metadata
	default:
		return fmt.Errorf("unknown item kind %q", kind)
	}
	inherited, err := s.Repo.InheritedMetadata(ctx, kind, id)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	return s.guardReceiptsMeta(ctx, p, projectID, inherited, own, patch, fmt.Sprintf("%s %d", kind, id))
}

// GuardReceiptsCreate is guardReceiptsMeta for a new item with metadata
// meta under parent (parentKind "project" for an epic, "epic" for a story,
// "story" for a ticket).
func (s *Service) GuardReceiptsCreate(ctx context.Context, p authz.Principal, parentKind string, parentID int, meta domain.Metadata) error {
	patch := domain.MetadataPatch(meta)
	if !patch.TouchesReceipts() {
		return nil
	}
	var projectID int
	var parentOwn domain.Metadata
	switch parentKind {
	case "project":
		proj, err := s.Repo.GetProject(ctx, parentID)
		if err != nil {
			return authz.NotFound("project %d not found", parentID)
		}
		projectID, parentOwn = proj.ID, proj.Metadata
	case "epic":
		e, err := s.Repo.GetEpic(ctx, parentID)
		if err != nil {
			return authz.NotFound("epic %d not found", parentID)
		}
		projectID, parentOwn = e.ProjectID, e.Metadata
	case "story":
		st, err := s.Repo.GetStory(ctx, parentID)
		if err != nil {
			return authz.NotFound("story %d not found", parentID)
		}
		projectID, parentOwn = st.ProjectID, st.Metadata
	default:
		return fmt.Errorf("unknown parent kind %q", parentKind)
	}
	inherited, err := s.Repo.InheritedMetadata(ctx, parentKind, parentID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	if inherited == nil {
		inherited = domain.Metadata{}
	}
	return s.guardReceiptsMeta(ctx, p, projectID, inherited.Merge(parentOwn), domain.Metadata{}, patch, "a new item under "+parentKind+" "+idStr(parentID))
}

// GuardReceiptsNewProject refuses a new project's metadata when its
// receipts keys are invalid (anyone), or, off the web UI, when they weaken
// the org's default (D9, D11): an agent can't create its way out of the
// org's setting.
func (s *Service) GuardReceiptsNewProject(ctx context.Context, p authz.Principal, orgID *int, meta domain.Metadata) error {
	patch := domain.MetadataPatch(meta)
	if !patch.TouchesReceipts() {
		return nil
	}
	if err := validReceiptsPatch(patch); err != nil {
		return err
	}
	if p.InUI() {
		return nil
	}
	base := ReceiptSettings{Mode: domain.ReceiptsOff, MinLevel: domain.StatusSelfChecked}
	if orgID != nil {
		org, err := s.Repo.GetOrg(ctx, *orgID)
		if err != nil {
			return guardReadError(fmt.Sprintf("org %d", *orgID), err)
		}
		if domain.ValidReceiptsMode(org.ReceiptsMode) {
			base.Mode = org.ReceiptsMode
		}
	}
	if after := withOverride(base, meta); weakens(base, after) {
		return authz.UIOnly(fmt.Sprintf("Creating a project whose metadata weakens the org's verification receipts default (from %s to %s)",
			describeReceipts(base), describeReceipts(after)), s.link("/projects"))
	}
	return nil
}

// StripImportedReceipts removes, off the web UI, every receipts metadata key
// from an import bundle's project, epics, stories and tickets, so an agent
// can't import a weaker setting: the new project follows the org default
// until a person sets it. It returns how many keys it removed. In the web UI
// the bundle is kept as it is (invalid values are dropped by the import's
// metadata cleaning either way).
func (s *Service) StripImportedReceipts(p authz.Principal, b *domain.ExportBundle) int {
	if p.InUI() {
		return 0
	}
	n := 0
	strip := func(m domain.Metadata) {
		for _, k := range []string{domain.MetaReceipts, domain.MetaReceiptsMinLevel} {
			if _, ok := m[k]; ok {
				delete(m, k)
				n++
			}
		}
	}
	strip(b.Project.Metadata)
	for i := range b.Epics {
		strip(b.Epics[i].Metadata)
	}
	for i := range b.Stories {
		strip(b.Stories[i].Metadata)
	}
	for i := range b.Tasks {
		strip(b.Tasks[i].Metadata)
	}
	return n
}

// guardReceiptsOrgMove refuses, off the web UI, moving project proj into
// org orgID when that weakens its effective receipts setting (D9): a
// project that follows its org's default would otherwise leave a Require
// org for an Off one, which turns Require off for every ticket in it. Both
// orgs are read strictly: one that can't be read refuses the move.
func (s *Service) guardReceiptsOrgMove(ctx context.Context, p authz.Principal, proj *domain.Project, orgID int) error {
	if p.InUI() {
		return nil
	}
	orgMode := func(id *int) (string, error) {
		if id == nil {
			return domain.ReceiptsOff, nil
		}
		org, err := s.Repo.GetOrg(ctx, *id)
		if err != nil {
			return "", guardReadError(fmt.Sprintf("org %d", *id), err)
		}
		if domain.ValidReceiptsMode(org.ReceiptsMode) {
			return org.ReceiptsMode, nil
		}
		return domain.ReceiptsOff, nil
	}
	settings := func(orgMode string) ReceiptSettings {
		rs := ReceiptSettings{Mode: orgMode, MinLevel: proj.ReceiptsMinLevel}
		if domain.ValidReceiptsMode(proj.ReceiptsMode) {
			rs.Mode = proj.ReceiptsMode
		}
		if !domain.ValidMinLevel(rs.MinLevel) {
			rs.MinLevel = domain.StatusSelfChecked
		}
		return withOverride(rs, proj.Metadata)
	}
	from, err := orgMode(proj.OrgID)
	if err != nil {
		return err
	}
	to, err := orgMode(&orgID)
	if err != nil {
		return err
	}
	before, after := settings(from), settings(to)
	if weakens(before, after) {
		return authz.UIOnly(fmt.Sprintf("Moving project %d to org %d, which weakens its verification receipts setting (from %s to %s),",
			proj.ID, orgID, describeReceipts(before), describeReceipts(after)), s.SettingsURL(proj.ID))
	}
	return nil
}
