package authz

import (
	"testing"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

func ip(i int) *int { return &i }

func TestLevelFromAccess(t *testing.T) {
	cases := []struct {
		a    repository.ProjectAccess
		want Level
	}{
		{repository.ProjectAccess{}, None},
		{repository.ProjectAccess{OrgRole: "member"}, None}, // org membership alone is not project access
		{repository.ProjectAccess{Member: true}, Member},
		{repository.ProjectAccess{Member: true, Lead: true}, Lead},
		{repository.ProjectAccess{Member: true, OwnerTeam: true}, Owner},
		{repository.ProjectAccess{Member: true, Lead: true, OwnerTeam: true}, Owner},
		{repository.ProjectAccess{OrgRole: "admin"}, OrgAdmin},
		{repository.ProjectAccess{OrgRole: "owner", Member: true}, OrgAdmin},
	}
	for _, c := range cases {
		if got := levelFromAccess(c.a); got != c.want {
			t.Errorf("%+v → %s, want %s", c.a, got, c.want)
		}
	}
}

// Open reassign: any project member may reassign or release a ticket that
// isn't in progress. Claims stay strict, and in-progress tickets stay put.
func TestOpenReassign(t *testing.T) {
	backlog := &domain.Task{ID: 1, OwnerUserID: ip(2), State: domain.TaskStateBacklog}
	blocked := &domain.Task{ID: 2, OwnerUserID: ip(2), State: domain.TaskStateBlocked}
	running := &domain.Task{ID: 3, OwnerUserID: ip(2), State: domain.TaskStateInProgress}
	rows := []struct {
		lvl    Level
		task   *domain.Task
		action TaskAction
		ok     bool
	}{
		{Member, backlog, TaskAssign, true},
		{Member, backlog, TaskUnassign, true},
		{Member, blocked, TaskAssign, true},
		{Member, running, TaskAssign, false},
		{Member, running, TaskUnassign, false},
		{Member, backlog, TaskClaim, false},
		{Member, backlog, TaskWork, false},
		{Member, backlog, TaskDispatch, false},
		{Member, backlog, TaskDelete, false},
		{None, backlog, TaskAssign, false},
	}
	for _, r := range rows {
		if got := OpenReassign(r.lvl, r.task, r.action); got != r.ok {
			t.Errorf("level=%s state=%s action=%d: %v, want %v", r.lvl, r.task.State, r.action, got, r.ok)
		}
	}
}

// The full decision table for ticket actions. me = 1, someone else = 2.
func TestCheckTask(t *testing.T) {
	const me = 1
	unassigned := &domain.Task{ID: 10}
	mine := &domain.Task{ID: 11, OwnerUserID: ip(me)}
	theirs := &domain.Task{ID: 12, OwnerUserID: ip(2), CreatedBy: ip(2)}
	theirsICreated := &domain.Task{ID: 13, OwnerUserID: ip(2), CreatedBy: ip(me)}
	legacy := &domain.Task{ID: 14} // no owner, no creator

	type row struct {
		lvl    Level
		task   *domain.Task
		action TaskAction
		ok     bool
	}
	rows := []row{
		// Nobody outside the project can do anything.
		{None, unassigned, TaskRead, false},
		{None, mine, TaskWork, false},

		// Reading and commenting: anyone on the project.
		{Member, theirs, TaskRead, true},

		// Working a ticket: its owner, or anyone for an unassigned one. Leads and
		// project owners reassign instead; only org admins override.
		{Member, mine, TaskWork, true},
		{Member, unassigned, TaskWork, true},
		{Member, theirs, TaskWork, false},
		{Lead, theirs, TaskWork, false},
		{Owner, theirs, TaskWork, false},
		{OrgAdmin, theirs, TaskWork, true},

		// Dispatch: owner or unassigned (claims it); only org admins may dispatch others' work.
		{Member, unassigned, TaskDispatch, true},
		{Member, mine, TaskDispatch, true},
		{Lead, theirs, TaskDispatch, false},
		{Owner, theirs, TaskDispatch, false},
		{OrgAdmin, theirs, TaskDispatch, true},

		// Claim: only unassigned (or already mine). Never steals, at any level below Super.
		{Member, unassigned, TaskClaim, true},
		{Member, mine, TaskClaim, true},
		{Member, theirs, TaskClaim, false},
		{Owner, theirs, TaskClaim, false},
		{OrgAdmin, theirs, TaskClaim, false},

		// Assign: my ticket or an unassigned one; leads and up reassign anyone's.
		{Member, mine, TaskAssign, true},
		{Member, unassigned, TaskAssign, true},
		{Member, theirs, TaskAssign, false},
		{Lead, theirs, TaskAssign, true},

		// Unassign: same as assign.
		{Member, mine, TaskUnassign, true},
		{Member, theirs, TaskUnassign, false},
		{Lead, theirs, TaskUnassign, true},

		// Plan (dependencies, moving between stories): owner, creator, anyone
		// for an unassigned ticket, or lead and up.
		{Member, mine, TaskPlan, true},
		{Member, theirsICreated, TaskPlan, true},
		{Member, unassigned, TaskPlan, true},
		{Member, theirs, TaskPlan, false},
		{Lead, theirs, TaskPlan, true},

		// Delete: owner, creator, or lead and up.
		{Member, mine, TaskDelete, true},
		{Member, theirsICreated, TaskDelete, true},
		{Member, theirs, TaskDelete, false},
		{Member, legacy, TaskDelete, false},
		{Lead, theirs, TaskDelete, true},

		// Global admins can do everything.
		{Super, theirs, TaskDispatch, true},
		{Super, theirs, TaskClaim, true},
	}
	for _, r := range rows {
		err := CheckTask(r.lvl, r.task, me, r.action)
		if (err == nil) != r.ok {
			t.Errorf("level=%s task=%d action=%d: err=%v, want ok=%v", r.lvl, r.task.ID, r.action, err, r.ok)
		}
		if err != nil && r.lvl == None {
			if StatusOf(err, 0) != 404 {
				t.Errorf("no access must look like not found, got %d", StatusOf(err, 0))
			}
		} else if err != nil && StatusOf(err, 0) != 403 {
			t.Errorf("denied action must be 403, got %d (%v)", StatusOf(err, 0), err)
		}
	}
}

func TestCanEditGrouping(t *testing.T) {
	owners := []int{1, 3}
	if !CanEditGrouping(Member, owners, 3) {
		t.Error("an owner of the epic can edit it")
	}
	if CanEditGrouping(Lead, owners, 2) {
		t.Error("a lead who isn't an owner can't edit it")
	}
	if !CanEditGrouping(Owner, owners, 2) || !CanEditGrouping(Super, nil, 0) {
		t.Error("project owners and global admins can edit any grouping")
	}
	if CanEditGrouping(Member, nil, 0) {
		t.Error("no user id must never match an owner list")
	}
}

func TestTaskViewIncludes(t *testing.T) {
	const me = 1
	cases := []struct {
		name string
		v    TaskView
		task domain.Task
		me   int
		want bool
	}{
		{"mine: owned by me", DefaultTaskView, domain.Task{OwnerUserID: ip(me)}, me, true},
		{"mine: owned by someone else", DefaultTaskView, domain.Task{OwnerUserID: ip(2), CreatedBy: ip(me)}, me, false},
		{"mine: unassigned, I created it", DefaultTaskView, domain.Task{CreatedBy: ip(me)}, me, true},
		{"mine: unassigned, someone else created it", DefaultTaskView, domain.Task{CreatedBy: ip(2)}, me, false},
		{"mine: legacy row (no creator, no owner)", DefaultTaskView, domain.Task{}, me, true},
		{"mine: local dev sees everything", DefaultTaskView, domain.Task{OwnerUserID: ip(2)}, 0, true},
		{"unassigned", TaskView{Mode: "unassigned"}, domain.Task{CreatedBy: ip(2)}, me, true},
		{"unassigned excludes owned", TaskView{Mode: "unassigned"}, domain.Task{OwnerUserID: ip(me)}, me, false},
		{"any", TaskView{Mode: "any"}, domain.Task{OwnerUserID: ip(2)}, me, true},
		{"user", TaskView{Mode: "user", UserID: 2}, domain.Task{OwnerUserID: ip(2)}, me, true},
		{"user excludes unassigned", TaskView{Mode: "user", UserID: 2}, domain.Task{CreatedBy: ip(2)}, me, false},
	}
	for _, c := range cases {
		if got := c.v.Includes(&c.task, c.me); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}
