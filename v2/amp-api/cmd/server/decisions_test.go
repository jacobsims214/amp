package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// Decision docs, Phase 2 (docs/specs/human-decision-docs.md §2, §3): an ask
// renders a judgment ticket's doc v1; the doc changes only in AMP, and the
// agent can draft but never record it.

// northStarAsk is the spec's #34 example ask, as MCP tool input.
func northStarAsk() map[string]any {
	return map[string]any{
		"question": "Which north-star metric do we commit to for the next 90 days?",
		"options": []any{
			map[string]any{"label": "A: Weekly active teams", "consequence": "Easy to measure; rewards seats over value"},
			map[string]any{"label": "B: Activation rate (40%, floor 30%)", "consequence": "Needs the onboarding event (#41)"},
			map[string]any{"label": "C: Net revenue retention", "consequence": "Lags 90 days; can't steer this quarter"},
		},
		"recommended": float64(1), "recommendation_reason": "B is the only one we can move and measure inside the quarter.",
		"evidence": []any{"artifact #1", "#3"},
	}
}

// decisionEvents counts decision.updated events for the project.
func decisionEvents(w *rbacWorld) func() []domain.Event {
	var mu sync.Mutex
	var got []domain.Event
	w.s.hub.AddSink(func(e domain.Event) {
		if e.Type == domain.EventDecisionUpdated && e.ProjectID == w.s.projectID {
			mu.Lock()
			got = append(got, e)
			mu.Unlock()
		}
	})
	return func() []domain.Event {
		mu.Lock()
		defer mu.Unlock()
		out := got
		got = nil
		return out
	}
}

// judgmentWithDoc plans the #34 → #41 chain as alice: bob's judgment ticket
// with an ask, and an agent ticket that waits on it.
func judgmentWithDoc(t *testing.T, w *rbacWorld) (j, dep int, created map[string]map[string]any) {
	t.Helper()
	res := w.s.as(w.alice).call(t, "amp_create_tasks", map[string]any{"project_id": float64(w.s.projectID), "tasks": []any{
		map[string]any{"ref": "j", "story_id": float64(w.s.storyID), "name": "Confirm: north-star targets", "performer": "human",
			"human_reason": "judgment", "owner": w.bob.Email, "description": "Pick the metric we steer by this quarter.",
			"acceptance_criteria": "- [ ] Metric chosen\n- [ ] Target and floor set", "ask": northStarAsk()},
		map[string]any{"ref": "dep", "story_id": float64(w.s.storyID), "name": "Instrument the onboarding event", "depends_on": []any{"j"},
			"description": "d", "acceptance_criteria": "a"},
	}})
	created = map[string]map[string]any{}
	for _, c := range res["created"].([]any) {
		m := c.(map[string]any)
		created[m["ref"].(string)] = m
	}
	return int(created["j"]["id"].(float64)), int(created["dep"]["id"].(float64)), created
}

func TestDecisions_AskRendersTheDoc(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	events := decisionEvents(w)
	alice := w.s.as(w.alice)
	j, dep, created := judgmentWithDoc(t, w)
	if created["j"]["decision_doc"] != true {
		t.Fatalf("created = %v, want decision_doc on j", created["j"])
	}
	if _, has := created["dep"]["decision_doc"]; has {
		t.Fatalf("an agent ticket got a doc: %v", created["dep"])
	}

	// The ask is not stored on the ticket; the doc's current version is.
	got := alice.call(t, "amp_get_task", map[string]any{"task_id": float64(j)})
	if _, has := got["task"].(map[string]any)["ask"]; has {
		t.Fatalf("the ticket stores its ask: %v", got["task"])
	}
	dec, ok := got["decision"].(map[string]any)
	if !ok {
		t.Fatalf("amp_get_task has no decision: %v", got)
	}
	ask := dec["ask"].(map[string]any)
	want := northStarAsk()
	if ask["question"] != want["question"] || ask["recommended"] != float64(1) || fmt.Sprint(ask["options"]) != fmt.Sprint(want["options"]) {
		t.Fatalf("decision ask = %v, want %v", ask, want)
	}
	if _, has := ask["recommendation_reason"]; has {
		t.Fatalf("the reason is metadata, not markdown: %v", ask)
	}
	if dec["author_kind"] != domain.AuthorAgent || dec["current_version"] != float64(1) {
		t.Fatalf("decision = %v", dec)
	}
	// The markdown is prose only: context, trade-offs, why. The question,
	// the options' structure and "decided means" aren't in it.
	text := dec["text"].(string)
	for _, s := range []string{"## Context", " › ", "- artifact #1", "- #3", fmt.Sprintf("Waiting on this: #%d", dep),
		"## Trade-offs", "**A: Weekly active teams.** Easy to measure", "## Why we recommend B: Activation rate (40%, floor 30%)",
		"B is the only one we can move and measure inside the quarter."} {
		if !strings.Contains(text, s) {
			t.Errorf("doc markdown lacks %q:\n%s", s, text)
		}
	}
	for _, s := range []string{"Which north-star metric", "Metric chosen", "Decided means"} {
		if strings.Contains(text, s) {
			t.Errorf("doc markdown has %q, which isn't its to hold:\n%s", s, text)
		}
	}

	// The artifact: role decision, kept, never expires; v1 by the agent.
	artID := int(dec["artifact_id"].(float64))
	a, err := w.s.repo.GetArtifact(context.Background(), artID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Role != domain.ArtifactRoleDecision || !a.Keep || a.ExpiresAt != nil || a.Kind != domain.ArtifactMarkdown || a.TaskID == nil || *a.TaskID != j {
		t.Fatalf("artifact = %+v", a)
	}
	v, err := w.s.repo.GetArtifactVersion(context.Background(), artID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if v.AuthorKind != domain.AuthorAgent || v.DecisionAsk == nil || v.DecisionAsk.Options[1].Label != "B: Activation rate (40%, floor 30%)" {
		t.Fatalf("v1 = %+v", v)
	}

	// GET /decision: the versions with their options. /decisions: own.
	full := ra.want(t, 200, "bob", http.MethodGet, fmt.Sprintf("/api/tasks/%d/decision", j), nil)
	vs := full["versions"].([]any)
	if len(vs) != 1 || vs[0].(map[string]any)["author_kind"] != "agent" || vs[0].(map[string]any)["ask"].(map[string]any)["question"] != want["question"] {
		t.Fatalf("versions = %v", full["versions"])
	}
	own := ra.want(t, 200, "bob", http.MethodGet, fmt.Sprintf("/api/tasks/%d/decisions", j), nil)["own"].(map[string]any)
	if int(own["artifact_id"].(float64)) != artID || own["versions"] != nil {
		t.Fatalf("own = %v", own)
	}
	ra.want(t, 404, "bob", http.MethodGet, fmt.Sprintf("/api/tasks/%d/decision", dep), nil)
	if code, _ := ra.do(t, "outsider", http.MethodGet, fmt.Sprintf("/api/tasks/%d/decision", j), nil); code < 400 {
		t.Fatalf("outsider read the decision doc: %d", code)
	}

	// One decision.updated for the one doc written.
	if ev := events(); len(ev) != 1 || fmt.Sprint(ev[0].Payload) != fmt.Sprint(map[string]any{"task_id": j, "version": 1}) {
		t.Fatalf("decision events = %v", ev)
	}
}

// amp_update_task(ask) creates a doc a judgment ticket lacks (one a person
// made in the web UI, which has no ask editor), and is refused once it has one.
func TestDecisions_UpdateAskCreatesAMissingDoc(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	events := decisionEvents(w)
	alice := w.s.as(w.alice)
	ui := ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/projects/%d/tasks", w.s.projectID), map[string]any{
		"story_id": w.s.storyID, "name": "Pick a vendor", "performer": "human", "human_reason": "judgment",
		"description": "Choose who hosts us", "acceptance_criteria": "Vendor chosen", "owner": w.bob.Email})
	j := int(ui["id"].(float64))
	ra.want(t, 404, "bob", http.MethodGet, fmt.Sprintf("/api/tasks/%d/decision", j), nil)

	out := alice.call(t, "amp_update_task", map[string]any{"task_id": float64(j), "ask": northStarAsk()})
	if out["decision_doc"] != true {
		t.Fatalf("update with ask = %v", out)
	}
	if d, ok := alice.call(t, "amp_get_task", map[string]any{"task_id": float64(j)})["decision"].(map[string]any); !ok || d["current_version"] != float64(1) {
		t.Fatalf("no doc after amp_update_task(ask): %v", d)
	}
	if n := len(events()); n != 1 {
		t.Fatalf("decision events = %d, want 1", n)
	}

	// Once it has a doc, its options change only in AMP.
	msg := alice.callExpectError(t, "amp_update_task", map[string]any{"task_id": float64(j), "ask": northStarAsk(), "name": "renamed"})
	wantErr(t, msg, "This ticket's options are in its decision doc; the owner edits them in AMP: ", fmt.Sprintf("board?task=%d", j))
	if tk := humanTask(t, alice, j); tk["name"] != "Pick a vendor" {
		t.Fatalf("a refused update changed the ticket: %v", tk["name"])
	}
	// The rest of the ticket stays editable.
	alice.call(t, "amp_update_task", map[string]any{"task_id": float64(j), "name": "Pick a hosting vendor"})

	// An ask goes only with a judgment ticket.
	agent := mkIn(t, alice, w.s.storyID, "Build it")
	wantErr(t, alice.callExpectError(t, "amp_update_task", map[string]any{"task_id": float64(agent), "ask": northStarAsk()}), "ask only goes with a judgment ticket")
	wantErr(t, alice.callExpectError(t, "amp_create_tasks", map[string]any{"project_id": float64(w.s.projectID), "tasks": []any{
		map[string]any{"ref": "x", "story_id": float64(w.s.storyID), "name": "Review it", "performer": "human", "human_reason": "review",
			"owner": w.bob.Email, "description": "d", "acceptance_criteria": "a", "ask": northStarAsk()}}}), `"x"`, "ask only goes with a judgment ticket")
	// An invalid ask is refused with the reason.
	bad := northStarAsk()
	bad["options"] = bad["options"].([]any)[:1]
	bad["recommended"] = float64(0)
	wantErr(t, alice.callExpectError(t, "amp_create_tasks", map[string]any{"project_id": float64(w.s.projectID), "tasks": []any{
		map[string]any{"ref": "q", "story_id": float64(w.s.storyID), "name": "Decide", "performer": "human", "human_reason": "judgment",
			"owner": w.bob.Email, "description": "d", "acceptance_criteria": "a", "ask": bad}}}), `"q"`, "2 to 4 options (got 1)")
}

// A doc that can't be written leaves the ticket created, with a warning; with
// no doc, setting the ask again is allowed and writes it.
func TestDecisions_CreationFailureWarns(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAsWith(t, w, func(h *api.RestHandler) {
		h.Service().DecisionDocFault = func(int) error { return errors.New("disk on fire") }
	})
	events := decisionEvents(w)
	code, out := ra.doVia(t, auth.ChannelToken, "alice", http.MethodPost, fmt.Sprintf("/api/projects/%d/tasks", w.s.projectID), map[string]any{
		"story_id": w.s.storyID, "name": "Confirm: north-star targets", "performer": "human", "human_reason": "judgment",
		"description": "Pick the metric", "acceptance_criteria": "Metric chosen", "owner": w.bob.Email, "ask": northStarAsk()})
	if code != 200 || !strings.Contains(fmt.Sprint(out["warnings"]), service.DecisionDocWarning) {
		t.Fatalf("create with a failing doc: %d %v", code, out)
	}
	j := int(out["id"].(float64))
	ra.want(t, 404, "bob", http.MethodGet, fmt.Sprintf("/api/tasks/%d/decision", j), nil)
	if n := len(events()); n != 0 {
		t.Fatalf("decision events for a doc never written: %d", n)
	}
	// The retry (this MCP server's service has no fault).
	alice := w.s.as(w.alice)
	alice.call(t, "amp_update_task", map[string]any{"task_id": float64(j), "ask": northStarAsk()})
	ra.want(t, 200, "bob", http.MethodGet, fmt.Sprintf("/api/tasks/%d/decision", j), nil)
}

// The §3 guards: the doc changes only in AMP, Record decision (not review)
// approves and keeps it, and only a person records the decision. Agent
// accounts get the same refusals.
func TestDecisions_Guards(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice, bob := w.s.as(w.alice), w.s.as(w.bob)
	j, _, _ := judgmentWithDoc(t, w)
	dec := alice.call(t, "amp_get_task", map[string]any{"task_id": float64(j)})["decision"].(map[string]any)
	art := dec["artifact_id"].(float64)
	link := fmt.Sprintf("board?task=%d", j)
	publish := "Decision docs change in AMP, not by publish: "
	personOnly := "Only a person records a decision, in AMP: "
	reviewKeep := "Decision docs are approved and kept by Record decision: "

	// Publishing to the doc, by anyone, any way.
	wantErr(t, alice.callExpectError(t, "amp_publish_artifact", map[string]any{"artifact_id": art, "content": "rewritten"}), publish, link)
	if code, out := ra.do(t, "bob", http.MethodPost, fmt.Sprintf("/api/artifacts/%d/versions", int(art)), map[string]any{"content": "rewritten"}); code != 403 ||
		!strings.Contains(fmt.Sprint(out), publish) {
		t.Fatalf("UI publish to the doc: %d %v", code, out)
	}
	// Review: an agent approving is told only a person records it; any other
	// review, and unkeeping, are Record decision's.
	wantErr(t, bob.callExpectError(t, "amp_review_artifact", map[string]any{"artifact_id": art, "approve": true}), personOnly, link)
	wantErr(t, bob.callExpectError(t, "amp_review_artifact", map[string]any{"artifact_id": art, "approve": false, "note": "tighter"}), reviewKeep, link)
	for _, path := range []string{"review", "keep"} {
		body := map[string]any{"approve": true}
		if path == "keep" {
			body = map[string]any{"keep": false}
		}
		if code, out := ra.do(t, "bob", http.MethodPost, fmt.Sprintf("/api/artifacts/%d/%s", int(art), path), body); code != 409 || !strings.Contains(fmt.Sprint(out), reviewKeep) {
			t.Fatalf("UI %s of the doc: %d %v", path, code, out)
		}
	}
	wantErr(t, bob.callExpectError(t, "amp_keep_artifact", map[string]any{"artifact_id": art, "keep": false}), reviewKeep, link)
	// Completing the ticket over MCP.
	wantErr(t, bob.callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(j), "note": "B"}), personOnly, link)

	// Agent accounts: the same strings, not the generic agent refusals.
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	agent := authz.Principal{UserID: w.bob.User.ID, Email: w.bob.User.Email, Agent: true, Channel: auth.ChannelMCP}
	ctx := context.Background()
	check := func(what string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), link) {
			t.Fatalf("agent account %s: %v, want %q", what, err, want)
		}
	}
	_, err := svc.PublishArtifact(ctx, agent, service.PublishArtifactRequest{ArtifactID: int(art), Content: []byte("x")})
	check("publish", err, publish)
	_, err = svc.ReviewArtifact(ctx, agent, int(art), 0, true, "")
	check("approve", err, personOnly)
	_, err = svc.KeepArtifact(ctx, agent, int(art), false)
	check("unkeep", err, reviewKeep)
	_, _, err = svc.CompleteHumanTask(ctx, agent, j, service.HumanDone{Note: "B"}, true)
	check("complete", err, personOnly)

	// Nothing changed.
	a, err := w.s.repo.GetArtifact(ctx, int(art))
	if err != nil {
		t.Fatal(err)
	}
	if a.CurrentVersion != 1 || !a.Keep || a.Status != domain.ArtifactInReview || a.ApprovedVersion != nil {
		t.Fatalf("a refused write changed the doc: %+v", a)
	}
	if st := stateOf(t, alice, j); st == "completed" {
		t.Fatal("a refused completion completed the ticket")
	}

	// Phase 2: the person marks it done in the UI with the card's note.
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", j),
		map[string]any{"note": "B: Activation rate (40%, floor 30%) — Go with B; keep trial at 14 days"})
	if c := commentBodies(t, alice, j); !strings.Contains(c, "Decision by bob: B: Activation rate (40%, floor 30%) — Go with B") {
		t.Fatalf("comments = %s", c)
	}
}

// Once a record is claimed (Phase 3's Record decision; set here directly),
// dependents read the doc's record: the option label as stored on the
// decided version, the text and the version, even before the ticket completes.
func TestDecisions_UpstreamPrefersTheRecord(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	j, dep, _ := judgmentWithDoc(t, w)
	if ups := upstreamOf(t, alice.call(t, "amp_get_task", map[string]any{"task_id": float64(dep)})); len(ups) != 0 {
		t.Fatalf("an undecided doc reached the dependent: %v", ups)
	}
	execSQL(t, `UPDATE decision_docs SET decided_version = 1, chosen_option = 1, decision_text = 'Go with B; keep trial at 14 days',
		decided_by = $2, recording_started_at = NOW() WHERE task_id = $1`, j, w.bob.User.ID)
	ups := upstreamOf(t, alice.call(t, "amp_get_task", map[string]any{"task_id": float64(dep)}))
	if len(ups) != 1 {
		t.Fatalf("upstream = %v", ups)
	}
	u := ups[0]
	if u["option"] != "B: Activation rate (40%, floor 30%)" || u["decision"] != "B: Activation rate (40%, floor 30%) — Go with B; keep trial at 14 days" ||
		u["version"] != float64(1) || u["decided_by_name"] != "bob" || !strings.Contains(fmt.Sprint(u["doc_url"]), "?v=1") {
		t.Fatalf("upstream decision = %v", u)
	}
	// The doc is claimed: setting the ask again says it's recorded.
	wantErr(t, alice.callExpectError(t, "amp_update_task", map[string]any{"task_id": float64(j), "ask": northStarAsk()}), "This decision is recorded and can't change: ")
}
