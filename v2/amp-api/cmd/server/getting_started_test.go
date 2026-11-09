package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
)

// The Get started checklist (#128) reads every step from server state: an
// agent reached AMP over MCP as you (or as an agent account you manage, or
// with a token of yours), you can see a project, you approved a plan, and a
// receipt is visible to you.
func TestGettingStarted_StepsFromServerState(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	ctx := context.Background()
	path := "/api/my-work/getting-started"
	step := func(out map[string]any, name string) map[string]any {
		t.Helper()
		m, ok := out[name].(map[string]any)
		if !ok {
			t.Fatalf("no %s step: %v", name, out)
		}
		return m
	}
	// The contact is written in the background: wait for it.
	agentDone := func(who string) map[string]any {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			a := step(ra.want(t, 200, who, "GET", path, nil), "agent")
			if a["done"] == true || time.Now().After(deadline) {
				return a
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Someone with no projects and no agent: nothing is done.
	out := ra.want(t, 200, "outsider", "GET", path, nil)
	for _, name := range []string{"agent", "project", "plan", "receipt"} {
		if step(out, name)["done"] != false {
			t.Fatalf("outsider's %s step is done: %v", name, out)
		}
	}
	// The receipt step is there even when nothing can be seen yet: it has
	// nothing to link to, and a receipt is never held out of sight.
	if r := step(out, "receipt"); r["project_id"] != nil || r["task_id"] != nil {
		t.Fatalf("an outsider's receipt step points somewhere: %v", r)
	}

	// alice can see the Acme project; no agent of hers has connected yet.
	out = ra.want(t, 200, "alice", "GET", path, nil)
	if step(out, "agent")["done"] != false {
		t.Fatalf("agent connected before any MCP call: %v", out)
	}
	if p := step(out, "project"); p["done"] != true || num(t, p, "project_id") == 0 {
		t.Fatalf("project step: %v", out)
	}
	if p := step(out, "plan"); p["done"] != false || num(t, p, "project_id") != w.s.projectID {
		t.Fatalf("plan step before any approval: %v", out)
	}
	if r := step(out, "receipt"); r["done"] != false {
		t.Fatalf("receipt step before any receipt: %v", out)
	}

	// Her agent calls a tool over MCP: the step is done, and stays done.
	w.as(w.alice).call(t, "amp_whoami", map[string]interface{}{})
	if a := agentDone("alice"); a["done"] != true || a["at"] == nil {
		t.Fatalf("agent step after an MCP call: %v", a)
	}
	if a := step(ra.want(t, 200, "alice", "GET", path, nil), "agent"); a["done"] != true {
		t.Fatalf("agent step didn't stay done: %v", a)
	}
	// It's hers: bob's agent hasn't connected.
	if out := ra.want(t, 200, "bob", "GET", path, nil); step(out, "agent")["done"] != false {
		t.Fatalf("alice's agent counted for bob: %v", out)
	}

	// An agent account bob manages connects over MCP: that counts for bob.
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	bot, err := w.s.repo.CreateAgentUser(ctx, "agent:bob-bot-"+sfx, "bob-bot-"+sfx+"@agents.rbac.test", "bob bot", &w.bob.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	w.as(&auth.Identity{Subject: bot.Subject, Email: bot.Email, Name: "bob bot", User: bot}).call(t, "amp_whoami", map[string]interface{}{})
	if a := agentDone("bob"); a["done"] != true {
		t.Fatalf("bob's agent account's contact: %v", a)
	}

	// An approval of hers in a project she can't see (an org she isn't in, say she
	// has since lost access) doesn't count, and doesn't leak that project.
	nobodys, err := w.s.repo.CreateOrg(ctx, "Nobody's "+sfx, "nobodys-"+sfx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	hTeam, err := w.s.repo.CreateTeam(ctx, nobodys.ID, "Hidden", nil)
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := w.s.repo.CreateProject(ctx, domain.CreateProjectRequest{
		Name: "hidden " + sfx, Code: "hid-" + sfx, OrgID: &nobodys.ID, TeamID: &hTeam.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	hEpic, err := w.s.repo.CreateEpic(ctx, domain.CreateEpicRequest{ProjectID: hidden.ID, Name: "e", Priority: "1"})
	if err != nil {
		t.Fatal(err)
	}
	hStory, err := w.s.repo.CreateStory(ctx, domain.CreateStoryRequest{ProjectID: hidden.ID, EpicID: hEpic.ID, Name: "s", Priority: "1"})
	if err != nil {
		t.Fatal(err)
	}
	hTask, err := w.s.repo.CreateTask(ctx, domain.CreateTaskRequest{ProjectID: hidden.ID, EpicID: hEpic.ID, StoryID: hStory.ID, Name: "hidden", Priority: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.s.repo.ApproveTask(ctx, hTask.ID, &w.alice.User.ID, nil); err != nil {
		t.Fatal(err)
	}
	if p := step(ra.want(t, 200, "alice", "GET", path, nil), "plan"); p["done"] != false || num(t, p, "project_id") == hidden.ID {
		t.Fatalf("an approval in a project alice can't see checked her plan step: %v", p)
	}

	// Nor does an approval of hers in a project she can see that is archived.
	archived, err := w.s.repo.CreateProject(ctx, domain.CreateProjectRequest{
		Name: "archived " + sfx, Code: "arc-" + sfx, OrgID: &w.org.ID, TeamID: &w.devs.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	aEpic, err := w.s.repo.CreateEpic(ctx, domain.CreateEpicRequest{ProjectID: archived.ID, Name: "e", Priority: "1"})
	if err != nil {
		t.Fatal(err)
	}
	aStory, err := w.s.repo.CreateStory(ctx, domain.CreateStoryRequest{ProjectID: archived.ID, EpicID: aEpic.ID, Name: "s", Priority: "1"})
	if err != nil {
		t.Fatal(err)
	}
	aTask, err := w.s.repo.CreateTask(ctx, domain.CreateTaskRequest{ProjectID: archived.ID, EpicID: aEpic.ID, StoryID: aStory.ID, Name: "old", Priority: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.s.repo.ApproveTask(ctx, aTask.ID, &w.alice.User.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.repo.ArchiveProject(ctx, archived.ID); err != nil {
		t.Fatal(err)
	}
	if p := step(ra.want(t, 200, "alice", "GET", path, nil), "plan"); p["done"] != false {
		t.Fatalf("an approval in an archived project checked alice's plan step: %v", p)
	}

	// Approving tickets is the plan step, for the person who approved.
	task, err := w.s.repo.CreateTask(ctx, domain.CreateTaskRequest{
		ProjectID: w.s.projectID, EpicID: w.s.epicID, StoryID: w.s.storyID, Name: "plan me", Priority: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.s.repo.ApproveTask(ctx, task.ID, &w.alice.User.ID, nil); err != nil {
		t.Fatal(err)
	}
	if p := step(ra.want(t, 200, "alice", "GET", path, nil), "plan"); p["done"] != true || num(t, p, "project_id") != w.s.projectID {
		t.Fatalf("plan step after her approval: %v", p)
	}
	// Someone else's approval doesn't check bob's step, and an outsider
	// learns nothing about the project.
	if p := step(ra.want(t, 200, "bob", "GET", path, nil), "plan"); p["done"] != false {
		t.Fatalf("alice's approval checked bob's plan step: %v", p)
	}
	out = ra.want(t, 200, "outsider", "GET", path, nil)
	for _, name := range []string{"project", "plan"} {
		if s := step(out, name); s["done"] != false || s["project_id"] != nil {
			t.Fatalf("outsider's %s step after an approval elsewhere: %v", name, out)
		}
	}

	// The receipt step is done once the user can see a receipt: a completing
	// run's final receipt in a project they can see, the same visibility the
	// ticket's own receipt view uses. A draft nobody can see yet doesn't count.
	setReceiptsMode(t, ra, w.s.projectID, "inform")
	alice := w.as(w.alice)
	ridTicket := mkIn(t, alice, w.s.storyID, "receipt me")
	ridDispatch := dispatchClaims(t, alice, ridTicket)
	alice.call(t, "amp_open_receipt", map[string]any{"task_id": float64(ridTicket), "dispatch_id": ridDispatch})
	if r := step(ra.want(t, 200, "alice", "GET", path, nil), "receipt"); r["done"] != false {
		t.Fatalf("an open draft checked alice's receipt step: %v", r)
	}
	if out := alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(ridTicket), "dispatch_id": ridDispatch,
		"checks": []any{passCheck("1")}}); out["state"] != "completed" {
		t.Fatalf("complete: %v", out)
	}
	r := step(ra.want(t, 200, "alice", "GET", path, nil), "receipt")
	if r["done"] != true || r["at"] == nil || num(t, r, "project_id") != w.s.projectID || num(t, r, "task_id") != ridTicket {
		t.Fatalf("receipt step after a receipt: %v", r)
	}
	// Anyone who can read that ticket sees the same receipt; someone who can't
	// see the project doesn't, and learns nothing about it.
	if r := step(ra.want(t, 200, "bob", "GET", path, nil), "receipt"); r["done"] != true || num(t, r, "task_id") != ridTicket {
		t.Fatalf("bob's receipt step, same project: %v", r)
	}
	if r := step(ra.want(t, 200, "outsider", "GET", path, nil), "receipt"); r["done"] != false || r["project_id"] != nil {
		t.Fatalf("a receipt in a project outsider can't see checked their step: %v", r)
	}
}
