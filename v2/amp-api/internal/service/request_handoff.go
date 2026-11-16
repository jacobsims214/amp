package service

// Team requests: the retry-safe planning handoff (epic 202, story 549, AMP #3965;
// approved product contract artifact 36 v2 §3 — "Handoff creates tickets awaiting
// per-ticket human approval — no auto-approval, no auto-dispatch; request text
// reaches agents as quoted untrusted data, never as instructions" — and default D11,
// "reconcile-on-retry, no batch-transaction rewrite"; build slice E1's handoff half
// in discoveries/team-inbox-approved-build-slices.md).
//
// WHAT THIS FILE IS. The one authorized path that turns a request into plan items:
// it takes a batch of ticket specs from the caller, creates the missing ones through
// Service.CreateTasks (unchanged), and links each one to the request as a
// `planned_as` relation through Service.AttachRequest (unchanged). It adds exactly
// three things to those two: an authorization decision, a retry ledger, and the
// untrusted-text framing.
//
// WHY IT IS WRITTEN FOR CreateTasks' REAL FAILURE MODE. CreateTasks validates a
// whole batch up front and then creates tickets ONE AT A TIME (plan.go, D11 kept it
// that way). A mid-batch failure returns the tickets it already made plus an error
// naming them. It is also not idempotent: it has no client token. So a caller that
// retries after a failure — the only thing a caller can do — would create the whole
// batch again and the request would be planned twice.
//
// The ledger that prevents that is the ticket's own metadata, not a table
// (domain.MetaRequestHandoff, domain.RequestHandoffMark, and
// repository/request_handoff.go's header for the full reasoning): the marker rides
// the INSERT that creates the ticket, so "ref `api` was created by handoff K of
// request R" and the ticket row commit together and there is no window in which a
// crash loses the mapping. A retry reads the ledger back, plans only what is
// missing, and rewrites the remainder's intra-batch dependencies from refs to the
// ids of work that already exists, so the remainder still wires to it.
//
// THREE PROPERTIES THIS FILE OWNS, AND WHERE EACH ONE LIVES.
//
//  1. A handoff NEITHER APPROVES NOR DISPATCHES (contract §3, the epic's "no
//     automatic planning approval or execution"). Nothing here calls
//     ApproveTickets, ApprovePlan or Dispatch, and nothing here writes approved_at,
//     approved_for, dispatch_id, dispatched_at or started_at: the tickets come out
//     of CreateTasks unapproved and undispatched and this file never touches them
//     again except to LINK them. The approval gate then refuses them through the
//     real plan-approval path (TestHandoffRequestNeverApprovesOrDispatches drives
//     Service.Dispatch rather than trusting this paragraph).
//
//  2. A handoff CANNOT DECIDE WHO OR WHEN. This is "never derive assignee/schedule/
//     auth decisions from prompt text" made structural rather than promised: the
//     input type has no assignee, no owner, no start_at, no performer, no
//     human_reason, no decide_by and no metadata, and a caller that supplies one is
//     refused BY NAME rather than silently ignored. A field the caller can set from
//     a requester's words is precisely the injection surface, and the only way to
//     prove the brief did not decide who does the work is for this path to have no
//     way to say so. A triager who wants an assignee or a schedule sets it on the
//     ticket afterwards, as their own deliberate act, with no request text in the
//     loop. (The same reasoning refuses a person's ticket here: CreateTasks requires
//     an owner for one, and an owner is exactly the field this path must not name.
//     A human gate is added by the triager's own planning act, not by a handoff.)
//
//  3. A REQUEST'S WORDS ARE QUOTED DATA, ALWAYS. Every description this path creates
//     is built by domain.HandoffBrief, so the request's own text lands inside the
//     untrusted fence and the caller cannot opt out by writing its own description:
//     the input's Plan text is the plan author's instruction and is concatenated
//     OUTSIDE the fence, and the request is appended inside it. A caller cannot pass
//     a description at all — only a plan body — which is what makes "every handoff
//     ticket carries the quote" true for every call rather than for the careful ones.
//
// What this file deliberately does not do — each one another slice's concern:
//
//   - no REST route, MCP tool or UI. This is the service those thin adapters call
//     (F5/I3/J2's planning rail).
//   - no routing decision. The project is the request's ACTIVE routes_to route (D9),
//     read from the link table and never from the caller or from metadata: a handoff
//     does not choose where a request goes, it plans into where triage already sent
//     it. A request with no route is refused and told to be routed first.
//   - no lifecycle move. Creating plan items does not move a request's state
//     (I8 — nothing auto-advances from ticket state, and nothing auto-advances from
//     planning either). `planned_at` is stamped by the first planned_as link, which
//     is the link repository's rule (#3875), not this file's.
//   - no request edit. The request is read, quoted and linked; its brief, subject,
//     rev and timestamps are never written. That is what "preserve the original
//     brief" means operationally: the canonical copy stays on the request row and the
//     plan quotes it.
//   - no dependency re-wiring of work that already exists. A retry reconciles what a
//     half-finished attempt left behind; it does not rewrite an existing ticket's
//     plan. When a retry's structure disagrees with what exists, that is refused with
//     the disagreement named rather than guessed at.
//   - no state gate. Whether a request in a closed state may be planned is a
//     lifecycle rule and the one transition policy owns it — the same reason
//     service/request_links.go refuses to invent one.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// HandoffItem is one ticket the caller wants planned for the request.
//
// Every field is a PLAN decision the caller is entitled to make. What is missing is
// the point: no AssignedTo, no Owner, no StartAt, no Performer or HumanReason, no
// DecideBy, no Metadata (see this file's property 2). A surface that wants to offer
// one of those offers it on the created ticket, after the fact.
type HandoffItem struct {
	// Ref names this item within the batch, so another item can depend on it and so
	// a retry can tell "this one already exists" from "this one is new". It is the
	// ledger's key, so it is stable across attempts of the same handoff.
	Ref string
	// StoryID is where the ticket goes. It must be in the request's routed project;
	// CreateTasks checks that and this path does not second-guess it.
	StoryID int
	// Name is the ticket's one-line title. A label, not an instruction: the
	// requester's own words belong in Plan's quote, never in the name.
	Name string
	// Plan is the plan author's instruction for this item — what the ticket is for
	// and what "done" looks like to the team. It is written OUTSIDE the untrusted
	// fence, because it is the team's own writing; the request is appended inside the
	// fence by domain.HandoffBrief. Empty is allowed: the ticket is then the quote
	// alone, which is the honest shape when nobody has said more than the request did.
	Plan string
	// AcceptanceCriteria is the plan author's definition of done.
	AcceptanceCriteria string
	// Priority is the plan's severity (0 low, 1 normal, 2 high, 3 critical). Empty
	// is normal.
	Priority string
	// DependsOn names what this item waits on: refs in this batch, or existing ticket
	// ids written as "#123" / "123". A ref that an earlier attempt of this handoff
	// already created is resolved to that ticket's id, so a retried remainder still
	// wires to the work that exists.
	DependsOn []string
}

// HandoffRequestInput is one planning handoff, as a surface receives it.
//
// There is deliberately no project: the request's active route is the project (D9),
// and a caller that could name one would be choosing where a request goes, which is
// triage's act (#3962's routes_to), not planning's. There is deliberately no org or
// actor either — the caller says what it wants planned, and every fact about the
// request and the caller's standing is read here from the rows.
type HandoffRequestInput struct {
	// RequestID is the request being planned, and TeamID the inbox the caller is
	// working in. The org is the team's, read from its row.
	RequestID int
	TeamID    int

	// HandoffKey is the caller's idempotency token for THIS batch: send the same key
	// to retry, a new key for a deliberate re-plan. It is what makes a retried
	// handoff the same batch instead of a second plan, and it is recorded on every
	// ticket the batch creates (domain.MetaRequestHandoff).
	HandoffKey string

	// Items is the batch. It may be shorter than an earlier attempt's batch: a retry
	// that sends only what it still needs is the normal shape, and refs already in
	// the ledger are reconciled whichever batch they arrive in.
	Items []HandoffItem
}

// HandoffItemResult is what one item of the batch became.
type HandoffItemResult struct {
	Ref string `json:"ref"`
	// TaskID is the ticket serving this ref — created by this call, or by an earlier
	// attempt of the same handoff and found in the ledger.
	TaskID int `json:"task_id"`
	// Created is true when THIS call created the ticket. False is "it already
	// existed", which is the whole point of the ledger and worth telling a caller:
	// "3 created, 1 already there" is a truthful report of a retry.
	Created bool `json:"created"`
	// Linked is true when this call wrote the `planned_as` relation. False is a
	// replay: the relation was already stored, and the attach path wrote nothing.
	Linked bool `json:"linked"`
}

// HandoffRequestResult is one handoff attempt.
type HandoffRequestResult struct {
	// ProjectID is the request's routed project, where the batch was planned. It is
	// read from the route, never from the caller.
	ProjectID int `json:"project_id"`
	// Items is every ref the batch now has a ticket for, in batch order.
	Items []HandoffItemResult `json:"items"`
	// Warnings are CreateTasks' own (a batch with no dependencies between its items,
	// a decision doc that could not be written) plus this file's "already created"
	// note. They are honest information, never a failure.
	Warnings []string `json:"warnings,omitempty"`
}

// HandoffRequest plans a batch of tickets into the project a request is routed to,
// links them to the request, and is safe to retry.
//
// The order of operations is the contract:
//
//  1. Refuse a caller with no standing, and refuse the fields this path must not be
//     able to set (property 2). A refusal costs nothing and writes nothing.
//  2. Read the request's active route — the project — and the caller's access to it.
//     A request that is not routed is refused with what to do about it, because
//     planning into a project nobody chose is the D9 mistake.
//  3. Serialize per request (an advisory lock, not a queue): two concurrent handoffs
//     of one request would both read an empty ledger and both plan. The loser is told
//     a handoff is already running, which is true and retryable.
//  4. Read the ledger, plan only what is missing, rewrite the remainder's
//     dependencies from refs to the ids that already exist, and create through
//     CreateTasks — which may itself fail partway, leaving tickets whose own markers
//     are the ledger for the next attempt.
//  5. Link EVERY item, including the ones the ledger supplied, as `planned_as`. The
//     attach path is idempotent, so a retry that died between creating and linking
//     finishes the linking; a fully-finished retry writes nothing at all.
//  6. Audit once, as `request.handoff`, with the ledger it acted on.
func (s *Service) HandoffRequest(ctx context.Context, p authz.Principal, in HandoffRequestInput) (*HandoffRequestResult, error) {
	// A submit-scoped credential may reach exactly one operation (its bound team's
	// submission). Planning work for a request is not it, and saying so names the
	// credential's limit instead of hiding it behind a 404.
	if err := p.RequireUnscoped("plan work for a request"); err != nil {
		return nil, err
	}

	key, items, err := normalizeHandoffInput(in)
	if err != nil {
		return nil, err
	}

	team, req, capability, err := s.loadInboxRequest(ctx, p, in.RequestID, in.TeamID)
	if err != nil {
		return nil, err
	}
	// Planning is triage: the inbox team and its managers. A requester is refused by
	// name rather than by 404 because they may already see the request they are
	// asking about — naming the rule tells them something true without telling them
	// anything new (the attach path's rule, restated for the same reason).
	if !capability.canMake(RequestCapabilityTeam) {
		if capability == RequestCapabilityNone {
			return nil, authz.RequestNotFound()
		}
		return nil, authz.Forbidden(
			"planning work for request %s is the inbox team's act: being its requester means following it and answering it, not deciding what serves it. Nothing was planned.", req.PublicRef)
	}

	// The project is triage's decision, read from the one place it is recorded.
	route, err := s.Repo.ActiveRequestRoute(ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		return nil, fmt.Errorf("read the route of request %d: %w", req.ID, err)
	}
	if route == nil {
		// Not a 404 and not a 409: this is a request the caller legitimately has, and
		// the missing thing is an earlier triage act, not a conflict they lost.
		return nil, authz.RequestInvalid(
			"request %s is not routed to a project yet: a handoff plans into the project the request is routed to (D9), so route it first (attach a routes_to link) and then plan it", req.PublicRef)
	}
	projectID := route.TargetID
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member); err != nil {
		if authz.StatusOf(err, 0) == 404 {
			// A project this caller may not see. The route is the request's own fact,
			// so the refusal can say it without naming a project id the caller had no
			// right to learn.
			return nil, authz.Forbidden(
				"request %s is routed to a project you can't see, so you can't plan work in it. Nothing was planned.", req.PublicRef)
		}
		return nil, fmt.Errorf("check access to project %d for request %d: %w", projectID, req.ID, err)
	}

	// One handoff per request at a time. Not a queue and not a retry loop: a handoff
	// is a caller's explicit act, and a caller told "one is already running" can
	// simply ask again — which is also the answer that keeps a wedged run from
	// holding an unbounded number of waiters.
	release, ok, err := s.Repo.TryNamedLock(ctx, handoffLockKey(req.ID))
	if err != nil {
		return nil, fmt.Errorf("lock the handoff of request %d: %w", req.ID, err)
	}
	if !ok {
		return nil, authz.RequestConflict(
			"a handoff of request %s is already running: wait for it to finish and send this batch again — it will reconcile against whatever that attempt created rather than planning it twice", req.PublicRef)
	}
	defer release()

	ledger, err := s.Repo.RequestHandoffLedger(ctx, req.ID, req.OrgID, req.TeamID, projectID, key)
	if err != nil {
		return nil, err
	}
	planned, err := planHandoffBatch(items, ledger)
	if err != nil {
		return nil, handoffRefusal(req, err)
	}

	result := &HandoffRequestResult{ProjectID: projectID}
	var created []CreatedTask
	if len(planned.specs) > 0 {
		specs := s.handoffSpecs(req, key, planned.specs)
		out, warnings, err := s.CreateTasks(ctx, p, projectID, specs)
		result.Warnings = append(result.Warnings, warnings...)
		// CreateTasks returns what it made even when it fails (it creates one ticket
		// at a time and D11 keeps it that way). Those tickets carry their own markers,
		// so they are the next attempt's ledger — reporting them is honest, and
		// discarding them would hide work that exists.
		created = append(created, out...)
		if err != nil {
			return nil, s.handoffCreateError(ctx, req, result, created, err)
		}
	}

	// Assemble the ref→id mapping this attempt answers with, then link every item.
	for _, item := range planned.order {
		id, isNew := planned.idOf(item.Ref, created)
		if id == 0 {
			return nil, fmt.Errorf("handoff of request %d: ref %q was planned but no ticket id came back", req.ID, item.Ref)
		}
		res := HandoffItemResult{Ref: item.Ref, TaskID: id, Created: isNew}
		attached, err := s.AttachRequest(ctx, p, AttachRequestInput{
			RequestID:  req.ID,
			TeamID:     team.ID,
			LinkKind:   domain.RequestLinkPlannedAs,
			TargetKind: domain.RequestTargetTask,
			TargetID:   id,
		})
		if err != nil {
			// The tickets exist and are marked, so the next attempt reconciles them;
			// what failed is the lineage write. Returning the error rather than a
			// warning is deliberate: a plan whose items are not linked to the request
			// is a request nobody can trace, and the caller must retry.
			return nil, fmt.Errorf("link handoff item %q (ticket %d) to request %d: %w", item.Ref, id, req.ID, err)
		}
		res.Linked = attached.Changed
		result.Items = append(result.Items, res)
	}
	if len(result.Items) == 0 {
		// Every item the caller named was already there and already linked. That is a
		// successful replay, not an empty plan: the answer names the tickets.
		return nil, fmt.Errorf("handoff of request %d: the batch named no items", req.ID)
	}

	if createdCount(result.Items) > 0 || linkedCount(result.Items) > 0 {
		s.recordRequestHandoff(ctx, p, team, req, projectID, key, result)
	}
	return result, nil
}

// HandoffItemRefusal is a caller's batch that this path will not plan, with the
// rule named. It is an authz-coded 400 (request.invalid): the caller can fix it.
type handoffItemRefusal struct {
	msg   string
	cause error
}

func (e *handoffItemRefusal) Error() string { return e.msg }

// handoffRefusal renders a batch refusal as the request surface's invalid code,
// keeping whatever cause it wraps so errors.Is still answers for a caller.
func handoffRefusal(req *domain.TeamRequest, err error) error {
	var r *handoffItemRefusal
	if errors.As(err, &r) {
		return &codedCause{err: authz.RequestInvalid("%s", r.msg), cause: r.cause}
	}
	return err
}

// normalizeHandoffInput validates the key and every item's own fields: the refs are
// names in a ledger, so their shape is checked here (domain.NormalizeHandoffRef)
// rather than left to CreateTasks, which allows any non-blank ref.
func normalizeHandoffInput(in HandoffRequestInput) (string, []HandoffItem, error) {
	key, err := domain.NormalizeHandoffKey(in.HandoffKey)
	if err != nil {
		return "", nil, authz.RequestInvalid("%s", err)
	}
	if len(in.Items) == 0 {
		return "", nil, authz.RequestInvalid("items is empty: a handoff plans at least one ticket")
	}
	items := make([]HandoffItem, len(in.Items))
	seen := map[string]bool{}
	for i, it := range in.Items {
		ref, err := domain.NormalizeHandoffRef(it.Ref)
		if err != nil {
			return "", nil, authz.RequestInvalid("items[%d]: %s", i, err)
		}
		if seen[ref] {
			return "", nil, authz.RequestInvalid("items[%d]: ref %q is used twice in this batch", i, ref)
		}
		seen[ref] = true
		if it.StoryID <= 0 {
			return "", nil, authz.RequestInvalid("items[%d] (%q): story_id is required: every ticket belongs to a story in the routed project", i, ref)
		}
		if strings.TrimSpace(it.Name) == "" {
			return "", nil, authz.RequestInvalid("items[%d] (%q): name is required", i, ref)
		}
		it.Ref, it.Name = ref, strings.TrimSpace(it.Name)
		it.Plan, it.AcceptanceCriteria = strings.TrimSpace(it.Plan), strings.TrimSpace(it.AcceptanceCriteria)
		items[i] = it
	}
	return key, items, nil
}

// plannedHandoffBatch is what a retry decides to do: which items to create, with
// their dependencies already rewritten, and where each ref's ticket is.
type plannedHandoffBatch struct {
	// order is the caller's items in the order it sent them, so the answer reads
	// back the way the request was written.
	order []HandoffItem
	// specs are the items to create, in batch order.
	specs []HandoffItem
	// existing is ref → ticket id for the items an earlier attempt made.
	existing map[string]int
}

// idOf answers "which ticket serves this ref": the one this call created, else the
// one the ledger already had. isNew says whether THIS call created it.
func (b *plannedHandoffBatch) idOf(ref string, created []CreatedTask) (int, bool) {
	for _, c := range created {
		if c.ID != 0 && c.Ref == ref {
			return c.ID, true
		}
	}
	if id, ok := b.existing[ref]; ok {
		return id, false
	}
	return 0, false
}

// planHandoffBatch reconciles the caller's batch against the ledger: items already
// there are dropped from the creation list, and the remainder's dependencies are
// rewritten so a ref that now names an existing ticket becomes that ticket's id.
//
// The one refusal is the case where a retry disagrees with what exists and no safe
// guess is available: an item that ALREADY exists naming a dependency that does not.
// That is not a reconciliation but a claim about history that cannot be true —
// CreateTasks resolves every dependency before it creates a ticket, so the ticket an
// earlier attempt made could only have been created with a dependency that existed
// then. Rewiring it now is a plan change, which is the triager's deliberate act on the
// ticket (`amp_set_task_dependencies`), not something a retry may infer from a batch.
//
// A NEW item is free to depend on anything resolvable: a ref in this batch, a ref the
// ledger already has (rewritten to that ticket's id), or a ticket id.
func planHandoffBatch(items []HandoffItem, ledger []repository.RequestHandoffRef) (*plannedHandoffBatch, error) {
	existing := make(map[string]int, len(ledger))
	for _, l := range ledger {
		existing[l.Ref] = l.TaskID
	}
	batch := &plannedHandoffBatch{order: items, existing: existing}
	refs := make(map[string]bool, len(items))
	for _, it := range items {
		refs[it.Ref] = true
	}

	for _, it := range items {
		if id, already := existing[it.Ref]; already {
			// It exists. Its dependencies were settled by the attempt that created it, so
			// every ref this retry names has to be something that was there too.
			for _, dep := range it.DependsOn {
				d := strings.TrimSpace(dep)
				if d == "" || isTicketIDRef(d) {
					continue
				}
				if _, ok := existing[d]; ok {
					continue
				}
				return nil, &handoffItemRefusal{
					msg: fmt.Sprintf("ref %q already exists as ticket %d, and this retry names %q as something it depends on, but nothing of this handoff created %q. "+
						"A handoff reconciles a retry — it does not rewire work it already created; set the dependency on ticket %d with amp_set_task_dependencies if that is the plan",
						it.Ref, id, d, d, id),
				}
			}
			continue
		}

		// New: rewrite every dependency that names an existing ticket.
		rewritten := make([]string, 0, len(it.DependsOn))
		for _, dep := range it.DependsOn {
			d := strings.TrimSpace(dep)
			if id, ok := existing[d]; ok {
				// "#<id>": what CreateTasks parses as an existing ticket.
				rewritten = append(rewritten, "#"+fmt.Sprint(id))
				continue
			}
			if d != "" && !refs[d] && !isTicketIDRef(d) {
				return nil, &handoffItemRefusal{
					msg: fmt.Sprintf("ref %q depends on %q, which is neither a ref in this batch, a ref an earlier attempt of this handoff created, nor a ticket id",
						it.Ref, d),
				}
			}
			rewritten = append(rewritten, d)
		}
		next := it
		next.DependsOn = rewritten
		batch.specs = append(batch.specs, next)
	}
	return batch, nil
}

// isTicketIDRef reports whether d is how CreateTasks spells an existing ticket id:
// "42" or "#42" (plan.go's dependency resolution).
func isTicketIDRef(d string) bool {
	n := strings.TrimPrefix(strings.TrimSpace(d), "#")
	if n == "" {
		return false
	}
	for i := 0; i < len(n); i++ {
		if n[i] < '0' || n[i] > '9' {
			return false
		}
	}
	return true
}

// handoffSpecs turns this path's narrow items into CreateTasks' TaskSpecs, and is
// where property 3 is enforced: the description is domain.HandoffBrief's output, so
// the request's words are inside the fence on every ticket this path creates.
//
// The fields this path must not set are left at their zero values on purpose, and
// each one is named so a future edit has to delete a sentence to change the rule:
// no AssignedTo, no Owner (CreateTasks' default owner is the calling triager, which is
// the plan author — never anything the request said), no StartAt, no
// Performer/HumanReason, no DecideBy. The metadata carries exactly one key — the
// ledger marker — so CreateTasks is otherwise handed an empty Metadata and nothing
// from the request can reach a metadata argument either.
func (s *Service) handoffSpecs(req *domain.TeamRequest, key string, items []HandoffItem) []TaskSpec {
	specs := make([]TaskSpec, len(items))
	for i, it := range items {
		specs[i] = TaskSpec{
			Ref:                it.Ref,
			StoryID:            it.StoryID,
			Name:               it.Name,
			Description:        domain.HandoffBrief(it.Plan, req),
			AcceptanceCriteria: it.AcceptanceCriteria,
			Priority:           it.Priority,
			DependsOn:          it.DependsOn,
			Metadata: domain.Metadata{
				domain.MetaRequestHandoff: domain.RequestHandoffMark(req.ID, key, it.Ref),
			},
		}
	}
	return specs
}

// handoffCreateError renders CreateTasks' mid-batch failure with the batch's real
// state: what this attempt created (they exist, they are marked, a retry reconciles
// them) and what it did not. The error is a 502-class failure rather than a 400 —
// the caller's batch was valid, the work just did not finish — so the caller retries
// with the SAME key and the ledger picks up where this attempt stopped.
func (s *Service) handoffCreateError(ctx context.Context, req *domain.TeamRequest, result *HandoffRequestResult, created []CreatedTask, cause error) error {
	made := 0
	for _, c := range created {
		if c.ID != 0 {
			made++
		}
	}
	return &codedCause{
		err: fmt.Errorf("planning request %s stopped part-way: %d of the batch's tickets were created and are recorded against this handoff, so they are not lost. "+
			"Retry with the same handoff_key and the batch will plan only what is missing: %w", req.PublicRef, made, cause),
		cause: cause,
	}
}

// createdCount and linkedCount summarize an attempt for the audit row and for
// "did anything actually happen?".
func createdCount(items []HandoffItemResult) int {
	n := 0
	for _, it := range items {
		if it.Created {
			n++
		}
	}
	return n
}

func linkedCount(items []HandoffItemResult) int {
	n := 0
	for _, it := range items {
		if it.Linked {
			n++
		}
	}
	return n
}

// handoffLockKey names one request's handoff lock. The request id alone is enough:
// request ids are global serials, so two tenants cannot share one.
func handoffLockKey(requestID int) string {
	return fmt.Sprintf("request-handoff:%d", requestID)
}

// recordRequestHandoff writes the one audit row a handoff gets: `request.handoff`, on
// the request's org, targeting the request, carrying the ledger it acted on as ids.
//
// It runs after the work committed, and how it fails is audit.go's rule: a failure is
// logged rather than returned, because a plan that exists must not be reported as
// failed over its audit row. A call that created and linked nothing never reaches
// here — an audit row for a no-op would say an act happened that did not.
//
// The lineage itself is NOT this row. Each created ticket's `planned_as` relation is
// a typed request_links row (055) and its own `link` history event (054, I6), written
// by AttachRequest's repository path; the audit row is the act's record for the audit
// log. The details are ids, counts and the caller's own key — never a ticket name, a
// brief or an email, because a request's history is requester-visible and its audit
// rows sit beside it.
func (s *Service) recordRequestHandoff(ctx context.Context, p authz.Principal, team *domain.Team,
	req *domain.TeamRequest, projectID int, key string, result *HandoffRequestResult) {
	ledger := make([]map[string]any, 0, len(result.Items))
	for _, it := range result.Items {
		ledger = append(ledger, map[string]any{
			"ref":     it.Ref,
			"task_id": it.TaskID,
			"created": it.Created,
			"linked":  it.Linked,
		})
	}
	s.audit(ctx, p, domain.AuditEntry{
		Action:     "request.handoff",
		OrgID:      intRef(team.OrgID),
		TargetType: "request",
		TargetID:   idStr(req.ID),
		TargetName: req.PublicRef,
		Summary: fmt.Sprintf("planned request %s into project %d: %d created, %d linked",
			req.PublicRef, projectID, createdCount(result.Items), linkedCount(result.Items)),
		Details: map[string]any{
			"team_id":     team.ID,
			"public_ref":  req.PublicRef,
			"project_id":  projectID,
			"handoff_key": key,
			"ledger":      ledger,
			"created":     createdCount(result.Items),
			"linked":      linkedCount(result.Items),
		},
	})
}
