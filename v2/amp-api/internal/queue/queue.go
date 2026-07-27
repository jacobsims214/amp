// Package queue defines the asynq task types shared between amp-api (which
// enqueues KB indexing work) and the in-process worker that actually
// performs it against Typesense. Kept deliberately tiny — task type names,
// payload shapes, and the Redis connection option builder. See
// docs/deploy-architecture.md for why this exists: a burst of KB writes
// (many agents, many projects) hitting Typesense synchronously and inline
// with the request was a real backpressure risk. Now amp-api just enqueues
// and returns immediately; a bounded-concurrency worker drains the queue at
// a steady rate.
package queue

import "github.com/hibiken/asynq"

const (
	TypeKBWriteDoc  = "kb:write_doc"
	TypeKBDeleteDoc = "kb:delete_doc"

	// QueueName is the single asynq queue everything here uses. A dedicated
	// name (rather than "default") makes it easy to spot in asynqmon and to
	// give it its own priority/concurrency later if other queues get added.
	QueueName = "kb-index"
)

// WriteDocPayload mirrors kb.Service.WriteDoc's arguments exactly.
type WriteDocPayload struct {
	ProjectID int      `json:"project_id"`
	Path      string   `json:"path"`
	Title     string   `json:"title"`
	Content   string   `json:"content"`
	Author    string   `json:"author"`
	Tags      []string `json:"tags"`
}

// DeleteDocPayload mirrors kb.Service.DeleteDoc's arguments exactly.
type DeleteDocPayload struct {
	ProjectID int    `json:"project_id"`
	Path      string `json:"path"`
}

// RedisConnOpt builds the connection options asynq's Client and Server both
// take. password may be empty (auth disabled).
func RedisConnOpt(addr, password string) asynq.RedisClientOpt {
	return asynq.RedisClientOpt{Addr: addr, Password: password}
}
