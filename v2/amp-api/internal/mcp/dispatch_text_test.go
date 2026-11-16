package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
)

// The text an agent reads about dispatching must never suggest AMP runs a
// worker itself. It used to give 'amp-worker' (a name no agent has) as the
// example agent, open amp_dispatch_task with "Start work on a ticket", and
// answer a dispatch with "dispatched to amp-worker": agents without the AMP
// plugin took that for a worker AMP had started and ended their turn, and the
// ticket sat "not started" until the watchdog returned it.

// TestDispatchNext_SaysAMPRunsNoAgents: the dispatch result says nothing
// works the ticket until the caller launches a worker. Its opening sentence
// is unchanged: scripts/lib/harness_overrides.yaml finds the manager prompt's
// quote of it by that sentence.
func TestDispatchNext_SaysAMPRunsNoAgents(t *testing.T) {
	task := &domain.Task{ID: 42, ProjectID: 3, DispatchID: "d_1"}
	at := time.Date(2026, 11, 11, 12, 30, 0, 0, time.UTC)
	got := dispatchNext(task, "amp-worker", &at, 30)

	if !strings.HasPrefix(got, "Ticket #42 is dispatched to amp-worker but no worker has started it yet. ") {
		t.Errorf("next's opening sentence changed (the harness build matches on it):\n%s", got)
	}
	for _, want := range []string{
		"AMP runs no agents: nothing works on it until you launch one.",
		"Start the worker now, in this same turn",
		"worker_prompt",
		"Agent/Task in Claude Code",
		`amp_claim_task(task_id=42, dispatch_id="d_1")`,
		"2026-11-11T12:30:00Z (30 minutes)",
		`amp_set_task_state(task_id=42, state="backlog"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("next lacks %q:\n%s", want, got)
		}
	}
}

// TestWorkerPrompt_DoesNotAssumeThePlugin: the worker prompt works for a
// worker without the AMP plugin's amp-execution skill, because it spells out
// the claim, complete and block calls itself.
func TestWorkerPrompt_DoesNotAssumeThePlugin(t *testing.T) {
	task := &domain.Task{ID: 5, ProjectID: 2, DispatchID: "d_9"}
	got := workerPrompt(task, "amp:amp-worker-backend")

	if !strings.HasPrefix(got, "Execute approved ticket #5 in project 2 (dispatched to amp:amp-worker-backend).") {
		t.Errorf("worker_prompt's opening sentence changed (the harness build matches on it):\n%s", got)
	}
	if strings.Contains(got, "follow the amp-execution protocol") {
		t.Errorf("worker_prompt still requires the amp-execution skill:\n%s", got)
	}
	for _, want := range []string{
		`amp_claim_task(task_id=5, dispatch_id="d_9")`,
		"amp-execution skill, if you have it",
		`amp_get_task(task_id=5, include=["comments","history","runs"])`,
		`amp_complete_task(task_id=5, dispatch_id="d_9"`,
		`amp_block_task(task_id=5, dispatch_id="d_9"`,
		"Don't end your run without one of those two calls.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("worker_prompt lacks %q:\n%s", want, got)
		}
	}
}

// TestInstructions_SayAMPRunsNoAgents: an agent learns on connect that every
// worker is one it launches, and that agent names are labels.
func TestInstructions_SayAMPRunsNoAgents(t *testing.T) {
	for _, want := range []string{
		"AMP's server runs no agents: no worker exists until you launch one",
		"agent_id and assigned_to are only labels for the agent you launch",
	} {
		if !strings.Contains(Instructions, want) {
			t.Errorf("Instructions lack %q", want)
		}
	}
}

// TestToolText_NoPhantomWorker reads every registered tool the way a client
// does (its JSON: description, title, every parameter's description) and
// checks the dispatch wording end to end.
func TestToolText_NoPhantomWorker(t *testing.T) {
	srv := server.NewMCPServer("t", "1")
	NewServer(nil, nil, hub.New(), nil).Register(srv)
	tools := srv.ListTools()
	if len(tools) == 0 {
		t.Fatal("no tools registered")
	}

	for name, st := range tools {
		raw, err := json.Marshal(st.Tool)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		for _, text := range descriptions(v) {
			// A bare 'amp-worker' names no agent anyone can launch.
			if strings.Contains(text, "'amp-worker'") {
				t.Errorf("%s offers the phantom agent 'amp-worker': %q", name, text)
			}
		}
	}

	dispatch := toolJSON(t, tools, "amp_dispatch_task")
	desc, _ := dispatch["description"].(string)
	if strings.HasPrefix(desc, "Start work on a ticket") {
		t.Errorf("amp_dispatch_task still opens as if AMP starts the work: %q", desc)
	}
	for _, want := range []string{"AMP runs no agents, so this starts nothing by itself", "you launch the worker in the same turn"} {
		if !strings.Contains(desc, want) {
			t.Errorf("amp_dispatch_task description lacks %q:\n%s", want, desc)
		}
	}
	if strings.Contains(desc, "hand a ticket to a worker agent") {
		t.Errorf("amp_dispatch_task's Use for keywords still say AMP hands the ticket over:\n%s", desc)
	}

	// Every field that names the agent says it is only a label.
	for _, f := range []struct{ tool, path string }{
		{"amp_dispatch_task", "agent_id"},
		{"amp_create_task", "assigned_to"},
		{"amp_create_tasks", "tasks.assigned_to"},
	} {
		d := propDescription(t, toolJSON(t, tools, f.tool), f.path)
		if !strings.Contains(d, "Only a label: AMP's server runs no agents.") {
			t.Errorf("%s %s doesn't say it is only a label: %q", f.tool, f.path, d)
		}
	}
	// The label help doesn't cost assigned_to its REQUIRED marker (tool()
	// derives the schema's required list from it).
	if d := propDescription(t, toolJSON(t, tools, "amp_create_task"), "assigned_to"); !strings.HasPrefix(d, "REQUIRED. ") {
		t.Errorf("amp_create_task assigned_to lost its REQUIRED marker: %q", d)
	}
}

// descriptions collects every "description" and "title" string in a tool's JSON.
func descriptions(v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if s, ok := val.(string); ok && (k == "description" || k == "title") {
				out = append(out, s)
				continue
			}
			out = append(out, descriptions(val)...)
		}
	case []any:
		for _, val := range x {
			out = append(out, descriptions(val)...)
		}
	}
	return out
}

func toolJSON(t *testing.T, tools map[string]*server.ServerTool, name string) map[string]any {
	t.Helper()
	st, ok := tools[name]
	if !ok {
		t.Fatalf("tool %s not registered", name)
	}
	raw, err := json.Marshal(st.Tool)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: unmarshal: %v", name, err)
	}
	return m
}

// propDescription reads a parameter's description from a tool's JSON;
// "tasks.assigned_to" is assigned_to inside the tasks array's items.
func propDescription(t *testing.T, tool map[string]any, path string) string {
	t.Helper()
	node, _ := tool["inputSchema"].(map[string]any)
	parts := strings.Split(path, ".")
	for i, p := range parts {
		props, _ := node["properties"].(map[string]any)
		next, ok := props[p].(map[string]any)
		if !ok {
			t.Fatalf("%v: no parameter %q", tool["name"], path)
		}
		if i < len(parts)-1 {
			next, _ = next["items"].(map[string]any)
		}
		node = next
	}
	d, _ := node["description"].(string)
	return d
}
