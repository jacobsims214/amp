// Package kb manages the knowledge bases backed by Typesense: one per
// project, plus one per team and one per org for knowledge shared across
// projects (see docs/kb-scopes-design.md).
//
// Architecture:
//   - One Typesense collection per KB: "kb_{project_id}", "kb_team_{id}", "kb_org_{id}"
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
}

// ErrReindexRunning is returned when a reindex of the same KB is already
// running, in this process or (with a ReindexLocker) on another replica.
var ErrReindexRunning = errors.New("a reindex of this knowledge base is already running")

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

// ensureCollection creates the Typesense collection for a project if it doesn't exist.
// Safe to call on every write — checks existence first.
func (s *Service) ensureCollection(ctx context.Context, sc Scope) error {
	name := collectionName(sc)

	// Check if it already exists.
	_, err := s.client.Collection(name).Retrieve(ctx)
	if err == nil {
		return nil // already exists
	}

	// Build the schema.
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

	_, err = s.client.Collections().Create(ctx, schema)
	if err != nil {
		return fmt.Errorf("create collection %s: %w", name, err)
	}
	s.log.Info("created KB collection", "kb", sc.String(), "collection", name, "semantic", s.ollamaURL != "")
	return nil
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
func (s *Service) GetDoc(ctx context.Context, sc Scope, path string) (*Doc, error) {
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

// ListDocs returns summaries of all documents (no content).
// Optionally filtered by tag.
func (s *Service) ListDocs(ctx context.Context, sc Scope, tag string) ([]DocSummary, error) {
	if err := s.ensureCollection(ctx, sc); err != nil {
		return nil, err
	}
	col := collectionName(sc)
	perPage := 250

	// Only fetch chunk_index=0 (canonical doc records)
	filterBy := "chunk_index:=0"
	if tag != "" {
		filterBy += fmt.Sprintf(" && tags:=%s", tag)
	}

	q := "*"
	results, err := s.client.Collection(col).Documents().Search(ctx, &api.SearchCollectionParams{
		Q:             &q,
		QueryBy:       pointer.String("title"),
		FilterBy:      pointer.String(filterBy),
		SortBy:        pointer.String("updated_at:desc"),
		PerPage:       &perPage,
		ExcludeFields: pointer.String("content,embedding,chunk_text"),
	})
	if err != nil {
		return nil, fmt.Errorf("list docs: %w", err)
	}

	out := make([]DocSummary, 0)
	if results.Hits == nil {
		return out, nil
	}
	for _, hit := range *results.Hits {
		if hit.Document == nil {
			continue
		}
		doc := mapToDoc(hit.Document)
		out = append(out, DocSummary{
			ID:        doc.ID,
			ProjectID: doc.ProjectID,
			Scope:     sc.Kind,
			ScopeID:   sc.ID,
			Reason:    doc.Reason,
			Path:      doc.Path,
			Title:     doc.Title,
			Tags:      doc.Tags,
			Author:    doc.Author,
			UpdatedAt: doc.UpdatedAt,
		})
	}
	return out, nil
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
// collection.
func (s *Service) CountDocs(ctx context.Context, sc Scope) (int, error) {
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
// stale doc counts, annotation statistics, and tag distribution.
func (s *Service) KBStatus(ctx context.Context, sc Scope) (*StatusInfo, error) {
	if err := s.ensureCollection(ctx, sc); err != nil {
		return nil, err
	}

	// List all docs (no tag filter)
	docs, err := s.ListDocs(ctx, sc, "")
	if err != nil {
		return nil, fmt.Errorf("list docs: %w", err)
	}

	// Initialize counters
	totalDocs := len(docs)
	staleDocs := 0
	docsWithAnnotations := 0
	totalAnnotations := 0
	unresolvedAnnotations := 0
	docsByTag := make(map[string]int)

	// 90 days in seconds
	ninetyDaysAgo := time.Now().AddDate(0, 0, -90).Unix()

	// Process each doc
	for _, summary := range docs {
		// Get full doc to access annotations
		fullDoc, err := s.GetDoc(ctx, sc, summary.Path)
		if err != nil {
			s.log.Warn("KBStatus: skip doc", "path", summary.Path, "err", err)
			continue
		}

		// Count stale docs (updated_at > 90 days ago)
		if fullDoc.UpdatedAt < ninetyDaysAgo {
			staleDocs++
		}

		// Count docs with annotations
		if len(fullDoc.Annotations) > 0 {
			docsWithAnnotations++
		}

		// Count total and unresolved annotations
		for _, ann := range fullDoc.Annotations {
			totalAnnotations++
			if !ann.IsResolved {
				unresolvedAnnotations++
			}
		}

		// Count docs by tag
		for _, tag := range fullDoc.Tags {
			docsByTag[tag]++
		}
	}

	// Get tag distribution from ListTags
	tagCounts, err := s.ListTags(ctx, sc)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}

	// Update docsByTag with actual counts from ListTags
	for _, tc := range tagCounts {
		docsByTag[tc.Tag] = tc.Count
	}

	return &StatusInfo{
		TotalDocs:             totalDocs,
		StaleDocs:             staleDocs,
		DocsWithAnnotations:   docsWithAnnotations,
		TotalAnnotations:      totalAnnotations,
		UnresolvedAnnotations: unresolvedAnnotations,
		DocsByTag:             docsByTag,
	}, nil
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

// Drop deletes a whole knowledge base (e.g. when its team or org is deleted).
func (s *Service) Drop(ctx context.Context, sc Scope) error {
	col := collectionName(sc)
	_, err := s.client.Collection(col).Delete(ctx)
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete collection %s: %w", col, err)
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
		return s.reindexFromStore(ctx, sc, rebuild)
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
		if _, err := s.client.Collection(col).Delete(ctx); err != nil {
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
	StreamKBDocuments(ctx context.Context, scope repository.KBScope, fn func(repository.KBDocument) error) error
	IsKBBackfillComplete(ctx context.Context, scope repository.KBScope) (bool, error)
	DeleteKBDocument(ctx context.Context, scope repository.KBScope, path, by string) error
	RestoreKBDocument(ctx context.Context, scope repository.KBScope, path, by string) (*repository.KBDocument, error)
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
	done, err := s.store.IsKBBackfillComplete(ctx, rs)
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
	old, err := s.GetDoc(ctx, sc, path)
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

// reindexFromStore re-indexes every live doc of a stored KB from Postgres,
// keeping each doc's own updated_at. Until the KB's backfill has completed,
// the docs the index lists are first copied into Postgres
// (insert-if-missing), and a rebuild is refused while Postgres holds fewer
// docs than the index: dropping the collection would delete the only copy
// of the rest. (#1990 replaces the drop with build-then-swap.)
func (s *Service) reindexFromStore(ctx context.Context, sc Scope, rebuild bool) error {
	rs := storeScope(sc)
	col := collectionName(sc)
	done, err := s.store.IsKBBackfillComplete(ctx, rs)
	if err != nil {
		return err
	}
	if !done {
		summaries, err := s.ListDocs(ctx, sc, "")
		if err != nil {
			return err
		}
		for _, sum := range summaries {
			if err := s.adoptIndexed(ctx, sc, sum.Path); err != nil {
				return fmt.Errorf("reindex: %w", err)
			}
		}
	}
	if rebuild {
		indexed, err := s.CountDocs(ctx, sc)
		if err != nil {
			return err
		}
		stored, err := s.store.CountKBDocuments(ctx, rs)
		if err != nil {
			return err
		}
		if stored < indexed {
			s.log.Error("reindex: not rebuilding the collection: postgres holds fewer docs than the index, so dropping it would lose the rest; re-indexing in place",
				"collection", col, "stored", stored, "indexed", indexed)
		} else {
			s.log.Info("reindex: upgrading collection to semantic search (was keyword-only)", "collection", col)
			if _, err := s.client.Collection(col).Delete(ctx); err != nil {
				s.log.Warn("reindex: failed to drop keyword-only collection, continuing with existing schema", "collection", col, "err", err)
			} else if err := s.ensureCollection(ctx, sc); err != nil {
				return fmt.Errorf("recreate collection with embeddings: %w", err)
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
	failed := 0
	for _, p := range paths {
		if err := s.indexFromStore(ctx, sc, p); err != nil {
			failed++
			s.log.Warn("reindex: index failed", "kb", sc.String(), "path", p, "err", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("reindex %s: %d of %d docs failed to index", sc, failed, len(paths))
	}
	return nil
}

// indexedDoc is what the search index holds for one doc.
type indexedDoc struct {
	path, title, content, author, reason string
	tags                                 []string
	annotations                          []Annotation
	updatedAt                            int64
}

func indexedFromRow(d *repository.KBDocument) indexedDoc {
	return indexedDoc{
		path: d.Path, title: d.Title, content: d.Content, author: d.Author, reason: d.Reason,
		tags: d.Tags, annotations: d.Annotations, updatedAt: d.UpdatedAt.Unix(),
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
	col := collectionName(sc)
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
