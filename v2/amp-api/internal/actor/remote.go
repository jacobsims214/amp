package actor

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/repository"
)

// Running across replicas (see internal/cluster).
//
// With a Placement, Registry.Get returns a router for the project rather
// than the project actor itself. The router hands each message to the
// project actor when this replica owns the project, and otherwise sends it
// to the owner's internal listener (InternalHandler), which delivers it to
// the actor there and sends back the reply. Callers can't tell the
// difference: they send a message and read its ReplyCh as always.

// Placement decides which replica runs each project's actors.
type Placement interface {
	// Alive reports whether this replica may run projects right now.
	Alive() bool
	// Claim makes this replica the project's owner unless another live
	// replica is; it returns "" when this replica owns it, else the
	// owner's address.
	Claim(ctx context.Context, projectID int) (string, error)
	// Token authenticates calls between replicas.
	Token() string
	// OnFence registers fn to run when this replica stops being alive.
	OnFence(fn func())
}

const (
	// routeTimeout bounds how long a message waits for its project to have
	// a reachable owner, e.g. while a dead owner's claim expires.
	routeTimeout = 30 * time.Second
	// forwardTimeout bounds one call to the owner.
	forwardTimeout = 60 * time.Second

	internalPath   = "/internal/actor"
	tokenHeader    = "X-AMP-Cluster-Token"
	notOwnerStatus = http.StatusConflict
)

// errRetry marks a failure where the message certainly wasn't delivered, so
// routing it again (perhaps to a new owner) is safe.
type errRetry struct{ err error }

func (e errRetry) Error() string { return e.err.Error() }
func (e errRetry) Unwrap() error { return e.err }

func retryable(err error) bool {
	var r errRetry
	return errors.As(err, &r)
}

// EnableCluster makes the registry share projects with other replicas
// through p. Call it before the registry is used.
func (r *Registry) EnableCluster(p Placement) {
	r.mu.Lock()
	r.place = p
	r.mu.Unlock()
	p.OnFence(r.StopAll)
}

// router is the PID callers get for a project when running across
// replicas. It routes; the project actor (here or on the owner) does the
// work.
type router struct {
	reg       *Registry
	projectID int
}

func (a *router) Receive(ctx actor.Context) {
	msg := ctx.Message()
	if _, ok := wireTypes[reflect.TypeOf(msg)]; !ok {
		return // lifecycle messages
	}
	if pid, ok := a.reg.ownedPID(a.projectID); ok {
		ctx.Send(pid, msg)
		return
	}
	go a.reg.route(a.projectID, msg)
}

// routerPID returns the project's router, starting it the first time.
func (r *Registry) routerPID(projectID int) (*actor.PID, error) {
	r.mu.RLock()
	pid, ok := r.routers[projectID]
	r.mu.RUnlock()
	if ok {
		return pid, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if pid, ok := r.routers[projectID]; ok {
		return pid, nil
	}
	props := actor.PropsFromProducer(func() actor.Actor { return &router{reg: r, projectID: projectID} })
	pid, err := r.system.Root.SpawnNamed(props, fmt.Sprintf("route-%d", projectID))
	if err != nil {
		return nil, fmt.Errorf("spawn router for project %d: %w", projectID, err)
	}
	r.routers[projectID] = pid
	return pid, nil
}

// ownedPID is the project's actor when this replica runs it and is alive.
func (r *Registry) ownedPID(projectID int) (*actor.PID, bool) {
	r.mu.RLock()
	pid, ok := r.actors[projectID]
	r.mu.RUnlock()
	if !ok || !r.place.Alive() {
		return nil, false
	}
	return pid, true
}

// adopt starts the project's actor here, after this replica claimed it.
func (r *Registry) adopt(projectID int) (*actor.PID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pid, ok := r.actors[projectID]; ok {
		return pid, nil
	}
	return r.spawnLocked(projectID)
}

// StopAll stops every project actor this replica runs: it is shutting down,
// or has stopped heartbeating so another replica may take its projects
// over. Later messages start them again (or go to the new owner).
func (r *Registry) StopAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, pid := range r.actors {
		r.system.Root.Stop(pid)
		delete(r.actors, id)
		delete(r.owners, id)
	}
}

// route delivers msg to the project's actor wherever it runs, retrying
// while the owner is changing. If it can't, the caller gets the error on
// ReplyCh.
func (r *Registry) route(projectID int, msg any) {
	deadline := time.Now().Add(routeTimeout)
	backoff := 25 * time.Millisecond
	for {
		err := r.routeOnce(projectID, msg)
		if err == nil {
			return
		}
		if !retryable(err) || time.Now().After(deadline) {
			slog.Default().Warn("could not deliver a message to its project", "project_id", projectID,
				"message", reflect.TypeOf(msg).Elem().Name(), "err", err)
			replyErr(msg, fmt.Errorf("project %d is unavailable: %w", projectID, err))
			return
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, time.Second)
	}
}

func (r *Registry) routeOnce(projectID int, msg any) error {
	if !r.place.Alive() {
		r.StopAll()
		return errRetry{errors.New("this replica can't reach the database")}
	}
	if pid, ok := r.ownedPID(projectID); ok {
		r.system.Root.Send(pid, msg)
		return nil
	}
	addr, err := r.ownerOf(projectID)
	if err != nil {
		return errRetry{err}
	}
	if addr == "" {
		pid, err := r.adopt(projectID)
		if err != nil {
			return err
		}
		r.system.Root.Send(pid, msg)
		return nil
	}
	err = r.forward(addr, projectID, msg)
	if retryable(err) {
		r.mu.Lock()
		delete(r.owners, projectID)
		r.mu.Unlock()
	}
	return err
}

// ownerOf is the project's owner: "" for this replica (claimed now if
// nobody alive had it), else the owner's address, cached until a call to
// it fails.
func (r *Registry) ownerOf(projectID int) (string, error) {
	r.mu.RLock()
	addr, ok := r.owners[projectID]
	r.mu.RUnlock()
	if ok {
		return addr, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr, err := r.place.Claim(ctx, projectID)
	if err != nil {
		return "", err
	}
	if addr != "" {
		r.mu.Lock()
		r.owners[projectID] = addr
		r.mu.Unlock()
	}
	return addr, nil
}

// ---- Calling the owner ----

// envelope is a message on its way to the owner.
type envelope struct {
	Type string
	Body []byte
}

// wireReply is the owner's reply. The reply's Err travels separately,
// keeping what callers check it for: authz status, not found, conflict.
type wireReply struct {
	Body   []byte
	Err    string
	Status int    // authz.Error status
	Kind   string // "not_found" or "conflict"
}

var httpClient = &http.Client{Transport: &http.Transport{
	Proxy:               nil,
	DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConnsPerHost: 64,
	IdleConnTimeout:     90 * time.Second,
}}

// forward sends msg to the owner at addr and puts its reply on msg's
// ReplyCh. Errors are retryable only when the owner certainly didn't run
// the message.
func (r *Registry) forward(addr string, projectID int, msg any) error {
	body, err := encodeMsg(msg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), forwardTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+addr+internalPath+"?project="+strconv.Itoa(projectID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set(tokenHeader, r.place.Token())
	resp, err := httpClient.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return errRetry{fmt.Errorf("owner %s unreachable: %w", addr, err)}
		}
		return fmt.Errorf("calling owner %s: %w", addr, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return fmt.Errorf("reading owner %s's reply: %w", addr, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusAccepted:
		return nil // no reply expected
	case notOwnerStatus:
		return errRetry{fmt.Errorf("%s no longer owns project %d", addr, projectID)}
	default:
		return fmt.Errorf("owner %s: %s: %s", addr, resp.Status, bytes.TrimSpace(data))
	}
	reply, err := decodeReply(msg, data)
	if err != nil {
		return err
	}
	deliver(msg, reply)
	return nil
}

// ---- Serving other replicas ----

// InternalHandler serves messages other replicas forward to the projects
// this one owns. Mount it on a listener only replicas can reach.
func (r *Registry) InternalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(internalPath, r.serveInternal)
	return mux
}

func (r *Registry) serveInternal(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.place == nil || subtle.ConstantTimeCompare([]byte(req.Header.Get(tokenHeader)), []byte(r.place.Token())) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	projectID, err := strconv.Atoi(req.URL.Query().Get("project"))
	if err != nil {
		http.Error(w, "bad project", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 64<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	msg, replyCh, err := decodeMsg(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Only the owner may run it; anyone else sends the caller to look again.
	pid, ok := r.ownedPID(projectID)
	if !ok {
		if !r.place.Alive() {
			http.Error(w, "not serving projects", notOwnerStatus)
			return
		}
		addr, err := r.place.Claim(req.Context(), projectID)
		if err != nil || addr != "" {
			http.Error(w, "not the owner", notOwnerStatus)
			return
		}
		if pid, err = r.adopt(projectID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	r.system.Root.Send(pid, msg)
	if !replyCh.IsValid() {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	timeout := time.NewTimer(forwardTimeout)
	defer timeout.Stop()
	chosen, reply, _ := reflect.Select([]reflect.SelectCase{
		{Dir: reflect.SelectRecv, Chan: replyCh},
		{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(req.Context().Done())},
		{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(timeout.C)},
	})
	if chosen != 0 {
		http.Error(w, "no reply from the project actor", http.StatusGatewayTimeout)
		return
	}
	out, err := encodeReply(reply)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-gob")
	_, _ = w.Write(out)
}

// ---- Codec ----
//
// Messages travel as gob: it keeps every exported field (JSON would drop
// those tagged json:"-", such as who created a ticket) and skips channels.

// wireTypes are the messages that can be sent to a project from outside
// its actors, by type, and by name for decoding.
var (
	wireTypes  = map[reflect.Type]string{}
	wireByName = map[string]reflect.Type{}
)

func init() {
	for _, m := range []any{
		&MsgCreateEpic{}, &MsgGetEpic{}, &MsgListEpicTasks{}, &MsgUpdateEpic{},
		&MsgCreateStory{}, &MsgGetStory{}, &MsgListStoryTasks{}, &MsgUpdateStory{},
		&MsgCreateTask{}, &MsgGetTask{}, &MsgListTasks{}, &MsgDispatchTask{}, &MsgCompleteTask{},
		&MsgBlockTask{}, &MsgAssignTask{}, &MsgUpdateTask{}, &MsgAddComment{}, &MsgGetComments{},
		&MsgSetTaskState{}, &MsgSetTaskStartAt{}, &MsgReset{}, &MsgDeleteEpic{}, &MsgDeleteStory{},
		&MsgDeleteTask{}, &MsgSetTaskDeps{}, &MsgReconcileDeps{}, &MsgMoveTask{}, &MsgMoveStory{},
		&MsgRestoreTask{}, &MsgRestoreEpic{}, &MsgRestoreStory{}, &MsgPing{},
	} {
		t := reflect.TypeOf(m)
		wireTypes[t] = t.Elem().Name()
		wireByName[t.Elem().Name()] = t
	}
	// Free-form metadata values arrive from JSON as these.
	gob.Register(map[string]any{})
	gob.Register([]any{})
	gob.Register(json.Number(""))
}

// hasData reports whether gob has any field of t to send (it refuses
// structs with none). skip names a field to leave out.
func hasData(t reflect.Type, skip string) bool {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() || f.Name == skip {
			continue
		}
		if k := f.Type.Kind(); k != reflect.Chan && k != reflect.Func {
			return true
		}
	}
	return false
}

func encodeMsg(msg any) ([]byte, error) {
	name, ok := wireTypes[reflect.TypeOf(msg)]
	if !ok {
		return nil, fmt.Errorf("%T can't be sent to another replica", msg)
	}
	env := envelope{Type: name}
	if hasData(reflect.TypeOf(msg).Elem(), "") {
		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(msg); err != nil {
			return nil, fmt.Errorf("encode %s: %w", name, err)
		}
		env.Body = buf.Bytes()
	}
	var out bytes.Buffer
	err := gob.NewEncoder(&out).Encode(env)
	return out.Bytes(), err
}

// decodeMsg rebuilds a message, giving it a fresh ReplyCh when it has
// one (returned so the caller can wait on it).
func decodeMsg(data []byte) (any, reflect.Value, error) {
	var env envelope
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&env); err != nil {
		return nil, reflect.Value{}, fmt.Errorf("decode envelope: %w", err)
	}
	t, ok := wireByName[env.Type]
	if !ok {
		return nil, reflect.Value{}, fmt.Errorf("unknown message %q", env.Type)
	}
	v := reflect.New(t.Elem())
	if len(env.Body) > 0 {
		if err := gob.NewDecoder(bytes.NewReader(env.Body)).Decode(v.Interface()); err != nil {
			return nil, reflect.Value{}, fmt.Errorf("decode %s: %w", env.Type, err)
		}
	}
	var ch reflect.Value
	if f := v.Elem().FieldByName("ReplyCh"); f.IsValid() {
		ch = reflect.MakeChan(f.Type(), 1)
		f.Set(ch)
	}
	return v.Interface(), ch, nil
}

func encodeReply(reply reflect.Value) ([]byte, error) {
	var w wireReply
	cp := reflect.New(reply.Type()).Elem()
	cp.Set(reply)
	if f := cp.FieldByName("Err"); f.IsValid() && !f.IsNil() {
		err := f.Interface().(error)
		w.Err = err.Error()
		var ae *authz.Error
		switch {
		case errors.As(err, &ae):
			w.Status = ae.Status
		case errors.Is(err, repository.ErrNotFound):
			w.Kind = "not_found"
		case errors.Is(err, repository.ErrConflict):
			w.Kind = "conflict"
		}
		f.Set(reflect.Zero(f.Type()))
	}
	if hasData(reply.Type(), "Err") {
		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(cp.Addr().Interface()); err != nil {
			return nil, fmt.Errorf("encode reply: %w", err)
		}
		w.Body = buf.Bytes()
	}
	var out bytes.Buffer
	err := gob.NewEncoder(&out).Encode(w)
	return out.Bytes(), err
}

// decodeReply rebuilds the reply msg expects on its ReplyCh.
func decodeReply(msg any, data []byte) (reflect.Value, error) {
	ch := reflect.ValueOf(msg).Elem().FieldByName("ReplyCh")
	if !ch.IsValid() {
		return reflect.Value{}, nil
	}
	var w wireReply
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&w); err != nil {
		return reflect.Value{}, fmt.Errorf("decode reply: %w", err)
	}
	reply := reflect.New(ch.Type().Elem())
	if len(w.Body) > 0 {
		if err := gob.NewDecoder(bytes.NewReader(w.Body)).Decode(reply.Interface()); err != nil {
			return reflect.Value{}, fmt.Errorf("decode reply: %w", err)
		}
	}
	if w.Err != "" {
		var err error = &remoteError{msg: w.Err, kind: w.Kind}
		if w.Status != 0 {
			err = &authz.Error{Status: w.Status, Msg: w.Err}
		}
		reply.Elem().FieldByName("Err").Set(reflect.ValueOf(err))
	}
	return reply.Elem(), nil
}

// remoteError is an error the owner replied with.
type remoteError struct{ msg, kind string }

func (e *remoteError) Error() string { return e.msg }

func (e *remoteError) Is(target error) bool {
	return (e.kind == "not_found" && target == repository.ErrNotFound) ||
		(e.kind == "conflict" && target == repository.ErrConflict)
}

// deliver puts reply on msg's ReplyCh without blocking the caller of
// deliver (callers make their channels buffered, but don't rely on it).
func deliver(msg any, reply reflect.Value) {
	ch := reflect.ValueOf(msg).Elem().FieldByName("ReplyCh")
	if !ch.IsValid() || ch.IsNil() || !reply.IsValid() {
		return
	}
	if !ch.TrySend(reply) {
		go ch.Send(reply)
	}
}

// replyErr answers msg with err, so its caller doesn't wait forever.
func replyErr(msg any, err error) {
	ch := reflect.ValueOf(msg).Elem().FieldByName("ReplyCh")
	if !ch.IsValid() {
		return
	}
	reply := reflect.New(ch.Type().Elem()).Elem()
	reply.FieldByName("Err").Set(reflect.ValueOf(err))
	deliver(msg, reply)
}
