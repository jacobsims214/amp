package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// Decision docs (docs/specs/human-decision-docs.md §2, §3): a judgment
// ticket's ask renders its decision doc v1, a markdown artifact (role
// decision, kept, no expiry) whose version metadata holds the options. The
// doc changes only in AMP; agents can draft it but never record it.

// DecisionDocWarning is returned with a judgment ticket whose doc could not
// be written: the ticket exists, and with no doc a retry is allowed.
const DecisionDocWarning = "decision doc not created; set ask again with amp_update_task"

// The guards' refusals (§3 Guards); each ends with the ticket's link.
const (
	decisionPublishRefusal    = "Decision docs change in AMP, not by publish: %s"
	decisionPersonOnlyRefusal = "Only a person records a decision, in AMP: %s"
	decisionReviewRefusal     = "Decision docs are approved and kept by Record decision: %s"
	decisionRecordedRefusal   = "This decision is recorded and can't change: %s"
	decisionOptionsRefusal    = "This ticket's options are in its decision doc; the owner edits them in AMP: %s"
	decisionMarkDoneRefusal   = "Record the decision on its doc: %s"
	decisionRecordingRefusal  = "This decision is already being recorded: %s"
	// DecisionStaleRefusal and DecisionCapRefusal are PUT doc's 409s (§3
	// Editing); the card shows them and keeps the draft.
	DecisionStaleRefusal = "The doc changed in another tab. Reload to see it"
	DecisionCapRefusal   = "This doc has 100 versions; record the decision or start a new judgment ticket"
	askOnlyJudgment      = "ask only goes with a judgment ticket (performer \"human\", human_reason \"judgment\")"
)

// UpstreamDecisions returns what people decided on the ticket's direct
// dependencies: for each human dependency, its decision doc's record once
// Record decision has claimed it, otherwise (completed tickets only) its
// latest non-empty human run note. A dependency with neither, or that isn't
// a human ticket in the same project, is left out. The caller needs to be
// able to read the ticket.
func (s *Service) UpstreamDecisions(ctx context.Context, p authz.Principal, taskID int) ([]domain.UpstreamDecision, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskRead)
	if err != nil {
		return nil, err
	}
	return s.upstreamDecisionsFor(ctx, t)
}

func (s *Service) upstreamDecisionsFor(ctx context.Context, t *domain.Task) ([]domain.UpstreamDecision, error) {
	out := []domain.UpstreamDecision{}
	names := map[int]string{}
	nameOf := func(id *int) string {
		if id == nil {
			return ""
		}
		name, ok := names[*id]
		if !ok {
			if u, err := s.Repo.GetUserByID(ctx, *id); err == nil && u != nil {
				name = userLabel(u)
			}
			names[*id] = name
		}
		return name
	}
	for _, depID := range t.DependencyIDs {
		dep, err := s.Repo.GetTask(ctx, depID)
		if err != nil || dep.ProjectID != t.ProjectID || !dep.IsHuman() {
			continue
		}
		doc, err := s.Repo.GetDecisionDoc(ctx, depID)
		if err != nil {
			return nil, err
		}
		if doc.Claimed() {
			out = append(out, s.recordedDecision(ctx, dep, doc, nameOf))
			continue
		}
		if dep.State != domain.TaskStateCompleted {
			continue
		}
		runs, err := s.Repo.TaskRuns(ctx, depID)
		if err != nil {
			return nil, err
		}
		var latest *domain.Run
		for i := range runs {
			r := &runs[i]
			if r.AgentID == domain.HumanAgentID && r.Outcome == domain.RunCompleted && r.Note != "" {
				latest = r // runs are oldest first
			}
		}
		if latest == nil {
			continue
		}
		out = append(out, domain.UpstreamDecision{TaskID: dep.ID, Name: dep.Name, Reason: dep.HumanReason,
			Decision: latest.Note, By: latest.UserID, ByName: nameOf(latest.UserID), At: latest.CreatedAt})
	}
	return out, nil
}

// recordedDecision is a claimed decision doc record as an upstream
// decision: the chosen option's label as stored on the decided version, and
// the decided text.
func (s *Service) recordedDecision(ctx context.Context, dep *domain.Task, doc *domain.DecisionDoc, nameOf func(*int) string) domain.UpstreamDecision {
	version := *doc.DecidedVersion
	d := domain.UpstreamDecision{TaskID: dep.ID, Name: dep.Name, Reason: dep.HumanReason, Decision: doc.DecisionText,
		DocURL: s.decisionDocVersionURL(doc.ArtifactID, version), Version: version, By: doc.DecidedBy, ByName: nameOf(doc.DecidedBy)}
	if doc.DecidedAt != nil {
		d.At = *doc.DecidedAt
	} else if doc.RecordingStartedAt != nil {
		d.At = *doc.RecordingStartedAt
	}
	if doc.ChosenOption != nil {
		if v, err := s.Repo.GetArtifactVersion(ctx, doc.ArtifactID, version); err == nil && v.DecisionAsk != nil {
			if i := *doc.ChosenOption; i >= 0 && i < len(v.DecisionAsk.Options) {
				d.Option = v.DecisionAsk.Options[i].Label
				d.Decision = d.Option + " — " + doc.DecisionText
			}
		}
	}
	return d
}

// decisionDocVersionURL is <doc link vN>: /artifacts/<id>?v=N.
func (s *Service) decisionDocVersionURL(artifactID, version int) string {
	return s.link(fmt.Sprintf("/artifacts/%d?v=%d", artifactID, version))
}

// checkHumanBrief applies the brief rule (§2) to a ticket as it will be:
// a human ticket of any reason needs a description and acceptance criteria,
// and a judgment ticket made off the web UI needs a valid ask. An ask is
// refused on any other ticket. ref, when set, names the batch ticket.
func (s *Service) checkHumanBrief(p authz.Principal, performer, desc, criteria, reason string, ask *domain.Ask, ref string) error {
	prefix := ""
	if ref != "" {
		prefix = fmt.Sprintf("%q: ", ref)
	}
	if performer != domain.PerformerHuman || reason != "judgment" {
		if ask != nil {
			return authz.BadRequest("%s%s", prefix, askOnlyJudgment)
		}
		if performer != domain.PerformerHuman {
			return nil
		}
	}
	missing := domain.HumanBriefMissing(desc, criteria, reason, ask)
	if p.InUI() {
		// The web UI has no ask editor: a person's judgment ticket without
		// one has no doc, and its owner writes the decision when done.
		missing = removeString(missing, "ask")
	}
	if len(missing) == 0 {
		return nil
	}
	msg := fmt.Sprintf("%sa human ticket needs a brief written for the person who does it; missing: %s", prefix, strings.Join(missing, ", "))
	if reason == "judgment" && !p.InUI() {
		msg += ". A judgment ticket's ask is {question, options: [{label, consequence}] (2 to 4), recommended (index), recommendation_reason, evidence}"
	}
	return authz.BadRequest("%s", msg)
}

func removeString(xs []string, x string) []string {
	out := xs[:0:0]
	for _, v := range xs {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

// createDecisionDoc renders a judgment ticket's doc v1 from its ask and
// writes it (Repo.CreateDecisionDoc). The ask must be valid.
func (s *Service) createDecisionDoc(ctx context.Context, p authz.Principal, t *domain.Task, ask *domain.Ask) (*domain.Artifact, error) {
	if s.DecisionDocFault != nil {
		if err := s.DecisionDocFault(t.ID); err != nil {
			return nil, err
		}
	}
	body := s.renderDecisionDoc(ctx, t, ask)
	a, err := s.Repo.CreateDecisionDoc(ctx, t.ID, domain.Artifact{ProjectID: t.ProjectID, Title: "Decision: " + t.Name, CreatedBy: p.UserRef()},
		[]byte(body), "text/markdown; charset=utf-8", "Drafted from the ticket's ask", ask.Stored())
	if err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "decision.create", ProjectID: intRef(t.ProjectID), TargetType: "task", TargetID: idStr(t.ID),
		TargetName: t.Name, Summary: fmt.Sprintf("drafted decision doc artifact #%d v1 for ticket %d", a.ID, t.ID),
		Details: map[string]any{"artifact_id": a.ID, "options": len(ask.Options)}})
	return a, nil
}

// draftDecisionDoc creates the doc and turns a failure into the warning the
// caller returns with the ticket. It reports whether the doc was written.
func (s *Service) draftDecisionDoc(ctx context.Context, p authz.Principal, t *domain.Task, ask *domain.Ask) bool {
	if _, err := s.createDecisionDoc(ctx, p, t, ask); err != nil {
		slog.Warn("decision doc not created", "task_id", t.ID, "err", err)
		return false
	}
	return true
}

// renderDecisionDoc is v1's markdown: prose only. The question, options and
// recommended index are the version's metadata, and "decided means" is the
// ticket's acceptance criteria; neither is written here.
func (s *Service) renderDecisionDoc(ctx context.Context, t *domain.Task, ask *domain.Ask) string {
	var b strings.Builder
	b.WriteString("## Context\n\n")
	var where []string
	if e, err := s.Repo.GetEpic(ctx, t.EpicID); err == nil && e != nil {
		where = append(where, e.Name)
	}
	if st, err := s.Repo.GetStory(ctx, t.StoryID); err == nil && st != nil {
		where = append(where, st.Name)
	}
	if len(where) > 0 {
		b.WriteString(strings.Join(where, " › ") + "\n\n")
	}
	var evidence []string
	for _, e := range ask.Evidence {
		if e = strings.TrimSpace(e); e != "" {
			evidence = append(evidence, "- "+e)
		}
	}
	if len(evidence) > 0 {
		b.WriteString("Evidence:\n\n" + strings.Join(evidence, "\n") + "\n\n")
	}
	if ids, err := s.Repo.Dependents(ctx, []int{t.ID}); err == nil && len(ids) > 0 {
		refs := make([]string, len(ids))
		for i, id := range ids {
			refs[i] = fmt.Sprintf("#%d", id)
		}
		b.WriteString("Waiting on this: " + strings.Join(refs, " ") + "\n\n")
	}
	b.WriteString("## Trade-offs\n\n")
	stored := ask.Stored()
	for _, o := range stored.Options {
		fmt.Fprintf(&b, "**%s.** %s\n\n", strings.TrimSuffix(o.Label, "."), o.Consequence)
	}
	fmt.Fprintf(&b, "## Why we recommend %s\n\n%s\n", stored.Options[stored.Recommended].Label, strings.TrimSpace(ask.Reason))
	return b.String()
}

// decisionDocFor loads a ticket's decision doc with its current version's
// markdown and options, and every version when withVersions; nil when the
// ticket has none.
func (s *Service) decisionDocFor(ctx context.Context, t *domain.Task, withVersions bool) (*domain.DecisionDoc, error) {
	doc, err := s.Repo.GetDecisionDoc(ctx, t.ID)
	if err != nil || doc == nil {
		return nil, err
	}
	doc.DocURL = s.link(fmt.Sprintf("/artifacts/%d", doc.ArtifactID))
	v, err := s.Repo.GetArtifactVersion(ctx, doc.ArtifactID, 0)
	if err != nil {
		return nil, err
	}
	doc.CurrentVersion, doc.AuthorKind, doc.Ask, doc.Text = v.Version, v.AuthorKind, v.DecisionAsk, string(v.Content)
	if withVersions {
		if doc.Versions, err = s.Repo.DecisionDocVersions(ctx, doc.ArtifactID); err != nil {
			return nil, err
		}
	}
	return doc, nil
}

// GetDecision is a ticket's decision doc for anyone who can read the
// ticket: the current version's options and markdown, and every version
// with its options (GET /api/tasks/:id/decision, amp_get_task's decision).
func (s *Service) GetDecision(ctx context.Context, p authz.Principal, taskID int) (*domain.DecisionDoc, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskRead)
	if err != nil {
		return nil, err
	}
	doc, err := s.decisionDocFor(ctx, t, true)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, authz.NotFound("ticket %d has no decision doc", taskID)
	}
	return doc, nil
}

// OwnDecision is the ticket's decision doc without its versions, or nil
// (GET /api/tasks/:id/decisions' own).
func (s *Service) OwnDecision(ctx context.Context, p authz.Principal, taskID int) (*domain.DecisionDoc, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskRead)
	if err != nil {
		return nil, err
	}
	return s.decisionDocFor(ctx, t, false)
}

// decisionArtifact reports whether artifact id is a ticket's decision doc,
// for the §3 guards, which run before the agent checks so that agent
// accounts get the same refusals. A caller who isn't on the artifact's
// project gets not found, as from GetArtifact. nil doc: not a decision doc
// (or no such artifact; the caller's own lookup reports that).
func (s *Service) decisionArtifact(ctx context.Context, p authz.Principal, id int) (*domain.DecisionDoc, string, error) {
	a, err := s.Repo.GetArtifact(ctx, id)
	if err != nil || a.Role != domain.ArtifactRoleDecision {
		return nil, "", nil
	}
	if _, err := s.Authz.RequireProject(ctx, p, a.ProjectID, authz.Member); err != nil {
		if authz.StatusOf(err, 0) == 404 {
			return nil, "", authz.NotFound("artifact %d not found", id)
		}
		return nil, "", err
	}
	doc, err := s.Repo.DecisionDocForArtifact(ctx, id)
	if err != nil || doc == nil {
		return nil, "", err
	}
	return doc, s.TaskURL(a.ProjectID, doc.TaskID), nil
}

// refuseAgentDecision refuses anyone but a person in the web UI completing
// a ticket that has a decision doc (amp_complete_task, an API token), with
// the decision refusal rather than the generic one. Agent accounts get it
// too: it runs before refuseAgentAccount.
func (s *Service) refuseAgentDecision(ctx context.Context, p authz.Principal, taskID int, viaAgent bool) error {
	if p.InUI() && !viaAgent {
		return nil
	}
	doc, err := s.Repo.GetDecisionDoc(ctx, taskID)
	if err != nil || doc == nil {
		return nil
	}
	t, err := s.Repo.GetTask(ctx, taskID)
	if err != nil {
		return nil
	}
	if _, err := s.Authz.RequireProject(ctx, p, t.ProjectID, authz.Member); err != nil {
		return nil // the normal checks refuse it
	}
	refusal := authz.Forbidden(decisionPersonOnlyRefusal, s.TaskURL(t.ProjectID, taskID))
	s.auditTaskDenied(ctx, p, "task.human.complete", t, refusal, map[string]any{"via_agent": viaAgent, "decision_doc": doc.ArtifactID})
	return refusal
}

// SaveDecisionDocRequest is a person's edit of a decision doc (PUT
// /api/tasks/:id/decision/doc): the version it was made on, and the doc's
// markdown and options, which become one new version.
type SaveDecisionDocRequest struct {
	BaseVersion int                 `json:"base_version"`
	Content     string              `json:"content"`
	Ask         *domain.DecisionAsk `json:"ask"`
}

// decisionTask loads a ticket for a decision write: the caller must be able
// to work it (as for Mark done) and be a person in the web UI (D17). The
// ticket must have a decision doc.
func (s *Service) decisionTask(ctx context.Context, p authz.Principal, taskID int, what string) (*domain.Task, *domain.DecisionDoc, string, error) {
	t, _, err := s.LoadTask(ctx, p, taskID, authz.TaskWork)
	if err != nil {
		return nil, nil, "", err
	}
	link := s.TaskURL(t.ProjectID, taskID)
	if err := authz.RequireUI(p, fmt.Sprintf(what, taskID), link); err != nil {
		s.auditTaskDenied(ctx, p, "decision.write", t, err, nil)
		return nil, nil, "", err
	}
	doc, err := s.Repo.GetDecisionDoc(ctx, taskID)
	if err != nil {
		return nil, nil, "", err
	}
	if doc == nil {
		return nil, nil, "", authz.NotFound("ticket %d has no decision doc", taskID)
	}
	return t, doc, link, nil
}

// SaveDecisionDoc writes a person's edit of a ticket's decision doc: its
// markdown and options as one new version (author person), made on
// base_version. It refuses a recorded (or claimed) doc, invalid options, a
// stale base and a doc at 100 versions. It returns the doc with its versions.
func (s *Service) SaveDecisionDoc(ctx context.Context, p authz.Principal, taskID int, req SaveDecisionDocRequest) (*domain.DecisionDoc, error) {
	t, doc, link, err := s.decisionTask(ctx, p, taskID, "Editing ticket %d's decision doc")
	if err != nil {
		return nil, err
	}
	if doc.Claimed() {
		return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf(decisionRecordedRefusal, link)}
	}
	if req.Ask == nil {
		return nil, authz.BadRequest("ask is missing: the doc's question and options")
	}
	ask := (&domain.Ask{Question: req.Ask.Question, Options: req.Ask.Options, Recommended: req.Ask.Recommended}).Stored()
	if err := ask.Validate(); err != nil {
		return nil, authz.BadRequest("%s", err.Error())
	}
	content := []byte(req.Content)
	if strings.TrimSpace(req.Content) == "" {
		return nil, authz.BadRequest("the doc's text is empty")
	}
	if len(content) > domain.MaxArtifactBytes {
		return nil, authz.BadRequest("the doc is %d bytes; the limit is %d (10 MB)", len(content), domain.MaxArtifactBytes)
	}
	version, err := s.Repo.SaveDecisionVersion(ctx, taskID, req.BaseVersion, content, "text/markdown; charset=utf-8", "Edited in AMP", ask, p.UserRef())
	switch {
	case errors.Is(err, repository.ErrDecisionRecorded):
		return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf(decisionRecordedRefusal, link)}
	case errors.Is(err, repository.ErrDecisionStale):
		return nil, &authz.Error{Status: 409, Msg: DecisionStaleRefusal}
	case errors.Is(err, repository.ErrDecisionVersionCap):
		return nil, &authz.Error{Status: 409, Msg: DecisionCapRefusal}
	case errors.Is(err, repository.ErrNoDecisionDoc):
		return nil, authz.NotFound("ticket %d has no decision doc", taskID)
	case err != nil:
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "decision.edit", ProjectID: intRef(t.ProjectID), TargetType: "task", TargetID: idStr(taskID),
		TargetName: t.Name, Summary: fmt.Sprintf("edited ticket %d's decision doc (artifact #%d v%d)", taskID, doc.ArtifactID, version),
		Details: map[string]any{"artifact_id": doc.ArtifactID, "version": version, "base_version": req.BaseVersion}})
	return s.decisionDocFor(ctx, t, true)
}

// RecordDecisionRequest is Record decision (POST
// /api/tasks/:id/decision/record): the version decided on, the chosen
// option's index in that version's options (nil: something else), and the
// decided text.
type RecordDecisionRequest struct {
	Version      int    `json:"version"`
	ChosenOption *int   `json:"chosen_option"`
	DecisionText string `json:"decision_text"`
}

// RecordDecision records a person's decision on a ticket's doc (§3 Record
// decision) and freezes the version decided on, its markdown and options:
//
//  1. who may act (as Mark done, in the web UI only), non-empty text, the
//     option in range of that version's options, and that the ticket can be
//     completed now (not blocked, its dependencies done);
//  2. the claim, under the edit lock (Repo.ClaimDecisionRecord): from here
//     no doc write succeeds and dependents read the claimed values;
//  3. the ticket's completion, with one run and one "Decision by" comment
//     attributed to the decider (finishHumanTask, guarded by
//     decision_docs.run_id in one transaction);
//  4. the doc approved at that version and kept, and decided_at set.
//
// If step 3 is refused (the ticket can't be completed, a 4xx), the claim is
// released and the doc is editable again. Any other failure keeps the claim:
// once it is a minute old a retry finishes with the stored values, whatever
// it sent. On an error, the returned doc is non-nil exactly when a claim
// stands, so the caller can tell open drawers.
func (s *Service) RecordDecision(ctx context.Context, p authz.Principal, taskID int, req RecordDecisionRequest) (*domain.DecisionDoc, error) {
	if err := s.refuseAgentDecision(ctx, p, taskID, false); err != nil {
		return nil, err
	}
	t, doc, link, err := s.decisionTask(ctx, p, taskID, "Recording ticket %d's decision")
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(req.DecisionText)
	if text == "" && !doc.Claimed() {
		return nil, authz.BadRequest("a decision ticket needs your decision: say what you decided")
	}
	if doc.DecidedAt != nil {
		return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf(decisionRecordedRefusal, link)}
	}
	notCurrent := func() error {
		return authz.BadRequest("v%d isn't the doc's current version (v%d); reload it and record again", req.Version, doc.CurrentVersion)
	}
	if !doc.Claimed() {
		if t.State == domain.TaskStateCompleted {
			return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf("ticket %d is already done; its decision is in its comments: %s", taskID, link)}
		}
		if err := s.refuseIncompletable(ctx, t, link); err != nil {
			return nil, err
		}
		v, err := s.Repo.GetArtifactVersion(ctx, doc.ArtifactID, req.Version)
		if err != nil || req.Version != doc.CurrentVersion {
			return nil, notCurrent()
		}
		if o := req.ChosenOption; o != nil && (v.DecisionAsk == nil || *o < 0 || *o >= len(v.DecisionAsk.Options)) {
			n := 0
			if v.DecisionAsk != nil {
				n = len(v.DecisionAsk.Options)
			}
			return nil, authz.BadRequest("chosen_option %d is not one of v%d's %d options; send null for something else", *o, req.Version, n)
		}
	}
	rec, retried, err := s.Repo.ClaimDecisionRecord(ctx, taskID, req.Version, req.ChosenOption, text, p.UserRef())
	switch {
	case errors.Is(err, repository.ErrDecisionRecorded):
		return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf(decisionRecordedRefusal, link)}
	case errors.Is(err, repository.ErrDecisionBeingRecorded):
		return nil, &authz.Error{Status: 409, Msg: fmt.Sprintf(decisionRecordingRefusal, link)}
	case errors.Is(err, repository.ErrDecisionNotCurrent):
		return nil, notCurrent()
	case errors.Is(err, repository.ErrNoDecisionDoc):
		return nil, authz.NotFound("ticket %d has no decision doc", taskID)
	case err != nil:
		return nil, err
	}
	// The note is built from the stored record, so a retry records what was
	// claimed: "<option label> — <text>\n<doc link vN>".
	version := *rec.DecidedVersion
	note := rec.DecisionText
	if rec.ChosenOption != nil {
		if v, err := s.Repo.GetArtifactVersion(ctx, rec.ArtifactID, version); err == nil && v.DecisionAsk != nil {
			if i := *rec.ChosenOption; i >= 0 && i < len(v.DecisionAsk.Options) {
				note = v.DecisionAsk.Options[i].Label + " — " + note
			}
		}
	}
	note += "\n" + s.decisionDocVersionURL(rec.ArtifactID, version)
	if _, _, err := s.finishHumanTask(ctx, p, t, HumanDone{Note: note}, &decisionFinish{decider: rec.DecidedBy}); err != nil {
		if st := authz.StatusOf(err, 500); st >= 400 && st < 500 {
			// The ticket can't be completed: nothing waits on a record that
			// won't finish, so the claim goes and the doc is editable again.
			if cur, gerr := s.Repo.GetTask(ctx, taskID); gerr == nil && cur.State != domain.TaskStateCompleted {
				if rerr := s.Repo.ReleaseDecisionClaim(ctx, taskID); rerr == nil {
					return nil, err
				}
			}
		}
		return rec, fmt.Errorf("recording didn't finish (%w); retry in a minute", err)
	}
	if err := s.Repo.FinishDecisionRecord(ctx, taskID, rec.DecidedBy); err != nil {
		return rec, fmt.Errorf("recording didn't finish (%w); retry in a minute", err)
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "decision.record", ProjectID: intRef(t.ProjectID), TargetType: "task", TargetID: idStr(taskID),
		TargetName: t.Name, Summary: fmt.Sprintf("recorded ticket %d's decision on artifact #%d v%d", taskID, rec.ArtifactID, version),
		Details: map[string]any{"artifact_id": rec.ArtifactID, "version": version, "chosen_option": rec.ChosenOption, "retried": retried}})
	return s.decisionDocFor(ctx, t, false)
}

// refuseIncompletable refuses (409) recording a decision on a ticket that
// can't be completed now: one still waiting on its dependencies, or blocked.
// Checked before the claim, so a refused record leaves nothing behind.
func (s *Service) refuseIncompletable(ctx context.Context, t *domain.Task, link string) error {
	var open []string
	for _, id := range t.DependencyIDs {
		if dep, err := s.Repo.GetTask(ctx, id); err == nil && dep.State != domain.TaskStateCompleted {
			open = append(open, fmt.Sprintf("#%d", id))
		}
	}
	if len(open) > 0 {
		return &authz.Error{Status: 409, Msg: fmt.Sprintf("ticket %d is waiting on %s; record its decision once they're done: %s", t.ID, strings.Join(open, " "), link)}
	}
	if t.State == domain.TaskStateBlocked {
		return &authz.Error{Status: 409, Msg: fmt.Sprintf("ticket %d is blocked; record its decision once it's unblocked: %s", t.ID, link)}
	}
	return nil
}
