package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"testing"

	"github.com/simstech/amp-api/internal/service"
)

// GET /api/projects/{id}/waiting-on-people (AMP #2496): Mission's Waiting on
// people from the server. Each person's counts and items are exactly what
// WaitingOn says for them, and the same items MCP's waiting_on_people lists.

// waitKey is one item as "kind:id,id".
func waitKey(kind string, ids []int) string {
	return fmt.Sprintf("%s:%v", kind, ids)
}

func anyInts(v any) []int {
	out := []int{}
	if v == nil {
		return out
	}
	for _, x := range v.([]any) {
		out = append(out, int(x.(float64)))
	}
	return out
}

func TestWaitingOnPeople_Endpoint(t *testing.T) {
	f := newWaitFixture(t)
	ctx := context.Background()
	w := f.w
	pid := w.s.projectID
	path := fmt.Sprintf("/api/projects/%d/waiting-on-people", pid)

	// Any member may read it; someone outside the project may not.
	res := f.ra.want(t, 200, "bob", http.MethodGet, path, nil)
	f.ra.want(t, 200, "carol", http.MethodGet, path, nil)
	if code, _ := f.ra.do(t, "outsider", http.MethodGet, path, nil); code != 403 && code != 404 {
		t.Fatalf("outsider got %d, want 403 or 404", code)
	}
	if code, _ := f.ra.do(t, "bob", http.MethodPost, path, nil); code != 405 {
		t.Fatalf("POST got %d, want 405", code)
	}
	if int(res["project_id"].(float64)) != pid {
		t.Fatalf("project_id = %v", res["project_id"])
	}

	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	people := map[int]map[string]any{}
	var order []int
	for _, x := range res["people"].([]any) {
		p := x.(map[string]any)
		uid := int(p["user_id"].(float64))
		people[uid] = p
		order = append(order, uid)
	}
	if !sort.IntsAreSorted(order) {
		t.Fatalf("people not in user id order: %v", order)
	}
	if people[w.alice.User.ID] == nil || people[w.bob.User.ID] == nil {
		t.Fatalf("want alice and bob, got %v", order)
	}

	// Counts and items match WaitingOn per person, in its order.
	endpoint := map[int][]string{}
	for uid, p := range people {
		want, err := svc.WaitingOn(ctx, uid, []int{pid})
		if err != nil {
			t.Fatal(err)
		}
		c := p["counts"].(map[string]any)
		for k, n := range map[string]int{"waiting": want.Counts.Waiting, "approve": want.Counts.Approve, "answer": want.Counts.Answer,
			"merge": want.Counts.Merge, "do": want.Counts.Do, "coming_up": want.Counts.ComingUp} {
			if int(c[k].(float64)) != n {
				t.Fatalf("user %d counts.%s = %v, WaitingOn says %d", uid, k, c[k], n)
			}
		}
		items := p["items"].([]any)
		if len(items) != len(want.Items) {
			t.Fatalf("user %d: %d items, WaitingOn has %d", uid, len(items), len(want.Items))
		}
		for i, x := range items {
			it := x.(map[string]any)
			wi := want.Items[i]
			if it["kind"] != wi.Kind || !slices.Equal(anyInts(it["task_ids"]), wi.TaskIDs) {
				t.Fatalf("user %d item %d = %v, WaitingOn %s %v", uid, i, it, wi.Kind, wi.TaskIDs)
			}
			if wi.Kind == service.WaitApprove && (int(it["epic_id"].(float64)) != wi.EpicID || it["epic_name"] != wi.EpicName) {
				t.Fatalf("user %d approve item = %v, want epic %d %q", uid, it, wi.EpicID, wi.EpicName)
			}
			// Nothing beyond counts and ids: no question text or PR links.
			for _, k := range []string{"question", "pr_url", "branch", "name"} {
				if _, ok := it[k]; ok {
					t.Fatalf("user %d item carries %s: %v", uid, k, it)
				}
			}
			endpoint[uid] = append(endpoint[uid], waitKey(wi.Kind, wi.TaskIDs))
		}
		coming := []int{}
		for _, cu := range want.ComingUp {
			coming = append(coming, cu.TaskID)
		}
		if got := anyInts(p["coming_up"]); !slices.Equal(got, coming) {
			t.Fatalf("user %d coming_up = %v, WaitingOn %v", uid, got, coming)
		}
	}

	// The fixture's shape, as a spot check.
	alice := people[w.alice.User.ID]
	ac := alice["counts"].(map[string]any)
	if ac["approve"].(float64) != 2 || ac["answer"].(float64) != 2 || ac["merge"].(float64) != 1 || ac["do"].(float64) != 2 || ac["coming_up"].(float64) != 2 {
		t.Fatalf("alice counts = %v", ac)
	}
	if got := anyInts(alice["coming_up"]); !slices.Contains(got, f.comingDeps) || !slices.Contains(got, f.comingApproval) {
		t.Fatalf("alice coming_up = %v", got)
	}
	if bc := people[w.bob.User.ID]["counts"].(map[string]any); bc["do"].(float64) != 1 || bc["waiting"].(float64) != 1 {
		t.Fatalf("bob counts = %v", bc)
	}
	for _, x := range people[w.bob.User.ID]["items"].([]any) {
		if !slices.Equal(anyInts(x.(map[string]any)["task_ids"]), []int{f.bobsHuman}) {
			t.Fatalf("bob items = %v", people[w.bob.User.ID]["items"])
		}
	}
	// Another project's question isn't here.
	for _, p := range people {
		for _, x := range p["items"].([]any) {
			if slices.Contains(anyInts(x.(map[string]any)["task_ids"]), f.q3) {
				t.Fatalf("project 2's question #%d listed in project %d", f.q3, pid)
			}
		}
	}

	// MCP's waiting_on_people (everyone's view) lists the same items per person.
	board := w.s.as(w.alice).call(t, "amp_list_tasks", map[string]any{"project_id": float64(pid), "owner": "any"})
	mcp := map[int][]string{}
	for _, x := range board["waiting_on_people"].([]any) {
		m := x.(map[string]any)
		if m["owner_user_id"] == nil {
			continue // unowned human tickets: "nobody", not a person
		}
		uid := int(m["owner_user_id"].(float64))
		mcp[uid] = append(mcp[uid], waitKey(m["kind"].(string), anyInts(m["task_ids"])))
	}
	for uid := range people {
		a, b := slices.Clone(endpoint[uid]), slices.Clone(mcp[uid])
		slices.Sort(a)
		slices.Sort(b)
		if !slices.Equal(a, b) {
			t.Fatalf("user %d: endpoint %v, MCP %v", uid, a, b)
		}
	}
	for uid := range mcp {
		if people[uid] == nil {
			t.Fatalf("MCP lists user %d, the endpoint doesn't", uid)
		}
	}
}
