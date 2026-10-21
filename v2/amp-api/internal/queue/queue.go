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
	// TypeKBIndexDoc brings one doc's search index entry in line with its
	// Postgres row (the source of truth): it carries no content.
	TypeKBIndexDoc = "kb:index_doc"

	// QueueName is the single asynq queue everything here uses. A dedicated
	// name (rather than "default") makes it easy to spot in asynqmon and to
	// give it its own priority/concurrency later if other queues get added.
	QueueName = "kb-index"
)

// WriteDocPayload mirrors kb.Service.WriteDoc's arguments exactly. New code
// enqueues it only for KBs not stored in Postgres (the global KB, or a
// service with no store); for the others it is read only to drain tasks
// queued before the Postgres store existed.
type WriteDocPayload struct {
	ProjectID int    `json:"project_id"`
	Scope     string `json:"scope,omitempty"` // "" = project (tasks queued before scopes)
	ScopeID   int    `json:"scope_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// Annotations (JSON) to write with the doc; "" keeps the existing doc's.
	Annotations string   `json:"annotations,omitempty"`
	Path        string   `json:"path"`
	Title       string   `json:"title"`
	Content     string   `json:"content"`
	Author      string   `json:"author"`
	Tags        []string `json:"tags"`
}

// DeleteDocPayload mirrors kb.Service.DeleteDoc's arguments exactly.
type DeleteDocPayload struct {
	ProjectID int    `json:"project_id"`
	Scope     string `json:"scope,omitempty"`
	ScopeID   int    `json:"scope_id,omitempty"`
	Path      string `json:"path"`
}

// IndexDocPayload names the doc to (re)index. The worker reads the doc's
// current row from Postgres, so jobs applied out of order still index the
// latest version, and a deleted row removes the doc from the index.
type IndexDocPayload struct {
	ProjectID int    `json:"project_id"`
	Scope     string `json:"scope"`
	ScopeID   int    `json:"scope_id"`
	Path      string `json:"path"`
}

// RedisConnOpt builds the connection options asynq's Client and Server both
// take. password may be empty (auth disabled).
func RedisConnOpt(addr, password string) asynq.RedisClientOpt {
	return asynq.RedisClientOpt{Addr: addr, Password: password}
}
