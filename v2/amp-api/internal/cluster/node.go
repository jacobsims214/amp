// Package cluster lets several amp-api replicas share the work.
//
// Each project's actors (internal/actor) hold that project's live state in
// memory, so they must run on exactly one replica at a time. That replica
// is the project's owner, recorded in Postgres (project_owners). A replica
// that receives a request for a project it doesn't own passes it to the
// owner (actor.Registry does that, over the internal listener); the first
// replica to touch an unowned project claims it.
//
// Ownership lasts as long as the owner keeps heartbeating (api_nodes).
// When an owner stops — it crashed, its node died, it lost the database —
// any other replica can claim its projects once its heartbeat is TTL old,
// and loads their state from Postgres. A replica that can't heartbeat
// stops serving its projects well before then (FenceAfter < TTL), so two
// replicas never run the same project's actors at once.
//
// Live events (internal/hub) are copied to every replica through Postgres
// NOTIFY (see Relay), so a browser sees changes whichever replica it is
// connected to.
package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/simstech/amp-api/internal/repository"
)

// Options tune a Node. Zero values take the defaults.
type Options struct {
	// ID names this replica; default: hostname plus a random suffix, so a
	// restarted process never inherits the old one's ownership.
	ID string
	// Addr is where other replicas reach this one's internal listener
	// (host:port), e.g. the pod IP.
	Addr string
	// Heartbeat is how often the node proves it is alive (default 2s).
	Heartbeat time.Duration
	// TTL is how long after its last heartbeat a node's projects can be
	// taken over (default 10s).
	TTL time.Duration
	// FenceAfter is how long without a successful heartbeat before the node
	// stops serving its projects (default 6s). It must be well under TTL.
	FenceAfter time.Duration
}

func (o *Options) defaults() {
	if o.ID == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "amp-api"
		}
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		o.ID = host + "-" + hex.EncodeToString(b)
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = 2 * time.Second
	}
	if o.TTL <= 0 {
		o.TTL = 10 * time.Second
	}
	if o.FenceAfter <= 0 {
		o.FenceAfter = o.TTL * 6 / 10
	}
}

// Node is this replica's membership: its heartbeat, its claims on projects,
// and the token replicas use with each other.
type Node struct {
	repo  *repository.Repo
	opts  Options
	token string
	log   *slog.Logger

	mu      sync.Mutex
	lastOK  time.Time // start of the last heartbeat that succeeded
	fenced  bool
	onFence []func()
}

// Join registers this replica and starts it as alive. Call Run to keep it
// alive and Leave on shutdown.
func Join(ctx context.Context, repo *repository.Repo, opts Options) (*Node, error) {
	opts.defaults()
	if opts.Addr == "" {
		return nil, fmt.Errorf("cluster: an address other replicas can reach is required")
	}
	if opts.FenceAfter >= opts.TTL {
		return nil, fmt.Errorf("cluster: FenceAfter (%s) must be less than TTL (%s)", opts.FenceAfter, opts.TTL)
	}
	tok, err := repo.ClusterToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("cluster token: %w", err)
	}
	n := &Node{repo: repo, opts: opts, token: tok, log: slog.Default().With("component", "cluster", "node", opts.ID)}
	if err := n.beat(ctx); err != nil {
		return nil, fmt.Errorf("cluster: first heartbeat: %w", err)
	}
	return n, nil
}

// ID is this replica's name.
func (n *Node) ID() string { return n.opts.ID }

// Addr is where other replicas reach this one.
func (n *Node) Addr() string { return n.opts.Addr }

// TTL is how long after its last heartbeat this replica's work may be taken
// over by another.
func (n *Node) TTL() time.Duration { return n.opts.TTL }

// Token authenticates calls between replicas.
func (n *Node) Token() string { return n.token }

// Alive reports whether this replica may run its projects: its last
// successful heartbeat began less than FenceAfter ago.
func (n *Node) Alive() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return time.Since(n.lastOK) < n.opts.FenceAfter
}

// OnFence registers fn to run when the node stops being alive; the
// registry stops every project actor it runs.
func (n *Node) OnFence(fn func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.onFence = append(n.onFence, fn)
}

// Claim makes this replica a project's owner unless another live replica
// already is. It returns "" when this replica owns the project, or the
// owner's address.
func (n *Node) Claim(ctx context.Context, projectID int) (string, error) {
	if !n.Alive() {
		return "", fmt.Errorf("this replica can't reach the database to keep its claims")
	}
	o, err := n.repo.ClaimProject(ctx, projectID, n.opts.ID, n.opts.TTL)
	if err != nil {
		return "", err
	}
	if o.NodeID == n.opts.ID {
		return "", nil
	}
	if o.Addr == "" {
		return "", fmt.Errorf("project %d: owner %s has no address", projectID, o.NodeID)
	}
	return o.Addr, nil
}

// Run heartbeats until ctx ends, fencing the node whenever heartbeats stop
// succeeding and pruning replicas that went away long ago.
func (n *Node) Run(ctx context.Context) {
	beat := time.NewTicker(n.opts.Heartbeat)
	defer beat.Stop()
	check := time.NewTicker(n.opts.Heartbeat / 4)
	defer check.Stop()
	prune := time.NewTicker(10 * time.Minute)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-beat.C:
			bctx, cancel := context.WithTimeout(ctx, n.opts.Heartbeat)
			if err := n.beat(bctx); err != nil && ctx.Err() == nil {
				n.log.Warn("heartbeat failed", "err", err)
			}
			cancel()
		case <-check.C:
			n.checkFence()
		case <-prune.C:
			if err := n.repo.PruneNodes(ctx, time.Hour); err != nil && ctx.Err() == nil {
				n.log.Warn("pruning old replicas failed", "err", err)
			}
		}
	}
}

func (n *Node) beat(ctx context.Context) error {
	start := time.Now()
	if err := n.repo.NodeHeartbeat(ctx, n.opts.ID, n.opts.Addr); err != nil {
		return err
	}
	n.mu.Lock()
	n.lastOK = start
	if n.fenced {
		n.fenced = false
		n.log.Info("heartbeats are succeeding again; serving projects")
	}
	n.mu.Unlock()
	return nil
}

func (n *Node) checkFence() {
	n.mu.Lock()
	if n.fenced || time.Since(n.lastOK) < n.opts.FenceAfter {
		n.mu.Unlock()
		return
	}
	n.fenced = true
	fns := append([]func(){}, n.onFence...)
	n.mu.Unlock()
	n.log.Error("no successful heartbeat; stopping this replica's projects so another can take them over", "after", n.opts.FenceAfter)
	for _, fn := range fns {
		fn()
	}
}

// Leave releases every project this replica owns and removes it, so the
// others take its projects over immediately. Call it after the replica has
// stopped serving.
func (n *Node) Leave(ctx context.Context) error {
	return n.repo.RemoveNode(ctx, n.opts.ID)
}
