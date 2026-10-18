package actor

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAwait(t *testing.T) {
	ch := make(chan ReplySimple, 1)
	ch <- ReplySimple{}
	if r := Await(context.Background(), ch); r.Err != nil {
		t.Fatalf("delivered reply: %v", r.Err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	r := Await(ctx, make(chan ReplyGetTask))
	if !errors.Is(r.Err, ErrNoReply) || r.Task != nil {
		t.Fatalf("stalled actor: %+v", r)
	}
}
