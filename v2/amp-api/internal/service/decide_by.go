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

// Decision escalation (#146), on the dispatch watchdog's pattern
// (watchdog.go): a person's ticket can carry a decide-by date (decide_by).
// My Work flags it once that passes, which is derived and needs no sweep.
// A project that turns escalation on (decision_escalation_minutes > 0; off
// by default, changed only by an owner in the web UI) has the sweep act once
// the date is that far behind:
//
//   - remind: a comment on the ticket naming the person it waits on, and a
//     system audit entry (task.decision_overdue), which webhooks deliver;
//   - raise priority one step, when decision_escalation_raise_priority;
//   - hand it to decision_backup_decider, when set, not already its owner,
//     still an active member of the project, and the ticket isn't started
//     (a started ticket stays with the person working it).
//
// Only a ticket its person can act on now is escalated (open, not blocked
// on dependencies, a schedule or plan approval, no decision being recorded:
// repository.decisionActionable).
//
// Each is logged on the ticket's history (decision_escalated). It happens
// once per ticket per decide-by date: the sweep claims the ticket in the
// database first (Repo.ClaimDecisionEscalation), and setting a new date
// re-arms it. It never records a decision, or says to take the recommended
// option: escalation reaches a person.

// MinSuggestedDecideByLead is how far ahead a suggested decide-by date must
// be. A date only a little in the future would act as a backdated one: the
// sweep would escalate the ticket (raising its priority and handing it to
// the backup decider) before its person could see or move it.
const MinSuggestedDecideByLead = time.Hour

// checkSuggestedDecideBy keeps everyone but the ticket's person to suggesting
// a decide-by date: the person sets it. The person is the ticket's owner,
// acting in the web UI (or the local-dev user, with auth off); they set,
// move or remove the date freely. Anyone else (an agent or any caller off
// the web UI, another user in the UI including a lead or the ticket's
// creator, and anyone at creation, when the ticket has no person yet) may
// only put a date on a ticket that has none, at least
// MinSuggestedDecideByLead ahead; moving or removing one is the person's.
// Otherwise a caller could backdate a ticket, or date it a moment ahead, so
// the next sweep escalates it, or quietly stop a person's deadline.
// Re-sending the date the ticket already has is a no-op and allowed.
func (s *Service) checkSuggestedDecideBy(p authz.Principal, t *domain.Task, next *time.Time, clearing bool, now time.Time) error {
	if t != nil && p.InUI() && (!p.HasUser() || (t.OwnerUserID != nil && *t.OwnerUserID == p.UserID)) {
		return nil
	}
	var link string
	var current *time.Time
	if t != nil {
		link, current = s.TaskURL(t.ProjectID, t.ID), t.DecideBy
	}
	notPerson := func(what string) error {
		if p.InUI() {
			return authz.Forbidden("%s is its person's (the ticket's owner): you can suggest a date only on a ticket that has none", what)
		}
		return authz.UIOnly(what, link)
	}
	switch {
	case clearing && current != nil:
		return notPerson(fmt.Sprintf("Removing ticket %d's decide-by date", t.ID))
	case next == nil:
		return nil
	case current != nil && current.Equal(*next):
		return nil
	case current != nil:
		return notPerson(fmt.Sprintf("Moving ticket %d's decide-by date (set by its person)", t.ID))
	case next.Before(now.Add(MinSuggestedDecideByLead)):
		return authz.BadRequest("decide_by must be in the future, at least %s from now: a suggested date is for the person to decide by, "+
			"so it can't be one that is already due (or about to be)", MinSuggestedDecideByLead)
	}
	return nil
}

// escalationActor is how escalation names itself in history and comments.
const escalationActor = "system"

// checkEscalationSettings refuses an escalation change off the web UI (an
// agent could otherwise turn it on to reassign a person's work), a negative
// delay, or a backup decider who isn't on the project.
func (s *Service) checkEscalationSettings(ctx context.Context, p authz.Principal, projectID int, u domain.ProjectSettings) error {
	if !u.ChangesEscalation() {
		return nil
	}
	if err := authz.RequireUI(p, "Changing decision escalation", s.SettingsURL(projectID)); err != nil {
		return err
	}
	if m := u.DecisionEscalationMinutes; m != nil && *m < 0 {
		return authz.BadRequest("decision_escalation_minutes is minutes past the decide-by date (0 turns escalation off), not %d", *m)
	}
	if b := u.DecisionBackupDecider; b != nil && *b != 0 {
		lvl, err := s.Authz.ProjectLevel(ctx, authz.Principal{UserID: *b}, projectID)
		if err != nil {
			return err
		}
		if lvl < authz.Member {
			return authz.BadRequest("user %d isn't on project %d: a backup decider must be a project member", *b, projectID)
		}
		if u, err := s.Repo.GetUserByID(ctx, *b); err != nil || u == nil || u.DisabledAt != nil || u.IsAgent() {
			return authz.BadRequest("user %d can't be a backup decider: it must be an active person's account", *b)
		}
	}
	return nil
}

// backupDeciderOK reports whether the project's backup decider can still
// take a ticket: still on the project and an active person's account. It is
// checked when the setting is saved, but they may leave the project or be
// disabled since, and the handover must not move a ticket to someone who
// can't see or act on it.
func (s *Service) backupDeciderOK(ctx context.Context, projectID, userID int) bool {
	lvl, err := s.Authz.ProjectLevel(ctx, authz.Principal{UserID: userID}, projectID)
	if err != nil || lvl < authz.Member {
		return false
	}
	u, err := s.Repo.GetUserByID(ctx, userID)
	return err == nil && u != nil && u.DisabledAt == nil && !u.IsAgent()
}

// SweepOverdueDecisions is one escalation pass: it escalates every person's
// ticket whose decide-by date is past its project's delay and that hasn't
// been escalated for that date, and reports how many it did. lock
// serialises the pass across replicas, as SweepNotStarted's does.
func (s *Service) SweepOverdueDecisions(ctx context.Context, lock func(context.Context, string) (func(), bool, error)) (int, error) {
	release, ok, err := lock(ctx, "decision-escalation")
	if err != nil {
		return 0, fmt.Errorf("decision escalation lock: %w", err)
	}
	if !ok {
		return 0, nil // another replica is sweeping
	}
	defer release()

	due, err := s.Repo.DueDecisions(ctx)
	if err != nil {
		return 0, fmt.Errorf("overdue decisions: %w", err)
	}
	projects := map[int]*domain.Project{}
	n := 0
	for _, d := range due {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		proj := projects[d.ProjectID]
		if proj == nil {
			if proj, err = s.Repo.GetProject(ctx, d.ProjectID); err != nil {
				slog.Warn("decision escalation: project not loaded", "project_id", d.ProjectID, "err", err)
				continue
			}
			projects[d.ProjectID] = proj
		}
		did, err := s.escalateDecision(ctx, proj, d)
		if err != nil {
			slog.Warn("decision escalation failed", "project_id", d.ProjectID, "task_id", d.TaskID, "err", err)
			continue
		}
		if did {
			n++
		}
	}
	return n, nil
}

// escalateDecision escalates one overdue ticket. It reports false when the
// claim was lost (the date moved, the ticket was done, or another pass got
// there first): then nothing is done. After the claim, a failed step is
// logged and the rest still run: the claim isn't given back, so a ticket
// is never escalated twice for one date.
func (s *Service) escalateDecision(ctx context.Context, proj *domain.Project, d repository.DueDecision) (bool, error) {
	claimed, err := s.Repo.ClaimDecisionEscalation(ctx, d.TaskID, d.DecideBy)
	if err != nil || !claimed {
		return false, err
	}
	t, err := s.Repo.GetTask(ctx, d.TaskID)
	if err != nil {
		return true, fmt.Errorf("load ticket: %w", err)
	}
	sys := authz.Principal{Email: escalationActor}
	owner := s.userNameOr(ctx, t.OwnerUserID, authz.Principal{Email: "nobody (unassigned)"})
	due := d.DecideBy.UTC().Format("2006-01-02 15:04 UTC")
	var actions []string

	if proj.DecisionEscalationRaisePriority {
		if up, changed := domain.RaisedPriority(t.Priority); changed {
			if err := s.escalationUpdate(ctx, t, domain.UpdateTaskRequest{TaskID: t.ID, Priority: up}); err != nil {
				slog.Warn("decision escalation: priority not raised", "task_id", t.ID, "err", err)
			} else {
				actions = append(actions, fmt.Sprintf("raised priority %s → %s", t.Priority, up))
			}
		}
	}
	newOwner := ""
	if b := proj.DecisionBackupDecider; b != nil && (t.OwnerUserID == nil || *t.OwnerUserID != *b) {
		if !s.backupDeciderOK(ctx, t.ProjectID, *b) {
			slog.Warn("decision escalation: backup decider no longer on the project or not active", "task_id", t.ID, "user_id", *b)
			actions = append(actions, "not handed to the backup decider (no longer on the project, or not an active account)")
		} else if t.State == domain.TaskStateInProgress {
			actions = append(actions, "not handed to the backup decider (its person has started it)")
		} else if _, err := s.escalationAssign(ctx, t, b); err != nil {
			slog.Warn("decision escalation: not handed to the backup decider", "task_id", t.ID, "err", err)
		} else {
			newOwner = s.userNameOr(ctx, b, sys)
			actions = append(actions, fmt.Sprintf("handed to %s (backup decider) from %s", newOwner, owner))
		}
	}

	// The reminder reaches a person, never decides for them.
	body := fmt.Sprintf("Decision overdue: this was to be decided by %s. %s, it is waiting on you", due, owner)
	if newOwner != "" {
		body = fmt.Sprintf("Decision overdue: this was to be decided by %s. Handed to %s, this project's backup decider (it was %s's)", due, newOwner, owner)
	}
	if down, err := s.Repo.OpenDownstream(ctx, []int{t.ID}); err == nil && len(down[t.ID]) > 0 {
		body += fmt.Sprintf("; %d ticket(s) wait on it", len(down[t.ID]))
	}
	body += ". Only a person records this decision, in AMP: " + s.TaskURL(t.ProjectID, t.ID)
	s.comment(t.ProjectID, t.ID, sys, body)
	actions = append([]string{"reminded " + owner}, actions...)

	detail := fmt.Sprintf("Decision overdue (decide by %s): %s", due, strings.Join(actions, "; "))
	if err := s.Repo.LogActivity(ctx, domain.ActivityLog{TaskID: t.ID, ProjectID: t.ProjectID, Actor: escalationActor,
		Action: "decision_escalated", Detail: detail}); err != nil {
		slog.Warn("decision escalation: history not written", "task_id", t.ID, "err", err)
	}
	s.RecordSystem(ctx, domain.AuditEntry{
		ProjectID: intRef(t.ProjectID), TargetType: "task", TargetID: idStr(t.ID), TargetName: t.Name,
		Action:  "task.decision_overdue",
		Summary: fmt.Sprintf("ticket %d's decision is overdue (decide by %s): %s", t.ID, due, strings.Join(actions, "; ")),
		Details: map[string]any{"decide_by": d.DecideBy, "owner_user_id": t.OwnerUserID, "actions": actions, "automatic": true},
	})
	return true, nil
}

// escalationUpdate changes a ticket through its actor as the escalation.
func (s *Service) escalationUpdate(ctx context.Context, t *domain.Task, req domain.UpdateTaskRequest) error {
	reply := make(chan actor.ReplySimple, 1)
	if err := s.Registry.Send(t.ProjectID, &actor.MsgUpdateTask{Req: req, ByName: escalationActor, ReplyCh: reply}); err != nil {
		return err
	}
	return actor.Await(ctx, reply).Err
}

// escalationAssign hands a ticket to the backup decider through its actor.
// The project's owner named that person, so the ticket keeps its plan
// approval (ByLead), as a lead's handover in the web UI does. A ticket its
// person has started is never taken from them (RequireIdle).
func (s *Service) escalationAssign(ctx context.Context, t *domain.Task, to *int) (*domain.Task, error) {
	reply := make(chan actor.ReplyGetTask, 1)
	if err := s.Registry.Send(t.ProjectID, &actor.MsgAssignTask{TaskID: t.ID, ExpectOwner: t.OwnerUserID, NewOwner: to,
		RequireIdle: true, ByLead: true, ByName: escalationActor, ReplyCh: reply}); err != nil {
		return nil, err
	}
	r := actor.Await(ctx, reply)
	if r.Err != nil {
		return nil, r.Err
	}
	if r.Task == nil {
		return nil, errors.New("no ticket in the reply")
	}
	return r.Task, nil
}

// runDecisionSweep is the escalation's share of the watchdog's tick.
func (s *Service) runDecisionSweep(ctx context.Context, lock func(context.Context, string) (func(), bool, error)) {
	if n, err := s.SweepOverdueDecisions(ctx, lock); err != nil && ctx.Err() == nil {
		slog.Warn("decision escalation sweep failed", "err", err)
	} else if n > 0 {
		slog.Info("decision escalation reminded people of overdue decisions", "count", n, "at", time.Now().UTC())
	}
}
