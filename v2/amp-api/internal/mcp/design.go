package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/service"
)

// Design work layers three knowledge bases, most general first: AMP's design
// guide (global KB, design/*), the org's design system (org KB,
// design-system/*), and the project's additions (project KB,
// design-system/*). amp_design_context resolves all three in one call, so
// an agent can't skip a layer.

const designSystemPrefix = "design-system/"

type designDoc struct {
	Path      string `json:"path"`
	Title     string `json:"title"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

type designLayer struct {
	KB     *service.KB `json:"kb,omitempty"`
	Docs   []designDoc `json:"docs"`
	Readme string      `json:"readme,omitempty"` // design-system/README.md, in full
}

func (s *Server) registerDesign(mcp *server.MCPServer) {
	ctx := context.Background()
	mcp.AddTool(tool("amp_design_context",
		"Everything design work in a project must follow, in one call, most general first: (1) craft: AMP's design guide "+
			"pages (read design/start-here.md and the pages it lists), (2) org: the org's design system in the org KB "+
			"(design-system/*: tokens, components, patterns, voice), (3) project: this project's additions and overrides in "+
			"the project KB (design-system/*). Where layers disagree the more specific one wins: project over org over the "+
			"guide's defaults. Call it before any mockup, screen, styling or visual asset, then read the docs it lists with "+
			"amp_guide (craft) and amp_kb_get (scope org / project). It also says what's missing and what to do about it.",
		props{
			"project_id": num("REQUIRED. The project the design work is for."),
		}), wrap(ctx, s.guard(authz.Member, s.designContext)))
}

func (s *Server) designContext(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	pid, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	if err := s.requireKB(); err != nil {
		return nil, err
	}
	p := caller(ctx).p
	layer := func(ref service.KBRef, prefix string) (*designLayer, error) {
		k, err := s.svc.ResolveKB(ctx, p, ref)
		if err != nil {
			return nil, err
		}
		docs, err := s.kb.ListDocs(ctx, k.Scope, "")
		if err != nil {
			return nil, err
		}
		out := &designLayer{KB: k, Docs: []designDoc{}}
		for _, d := range docs {
			if strings.HasPrefix(d.Path, prefix) {
				out.Docs = append(out.Docs, designDoc{Path: d.Path, Title: d.Title, UpdatedAt: d.UpdatedAt})
			}
		}
		sort.Slice(out.Docs, func(i, j int) bool { return out.Docs[i].Path < out.Docs[j].Path })
		if prefix == designSystemPrefix {
			if doc, err := s.kb.GetDoc(ctx, k.Scope, designSystemPrefix+"README.md"); err == nil && doc != nil {
				out.Readme = doc.Content
			}
		}
		return out, nil
	}

	craft, err := layer(service.KBRef{Scope: kb.ScopeGlobal}, "design/")
	if err != nil {
		return nil, fmt.Errorf("reading AMP's design guide: %w", err)
	}
	craft.KB = nil
	project, err := layer(service.KBRef{Scope: "project", ProjectID: pid}, designSystemPrefix)
	if err != nil {
		return nil, err
	}
	org, orgErr := layer(service.KBRef{Scope: "org", ProjectID: pid}, designSystemPrefix)

	out := map[string]interface{}{"project_id": pid, "craft": craft, "project": project}
	var next []string
	next = append(next, "Read the craft pages first: amp_guide with topic \"design/start-here.md\", then the pages it lists.")
	switch {
	case orgErr != nil:
		out["org"] = nil
		next = append(next, "This project isn't in an org (or its org KB can't be read: "+orgErr.Error()+"), so there's no org design system. "+
			"Use the product's existing tokens from its code, and say so in your artifact's note.")
	case len(org.Docs) == 0:
		out["org"] = org
		next = append(next, fmt.Sprintf("Org %q has no design system yet (nothing under design-system/ in its KB). Tell your user: "+
			"the way to get one is a Design System project for the org (amp_guide topic \"design/building-a-design-system.md\"). "+
			"Until then, follow the product's existing tokens from its code, and say in your artifact's note that it isn't "+
			"based on an org design system.", org.KB.Name))
	default:
		out["org"] = org
		next = append(next, "Read the org design system: amp_kb_get with scope \"org\" and project_id for each path under org.docs "+
			"(start with design-system/README.md, tokens and components). Use its tokens and components; don't invent new ones.")
	}
	if len(project.Docs) > 0 {
		next = append(next, "Then this project's additions: amp_kb_get (project scope) for each path under project.docs. They override the org's.")
	} else {
		next = append(next, "This project has no design-system/ docs of its own: it uses the org's as is. If your work needs a "+
			"project-specific decision (a product accent, a component the org doesn't have), propose it to your user and record "+
			"it in the project KB under design-system/ once agreed.")
	}
	out["next"] = next
	return jsonResult(out)
}
