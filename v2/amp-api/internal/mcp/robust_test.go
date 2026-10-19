package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/simstech/amp-api/internal/actor"
)

func call(name string) mcpgo.CallToolRequest {
	var r mcpgo.CallToolRequest
	r.Params.Name = name
	return r
}

// A handler that ignores its context and never returns still gets the
// caller an answer by the deadline, worded by whether a retry is safe.
func TestMiddleware_DeadlineAnswersEvenIfTheHandlerHangs(t *testing.T) {
	toolDeadlines["amp_test_write"] = 50 * time.Millisecond
	toolDeadlines["amp_get_test"] = 50 * time.Millisecond
	defer delete(toolDeadlines, "amp_test_write")
	defer delete(toolDeadlines, "amp_get_test")
	hang := Middleware()(func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		select {} // ignores ctx entirely
	})
	start := time.Now()
	_, err := hang(context.Background(), call("amp_test_write"))
	if took := time.Since(start); took > time.Second || err == nil || !strings.Contains(err.Error(), "may still complete") {
		t.Fatalf("write: took %s, err %v", took, err)
	}
	_, err = hang(context.Background(), call("amp_get_test"))
	if err == nil || !strings.Contains(err.Error(), "retry it once") {
		t.Fatalf("read: err %v", err)
	}
}

// A handler waiting on an actor that never replies gets a reply with Err set
// at the deadline, which the middleware reports as a timeout.
func TestMiddleware_StalledActorBecomesATimeout(t *testing.T) {
	toolDeadlines["amp_test_actor"] = 50 * time.Millisecond
	defer delete(toolDeadlines, "amp_test_actor")
	h := Middleware()(func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		never := make(chan actor.ReplySimple) // nobody ever replies
		if r := actor.Await(ctx, never); r.Err != nil {
			return nil, r.Err
		}
		return nil, nil
	})
	_, err := h(context.Background(), call("amp_test_actor"))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err %v", err)
	}
}

func TestMiddleware_PanicBecomesAnError(t *testing.T) {
	h := Middleware()(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) { panic("boom") })
	if _, err := h(context.Background(), call("amp_whoami")); err == nil || !strings.Contains(err.Error(), "internal error") {
		t.Fatalf("err %v", err)
	}
}

// The client going away isn't reported as AMP timing out.
func TestMiddleware_ClientCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := Middleware()(func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := h(ctx, call("amp_whoami")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}
