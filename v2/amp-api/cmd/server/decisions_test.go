package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// Decision docs (docs/specs/human-decision-docs.md §2, §3): an ask renders a
// judgment ticket's doc v1; the doc changes only in AMP (a person's edit is a
// new version), the agent can draft but never record it, and Record decision
// freezes the version dependents receive.

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

	// Phase 3: even the person in the UI records it on the doc, not by Mark done.
	if code, out := ra.do(t, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", j),
		map[string]any{"note": "B: Activation rate (40%, floor 30%) — Go with B; keep trial at 14 days"}); code != 409 ||
		!strings.Contains(fmt.Sprint(out), "Record the decision on its doc: ") || !strings.Contains(fmt.Sprint(out), link) {
		t.Fatalf("UI Mark done on a doc ticket: %d %v", code, out)
	}
	if st := stateOf(t, alice, j); st == "completed" {
		t.Fatal("a refused Mark done completed the ticket")
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

// Phase 3: editing and recording.

// docPath is a ticket's decision route.
func docPath(j int, sub string) string { return fmt.Sprintf("/api/tasks/%d/decision%s", j, sub) }

// editedAsk is the #34 ask's stored options with option B's label tightened
// (the J1 edit), as PUT doc's ask.
func editedAsk(label string) map[string]any {
	a := northStarAsk()
	delete(a, "recommendation_reason")
	delete(a, "evidence")
	opts := slices.Clone(a["options"].([]any))
	opts[1] = map[string]any{"label": label, "consequence": "Needs the onboarding event (#41)"}
	a["options"] = opts
	return a
}

// humanRuns counts a ticket's runs by its person.
func humanRuns(t *testing.T, w *rbacWorld, id int) int {
	t.Helper()
	runs, err := w.s.repo.TaskRuns(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range runs {
		if r.AgentID == domain.HumanAgentID {
			n++
		}
	}
	return n
}

// PUT doc: the person's edit becomes one new version (markdown and options,
// author person); stale bases, other channels, bad options and the version
// cap are refused, and nothing changes.
func TestDecisions_EditDoc(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	events := decisionEvents(w)
	j, _, _ := judgmentWithDoc(t, w)
	events()
	v1 := ra.want(t, 200, "bob", http.MethodGet, docPath(j, ""), nil)
	text := v1["text"].(string) + "\nThe owner's note: trial stays at 14 days.\n"
	label := "B: Activation rate (45%, floor 30%)"

	out := ra.want(t, 200, "bob", http.MethodPut, docPath(j, "/doc"), map[string]any{"base_version": 1, "content": text, "ask": editedAsk(label)})
	if out["current_version"] != float64(2) || out["author_kind"] != domain.AuthorPerson || out["text"] != text {
		t.Fatalf("after edit: %v", out)
	}
	vs := out["versions"].([]any)
	if len(vs) != 2 || vs[0].(map[string]any)["author_kind"] != "agent" || vs[1].(map[string]any)["author_kind"] != "person" {
		t.Fatalf("versions = %v", vs)
	}
	if got := vs[1].(map[string]any)["ask"].(map[string]any)["options"].([]any)[1].(map[string]any)["label"]; got != label {
		t.Fatalf("v2's option B = %v", got)
	}
	if ev := events(); len(ev) != 1 || fmt.Sprint(ev[0].Payload) != fmt.Sprint(map[string]any{"task_id": j, "version": 2}) {
		t.Fatalf("decision events = %v", ev)
	}
	a, err := w.s.repo.GetArtifact(context.Background(), int(v1["artifact_id"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Keep || a.ExpiresAt != nil {
		t.Fatalf("an edit let the doc expire: %+v", a)
	}

	// A stale base: the other tab's draft is refused, v2 stays.
	if code, out := ra.do(t, "bob", http.MethodPut, docPath(j, "/doc"), map[string]any{"base_version": 1, "content": "other tab", "ask": editedAsk(label)}); code != 409 ||
		out["error"] != service.DecisionStaleRefusal {
		t.Fatalf("stale base: %d %v", code, out)
	}
	// Off the web UI: an API token, a non-UI REST client.
	for _, ch := range []string{auth.ChannelToken, auth.ChannelRESTOther} {
		if code, out := ra.doVia(t, ch, "bob", http.MethodPut, docPath(j, "/doc"), map[string]any{"base_version": 2, "content": "x", "ask": editedAsk(label)}); code != 403 ||
			!strings.Contains(fmt.Sprint(out), "web UI") {
			t.Fatalf("PUT doc over %s: %d %v", ch, code, out)
		}
	}
	// One option.
	bad := editedAsk(label)
	bad["options"] = bad["options"].([]any)[:1]
	bad["recommended"] = float64(0)
	if code, out := ra.do(t, "bob", http.MethodPut, docPath(j, "/doc"), map[string]any{"base_version": 2, "content": "x", "ask": bad}); code != 400 ||
		!strings.Contains(fmt.Sprint(out), "2 to 4 options (got 1)") {
		t.Fatalf("one option: %d %v", code, out)
	}
	if ev := events(); len(ev) != 0 {
		t.Fatalf("refused edits emitted %v", ev)
	}
	if d := ra.want(t, 200, "bob", http.MethodGet, docPath(j, ""), nil); d["current_version"] != float64(2) {
		t.Fatalf("a refused edit changed the doc: %v", d["current_version"])
	}

	// 100 versions: the cap.
	art := int(v1["artifact_id"].(float64))
	execSQL(t, `INSERT INTO artifact_versions (artifact_id, version, content, content_type, size_bytes, sha256, author_kind)
		SELECT $1, g, 'x', 'text/markdown', 1, '', 'person' FROM generate_series(3, 100) g`, art)
	execSQL(t, `UPDATE artifacts SET current_version = 100 WHERE id = $1`, art)
	if code, out := ra.do(t, "bob", http.MethodPut, docPath(j, "/doc"), map[string]any{"base_version": 100, "content": "x", "ask": editedAsk(label)}); code != 409 ||
		out["error"] != service.DecisionCapRefusal {
		t.Fatalf("101st version: %d %v", code, out)
	}
}

// Record decision at v2 approves and keeps v2, completes the ticket, posts
// the comment once, and gives the dependent the label as stored on v2.
func TestDecisions_RecordDecision(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	events := decisionEvents(w)
	alice := w.s.as(w.alice)
	j, dep, _ := judgmentWithDoc(t, w)
	link := fmt.Sprintf("board?task=%d", j)
	v1 := ra.want(t, 200, "bob", http.MethodGet, docPath(j, ""), nil)
	art := int(v1["artifact_id"].(float64))
	label := "B: Activation rate (45%, floor 30%)"
	ra.want(t, 200, "bob", http.MethodPut, docPath(j, "/doc"), map[string]any{"base_version": 1, "content": v1["text"], "ask": editedAsk(label)})
	events()
	record := func(version int, option any, text string) map[string]any {
		return map[string]any{"version": version, "chosen_option": option, "decision_text": text}
	}

	// Refused: a version that isn't current, no text, an option out of
	// range, any channel but the web UI, and Mark done.
	for _, c := range []struct {
		body map[string]any
		code int
		want string
	}{
		{record(1, 1, "Go with B"), 400, "isn't the doc's current version"},
		{record(2, 1, "  "), 400, "say what you decided"},
		{record(2, 3, "Go with B"), 400, "chosen_option 3 is not one of v2's 3 options"},
	} {
		if code, out := ra.do(t, "bob", http.MethodPost, docPath(j, "/record"), c.body); code != c.code || !strings.Contains(fmt.Sprint(out), c.want) {
			t.Fatalf("record %v: %d %v, want %d %q", c.body, code, out, c.code, c.want)
		}
	}
	if code, out := ra.doVia(t, auth.ChannelToken, "bob", http.MethodPost, docPath(j, "/record"), record(2, 1, "Go with B")); code != 403 ||
		!strings.Contains(fmt.Sprint(out), "Only a person records a decision, in AMP: ") {
		t.Fatalf("record over a token: %d %v", code, out)
	}
	if code, _ := ra.do(t, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", j), map[string]any{"note": "B"}); code != 409 {
		t.Fatalf("Mark done on a doc ticket: %d, want 409", code)
	}
	if ev := events(); len(ev) != 0 {
		t.Fatalf("refused records emitted %v", ev)
	}

	out := ra.want(t, 200, "bob", http.MethodPost, docPath(j, "/record"), record(2, 1, "Go with B; keep trial at 14 days"))
	if out["decided_version"] != float64(2) || out["chosen_option"] != float64(1) || out["decided_at"] == nil || out["decided_by_name"] != "bob" {
		t.Fatalf("record = %v", out)
	}
	a, err := w.s.repo.GetArtifact(context.Background(), art)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != domain.ArtifactApproved || a.ApprovedVersion == nil || *a.ApprovedVersion != 2 || !a.Keep || a.ExpiresAt != nil {
		t.Fatalf("doc after record: %+v", a)
	}
	if st := stateOf(t, alice, j); st != "completed" {
		t.Fatalf("ticket after record: %s", st)
	}
	c := commentBodies(t, alice, j)
	if !strings.Contains(c, "Decision by bob: "+label+" — Go with B; keep trial at 14 days") || !strings.Contains(c, fmt.Sprintf("/artifacts/%d?v=2", art)) {
		t.Fatalf("comments = %s", c)
	}
	if n := strings.Count(c, "Decision by bob"); n != 1 {
		t.Fatalf("%d decision comments: %s", n, c)
	}
	if n := humanRuns(t, w, j); n != 1 {
		t.Fatalf("%d human runs, want 1", n)
	}
	ups := upstreamOf(t, alice.call(t, "amp_get_task", map[string]any{"task_id": float64(dep)}))
	if len(ups) != 1 || ups[0]["option"] != label || ups[0]["decision"] != label+" — Go with B; keep trial at 14 days" || ups[0]["version"] != float64(2) {
		t.Fatalf("dependent's upstream = %v", ups)
	}
	if strip := ra.want(t, 200, "bob", http.MethodGet, fmt.Sprintf("/api/tasks/%d/decisions", dep), nil)["upstream"].([]any); len(strip) != 1 ||
		strip[0].(map[string]any)["option"] != label {
		t.Fatalf("dependent's drawer = %v", strip)
	}
	if ev := events(); len(ev) != 1 || fmt.Sprint(ev[0].Payload) != fmt.Sprint(map[string]any{"task_id": j, "version": 2}) {
		t.Fatalf("decision events = %v", ev)
	}

	// Recorded: read-only, no second record.
	if code, out := ra.do(t, "bob", http.MethodPost, docPath(j, "/record"), record(2, 0, "A after all")); code != 409 ||
		!strings.Contains(fmt.Sprint(out), "This decision is recorded and can't change: ") || !strings.Contains(fmt.Sprint(out), link) {
		t.Fatalf("second record: %d %v", code, out)
	}
	if code, out := ra.do(t, "bob", http.MethodPut, docPath(j, "/doc"), map[string]any{"base_version": 2, "content": "x", "ask": editedAsk(label)}); code != 409 ||
		!strings.Contains(fmt.Sprint(out), "This decision is recorded and can't change: ") {
		t.Fatalf("edit after record: %d %v", code, out)
	}
	if n := humanRuns(t, w, j); n != 1 {
		t.Fatalf("%d human runs after the repeat, want 1", n)
	}
	if ev := events(); len(ev) != 0 {
		t.Fatalf("refused writes emitted %v", ev)
	}
}

// Something else: no option, the text alone.
func TestDecisions_RecordSomethingElse(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	j, dep, _ := judgmentWithDoc(t, w)
	ra.want(t, 200, "bob", http.MethodPost, docPath(j, "/record"), map[string]any{"version": 1, "chosen_option": nil, "decision_text": "Hold until Q3"})
	ups := upstreamOf(t, alice.call(t, "amp_get_task", map[string]any{"task_id": float64(dep)}))
	if len(ups) != 1 || ups[0]["decision"] != "Hold until Q3" || ups[0]["option"] != nil {
		t.Fatalf("upstream = %v", ups)
	}
	if c := commentBodies(t, alice, j); !strings.Contains(c, "Decision by bob: Hold until Q3\n") {
		t.Fatalf("comments = %s", c)
	}
}

// Two records at once: one 200 and one 409. An edit racing a record:
// exactly one of them succeeds.
func TestDecisions_RecordRaces(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	race := func(a, b func() int) []int {
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i, f := range []func() int{a, b} {
			wg.Add(1)
			go func() { defer wg.Done(); codes[i] = f() }()
		}
		wg.Wait()
		slices.Sort(codes)
		return codes
	}
	j, _, _ := judgmentWithDoc(t, w)
	rec := func(j int, text string) func() int {
		return func() int {
			code, _ := ra.do(t, "bob", http.MethodPost, docPath(j, "/record"), map[string]any{"version": 1, "chosen_option": 1, "decision_text": text})
			return code
		}
	}
	if codes := race(rec(j, "first"), rec(j, "second")); codes[0] != 200 || codes[1] != 409 {
		t.Fatalf("concurrent records: %v, want [200 409]", codes)
	}
	if n := humanRuns(t, w, j); n != 1 {
		t.Fatalf("%d human runs, want 1", n)
	}
	for i := 0; i < 3; i++ {
		k, _, _ := judgmentWithDoc(t, w)
		edit := func() int {
			code, _ := ra.do(t, "bob", http.MethodPut, docPath(k, "/doc"), map[string]any{"base_version": 1, "content": "edited", "ask": editedAsk("B: tighter")})
			return code
		}
		codes := race(edit, rec(k, "B"))
		if ok := slices.Index(codes, 200); ok < 0 || slices.Contains(codes[ok+1:], 200) || slices.Contains(codes[:ok], 200) {
			t.Fatalf("edit racing a record: %v, want exactly one 200", codes)
		}
	}
}

// A record that stops after its claim: the dependent already reads the
// claim, a retry within the minute is "being recorded", and once the claim
// is stale a retry finishes with the stored values: one run, one comment.
func TestDecisions_RecordRetry(t *testing.T) {
	w := newRBACWorld(t)
	var calls atomic.Int32
	ra := newRestAsWith(t, w, func(h *api.RestHandler) {
		h.Service().DecisionRunFault = func(int) error {
			if calls.Add(1) == 1 {
				return errors.New("disk on fire")
			}
			return nil
		}
	})
	events := decisionEvents(w)
	alice := w.s.as(w.alice)
	j, dep, _ := judgmentWithDoc(t, w)
	events()
	body := map[string]any{"version": 1, "chosen_option": 1, "decision_text": "Go with B; keep trial at 14 days"}
	if code, out := ra.do(t, "bob", http.MethodPost, docPath(j, "/record"), body); code != 500 || !strings.Contains(fmt.Sprint(out), "Recording didn't finish") &&
		!strings.Contains(fmt.Sprint(out), "recording didn't finish") {
		t.Fatalf("record with the run failing: %d %v", code, out)
	}
	// The claim stands, so open drawers hear of it (and show the retry).
	if ev := events(); len(ev) != 1 || fmt.Sprint(ev[0].Payload) != fmt.Sprint(map[string]any{"task_id": j, "version": 1}) {
		t.Fatalf("a record that claimed and stopped emitted %v, want one decision.updated", ev)
	}
	ups := upstreamOf(t, alice.call(t, "amp_get_task", map[string]any{"task_id": float64(dep)}))
	if len(ups) != 1 || ups[0]["decision"] != "B: Activation rate (40%, floor 30%) — Go with B; keep trial at 14 days" {
		t.Fatalf("the dependent doesn't see the claim: %v", ups)
	}
	if n := humanRuns(t, w, j); n != 0 {
		t.Fatalf("%d runs after the failed step", n)
	}
	if code, out := ra.do(t, "bob", http.MethodPost, docPath(j, "/record"), body); code != 409 || !strings.Contains(fmt.Sprint(out), "already being recorded") {
		t.Fatalf("retry within the minute: %d %v", code, out)
	}
	execSQL(t, `UPDATE decision_docs SET recording_started_at = NOW() - INTERVAL '2 minutes' WHERE task_id = $1`, j)
	// The retry finishes with the stored values, whatever it sends, and is
	// attributed to the decider (bob), not to whoever retries (an admin
	// here).
	ra.want(t, 200, "root", http.MethodPost, docPath(j, "/record"), map[string]any{"version": 1, "chosen_option": 0, "decision_text": "A instead"})
	c := commentBodies(t, alice, j)
	if strings.Count(c, "Decision by bob") != 1 || !strings.Contains(c, "B: Activation rate (40%, floor 30%) — Go with B; keep trial at 14 days") ||
		strings.Contains(c, "A instead") || strings.Contains(c, "Decision by root") {
		t.Fatalf("comments = %s", c)
	}
	if n := humanRuns(t, w, j); n != 1 {
		t.Fatalf("%d human runs, want 1", n)
	}
	runs, err := w.s.repo.TaskRuns(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	if u := runs[len(runs)-1].UserID; u == nil || *u != w.bob.User.ID {
		t.Fatalf("the run is attributed to %v, want bob (%d)", u, w.bob.User.ID)
	}
	art, err := w.s.repo.GetArtifact(context.Background(), int(ra.want(t, 200, "bob", http.MethodGet, docPath(j, ""), nil)["artifact_id"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	if art.ReviewedBy == nil || *art.ReviewedBy != w.bob.User.ID {
		t.Fatalf("reviewed_by = %v, want bob", art.ReviewedBy)
	}
	d := ra.want(t, 200, "bob", http.MethodGet, docPath(j, ""), nil)
	if d["decided_at"] == nil || d["chosen_option"] != float64(1) {
		t.Fatalf("doc after the retry: %v", d)
	}
	if st := stateOf(t, alice, j); st != "completed" {
		t.Fatalf("ticket after the retry: %s", st)
	}
	if n := len(events()); n != 1 {
		t.Fatalf("decision events after the retry = %d, want 1", n)
	}
}

// Once the run is written (decision_docs.run_id), no later attempt writes a
// second run or comment: here a record stops after its run, and the retry
// only finishes it.
func TestDecisions_RecordRunOnce(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	j, _, _ := judgmentWithDoc(t, w)
	ra.want(t, 200, "bob", http.MethodPost, docPath(j, "/record"), map[string]any{"version": 1, "chosen_option": 1, "decision_text": "Go with B"})
	// As if it had stopped between the run and the last step, a minute ago.
	execSQL(t, `UPDATE decision_docs SET decided_at = NULL, recording_started_at = NOW() - INTERVAL '2 minutes' WHERE task_id = $1`, j)
	ra.want(t, 200, "bob", http.MethodPost, docPath(j, "/record"), map[string]any{"version": 1, "chosen_option": 1, "decision_text": "Go with B"})
	if n := humanRuns(t, w, j); n != 1 {
		t.Fatalf("%d human runs after the retry, want 1", n)
	}
	if c := commentBodies(t, alice, j); strings.Count(c, "Decision by bob") != 1 {
		t.Fatalf("decision comments after the retry: %s", c)
	}
	if d := ra.want(t, 200, "bob", http.MethodGet, docPath(j, ""), nil); d["decided_at"] == nil {
		t.Fatalf("the retry didn't finish the record: %v", d)
	}
}

// A ticket that can't be completed isn't recorded: one still waiting on its
// dependencies is refused before any claim, and a completion the server
// refuses releases the claim, so the doc stays editable and dependents see
// nothing.
func TestDecisions_RecordNeedsACompletableTicket(t *testing.T) {
	w := newRBACWorld(t)
	var refuse atomic.Bool
	ra := newRestAsWith(t, w, func(h *api.RestHandler) {
		h.Service().DecisionCompleteFault = func(int) error {
			if refuse.Load() {
				return &authz.Error{Status: 409, Msg: "ticket can't be completed now"}
			}
			return nil
		}
	})
	events := decisionEvents(w)
	alice := w.s.as(w.alice)
	res := alice.call(t, "amp_create_tasks", map[string]any{"project_id": float64(w.s.projectID), "tasks": []any{
		map[string]any{"ref": "first", "story_id": float64(w.s.storyID), "name": "Gather the numbers", "description": "d", "acceptance_criteria": "a"},
		map[string]any{"ref": "j", "story_id": float64(w.s.storyID), "name": "Confirm: north-star targets", "performer": "human",
			"human_reason": "judgment", "owner": w.bob.Email, "description": "Pick the metric", "acceptance_criteria": "Metric chosen",
			"ask": northStarAsk(), "depends_on": []any{"first"}},
	}})
	ids := map[string]int{}
	for _, c := range res["created"].([]any) {
		m := c.(map[string]any)
		ids[m["ref"].(string)] = int(m["id"].(float64))
	}
	j := ids["j"]
	events()
	body := map[string]any{"version": 1, "chosen_option": 1, "decision_text": "Go with B"}
	if code, out := ra.do(t, "bob", http.MethodPost, docPath(j, "/record"), body); code != 409 ||
		!strings.Contains(fmt.Sprint(out), fmt.Sprintf("is waiting on #%d", ids["first"])) {
		t.Fatalf("record on a ticket waiting on its dependencies: %d %v", code, out)
	}
	noClaim := func(when string) {
		t.Helper()
		d := ra.want(t, 200, "bob", http.MethodGet, docPath(j, ""), nil)
		if d["decided_version"] != nil || d["recording_started_at"] != nil || d["decision_text"] != nil {
			t.Fatalf("%s: a claim was left: %v", when, d)
		}
		if n := len(events()); n != 0 {
			t.Fatalf("%s: %d decision events", when, n)
		}
	}
	noClaim("waiting on dependencies")

	// Its dependency done, but the completion refused: the claim is released.
	ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", ids["first"]), nil)
	refuse.Store(true)
	if code, out := ra.do(t, "bob", http.MethodPost, docPath(j, "/record"), body); code != 409 || !strings.Contains(fmt.Sprint(out), "can't be completed now") {
		t.Fatalf("record with the completion refused: %d %v", code, out)
	}
	noClaim("completion refused")
	if st := stateOf(t, alice, j); st == "completed" {
		t.Fatal("a refused record completed the ticket")
	}
	// The doc is editable, and the record works once the ticket can finish.
	ra.want(t, 200, "bob", http.MethodPut, docPath(j, "/doc"), map[string]any{"base_version": 1, "content": "edited", "ask": editedAsk("B: tighter")})
	refuse.Store(false)
	ra.want(t, 200, "bob", http.MethodPost, docPath(j, "/record"), map[string]any{"version": 2, "chosen_option": 1, "decision_text": "Go with B"})
}
