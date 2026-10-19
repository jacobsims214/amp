package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
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
	s.registerDesignSystem(mcp)
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

func (s *Server) registerDesignSystem(mcp *server.MCPServer) {
	ctx := context.Background()
	mcp.AddTool(tool("amp_design_system_start",
		"Start (or resume) building a design system with AMP's design system process, the same every time. scope \"org\": the org's "+
			"design system, built in the org's Design System project (created on first use, metadata purpose=design-system) and "+
			"published to the org KB; only org owners and admins. scope \"project\": a product project's layer on top of the org's "+
			"(only differences), built in that project and published to its KB; the org must have published first. It opens the "+
			"next version as an epic of phase tickets from the template, wired with dependencies, each ending in an artifact the "+
			"user approves, the last one publishing with amp_design_system_publish. If a version is already being built it returns "+
			"that one. Then check and get the plan approved like any plan, and dispatch the ready phase tickets. Read amp_guide "+
			"design/building-a-design-system.md first.",
		props{
			"scope":      str("REQUIRED. \"org\" or \"project\"."),
			"project_id": num("For scope project: the product project. For scope org: optional, any project in the org (to pick the org)."),
			"org_id":     num("For scope org: the org, if project_id isn't given and you're in several orgs."),
			"team_id":    num("For scope org, first time only: the team that will own the Design System project (default: your team in the org)."),
			"code":       str("For scope org, first time only: the Design System project's code (2-6 letters; default from the org slug + DS)."),
			"brief":      str("Optional. What this version is for, in the user's words (\"first version: unify the storefront and back office\")."),
		}), wrap(ctx, s.open(s.designSystemStart)))

	mcp.AddTool(tool("amp_design_system_publish",
		"Publish the design system version being built in a project (the org's Design System project, or a product project for its "+
			"layer). The ONLY way design-system/ docs are written. Refuses until every phase ticket of the version has an artifact a "+
			"person approved. Org scope: only org owners and admins. Writes the docs to the KB, stamps the README with the version, "+
			"layer, source project, approved artifacts and changelog, keeps the approved artifacts, and records the version; every "+
			"project's amp_design_context sees it at once.",
		props{
			"project_id": num("REQUIRED. The project the version was built in."),
			"docs": map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"},
				"description": "REQUIRED. Doc name → full markdown. Org: README.md, tokens.md, components.md, patterns.md, voice.md (brand.md optional). Project: README.md (which org version it extends, what it changes), plus tokens.md / components.md / patterns.md with only the differences."},
			"changelog": str("REQUIRED. One line: what this version is or changes."),
		}), wrap(ctx, s.guard(authz.Member, s.designSystemPublish)))
}

func (s *Server) designSystemStart(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	p := caller(ctx).p
	var res *service.StartDesignSystemResult
	var err error
	switch optStr(args, "scope", "") {
	case "org":
		orgID := optInt(args, "org_id", 0)
		if pid := optInt(args, "project_id", 0); orgID == 0 && pid != 0 {
			proj, err := s.repo.GetProject(ctx, pid)
			if err != nil || proj.OrgID == nil {
				return nil, fmt.Errorf("project %d not found, or not in an org", pid)
			}
			if _, err := s.svc.Authz.RequireProject(ctx, p, pid, authz.Member); err != nil {
				return nil, err
			}
			orgID = *proj.OrgID
		}
		if orgID == 0 {
			orgs, err := s.repo.ListOrgs(ctx, p.UserRef())
			if err != nil {
				return nil, err
			}
			if len(orgs) != 1 {
				return nil, fmt.Errorf("pass org_id (or a project_id in the org): you're in %d orgs", len(orgs))
			}
			orgID = orgs[0].ID
		}
		res, err = s.svc.StartOrgDesignSystem(ctx, p, orgID, optIntPtr(args, "team_id"), strings.ToUpper(optStr(args, "code", "")), optStr(args, "brief", ""))
	case "project":
		pid, perr := reqInt(args, "project_id")
		if perr != nil {
			return nil, perr
		}
		if err := s.checkProjectNotArchived(ctx, pid); err != nil {
			return nil, err
		}
		res, err = s.svc.StartProjectDesignSystem(ctx, p, pid, optStr(args, "brief", ""))
	default:
		return nil, fmt.Errorf("scope must be \"org\" or \"project\"")
	}
	if err != nil {
		return nil, err
	}
	if res.CreatedProj {
		s.hub.Publish(domain.Event{Type: domain.EventProjectCreated, ProjectID: res.Project.ID, Payload: res.Project, At: time.Now()})
	}
	next := fmt.Sprintf("Version %d of the %s design system is planned in project %d (%s), epic %d: one ticket per phase, chained. "+
		"Run amp_check_plan, present the plan to your user and get it approved, then dispatch each ready phase ticket to the designer. "+
		"Each phase publishes an artifact and waits for your user's review; the last ticket publishes with amp_design_system_publish.",
		res.Version, res.DesignSystem.Scope, res.Project.ID, res.Project.Name, res.EpicID)
	if res.AlreadyOpen {
		next = fmt.Sprintf("Version %d is already being built in project %d (%s), epic %d. Continue it: see amp_design_context for each phase's status.",
			res.Version, res.Project.ID, res.Project.Name, res.EpicID)
	}
	return jsonResult(map[string]interface{}{"result": res, "next": next})
}

func (s *Server) designSystemPublish(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	pid, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	raw, _ := args["docs"].(map[string]interface{})
	docs := map[string]string{}
	for k, v := range raw {
		str, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("docs[%q] must be markdown text", k)
		}
		docs[k] = str
	}
	st, err := s.svc.PublishDesignSystem(ctx, caller(ctx).p, pid, docs, optStr(args, "changelog", ""))
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"design_system": st,
		"next": "Published. Complete the publish ticket. Every project now gets this version from amp_design_context."})
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
	orgStatus, projStatus, err := s.svc.DesignSystemStatuses(ctx, p, pid)
	if err != nil {
		return nil, err
	}
	out["org_design_system"], out["project_design_system"] = orgStatus, projStatus
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
			"an org owner or admin builds one with amp_design_system_start scope \"org\" (amp_guide topic \"design/building-a-design-system.md\"). "+
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
		// design-system/ docs are written only by amp_design_system_publish (amp_kb_write refuses the path), so
		// point at the supported routes: the project layer once the org has published, decisions/ until then.
		msg := "This project has no design-system/ docs of its own: it uses the org's as is. If your work needs a " +
			"project-specific decision (a product accent, a component the org doesn't have), propose it to your user first. "
		if orgErr == nil && len(org.Docs) > 0 {
			msg += "Once agreed, a lasting one belongs in the project's design system layer, built with amp_design_system_start " +
				"scope \"project\" (amp_guide topic \"design/building-a-design-system.md\"); a one-off decision can go in the " +
				"project KB under decisions/ with amp_kb_write. "
		} else {
			msg += "Until the org publishes its design system (a project layer can only be built on top of it, with " +
				"amp_design_system_start scope \"project\"), record an agreed one-off decision in the project KB under decisions/ " +
				"with amp_kb_write. "
		}
		next = append(next, msg+"Don't write under design-system/ yourself: those docs are written only by publishing a design system version.")
	}
	out["next"] = next
	return jsonResult(out)
}
