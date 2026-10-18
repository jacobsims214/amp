package mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// SetAppURL tells the server AMP's public address, for links to artifacts.
func (s *Server) SetAppURL(u string) { s.svc.AppURL = u }

// SetArtifactRetention sets how long unkept artifacts live after their last version.
func (s *Server) SetArtifactRetention(d time.Duration) { s.svc.ArtifactRetention = d }

func (s *Server) registerArtifacts(mcp *server.MCPServer) {
	ctx := context.Background()
	mcp.AddTool(tool("amp_publish_artifact",
		"Publish something for people to look at in AMP: a UI mockup or clickable prototype (html), a diagram or icon (svg), "+
			"an image, a document (markdown or pdf), or plain text. AMP stores it, versions it, and shows it sandboxed to everyone on "+
			"the project, whatever harness they use; the result has its url. Make html a single self-contained file (inline CSS and JS; "+
			"CDN scripts and fonts are fine, other files aren't reachable). Pass artifact_id to publish a new version of one you "+
			"published before: it goes back into review. Each publish is noted on the ticket. To have a person review it, block the "+
			"ticket (amp_block_task) asking them to review the artifact at its url; their approval or requested changes answer the "+
			"question and come back as a comment on the ticket.",
		props{
			"task_id":        num("The ticket it's for (normally your own). Either task_id or project_id is required for a new artifact."),
			"project_id":     num("The project, for an artifact that isn't tied to a ticket."),
			"artifact_id":    num("Publish a new version of this artifact instead of a new one."),
			"title":          str("REQUIRED for a new artifact. What it is, e.g. 'Settings page mockup'. On a new version, replaces the title."),
			"kind":           str("REQUIRED for a new artifact: html, svg, image, markdown, pdf or text."),
			"content":        str("The content as text (html, svg, markdown, text)."),
			"content_base64": str("The content base64-encoded, for binary files (image, pdf). Use content or content_base64."),
			"note":           str("Optional. What this version is or changed, shown with it."),
		}), wrap(ctx, s.open(s.publishArtifact)))

	mcp.AddTool(tool("amp_review_artifact",
		"Record the user's verdict on an artifact's latest version: approve it, or request changes with what to change. Call it "+
			"only when the user gives that verdict in chat (\"looks good, approve it\", \"move Save to the bottom\"), never on your own "+
			"judgment; people can also review in AMP. The verdict is posted on the artifact's ticket, and if the ticket is waiting on "+
			"review it goes back to ready for the next dispatch. Agent accounts can't review.",
		props{
			"artifact_id": num("REQUIRED. Artifact ID."),
			"approve":     boolean("REQUIRED. true to approve, false to request changes."),
			"note":        str("What to change (required when approve is false), in the user's words with any detail the agent needs."),
		}), wrap(ctx, s.open(s.reviewArtifact)))

	mcp.AddTool(tool("amp_keep_artifact",
		"Keep an artifact so it never expires, or stop keeping it. Artifacts nobody keeps are deleted a while after their last "+
			"version (expires_at); people are warned first. Call it only when the user asks to keep (or let go of) something.",
		props{
			"artifact_id": num("REQUIRED. Artifact ID."),
			"keep":        boolean("REQUIRED. true to keep it, false to let it expire again."),
		}), wrap(ctx, s.open(s.keepArtifact)))

	mcp.AddTool(tool("amp_list_artifacts",
		"List a project's artifacts (designs, diagrams, images, documents), or one ticket's: title, kind, review status "+
			"(in_review, approved, changes_requested), latest and approved version, and url.",
		props{
			"project_id": num("The project. Either project_id or task_id is required."),
			"task_id":    num("Only this ticket's artifacts."),
		}), wrap(ctx, s.open(s.listArtifacts)))

	mcp.AddTool(tool("amp_get_artifact",
		"Get an artifact: its details, versions and review status, and the content of one version (the latest by default; "+
			"images come back as images you can see). Use it to build what a person approved (approved_version), or to compare "+
			"what you built with the design. Review feedback is in the ticket's comments.",
		props{
			"artifact_id":     num("REQUIRED. Artifact ID."),
			"version":         num("Optional. Which version; default the latest. Use approved_version to get what was approved."),
			"include_content": boolean("Optional, default true. False returns only the details and versions."),
		}), wrap(ctx, s.open(s.getArtifact)))
}

func (s *Server) publishArtifact(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	req := service.PublishArtifactRequest{
		ArtifactID: optInt(args, "artifact_id", 0),
		ProjectID:  optInt(args, "project_id", 0),
		TaskID:     optIntPtr(args, "task_id"),
		Title:      optStr(args, "title", ""),
		Kind:       strings.ToLower(strings.TrimSpace(optStr(args, "kind", ""))),
		Note:       optStr(args, "note", ""),
	}
	if b64 := optStr(args, "content_base64", ""); b64 != "" {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return nil, fmt.Errorf("content_base64 isn't valid base64")
		}
		req.Content = raw
	} else {
		req.Content = []byte(optStr(args, "content", ""))
	}
	if req.ArtifactID == 0 && req.TaskID == nil && req.ProjectID == 0 {
		return nil, fmt.Errorf("task_id or project_id is required (or artifact_id for a new version)")
	}
	if pid := s.artifactProject(ctx, req); pid != 0 {
		if err := s.checkProjectNotArchived(ctx, pid); err != nil {
			return nil, err
		}
	}
	a, err := s.svc.PublishArtifact(ctx, caller(ctx).p, req)
	if err != nil {
		return nil, err
	}
	s.hub.Publish(domain.Event{Type: domain.EventArtifactUpdated, ProjectID: a.ProjectID, Payload: a, At: a.UpdatedAt})
	next := "Published. To get it reviewed, block the ticket with amp_block_task, asking the person to review it"
	if req.ArtifactID == 0 && a.TaskID != nil {
		if others, err := s.repo.ListArtifacts(ctx, a.ProjectID, a.TaskID); err == nil && len(others) > 1 {
			next = fmt.Sprintf("Published as a NEW artifact, but ticket %d already had %d other(s): if this revises one of them, publish it as a new version of that one (artifact_id) next time. ", *a.TaskID, len(others)-1) + next
		}
	}
	if a.URL != "" {
		next += " at " + a.URL
	}
	next += "; then stop. Their verdict comes back on the ticket."
	return jsonResult(map[string]interface{}{"artifact": a, "next": next})
}

// artifactProject is the project a publish lands in, when it can be told
// cheaply (0 otherwise; the service still checks access).
func (s *Server) artifactProject(ctx context.Context, req service.PublishArtifactRequest) int {
	switch {
	case req.ArtifactID != 0:
		if a, err := s.repo.GetArtifact(ctx, req.ArtifactID); err == nil {
			return a.ProjectID
		}
	case req.TaskID != nil:
		if t, err := s.repo.GetTask(ctx, *req.TaskID); err == nil {
			return t.ProjectID
		}
	}
	return req.ProjectID
}

func (s *Server) listArtifacts(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID := optInt(args, "project_id", 0)
	taskID := optIntPtr(args, "task_id")
	if taskID != nil && projectID == 0 {
		t, err := s.repo.GetTask(ctx, *taskID)
		if err != nil {
			return nil, fmt.Errorf("task %d not found", *taskID)
		}
		projectID = t.ProjectID
	}
	if projectID == 0 {
		return nil, fmt.Errorf("project_id or task_id is required")
	}
	list, err := s.svc.ListArtifacts(ctx, caller(ctx).p, projectID, taskID)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"artifacts": list, "count": len(list)})
}

func (s *Server) getArtifact(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	id, err := reqInt(args, "artifact_id")
	if err != nil {
		return nil, err
	}
	a, versions, err := s.svc.GetArtifact(ctx, caller(ctx).p, id)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{"artifact": a, "versions": versions}
	include := true
	if v, ok := args["include_content"].(bool); ok {
		include = v
	}
	if !include {
		return jsonResult(out)
	}
	_, v, err := s.svc.ArtifactContent(ctx, caller(ctx).p, id, optInt(args, "version", 0))
	if err != nil {
		return nil, err
	}
	s.svc.RecordArtifactView(ctx, caller(ctx).p, a, v.Version, "mcp")
	out["version"] = v.Version
	ct := strings.SplitN(v.ContentType, ";", 2)[0]
	switch {
	case strings.HasPrefix(ct, "image/") && ct != "image/svg+xml":
		res, err := jsonResult(out)
		if err != nil {
			return nil, err
		}
		res.Content = append(res.Content, mcpgo.NewImageContent(base64.StdEncoding.EncodeToString(v.Content), ct))
		return res, nil
	case utf8.Valid(v.Content):
		out["content"] = string(v.Content)
	default:
		out["content_base64"] = base64.StdEncoding.EncodeToString(v.Content)
	}
	return jsonResult(out)
}

func (s *Server) reviewArtifact(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	id, err := reqInt(args, "artifact_id")
	if err != nil {
		return nil, err
	}
	approve, ok := args["approve"].(bool)
	if !ok {
		return nil, fmt.Errorf("approve is required: true or false")
	}
	a, err := s.svc.ReviewArtifact(ctx, caller(ctx).p, id, 0, approve, optStr(args, "note", ""))
	if a != nil {
		s.hub.Publish(domain.Event{Type: domain.EventArtifactUpdated, ProjectID: a.ProjectID, Payload: a, At: a.UpdatedAt})
	}
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"artifact": a})
}

func (s *Server) keepArtifact(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	id, err := reqInt(args, "artifact_id")
	if err != nil {
		return nil, err
	}
	keep, ok := args["keep"].(bool)
	if !ok {
		return nil, fmt.Errorf("keep is required: true or false")
	}
	a, err := s.svc.KeepArtifact(ctx, caller(ctx).p, id, keep)
	if err != nil {
		return nil, err
	}
	s.hub.Publish(domain.Event{Type: domain.EventArtifactUpdated, ProjectID: a.ProjectID, Payload: a, At: time.Now()})
	return jsonResult(map[string]interface{}{"artifact": a})
}
