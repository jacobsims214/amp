package main

import (
	"strings"
	"testing"
)

// amp_design_context resolves the design layers for a project: the org's
// design system (org KB design-system/*), the project's additions, and what's
// missing.
func TestDesignContext_Layers(t *testing.T) {
	w := newRBACWorld(t)
	if w.s.kb == nil {
		t.Skip("needs TEST_TYPESENSE_URL")
	}
	pid := float64(w.s.projectID)
	alice := w.as(w.alice)

	// No org design system yet: it says so, and how to get one.
	out := alice.call(t, "amp_design_context", map[string]any{"project_id": pid})
	if !strings.Contains(strings.Join(toStrings(out["next"]), " "), "has no design system yet") {
		t.Fatalf("missing-org next: %v", out["next"])
	}

	// An org admin publishes the org design system; the project adds an accent.
	w.as(w.orgadmin).call(t, "amp_kb_write", map[string]any{"scope": "org", "project_id": pid, "path": "design-system/README.md",
		"title": "Acme design system", "content": "# Acme design system\n\nVersion 1. Calm, dense, precise.", "tags": []any{"design-system"},
		"reason": "The org's design system applies to every Acme product; built in the Design System project."})
	w.as(w.orgadmin).call(t, "amp_kb_write", map[string]any{"scope": "org", "project_id": pid, "path": "design-system/tokens.md",
		"title": "Tokens", "content": "--accent: #0f766e", "tags": []any{"design-system"},
		"reason": "The org's design system applies to every Acme product; built in the Design System project."})
	alice.call(t, "amp_kb_write", map[string]any{"project_id": pid, "path": "design-system/README.md",
		"title": "Storefront additions", "content": "Extends Acme v1. Promotions accent #c2410c.", "tags": []any{"design-system"}})
	alice.call(t, "amp_kb_write", map[string]any{"project_id": pid, "path": "architecture/api.md",
		"title": "Not design", "content": "x", "tags": []any{"arch"}})

	out = alice.call(t, "amp_design_context", map[string]any{"project_id": pid})
	org := out["org"].(map[string]any)
	if paths := docPaths(org); strings.Join(paths, ",") != "design-system/README.md,design-system/tokens.md" {
		t.Fatalf("org docs: %v", paths)
	}
	if !strings.Contains(org["readme"].(string), "Calm, dense, precise") {
		t.Fatalf("org readme: %v", org["readme"])
	}
	if paths := docPaths(out["project"].(map[string]any)); strings.Join(paths, ",") != "design-system/README.md" {
		t.Fatalf("project docs (design-system only): %v", paths)
	}
	if next := strings.Join(toStrings(out["next"]), " "); !strings.Contains(next, "Read the org design system") || !strings.Contains(next, "override the org's") {
		t.Fatalf("next: %v", next)
	}

	// Someone outside the project can't read its design context.
	if msg := w.as(w.outsider).callExpectError(t, "amp_design_context", map[string]any{"project_id": pid}); msg == "" {
		t.Fatal("outsider read the design context")
	}
}

func docPaths(layer map[string]any) []string {
	var out []string
	for _, d := range layer["docs"].([]any) {
		out = append(out, d.(map[string]any)["path"].(string))
	}
	return out
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}
