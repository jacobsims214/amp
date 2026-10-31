package main

// Dispatch integrity (AMP #2801, epic 180, story 444): a dispatched ticket
// isn't started until its worker claims it, every state-changing result says
// what to do next, a dispatched ticket nobody starts is flagged and then
// returned, and a completion that isn't the worker's is labelled a closeout.
//
// These go through the MCP surface like an agent does, and through the
// service for the watchdog (which is a background job, not a tool call).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// ageDispatch backdates a ticket's dispatch so a test doesn't wait 30
// minutes for the watchdog: dispatched minutes ago, not started.
func ageDispatch(t *testing.T, s *mcpStack, id int, minutes int) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `UPDATE tasks SET dispatched_at = NOW() - $2::float8 * interval '1 minute' WHERE id = $1`,
		id, float64(minutes)); err != nil {
		t.Fatalf("age dispatch of %d: %v", id, err)
	}
}

// dispatchClaims dispatches id and returns the dispatch_id the result handed
// out, which is what a manager would pass to its worker.
func dispatchClaims(t *testing.T, s *mcpStack, id int) string {
	t.Helper()
	out := s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(id), "agent_id": "amp-worker"})
	id2, _ := out["dispatch_id"].(string)
	if id2 == "" {
		t.Fatalf("dispatch of %d returned no dispatch_id: %v", id, out)
	}
	return id2
}

// A dispatched ticket is not started: phase says so, the dispatch result
// carries everything needed to launch the worker, and the watchdog lists it
// once the flag timeout has passed.
func TestDispatchIntegrity_DispatchedIsNotStarted(t *testing.T) {
	s := setupMCP(t)
	id := mkIn(t, s, s.storyID, "nobody will start this")

	out := s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(id), "agent_id": "amp-worker"})
	for _, k := range []string{"dispatch_id", "worker_prompt", "next", "start_by"} {
		if v, _ := out[k].(string); v == "" {
			t.Fatalf("dispatch result lacks %s: %v", k, out)
		}
	}
	if out["phase"] != domain.PhaseNotStarted || out["state"] != "in_progress" {
		t.Fatalf("dispatch result phase/state = %v/%v, want not_started/in_progress", out["phase"], out["state"])
	}
	prompt, _ := out["worker_prompt"].(string)
	dispatchID, _ := out["dispatch_id"].(string)
	if !strings.Contains(prompt, fmt.Sprintf("amp_claim_task(task_id=%d, dispatch_id=%q)", id, dispatchID)) {
		t.Fatalf("worker_prompt doesn't tell the worker how to claim:\n%s", prompt)
	}
	if next, _ := out["next"].(string); !strings.Contains(next, "Start the worker now") {
		t.Fatalf("dispatch next doesn't say to launch the worker: %q", next)
	}

	// The ticket: in progress, no start, the token kept.
	task := planTask(t, s, id)
	if task["started_at"] != nil || task["phase"] != domain.PhaseNotStarted {
		t.Fatalf("ticket after dispatch = %v (started_at %v), want not started", task["phase"], task["started_at"])
	}
	if task["dispatch_id"] != dispatchID {
		t.Fatalf("ticket dispatch_id = %v, want %q", task["dispatch_id"], dispatchID)
	}

	// Too early to flag: it is the normal state right after a dispatch.
	board := s.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(s.projectID), "owner": "any"})
	if got := board["not_started"].([]interface{}); len(got) != 0 {
		t.Fatalf("a fresh dispatch is already flagged: %v", got)
	}

	// Past the flag timeout (10 min): listed, with how long.
	ageDispatch(t, s, id, 12)
	board = s.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(s.projectID), "owner": "any"})
	listed := board["not_started"].([]interface{})
	if len(listed) != 1 {
		t.Fatalf("not_started = %v, want the one ticket", listed)
	}
	item := listed[0].(map[string]interface{})
	if int(item["id"].(float64)) != id || int(item["minutes"].(float64)) != 12 || item["agent_id"] != "amp-worker" {
		t.Fatalf("not_started item = %v", item)
	}
}

// The manager sees a not-started ticket in its owner's waiting_on_people as
// kind "start", which is the same item My work renders.
func TestDispatchIntegrity_NotStartedIsWaitingOnTheOwner(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	id := mkIn(t, alice, w.s.storyID, "dispatched, never started")
	dispatchClaims(t, alice, id)
	ageDispatch(t, w.s, id, 15)

	board := alice.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(w.s.projectID)})
	people := board["waiting_on_people"].([]interface{})
	var start map[string]interface{}
	for _, x := range people {
		m := x.(map[string]interface{})
		if m["kind"] == service.WaitStart {
			start = m
		}
	}
	if start == nil {
		t.Fatalf("no start item in waiting_on_people: %v", people)
	}
	if int(start["id"].(float64)) != id || start["state"] != domain.PhaseNotStarted || int(start["not_started_minutes"].(float64)) < 14 {
		t.Fatalf("start item = %v", start)
	}
}

// A worker starts its ticket by claiming it with the dispatch_id, and the
// claim shows on the ticket. A repeat is harmless; a token from an earlier
// dispatch is refused.
func TestDispatchIntegrity_ClaimStartsTheTicket(t *testing.T) {
	s := setupMCP(t)
	id := mkIn(t, s, s.storyID, "claim me")
	dispatchID := dispatchClaims(t, s, id)

	out := s.call(t, "amp_claim_task", map[string]interface{}{"task_id": float64(id), "dispatch_id": dispatchID})
	if out["phase"] != domain.PhaseStarted || out["dispatch_id"] != dispatchID {
		t.Fatalf("claim result = %v, want started under %q", out, dispatchID)
	}
	if next, _ := out["next"].(string); !strings.Contains(next, "Finish this ticket yourself") {
		t.Fatalf("claim next doesn't send the worker to finish it: %q", next)
	}
	task := planTask(t, s, id)
	if task["started_at"] == nil || task["phase"] != domain.PhaseStarted {
		t.Fatalf("ticket after claim = %v, want started", task)
	}

	// The claim is on the ticket as a comment.
	comments, _ := s.repo.GetComments(context.Background(), id)
	found := false
	for _, c := range comments {
		if strings.Contains(c.Body, "picked this up") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no claim comment on the ticket: %+v", comments)
	}

	// Idempotent: the same claim again changes nothing and says so.
	again := s.call(t, "amp_claim_task", map[string]interface{}{"task_id": float64(id), "dispatch_id": dispatchID})
	if again["already_started"] != true {
		t.Fatalf("repeat claim = %v, want already_started", again)
	}

	// The ticket is started, so it is never flagged or returned.
	ageDispatch(t, s, id, 45)
	board := s.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(s.projectID), "owner": "any"})
	if got := board["not_started"].([]interface{}); len(got) != 0 {
		t.Fatalf("a started ticket is listed as not started: %v", got)
	}
	_, _ = service.New(s.repo, s.reg, s.kb).SweepNotStarted(context.Background(), allowAll)
	if after, _ := s.repo.GetTask(context.Background(), id); after.State != domain.TaskStateInProgress || after.ReturnedAt != nil {
		t.Fatalf("watchdog touched a started ticket: %v %v", after.State, after.ReturnedAt)
	}

	// A stale token — the ticket was returned and dispatched again — is
	// refused, naming what the worker should do.
	other := mkIn(t, s, s.storyID, "re-dispatched")
	old := dispatchClaims(t, s, other)
	if _, err := s.repo.GetTask(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	s.call(t, "amp_return_task", map[string]interface{}{"task_id": float64(other), "reason": "worker died"})
	newID := dispatchClaims(t, s, other)
	if old == newID {
		t.Fatalf("re-dispatch reused the token %q", old)
	}
	msg := s.callExpectError(t, "amp_claim_task", map[string]interface{}{"task_id": float64(other), "dispatch_id": old})
	if !strings.Contains(msg, "no longer current") {
		t.Fatalf("stale claim: %q", msg)
	}
	// The current one works.
	s.call(t, "amp_claim_task", map[string]interface{}{"task_id": float64(other), "dispatch_id": newID})
}

// A plugin that doesn't claim still counts as started at its first activity:
// a comment, a run report, a block. That is what keeps old workers out of the
// watchdog.
func TestDispatchIntegrity_FirstActivityIsTheFallbackStart(t *testing.T) {
	for _, c := range []struct {
		name string
		call func(t *testing.T, s *mcpStack, id int)
	}{
		{"comment", func(t *testing.T, s *mcpStack, id int) {
			s.call(t, "amp_add_task_comment", map[string]interface{}{"task_id": float64(id), "body": "Starting work.", "author": "amp-worker"})
		}},
		{"report run", func(t *testing.T, s *mcpStack, id int) {
			s.call(t, "amp_report_run", map[string]interface{}{"task_id": float64(id), "outcome": "progress", "model": "m"})
		}},
		{"block", func(t *testing.T, s *mcpStack, id int) {
			s.call(t, "amp_block_task", map[string]interface{}{"task_id": float64(id), "reason": "Stripe or Avalara?"})
		}},
		{"complete", func(t *testing.T, s *mcpStack, id int) {
			s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(id)})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := setupMCP(t)
			id := mkIn(t, s, s.storyID, "old plugin worker")
			dispatchClaims(t, s, id) // the manager hands out a token the worker ignores
			c.call(t, s, id)
			task, err := s.repo.GetTask(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if task.StartedAt == nil || task.StartedBy != domain.StartedByInferred {
				t.Fatalf("after the first activity: started_at %v started_by %q, want an inferred start", task.StartedAt, task.StartedBy)
			}
		})
	}
}

// Every state-changing result carries next, worded for its caller.
func TestDispatchIntegrity_NextOnEveryStateChangingResult(t *testing.T) {
	s := setupMCP(t)
	id := mkIn(t, s, s.storyID, "next everywhere")
	dispatchID := dispatchClaims(t, s, id)

	for _, c := range []struct {
		tool string
		args map[string]interface{}
		want string
	}{
		{"amp_claim_task", map[string]interface{}{"task_id": float64(id), "dispatch_id": dispatchID}, "Finish this ticket yourself"},
		{"amp_add_task_comment", map[string]interface{}{"task_id": float64(id), "body": "finding", "author": "amp-worker"}, "Keep working"},
		{"amp_report_run", map[string]interface{}{"task_id": float64(id), "outcome": "progress", "model": "m"}, "still needs amp_complete_task"},
		{"amp_set_task_state", map[string]interface{}{"task_id": float64(id), "state": "blocked", "reason": "a question"}, "is blocked"},
		{"amp_answer_question", map[string]interface{}{"task_id": float64(id), "answer": "do it"}, "dispatch it again"},
	} {
		out := s.call(t, c.tool, c.args)
		next, _ := out["next"].(string)
		if !strings.Contains(next, c.want) {
			t.Errorf("%s next = %q, want it to contain %q", c.tool, next, c.want)
		}
	}

	// A re-dispatch of a ticket that was returned says it was returned.
	orphan := mkIn(t, s, s.storyID, "returned then dispatched again")
	dispatchClaims(t, s, orphan)
	ret := s.call(t, "amp_return_task", map[string]interface{}{"task_id": float64(orphan), "reason": "worker timed out"})
	if next, _ := ret["next"].(string); !strings.Contains(next, "back in backlog") {
		t.Errorf("return next = %q", next)
	}
	again := s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(orphan), "agent_id": "amp-worker"})
	if again["previously_returned"] == nil || again["next"] == "" {
		t.Fatalf("re-dispatch of a returned ticket = %v", again)
	}

	// A worker's own completion and a closeout say different things.
	worker := mkIn(t, s, s.storyID, "worker completes")
	wtok := dispatchClaims(t, s, worker)
	s.call(t, "amp_claim_task", map[string]interface{}{"task_id": float64(worker), "dispatch_id": wtok})
	done := s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(worker), "dispatch_id": wtok})
	if done["closeout"] == true {
		t.Fatalf("the worker's own completion was labelled a closeout: %v", done)
	}
	if next, _ := done["next"].(string); !strings.Contains(next, "Tell your manager") {
		t.Errorf("worker completion next = %q", next)
	}
}

// A completion without the ticket's dispatch token is a closeout: allowed,
// recorded as one, and it never needs a dispatch first when the ticket has
// runs (the #2473/#2494 pattern).
func TestDispatchIntegrity_CloseoutLabelling(t *testing.T) {
	s := setupMCP(t)

	// 1. A ready ticket with a run is closed out directly: no dispatch.
	ready := mkIn(t, s, s.storyID, "work done, ticket still ready")
	s.call(t, "amp_report_run", map[string]interface{}{"task_id": float64(ready), "outcome": "progress",
		"model": "m", "branch": "b", "commit": "abcdef1"})
	out := s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(ready),
		"closeout_reason": "the worker's session died; its commit is in"})
	if out["closeout"] != true {
		t.Fatalf("direct closeout = %v, want closeout", out)
	}
	if next, _ := out["next"].(string); !strings.Contains(next, "Closed out by you") {
		t.Errorf("closeout next = %q", next)
	}
	// History says it was not the worker's, with the reason.
	hist, _ := s.repo.GetTicketHistory(context.Background(), ready)
	var closed *domain.ActivityLog
	for i := range hist {
		if hist[i].Action == "closed_out" {
			closed = &hist[i]
		}
	}
	if closed == nil || !strings.Contains(closed.Detail, "not the worker") || !strings.Contains(closed.Detail, "session died") {
		t.Fatalf("closeout history = %+v", hist)
	}
	// The run says so too.
	runs, _ := s.repo.TaskRuns(context.Background(), ready)
	if len(runs) < 2 || !strings.HasPrefix(runs[len(runs)-1].Note, "closeout") {
		t.Fatalf("closeout run note = %+v", runs)
	}

	// 2. A ready ticket with nothing on it is refused: no dispatch, no work,
	// no closeout.
	bare := mkIn(t, s, s.storyID, "nothing on it")
	msg := s.callExpectError(t, "amp_complete_task", map[string]interface{}{"task_id": float64(bare)})
	if !strings.Contains(msg, "never dispatched") {
		t.Fatalf("closeout of an empty ready ticket: %q", msg)
	}

	// 3. A claimed ticket finished by somebody else (no token) is a closeout
	// of a started ticket.
	busy := mkIn(t, s, s.storyID, "worker died mid-run")
	tok := dispatchClaims(t, s, busy)
	s.call(t, "amp_claim_task", map[string]interface{}{"task_id": float64(busy), "dispatch_id": tok})
	out = s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(busy), "closeout_reason": "AMP connection lost"})
	if out["closeout"] != true {
		t.Fatalf("closeout of a started ticket = %v", out)
	}

	// 4. A stale token is refused, not silently downgraded to a closeout.
	other := mkIn(t, s, s.storyID, "returned and re-dispatched")
	stale := dispatchClaims(t, s, other)
	s.call(t, "amp_return_task", map[string]interface{}{"task_id": float64(other), "reason": "nobody started it"})
	fresh := dispatchClaims(t, s, other)
	s.call(t, "amp_claim_task", map[string]interface{}{"task_id": float64(other), "dispatch_id": fresh})
	msg = s.callExpectError(t, "amp_complete_task", map[string]interface{}{"task_id": float64(other), "dispatch_id": stale})
	if !strings.Contains(msg, "no longer current") {
		t.Fatalf("complete with a stale token: %q", msg)
	}
}

// A dispatched ticket nobody started, completed without the dispatch token
// (AMP #2908): with an explicit closeout_reason it is a labelled closeout;
// without one it stays the old plugin's own completion (backward
// compatibility); with the current token it is the worker's.
func TestDispatchIntegrity_CloseoutReasonOnNeverStarted(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()
	historyOf := func(id int, action string) *domain.ActivityLog {
		hist, _ := s.repo.GetTicketHistory(ctx, id)
		for i := range hist {
			if hist[i].Action == action {
				return &hist[i]
			}
		}
		return nil
	}
	lastRunNote := func(id int) string {
		runs, _ := s.repo.TaskRuns(ctx, id)
		if len(runs) == 0 {
			return ""
		}
		return runs[len(runs)-1].Note
	}
	startedBy := func(id int) string {
		tk, err := s.repo.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return tk.StartedBy
	}

	// 1. With a reason: a closeout, labelled everywhere.
	closed := mkIn(t, s, s.storyID, "dispatched, never started, closed out")
	dispatchClaims(t, s, closed)
	out := s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(closed),
		"closeout_reason": "worker connection lost", "model": "m"})
	if out["closeout"] != true || out["closeout_reason"] != "worker connection lost" {
		t.Fatalf("closeout with a reason = %v", out)
	}
	if next, _ := out["next"].(string); !strings.Contains(next, "Closed out by you") {
		t.Errorf("closeout next = %q", next)
	}
	if h := historyOf(closed, "closed_out"); h == nil || !strings.Contains(h.Detail, "(not the worker): worker connection lost") {
		t.Fatalf("closeout history = %+v", h)
	}
	if historyOf(closed, "completed") != nil {
		t.Errorf("closeout also recorded as the worker's completion")
	}
	if got := startedBy(closed); got != domain.StartedByCloseout {
		t.Errorf("started_by = %q, want %q", got, domain.StartedByCloseout)
	}
	if note := lastRunNote(closed); note != "closeout: worker connection lost" {
		t.Errorf("run note = %q", note)
	}

	// 2. No reason: unchanged, the old plugin's own completion.
	legacy := mkIn(t, s, s.storyID, "dispatched, never started, bare completion")
	dispatchClaims(t, s, legacy)
	out = s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(legacy), "model": "m", "run_note": "did the work"})
	if out["closeout"] == true {
		t.Fatalf("bare completion of a never-started ticket labelled a closeout: %v", out)
	}
	if h := historyOf(legacy, "completed"); h == nil || !strings.Contains(h.Detail, "Task completed by agent") {
		t.Fatalf("legacy completion history = %+v", h)
	}
	if historyOf(legacy, "closed_out") != nil {
		t.Errorf("legacy completion has a closed_out row")
	}
	if got := startedBy(legacy); got != domain.StartedByInferred {
		t.Errorf("started_by = %q, want %q", got, domain.StartedByInferred)
	}
	if note := lastRunNote(legacy); note != "did the work" {
		t.Errorf("legacy run note = %q", note)
	}

	// 3. The current token: the worker's, even with a reason.
	worker := mkIn(t, s, s.storyID, "dispatched, worker completes without claiming")
	tok := dispatchClaims(t, s, worker)
	out = s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(worker), "dispatch_id": tok,
		"closeout_reason": "ignored for the worker"})
	if out["closeout"] == true {
		t.Fatalf("the worker's own completion was labelled a closeout: %v", out)
	}
	if h := historyOf(worker, "completed"); h == nil || historyOf(worker, "closed_out") != nil {
		t.Fatalf("worker completion history = %+v", h)
	}
}

// The watchdog returns a ticket nobody started once its project's return
// timeout has passed, and a manager sees it in the returned list until it is
// dispatched again.
func TestDispatchIntegrity_WatchdogReturnsUnstartedTicket(t *testing.T) {
	s := setupMCP(t)
	svc := service.New(s.repo, s.reg, s.kb)
	ctx := context.Background()

	dead := mkIn(t, s, s.storyID, "nobody started this")
	dispatchClaims(t, s, dead)
	ageDispatch(t, s, dead, 31)

	n, err := svc.SweepNotStarted(ctx, allowAll)
	if err != nil || n != 1 {
		t.Fatalf("sweep: returned %d, err %v; want 1", n, err)
	}
	task, _ := s.repo.GetTask(ctx, dead)
	if task.State != domain.TaskStateBacklog {
		t.Fatalf("returned ticket state = %s, want backlog", task.State)
	}
	if task.DispatchID != "" || task.DispatchedAt != nil {
		t.Fatalf("returned ticket kept its dispatch: id %q at %v", task.DispatchID, task.DispatchedAt)
	}
	if task.ReturnedAt == nil || !strings.Contains(task.ReturnedReason, "not started within 30 min of dispatch to amp-worker") {
		t.Fatalf("returned reason = %q at %v", task.ReturnedReason, task.ReturnedAt)
	}

	// History and audit say who returned it and why.
	hist, _ := s.repo.GetTicketHistory(ctx, dead)
	var returned *domain.ActivityLog
	for i := range hist {
		if hist[i].Action == "returned" {
			returned = &hist[i]
		}
	}
	if returned == nil || returned.Actor != "system" || !strings.Contains(returned.Detail, "Returned to ready") {
		t.Fatalf("return history = %+v", hist)
	}

	// The board lists it under returned until it's dispatched again.
	board := s.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(s.projectID), "owner": "any"})
	list := board["returned"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("returned list = %v", list)
	}
	item := list[0].(map[string]interface{})
	if int(item["id"].(float64)) != dead || !strings.Contains(item["returned_reason"].(string), "not started within 30 min") {
		t.Fatalf("returned item = %v", item)
	}
	if ready := board["ready_to_dispatch"].([]interface{}); len(ready) != 1 || int(ready[0].(float64)) != dead {
		t.Fatalf("returned ticket isn't ready again: %v", ready)
	}

	// Dispatching it again clears the mark.
	s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(dead), "agent_id": "amp-worker"})
	board = s.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(s.projectID), "owner": "any"})
	if got := board["returned"].([]interface{}); len(got) != 0 {
		t.Fatalf("re-dispatched ticket is still listed as returned: %v", got)
	}
}

// Two replicas sweeping at once return a ticket once, not twice.
func TestDispatchIntegrity_WatchdogTwoReplicas(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()

	// Two services over the same database and actor registry, as two
	// replicas are: both would sweep, but only one takes the lock.
	replicaA := service.New(s.repo, s.reg, s.kb)
	replicaB := service.New(s.repo, s.reg, s.kb)

	for i := 0; i < 5; i++ {
		id := mkIn(t, s, s.storyID, fmt.Sprintf("orphan %d", i))
		dispatchClaims(t, s, id)
		ageDispatch(t, s, id, 40)
	}

	// The real lock: a named advisory lock on its own connection.
	lock := s.repo.TryNamedLock
	type res struct {
		n   int
		err error
	}
	results := make(chan res, 2)
	for _, svc := range []*service.Service{replicaA, replicaB} {
		go func(svc *service.Service) {
			n, err := svc.SweepNotStarted(ctx, lock)
			results <- res{n, err}
		}(svc)
	}
	total := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("sweep: %v", r.err)
		}
		total += r.n
	}
	if total != 5 {
		t.Fatalf("two replicas returned %d tickets, want 5 exactly once", total)
	}

	// Every ticket carries exactly one return, so the second replica did
	// nothing rather than returning them again.
	for _, id := range allTaskIDs(t, s) {
		hist, err := s.repo.GetTicketHistory(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		returns := 0
		for _, h := range hist {
			if h.Action == "returned" {
				returns++
			}
		}
		if returns > 1 {
			t.Fatalf("task %d was returned %d times", id, returns)
		}
	}
}

// The timeout settings are per project, and 0 turns each off.
func TestDispatchIntegrity_WatchdogSettings(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	ctx := context.Background()

	off := w.s.call(t, "amp_update_project_settings", map[string]interface{}{
		"project_id": float64(w.s.projectID), "not_started_flag_minutes": float64(0), "not_started_return_minutes": float64(0),
	})
	proj := off["project"].(map[string]interface{})
	if proj["not_started_flag_minutes"].(float64) != 0 || proj["not_started_return_minutes"].(float64) != 0 {
		t.Fatalf("settings = %v/%v, want 0/0", proj["not_started_flag_minutes"], proj["not_started_return_minutes"])
	}

	id := mkIn(t, alice, w.s.storyID, "returning is off")
	dispatchClaims(t, alice, id)
	ageDispatch(t, w.s, id, 120)
	if n, err := svc.SweepNotStarted(ctx, allowAll); err != nil || n != 0 {
		t.Fatalf("with returning off the sweep returned %d (err %v)", n, err)
	}
	board := alice.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(w.s.projectID)})
	if got := board["not_started"].([]interface{}); len(got) != 0 {
		t.Fatalf("with flagging off the ticket is listed: %v", got)
	}

	// Back on with a shorter timeout: returned at once.
	back := w.s.call(t, "amp_update_project_settings", map[string]interface{}{
		"project_id": float64(w.s.projectID), "not_started_flag_minutes": float64(1), "not_started_return_minutes": float64(2),
	})
	if back["project"].(map[string]interface{})["not_started_return_minutes"].(float64) != 2 {
		t.Fatalf("settings didn't take: %v", back)
	}
	if n, err := svc.SweepNotStarted(ctx, allowAll); err != nil || n != 1 {
		t.Fatalf("sweep with a 2 min timeout: %d (err %v), want 1", n, err)
	}
}

// amp_return_task is the harness's release: it returns a dispatched ticket
// with its own reason, and refuses one a worker already started.
func TestDispatchIntegrity_ReturnTask(t *testing.T) {
	s := setupMCP(t)
	id := mkIn(t, s, s.storyID, "worker crashed")
	dispatchClaims(t, s, id)

	out := s.call(t, "amp_return_task", map[string]interface{}{"task_id": float64(id), "reason": "worker amp-worker-backend timed out after 20 min"})
	if out["returned"] != true || out["state"] != "backlog" {
		t.Fatalf("return = %v", out)
	}
	if !strings.Contains(out["returned_reason"].(string), "timed out after 20 min") {
		t.Fatalf("returned_reason = %v", out["returned_reason"])
	}
	if next, _ := out["next"].(string); !strings.Contains(next, "back in backlog") {
		t.Errorf("return next = %q", next)
	}

	// A started ticket isn't returned from under its worker.
	busy := mkIn(t, s, s.storyID, "worker is running")
	tok := dispatchClaims(t, s, busy)
	s.call(t, "amp_claim_task", map[string]interface{}{"task_id": float64(busy), "dispatch_id": tok})
	msg := s.callExpectError(t, "amp_return_task", map[string]interface{}{"task_id": float64(busy), "reason": "give up"})
	if !strings.Contains(msg, "was started") {
		t.Fatalf("return of a started ticket: %q", msg)
	}
}

// The run holding a ticket's current dispatch may return its own started
// ticket (AMP #2909): the harness claims for its worker, so every ticket it
// hands back is started. It comes back returned (reason, returned list,
// history) with the start cleared. Without the dispatch_id, or with a stale
// one, a started ticket is still refused.
func TestDispatchIntegrity_ReturnStartedByCurrentHolder(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()

	id := mkIn(t, s, s.storyID, "harness worker gave up")
	tok := dispatchClaims(t, s, id)
	s.call(t, "amp_claim_task", map[string]interface{}{"task_id": float64(id), "dispatch_id": tok})

	// No dispatch_id: refused as before, and nothing changes.
	msg := s.callExpectError(t, "amp_return_task", map[string]interface{}{"task_id": float64(id), "reason": "give up"})
	if !strings.Contains(msg, "was started") {
		t.Fatalf("return of a started ticket without dispatch_id: %q", msg)
	}
	// A stale dispatch_id: refused, naming the current one.
	msg = s.callExpectError(t, "amp_return_task", map[string]interface{}{"task_id": float64(id), "reason": "give up",
		"dispatch_id": "d_notthecurrentone"})
	if !strings.Contains(msg, "no longer current") || !strings.Contains(msg, tok) {
		t.Fatalf("return with a stale dispatch_id: %q", msg)
	}
	if task, _ := s.repo.GetTask(ctx, id); task.State != domain.TaskStateInProgress || task.StartedAt == nil || task.DispatchID != tok {
		t.Fatalf("refused returns changed the ticket: state %s started %v dispatch %q", task.State, task.StartedAt, task.DispatchID)
	}

	// The current holder returns it.
	reason := "abandoned: cancelled: the manager's run was cancelled"
	out := s.call(t, "amp_return_task", map[string]interface{}{"task_id": float64(id), "reason": reason, "dispatch_id": tok})
	if out["returned"] != true || out["state"] != "backlog" || out["returned_reason"] != reason {
		t.Fatalf("return by the current holder = %v", out)
	}
	task, _ := s.repo.GetTask(ctx, id)
	if task.State != domain.TaskStateBacklog || task.ReturnedAt == nil || task.ReturnedReason != reason {
		t.Fatalf("returned ticket: state %s returned_at %v reason %q", task.State, task.ReturnedAt, task.ReturnedReason)
	}
	if task.StartedAt != nil || task.StartedBy != "" || task.DispatchID != "" || task.DispatchedAt != nil {
		t.Fatalf("returned ticket kept its run: started %v by %q, dispatch %q at %v",
			task.StartedAt, task.StartedBy, task.DispatchID, task.DispatchedAt)
	}
	hist, _ := s.repo.GetTicketHistory(ctx, id)
	var returned *domain.ActivityLog
	for i := range hist {
		if hist[i].Action == "returned" {
			returned = &hist[i]
		}
	}
	if returned == nil || !strings.Contains(returned.Detail, "Returned to ready: "+reason) {
		t.Fatalf("return history = %+v", hist)
	}
	board := s.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(s.projectID), "owner": "any"})
	list := board["returned"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("returned list = %v", list)
	}
	if item := list[0].(map[string]interface{}); int(item["id"].(float64)) != id || item["returned_reason"] != reason {
		t.Fatalf("returned item = %v", item)
	}

	// The same token again is now stale: the dispatch it named is gone.
	msg = s.callExpectError(t, "amp_return_task", map[string]interface{}{"task_id": float64(id), "reason": "again", "dispatch_id": tok})
	if !strings.Contains(msg, "not in progress") && !strings.Contains(msg, "no longer current") {
		t.Fatalf("second return with the spent token: %q", msg)
	}
}

// The backfill: an in_progress ticket that predates started_at reads as
// started, so nothing in flight is flagged or returned.
func TestDispatchIntegrity_BackfillLeavesTicketsInFlightAlone(t *testing.T) {
	s := setupMCP(t)
	id := mkIn(t, s, s.storyID, "in flight when it landed")
	dispatchClaims(t, s, id)

	// Simulate the pre-migration row: in progress, no start, dispatched long
	// ago, then the migration's backfill.
	ctx := context.Background()
	ageDispatch(t, s, id, 120)
	conn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`UPDATE tasks SET started_at = NULL, started_by = '', dispatch_id = '' WHERE id = $1`,
		`UPDATE tasks SET started_at = COALESCE(dispatched_at, updated_at), started_by = 'backfill' WHERE id = $1`,
	} {
		if _, err := conn.Exec(ctx, q, id); err != nil {
			t.Fatal(err)
		}
	}
	task, _ := s.repo.GetTask(ctx, id)
	if task.StartedAt == nil || task.StartedBy != domain.StartedByBackfill {
		t.Fatalf("backfill = %v/%q", task.StartedAt, task.StartedBy)
	}
	if n, err := service.New(s.repo, s.reg, s.kb).SweepNotStarted(ctx, allowAll); err != nil || n != 0 {
		t.Fatalf("the watchdog returned a backfilled ticket: %d (err %v)", n, err)
	}
}

// allowAll is a no-op lock: the tests that aren't about the multi-replica
// lock sweep without one.
func allowAll(context.Context, string) (func(), bool, error) {
	return func() {}, true, nil
}

// The web UI's REST twin of amp_return_task: POST /api/tasks/{id}/return.
// My work offers "Return to ready" on a dispatched ticket no worker started
// (waiting_on_people kind "start"), and that button needs a route (#2803).
func TestDispatchIntegrity_RestReturnTask(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	s := w.as(w.alice)

	path := func(id int) string { return fmt.Sprintf("/api/tasks/%d/return", id) }
	state := func(id int) *domain.Task {
		t.Helper()
		tk, err := w.s.repo.GetTask(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}

	// A dispatched ticket no worker started goes back to ready, with the reason.
	id := mkIn(t, s, w.s.storyID, "no worker started it")
	dispatchClaims(t, s, id)
	out := ra.want(t, http.StatusOK, "alice", http.MethodPost, path(id), map[string]any{"reason": "no worker started it"})
	task, _ := out["task"].(map[string]any)
	if task == nil || task["state"] != "backlog" {
		t.Fatalf("return = %v", out)
	}
	got := state(id)
	if got.ReturnedAt == nil || !strings.Contains(got.ReturnedReason, "no worker started it") {
		t.Fatalf("returned_at/reason = %v/%q", got.ReturnedAt, got.ReturnedReason)
	}
	if got.DispatchID != "" || got.DispatchedAt != nil {
		t.Fatalf("the dispatch was not cleared: %q %v", got.DispatchID, got.DispatchedAt)
	}

	// A started ticket is somebody's live run: refused, like the tool.
	busy := mkIn(t, s, w.s.storyID, "worker is running")
	tok := dispatchClaims(t, s, busy)
	s.call(t, "amp_claim_task", map[string]any{"task_id": float64(busy), "dispatch_id": tok})
	if code, body := ra.do(t, "alice", http.MethodPost, path(busy), map[string]any{"reason": "give up"}); code != http.StatusConflict {
		t.Fatalf("returning a started ticket: %d %v, want 409", code, body)
	} else if !strings.Contains(fmt.Sprint(body["error"]), "was started") {
		t.Fatalf("returning a started ticket said %v", body["error"])
	}

	// A ticket that isn't dispatched at all can't be returned either.
	ready := mkIn(t, s, w.s.storyID, "never dispatched")
	if code, _ := ra.do(t, "alice", http.MethodPost, path(ready), map[string]any{"reason": "x"}); code != http.StatusConflict {
		t.Fatalf("returning a ready ticket: %d, want 409", code)
	}

	// A reason is required: it's what the manager and the history see.
	other := mkIn(t, s, w.s.storyID, "no reason given")
	dispatchClaims(t, s, other)
	if code, _ := ra.do(t, "alice", http.MethodPost, path(other), map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("returning without a reason: %d, want 400", code)
	}

	// GET isn't a return.
	if code, _ := ra.do(t, "alice", http.MethodGet, path(id), nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET return: %d, want 405", code)
	}
}

// Every task read carries what the board and My work render (AMP #2803): the
// phase, and how long a dispatched ticket has waited for a worker. The UI
// does no arithmetic of its own, so these have to be on the ticket itself,
// not only in the not_started list.
func TestDispatchIntegrity_TaskReadCarriesPhaseAndMinutes(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	s := w.as(w.alice)

	read := func(id int) map[string]any {
		t.Helper()
		out := ra.want(t, http.StatusOK, "alice", http.MethodGet, fmt.Sprintf("/api/tasks/%d", id), nil)
		tk, _ := out["task"].(map[string]any)
		if tk == nil {
			t.Fatalf("task %d: no task in %v", id, out)
		}
		return tk
	}

	// Dispatched, not started, dispatched 20 minutes ago: not_started + 20.
	waiting := mkIn(t, s, w.s.storyID, "no worker started it")
	dispatchClaims(t, s, waiting)
	ageDispatch(t, w.s, waiting, 20)
	tk := read(waiting)
	if tk["phase"] != domain.PhaseNotStarted {
		t.Fatalf("phase = %v, want %q", tk["phase"], domain.PhaseNotStarted)
	}
	// The minutes are derived from the row, so a read straight from the
	// repository sees the back-dated dispatch (the running project actor
	// still holds the ticket's in-memory dispatch time).
	aged, err := w.s.repo.GetTask(context.Background(), waiting)
	if err != nil {
		t.Fatal(err)
	}
	if aged.NotStartedMinutes < 19 || aged.NotStartedMinutes > 21 {
		t.Fatalf("repo not_started_minutes = %d, want about 20", aged.NotStartedMinutes)
	}
	// And it goes out under the name the UI reads.
	b, _ := json.Marshal(aged)
	if !strings.Contains(string(b), `"not_started_minutes":20`) && !strings.Contains(string(b), `"not_started_minutes":21`) {
		t.Fatalf("the JSON has no not_started_minutes: %s", b)
	}

	// Started: phase started, and no minutes at all (it isn't waiting).
	running := mkIn(t, s, w.s.storyID, "worker is running")
	tok := dispatchClaims(t, s, running)
	s.call(t, "amp_claim_task", map[string]any{"task_id": float64(running), "dispatch_id": tok})
	tk = read(running)
	if tk["phase"] != domain.PhaseStarted {
		t.Fatalf("phase = %v, want %q", tk["phase"], domain.PhaseStarted)
	}
	if _, ok := tk["not_started_minutes"]; ok {
		t.Fatalf("a started ticket carries not_started_minutes: %v", tk)
	}

	// Ready, done, blocked: no phase at all, so the UI shows nothing.
	ready := mkIn(t, s, w.s.storyID, "never dispatched")
	if tk = read(ready); tk["phase"] != nil {
		t.Fatalf("a ready ticket has a phase: %v", tk["phase"])
	}

	// The project task list the board reads carries the phase too.
	listed := ra.want(t, http.StatusOK, "alice", http.MethodGet, fmt.Sprintf("/api/projects/%d/tasks?owner=any", w.s.projectID), nil)
	var found map[string]any
	for _, x := range listed["tasks"].([]any) {
		if m := x.(map[string]any); int(m["id"].(float64)) == waiting {
			found = m
		}
	}
	if found == nil || found["phase"] != domain.PhaseNotStarted {
		t.Fatalf("the project task list's ticket = %v", found)
	}
}

// allTaskIDs lists the test project's tickets.
func allTaskIDs(t *testing.T, s *mcpStack) []int {
	t.Helper()
	tasks, err := s.repo.ListTasks(context.Background(), s.projectID, "")
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	out := make([]int, len(tasks))
	for i, task := range tasks {
		out[i] = task.ID
	}
	return out
}

// localDevPrincipal is the identity the test stack calls as (auth is
// disabled: the local-dev user, in-process).
func localDevPrincipal() authz.Principal { return authz.Principal{} }

var _ = time.Minute

// The actor's dispatch clears are persisted (AMP #2944): completion, a state
// change off in_progress, and an owner releasing a ticket mid-flight each
// leave dispatch_id empty in the database (and the release clears the start),
// so a reloaded actor can't make a revoked token current again.
func TestDispatchIntegrity_DispatchClearsArePersisted(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	ctx := context.Background()
	reload := func(id int) *domain.Task {
		t.Helper()
		task, err := w.s.repo.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("reload %d: %v", id, err)
		}
		return task
	}

	// 1. The worker's own completion.
	done := mkIn(t, alice, w.s.storyID, "completed by its worker")
	tok := dispatchClaims(t, alice, done)
	alice.call(t, "amp_claim_task", map[string]interface{}{"task_id": float64(done), "dispatch_id": tok})
	if got := reload(done).DispatchID; got != tok {
		t.Fatalf("dispatch_id before completion = %q, want %q", got, tok)
	}
	alice.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(done), "dispatch_id": tok})
	if got := reload(done); got.DispatchID != "" || got.State != domain.TaskStateCompleted {
		t.Fatalf("after completion: state %s dispatch_id %q, want completed and empty", got.State, got.DispatchID)
	}

	// 2. The manager moves a claimed ticket back to backlog: the worker's
	// token is revoked, and stays revoked after a reload.
	moved := mkIn(t, alice, w.s.storyID, "moved back by the manager")
	tok = dispatchClaims(t, alice, moved)
	alice.call(t, "amp_claim_task", map[string]interface{}{"task_id": float64(moved), "dispatch_id": tok})
	alice.call(t, "amp_set_task_state", map[string]interface{}{"task_id": float64(moved), "state": "backlog"})
	if got := reload(moved); got.DispatchID != "" || got.State != domain.TaskStateBacklog {
		t.Fatalf("after set_state(backlog): state %s dispatch_id %q, want backlog and empty", got.State, got.DispatchID)
	}

	// 3. The owner releases a claimed ticket mid-flight: no dispatch and no
	// start left in the database.
	released := mkIn(t, alice, w.s.storyID, "released by its owner")
	tok = dispatchClaims(t, alice, released)
	alice.call(t, "amp_claim_task", map[string]interface{}{"task_id": float64(released), "dispatch_id": tok})
	if got := reload(released); got.StartedAt == nil {
		t.Fatalf("claimed ticket has no started_at in the database")
	}
	alice.call(t, "amp_assign_task", map[string]interface{}{"task_id": float64(released), "user": "none"})
	got := reload(released)
	if got.DispatchID != "" || got.StartedAt != nil || got.StartedBy != "" || got.State != domain.TaskStateBacklog {
		t.Fatalf("after release: state %s dispatch_id %q started_at %v started_by %q, want backlog with all cleared",
			got.State, got.DispatchID, got.StartedAt, got.StartedBy)
	}
}

// A returned ticket that is then completed isn't "returned" any more
// (AMP #2947): closed out directly with closeout_reason, moved to completed
// with amp_set_task_state, or completed by a person in the web UI, it loses
// returned_at/returned_reason in the database and leaves the returned list
// (the repository's and amp_list_tasks'). Re-read from the database, so a
// reloaded actor agrees.
func TestDispatchIntegrity_CompletionClearsReturned(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	ctx := context.Background()
	reload := func(id int) *domain.Task {
		t.Helper()
		task, err := w.s.repo.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("reload %d: %v", id, err)
		}
		return task
	}
	inRepoReturned := func(id int) bool {
		t.Helper()
		list, err := w.s.repo.Returned(ctx, w.s.projectID)
		if err != nil {
			t.Fatalf("Returned: %v", err)
		}
		for _, r := range list {
			if r.ID == id {
				return true
			}
		}
		return false
	}
	inListReturned := func(id int) bool {
		t.Helper()
		out := alice.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(w.s.projectID)})
		list, _ := out["returned"].([]interface{})
		for _, r := range list {
			if m, _ := r.(map[string]interface{}); m != nil && m["id"] == float64(id) {
				return true
			}
		}
		return false
	}
	// returned dispatches a ticket, returns it, and records a run on it, so
	// it can be closed out directly; it checks the ticket reads as returned.
	returned := func(name string) int {
		t.Helper()
		id := mkIn(t, alice, w.s.storyID, name)
		dispatchClaims(t, alice, id)
		alice.call(t, "amp_return_task", map[string]interface{}{"task_id": float64(id), "reason": "worker never started"})
		alice.call(t, "amp_report_run", map[string]interface{}{"task_id": float64(id), "outcome": "progress",
			"model": "m", "branch": "b", "commit": "abcdef1"})
		if got := reload(id); got.ReturnedAt == nil || got.ReturnedReason == "" {
			t.Fatalf("%d after return: returned_at %v reason %q, want both set", id, got.ReturnedAt, got.ReturnedReason)
		}
		if !inRepoReturned(id) || !inListReturned(id) {
			t.Fatalf("%d after return: missing from the returned list", id)
		}
		return id
	}
	cleared := func(id int, how string) {
		t.Helper()
		got := reload(id)
		if got.State != domain.TaskStateCompleted {
			t.Fatalf("%s: state %s, want completed", how, got.State)
		}
		if got.ReturnedAt != nil || got.ReturnedReason != "" {
			t.Fatalf("%s: returned_at %v reason %q in the database, want cleared", how, got.ReturnedAt, got.ReturnedReason)
		}
		if inRepoReturned(id) {
			t.Fatalf("%s: still in Repo.Returned", how)
		}
		if inListReturned(id) {
			t.Fatalf("%s: still in amp_list_tasks' returned list", how)
		}
	}

	conn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// 1. Closed out directly, the path the decision gives returned work.
	closed := returned("returned, then closed out")
	out := alice.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(closed),
		"closeout_reason": "its commit landed before the return"})
	if out["closeout"] != true {
		t.Fatalf("closeout = %v", out)
	}
	cleared(closed, "closeout")

	// 2. Moved to completed with amp_set_task_state. Off the web UI that
	// needs an in-progress ticket, and a dispatch clears the stamp itself,
	// so the stamp is one that survived in the database (written before
	// this fix): the state change must clear it there too.
	set := mkIn(t, alice, w.s.storyID, "stamped, then set completed")
	dispatchClaims(t, alice, set)
	if _, err := conn.Exec(ctx, `UPDATE tasks SET returned_at = NOW(), returned_reason = 'legacy' WHERE id = $1`, set); err != nil {
		t.Fatal(err)
	}
	alice.call(t, "amp_set_task_state", map[string]interface{}{"task_id": float64(set), "state": "completed",
		"reason": "done elsewhere"})
	cleared(set, "set_state(completed)")

	// 3. Completed by a person in the web UI.
	ui := returned("returned, then completed in the UI")
	ra.want(t, http.StatusOK, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", ui), map[string]any{"note": "done"})
	cleared(ui, "UI completion")

	// Belt and braces: a completed ticket whose stamp survived (data written
	// before this fix) still isn't listed.
	if _, err := conn.Exec(ctx, `UPDATE tasks SET returned_at = NOW(), returned_reason = 'legacy' WHERE id = $1`, closed); err != nil {
		t.Fatal(err)
	}
	if inRepoReturned(closed) {
		t.Fatalf("a completed ticket with a stale return stamp is listed by Repo.Returned")
	}
}
