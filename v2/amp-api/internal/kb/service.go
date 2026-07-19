// Package kb manages the per-project knowledge base backed by Typesense.
//
// Architecture:
//   - One Typesense collection per project: "kb_{project_id}"
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
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/typesense/typesense-go/v4/typesense"
	"github.com/typesense/typesense-go/v4/typesense/api"
	"github.com/typesense/typesense-go/v4/typesense/api/pointer"
)

// Service is the KB business logic layer.
type Service struct {
	client       *typesense.Client
	typesenseURL string
	typesenseKey string
	ollamaURL    string
	ollamaModel  string
	log          *slog.Logger
}

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
	ID          string      `json:"id"` // sha256(project_id:path)
	ProjectID   int         `json:"project_id"`
	Path        string      `json:"path"` // e.g. "architecture/auth.md"
	Title       string      `json:"title"`
	Content     string      `json:"content"` // full raw markdown
	Tags        []string    `json:"tags"`
	Author      string      `json:"author"`
	ChunkIndex  int         `json:"chunk_index"` // 0 for the canonical doc record
	ChunkText   string      `json:"chunk_text"`  // the searchable portion
	UpdatedAt   int64       `json:"updated_at"`  // unix timestamp
	Annotations []Annotation `json:"annotations"`
}

type Annotation struct {
	Author      string `json:"author"`
	Text        string `json:"text"`
	CreatedAt   int64  `json:"created_at"`
	IsResolved  bool   `json:"is_resolved"`
}

type DocSummary struct {
	ID        string   `json:"id"`
	ProjectID int      `json:"project_id"`
	Path      string   `json:"path"`
	Title     string   `json:"title"`
	Tags      []string `json:"tags"`
	Author    string   `json:"author"`
	UpdatedAt int64    `json:"updated_at"`
}

type SearchResult struct {
	Path            string   `json:"path"`
	Title           string   `json:"title"`
	Tags            []string `json:"tags"`
	Excerpt         string   `json:"excerpt"` // best-matching chunk_text, trimmed
	Author          string   `json:"author"`
	UpdatedAt       int64    `json:"updated_at"`
	Score           float64  `json:"score"`
	AnnotationCount int      `json:"annotation_count"`
	LatestAnnotation string  `json:"latest_annotation"`
	RecencyLabel    string   `json:"recency_label"`
}

type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// StatusInfo holds health metrics for the knowledge base.
type StatusInfo struct {
	TotalDocs           int            `json:"total_docs"`
	StaleDocs           int            `json:"stale_docs"`
	DocsWithAnnotations int            `json:"docs_with_annotations"`
	TotalAnnotations    int            `json:"total_annotations"`
	UnresolvedAnnotations int          `json:"unresolved_annotations"`
	DocsByTag           map[string]int `json:"docs_by_tag"`
}

// ---- Collection naming ----

func collectionName(projectID int) string {
	return fmt.Sprintf("kb_%d", projectID)
}

func docID(projectID int, path string, chunkIndex int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%d", projectID, path, chunkIndex)))
	return fmt.Sprintf("%x", h[:8]) // 16 hex chars, short and stable
}

// ---- Collection bootstrap ----

// ensureCollection creates the Typesense collection for a project if it doesn't exist.
// Safe to call on every write — checks existence first.
func (s *Service) ensureCollection(ctx context.Context, projectID int) error {
	name := collectionName(projectID)

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
	s.log.Info("created KB collection", "project_id", projectID, "collection", name, "semantic", s.ollamaURL != "")
	return nil
}

// ---- Write ----

// WriteDoc upserts a document into the KB.
// The doc is chunked automatically; Typesense generates embeddings via Ollama.
// On subsequent writes to the same path, all old chunks are deleted and new ones inserted.
func (s *Service) WriteDoc(ctx context.Context, projectID int, path, title, content, author string, tags []string) (*DocSummary, error) {
	if err := s.ensureCollection(ctx, projectID); err != nil {
		return nil, err
	}

	col := collectionName(projectID)
	now := time.Now().Unix()

	if tags == nil {
		tags = []string{}
	}

	// Delete existing chunks for this path before inserting new ones.
	_ = s.deleteByPath(ctx, col, projectID, path)

	// Chunk the content.
	chunks := chunkMarkdown(content, 500) // ~500 tokens per chunk
	if len(chunks) == 0 {
		chunks = []string{content}
	}

	// Index each chunk via Upsert (create-or-update by id).
	for i, chunk := range chunks {
		doc := map[string]interface{}{
			"id":                docID(projectID, path, i),
			"project_id":        projectID,
			"path":              path,
			"title":             title,
			"tags":              tags,
			"author":            author,
			"chunk_index":       i,
			"chunk_text":        chunk,
			"updated_at":        now,
			"annotation_count":  0,
			"latest_annotation": "",
		}
		// Only store the full content on the first chunk.
		if i == 0 {
			doc["content"] = content
		} else {
			doc["content"] = ""
		}

		_, err := s.client.Collection(col).Documents().Upsert(ctx, doc, &api.DocumentIndexParameters{})
		if err != nil {
			return nil, fmt.Errorf("index chunk %d of %s: %w", i, path, err)
		}
	}

	s.log.Info("doc written", "project_id", projectID, "path", path, "chunks", len(chunks))

	return &DocSummary{
		ID:        docID(projectID, path, 0),
		ProjectID: projectID,
		Path:      path,
		Title:     title,
		Tags:      tags,
		Author:    author,
		UpdatedAt: now,
	}, nil
}

// ---- Read ----

// GetDoc retrieves a document by path, returning the full content (chunk_index=0).
func (s *Service) GetDoc(ctx context.Context, projectID int, path string) (*Doc, error) {
	col := collectionName(projectID)
	id := docID(projectID, path, 0)

	raw, err := s.client.Collection(col).Document(id).Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("doc not found: %s/%s: %w", col, path, err)
	}
	return mapToDoc(raw), nil
}

// AnnotateDoc adds an annotation to a document.
// The upsert only updates the annotations field on chunk_index=0.
// Typesense will merge the fields.
func (s *Service) AnnotateDoc(ctx context.Context, projectID int, path, text, author string) (*Annotation, error) {
	doc, err := s.GetDoc(ctx, projectID, path)
	if err != nil {
		return nil, err
	}
	ann := &Annotation{
		Author:     author,
		Text:       text,
		CreatedAt:  time.Now().Unix(),
		IsResolved: false,
	}
	doc.Annotations = append(doc.Annotations, *ann)

	col := collectionName(projectID)
	_, err = s.client.Collection(col).Documents().Upsert(ctx, map[string]interface{}{
		"id":                doc.ID,
		"project_id":        doc.ProjectID,
		"path":              doc.Path,
		"title":             doc.Title,
		"content":           doc.Content,
		"tags":              doc.Tags,
		"author":            doc.Author,
		"chunk_index":       0,
		"chunk_text":        doc.ChunkText,
		"updated_at":        doc.UpdatedAt,
		"annotations":       doc.Annotations,
		"annotation_count":  len(doc.Annotations),
		"latest_annotation": getLatestAnnotation(doc.Annotations),
	}, &api.DocumentIndexParameters{})
	if err != nil {
		return nil, fmt.Errorf("annotate doc %s: %w", path, err)
	}

	// Propagate annotation count and latest annotation to all other chunks
	// so search results from any chunk show annotation data.
	count := len(doc.Annotations)
	latest := getLatestAnnotation(doc.Annotations)
	propFilter := fmt.Sprintf("project_id:=%d && path:=%s && chunk_index:>0", projectID, path)
	_, _ = s.client.Collection(col).Documents().Update(ctx, map[string]interface{}{
		"annotation_count":  count,
		"latest_annotation": latest,
	}, &api.UpdateDocumentsParams{
		FilterBy: &propFilter,
	})

	return ann, nil
}

// ListDocs returns summaries of all documents (no content).
// Optionally filtered by tag.
func (s *Service) ListDocs(ctx context.Context, projectID int, tag string) ([]DocSummary, error) {
	if err := s.ensureCollection(ctx, projectID); err != nil {
		return nil, err
	}
	col := collectionName(projectID)
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
func (s *Service) Search(ctx context.Context, projectID int, query string, tags []string, limit int, recencyBoost float64, minRecencyDays int) ([]SearchResult, error) {
	if err := s.ensureCollection(ctx, projectID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 3 // small default — agents call this from limited-context models
	}

	col := collectionName(projectID)

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

	hasEmbedding := s.collectionHasEmbedding(ctx, collectionName(projectID))

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
	for _, hit := range *results.Hits {
		if hit.Document == nil {
			continue
		}
		doc := mapToDoc(hit.Document)
		score := 0.0
		if hit.VectorDistance != nil {
			// Convert distance to similarity: lower distance = higher score
			score = float64(1.0 - *hit.VectorDistance)
		} else if hit.TextMatch != nil {
			score = float64(*hit.TextMatch)
		}
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
			Path:             doc.Path,
			Title:            doc.Title,
			Tags:             doc.Tags,
			Excerpt:          trimExcerpt(doc.ChunkText, 120),
			Author:           doc.Author,
			UpdatedAt:        doc.UpdatedAt,
			Score:            score,
			AnnotationCount:  annCount,
			LatestAnnotation: annLatest,
			RecencyLabel:     computeRecencyLabel(time.Unix(doc.UpdatedAt, 0)),
		})
	}
	return out, nil
}

// ---- Tags ----

// ListTags returns all tags in a project's KB with their document counts.
func (s *Service) ListTags(ctx context.Context, projectID int) ([]TagCount, error) {
	if err := s.ensureCollection(ctx, projectID); err != nil {
		return nil, err
	}
	col := collectionName(projectID)

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
func (s *Service) KBStatus(ctx context.Context, projectID int) (*StatusInfo, error) {
	if err := s.ensureCollection(ctx, projectID); err != nil {
		return nil, err
	}

	// List all docs (no tag filter)
	docs, err := s.ListDocs(ctx, projectID, "")
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
		fullDoc, err := s.GetDoc(ctx, projectID, summary.Path)
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
	tagCounts, err := s.ListTags(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}

	// Update docsByTag with actual counts from ListTags
	for _, tc := range tagCounts {
		docsByTag[tc.Tag] = tc.Count
	}

	return &StatusInfo{
		TotalDocs:           totalDocs,
		StaleDocs:           staleDocs,
		DocsWithAnnotations: docsWithAnnotations,
		TotalAnnotations:    totalAnnotations,
		UnresolvedAnnotations: unresolvedAnnotations,
		DocsByTag:           docsByTag,
	}, nil
}

// ---- Delete ----

// DeleteDoc removes all chunks for a document path.
func (s *Service) DeleteDoc(ctx context.Context, projectID int, path string) error {
	col := collectionName(projectID)
	return s.deleteByPath(ctx, col, projectID, path)
}

// DeleteProject drops the entire Typesense collection for a project.
func (s *Service) DeleteProject(ctx context.Context, projectID int) error {
	col := collectionName(projectID)
	_, err := s.client.Collection(col).Delete(ctx)
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete collection %s: %w", col, err)
	}
	return nil
}

// Reindex re-triggers embedding generation for all documents by re-upserting them.
// This is useful when switching embedding models.
func (s *Service) Reindex(ctx context.Context, projectID int) error {
	docs, err := s.ListDocs(ctx, projectID, "")
	if err != nil {
		return err
	}
	for _, summary := range docs {
		full, err := s.GetDoc(ctx, projectID, summary.Path)
		if err != nil {
			s.log.Warn("reindex: skip doc", "path", summary.Path, "err", err)
			continue
		}
		if _, err := s.WriteDoc(ctx, projectID, full.Path, full.Title, full.Content, full.Author, full.Tags); err != nil {
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
// Returns false if Ollama is not running — collection is created keyword-only.
// When Ollama later becomes available, call Reindex to add embeddings.
func (s *Service) ollamaReachable(ctx context.Context) bool {
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, s.ollamaURL+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// SearchAPIKey returns the API key for the UI to search directly.
// For a local dev stack the admin key doubles as the search key.
func (s *Service) SearchAPIKey() string { return s.typesenseKey }

// TypesenseURL returns the Typesense URL for the frontend direct search.
func (s *Service) TypesenseURL() string { return s.typesenseURL }

// ---- Internal helpers ----

func (s *Service) deleteByPath(ctx context.Context, col string, projectID int, path string) error {
	filterBy := fmt.Sprintf("project_id:=%d && path:=%s", projectID, path)
	_, err := s.client.Collection(col).Documents().Delete(ctx, &api.DeleteDocumentsParams{
		FilterBy: &filterBy,
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete chunks for %s: %w", path, err)
	}
	return nil
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

// getLatestAnnotation returns the most recent annotation's text (first 80 chars).
// The annotations slice is expected to be ordered with the most recent at the end.
func getLatestAnnotation(anns []Annotation) string {
	if len(anns) == 0 {
		return ""
	}
	latest := anns[len(anns)-1] // last in slice = most recent
	if len(latest.Text) > 80 {
		return latest.Text[:80] + "…"
	}
	return latest.Text
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
