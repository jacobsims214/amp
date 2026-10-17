package cluster

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/repository"
)

// eventsChannel is the NOTIFY channel live events travel on.
const eventsChannel = "amp_live_events"

// maxInline is the largest event sent inside the NOTIFY payload itself
// (Postgres allows just under 8000 bytes); bigger ones are stored in
// live_events and sent by id.
const maxInline = 7000

// relayed is one event on the wire: inline, or a live_events id.
type relayed struct {
	Origin string           `json:"o"`
	Event  *json.RawMessage `json:"e,omitempty"`
	Ref    int64            `json:"r,omitempty"`
}

// wireEvent is domain.Event with its payload kept as raw JSON, so the
// receiving replica sends browsers exactly what the publisher would have.
type wireEvent struct {
	Type      domain.EventType `json:"type"`
	ProjectID int              `json:"project_id"`
	Payload   json.RawMessage  `json:"payload"`
	At        time.Time        `json:"at"`
}

// Relay copies live events between replicas: everything published on this
// replica's hub goes to the others, and theirs come here.
type Relay struct {
	repo *repository.Repo
	hub  *hub.Hub
	node string
	out  chan domain.Event
	log  *slog.Logger
	// ready is closed once this replica is listening (tests wait on it).
	ready chan struct{}
}

// NewRelay connects h to the other replicas. Call Run to start it.
func NewRelay(repo *repository.Repo, h *hub.Hub, nodeID string) *Relay {
	r := &Relay{repo: repo, hub: h, node: nodeID, out: make(chan domain.Event, 4096),
		log: slog.Default().With("component", "relay", "node", nodeID), ready: make(chan struct{})}
	h.SetRelay(r.publish)
	return r
}

// Ready is closed once the relay is receiving other replicas' events.
func (r *Relay) Ready() <-chan struct{} { return r.ready }

// publish queues an event for the other replicas; it never blocks the
// publishing actor, so a stalled database drops events instead.
func (r *Relay) publish(evt domain.Event) {
	select {
	case r.out <- evt:
	default:
		r.log.Warn("relay queue full; other replicas miss an event", "type", evt.Type, "project_id", evt.ProjectID)
	}
}

// Run sends and receives events until ctx ends.
func (r *Relay) Run(ctx context.Context) {
	go r.send(ctx)
	go r.prune(ctx)
	once := false
	for backoff := 100 * time.Millisecond; ctx.Err() == nil; {
		err := r.repo.Listen(ctx, eventsChannel, func() {
			if !once {
				once = true
				close(r.ready)
			}
			backoff = 100 * time.Millisecond
		}, r.receive)
		if ctx.Err() != nil {
			return
		}
		r.log.Warn("listening for other replicas' events failed; retrying", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func (r *Relay) send(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-r.out:
			if err := r.sendOne(ctx, evt); err != nil && ctx.Err() == nil {
				r.log.Warn("relaying an event failed", "type", evt.Type, "err", err)
			}
		}
	}
}

func (r *Relay) sendOne(ctx context.Context, evt domain.Event) error {
	body, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	raw := json.RawMessage(body)
	msg := relayed{Origin: r.node, Event: &raw}
	if len(body) > maxInline {
		id, err := r.repo.PutLiveEvent(ctx, string(body))
		if err != nil {
			return err
		}
		msg = relayed{Origin: r.node, Ref: id}
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return r.repo.Notify(ctx, eventsChannel, string(payload))
}

func (r *Relay) receive(payload string) {
	var msg relayed
	if err := json.Unmarshal([]byte(payload), &msg); err != nil || msg.Origin == r.node {
		return
	}
	body := []byte(nil)
	if msg.Event != nil {
		body = *msg.Event
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s, err := r.repo.LiveEvent(ctx, msg.Ref)
		cancel()
		if err != nil {
			r.log.Warn("reading a relayed event failed", "ref", strconv.FormatInt(msg.Ref, 10), "err", err)
			return
		}
		body = []byte(s)
	}
	var w wireEvent
	if err := json.Unmarshal(body, &w); err != nil {
		return
	}
	r.hub.Deliver(domain.Event{Type: w.Type, ProjectID: w.ProjectID, Payload: w.Payload, At: w.At})
}

func (r *Relay) prune(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.repo.PruneLiveEvents(ctx, 10*time.Minute); err != nil && ctx.Err() == nil {
				r.log.Warn("pruning stored events failed", "err", err)
			}
		}
	}
}
