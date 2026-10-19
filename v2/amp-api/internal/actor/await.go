package actor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
)

// ErrNoReply is wrapped into a reply's Err when the caller's context ends
// before the actor answers (a deadline, or the client went away).
var ErrNoReply = errors.New("AMP didn't finish in time")

// Await waits for an actor's reply, or for ctx to end. Every reply type
// carries an Err field; when ctx ends first, Await returns a reply with Err
// set (wrapping ErrNoReply and ctx's error), so callers' existing
// `if reply.Err != nil` handling covers timeouts and cancellations. The
// actor may still process the message afterwards: a write that timed out
// may have happened.
func Await[T any](ctx context.Context, ch <-chan T) T {
	select {
	case r := <-ch:
		return r
	case <-ctx.Done():
		var zero T
		v := reflect.ValueOf(&zero).Elem()
		if f := v.FieldByName("Err"); f.IsValid() && f.CanSet() && f.Type() == reflect.TypeOf((*error)(nil)).Elem() {
			f.Set(reflect.ValueOf(fmt.Errorf("%w (%v): it may still complete, so check before retrying a change", ErrNoReply, ctx.Err())))
		}
		return zero
	}
}
