package service

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/kb"
)

// ---- Design systems: one process for every org and project ----
//
// An org's design system is built in the org's Design System project; a
// product project's layer is built in that project. Both follow a fixed
// template of phase tickets (amp_design_system_start), each ending in an
// artifact a person approves, and are published to their KB's design-system/
// docs only by PublishDesignSystem, which checks every phase was approved.

// dsPhase is one ticket of the template.
type dsPhase struct {
	Phase, Story, Name, Agent, Description, Criteria string
	DependsOn                                        []string
}

const phasePublish = "publish"

var orgDesignSystemPhases = []dsPhase{
	{Phase: "audit", Story: "Discovery", Name: "Audit: products, brand and design debt", Agent: "amp-designer",
		Description: "Inventory what the org's products look like today, and what the brand asks for. Screenshot the key screens of each product; extract the colors, fonts, type sizes, spacing, radii and shadows actually in use (from CSS/Tailwind config, or the KBs if there's no code); list the components in use and where they're inconsistent. Ask the user for brand assets (logo, brand colors, fonts) and for products they admire, with a question on the ticket if they aren't in the KBs. Follow amp_guide design/building-a-design-system.md.",
		Criteria:    "An artifact (markdown, or HTML contact sheet of screenshots and swatches) with the inventory, the inconsistencies, and the brand inputs, published on this ticket and approved by the user."},
	{Phase: "direction", Story: "Discovery", Name: "Direction: style tiles", Agent: "amp-designer", DependsOn: []string{"audit"},
		Description: "Two or three distinct directions as style tiles in ONE self-contained HTML artifact with a switcher: type pairing, palette, a button, an input, a card, a table row, and one real screen from the org's products in that style. Each direction says what it's for in a sentence. The user picks one (or a mix) when reviewing.",
		Criteria:    "One HTML artifact with 2-3 switchable style tiles, approved by the user; the chosen direction (and why) is recorded in a ticket comment."},
	{Phase: "tokens", Story: "Foundations", Name: "Foundations: design tokens", Agent: "amp-designer", DependsOn: []string{"direction"},
		Description: "Turn the chosen direction into tokens: color roles for light and dark (background, surface, ink levels, line, accent, focus, success, warning, danger) checked to WCAG AA, type families and scale, spacing scale, radius, elevation, motion. Publish a live token sheet: an HTML specimen of every token with a light/dark toggle, and the same tokens as a :root CSS block and a Tailwind @theme block shown in the page.",
		Criteria:    "HTML token sheet artifact (light and dark, contrast noted, CSS and Tailwind blocks included) approved by the user."},
	{Phase: "components", Story: "Components", Name: "Components: core gallery", Agent: "amp-designer", DependsOn: []string{"tokens"},
		Description: "Design the core components using only the approved tokens, in every state (default, hover, focus, active, disabled, loading, error) and size: button, link, input, textarea, select, checkbox, radio, switch, badge, avatar, card, table, tabs, sidebar and top-bar navigation, dialog, toast, tooltip, empty state, skeleton. One self-contained HTML gallery.",
		Criteria:    "HTML component gallery artifact covering the core set and states, tokens only, approved by the user."},
	{Phase: "patterns", Story: "Patterns and voice", Name: "Patterns and voice", Agent: "amp-designer", DependsOn: []string{"components"},
		Description: "The org's decisions for page layouts, navigation, forms, settings, tables, empty/loading/error states, destructive actions, and the voice of the copy (tone, words to use and avoid, capitalization, error-message style), each with a short example. Markdown, or HTML with example screens.",
		Criteria:    "Patterns and voice artifact approved by the user."},
	{Phase: phasePublish, Story: "Publish", Name: "Publish the design system", Agent: "amp-designer", DependsOn: []string{"audit", "direction", "tokens", "components", "patterns"},
		Description: "Write the design system docs from the APPROVED artifacts (amp_get_artifact with approved_version) and publish them with amp_design_system_publish: README.md (what it is, 3-5 concrete principles), tokens.md (every token with role and value, then the CSS and Tailwind blocks), components.md, patterns.md, voice.md, and a one-line changelog. Only an org owner or admin can publish; this ticket is theirs.",
		Criteria:    "amp_design_system_publish succeeded: the docs are in the org KB under design-system/ and the approved artifacts are kept."},
}

var projectDesignSystemPhases = []dsPhase{
	{Phase: "delta", Story: "Design system layer", Name: "Design delta: what this product needs beyond the org system", Agent: "amp-designer",
		Description: "Read the org's design system (amp_design_context) and this product's existing UI and KB. List what the product needs that the org system doesn't have, or must do differently: an accent for its audience, density, a component only it has, product-specific patterns. Keep it to real differences; everything else is the org's.",
		Criteria:    "A markdown artifact listing the differences, each with the reason, approved by the user."},
	{Phase: "additions", Story: "Design system layer", Name: "Design additions and overrides", Agent: "amp-designer", DependsOn: []string{"delta"},
		Description: "Design the approved differences on top of the org's tokens and components: one self-contained HTML artifact showing each override and new component next to the org default, in the states that matter, with the token values.",
		Criteria:    "HTML artifact of the additions and overrides, approved by the user."},
	{Phase: phasePublish, Story: "Design system layer", Name: "Publish the project design system", Agent: "amp-designer", DependsOn: []string{"delta", "additions"},
		Description: "Write the project's design-system docs from the APPROVED artifacts and publish them with amp_design_system_publish: README.md (which org version it extends and what it changes), plus tokens.md / components.md / patterns.md with only the differences, and a one-line changelog.",
		Criteria:    "amp_design_system_publish succeeded: the docs are in the project KB under design-system/."},
}

// Docs each scope must publish, and may.
var (
	orgRequiredDocs     = []string{"README.md", "tokens.md", "components.md", "patterns.md", "voice.md"}
	projectRequiredDocs = []string{"README.md"}
	allowedDocs         = map[string]bool{"README.md": true, "tokens.md": true, "components.md": true, "patterns.md": true, "voice.md": true, "brand.md": true}
)

// DesignSystemStatus is where a design system stands.
type DesignSystemStatus struct {
	*domain.DesignSystem
	SourceProjectName string                       `json:"source_project_name,omitempty"`
	Building          bool                         `json:"building"` // a version is in progress
	Phases            []DesignSystemPhaseStatus    `json:"phases,omitempty"`
	Versions          []domain.DesignSystemVersion `json:"versions,omitempty"`
}

// DesignSystemPhaseStatus is one phase ticket of the version being built.
type DesignSystemPhaseStatus struct {
	Phase           string `json:"phase"`
	TaskID          int    `json:"task_id"`
	Name            string `json:"name"`
	State           string `json:"state"`
	ArtifactID      int    `json:"artifact_id,omitempty"`
	ArtifactStatus  string `json:"artifact_status,omitempty"`
	ApprovedVersion int    `json:"approved_version,omitempty"`
}

// StartDesignSystemResult is what amp_design_system_start made (or found).
type StartDesignSystemResult struct {
	DesignSystem *domain.DesignSystem `json:"design_system"`
	Project      *domain.Project      `json:"project"`
	EpicID       int                  `json:"epic_id"`
	Version      int                  `json:"version"` // the version being built
	Tickets      []CreatedTask        `json:"tickets"`
	AlreadyOpen  bool                 `json:"already_open"`
	CreatedProj  bool                 `json:"created_project"`
}

func (s *Service) requireOrgAdmin(ctx context.Context, p authz.Principal, orgID int) error {
	if p.Super {
		return nil
	}
	role, err := s.Authz.OrgRole(ctx, p, orgID)
	if err != nil {
		return err
	}
	if role != domain.OrgRoleOwner && role != domain.OrgRoleAdmin {
		return authz.Forbidden("only the org's owners and admins can do this for the org's design system (it's published to the org KB)")
	}
	return nil
}

var letters = regexp.MustCompile(`[^A-Za-z]`)

// StartOrgDesignSystem finds or creates the org's Design System project and
// opens the next version from the template. An org owner or admin starts
// it (they also publish it). If a version is already being built, it's
// returned as is.
func (s *Service) StartOrgDesignSystem(ctx context.Context, p authz.Principal, orgID int, teamID *int, code, brief string) (*StartDesignSystemResult, error) {
	if err := s.requireOrgAdmin(ctx, p, orgID); err != nil {
		return nil, err
	}
	org, err := s.Repo.GetOrg(ctx, orgID)
	if err != nil {
		return nil, authz.NotFound("org %d not found", orgID)
	}
	ds, err := s.Repo.OrgDesignSystem(ctx, orgID)
	if err != nil {
		return nil, err
	}
	res := &StartDesignSystemResult{}
	if ds == nil {
		home, err := s.ResolveProjectHome(ctx, p, &orgID, teamID)
		if err != nil {
			return nil, err
		}
		if code == "" {
			base := strings.ToUpper(letters.ReplaceAllString(org.Slug, ""))
			if len(base) > 4 {
				base = base[:4]
			}
			code = base + "DS"
		}
		proj, err := s.Repo.CreateProject(ctx, domain.CreateProjectRequest{
			Name: org.Name + " Design System", Code: code, OrgID: home.OrgID, TeamID: home.TeamID, CreatedBy: home.CreatedBy,
			Description: "The " + org.Name + " design system: built and updated here, published to the org knowledge base (design-system/). Every product project builds on it.",
			Metadata:    domain.Metadata{"purpose": "design-system"},
		})
		if err != nil {
			return nil, fmt.Errorf("creating the Design System project (code %q): %w; pass a different code", code, err)
		}
		s.audit(ctx, p, domain.AuditEntry{Action: "project.create", ProjectID: intRef(proj.ID), TargetType: "project", TargetID: idStr(proj.ID), Summary: "created the org's Design System project"})
		if ds, err = s.Repo.CreateDesignSystem(ctx, domain.DesignSystem{Scope: domain.DesignSystemOrg, OrgID: orgID, SourceProjectID: proj.ID}, p.UserRef()); err != nil {
			return nil, err
		}
		res.CreatedProj = true
	}
	return s.openDesignSystemVersion(ctx, p, ds, orgDesignSystemPhases, brief, res)
}

// StartProjectDesignSystem opens the next version of a product project's
// design system layer. The org must have published its design system first:
// a project layer only records differences from it.
func (s *Service) StartProjectDesignSystem(ctx context.Context, p authz.Principal, projectID int, brief string) (*StartDesignSystemResult, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member); err != nil {
		return nil, err
	}
	proj, err := s.Repo.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if proj.OrgID == nil {
		return nil, authz.BadRequest("project %d isn't in an org; a project design system extends its org's", projectID)
	}
	if src, _ := s.Repo.DesignSystemBySource(ctx, projectID); src != nil && src.Scope == domain.DesignSystemOrg {
		return nil, authz.BadRequest("project %d is the org's Design System project; use scope org", projectID)
	}
	orgDS, err := s.Repo.OrgDesignSystem(ctx, *proj.OrgID)
	if err != nil {
		return nil, err
	}
	if orgDS == nil || orgDS.Version == 0 {
		return nil, authz.BadRequest("the org hasn't published a design system yet. Build it first: amp_design_system_start with scope \"org\" (an org owner or admin), then come back for the project layer")
	}
	ds, err := s.Repo.ProjectDesignSystem(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if ds == nil {
		pid := projectID
		if ds, err = s.Repo.CreateDesignSystem(ctx, domain.DesignSystem{Scope: domain.DesignSystemProject, OrgID: *proj.OrgID, ProjectID: &pid, SourceProjectID: projectID}, p.UserRef()); err != nil {
			return nil, err
		}
	}
	return s.openDesignSystemVersion(ctx, p, ds, projectDesignSystemPhases, brief, &StartDesignSystemResult{})
}

func (s *Service) openDesignSystemVersion(ctx context.Context, p authz.Principal, ds *domain.DesignSystem, phases []dsPhase, brief string, res *StartDesignSystemResult) (*StartDesignSystemResult, error) {
	proj, err := s.Repo.GetProject(ctx, ds.SourceProjectID)
	if err != nil {
		return nil, err
	}
	res.DesignSystem, res.Project, res.Version = ds, proj, ds.Version+1
	if ds.CurrentEpicID != nil {
		if _, err := s.Repo.GetEpic(ctx, *ds.CurrentEpicID); err == nil {
			res.EpicID, res.AlreadyOpen = *ds.CurrentEpicID, true
			for _, ph := range s.designSystemPhases(ctx, ds) {
				res.Tickets = append(res.Tickets, CreatedTask{Ref: ph.Phase, ID: ph.TaskID, State: ph.State})
			}
			return res, nil
		}
	}
	label := "Design system"
	if ds.Scope == domain.DesignSystemProject {
		label = proj.Name + " design system layer"
	}
	meta := domain.Metadata{"design_system_id": ds.ID, "design_system_version": res.Version}
	desc := fmt.Sprintf("Version %d, built with AMP's design system process (amp_guide design/building-a-design-system.md). Each phase ends in an artifact the user approves; the last ticket publishes with amp_design_system_publish.", res.Version)
	if strings.TrimSpace(brief) != "" {
		desc += "\n\nBrief: " + strings.TrimSpace(brief)
	}
	epicReply := make(chan actor.ReplyCreateEpic, 1)
	if err := s.Registry.Send(proj.ID, &actor.MsgCreateEpic{Req: domain.CreateEpicRequest{ProjectID: proj.ID,
		Name: fmt.Sprintf("%s v%d", label, res.Version), Description: desc, Priority: "1", CreatedBy: p.UserRef(), Metadata: meta}, ReplyCh: epicReply}); err != nil {
		return nil, err
	}
	er := <-epicReply
	if er.Err != nil {
		return nil, er.Err
	}
	stories := map[string]int{}
	var specs []TaskSpec
	for _, ph := range phases {
		if _, ok := stories[ph.Story]; !ok {
			sr := make(chan actor.ReplyCreateStory, 1)
			if err := s.Registry.Send(proj.ID, &actor.MsgCreateStory{Req: domain.CreateStoryRequest{ProjectID: proj.ID, EpicID: er.Epic.ID,
				Name: ph.Story, Priority: "1", CreatedBy: p.UserRef()}, ReplyCh: sr}); err != nil {
				return nil, err
			}
			r := <-sr
			if r.Err != nil {
				return nil, r.Err
			}
			stories[ph.Story] = r.Story.ID
		}
		d := ph.Description
		if strings.TrimSpace(brief) != "" {
			d += "\n\nBrief for this version: " + strings.TrimSpace(brief)
		}
		specs = append(specs, TaskSpec{Ref: ph.Phase, StoryID: stories[ph.Story], Name: ph.Name, Description: d, AcceptanceCriteria: ph.Criteria,
			Priority: "1", AssignedTo: ph.Agent, DependsOn: ph.DependsOn,
			Metadata: domain.Metadata{"design_system_phase": ph.Phase}})
	}
	created, _, err := s.CreateTasks(ctx, p, proj.ID, specs)
	if err != nil {
		return nil, err
	}
	epicID := er.Epic.ID
	if err := s.Repo.SetDesignSystemEpic(ctx, ds.ID, &epicID); err != nil {
		return nil, err
	}
	ds.CurrentEpicID = &epicID
	res.EpicID, res.Tickets = epicID, created
	s.audit(ctx, p, domain.AuditEntry{Action: "design_system.start", ProjectID: intRef(proj.ID), TargetType: "design_system", TargetID: idStr(ds.ID),
		Summary: fmt.Sprintf("opened %s design system v%d", ds.Scope, res.Version)})
	return res, nil
}

// designSystemPhases is the phase tickets of the version being built, with
// their artifacts' review status.
func (s *Service) designSystemPhases(ctx context.Context, ds *domain.DesignSystem) []DesignSystemPhaseStatus {
	if ds.CurrentEpicID == nil {
		return nil
	}
	tasks, err := s.Repo.ListTasks(ctx, ds.SourceProjectID, "")
	if err != nil {
		return nil
	}
	var out []DesignSystemPhaseStatus
	for _, t := range tasks {
		phase, _ := t.Metadata["design_system_phase"].(string)
		if t.EpicID != *ds.CurrentEpicID || phase == "" {
			continue
		}
		ph := DesignSystemPhaseStatus{Phase: phase, TaskID: t.ID, Name: t.Name, State: string(t.State)}
		tid := t.ID
		if arts, err := s.Repo.ListArtifacts(ctx, ds.SourceProjectID, &tid); err == nil {
			for _, a := range arts {
				if ph.ArtifactID == 0 || (a.Status == domain.ArtifactApproved && ph.ArtifactStatus != domain.ArtifactApproved) {
					ph.ArtifactID, ph.ArtifactStatus = a.ID, a.Status
					if a.ApprovedVersion != nil {
						ph.ApprovedVersion = *a.ApprovedVersion
					}
				}
			}
		}
		out = append(out, ph)
	}
	return out
}

// DesignSystemStatusFor is where a design system stands (nil if there's none).
func (s *Service) designSystemStatus(ctx context.Context, ds *domain.DesignSystem) *DesignSystemStatus {
	if ds == nil {
		return nil
	}
	st := &DesignSystemStatus{DesignSystem: ds, Building: ds.CurrentEpicID != nil}
	if proj, err := s.Repo.GetProject(ctx, ds.SourceProjectID); err == nil {
		st.SourceProjectName = proj.Name
	}
	st.Phases = s.designSystemPhases(ctx, ds)
	st.Versions, _ = s.Repo.DesignSystemVersions(ctx, ds.ID)
	return st
}

// DesignSystemStatuses is where the org's design system and the project's
// layer stand, for a project member. Either may be nil.
func (s *Service) DesignSystemStatuses(ctx context.Context, p authz.Principal, projectID int) (org, project *DesignSystemStatus, err error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member); err != nil {
		return nil, nil, err
	}
	proj, err := s.Repo.GetProject(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	if proj.OrgID != nil {
		ods, err := s.Repo.OrgDesignSystem(ctx, *proj.OrgID)
		if err != nil {
			return nil, nil, err
		}
		org = s.designSystemStatus(ctx, ods)
	}
	pds, err := s.Repo.ProjectDesignSystem(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	return org, s.designSystemStatus(ctx, pds), nil
}

// PublishDesignSystem publishes the version being built in sourceProjectID:
// it refuses unless every phase ticket has an artifact a person approved,
// writes the docs to the KB's design-system/ (the only way they're written),
// stamps the README with the version, source and approved artifacts, keeps
// those artifacts, and records the version.
func (s *Service) PublishDesignSystem(ctx context.Context, p authz.Principal, sourceProjectID int, docs map[string]string, changelog string) (*DesignSystemStatus, error) {
	if s.KB == nil {
		return nil, authz.BadRequest("the knowledge base is not configured")
	}
	ds, err := s.Repo.DesignSystemBySource(ctx, sourceProjectID)
	if err != nil {
		return nil, err
	}
	if ds == nil {
		return nil, authz.BadRequest("project %d has no design system; start one with amp_design_system_start", sourceProjectID)
	}
	if _, err := s.Authz.RequireProject(ctx, p, sourceProjectID, authz.Member); err != nil {
		return nil, err
	}
	var scope kb.Scope
	if ds.Scope == domain.DesignSystemOrg {
		if err := s.requireOrgAdmin(ctx, p, ds.OrgID); err != nil {
			return nil, err
		}
		scope = kb.Org(ds.OrgID)
	} else {
		k, err := s.ResolveKB(ctx, p, KBRef{Scope: "project", ProjectID: *ds.ProjectID})
		if err != nil {
			return nil, err
		}
		if err := k.RequireWrite(); err != nil {
			return nil, err
		}
		scope = k.Scope
	}
	if ds.CurrentEpicID == nil {
		return nil, authz.BadRequest("no version is being built; start one with amp_design_system_start")
	}
	changelog = strings.TrimSpace(changelog)
	if changelog == "" {
		return nil, authz.BadRequest("changelog is required: one line on what this version is or changes")
	}

	// Every phase approved.
	var missing []string
	var approved []domain.DesignSystemArtifact
	phases := s.designSystemPhases(ctx, ds)
	for _, ph := range phases {
		if ph.Phase == phasePublish {
			continue
		}
		if ph.ArtifactStatus != domain.ArtifactApproved || ph.ApprovedVersion == 0 {
			why := "no artifact"
			if ph.ArtifactID != 0 {
				why = fmt.Sprintf("artifact #%d is %s", ph.ArtifactID, strings.ReplaceAll(ph.ArtifactStatus, "_", " "))
			}
			missing = append(missing, fmt.Sprintf("%s (ticket %d: %s)", ph.Phase, ph.TaskID, why))
			continue
		}
		a, _ := s.Repo.GetArtifact(ctx, ph.ArtifactID)
		title := ""
		if a != nil {
			title = a.Title
		}
		approved = append(approved, domain.DesignSystemArtifact{Phase: ph.Phase, TaskID: ph.TaskID, ArtifactID: ph.ArtifactID,
			Version: ph.ApprovedVersion, Title: title, URL: s.ArtifactURL(ph.ArtifactID)})
	}
	if len(phases) == 0 {
		missing = append(missing, "the version's phase tickets (were they deleted?)")
	}
	if len(missing) > 0 {
		return nil, authz.BadRequest("can't publish until a person has approved every phase's artifact; still open: %s", strings.Join(missing, "; "))
	}

	// The docs.
	clean := map[string]string{}
	for name, content := range docs {
		name = strings.TrimPrefix(strings.TrimSpace(name), domain.DesignSystemPrefix)
		if !allowedDocs[name] {
			return nil, authz.BadRequest("unknown design system doc %q: use README.md, tokens.md, components.md, patterns.md, voice.md or brand.md", name)
		}
		if strings.TrimSpace(content) == "" {
			return nil, authz.BadRequest("%s is empty", name)
		}
		clean[name] = content
	}
	required := orgRequiredDocs
	if ds.Scope == domain.DesignSystemProject {
		required = projectRequiredDocs
	}
	for _, name := range required {
		if _, ok := clean[name]; !ok {
			return nil, authz.BadRequest("docs must include %s (required: %s)", name, strings.Join(required, ", "))
		}
	}

	version := ds.Version + 1
	proj, _ := s.Repo.GetProject(ctx, ds.SourceProjectID)
	org, _ := s.Repo.GetOrg(ctx, ds.OrgID)
	prev, _ := s.Repo.DesignSystemVersions(ctx, ds.ID)
	clean["README.md"] = s.designSystemReadme(ctx, ds, proj, org, version, p, clean["README.md"], approved, changelog, prev)

	reason := fmt.Sprintf("%s design system v%d, published from project %d via amp_design_system_publish after the user approved every phase.", strings.ToUpper(ds.Scope[:1])+ds.Scope[1:], version, ds.SourceProjectID)
	if ds.Scope == domain.DesignSystemProject {
		reason = ""
	}
	names := make([]string, 0, len(clean))
	for n := range clean {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		title := docTitle(clean[n], n)
		if _, err := s.KB.WriteDoc(ctx, scope, domain.DesignSystemPrefix+n, title, clean[n], p.Email,
			[]string{"design-system", fmt.Sprintf("v%d", version), ds.Scope}, reason, nil); err != nil {
			return nil, fmt.Errorf("writing %s: %w", n, err)
		}
	}
	for _, a := range approved {
		_, _ = s.Repo.KeepArtifact(ctx, a.ArtifactID, true, p.UserRef(), s.artifactExpiry())
	}
	if err := s.Repo.PublishDesignSystem(ctx, ds.ID, version, changelog, approved, p.UserRef()); err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "design_system.publish", ProjectID: intRef(ds.SourceProjectID), TargetType: "design_system",
		TargetID: idStr(ds.ID), Summary: fmt.Sprintf("published %s design system v%d: %s", ds.Scope, version, changelog)})
	ds, err = s.Repo.DesignSystemBySource(ctx, sourceProjectID)
	if err != nil {
		return nil, err
	}
	return s.designSystemStatus(ctx, ds), nil
}

func docTitle(content, name string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return strings.TrimSuffix(name, ".md")
}

// designSystemReadme stamps the README: version, layer, source, approved
// artifacts and changelog, around the publisher's content.
func (s *Service) designSystemReadme(ctx context.Context, ds *domain.DesignSystem, proj *domain.Project, org *domain.Org, version int,
	p authz.Principal, body string, approved []domain.DesignSystemArtifact, changelog string, prev []domain.DesignSystemVersion) string {
	orgName, projName, projCode := "", "", ""
	if org != nil {
		orgName = org.Name
	}
	if proj != nil {
		projName, projCode = proj.Name, proj.Code
	}
	var b strings.Builder
	layer := fmt.Sprintf("the org layer: applies to every project in %s", orgName)
	if ds.Scope == domain.DesignSystemProject {
		ext := ""
		if ods, _ := s.Repo.OrgDesignSystem(ctx, ds.OrgID); ods != nil {
			ext = fmt.Sprintf(" v%d", ods.Version)
		}
		layer = fmt.Sprintf("the project layer for %s: extends the %s design system%s, and only records differences", projName, orgName, ext)
	}
	fmt.Fprintf(&b, "> **Design system v%d** · published %s by %s · %s.\n", version, time.Now().UTC().Format("Jan 2, 2006"), p.Email, layer)
	fmt.Fprintf(&b, "> Built in project %s (%s, #%d) with AMP's design system process. Don't edit these docs directly: change them through a new version (amp_design_system_start).\n\n", projName, projCode, ds.SourceProjectID)
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n\n## Approved artifacts\n\n")
	for _, a := range approved {
		ref := fmt.Sprintf("artifact #%d v%d", a.ArtifactID, a.Version)
		if a.URL != "" {
			ref = fmt.Sprintf("[%s](%s)", ref, a.URL)
		}
		fmt.Fprintf(&b, "- **%s**: %s (%s, ticket %d)\n", a.Phase, a.Title, ref, a.TaskID)
	}
	b.WriteString("\n## Changelog\n\n")
	fmt.Fprintf(&b, "- **v%d** (%s): %s\n", version, time.Now().UTC().Format("2006-01-02"), changelog)
	for _, v := range prev {
		fmt.Fprintf(&b, "- **v%d** (%s): %s\n", v.Version, v.PublishedAt.UTC().Format("2006-01-02"), v.Changelog)
	}
	return b.String()
}
