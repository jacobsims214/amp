package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/kb"
)

// The AMP guide: the repo's kb/ folder, synced into the global KB.

const guideDir = "../../../kb"

// The shipped guide parses, has its start page, and its cross-references
// point at pages that exist.
func TestGuide_ShippedDocs(t *testing.T) {
	docs, err := kb.ReadGuide(guideDir)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, d := range docs {
		paths[d.Path] = true
		if d.Title == "" || strings.TrimSpace(d.Content) == "" {
			t.Errorf("%s: empty title or content", d.Path)
		}
		if strings.HasPrefix(d.Content, "---") {
			t.Errorf("%s: front matter left in the content", d.Path)
		}
	}
	if !paths["start-here.md"] {
		t.Fatal("no start-here.md: amp_guide with no topic returns it")
	}
	if paths["README.md"] {
		t.Error("README.md is for people editing the guide and shouldn't be synced")
	}
	// References to guide pages: top-level pages, or pages in one of the
	// guide's folders (not a project's architecture/ doc or the repo's docs/).
	dirs := map[string]bool{"": true}
	for p := range paths {
		dirs[filepath.Dir(p)+"/"] = true
	}
	ref := regexp.MustCompile("`((?:[a-z-]+/)?)([a-z-]+\\.md)`")
	for _, d := range docs {
		for _, m := range ref.FindAllStringSubmatch(d.Content, -1) {
			if dirs[m[1]] && !paths[m[1]+m[2]] && m[1]+m[2] != "README.md" {
				t.Errorf("%s refers to %s, which isn't a guide page", d.Path, m[1]+m[2])
			}
		}
	}
}

func TestGuide_Sync(t *testing.T) {
	s := setupMCP(t)
	if s.kb == nil {
		t.Skip("TEST_TYPESENSE_URL not set")
	}
	ctx := context.Background()
	t.Cleanup(func() { _ = s.kb.Drop(ctx, kb.Global()) })
	_ = s.kb.Drop(ctx, kb.Global())

	dir := t.TempDir()
	put := func(path, body string) {
		t.Helper()
		p := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("start-here.md", "---\ntitle: Start\ntags: [onboarding, start]\n---\n\n# Start here\nWelcome.\n")
	put("working/handover.md", "# Handing over\nMove tickets with a reason.\n")
	put("README.md", "not synced")

	res, err := s.kb.SyncGlobal(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != 2 || res.Unchanged != 0 || len(res.Deleted) != 0 {
		t.Fatalf("first sync: %+v", res)
	}
	doc, err := s.kb.GetDoc(ctx, kb.Global(), "start-here.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != "Start" || !slices.Equal(doc.Tags, []string{"onboarding", "start"}) || !strings.HasPrefix(doc.Content, "# Start here") {
		t.Fatalf("front matter not applied: %q %v %q", doc.Title, doc.Tags, doc.Content)
	}
	if doc, _ := s.kb.GetDoc(ctx, kb.Global(), "working/handover.md"); doc == nil || doc.Title != "Handing over" {
		t.Fatalf("title from heading: %+v", doc)
	}

	// Same files again: nothing written.
	if res, err = s.kb.SyncGlobal(ctx, dir); err != nil || len(res.Written) != 0 || res.Unchanged != 2 {
		t.Fatalf("resync: %+v %v", res, err)
	}

	// A changed page is rewritten and a removed one is deleted.
	put("start-here.md", "# Start here\nWelcome, again.\n")
	_ = os.Remove(filepath.Join(dir, "working/handover.md"))
	if res, err = s.kb.SyncGlobal(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Written, []string{"start-here.md"}) || !slices.Equal(res.Deleted, []string{"working/handover.md"}) {
		t.Fatalf("change sync: %+v", res)
	}

	// An empty folder is a packaging mistake: it never wipes the guide.
	if _, err := s.kb.SyncGlobal(ctx, t.TempDir()); err == nil {
		t.Fatal("empty folder synced")
	}
	if n, _ := s.kb.CountDocs(ctx, kb.Global()); n != 1 {
		t.Fatalf("guide has %d docs after an empty sync, want 1", n)
	}
}

// Every user reads the guide; nobody changes it through AMP.
func TestGuide_MCP(t *testing.T) {
	w := newRBACWorld(t)
	if w.s.kb == nil {
		t.Skip("TEST_TYPESENSE_URL not set")
	}
	ctx := context.Background()
	t.Cleanup(func() { _ = w.s.kb.Drop(ctx, kb.Global()) })
	_ = w.s.kb.Drop(ctx, kb.Global())

	// Before the sync lands: a clear "not loaded yet".
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_guide", map[string]interface{}{}), "isn't loaded yet")

	if _, err := w.s.kb.SyncGlobal(ctx, guideDir); err != nil {
		t.Fatal(err)
	}

	// No topic: the onboarding script and the page index, for anyone,
	// including someone in another org.
	for _, u := range []*auth.Identity{w.alice, w.outsider} {
		got := w.as(u).call(t, "amp_guide", map[string]interface{}{})
		if got["path"] != "start-here.md" || !strings.Contains(got["content"].(string), "onboarding script") {
			t.Fatalf("no-topic guide: %v", got["path"])
		}
		if pages, _ := got["pages"].([]interface{}); len(pages) < 10 {
			t.Fatalf("page index has %d pages", len(pages))
		}
	}

	// A page by path, and a topic by search.
	if got := w.as(w.alice).call(t, "amp_guide", map[string]interface{}{"topic": "working/questions"}); got["path"] != "working/questions.md" {
		t.Fatalf("by path: %v", got["path"])
	}
	// Questions people actually ask land on the page about it, not on a
	// page that merely mentions it (start-here mentions everything).
	for q, want := range map[string]string{
		"how do I hand my tickets to someone":            "working/changes-outside-amp.md",
		"my worker seems stuck":                          "troubleshooting.md",
		"what did the agents cost, can we capitalize it": "visibility/reports-and-spend.md",
		"trace a commit back to its ticket":              "visibility/traceability.md",
	} {
		got := w.as(w.alice).call(t, "amp_guide", map[string]interface{}{"topic": q})
		if got["path"] != want {
			t.Errorf("%q → %v, want %s (also %v)", q, got["path"], want, got["also"])
		}
		seen := map[interface{}]bool{got["path"]: true}
		for _, a := range got["also"].([]interface{}) {
			p := a.(map[string]interface{})["path"]
			if seen[p] {
				t.Errorf("%q: %v listed twice", q, p)
			}
			seen[p] = true
		}
	}

	// The KB tools read it as scope global...
	list := w.as(w.outsider).call(t, "amp_kb_list", map[string]interface{}{"scope": "global"})
	if list["count"].(float64) < 10 {
		t.Fatalf("amp_kb_list global: %v", list["count"])
	}
	w.as(w.alice).call(t, "amp_kb_get", map[string]interface{}{"scope": "global", "path": "concepts.md"})

	// ...and refuse every change, annotations included (they'd be seen by
	// every org).
	write := map[string]interface{}{"scope": "global", "path": "x.md", "title": "x", "content": "x", "tags": []interface{}{"x"}}
	wantErr(t, w.as(w.root).callExpectError(t, "amp_kb_write", write), "read-only", "AMP repo")
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_kb_delete", map[string]interface{}{"scope": "global", "path": "concepts.md"}), "read-only")
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_kb_annotate", map[string]interface{}{"scope": "global", "path": "concepts.md", "text": "hi"}), "read-only")

	// And the project's KB sources list it last.
	src := w.as(w.alice).call(t, "amp_kb_sources", map[string]interface{}{"project_id": float64(w.s.projectID)})
	kbs, _ := src["sources"].([]interface{})
	if len(kbs) == 0 || kbs[len(kbs)-1].(map[string]interface{})["scope"] != "global" {
		t.Fatalf("sources: %v", src)
	}
}
