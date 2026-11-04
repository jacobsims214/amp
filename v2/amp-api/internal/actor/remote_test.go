package actor

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// A message forwarded to another replica arrives with every field, including
// the ones JSON leaves out (who created a ticket), metadata, and times, and
// with a fresh reply channel of the right type.
func TestCodec_MessagesRoundTrip(t *testing.T) {
	by, owner := 7, 9
	start := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	cases := []any{
		&MsgCreateTask{Req: domain.CreateTaskRequest{ProjectID: 3, EpicID: 4, StoryID: 5, Name: "n", Description: "d",
			DependencyIDs: []int{1, 2}, CreatedBy: &by, OwnerUserID: &owner,
			Metadata: domain.Metadata{"jira": "AMP-1", "points": 3.0, "tags": []any{"a", true}, "nested": map[string]any{"k": nil}}},
			ReplyCh: make(chan ReplyCreateTask, 1)},
		&MsgUpdateTask{Req: domain.UpdateTaskRequest{TaskID: 11, Description: "x", Clear: []string{"assigned_to"},
			Metadata: domain.MetadataPatch{"gone": nil}}, ReplyCh: make(chan ReplySimple, 1)},
		&MsgDispatchTask{TaskID: 2, AgentID: "claude", By: &by, Override: true, DispatchID: "d_abc", ReplyCh: make(chan ReplySimple, 1)},
		&MsgClaimStart{TaskID: 2, DispatchID: "d_abc", Post: "picked this up", ByUser: &by, ReplyCh: make(chan ReplyStart, 1)},
		&MsgReturnTask{TaskID: 2, Reason: "worker died", By: &by, ByName: "harness", ReplyCh: make(chan ReplyGetTask, 1)},
		&MsgCompleteTask{TaskID: 2, DispatchID: "d_abc", CloseoutReason: "worker died", CloseoutOK: true, ReplyCh: make(chan ReplyComplete, 1)},
		&MsgAssignTask{TaskID: 2, ExpectOwner: nil, NewOwner: &owner, RequireIdle: true, By: &by, ByName: "dana", ReplyCh: make(chan ReplyGetTask, 1)},
		&MsgSetTaskStartAt{TaskID: 2, StartAt: &start, ReplyCh: make(chan ReplySimple, 1)},
		&MsgAddComment{Req: domain.AddCommentRequest{TaskID: 2, Author: "a", Body: "b", AuthorUserID: &by}, ReplyCh: make(chan ReplyAddComment, 1)},
		&MsgReset{ReplyCh: make(chan ReplySimple, 1)}, // nothing but a reply channel
		&MsgPing{ReplyCh: make(chan ReplySimple, 1)},
	}
	for _, in := range cases {
		name := reflect.TypeOf(in).Elem().Name()
		t.Run(name, func(t *testing.T) {
			data, err := encodeMsg(in)
			if err != nil {
				t.Fatal(err)
			}
			out, ch, err := decodeMsg(data)
			if err != nil {
				t.Fatal(err)
			}
			if !ch.IsValid() || ch.Cap() != 1 {
				t.Fatalf("no usable reply channel: %v", ch)
			}
			// Compare with the reply channels set aside.
			a, b := reflect.ValueOf(in).Elem(), reflect.ValueOf(out).Elem()
			ac := reflect.New(a.Type()).Elem()
			ac.Set(a)
			bc := reflect.New(b.Type()).Elem()
			bc.Set(b)
			ac.FieldByName("ReplyCh").Set(reflect.Zero(ac.FieldByName("ReplyCh").Type()))
			bc.FieldByName("ReplyCh").Set(reflect.Zero(bc.FieldByName("ReplyCh").Type()))
			if !reflect.DeepEqual(ac.Interface(), bc.Interface()) {
				t.Fatalf("changed in transit:\n sent %+v\n  got %+v", ac.Interface(), bc.Interface())
			}
		})
	}
}

// Every message a caller can send a project can be forwarded.
func TestCodec_EveryOutsideMessageIsRegistered(t *testing.T) {
	for _, m := range []any{&MsgCreateEpic{}, &MsgGetEpic{}, &MsgUpdateEpic{}, &MsgCreateStory{}, &MsgGetStory{},
		&MsgUpdateStory{}, &MsgCreateTask{}, &MsgGetTask{}, &MsgListTasks{}, &MsgDispatchTask{}, &MsgCompleteTask{},
		&MsgBlockTask{}, &MsgAssignTask{}, &MsgUpdateTask{}, &MsgAddComment{}, &MsgGetComments{}, &MsgSetTaskState{},
		&MsgSetTaskStartAt{}, &MsgReset{}, &MsgDeleteEpic{}, &MsgDeleteStory{}, &MsgDeleteTask{}, &MsgSetTaskDeps{},
		&MsgReconcileDeps{}, &MsgMoveTask{}, &MsgMoveStory{}, &MsgPing{}, &MsgClaimStart{}, &MsgReturnTask{},
		&MsgSetMergeState{}, &MsgApproveTask{}} {
		if _, err := encodeMsg(m); err != nil {
			t.Errorf("%T: %v", m, err)
		}
	}
	if _, err := encodeMsg(&MsgDepCompleted{}); err == nil {
		t.Error("an actor-internal message was accepted for forwarding")
	}
}

// Replies keep their data, and errors keep what callers check them for.
func TestCodec_RepliesAndErrors(t *testing.T) {
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	owner := 4
	task := &domain.Task{ID: 5, ProjectID: 1, Name: "t", State: domain.TaskStateBlocked, OwnerUserID: &owner,
		DependencyIDs: []int{1}, BlockedByIDs: []int{1}, UpdatedAt: now, Metadata: domain.Metadata{"k": "v"}}

	roundTrip := func(t *testing.T, msg any, reply any) reflect.Value {
		t.Helper()
		data, err := encodeReply(reflect.ValueOf(reply))
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeReply(msg, data)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := roundTrip(t, &MsgGetTask{ReplyCh: make(chan ReplyGetTask, 1)}, ReplyGetTask{Task: task}).Interface().(ReplyGetTask)
	if got.Err != nil || !reflect.DeepEqual(got.Task, task) {
		t.Fatalf("task reply changed: %+v %v", got.Task, got.Err)
	}

	errs := []struct {
		err   error
		check func(error) bool
	}{
		{authz.BadRequest("clear: %q can't be cleared", "name"), func(e error) bool { return authz.StatusOf(e, 500) == 400 }},
		{authz.Forbidden("no"), func(e error) bool { return authz.StatusOf(e, 500) == 403 }},
		{fmt.Errorf("task 9: %w", repository.ErrNotFound), func(e error) bool { return errors.Is(e, repository.ErrNotFound) }},
		{fmt.Errorf("busy: %w", repository.ErrConflict), func(e error) bool { return errors.Is(e, repository.ErrConflict) }},
		{errors.New("task 3 is already completed"), func(e error) bool { return authz.StatusOf(e, 500) == 500 }},
	}
	for _, c := range errs {
		r := roundTrip(t, &MsgCompleteTask{ReplyCh: make(chan ReplyComplete, 1)}, ReplyComplete{Err: c.err}).Interface().(ReplyComplete)
		if r.Err == nil || r.Err.Error() != c.err.Error() || !c.check(r.Err) {
			t.Errorf("error %q came back as %#v", c.err, r.Err)
		}
	}
	if r := roundTrip(t, &MsgCompleteTask{ReplyCh: make(chan ReplyComplete, 1)}, ReplyComplete{}).Interface().(ReplyComplete); r.Err != nil {
		t.Errorf("success came back as %v", r.Err)
	}
}
