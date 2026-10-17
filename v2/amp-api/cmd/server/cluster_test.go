package main

// Several amp-api replicas against one database (internal/cluster): each
// project runs on one replica, the others pass its requests there, live
// events reach every replica, and a replica that dies or loses the
// database hands its projects over.
//
// Set TEST_CLUSTER=1 to also run every MCP test (setupMCP) through a
// replica that forwards to another.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	protoactor "github.com/asynkron/protoactor-go/actor"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/cluster"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/mcp"
	"github.com/simstech/amp-api/internal/repository"
)

// Short timings so takeovers happen within a test.
var fastCluster = cluster.Options{Heartbeat: 100 * time.Millisecond, TTL: 1500 * time.Millisecond, FenceAfter: 800 * time.Millisecond}

// replica is one amp-api replica: its own actors, hub, and MCP server.
type replica struct {
	name   string
	node   *cluster.Node
	reg    *actor.Registry
	hub    *hub.Hub
	mcp    *mcpStack
	srv    *httptest.Server
	stopHB context.CancelFunc // stops heartbeats (and the relay)
	system *protoactor.ActorSystem
	down   sync.Once
}

func (r *replica) shutdownActors() { r.down.Do(r.system.Shutdown) }

// startReplica joins a replica to the group, relaying live events through
// h to and from the others.
func startReplica(t *testing.T, repo *repository.Repo, h *hub.Hub, name string, opts cluster.Options) *replica {
	return joinReplica(t, repo, h, name, opts, true)
}

// joinReplica is startReplica; without relay, replicas share h directly.
func joinReplica(t *testing.T, repo *repository.Repo, h *hub.Hub, name string, opts cluster.Options, relay bool) *replica {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	opts.ID = fmt.Sprintf("%s-%s-%d", t.Name(), name, time.Now().UnixNano())
	opts.Addr = ln.Addr().String()
	node, err := cluster.Join(context.Background(), repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	system := protoactor.NewActorSystem()
	reg := actor.NewRegistry(system, repo, h)
	reg.EnableCluster(node)

	ctx, cancel := context.WithCancel(context.Background())
	go node.Run(ctx)
	if relay {
		relay := cluster.NewRelay(repo, h, node.ID())
		go relay.Run(ctx)
		select {
		case <-relay.Ready():
		case <-time.After(5 * time.Second):
			t.Fatal("relay never started listening")
		}
	}
	srv := httptest.NewUnstartedServer(reg.InternalHandler())
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()

	srvMCP := mcpserver.NewMCPServer("amp-api-test", "2.0.0", mcpserver.WithToolCapabilities(false))
	mcp.NewServer(reg, repo, h, nil).Register(srvMCP)
	r := &replica{name: name, node: node, reg: reg, hub: h, srv: srv, stopHB: cancel, system: system,
		mcp: &mcpStack{srv: srvMCP, repo: repo, reg: reg, hub: h}}
	t.Cleanup(func() {
		cancel()
		srv.Close()
		r.shutdownActors()
		_ = node.Leave(context.Background())
	})
	return r
}

// crash stops the replica the way a killed pod stops: no handover.
func (r *replica) crash() {
	r.stopHB()
	r.srv.CloseClientConnections()
	r.srv.Close()
	r.reg.StopAll()
	r.shutdownActors()
}

// clusterProject makes a project with an epic and story, and points both
// replicas' MCP stacks at it.
func clusterProject(t *testing.T, repo *repository.Repo, rs ...*replica) {
	t.Helper()
	ctx := context.Background()
	ts := time.Now().UnixNano()
	proj, err := repo.CreateProject(ctx, domain.CreateProjectRequest{Name: fmt.Sprintf("cluster-%s-%d", t.Name(), ts), Code: fmt.Sprintf("cl-%d", ts)})
	if err != nil {
		t.Fatal(err)
	}
	epic, err := repo.CreateEpic(ctx, domain.CreateEpicRequest{ProjectID: proj.ID, Name: "e", Priority: "1"})
	if err != nil {
		t.Fatal(err)
	}
	story, err := repo.CreateStory(ctx, domain.CreateStoryRequest{ProjectID: proj.ID, EpicID: epic.ID, Name: "s", AcceptanceCriteria: "a", Priority: "1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		r.mcp.projectID, r.mcp.epicID, r.mcp.storyID = proj.ID, epic.ID, story.ID
	}
}

// forwardingRegistry is TEST_CLUSTER mode for setupMCP: a registry on a
// replica that doesn't own projectID, so everything the test does to the
// project is forwarded to another replica that does.
func forwardingRegistry(t *testing.T, repo *repository.Repo, h *hub.Hub, projectID int) *actor.Registry {
	t.Helper()
	owner := joinReplica(t, repo, h, "owner", cluster.Options{}, false)
	if err := owner.reg.Touch(projectID); err != nil {
		t.Fatal(err)
	}
	return joinReplica(t, repo, h, "front", cluster.Options{}, false).reg
}

func clusterRepo(t *testing.T) *repository.Repo {
	t.Helper()
	repo, err := repository.New(context.Background(), testDSN(t))
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.RunMigrations(context.Background(), migrations); err != nil {
		t.Fatal(err)
	}
	return repo
}

func ownerOf(t *testing.T, repo *repository.Repo, projectID int) (node string, epoch int64) {
	t.Helper()
	o, err := repo.ClaimProject(context.Background(), projectID, "nobody-"+t.Name(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return o.NodeID, o.Epoch
}

func dbState(t *testing.T, repo *repository.Repo, taskID int) string {
	t.Helper()
	task, err := repo.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return string(task.State)
}

// A request on a replica that doesn't own the project runs on the owner,
// whose actors stay the single source of truth.
func TestCluster_RequestsRunOnTheOwner(t *testing.T) {
	repo := clusterRepo(t)
	a := startReplica(t, repo, hub.New(), "a", cluster.Options{})
	b := startReplica(t, repo, hub.New(), "b", cluster.Options{})
	clusterProject(t, repo, a, b)

	// A touches the project first, so A owns it.
	dep := taskID(t, a.mcp.call(t, "amp_create_task", map[string]any{"project_id": float64(a.mcp.projectID), "story_id": float64(a.mcp.storyID),
		"name": "schema", "description": "d", "acceptance_criteria": "a"}))
	if n, _ := ownerOf(t, repo, a.mcp.projectID); n != a.node.ID() {
		t.Fatalf("owner is %s, want %s", n, a.node.ID())
	}

	// Through B: create a dependent ticket, finish its dependency, read it back.
	next := taskID(t, b.mcp.call(t, "amp_create_task", map[string]any{"project_id": float64(b.mcp.projectID), "story_id": float64(b.mcp.storyID),
		"name": "endpoint", "description": "d", "acceptance_criteria": "a", "dependency_ids": []any{float64(dep)}}))
	if st := stateOf(t, b.mcp, next); st != "blocked" {
		t.Fatalf("new dependent ticket via B: %s", st)
	}
	b.mcp.call(t, "amp_complete_task", map[string]any{"task_id": float64(dep)})
	if st := stateOf(t, b.mcp, next); st != "backlog" {
		t.Fatalf("after its dependency finished via B: %s", st)
	}
	// A's actors saw all of it (they did it), and so did the database.
	if st := stateOf(t, a.mcp, next); st != "backlog" {
		t.Fatalf("A's view: %s", st)
	}
	if st := dbState(t, repo, next); st != "backlog" {
		t.Fatalf("database: %s", st)
	}
	// B never started the project's actors.
	if n, _ := ownerOf(t, repo, a.mcp.projectID); n != a.node.ID() {
		t.Fatalf("ownership moved to %s", n)
	}

	// Errors come back with their meaning: a refused clear is a bad request.
	msg := b.mcp.callExpectError(t, "amp_update_task", map[string]any{"task_id": float64(next), "clear": []any{"name"}})
	if !strings.Contains(msg, "can't be cleared") {
		t.Fatalf("error from the owner: %q", msg)
	}
}

// Two replicas racing to start the same ticket: exactly one wins, because
// both go through the one actor that owns it.
func TestCluster_OnlyOneDispatchWins(t *testing.T) {
	repo := clusterRepo(t)
	a := startReplica(t, repo, hub.New(), "a", cluster.Options{})
	b := startReplica(t, repo, hub.New(), "b", cluster.Options{})
	clusterProject(t, repo, a, b)
	id := taskID(t, a.mcp.call(t, "amp_create_task", map[string]any{"project_id": float64(a.mcp.projectID), "story_id": float64(a.mcp.storyID),
		"name": "contended", "description": "d", "acceptance_criteria": "a"}))

	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		r := a
		if i%2 == 1 {
			r = b
		}
		wg.Add(1)
		go func(r *replica, i int) {
			defer wg.Done()
			reply := make(chan actor.ReplySimple, 1)
			if err := r.reg.Send(r.mcp.projectID, &actor.MsgDispatchTask{TaskID: id, AgentID: fmt.Sprintf("%s-%d", r.name, i), ReplyCh: reply}); err != nil {
				t.Error(err)
				return
			}
			if (<-reply).Err == nil {
				wins.Add(1)
			}
		}(r, i)
	}
	wg.Wait()
	if w := wins.Load(); w != 1 {
		t.Fatalf("%d dispatches won, want exactly 1", w)
	}
}

// sseEvents connects to a replica's live events and returns the types seen.
func sseEvents(t *testing.T, h *hub.Hub) func(want string, timeout time.Duration) map[string]any {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	events := make(chan map[string]any, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if line, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var e map[string]any
				if json.Unmarshal([]byte(line), &e) == nil {
					events <- e
				}
			}
		}
	}()
	return func(want string, timeout time.Duration) map[string]any {
		t.Helper()
		deadline := time.After(timeout)
		for {
			select {
			case e := <-events:
				if e["type"] == want {
					return e
				}
			case <-deadline:
				t.Fatalf("no %s event within %s", want, timeout)
				return nil
			}
		}
	}
}

// A browser connected to any replica sees every change, including ones too
// big to fit in a NOTIFY.
func TestCluster_LiveEventsReachEveryReplica(t *testing.T) {
	repo := clusterRepo(t)
	a := startReplica(t, repo, hub.New(), "a", cluster.Options{})
	b := startReplica(t, repo, hub.New(), "b", cluster.Options{})
	clusterProject(t, repo, a, b)
	onB := sseEvents(t, b.hub)
	onA := sseEvents(t, a.hub)

	big := strings.Repeat("long description ", 1000) // ~17KB
	id := taskID(t, a.mcp.call(t, "amp_create_task", map[string]any{"project_id": float64(a.mcp.projectID), "story_id": float64(a.mcp.storyID),
		"name": "visible everywhere", "description": big, "acceptance_criteria": "a"}))

	e := onB("task.created", 5*time.Second)
	p, _ := e["payload"].(map[string]any)
	if p["name"] != "visible everywhere" || p["description"] != big || int(e["project_id"].(float64)) != a.mcp.projectID {
		t.Fatalf("event on B: %v", e["project_id"])
	}
	onA("task.created", 5*time.Second)

	// And the other way: a change made through B (run by A) shows on both.
	b.mcp.call(t, "amp_update_task", map[string]any{"task_id": float64(id), "name": "renamed"})
	if p := onB("task.updated", 5*time.Second)["payload"].(map[string]any); p["name"] != "renamed" {
		t.Fatalf("update on B: %v", p["name"])
	}
	onA("task.updated", 5*time.Second)
}

// When the owner dies without a word, another replica takes its projects
// over once its heartbeat expires, from what the database holds.
func TestCluster_TakeoverAfterOwnerDies(t *testing.T) {
	repo := clusterRepo(t)
	a := startReplica(t, repo, hub.New(), "a", fastCluster)
	b := startReplica(t, repo, hub.New(), "b", fastCluster)
	clusterProject(t, repo, a, b)
	id := taskID(t, a.mcp.call(t, "amp_create_task", map[string]any{"project_id": float64(a.mcp.projectID), "story_id": float64(a.mcp.storyID),
		"name": "survives", "description": "d", "acceptance_criteria": "a"}))
	a.mcp.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(id), "agent_id": "claude"})
	_, epoch := ownerOf(t, repo, a.mcp.projectID)

	a.crash()
	start := time.Now()
	// B waits for A's claim to expire, then serves the project itself.
	if st := stateOf(t, b.mcp, id); st != "in_progress" {
		t.Fatalf("after takeover: %s", st)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("takeover took %s", waited)
	}
	if n, e := ownerOf(t, repo, a.mcp.projectID); n != b.node.ID() || e != epoch+1 {
		t.Fatalf("owner %s epoch %d, want %s epoch %d", n, e, b.node.ID(), epoch+1)
	}
	b.mcp.call(t, "amp_complete_task", map[string]any{"task_id": float64(id)})
	if st := dbState(t, repo, id); st != "completed" {
		t.Fatalf("database after completing on B: %s", st)
	}
}

// A replica that can't heartbeat stops running its projects before anyone
// else may take them: until its heartbeat recovers it refuses to answer
// rather than answer from state that may be stale, and once it recovers
// it passes requests to the new owner.
func TestCluster_ReplicaWithoutHeartbeatStopsServing(t *testing.T) {
	repo := clusterRepo(t)
	a := startReplica(t, repo, hub.New(), "a", fastCluster)
	b := startReplica(t, repo, hub.New(), "b", fastCluster)
	clusterProject(t, repo, a, b)
	id := taskID(t, a.mcp.call(t, "amp_create_task", map[string]any{"project_id": float64(a.mcp.projectID), "story_id": float64(a.mcp.storyID),
		"name": "fenced", "description": "d", "acceptance_criteria": "a"}))

	// A loses the database (its heartbeats stop) but keeps running.
	a.stopHB()
	time.Sleep(fastCluster.FenceAfter + 100*time.Millisecond)
	if a.node.Alive() {
		t.Fatal("A still thinks it may serve projects")
	}
	// B's request waits out A's claim, then B runs the project.
	b.mcp.call(t, "amp_update_task", map[string]any{"task_id": float64(id), "name": "changed on B"})
	if n, _ := ownerOf(t, repo, a.mcp.projectID); n != b.node.ID() {
		t.Fatalf("owner %s, want B", n)
	}

	// A's heartbeat comes back. Its old actors are gone: reading through A
	// gets B's answer, not A's stale copy, and B keeps the project.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.node.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !a.node.Alive() {
		if time.Now().After(deadline) {
			t.Fatal("A's heartbeat never recovered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if name := planTask(t, a.mcp, id)["name"]; name != "changed on B" {
		t.Fatalf("A answered from stale state: %v", name)
	}
	if n, _ := ownerOf(t, repo, a.mcp.projectID); n != b.node.ID() {
		t.Fatalf("a recovered replica took the project back: %s", n)
	}
}

// A replica shutting down hands its projects over at once, without making
// anyone wait for its heartbeat to expire.
func TestCluster_ShutdownHandsOverAtOnce(t *testing.T) {
	repo := clusterRepo(t)
	a := startReplica(t, repo, hub.New(), "a", cluster.Options{}) // 10s TTL
	b := startReplica(t, repo, hub.New(), "b", cluster.Options{})
	clusterProject(t, repo, a, b)
	id := taskID(t, a.mcp.call(t, "amp_create_task", map[string]any{"project_id": float64(a.mcp.projectID), "story_id": float64(a.mcp.storyID),
		"name": "handed over", "description": "d", "acceptance_criteria": "a"}))

	a.srv.Close()
	a.reg.StopAll()
	a.shutdownActors()
	if err := a.node.Leave(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.stopHB()
	start := time.Now()
	if st := stateOf(t, b.mcp, id); st != "backlog" {
		t.Fatalf("on B: %s", st)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("handover took %s", waited)
	}
}

// A ticket whose start time passed while its replica was down unblocks as
// soon as another replica starts the project.
func TestCluster_ScheduledTicketUnblocksAfterTakeover(t *testing.T) {
	repo := clusterRepo(t)
	a := startReplica(t, repo, hub.New(), "a", fastCluster)
	b := startReplica(t, repo, hub.New(), "b", fastCluster)
	clusterProject(t, repo, a, b)
	id := taskID(t, a.mcp.call(t, "amp_create_task", map[string]any{"project_id": float64(a.mcp.projectID), "story_id": float64(a.mcp.storyID),
		"name": "later", "description": "d", "acceptance_criteria": "a"}))
	at := time.Now().Add(time.Second)
	reply := make(chan actor.ReplySimple, 1)
	if err := a.reg.Send(a.mcp.projectID, &actor.MsgSetTaskStartAt{TaskID: id, StartAt: &at, ReplyCh: reply}); err != nil {
		t.Fatal(err)
	}
	if r := <-reply; r.Err != nil {
		t.Fatal(r.Err)
	}
	if st := dbState(t, repo, id); st != "blocked" {
		t.Fatalf("scheduled ticket: %s", st)
	}

	a.crash()
	time.Sleep(time.Until(at) + 200*time.Millisecond)
	if st := dbState(t, repo, id); st != "blocked" {
		t.Fatalf("unblocked with no replica running it: %s", st)
	}
	if err := b.reg.Touch(a.mcp.projectID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for dbState(t, repo, id) != "backlog" {
		if time.Now().After(deadline) {
			t.Fatalf("still %s after the takeover", dbState(t, repo, id))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The internal listener only answers replicas.
func TestCluster_InternalListenerNeedsTheToken(t *testing.T) {
	repo := clusterRepo(t)
	a := startReplica(t, repo, hub.New(), "a", cluster.Options{})
	for _, tok := range []string{"", "wrong"} {
		req, _ := http.NewRequest(http.MethodPost, a.srv.URL+"/internal/actor?project=1", strings.NewReader("x"))
		if tok != "" {
			req.Header.Set("X-AMP-Cluster-Token", tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: %d", tok, resp.StatusCode)
		}
	}
}

// A legacy MCP client keeps its session when its requests land on
// different replicas.
func TestCluster_LegacyMCPSessionAcrossReplicas(t *testing.T) {
	repo := clusterRepo(t)
	a := startReplica(t, repo, hub.New(), "a", cluster.Options{})
	b := startReplica(t, repo, hub.New(), "b", cluster.Options{})
	clusterProject(t, repo, a, b)
	base, err := auth.NewVerifier(context.Background(), auth.Config{})
	if err != nil {
		t.Fatal(err)
	}
	url := func(r *replica) string {
		cfg := mcpHTTPConfig{PublicURL: "http://amp.test/mcp", OAuthURL: "http://amp.test/mcp-oauth"}
		h := newMCPHTTPHandler(newMCPServer(mcp.NewServer(r.reg, repo, r.hub, nil)), base.WithAudiences("mcp", "amp-mcp"), cfg)
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return srv.URL + "/"
	}
	ua, ub := url(a), url(b)

	init := post(t, ua, nil, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": legacyVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "legacy", "version": "1"}}})
	sid := init.header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("no session from A: %s", init.raw)
	}
	h := map[string]string{"Mcp-Session-Id": sid, "MCP-Protocol-Version": legacyVersion}
	post(t, ub, h, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	res := post(t, ub, h, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{
		"name": "amp_list_tasks", "arguments": map[string]any{"project_id": float64(b.mcp.projectID)}}})
	if res.status != http.StatusOK || errCode(res) != 0 || !strings.Contains(toolText(t, res), "ready_to_dispatch") {
		t.Fatalf("session from A used on B: %d %s", res.status, res.raw)
	}
}
