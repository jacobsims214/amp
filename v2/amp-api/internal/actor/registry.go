package actor

import (
	"fmt"
	"sync"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/repository"
)

// Registry maps project IDs to their running ProjectActor PIDs.
// It creates actors on first access (lazy init) and keeps them alive
// for the lifetime of the process — the supervisor restarts any that crash.
//
// Across replicas (EnableCluster), a project's actors run only on the
// replica that owns it, and Get returns a router that finds them (see
// remote.go).
type Registry struct {
	mu     sync.RWMutex
	system *actor.ActorSystem
	actors map[int]*actor.PID // project actors running here
	repo   *repository.Repo
	hub    *hub.Hub

	place   Placement          // nil: a single replica runs every project
	routers map[int]*actor.PID // project → its router (with place)
	owners  map[int]string     // project → its owner's address, when not here
	spawns  int                // names restarted project actors uniquely
}

func NewRegistry(system *actor.ActorSystem, repo *repository.Repo, h *hub.Hub) *Registry {
	return &Registry{
		system:  system,
		actors:  make(map[int]*actor.PID),
		repo:    repo,
		hub:     h,
		routers: make(map[int]*actor.PID),
		owners:  make(map[int]string),
	}
}

// System exposes the underlying ActorSystem for callers that need to Send messages directly.
func (r *Registry) System() *actor.ActorSystem { return r.system }

// Get returns the PID to send a project's messages to, creating its actor
// if it doesn't exist. Across replicas it is the project's router.
func (r *Registry) Get(projectID int) (*actor.PID, error) {
	r.mu.RLock()
	clustered := r.place != nil
	r.mu.RUnlock()
	if clustered {
		return r.routerPID(projectID)
	}

	r.mu.RLock()
	pid, ok := r.actors[projectID]
	r.mu.RUnlock()
	if ok {
		return pid, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after acquiring write lock.
	if pid, ok = r.actors[projectID]; ok {
		return pid, nil
	}
	return r.spawnLocked(projectID)
}

// spawnLocked starts a project's actor; r.mu must be held.
func (r *Registry) spawnLocked(projectID int) (*actor.PID, error) {
	props := actor.PropsFromProducer(func() actor.Actor {
		return NewProjectActor(projectID, r.repo, r.hub)
	})

	// A project whose actor was stopped (its replica lost and regained it)
	// gets a fresh name: the old one may still be shutting down.
	name := fmt.Sprintf("project-%d", projectID)
	if r.spawns++; r.place != nil {
		name = fmt.Sprintf("project-%d-%d", projectID, r.spawns)
	}
	pid, err := r.system.Root.SpawnNamed(props, name)
	if err != nil {
		return nil, fmt.Errorf("spawn project actor %d: %w", projectID, err)
	}

	r.actors[projectID] = pid
	return pid, nil
}

// Touch makes sure the project's actor is running somewhere (e.g. so its
// scheduled tickets unblock on time) and waits until it answers.
func (r *Registry) Touch(projectID int) error {
	reply := make(chan ReplySimple, 1)
	if err := r.Send(projectID, &MsgPing{ReplyCh: reply}); err != nil {
		return err
	}
	return (<-reply).Err
}

// Send delivers a message to the project actor and returns immediately.
// Use this for fire-and-forget. For request/reply use Ask().
func (r *Registry) Send(projectID int, msg interface{}) error {
	pid, err := r.Get(projectID)
	if err != nil {
		return err
	}
	r.system.Root.Send(pid, msg)
	return nil
}
