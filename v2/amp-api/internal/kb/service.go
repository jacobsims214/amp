// Package kb manages the knowledge bases backed by Typesense: one per
// project, plus one per team and one per org for knowledge shared across
// projects (see docs/kb-scopes-design.md).
//
// Architecture:
//   - One Typesense collection per KB, reached by its name: "kb_{project_id}", "kb_team_{id}", "kb_org_{id}".
//     After its first reindex a stored KB's name is an alias of a versioned
//     collection ("kb_5_v<n>"), swapped to a new build on each reindex.
//   - Documents are chunked (~500 tokens) for precise retrieval
//   - Typesense auto-embeds via Ollama at index AND query time (no vectors in Go code)
//   - Hybrid search (keyword + semantic) with rank fusion
//   - Collections are created on first write (no explicit init step)
//
// Agents use the amp_kb_* MCP tools. The UI uses the REST endpoints.
package kb

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hibiken/asynq"
	"github.com/typesense/typesense-go/v4/typesense"
	"github.com/typesense/typesense-go/v4/typesense/api"
	"github.com/typesense/typesense-go/v4/typesense/api/pointer"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/queue"
	"github.com/simstech/amp-api/internal/repository"
)

// Service is the KB business logic layer.
type Service struct {
	client       *typesense.Client
	typesenseURL string
	typesenseKey string
	ollamaURL    string
	ollamaModel  string
	log          *slog.Logger
	queueClient  *asynq.Client // nil = write/delete synchronously; set via SetQueueClient

	// store holds project, team and org docs in Postgres, the source of
	// truth (decisions/kb-postgres-source-of-truth.md); Typesense is only
	// their search index. nil (tools and tests without Postgres) keeps the
	// old Typesense-only behaviour. The global KB never uses it.
	store Store

	// Reindex is single-flight per KB: reindexing marks the collections
	// being reindexed in this process, and reindexLock (optional, set via
	// SetReindexLock) holds a lock shared by every replica.
	reindexMu   sync.Mutex
	reindexing  map[string]bool
	reindexLock ReindexLocker

	// reindexHook, when set (tests only), is called at each stage of a
	// stored KB's reindex: "built", "verified" (just before the swap) and
	// "swapped".
	reindexHook func(ctx context.Context, stage string)

	// reconcileOnly, when set (tests only), limits ReconcileAll to the KBs
	// it returns true for.
	reconcileOnly func(Scope) bool

	// backfillOnly, when set (tests only), limits BackfillAll to the KBs
	// it returns true for.
	backfillOnly func(Scope) bool
}

// ErrReindexRunning is returned when a reindex of the same KB is already
// running, in this process or (with a ReindexLocker) on another replica.
var ErrReindexRunning = errors.New("a reindex of this knowledge base is already running")

// ErrReindexRefused is returned when a reindex of a stored KB would drop
// docs: the index holds docs Postgres has no row for, so a collection built
// from Postgres would lack them and swapping it in would lose their only
// copy. Nothing is changed.
var ErrReindexRefused = errors.New("reindex refused: the search index holds docs that Postgres has not got")

// ReindexLocker takes a lock named key that every replica shares (e.g. a
// Postgres advisory lock) without waiting. ok is false when someone else
// holds it; release frees it and is only set when ok is true.
type ReindexLocker func(ctx context.Context, key string) (release func(), ok bool, err error)

// New creates a KB service connected to Typesense.
// ollamaURL e.g. "http://ollama:11434" — used for auto-embedding.
// If empty, semantic search is disabled and keyword-only search is used.
func New(typesenseURL, typesenseKey, ollamaURL, ollamaModel string) *Service {
	client := typesense.NewClient(
		typesense.WithServer(typesenseURL),
		typesense.WithAPIKey(typesenseKey),
		typesense.WithConnectionTimeout(10*time.Second),
		typesense.WithRetryInterval(100*time.Millisecond),
		typesense.WithNumRetries(2),
	)
	if ollamaModel == "" {
		ollamaModel = "nomic-embed-text"
	}
	return &Service{
		client:       client,
		typesenseURL: typesenseURL,
		typesenseKey: typesenseKey,
		ollamaURL:    ollamaURL,
		ollamaModel:  ollamaModel,
		log:          slog.Default().With("component", "kb"),
	}
}

// ---- Public domain types ----

type Doc struct {
	ID        string `json:"id"`         // sha256(project_id:path)
	ProjectID int    `json:"project_id"` // 0 in team and org KBs
	Scope     string `json:"scope"`
	ScopeID   int    `json:"scope_id"`
	// Reason records why a doc was placed in a team or org KB rather than a
	// project's.
	Reason      string       `json:"reason,omitempty"`
	Path        string       `json:"path"` // e.g. "architecture/auth.md"
	Title       string       `json:"title"`
	Content     string       `json:"content"` // full raw markdown
	Tags        []string     `json:"tags"`
	Author      string       `json:"author"`
	ChunkIndex  int          `json:"chunk_index"` // 0 for the canonical doc record
	ChunkText   string       `json:"chunk_text"`  // the searchable portion
	UpdatedAt   int64        `json:"updated_at"`  // unix timestamp
	Annotations []Annotation `json:"annotations"`
}

type Annotation = domain.KBAnnotation

type DocSummary struct {
	ID        string   `json:"id"`
	ProjectID int      `json:"project_id"`
	Scope     string   `json:"scope"`
	ScopeID   int      `json:"scope_id"`
	Reason    string   `json:"reason,omitempty"`
	Path      string   `json:"path"`
	Title     string   `json:"title"`
	Tags      []string `json:"tags"`
	Author    string   `json:"author"`
	UpdatedAt int64    `json:"updated_at"`
}

type SearchResult struct {
	Scope     string   `json:"scope"`
	ScopeID   int      `json:"scope_id"`
	Path      string   `json:"path"`
	Title     string   `json:"title"`
	Tags      []string `json:"tags"`
	Excerpt   string   `json:"excerpt"` // best-matching chunk_text, trimmed
	Author    string   `json:"author"`
	UpdatedAt int64    `json:"updated_at"`
	// Score is the hit's relevance in [0,1], highest first. On a hybrid
	// search it is Typesense's rank-fusion score (the value results are
	// ordered by); on a keyword-only collection it is the reciprocal of the
	// hit's rank. It is never the raw text_match.
	Score float64 `json:"score"`
	// VectorScore is the semantic similarity (1 - vector distance, clamped
	// to [0,1]) when the hit came back from the vector half of a hybrid
	// search; absent otherwise.
	VectorScore *float64 `json:"vector_score,omitempty"`
	// KeywordMatch is true when the hit matched the query's keywords.
	KeywordMatch     bool   `json:"keyword_match,omitempty"`
	AnnotationCount  int    `json:"annotation_count"`
	LatestAnnotation string `json:"latest_annotation"`
	RecencyLabel     string `json:"recency_label"`
}

type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// StatusInfo holds health metrics for the knowledge base.
type StatusInfo struct {
	TotalDocs             int            `json:"total_docs"`
	StaleDocs             int            `json:"stale_docs"`
	DocsWithAnnotations   int            `json:"docs_with_annotations"`
	TotalAnnotations      int            `json:"total_annotations"`
	UnresolvedAnnotations int            `json:"unresolved_annotations"`
	DocsByTag             map[string]int `json:"docs_by_tag"`

	// StoredDocs is the number of live Postgres rows (kb_documents), the
	// source of truth for project, team and org KBs. Absent for the global
	// KB and a service with no store, which keep docs in the index only
	// (Note says so).
	StoredDocs *int `json:"stored_docs,omitempty"`
	// IndexedDocs is the number of docs (chunk-0 records) in the search
	// index, counted by Typesense itself, so it is exact beyond one page.
	// Absent when the index can't be reached; IndexError says why.
	IndexedDocs *int   `json:"indexed_docs,omitempty"`
	IndexError  string `json:"index_error,omitempty"`
	// Backfill is the KB's one-time copy from Typesense into Postgres
	// (kb_backfills). Absent where StoredDocs is.
	Backfill *BackfillState `json:"backfill,omitempty"`
	Note     string         `json:"note,omitempty"`
}

// Backfill states reported by KBStatus.
const (
	BackfillComplete   = "complete"    // completed_at is set: Postgres holds every doc
	BackfillFailed     = "failed"      // the last attempt failed; the next startup retries
	BackfillStarted    = "started"     // begun, not finished (running now, or the pod stopped)
	BackfillNotStarted = "not_started" // no kb_backfills row yet
)

// BackfillState is a KB's backfill record, as KBStatus reports it. Times
// are unix seconds, 0 (omitted) when unset.
type BackfillState struct {
	Status      string `json:"status"`
	StartedAt   int64  `json:"started_at,omitempty"`
	CompletedAt int64  `json:"completed_at,omitempty"`
	DocCount    int    `json:"doc_count"`
	LastError   string `json:"last_error,omitempty"`
}

// backfillStateOf turns a kb_backfills row (nil = none) into a BackfillState.
func backfillStateOf(b *repository.KBBackfill) *BackfillState {
	if b == nil {
		return &BackfillState{Status: BackfillNotStarted}
	}
	st := &BackfillState{DocCount: b.DocCount, LastError: b.LastError}
	if !b.StartedAt.IsZero() {
		st.StartedAt = b.StartedAt.Unix()
	}
	switch {
	case b.CompletedAt != nil:
		st.Status = BackfillComplete
		st.CompletedAt = b.CompletedAt.Unix()
	case b.LastError != "":
		st.Status = BackfillFailed
	default:
		st.Status = BackfillStarted
	}
	return st
}

// ---- Scopes ----

// Scope kinds: which knowledge base a call is about.
const (
	ScopeProject = "project"
	ScopeTeam    = "team"
	ScopeOrg     = "org"
	// ScopeGlobal is the AMP guide: docs shipped with AMP itself (v2/kb in
	// the repo), readable by everyone and written only by the startup sync.
	ScopeGlobal = "global"
)

// Scope names one knowledge base: a project's, a team's, or an org's.
type Scope struct {
	Kind string
	ID   int
}

func Project(id int) Scope { return Scope{Kind: ScopeProject, ID: id} }
func Team(id int) Scope    { return Scope{Kind: ScopeTeam, ID: id} }
func Org(id int) Scope     { return Scope{Kind: ScopeOrg, ID: id} }
func Global() Scope        { return Scope{Kind: ScopeGlobal} }

func (sc Scope) String() string { return fmt.Sprintf("%s %d", sc.Kind, sc.ID) }

// projectID is the value stored in documents' project_id field: the
// project for a project KB, 0 for team and org KBs.
func (sc Scope) projectID() int {
	if sc.Kind == ScopeProject {
		return sc.ID
	}
	return 0
}

// ---- Collection naming ----

// collectionName and docID keep the original project naming, so existing
// project KBs are read and written exactly as before.
func collectionName(sc Scope) string {
	if sc.Kind == ScopeProject {
		return fmt.Sprintf("kb_%d", sc.ID)
	}
	return fmt.Sprintf("kb_%s_%d", sc.Kind, sc.ID)
}

func docID(sc Scope, path string, chunkIndex int) string {
	key := fmt.Sprintf("%d:%s:%d", sc.ID, path, chunkIndex)
	if sc.Kind != ScopeProject {
		key = sc.Kind + ":" + key
	}
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", h[:8]) // 16 hex chars, short and stable
}

// ---- Collection bootstrap ----

// ensureCollection creates the Typesense collection for a KB if its name
// resolves to nothing: neither a collection nor an alias (a reindexed stored
// KB is reached through an alias, see "Collections and the alias"). Safe to
// call on every write: it checks first, and only a 404 creates one, so a
// transient error never creates a collection that would shadow the alias.
func (s *Service) ensureCollection(ctx context.Context, sc Scope) error {
	name := collectionName(sc)

	_, err := s.client.Collection(name).Retrieve(ctx)
	if err == nil {
		return nil // already exists (a collection, or the alias's target)
	}
	if !isNotFound(err) {
		return fmt.Errorf("check collection %s: %w", name, err)
	}

	_, err = s.client.Collections().Create(ctx, s.collectionSchema(ctx, name))
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return nil // created concurrently
		}
		return fmt.Errorf("create collection %s: %w", name, err)
	}
	s.log.Info("created KB collection", "kb", sc.String(), "collection", name, "semantic", s.ollamaURL != "")
	s.unshadowAlias(ctx, name)
	return nil
}

// unshadowAlias handles a collection just created under a KB's base name
// while an alias of that name appeared (a reindex swapped one in between
// the 404 and the create). Typesense lets a collection and an alias share a
// name, and the collection wins every lookup, so it would hide the reindexed
// collection. If the alias's target exists, the new collection goes (it holds
// at most docs whose rows are in Postgres); if the alias dangles, the alias
// goes and the new collection serves.
func (s *Service) unshadowAlias(ctx context.Context, name string) {
	a, err := s.client.Alias(name).Retrieve(ctx)
	if err != nil {
		return // no alias (the normal case)
	}
	if _, err := s.client.Collection(a.CollectionName).Retrieve(ctx); err == nil {
		if _, err := s.client.Collection(name).Delete(ctx); err != nil && !isNotFound(err) {
			s.log.Error("a collection shadows the KB's alias and could not be removed", "collection", name, "alias_target", a.CollectionName, "err", err)
		} else {
			s.log.Warn("removed a collection that shadowed the KB's alias", "collection", name, "alias_target", a.CollectionName)
		}
		return
	}
	if _, err := s.client.Alias(name).Delete(ctx); err != nil && !isNotFound(err) {
		s.log.Warn("could not remove a dangling KB alias", "alias", name, "err", err)
	}
}

// collectionSchema is the schema of a KB collection named name.
func (s *Service) collectionSchema(ctx context.Context, name string) *api.CollectionSchema {
	schema := &api.CollectionSchema{
		Name: name,
		Fields: []api.Field{
			{Name: "project_id", Type: "int32", Facet: pointer.True()},
			{Name: "path", Type: "string", Facet: pointer.True()},
			{Name: "title", Type: "string"},
			{Name: "tags", Type: "string[]", Facet: pointer.True()},
			{Name: "author", Type: "string", Facet: pointer.True()},
			{Name: "chunk_index", Type: "int32"},
			{Name: "chunk_text", Type: "string"},
			// content is stored but NOT indexed — large, not needed for search
			{Name: "content", Type: "string", Index: pointer.False()},
			// sort by recency
			{Name: "updated_at", Type: "int64", Sort: pointer.True()},
			// annotations stored as JSON string on chunk_index=0; not indexed because annotations are returned with doc reads, not searched directly
			{Name: "annotations", Type: "string", Index: pointer.False()},
		},
	}

	// Wire up semantic embedding via Ollama if configured and reachable.
	if s.ollamaURL != "" && s.ollamaReachable(ctx) {
		ollamaEmbedURL := s.ollamaURL + "/v1/embeddings"
		schema.Fields = append(schema.Fields, api.Field{
			Name: "embedding",
			Type: "float[]",
			Embed: &api.FieldEmbed{
				From: []string{"title", "chunk_text"},
				ModelConfig: struct {
					AccessToken    *string `json:"access_token,omitempty"`
					ApiKey         *string `json:"api_key,omitempty"`
					ClientId       *string `json:"client_id,omitempty"`
					ClientSecret   *string `json:"client_secret,omitempty"`
					IndexingPrefix *string `json:"indexing_prefix,omitempty"`
					ModelName      string  `json:"model_name"`
					ProjectId      *string `json:"project_id,omitempty"`
					QueryPrefix    *string `json:"query_prefix,omitempty"`
					RefreshToken   *string `json:"refresh_token,omitempty"`
					Url            *string `json:"url,omitempty"`
				}{
					// Must start with "openai/" for Typesense to use the OpenAI-compatible path.
					ModelName: "openai/" + s.ollamaModel,
					ApiKey:    pointer.String("ignored"),
					Url:       pointer.String(ollamaEmbedURL),
				},
			},
		})
		s.log.Info("semantic search enabled via Ollama", "url", ollamaEmbedURL, "model", s.ollamaModel)
	} else if s.ollamaURL != "" {
		s.log.Warn("Ollama not reachable — creating collection with keyword search only. Run 'make kb-setup' to enable semantic search.", "url", s.ollamaURL)
	}
	return schema
}

// ---- Write ----

// WriteDoc upserts a document into the KB.
// For project, team and org KBs with a store set, the doc is committed to
// Postgres first and then indexed from the committed row (queued, or now
// without a queue); an indexing failure after the commit is logged and the
// saved doc is still returned. The global KB (and a service with no store)
// is written to Typesense only.
// The doc is chunked automatically; Typesense generates embeddings via Ollama.
// A rewrite upserts the new chunks, then deletes chunks the new version no
// longer has.
//
// reason says why a doc belongs in a team or org KB; it is stored with the doc.
//
// Rewriting a doc keeps its annotations. annotations, when non-empty, are
// written as the doc's annotations instead (reindex and import carry them
// over this way); when empty, the existing doc's annotations are kept.
func (s *Service) WriteDoc(ctx context.Context, sc Scope, path, title, content, author string, tags []string, reason string, annotations []Annotation) (*DocSummary, error) {
	if tags == nil {
		tags = []string{}
	}
	if s.stored(sc) {
		return s.writeStored(ctx, sc, path, title, content, author, tags, reason, annotations)
	}

	// Deterministic — computed the same way whether we write now or later,
	// so callers get an immediate, correct summary either way.
	summary := &DocSummary{
		ID:        docID(sc, path, 0),
		ProjectID: sc.projectID(),
		Scope:     sc.Kind,
		ScopeID:   sc.ID,
		Reason:    reason,
		Path:      path,
		Title:     title,
		Tags:      tags,
		Author:    author,
		UpdatedAt: time.Now().Unix(),
	}

	if s.queueClient != nil {
		payload, err := json.Marshal(queue.WriteDocPayload{
			ProjectID: sc.projectID(), Scope: sc.Kind, ScopeID: sc.ID,
			Path: path, Title: title, Content: content, Author: author, Tags: tags, Reason: reason,
			Annotations: annotationsJSON(annotations),
		})
		if err == nil {
			task := asynq.NewTask(queue.TypeKBWriteDoc, payload)
			if _, err := s.queueClient.EnqueueContext(ctx, task, asynq.Queue(queue.QueueName)); err == nil {
				s.log.Info("doc write enqueued", "kb", sc.String(), "path", path)
				return summary, nil
			} else {
				s.log.Warn("enqueue failed, writing synchronously instead", "kb", sc.String(), "path", path, "err", err)
			}
		} else {
			s.log.Warn("marshal enqueue payload failed, writing synchronously instead", "err", err)
		}
	}

	if _, err := s.writeDocNow(ctx, sc, path, title, content, author, tags, reason, annotations); err != nil {
		return nil, err
	}
	return summary, nil
}

// annotationsJSON encodes annotations for a queued write ("" for none).
func annotationsJSON(anns []Annotation) string {
	if len(anns) == 0 {
		return ""
	}
	b, err := json.Marshal(anns)
	if err != nil {
		return ""
	}
	return string(b)
}

// writeDocNow performs the actual Typesense indexing. Called either
// directly (no queue configured — local dev without Valkey, or enqueue
// itself failed) or by the asynq worker via HandleWriteDocTask.
func (s *Service) writeDocNow(ctx context.Context, sc Scope, path, title, content, author string, tags []string, reason string, annotations []Annotation) (*DocSummary, error) {
	now := time.Now().Unix()
	if tags == nil {
		tags = []string{}
	}

	// A rewrite keeps the doc's annotations. Only a doc that isn't there
	// has none: any other read error stops the write rather than dropping
	// them.
	if len(annotations) == 0 {
		old, err := s.GetDoc(ctx, sc, path)
		switch {
		case err == nil:
			annotations = old.Annotations
		case !isNotFound(err):
			return nil, fmt.Errorf("read annotations of %s before rewriting it: %w", path, err)
		}
	}
	chunks, err := s.indexChunks(ctx, sc, indexedDoc{
		path: path, title: title, content: content, author: author, tags: tags, reason: reason,
		annotations: annotations, updatedAt: now,
	})
	if err != nil {
		return nil, err
	}
	s.log.Info("doc written", "kb", sc.String(), "path", path, "chunks", chunks)

	return &DocSummary{
		ID:        docID(sc, path, 0),
		ProjectID: sc.projectID(),
		Scope:     sc.Kind,
		ScopeID:   sc.ID,
		Reason:    reason,
		Path:      path,
		Title:     title,
		Tags:      tags,
		Author:    author,
		UpdatedAt: now,
	}, nil
}

// ---- Read ----

// GetDoc retrieves a document by path, returning the full content (chunk_index=0).
// A stored KB (project, team, org) reads Postgres, so a doc reads back right
// after it is written; until the KB's backfill completes, a doc only the
// index has yet is read from the index (see storedRead). The global KB reads
// the index.
func (s *Service) GetDoc(ctx context.Context, sc Scope, path string) (*Doc, error) {
	if !s.stored(sc) {
		return s.getIndexed(ctx, sc, path)
	}
	rs := storeScope(sc)
	d, err := s.store.GetKBDocument(ctx, rs, path)
	if err == nil {
		return docFromRow(sc, d), nil
	}
	if !errors.Is(err, repository.ErrNotFound) {
		return nil, fmt.Errorf("read %s/%s: %w", collectionName(sc), path, err)
	}
	notFound := fmt.Errorf("doc not found: %s/%s: %w", collectionName(sc), path, err)
	done, deleted, err := s.storedRead(ctx, sc)
	if err != nil {
		return nil, err
	}
	if done || deleted[path] {
		return nil, notFound
	}
	doc, err := s.getIndexed(ctx, sc, path)
	if err != nil {
		if isNotFound(err) {
			return nil, notFound
		}
		return nil, err
	}
	return doc, nil
}

// storedRead tells a read of a stored KB whether Postgres alone answers it
// (the KB's backfill has completed). Until then, docs written before the
// Postgres store existed may be only in the index, and a read merges them
// in, skipping the paths in deleted: those were deleted in Postgres, and an
// index entry for one is stale (its removal failed or hasn't run yet).
func (s *Service) storedRead(ctx context.Context, sc Scope) (done bool, deleted map[string]bool, err error) {
	rs := storeScope(sc)
	if done, err = s.backfillDone(ctx, sc); err != nil || done {
		return done, nil, err
	}
	paths, err := s.store.ListDeletedKBPaths(ctx, rs)
	if err != nil {
		return false, nil, err
	}
	deleted = make(map[string]bool, len(paths))
	for _, p := range paths {
		deleted[p] = true
	}
	return false, deleted, nil
}

// getIndexed reads a doc's canonical record (chunk 0) from the index.
func (s *Service) getIndexed(ctx context.Context, sc Scope, path string) (*Doc, error) {
	col := collectionName(sc)
	id := docID(sc, path, 0)

	raw, err := s.client.Collection(col).Document(id).Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("doc not found: %s/%s: %w", col, path, err)
	}
	doc := mapToDoc(raw)
	doc.Scope, doc.ScopeID = sc.Kind, sc.ID
	return doc, nil
}

// AnnotateDoc adds an annotation to a document.
// The upsert only updates the annotations field on chunk_index=0.
// Typesense will merge the fields.
func (s *Service) AnnotateDoc(ctx context.Context, sc Scope, path, text, author string) (*Annotation, error) {
	ann := &Annotation{
		Author:     author,
		Text:       text,
		CreatedAt:  time.Now().Unix(),
		IsResolved: false,
	}
	if s.stored(sc) {
		if _, err := s.updateStoredAnnotations(ctx, sc, path, func(anns []Annotation) ([]Annotation, error) {
			return append(anns, *ann), nil
		}); err != nil {
			return nil, err
		}
		return ann, nil
	}
	doc, err := s.GetDoc(ctx, sc, path)
	if err != nil {
		return nil, err
	}
	doc.Annotations = append(doc.Annotations, *ann)
	if err := s.saveAnnotations(ctx, sc, doc); err != nil {
		return nil, fmt.Errorf("annotate doc %s: %w", path, err)
	}
	return ann, nil
}

// ResolveAnnotation marks a doc's annotation resolved (worked into the doc)
// or, with resolved false, reopens it. index is the annotation's position in
// the doc's annotations, oldest first, as amp_kb_get returns them;
// annotations are only ever appended, so positions don't change.
func (s *Service) ResolveAnnotation(ctx context.Context, sc Scope, path string, index int, resolved bool, by string) (*Doc, error) {
	if s.stored(sc) {
		return s.updateStoredAnnotations(ctx, sc, path, func(anns []Annotation) ([]Annotation, error) {
			return anns, resolveAt(anns, path, index, resolved, by)
		})
	}
	doc, err := s.GetDoc(ctx, sc, path)
	if err != nil {
		return nil, err
	}
	if err := resolveAt(doc.Annotations, path, index, resolved, by); err != nil {
		return nil, err
	}
	if err := s.saveAnnotations(ctx, sc, doc); err != nil {
		return nil, fmt.Errorf("resolve annotation on %s: %w", path, err)
	}
	return doc, nil
}

// resolveAt marks anns[index] resolved by by, or reopens it.
func resolveAt(anns []Annotation, path string, index int, resolved bool, by string) error {
	if index < 0 || index >= len(anns) {
		return fmt.Errorf("%s has %d annotations; index %d is out of range (0-based, oldest first)", path, len(anns), index)
	}
	a := &anns[index]
	a.IsResolved = resolved
	if resolved {
		a.ResolvedBy, a.ResolvedAt = by, time.Now().Unix()
	} else {
		a.ResolvedBy, a.ResolvedAt = "", 0
	}
	return nil
}

// saveAnnotations stores doc's annotations on its first chunk and the
// preview fields on every chunk, so search hits from any chunk show them.
func (s *Service) saveAnnotations(ctx context.Context, sc Scope, doc *Doc) error {
	col := collectionName(sc)
	// Marshal annotations to JSON string for Typesense schema compliance
	annJSON, _ := json.Marshal(doc.Annotations)
	_, err := s.client.Collection(col).Documents().Upsert(ctx, map[string]interface{}{
		"id":                doc.ID,
		"project_id":        doc.ProjectID,
		"reason":            doc.Reason,
		"path":              doc.Path,
		"title":             doc.Title,
		"content":           doc.Content,
		"tags":              doc.Tags,
		"author":            doc.Author,
		"chunk_index":       0,
		"chunk_text":        doc.ChunkText,
		"updated_at":        doc.UpdatedAt,
		"annotations":       string(annJSON),
		"annotation_count":  len(doc.Annotations),
		"latest_annotation": getLatestAnnotation(doc.Annotations),
	}, &api.DocumentIndexParameters{})
	if err != nil {
		return err
	}

	// Propagate annotation count and latest annotation to all other chunks
	// so search results from any chunk show annotation data.
	count := len(doc.Annotations)
	latest := getLatestAnnotation(doc.Annotations)
	propFilter := fmt.Sprintf("path:=%s && chunk_index:>0", quoteFilter(doc.Path))
	_, _ = s.client.Collection(col).Documents().Update(ctx, map[string]interface{}{
		"annotation_count":  count,
		"latest_annotation": latest,
	}, &api.UpdateDocumentsParams{
		FilterBy: &propFilter,
	})
	return nil
}

// ListDocs returns summaries of all documents (no content), most recently
// updated first. Optionally filtered by tag. A stored KB lists Postgres
// (merged with index-only docs until its backfill completes); the global KB
// lists the index.
func (s *Service) ListDocs(ctx context.Context, sc Scope, tag string) ([]DocSummary, error) {
	var docs []*Doc
	var err error
	if s.stored(sc) {
		docs, err = s.listStored(ctx, sc, tag)
	} else {
		if err := s.ensureCollection(ctx, sc); err != nil {
			return nil, err
		}
		docs, err = s.listIndexed(ctx, sc, tag)
	}
	if err != nil {
		return nil, err
	}
	out := make([]DocSummary, 0, len(docs))
	for _, d := range docs {
		out = append(out, DocSummary{
			ID: d.ID, ProjectID: d.ProjectID, Scope: sc.Kind, ScopeID: sc.ID, Reason: d.Reason,
			Path: d.Path, Title: d.Title, Tags: d.Tags, Author: d.Author, UpdatedAt: d.UpdatedAt,
		})
	}
	return out, nil
}

// listStored lists a stored KB's live docs with their annotations, most
// recently updated first. Before the backfill completes, docs only the index
// has are merged in (Postgres wins for a path both have).
func (s *Service) listStored(ctx context.Context, sc Scope, tag string) ([]*Doc, error) {
	rows, err := s.store.ListKBDocuments(ctx, storeScope(sc), 0, 0)
	if err != nil {
		return nil, fmt.Errorf("list docs: %w", err)
	}
	out := make([]*Doc, 0, len(rows))
	have := make(map[string]bool, len(rows))
	for i := range rows {
		have[rows[i].Path] = true
		if tag != "" && !slices.Contains(rows[i].Tags, tag) {
			continue
		}
		d := docFromRow(sc, &rows[i])
		d.Content, d.ChunkText = "", "" // a listing carries no content, as from the index
		out = append(out, d)
	}
	done, deleted, err := s.storedRead(ctx, sc)
	if err != nil {
		return nil, err
	}
	if done {
		return out, nil
	}
	indexed, err := s.listIndexed(ctx, sc, tag)
	if err != nil {
		return nil, err
	}
	merged := false
	for _, d := range indexed {
		if have[d.Path] || deleted[d.Path] {
			continue
		}
		out = append(out, d)
		merged = true
	}
	if merged {
		sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	}
	return out, nil
}

// listIndexed lists the docs the index holds (chunk 0 of each, with
// annotations but without content), most recently updated first, page by
// page so a KB of any size is listed whole. A collection that doesn't exist
// yet lists empty.
func (s *Service) listIndexed(ctx context.Context, sc Scope, tag string) ([]*Doc, error) {
	col := collectionName(sc)
	const perPage = 250
	filterBy := "chunk_index:=0"
	if tag != "" {
		filterBy += " && tags:=" + quoteFilter(tag)
	}
	out := make([]*Doc, 0)
	seen := map[string]bool{}
	for page := 1; ; page++ {
		q, pp, pg := "*", perPage, page
		results, err := s.client.Collection(col).Documents().Search(ctx, &api.SearchCollectionParams{
			Q:             &q,
			QueryBy:       pointer.String("title"),
			FilterBy:      pointer.String(filterBy),
			SortBy:        pointer.String("updated_at:desc"),
			PerPage:       &pp,
			Page:          &pg,
			ExcludeFields: pointer.String("content,embedding,chunk_text"),
		})
		if err != nil {
			if isNotFound(err) {
				return out, nil
			}
			return nil, fmt.Errorf("list docs: %w", err)
		}
		if results.Hits == nil {
			return out, nil
		}
		for _, hit := range *results.Hits {
			if hit.Document == nil {
				continue
			}
			doc := mapToDoc(hit.Document)
			// A write between pages can shift a doc onto the next page.
			if seen[doc.Path] {
				continue
			}
			seen[doc.Path] = true
			doc.Scope, doc.ScopeID = sc.Kind, sc.ID
			out = append(out, doc)
		}
		if len(*results.Hits) < perPage {
			return out, nil
		}
	}
}

// computeRecencyLabel returns a human-readable label based on how recent the document is.
func computeRecencyLabel(t time.Time) string {
	days := int(time.Since(t).Hours() / 24)
	switch {
	case days <= 1:
		return "today"
	case days <= 7:
		return "this week"
	case days <= 30:
		return "this month"
	case days <= 365:
		months := int(time.Since(t).Hours() / 24 / 30)
		return fmt.Sprintf("%d months ago", months)
	default:
		return "over 1 year"
	}
}

// ---- Search ----

// Search performs hybrid keyword+semantic search scoped to a project.
// Returns up to limit results (default 3). Each result is one chunk.
// Excerpts are capped at 120 chars to keep MCP responses small for local LLMs.
// recencyBoost controls secondary sorting by updated_at (higher = more weight to recency).
// minRecencyDays filters out documents older than N days (0 = no filter).
func (s *Service) Search(ctx context.Context, sc Scope, query string, tags []string, limit int, recencyBoost float64, minRecencyDays int) ([]SearchResult, error) {
	if err := s.ensureCollection(ctx, sc); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 3 // small default — agents call this from limited-context models
	}

	col := collectionName(sc)

	filterBy := "chunk_index:>=0" // include all chunks
	if len(tags) > 0 {
		filterBy += fmt.Sprintf(" && tags:=[%s]", strings.Join(tags, ","))
	}

	// Add recency filter if minRecencyDays > 0
	if minRecencyDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -minRecencyDays).Unix()
		filterBy += fmt.Sprintf(" && updated_at:>=%d", cutoff)
	}

	// Build SortBy - include updated_at:desc as secondary sort when recencyBoost > 0
	sortBy := "_text_match:desc"
	if recencyBoost > 0 {
		sortBy += ",updated_at:desc"
	}

	hasEmbedding := s.collectionHasEmbedding(ctx, collectionName(sc))

	// When using a remote embedder (Ollama), include embedding in query_by
	// and set prefix=false (required for remote embedders).
	var queryBy string
	var prefix *string
	var vectorQuery *string

	if hasEmbedding {
		queryBy = "title,chunk_text,embedding"
		// Remote embedders require prefix=false
		p := "false"
		prefix = &p
		// Empty vector list → Typesense embeds the query text via Ollama
		vq := fmt.Sprintf("embedding:([], k:%d, distance_threshold:0.90)", limit*3)
		vectorQuery = &vq
	} else {
		queryBy = "title,chunk_text"
	}

	results, err := s.client.Collection(col).Documents().Search(ctx, &api.SearchCollectionParams{
		Q:             &query,
		QueryBy:       &queryBy,
		FilterBy:      pointer.String(filterBy),
		SortBy:        pointer.String(sortBy),
		VectorQuery:   vectorQuery,
		Prefix:        prefix,
		PerPage:       &limit,
		ExcludeFields: pointer.String("embedding,content"),
	})
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	out := make([]SearchResult, 0)
	if results.Hits == nil {
		return out, nil
	}
	prevScore := 1.0
	for rank, hit := range *results.Hits {
		if hit.Document == nil {
			continue
		}
		doc := mapToDoc(hit.Document)
		hs := scoreHit(hit, rank)
		// Typesense's order is authoritative: never let a row outscore the
		// one above it, even if a fallback score would say otherwise.
		if hs.score > prevScore {
			hs.score = prevScore
		}
		prevScore = hs.score
		// Check stored fields on the chunk document
		storedCount := 0
		storedLatest := ""
		if m, ok := (*hit.Document)["annotation_count"].(float64); ok {
			storedCount = int(m)
		}
		if m, ok := (*hit.Document)["latest_annotation"].(string); ok {
			storedLatest = m
		}

		annCount := storedCount
		if annCount == 0 && len(doc.Annotations) > 0 {
			annCount = len(doc.Annotations)
		}
		annLatest := storedLatest
		if annLatest == "" {
			annLatest = getLatestAnnotation(doc.Annotations)
		}

		out = append(out, SearchResult{
			Scope:            sc.Kind,
			ScopeID:          sc.ID,
			Path:             doc.Path,
			Title:            doc.Title,
			Tags:             doc.Tags,
			Excerpt:          trimExcerpt(doc.ChunkText, 120),
			Author:           doc.Author,
			UpdatedAt:        doc.UpdatedAt,
			Score:            hs.score,
			VectorScore:      hs.vector,
			KeywordMatch:     hs.keyword,
			AnnotationCount:  annCount,
			LatestAnnotation: annLatest,
			RecencyLabel:     computeRecencyLabel(time.Unix(doc.UpdatedAt, 0)),
		})
	}
	return out, nil
}

// hitScore is a search hit's relevance as reported to callers.
type hitScore struct {
	score   float64  // overall relevance in [0,1]
	vector  *float64 // semantic similarity in [0,1], nil if no vector distance
	keyword bool     // matched on keywords
}

// scoreHit maps a Typesense hit to a 0-1 relevance score. rank is the hit's
// 0-based position in the results, which Typesense has already ordered.
//
// Typesense orders hybrid results by rank_fusion_score (keyword rank and
// vector rank fused, weights summing to 1), so that is the score when
// present. Without it (a keyword-only collection) the raw text_match is a
// packed ~1e18 integer, not a similarity, so the score is derived from rank
// instead: 1/(rank+1), the same reciprocal-rank form fusion uses. A hit with
// only a vector distance falls back to its clamped similarity.
func scoreHit(hit api.SearchResultHit, rank int) hitScore {
	var hs hitScore
	if hit.VectorDistance != nil {
		v := clamp01(1.0 - float64(*hit.VectorDistance))
		hs.vector = &v
	}
	hs.keyword = hit.TextMatch != nil && *hit.TextMatch > 0

	switch {
	case hit.HybridSearchInfo != nil && hit.HybridSearchInfo.RankFusionScore != nil:
		hs.score = clamp01(float64(*hit.HybridSearchInfo.RankFusionScore))
	case hs.keyword || hs.vector == nil:
		hs.score = 1.0 / float64(rank+1)
	default:
		hs.score = *hs.vector
	}
	return hs
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// CountDocs returns how many documents a KB holds, 0 if nothing has ever
// been written to it. Unlike the other reads it doesn't create the
// collection. A stored KB counts its live Postgres rows (plus index-only
// docs until its backfill completes).
func (s *Service) CountDocs(ctx context.Context, sc Scope) (int, error) {
	if !s.stored(sc) {
		return s.countIndexed(ctx, sc)
	}
	done, _, err := s.storedRead(ctx, sc)
	if err != nil {
		return 0, err
	}
	if done {
		return s.store.CountKBDocuments(ctx, storeScope(sc))
	}
	docs, err := s.listStored(ctx, sc, "")
	if err != nil {
		return 0, err
	}
	return len(docs), nil
}

// countIndexed counts the docs the index holds.
func (s *Service) countIndexed(ctx context.Context, sc Scope) (int, error) {
	q := "*"
	perPage := 0
	res, err := s.client.Collection(collectionName(sc)).Documents().Search(ctx, &api.SearchCollectionParams{
		Q:        &q,
		QueryBy:  pointer.String("title"),
		FilterBy: pointer.String("chunk_index:=0"),
		PerPage:  &perPage,
	})
	if err != nil {
		if isNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("count docs: %w", err)
	}
	if res.Found == nil {
		return 0, nil
	}
	return *res.Found, nil
}

// ---- Tags ----

// ListTags returns all tags in a project's KB with their document counts.
func (s *Service) ListTags(ctx context.Context, sc Scope) ([]TagCount, error) {
	if err := s.ensureCollection(ctx, sc); err != nil {
		return nil, err
	}
	col := collectionName(sc)

	q := "*"
	filterBy := "chunk_index:=0"
	facetBy := "tags"
	maxFacetValues := 100
	perPage := 0

	results, err := s.client.Collection(col).Documents().Search(ctx, &api.SearchCollectionParams{
		Q:              &q,
		QueryBy:        pointer.String("title"),
		FilterBy:       pointer.String(filterBy),
		FacetBy:        &facetBy,
		MaxFacetValues: &maxFacetValues,
		PerPage:        &perPage,
		ExcludeFields:  pointer.String("content,embedding,chunk_text"),
	})
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}

	out := make([]TagCount, 0)
	if results.FacetCounts == nil {
		return out, nil
	}
	for _, facet := range *results.FacetCounts {
		if facet.Counts == nil {
			continue
		}
		for _, count := range *facet.Counts {
			tag := ""
			if count.Value != nil {
				tag = *count.Value
			}
			cnt := 0
			if count.Count != nil {
				cnt = *count.Count
			}
			if tag != "" {
				out = append(out, TagCount{Tag: tag, Count: cnt})
			}
		}
	}
	return out, nil
}

// KBStatus returns health metrics for the knowledge base.
// This is an idempotent, read-only operation that provides doc counts,
// stale doc counts, annotation statistics, and tag distribution. A stored KB
// counts from the same docs ListDocs returns; the global KB from the index.
// It also reports durability: stored_docs (live Postgres rows), indexed_docs
// (the index's own chunk-0 count) and the KB's backfill state. For a
// backfilled KB in sync, stored_docs == indexed_docs; the reconciler closes
// a gap on its next pass.
func (s *Service) KBStatus(ctx context.Context, sc Scope) (*StatusInfo, error) {
	var docs []*Doc
	if s.stored(sc) {
		var err error
		if docs, err = s.listStored(ctx, sc, ""); err != nil {
			return nil, fmt.Errorf("list docs: %w", err)
		}
	} else {
		if err := s.ensureCollection(ctx, sc); err != nil {
			return nil, err
		}
		summaries, err := s.ListDocs(ctx, sc, "")
		if err != nil {
			return nil, fmt.Errorf("list docs: %w", err)
		}
		for _, summary := range summaries {
			// Get full doc to access annotations
			fullDoc, err := s.GetDoc(ctx, sc, summary.Path)
			if err != nil {
				s.log.Warn("KBStatus: skip doc", "path", summary.Path, "err", err)
				continue
			}
			docs = append(docs, fullDoc)
		}
	}

	st := &StatusInfo{TotalDocs: len(docs), DocsByTag: make(map[string]int)}
	ninetyDaysAgo := time.Now().AddDate(0, 0, -90).Unix()
	for _, d := range docs {
		if d.UpdatedAt < ninetyDaysAgo {
			st.StaleDocs++
		}
		if len(d.Annotations) > 0 {
			st.DocsWithAnnotations++
		}
		for _, ann := range d.Annotations {
			st.TotalAnnotations++
			if !ann.IsResolved {
				st.UnresolvedAnnotations++
			}
		}
		for _, tag := range d.Tags {
			st.DocsByTag[tag]++
		}
	}
	s.indexedStatus(ctx, sc, st)
	if s.stored(sc) {
		// docs above came from storedRead, which marks a KB with no
		// collection complete, so the backfill state is read after it.
		if err := s.storedStatus(ctx, sc, st); err != nil {
			return nil, err
		}
		// Counted from every doc above; the index's tag facets stop at 100.
		return st, nil
	}
	if sc.Kind == ScopeGlobal {
		st.Note = "stored_docs and backfill are not reported: the global KB (the AMP guide) is synced from the repo into the search index only, with no Postgres copy."
	} else {
		st.Note = "stored_docs and backfill are not reported: this server has no Postgres KB store, so the search index is the only copy."
	}

	// Get tag distribution from ListTags
	tagCounts, err := s.ListTags(ctx, sc)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	for _, tc := range tagCounts {
		st.DocsByTag[tc.Tag] = tc.Count
	}
	return st, nil
}

// indexedStatus sets st.IndexedDocs from the index, or st.IndexError when
// it can't be counted. Status stays readable while Typesense is down: a
// stored KB's other numbers come from Postgres.
func (s *Service) indexedStatus(ctx context.Context, sc Scope, st *StatusInfo) {
	n, err := s.countIndexed(ctx, sc)
	if err != nil {
		st.IndexError = err.Error()
		return
	}
	st.IndexedDocs = &n
}

// storedStatus sets st.StoredDocs (live Postgres rows) and st.Backfill.
func (s *Service) storedStatus(ctx context.Context, sc Scope, st *StatusInfo) error {
	rs := storeScope(sc)
	n, err := s.store.CountKBDocuments(ctx, rs)
	if err != nil {
		return fmt.Errorf("count stored docs: %w", err)
	}
	st.StoredDocs = &n
	b, err := s.store.GetKBBackfill(ctx, rs)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return fmt.Errorf("backfill state: %w", err)
	}
	st.Backfill = backfillStateOf(b)
	return nil
}

// ---- Delete ----

// DeleteDoc removes a doc. For a stored KB (project, team, org) it
// soft-deletes the Postgres row first, so the doc stays in the 30-day trash
// (RestoreDoc brings it back; PurgeTrash removes it for good), and then
// removes it from the search index. by is who deleted it. Deleting a doc
// that has no live row is not an error. The global KB, and a service with
// no store, delete from the index only, as before.
func (s *Service) DeleteDoc(ctx context.Context, sc Scope, path, by string) error {
	if s.stored(sc) {
		if err := s.softDeleteStored(ctx, sc, path, by); err != nil {
			return err
		}
		s.index(ctx, sc, path)
		return nil
	}
	if s.queueClient != nil {
		payload, err := json.Marshal(queue.DeleteDocPayload{ProjectID: sc.projectID(), Scope: sc.Kind, ScopeID: sc.ID, Path: path})
		if err == nil {
			task := asynq.NewTask(queue.TypeKBDeleteDoc, payload)
			if _, err := s.queueClient.EnqueueContext(ctx, task, asynq.Queue(queue.QueueName)); err == nil {
				s.log.Info("doc delete enqueued", "kb", sc.String(), "path", path)
				return nil
			} else {
				s.log.Warn("enqueue failed, deleting synchronously instead", "kb", sc.String(), "path", path, "err", err)
			}
		} else {
			s.log.Warn("marshal enqueue payload failed, deleting synchronously instead", "err", err)
		}
	}
	return s.deleteDocNow(ctx, sc, path)
}

// softDeleteStored soft-deletes path's Postgres row. A doc that so far
// exists only in the index is copied into Postgres first (adoptIndexed), so
// it is deleted into the trash rather than dropped. A path with no live row
// is a no-op: the caller still clears the index.
func (s *Service) softDeleteStored(ctx context.Context, sc Scope, path, by string) error {
	if err := s.adoptIndexed(ctx, sc, path); err != nil {
		return err
	}
	err := s.store.DeleteKBDocument(ctx, storeScope(sc), path, by)
	if errors.Is(err, repository.ErrNotFound) {
		s.log.Info("delete: no live doc in postgres", "kb", sc.String(), "path", path)
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete %s: %w", path, err)
	}
	s.log.Info("doc moved to the trash", "kb", sc.String(), "path", path, "by", by)
	return nil
}

// RestoreDoc brings back a soft-deleted doc of a stored KB, with the content,
// title, tags and annotations it had when it was deleted, and makes it
// searchable again. It fails if the path has no deleted row (never deleted,
// already restored, or purged after 30 days in the trash). by is who
// restored it.
func (s *Service) RestoreDoc(ctx context.Context, sc Scope, path, by string) (*Doc, error) {
	if !s.stored(sc) {
		return nil, fmt.Errorf("restore %s: deleted docs of the %s KB are not kept", path, sc.Kind)
	}
	d, err := s.store.RestoreKBDocument(ctx, storeScope(sc), path, by)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, fmt.Errorf("no deleted doc %s in %s (never deleted, already restored, or purged after %s): %w",
			path, sc, repository.TrashRetention, err)
	}
	if err != nil {
		return nil, fmt.Errorf("restore %s: %w", path, err)
	}
	s.log.Info("doc restored from the trash", "kb", sc.String(), "path", path, "by", by)
	s.index(ctx, sc, path)
	return docFromRow(sc, d), nil
}

// DeletedDoc is a doc in a KB's trash, restorable with RestoreDoc.
type DeletedDoc struct {
	Path      string   `json:"path"`
	Title     string   `json:"title"`
	Tags      []string `json:"tags"`
	DeletedAt int64    `json:"deleted_at"` // unix seconds
	DeletedBy string   `json:"deleted_by"`
}

// ListDeletedDocs lists a stored KB's docs in the trash that RestoreDoc can
// still bring back (deleted less than 30 days ago), most recently deleted
// first. The global KB keeps no deleted docs, so it is an error.
func (s *Service) ListDeletedDocs(ctx context.Context, sc Scope) ([]DeletedDoc, error) {
	if !s.stored(sc) {
		return nil, fmt.Errorf("deleted docs of the %s KB are not kept", sc.Kind)
	}
	rows, err := s.store.ListDeletedKBDocuments(ctx, storeScope(sc))
	if err != nil {
		return nil, fmt.Errorf("list the deleted docs of %s: %w", sc, err)
	}
	out := make([]DeletedDoc, 0, len(rows))
	for _, d := range rows {
		out = append(out, DeletedDoc{Path: d.Path, Title: d.Title, Tags: d.Tags, DeletedAt: d.DeletedAt.Unix(), DeletedBy: d.DeletedBy})
	}
	return out, nil
}

// deleteDocNow performs the actual Typesense deletion. Called either
// directly or by the asynq worker via HandleDeleteDocTask.
func (s *Service) deleteDocNow(ctx context.Context, sc Scope, path string) error {
	col := collectionName(sc)
	return s.deleteByPath(ctx, col, path)
}

// ---- Async worker handlers ----
// Registered with asynq's ServeMux by whichever process runs the worker
// (currently amp-api itself, in a background goroutine — see main.go).

// SetQueueClient enables queue-backed writes. Call once at startup; nil
// (the default) means WriteDoc/DeleteDoc always write synchronously.
func (s *Service) SetQueueClient(c *asynq.Client) {
	s.queueClient = c
}

func (s *Service) HandleWriteDocTask(ctx context.Context, t *asynq.Task) error {
	var p queue.WriteDocPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("unmarshal write_doc payload: %w", err)
	}
	var anns []Annotation
	if p.Annotations != "" {
		if err := json.Unmarshal([]byte(p.Annotations), &anns); err != nil {
			return fmt.Errorf("unmarshal write_doc annotations: %w", err)
		}
	}
	sc := payloadScope(p.Scope, p.ScopeID, p.ProjectID)
	if s.stored(sc) {
		return s.drainLegacyWrite(ctx, sc, p, anns)
	}
	_, err := s.writeDocNow(ctx, sc, p.Path, p.Title, p.Content, p.Author, p.Tags, p.Reason, anns)
	return err
}

// HandleIndexDocTask runs a kb:index_doc job: it indexes the doc's current
// Postgres row, or removes the doc from the index if the row is gone.
func (s *Service) HandleIndexDocTask(ctx context.Context, t *asynq.Task) error {
	var p queue.IndexDocPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("unmarshal index_doc payload: %w", err)
	}
	sc := payloadScope(p.Scope, p.ScopeID, p.ProjectID)
	if !s.stored(sc) {
		return fmt.Errorf("index_doc for %s: this KB is not stored in Postgres", sc)
	}
	return s.indexFromStore(ctx, sc, p.Path)
}

func (s *Service) HandleDeleteDocTask(ctx context.Context, t *asynq.Task) error {
	var p queue.DeleteDocPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("unmarshal delete_doc payload: %w", err)
	}
	sc := payloadScope(p.Scope, p.ScopeID, p.ProjectID)
	if s.stored(sc) {
		// Queued by a pre-upgrade amp-api: delete into the trash too, or the
		// still-live row would put the doc back in the index on its next
		// annotation or reindex.
		if err := s.softDeleteStored(ctx, sc, p.Path, "kb-legacy-delete"); err != nil {
			return err
		}
		return s.indexFromStore(ctx, sc, p.Path)
	}
	return s.deleteDocNow(ctx, sc, p.Path)
}

// payloadScope reads a queued task's scope. Tasks enqueued before scopes
// existed carry only a project id.
func payloadScope(kind string, id, projectID int) Scope {
	if kind == "" {
		return Project(projectID)
	}
	return Scope{Kind: kind, ID: id}
}

// dropDeletedBy is the deleted_by Drop records on the docs it moves to the
// trash, so RestoreKB brings back exactly those (not docs deleted one by
// one before).
const dropDeletedBy = "kb-drop"

// dropLockWait bounds how long a Drop that found a reindex of the same KB
// running waits, in the background, to drop the search collections.
// dropLockPoll is how often it retries. Variables so tests can shorten them.
var (
	dropLockWait = 30 * time.Minute
	dropLockPoll = 500 * time.Millisecond
)

// Drop deletes a whole knowledge base (e.g. when its team or org is deleted).
// A stored KB's docs go to the trash (soft-deleted, purged after 30 days,
// restorable until then with RestoreKB) before its search collection is
// dropped; if that fails, the collection is kept.
//
// Dropping the collections takes the KB's reindex lock, so a reindex can't
// swap a new build and alias in after them. If a reindex holds the lock,
// Drop returns once the docs are in the trash and drops the collections in
// the background when the reindex ends (waiting at most dropLockWait). The
// reindex can't bring the docs back meanwhile: it builds from live rows
// only, and its catch-up removes the trashed paths.
func (s *Service) Drop(ctx context.Context, sc Scope) error {
	if s.stored(sc) {
		if err := s.trashStored(ctx, sc); err != nil {
			return fmt.Errorf("move the docs of %s to the trash: %w", sc, err)
		}
	}
	release, err := s.beginReindex(ctx, sc)
	if errors.Is(err, ErrReindexRunning) {
		s.log.Info("drop: a reindex is running; its collections are dropped when it ends", "kb", sc.String())
		go s.dropWhenFree(sc)
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	return s.dropCollections(ctx, collectionName(sc))
}

// dropWhenFree waits for a running reindex of sc to end, then drops sc's
// collections. Giving up (after dropLockWait) leaves an orphaned index of a
// KB whose docs are already in the trash, so it is only logged.
func (s *Service) dropWhenFree(sc Scope) {
	ctx, cancel := context.WithTimeout(context.Background(), dropLockWait)
	defer cancel()
	for {
		release, err := s.beginReindex(ctx, sc)
		if err == nil {
			err = s.dropCollections(ctx, collectionName(sc))
			release()
			if err != nil {
				s.log.Warn("drop after reindex failed; the collections are left", "kb", sc.String(), "err", err)
			} else {
				s.log.Info("dropped the collections after the reindex ended", "kb", sc.String())
			}
			return
		}
		if !errors.Is(err, ErrReindexRunning) {
			s.log.Warn("drop after reindex: could not take the reindex lock; the collections are left", "kb", sc.String(), "err", err)
			return
		}
		select {
		case <-ctx.Done():
			s.log.Warn("drop: the reindex did not end in time; the collections are left", "kb", sc.String(), "waited", dropLockWait)
			return
		case <-time.After(dropLockPoll):
		}
	}
}

// RestoreKB brings back a dropped KB (its team, org or project was deleted
// and is being restored): every doc Drop moved to the trash that is still
// there (not purged after 30 days) is restored, with its content and
// annotations, and re-indexed, which recreates the search collection. Docs
// deleted one by one before the drop stay in the trash. It returns how many
// docs it restored. by is who restored them.
func (s *Service) RestoreKB(ctx context.Context, sc Scope, by string) (int, error) {
	if !s.stored(sc) {
		return 0, fmt.Errorf("restore %s: deleted docs of the %s KB are not kept", sc, sc.Kind)
	}
	rs := storeScope(sc)
	paths, err := s.store.ListTrashedKBPaths(ctx, rs, dropDeletedBy)
	if err != nil {
		return 0, fmt.Errorf("list the dropped docs of %s: %w", sc, err)
	}
	n := 0
	for _, p := range paths {
		_, err := s.store.RestoreKBDocument(ctx, rs, p, by)
		if errors.Is(err, repository.ErrNotFound) {
			continue // restored or purged since the listing
		}
		if err != nil {
			return n, fmt.Errorf("restore %s: %w", p, err)
		}
		n++
		s.index(ctx, sc, p)
	}
	s.log.Info("KB restored from the trash", "kb", sc.String(), "docs", n, "by", by)
	return n, nil
}

// trashStored soft-deletes every live doc of a stored KB. Until its backfill
// completes, docs only the index has are first copied into Postgres, so
// dropping the collection afterwards loses none of them.
func (s *Service) trashStored(ctx context.Context, sc Scope) error {
	rs := storeScope(sc)
	done, err := s.backfillDone(ctx, sc)
	if err != nil {
		return err
	}
	if !done {
		indexed, err := s.listIndexed(ctx, sc, "")
		if err != nil {
			return err
		}
		for _, d := range indexed {
			if err := s.adoptIndexed(ctx, sc, d.Path); err != nil {
				return err
			}
		}
	}
	var paths []string
	if err := s.store.StreamKBDocuments(ctx, rs, func(d repository.KBDocument) error {
		paths = append(paths, d.Path)
		return nil
	}); err != nil {
		return err
	}
	for _, p := range paths {
		if err := s.store.DeleteKBDocument(ctx, rs, p, dropDeletedBy); err != nil && !errors.Is(err, repository.ErrNotFound) {
			return err
		}
	}
	return nil
}

// SetReindexLock makes reindexing single-flight across replicas too, not
// just within this process. Call once at startup.
func (s *Service) SetReindexLock(l ReindexLocker) {
	s.reindexMu.Lock()
	defer s.reindexMu.Unlock()
	s.reindexLock = l
}

// beginReindex claims the right to reindex sc, or returns
// ErrReindexRunning. Two overlapping runs on one KB can lose docs: on the
// rebuild path the second snapshots after the first dropped the collection,
// then drops the collection the first is writing into.
func (s *Service) beginReindex(ctx context.Context, sc Scope) (release func(), err error) {
	col := collectionName(sc)
	s.reindexMu.Lock()
	if s.reindexing[col] {
		s.reindexMu.Unlock()
		return nil, ErrReindexRunning
	}
	if s.reindexing == nil {
		s.reindexing = map[string]bool{}
	}
	s.reindexing[col] = true
	locker := s.reindexLock
	s.reindexMu.Unlock()

	done := func() {
		s.reindexMu.Lock()
		delete(s.reindexing, col)
		s.reindexMu.Unlock()
	}
	if locker == nil {
		return done, nil
	}
	unlock, ok, err := locker(ctx, "kb_reindex:"+col)
	if err != nil {
		done()
		return nil, fmt.Errorf("reindex lock for %s: %w", col, err)
	}
	if !ok {
		done()
		return nil, ErrReindexRunning
	}
	return func() { unlock(); done() }, nil
}

// StartReindex starts a reindex of sc in the background and returns at
// once, or returns ErrReindexRunning without starting one. The run outlives
// ctx; failures are logged.
func (s *Service) StartReindex(ctx context.Context, sc Scope) error {
	release, err := s.beginReindex(ctx, sc)
	if err != nil {
		return err
	}
	go func() {
		defer release()
		if err := s.runReindex(context.Background(), sc); err != nil {
			s.log.Error("kb reindex failed", "kb", sc.String(), "err", err)
		}
	}()
	return nil
}

// Reindex re-triggers embedding generation for all documents by re-upserting them.
// This is useful when switching embedding models. It returns
// ErrReindexRunning, without doing anything, if the KB is already being
// reindexed.
func (s *Service) Reindex(ctx context.Context, sc Scope) error {
	release, err := s.beginReindex(ctx, sc)
	if err != nil {
		return err
	}
	defer release()
	return s.runReindex(ctx, sc)
}

func (s *Service) runReindex(ctx context.Context, sc Scope) error {
	// If the collection was originally created before Ollama was reachable
	// (e.g. a race at first-write time against the post-install model pull
	// Job), it's permanently locked into keyword-only mode — Typesense
	// collections can't gain a field after creation. Detect that and
	// rebuild it so ensureCollection picks up the embedding field this
	// time, now that Ollama is reachable.
	rebuild := s.ollamaURL != "" && s.ollamaReachable(ctx) && !s.collectionHasEmbedding(ctx, collectionName(sc))
	return s.reindex(ctx, sc, rebuild)
}

// reindex re-writes every doc in a KB, first dropping and recreating the
// collection when rebuild is set. Docs (with their annotations) are read
// before anything is dropped and written back explicitly.
func (s *Service) reindex(ctx context.Context, sc Scope, rebuild bool) error {
	if s.stored(sc) {
		return s.reindexFromStore(ctx, sc)
	}
	col := collectionName(sc)

	// Snapshot full doc content BEFORE any possible drop+recreate below —
	// once the collection is dropped there is nothing left to read back.
	summaries, err := s.ListDocs(ctx, sc, "")
	if err != nil {
		return err
	}
	docs := make([]*Doc, 0, len(summaries))
	for _, summary := range summaries {
		full, err := s.GetDoc(ctx, sc, summary.Path)
		if err != nil {
			s.log.Warn("reindex: skip doc", "path", summary.Path, "err", err)
			continue
		}
		docs = append(docs, full)
	}

	if rebuild {
		s.log.Info("reindex: upgrading collection to semantic search (was keyword-only)", "collection", col)
		if err := s.dropCollections(ctx, col); err != nil {
			s.log.Warn("reindex: failed to drop keyword-only collection, continuing with existing schema", "collection", col, "err", err)
		} else if err := s.ensureCollection(ctx, sc); err != nil {
			return fmt.Errorf("recreate collection with embeddings: %w", err)
		}
	}

	for _, full := range docs {
		// Pass the annotations explicitly: if the collection was just
		// recreated, there is nothing left to read them back from.
		if _, err := s.WriteDoc(ctx, sc, full.Path, full.Title, full.Content, full.Author, full.Tags, full.Reason, full.Annotations); err != nil {
			s.log.Warn("reindex: write failed", "path", full.Path, "err", err)
		}
	}
	return nil
}

// collectionHasEmbedding checks whether a Typesense collection has an embedding field.
// This determines whether hybrid search is available or if we fall back to keyword-only.
func (s *Service) collectionHasEmbedding(ctx context.Context, collectionName string) bool {
	col, err := s.client.Collection(collectionName).Retrieve(ctx)
	if err != nil {
		return false
	}
	for _, f := range col.Fields {
		if f.Name == "embedding" {
			return true
		}
	}
	return false
}

// ollamaReachable does a quick health check on the Ollama server.
// Returns false if Ollama is not running or has no models — collection is created keyword-only.
// When Ollama later becomes available with models, call Reindex to add embeddings.
func (s *Service) ollamaReachable(ctx context.Context) bool {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, s.ollamaURL+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	// Check that at least one model is available. Ollama's /api/tags returns
	// a flat {"models": [{"name": "..."}, ...]} — no nested wrapper.
	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false
	}
	return len(result.Models) > 0
}

// SearchAPIKey returns the API key for the UI to search directly.
// For a local dev stack the admin key doubles as the search key.
func (s *Service) SearchAPIKey() string { return s.typesenseKey }

// TypesenseURL returns the Typesense URL for the frontend direct search.
func (s *Service) TypesenseURL() string { return s.typesenseURL }

// ---- Postgres store (source of truth) ----
//
// For project, team and org KBs with a store set, every write and every
// annotation change commits to Postgres first; Typesense is updated after
// the commit, from the committed row. An indexing failure after the commit
// is logged and never loses the doc: the row is already saved, and the next
// index job, reindex or reconcile brings the index back in line.

// Store is the Postgres store of KB docs. *repository.Repo implements it.
type Store interface {
	UpsertKBDocument(ctx context.Context, scope repository.KBScope, w repository.KBDocWrite) (*repository.KBDocument, bool, error)
	GetKBDocument(ctx context.Context, scope repository.KBScope, path string) (*repository.KBDocument, error)
	UpdateKBAnnotations(ctx context.Context, scope repository.KBScope, path string,
		fn func([]domain.KBAnnotation) ([]domain.KBAnnotation, error)) (*repository.KBDocument, error)
	InsertKBDocumentIfMissing(ctx context.Context, scope repository.KBScope, d repository.KBDocument, by string) (bool, error)
	CountKBDocuments(ctx context.Context, scope repository.KBScope) (int, error)
	ListKBDocuments(ctx context.Context, scope repository.KBScope, limit, offset int) ([]repository.KBDocument, error)
	ListDeletedKBPaths(ctx context.Context, scope repository.KBScope) ([]string, error)
	ListDeletedKBDocuments(ctx context.Context, scope repository.KBScope) ([]repository.KBDeletedDoc, error)
	StreamKBDocuments(ctx context.Context, scope repository.KBScope, fn func(repository.KBDocument) error) error
	IsKBBackfillComplete(ctx context.Context, scope repository.KBScope) (bool, error)
	DeleteKBDocument(ctx context.Context, scope repository.KBScope, path, by string) error
	RestoreKBDocument(ctx context.Context, scope repository.KBScope, path, by string) (*repository.KBDocument, error)
	ListTrashedKBPaths(ctx context.Context, scope repository.KBScope, by string) ([]string, error)
	ListBackfilledKBScopes(ctx context.Context) ([]repository.KBScope, error)
	StartKBBackfill(ctx context.Context, scope repository.KBScope) error
	CompleteKBBackfill(ctx context.Context, scope repository.KBScope, docCount int) error
	FailKBBackfill(ctx context.Context, scope repository.KBScope, msg string) error
	GetKBBackfill(ctx context.Context, scope repository.KBScope) (*repository.KBBackfill, error)
}

var _ Store = (*repository.Repo)(nil)

// SetStore makes Postgres the source of truth for project, team and org
// KBs. Call once at startup, before serving.
func (s *Service) SetStore(st Store) { s.store = st }

// stored reports whether sc's docs live in Postgres. The global KB (the AMP
// guide) never does: it is synced from the repo's kb/ folder.
func (s *Service) stored(sc Scope) bool {
	return s.store != nil && sc.Kind != ScopeGlobal
}

func storeScope(sc Scope) repository.KBScope { return repository.KBScope{Kind: sc.Kind, ID: sc.ID} }

// writeStored is WriteDoc for a stored KB: commit to Postgres, then index.
func (s *Service) writeStored(ctx context.Context, sc Scope, path, title, content, author string, tags []string, reason string, annotations []Annotation) (*DocSummary, error) {
	if err := s.adoptIndexed(ctx, sc, path); err != nil {
		return nil, err
	}
	rs := storeScope(sc)
	d, _, err := s.store.UpsertKBDocument(ctx, rs, repository.KBDocWrite{
		Path: path, Title: title, Tags: tags, Author: author, Reason: reason, Content: content, By: author,
	})
	if err != nil {
		return nil, fmt.Errorf("save %s: %w", path, err)
	}
	if len(annotations) > 0 {
		if d, err = s.store.UpdateKBAnnotations(ctx, rs, path, func([]domain.KBAnnotation) ([]domain.KBAnnotation, error) {
			return annotations, nil
		}); err != nil {
			return nil, fmt.Errorf("save annotations of %s: %w", path, err)
		}
	}
	s.index(ctx, sc, path)
	return summaryFromRow(sc, d), nil
}

// updateStoredAnnotations changes a stored doc's annotations under its row
// lock (fn gets the current list and returns the new one), then indexes it.
func (s *Service) updateStoredAnnotations(ctx context.Context, sc Scope, path string, fn func([]Annotation) ([]Annotation, error)) (*Doc, error) {
	if err := s.adoptIndexed(ctx, sc, path); err != nil {
		return nil, err
	}
	d, err := s.store.UpdateKBAnnotations(ctx, storeScope(sc), path, fn)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, fmt.Errorf("doc not found: %s/%s: %w", collectionName(sc), path, err)
	}
	if err != nil {
		return nil, err
	}
	s.index(ctx, sc, path)
	return docFromRow(sc, d), nil
}

// adoptIndexed copies a doc that so far exists only in the Typesense index
// into Postgres (insert-if-missing, keeping its annotations and updated_at)
// before its first Postgres write or annotation. It does nothing once the
// KB's backfill has completed, or when Postgres has a row for the path.
// Without it, the first write to such a doc would start its row without the
// annotations the index holds, and the backfill would then skip the path as
// already present. A doc deleted in Postgres stays deleted: insert-if-missing
// skips any path with a row, live or deleted.
func (s *Service) adoptIndexed(ctx context.Context, sc Scope, path string) error {
	rs := storeScope(sc)
	done, err := s.backfillDone(ctx, sc)
	if err != nil {
		return fmt.Errorf("check the backfill of %s: %w", sc, err)
	}
	if done {
		return nil
	}
	if _, err := s.store.GetKBDocument(ctx, rs, path); err == nil {
		return nil
	} else if !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	old, err := s.getIndexed(ctx, sc, path)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		// Writing now could drop the indexed copy's annotations for good.
		return fmt.Errorf("read the indexed copy of %s before its first Postgres write (this KB is not backfilled yet): %w", path, err)
	}
	row := repository.KBDocument{
		Path: path, Title: old.Title, Tags: old.Tags, Author: old.Author, Reason: old.Reason,
		Content: old.Content, Annotations: old.Annotations,
	}
	if old.UpdatedAt > 0 {
		row.UpdatedAt = time.Unix(old.UpdatedAt, 0)
	}
	if _, err := s.store.InsertKBDocumentIfMissing(ctx, rs, row, "kb-adopt-on-write"); err != nil {
		return fmt.Errorf("copy the indexed %s into postgres: %w", path, err)
	}
	return nil
}

// index brings path's search entry in line with its Postgres row: through
// the queue when there is one, else now. It never fails the caller, whose
// change is already committed; a failure is logged, and the doc is
// re-indexed by the next write, reindex or reconcile.
func (s *Service) index(ctx context.Context, sc Scope, path string) {
	if s.queueClient != nil {
		payload, err := json.Marshal(queue.IndexDocPayload{ProjectID: sc.projectID(), Scope: sc.Kind, ScopeID: sc.ID, Path: path})
		if err == nil {
			task := asynq.NewTask(queue.TypeKBIndexDoc, payload)
			if _, err = s.queueClient.EnqueueContext(ctx, task, asynq.Queue(queue.QueueName)); err == nil {
				return
			}
		}
		s.log.Warn("enqueue index job failed, indexing synchronously instead", "kb", sc.String(), "path", path, "err", err)
	}
	if err := s.indexFromStore(ctx, sc, path); err != nil {
		s.log.Error("doc saved in postgres but not indexed; search misses it until it is re-indexed",
			"kb", sc.String(), "path", path, "err", err)
	}
}

// indexFromStore indexes path's current Postgres row, or removes path from
// the index when it has no live row (deleted or purged). Jobs carry only the
// path, so one applied late or out of order still indexes the latest row.
// If the row changes while it is being indexed (another job may have
// indexed the newer row first and then been overwritten by this one), it
// indexes again until what it indexed is still current.
func (s *Service) indexFromStore(ctx context.Context, sc Scope, path string) error {
	rs := storeScope(sc)
	d, err := s.store.GetKBDocument(ctx, rs, path)
	if errors.Is(err, repository.ErrNotFound) {
		// Never drop an indexed doc Postgres hasn't got yet.
		if err := s.adoptIndexed(ctx, sc, path); err != nil {
			return err
		}
		d, err = s.store.GetKBDocument(ctx, rs, path)
	}
	for attempt := 1; ; attempt++ {
		if errors.Is(err, repository.ErrNotFound) {
			if err := s.deleteByPath(ctx, collectionName(sc), path); err != nil {
				return err
			}
			s.log.Info("doc removed from the index", "kb", sc.String(), "path", path)
			return nil
		}
		if err != nil {
			return fmt.Errorf("load %s to index it: %w", path, err)
		}
		chunks, ierr := s.indexChunks(ctx, sc, indexedFromRow(d))
		if ierr != nil {
			return ierr
		}
		indexed := d
		d, err = s.store.GetKBDocument(ctx, rs, path)
		if err == nil && sameIndexed(indexed, d) {
			s.log.Info("doc indexed", "kb", sc.String(), "path", path, "version", d.Version, "chunks", chunks)
			return nil
		}
		if attempt == 5 {
			return fmt.Errorf("%s kept changing while it was being indexed", path)
		}
	}
}

// sameIndexed reports whether a and b index identically.
func sameIndexed(a, b *repository.KBDocument) bool {
	if a.Version != b.Version || a.Author != b.Author || a.Reason != b.Reason || !a.UpdatedAt.Equal(b.UpdatedAt) {
		return false
	}
	ja, _ := json.Marshal(kbAnnotationsOrEmpty(a.Annotations))
	jb, _ := json.Marshal(kbAnnotationsOrEmpty(b.Annotations))
	return string(ja) == string(jb)
}

func kbAnnotationsOrEmpty(a []Annotation) []Annotation {
	if a == nil {
		return []Annotation{}
	}
	return a
}

// drainLegacyWrite runs a kb:write_doc task for a stored KB. Only an older
// amp-api enqueues those (before the store existed, or during the rolling
// deploy that introduced it), and its payload may be the only copy of that
// write, so it is saved to Postgres first and then indexed from there.
func (s *Service) drainLegacyWrite(ctx context.Context, sc Scope, p queue.WriteDocPayload, anns []Annotation) error {
	rs := storeScope(sc)
	if err := s.adoptIndexed(ctx, sc, p.Path); err != nil {
		return err
	}
	_, err := s.store.GetKBDocument(ctx, rs, p.Path)
	switch {
	case err == nil:
		// Apply it like any write. If a newer write already landed, the one
		// this overwrites stays in the version history. The payload's
		// annotations (an old reindex or import's snapshot) are not applied
		// over the row's current ones.
		if _, _, err := s.store.UpsertKBDocument(ctx, rs, repository.KBDocWrite{
			Path: p.Path, Title: p.Title, Tags: p.Tags, Author: p.Author, Reason: p.Reason, Content: p.Content, By: p.Author,
		}); err != nil {
			return fmt.Errorf("save queued write of %s: %w", p.Path, err)
		}
	case errors.Is(err, repository.ErrNotFound):
		// No live row: insert it unless the path has a deleted row (it was
		// deleted after this write was queued, so it stays deleted).
		if _, err := s.store.InsertKBDocumentIfMissing(ctx, rs, repository.KBDocument{
			Path: p.Path, Title: p.Title, Tags: p.Tags, Author: p.Author, Reason: p.Reason, Content: p.Content, Annotations: anns,
		}, p.Author); err != nil {
			return fmt.Errorf("save queued write of %s: %w", p.Path, err)
		}
	default:
		return err
	}
	return s.indexFromStore(ctx, sc, p.Path)
}

// ---- Reindex: build a new collection, then swap the alias (#1990) ----
//
// A stored KB's search collection is reached by its base name
// (collectionName, e.g. "kb_5"). A reindex never deletes the only copy of
// anything: it builds a new, versioned collection ("kb_5_v<unix nanos>")
// from Postgres, verifies it, points an alias named after the base at it,
// and only then drops the collection that served before. Every other path
// (search, writes, index jobs, reads of the index, status, Drop) keeps using
// the base name, which Typesense resolves through the alias.
//
// A KB that has never been reindexed this way is served by a real collection
// with the base name, and Typesense can't create an alias with the name of a
// collection. The first reindex therefore deletes that collection and then
// creates the alias. Between the two the name resolves to nothing for a
// moment (searches and index writes fail and are retried, or caught up
// below), but no doc exists only in memory: every doc is in Postgres, which
// was checked to cover the index, and in the verified new collection.
//
// Writes during a reindex: a change committed after the build read its row
// is indexed by its own index job into whatever the base name resolved to,
// which before the swap is the old collection. So the reindex catches up
// twice: before the swap (and before verifying the count) and again after
// it, re-indexing every doc whose row changed since it was indexed into the
// new collection and removing docs that are no longer live. A change
// committed after the second catch-up read is indexed by its own job, which
// runs after the swap and so writes into the new collection.

// reindexFromStore rebuilds a stored KB's search collection from Postgres,
// keeping each doc's own updated_at. It refuses (ErrReindexRefused, nothing
// changed) until the KB's backfill has completed (ErrBackfillPending), and
// when the index holds any doc Postgres has no row for. A failure or cancel before the
// swap drops only the new collection; the old one keeps serving, unchanged.
func (s *Service) reindexFromStore(ctx context.Context, sc Scope) error {
	base := collectionName(sc)
	done, err := s.backfillDone(ctx, sc)
	if err != nil {
		return fmt.Errorf("reindex: check the backfill of %s: %w", sc, err)
	}
	if !done {
		s.log.Warn("reindex refused: the KB's backfill has not completed; nothing was changed", "kb", sc.String())
		return fmt.Errorf("%w: %w: %s (it runs at startup; a failed one retries on the next)", ErrReindexRefused, ErrBackfillPending, sc)
	}
	if err := s.checkStoreCoversIndex(ctx, sc); err != nil {
		return err
	}
	kc, err := s.kbCollections(ctx, base)
	if err != nil {
		return err
	}
	// Builds left by an interrupted reindex were never swapped in: only the
	// alias target serves. (The caller holds the reindex lock, so none of
	// them is being built now.)
	for _, b := range kc.builds {
		if b != kc.alias {
			s.dropCollection(ctx, b)
		}
	}

	newCol := fmt.Sprintf("%s_v%d", base, time.Now().UnixNano())
	if _, err := s.client.Collections().Create(ctx, s.collectionSchema(ctx, newCol)); err != nil {
		return fmt.Errorf("reindex: create collection %s: %w", newCol, err)
	}
	keepNew := false
	defer func() {
		if !keepNew {
			s.dropCollection(context.WithoutCancel(ctx), newCol)
		}
	}()

	// path -> fingerprint of the row indexed into newCol.
	indexed := map[string]string{}
	if _, err := s.syncCollection(ctx, sc, newCol, indexed); err != nil {
		return fmt.Errorf("reindex: build %s: %w", newCol, err)
	}
	s.runHook(ctx, "built")
	if err := s.verifyBuild(ctx, sc, newCol, indexed); err != nil {
		return fmt.Errorf("reindex: verify %s: %w", newCol, err)
	}
	s.runHook(ctx, "verified")
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("reindex cancelled before the swap; %s still serves: %w", base, err)
	}

	// From here on, finish even if ctx is cancelled: stopping half way
	// through the first migration would leave the base name resolving to
	// nothing.
	swapCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	keepNew, err = s.swapAlias(swapCtx, kc, newCol)
	if err != nil {
		return fmt.Errorf("reindex: swap %s to %s: %w", base, newCol, err)
	}
	keepNew = true
	s.log.Info("reindex: alias swapped", "kb", sc.String(), "alias", base, "collection", newCol, "docs", len(indexed),
		"old_collection", kc.alias, "migrated_from_collection", kc.real)
	s.runHook(swapCtx, "swapped")

	// Catch up on changes whose index jobs wrote into the old collection.
	_, syncErr := s.syncCollection(swapCtx, sc, newCol, indexed)
	if kc.alias != "" && kc.alias != newCol {
		s.dropCollection(swapCtx, kc.alias)
	}
	if syncErr != nil {
		return fmt.Errorf("reindex: catch up after the swap (the reconciler or the next write fixes the rest): %w", syncErr)
	}
	return nil
}

func (s *Service) runHook(ctx context.Context, stage string) {
	if s.reindexHook != nil {
		s.reindexHook(ctx, stage)
	}
}

// checkStoreCoversIndex makes sure a collection built from Postgres would
// hold every doc the current index has (the caller has checked that the
// KB's backfill completed): every indexed path must have a Postgres row, a
// live one or a deleted one (the index entry is stale, and the new
// collection rightly leaves it out).
func (s *Service) checkStoreCoversIndex(ctx context.Context, sc Scope) error {
	rs := storeScope(sc)
	indexed, err := s.listIndexed(ctx, sc, "")
	if err != nil {
		return err
	}
	live := map[string]bool{}
	if err := s.store.StreamKBDocuments(ctx, rs, func(d repository.KBDocument) error {
		live[d.Path] = true
		return nil
	}); err != nil {
		return err
	}
	deletedPaths, err := s.store.ListDeletedKBPaths(ctx, rs)
	if err != nil {
		return err
	}
	deleted := make(map[string]bool, len(deletedPaths))
	for _, p := range deletedPaths {
		deleted[p] = true
	}
	var missing []string
	for _, d := range indexed {
		if !live[d.Path] && !deleted[d.Path] {
			missing = append(missing, d.Path)
		}
	}
	if len(missing) > 0 {
		s.log.Error("reindex refused: the index holds docs postgres has no row for; nothing was changed",
			"kb", sc.String(), "stored", len(live), "indexed", len(indexed), "missing", len(missing))
		return fmt.Errorf("%w: %s has %d live docs in postgres and %d in the index; %d indexed docs have no postgres row (first: %s)",
			ErrReindexRefused, sc, len(live), len(indexed), len(missing), missing[0])
	}
	return nil
}

// rowFingerprint changes whenever what a row indexes as changes.
func rowFingerprint(d *repository.KBDocument) string {
	anns, _ := json.Marshal(kbAnnotationsOrEmpty(d.Annotations))
	tags, _ := json.Marshal(d.Tags)
	return fmt.Sprintf("%d|%d|%q|%q|%q|%s|%s", d.Version, d.UpdatedAt.UnixNano(), d.Title, d.Author, d.Reason, tags, anns)
}

// syncCollection brings col in line with the KB's live Postgres rows:
// indexes every row whose fingerprint differs from indexed[path] (all of
// them when indexed is empty), and removes the paths in indexed that are no
// longer live. indexed is updated to what col now holds. It returns how many
// docs it changed.
func (s *Service) syncCollection(ctx context.Context, sc Scope, col string, indexed map[string]string) (int, error) {
	rs := storeScope(sc)
	live := map[string]string{}
	if err := s.store.StreamKBDocuments(ctx, rs, func(d repository.KBDocument) error {
		live[d.Path] = rowFingerprint(&d)
		return nil
	}); err != nil {
		return 0, err
	}
	changed, failed := 0, 0
	var firstErr error
	fail := func(path string, err error) {
		failed++
		if firstErr == nil {
			firstErr = err
		}
		s.log.Warn("reindex: index failed", "kb", sc.String(), "collection", col, "path", path, "err", err)
	}
	for path, fp := range live {
		if err := ctx.Err(); err != nil {
			return changed, err
		}
		if indexed[path] == fp {
			continue
		}
		d, err := s.store.GetKBDocument(ctx, rs, path)
		if errors.Is(err, repository.ErrNotFound) {
			live[path] = "" // deleted since the listing: removed below
			continue
		}
		if err != nil {
			fail(path, err)
			continue
		}
		if _, err := s.indexChunksInto(ctx, sc, col, indexedFromRow(d)); err != nil {
			fail(path, err)
			delete(indexed, path) // unknown state: retried by the next pass
			continue
		}
		indexed[path] = rowFingerprint(d)
		changed++
	}
	for path := range indexed {
		if fp, ok := live[path]; ok && fp != "" {
			continue
		}
		if err := s.deleteByPath(ctx, col, path); err != nil {
			fail(path, err)
			continue
		}
		delete(indexed, path)
		changed++
	}
	if failed > 0 {
		return changed, fmt.Errorf("%d docs failed to index into %s: %w", failed, col, firstErr)
	}
	return changed, nil
}

// verifyBuild catches up on writes made while col was being built and
// checks that col holds exactly the docs it was given. Typesense indexes
// each upsert before answering, so the count matches at once unless a write
// or delete raced the count; it retries a few times for that.
func (s *Service) verifyBuild(ctx context.Context, sc Scope, col string, indexed map[string]string) error {
	var got int
	for attempt := 1; attempt <= 5; attempt++ {
		if _, err := s.syncCollection(ctx, sc, col, indexed); err != nil {
			return err
		}
		n, err := s.countIn(ctx, col)
		if err != nil {
			return err
		}
		if n == len(indexed) {
			return nil
		}
		got = n
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s holds %d docs, want %d", col, got, len(indexed))
}

// swapAlias points the alias kc.base at newCol. On the first migration (a
// real collection holds the base name) it deletes that collection first.
// keepNew reports whether newCol must be kept even though err is set: the
// old collection is already gone, so newCol is the only search index left.
func (s *Service) swapAlias(ctx context.Context, kc kbCollections, newCol string) (keepNew bool, err error) {
	for attempt := 1; ; attempt++ {
		_, err := s.client.Aliases().Upsert(ctx, kc.base, &api.CollectionAliasSchema{CollectionName: newCol})
		if err == nil {
			break
		}
		// Typesense refuses an alias with a collection's name. If that's
		// why, delete the collection: its docs are all in Postgres (checked
		// before the build) and in the verified newCol. On a retry it is one
		// an index write recreated in the moment the name resolved to
		// nothing; the catch-up after the swap re-indexes its docs.
		c, gerr := s.client.Collection(kc.base).Retrieve(ctx)
		if gerr != nil || c.Name != kc.base || attempt > 5 {
			return keepNew, err
		}
		if _, derr := s.client.Collection(kc.base).Delete(ctx); derr != nil && !isNotFound(derr) {
			return keepNew, fmt.Errorf("delete collection %s to make way for its alias: %w", kc.base, derr)
		}
		keepNew = true
		s.log.Info("reindex: dropped the KB's original collection to replace it with an alias", "collection", kc.base, "new", newCol)
	}
	// A collection created under the base name since the alias went in
	// would shadow it (ensureCollection also guards against this).
	if c, err := s.client.Collection(kc.base).Retrieve(ctx); err == nil && c.Name == kc.base {
		s.unshadowAlias(ctx, kc.base)
	}
	return true, nil
}

// kbCollections is what a KB's base name maps to in Typesense.
type kbCollections struct {
	base   string
	alias  string   // the alias's target collection; "" without an alias
	real   bool     // a real collection is named base (not yet migrated to an alias)
	builds []string // collections built by reindexes (base_v<n>), the alias target among them
}

func (s *Service) kbCollections(ctx context.Context, base string) (kbCollections, error) {
	kc := kbCollections{base: base}
	a, err := s.client.Alias(base).Retrieve(ctx)
	switch {
	case err == nil:
		kc.alias = a.CollectionName
	case !isNotFound(err):
		return kc, fmt.Errorf("read alias %s: %w", base, err)
	}
	cols, err := s.client.Collections().Retrieve(ctx, &api.GetCollectionsParams{ExcludeFields: pointer.String("fields")})
	if err != nil {
		return kc, fmt.Errorf("list collections: %w", err)
	}
	for _, c := range cols {
		switch {
		case c.Name == base:
			kc.real = true
		case isBuildOf(base, c.Name):
			kc.builds = append(kc.builds, c.Name)
		}
	}
	return kc, nil
}

// isBuildOf reports whether name is a collection a reindex built for base.
func isBuildOf(base, name string) bool {
	rest, ok := strings.CutPrefix(name, base+"_v")
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// dropCollections deletes everything a KB's base name maps to: the alias,
// its target, a real collection of that name, and leftover builds.
func (s *Service) dropCollections(ctx context.Context, base string) error {
	kc, err := s.kbCollections(ctx, base)
	if err != nil {
		return err
	}
	if kc.alias != "" {
		// The alias first: deleting a collection by the alias's name would
		// delete its target and leave the alias dangling.
		if _, err := s.client.Alias(base).Delete(ctx); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete alias %s: %w", base, err)
		}
	}
	if kc.real {
		if _, err := s.client.Collection(base).Delete(ctx); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete collection %s: %w", base, err)
		}
	}
	for _, b := range kc.builds {
		if _, err := s.client.Collection(b).Delete(ctx); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete collection %s: %w", b, err)
		}
	}
	return nil
}

// dropCollection deletes one collection by its own name, logging a failure.
func (s *Service) dropCollection(ctx context.Context, name string) {
	if _, err := s.client.Collection(name).Delete(ctx); err != nil && !isNotFound(err) {
		s.log.Warn("could not drop collection; a later reindex retries", "collection", name, "err", err)
	}
}

// countIn counts the docs (chunk 0 records) in collection col.
func (s *Service) countIn(ctx context.Context, col string) (int, error) {
	q, perPage := "*", 0
	res, err := s.client.Collection(col).Documents().Search(ctx, &api.SearchCollectionParams{
		Q: &q, QueryBy: pointer.String("title"), FilterBy: pointer.String("chunk_index:=0"), PerPage: &perPage,
	})
	if err != nil {
		return 0, err
	}
	if res.Found == nil {
		return 0, nil
	}
	return *res.Found, nil
}

// indexedDoc is what the search index holds for one doc.
type indexedDoc struct {
	path, title, content, author, reason string
	tags                                 []string
	annotations                          []Annotation
	updatedAt                            int64
	// version is the Postgres row's version, stored on each chunk (not in
	// the schema, so Typesense keeps it without indexing it) for the
	// reconciler to spot a stale entry. 0 = unknown (not written).
	version int
}

func indexedFromRow(d *repository.KBDocument) indexedDoc {
	return indexedDoc{
		path: d.Path, title: d.Title, content: d.Content, author: d.Author, reason: d.Reason,
		tags: d.Tags, annotations: d.Annotations, updatedAt: d.UpdatedAt.Unix(), version: d.Version,
	}
}

// indexChunks writes a doc into its KB's collection, ~500 tokens per chunk.
// It upserts the new chunks first and only then deletes chunks past the new
// last one, so a failure part way leaves the doc searchable (some chunks
// old, some new; the next index run fixes it), never missing.
func (s *Service) indexChunks(ctx context.Context, sc Scope, d indexedDoc) (int, error) {
	if err := s.ensureCollection(ctx, sc); err != nil {
		return 0, err
	}
	return s.indexChunksInto(ctx, sc, collectionName(sc), d)
}

// indexChunksInto is indexChunks into the collection (or alias) col.
func (s *Service) indexChunksInto(ctx context.Context, sc Scope, col string, d indexedDoc) (int, error) {
	tags := d.tags
	if tags == nil {
		tags = []string{}
	}
	annJSON := "[]"
	if len(d.annotations) > 0 {
		b, err := json.Marshal(d.annotations)
		if err != nil {
			return 0, fmt.Errorf("encode annotations of %s: %w", d.path, err)
		}
		annJSON = string(b)
	}
	annCount, annLatest := len(d.annotations), getLatestAnnotation(d.annotations)

	chunks := chunkMarkdown(d.content, 500) // ~500 tokens per chunk
	if len(chunks) == 0 {
		chunks = []string{d.content}
	}
	for i, chunk := range chunks {
		doc := map[string]interface{}{
			"id":          docID(sc, d.path, i),
			"project_id":  sc.projectID(),
			"reason":      d.reason,
			"path":        d.path,
			"title":       d.title,
			"tags":        tags,
			"author":      d.author,
			"chunk_index": i,
			"chunk_text":  chunk,
			"updated_at":  d.updatedAt,
			// Every chunk carries the count and latest annotation so any
			// search hit shows them; the annotations live on chunk 0.
			"annotation_count":  annCount,
			"latest_annotation": annLatest,
			"annotations":       "[]",
			"content":           "",
		}
		if d.version > 0 {
			doc[indexedVersionField] = d.version
		}
		// Only the first chunk stores the full content and annotations.
		if i == 0 {
			doc["content"] = d.content
			doc["annotations"] = annJSON
		}
		if _, err := s.client.Collection(col).Documents().Upsert(ctx, doc, &api.DocumentIndexParameters{}); err != nil {
			return 0, fmt.Errorf("index chunk %d of %s: %w", i, d.path, err)
		}
	}
	// Only now remove the chunks an older, longer version left behind.
	stale := fmt.Sprintf("path:=%s && chunk_index:>=%d", quoteFilter(d.path), len(chunks))
	if _, err := s.client.Collection(col).Documents().Delete(ctx, &api.DeleteDocumentsParams{FilterBy: &stale}); err != nil && !isNotFound(err) {
		return len(chunks), fmt.Errorf("delete stale chunks of %s: %w", d.path, err)
	}
	return len(chunks), nil
}

// docFromRow is a stored doc in the shape GetDoc returns.
func docFromRow(sc Scope, d *repository.KBDocument) *Doc {
	tags := d.Tags
	if tags == nil {
		tags = []string{}
	}
	chunkText := d.Content
	if chunks := chunkMarkdown(d.Content, 500); len(chunks) > 0 {
		chunkText = chunks[0]
	}
	return &Doc{
		ID: docID(sc, d.Path, 0), ProjectID: sc.projectID(), Scope: sc.Kind, ScopeID: sc.ID,
		Reason: d.Reason, Path: d.Path, Title: d.Title, Content: d.Content, Tags: tags, Author: d.Author,
		ChunkText: chunkText, UpdatedAt: d.UpdatedAt.Unix(), Annotations: kbAnnotationsOrEmpty(d.Annotations),
	}
}

// summaryFromRow is a stored doc in the shape WriteDoc returns.
func summaryFromRow(sc Scope, d *repository.KBDocument) *DocSummary {
	tags := d.Tags
	if tags == nil {
		tags = []string{}
	}
	return &DocSummary{
		ID: docID(sc, d.Path, 0), ProjectID: sc.projectID(), Scope: sc.Kind, ScopeID: sc.ID,
		Reason: d.Reason, Path: d.Path, Title: d.Title, Tags: tags, Author: d.Author, UpdatedAt: d.UpdatedAt.Unix(),
	}
}

// ---- Internal helpers ----

// deleteByPath removes every chunk of a doc. Each collection holds one KB,
// so the path alone identifies the doc.
func (s *Service) deleteByPath(ctx context.Context, col string, path string) error {
	filterBy := "path:=" + quoteFilter(path)
	_, err := s.client.Collection(col).Documents().Delete(ctx, &api.DeleteDocumentsParams{
		FilterBy: &filterBy,
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete chunks for %s: %w", path, err)
	}
	return nil
}

// quoteFilter quotes a value for a Typesense filter_by expression, so paths
// containing spaces, commas, or operators match exactly.
func quoteFilter(v string) string {
	return "`" + strings.ReplaceAll(v, "`", "") + "`"
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "Not Found")
}

func mapToDoc(raw interface{}) *Doc {
	m, ok := raw.(*map[string]interface{})
	if !ok {
		if m2, ok2 := raw.(map[string]interface{}); ok2 {
			return mapToDocFromMap(m2)
		}
		return &Doc{}
	}
	return mapToDocFromMap(*m)
}

func mapToDocFromMap(m map[string]interface{}) *Doc {
	doc := &Doc{}
	if v, ok := m["id"].(string); ok {
		doc.ID = v
	}
	if v, ok := m["project_id"].(float64); ok {
		doc.ProjectID = int(v)
	}
	if v, ok := m["path"].(string); ok {
		doc.Path = v
	}
	if v, ok := m["reason"].(string); ok {
		doc.Reason = v
	}
	if v, ok := m["title"].(string); ok {
		doc.Title = v
	}
	if v, ok := m["content"].(string); ok {
		doc.Content = v
	}
	if v, ok := m["chunk_text"].(string); ok {
		doc.ChunkText = v
	}
	if v, ok := m["author"].(string); ok {
		doc.Author = v
	}
	if v, ok := m["chunk_index"].(float64); ok {
		doc.ChunkIndex = int(v)
	}
	if v, ok := m["updated_at"].(float64); ok {
		doc.UpdatedAt = int64(v)
	}
	if raw, ok := m["tags"]; ok {
		switch v := raw.(type) {
		case []interface{}:
			for _, t := range v {
				if s, ok := t.(string); ok {
					doc.Tags = append(doc.Tags, s)
				}
			}
		case []string:
			doc.Tags = v
		}
	}
	if doc.Tags == nil {
		doc.Tags = []string{}
	}
	// Deserialize annotations — can be JSON string (new collections with schema field)
	// or direct slice (existing collections without schema field)
	if raw, ok := m["annotations"]; ok {
		switch v := raw.(type) {
		case string:
			if err := json.Unmarshal([]byte(v), &doc.Annotations); err == nil {
				if doc.Annotations == nil {
					doc.Annotations = []Annotation{}
				}
			}
		case []interface{}:
			// Direct slice from Typesense — marshal to JSON then unmarshal to typed slice
			if data, err := json.Marshal(v); err == nil {
				if err := json.Unmarshal(data, &doc.Annotations); err == nil {
					if doc.Annotations == nil {
						doc.Annotations = []Annotation{}
					}
				}
			}
		}
	}
	return doc
}

// getLatestAnnotation returns the most recent unresolved annotation's text
// (first 80 characters), the preview search results show. Resolved notes
// are already worked into the doc, so they aren't previewed.
// The annotations slice is expected to be ordered with the most recent at the end.
func getLatestAnnotation(anns []Annotation) string {
	for i := len(anns) - 1; i >= 0; i-- {
		if !anns[i].IsResolved {
			return trimExcerpt(anns[i].Text, 80)
		}
	}
	return ""
}

func trimExcerpt(s string, maxChars int) string {
	if utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxChars]) + "…"
}

// chunkMarkdown splits markdown content into chunks of approximately maxTokens tokens.
// Simple word-based splitting — good enough for semantic chunking.
// Respects paragraph boundaries where possible.
func chunkMarkdown(content string, maxTokens int) []string {
	if content == "" {
		return nil
	}

	// Split by double-newline (paragraph boundary).
	paragraphs := strings.Split(content, "\n\n")
	var chunks []string
	var current strings.Builder
	currentTokens := 0

	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		paraTokens := estimateTokens(para)

		if currentTokens+paraTokens > maxTokens && currentTokens > 0 {
			chunks = append(chunks, strings.TrimSpace(current.String()))
			current.Reset()
			currentTokens = 0
		}

		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(para)
		currentTokens += paraTokens
	}

	if current.Len() > 0 {
		chunks = append(chunks, strings.TrimSpace(current.String()))
	}

	return chunks
}

// estimateTokens is a rough token count: ~0.75 tokens per word.
func estimateTokens(s string) int {
	words := len(strings.Fields(s))
	tokens := (words * 4) / 3
	if tokens < 1 {
		return 1
	}
	return tokens
}
