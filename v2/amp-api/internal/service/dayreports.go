package service

import (
	"context"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// Daily reports: one per person per day, generated from their activity and
// tickets, with a summary in their words. Anyone sharing an org with the
// author can read them; tickets from projects the reader can't see are left
// out (the summary is shown as written).

const dayLayout = "2006-01-02"

// maxReportRange is the longest range one listing or velocity call covers.
const maxReportRange = 92

// DayWindow resolves a day ("" = today) in a time zone ("" = UTC) to its
// start and end instants.
func DayWindow(date, tz string) (day string, start, end time.Time, err error) {
	loc, err := loadTZ(tz)
	if err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	var d time.Time
	if strings.TrimSpace(date) == "" {
		now := time.Now().In(loc)
		d = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	} else if d, err = time.ParseInLocation(dayLayout, strings.TrimSpace(date), loc); err != nil {
		return "", time.Time{}, time.Time{}, authz.BadRequest("date must be YYYY-MM-DD, got %q", date)
	}
	return d.Format(dayLayout), d, d.AddDate(0, 0, 1), nil
}

func loadTZ(tz string) (*time.Location, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, authz.BadRequest("unknown time zone %q; use an IANA name like America/New_York", tz)
	}
	return loc, nil
}

func tzName(tz string) string {
	if strings.TrimSpace(tz) == "" {
		return "UTC"
	}
	return strings.TrimSpace(tz)
}

// dayRange resolves from/to days ("" = the last 7 days ending today in tz).
func dayRange(from, to, tz string) (string, string, time.Time, time.Time, error) {
	toDay, _, end, err := DayWindow(to, tz)
	if err != nil {
		return "", "", time.Time{}, time.Time{}, err
	}
	if strings.TrimSpace(from) == "" {
		from = end.AddDate(0, 0, -7).Format(dayLayout)
	}
	fromDay, start, _, err := DayWindow(from, tz)
	if err != nil {
		return "", "", time.Time{}, time.Time{}, err
	}
	if start.After(end) {
		return "", "", time.Time{}, time.Time{}, authz.BadRequest("from (%s) is after to (%s)", fromDay, toDay)
	}
	if end.Sub(start) > maxReportRange*24*time.Hour+time.Hour {
		return "", "", time.Time{}, time.Time{}, authz.BadRequest("at most %d days at a time", maxReportRange)
	}
	return fromDay, toDay, start, end, nil
}

// reportAuthor resolves whose reports are asked for ("" = me) and checks
// p may read them: their own, anyone sharing an org with them, or a global
// admin. Anyone else's are reported as not found.
func (s *Service) reportAuthor(ctx context.Context, p authz.Principal, ref string) (*domain.User, error) {
	if strings.TrimSpace(ref) == "" {
		ref = "me"
	}
	id, err := s.Authz.ResolveUser(ctx, p, ref)
	if err != nil {
		return nil, err
	}
	if id != p.UserID && !p.Super {
		ok, err := s.Repo.SharesOrg(ctx, id, p.UserID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, authz.NotFound("no reports for %s; you can read the reports of people in your orgs", ref)
		}
	}
	return s.Repo.GetUserByID(ctx, id)
}

// forViewer drops tickets in projects p can't see (not for the author or a
// global admin) and renders the markdown.
func (s *Service) forViewer(ctx context.Context, p authz.Principal, r *domain.DayReport) error {
	if r.UserID != p.UserID {
		visible, err := s.Authz.VisibleProjects(ctx, p)
		if err != nil {
			return err
		}
		r.Data, r.HiddenItems = r.Data.Filter(visible)
	}
	r.Markdown = r.RenderMarkdown()
	return nil
}

// DayReportDraft builds p's report for a day from their activity and
// tickets, keeping the summary they already saved for that day, if any.
// Nothing is saved.
func (s *Service) DayReportDraft(ctx context.Context, p authz.Principal, date, tz string) (*domain.DayReport, error) {
	if !p.HasUser() {
		return nil, authz.BadRequest("daily reports need a signed-in AMP user")
	}
	day, start, end, err := DayWindow(date, tz)
	if err != nil {
		return nil, err
	}
	data, err := s.Repo.BuildDayReportData(ctx, p.UserID, start, end)
	if err != nil {
		return nil, err
	}
	u, err := s.Repo.GetUserByID(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	r := &domain.DayReport{UserID: u.ID, UserEmail: u.Email, UserName: u.DisplayName, Day: day, TZ: tzName(tz), Data: data}
	if saved, err := s.Repo.GetDayReport(ctx, p.UserID, day); err != nil {
		return nil, err
	} else if saved != nil {
		r.Summary, r.Saved, r.CreatedAt, r.UpdatedAt = saved.Summary, true, saved.CreatedAt, saved.UpdatedAt
	}
	return r, s.forViewer(ctx, p, r)
}

// SaveDayReport saves p's report for a day with their summary, taking a
// fresh snapshot of their tickets. Saving again replaces it.
func (s *Service) SaveDayReport(ctx context.Context, p authz.Principal, date, tz, summary string) (*domain.DayReport, error) {
	if !p.HasUser() {
		return nil, authz.BadRequest("daily reports need a signed-in AMP user")
	}
	if len(summary) > 20000 {
		return nil, authz.BadRequest("summary is too long (max 20000 characters)")
	}
	day, start, end, err := DayWindow(date, tz)
	if err != nil {
		return nil, err
	}
	data, err := s.Repo.BuildDayReportData(ctx, p.UserID, start, end)
	if err != nil {
		return nil, err
	}
	r, err := s.Repo.SaveDayReport(ctx, p.UserID, day, tzName(tz), strings.TrimSpace(summary), data)
	if err != nil {
		return nil, err
	}
	return r, s.forViewer(ctx, p, r)
}

// DayReports lists someone's saved reports between two days (newest
// first), as p may see them.
func (s *Service) DayReports(ctx context.Context, p authz.Principal, userRef, from, to, tz string) ([]domain.DayReport, error) {
	u, err := s.reportAuthor(ctx, p, userRef)
	if err != nil {
		return nil, err
	}
	fromDay, toDay, _, _, err := dayRange(from, to, tz)
	if err != nil {
		return nil, err
	}
	reports, err := s.Repo.ListDayReports(ctx, u.ID, fromDay, toDay)
	if err != nil {
		return nil, err
	}
	for i := range reports {
		if err := s.forViewer(ctx, p, &reports[i]); err != nil {
			return nil, err
		}
	}
	return reports, nil
}

// Velocity counts someone's completed tickets per day between two days
// (every day present, zeros included), in projects p can see.
func (s *Service) Velocity(ctx context.Context, p authz.Principal, userRef, from, to, tz string) ([]domain.VelocityDay, error) {
	u, err := s.reportAuthor(ctx, p, userRef)
	if err != nil {
		return nil, err
	}
	_, _, start, end, err := dayRange(from, to, tz)
	if err != nil {
		return nil, err
	}
	rows, err := s.Repo.Velocity(ctx, u.ID, start, end, tzName(tz))
	if err != nil {
		return nil, err
	}
	var visible map[int]bool
	if u.ID != p.UserID {
		if visible, err = s.Authz.VisibleProjects(ctx, p); err != nil {
			return nil, err
		}
	}
	byDay := map[string]*domain.VelocityDay{}
	var out []domain.VelocityDay
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		out = append(out, domain.VelocityDay{Day: d.Format(dayLayout), ByProject: map[string]int{}})
	}
	for i := range out {
		byDay[out[i].Day] = &out[i]
	}
	for _, r := range rows {
		if visible != nil && !visible[r.ProjectID] {
			continue
		}
		if d, ok := byDay[r.Day]; ok {
			d.Completed += r.Completed
			d.ByProject[r.ProjectCode] += r.Completed
		}
	}
	return out, nil
}

// ReportPeople lists whose reports p can read: themselves and everyone in
// their orgs (everyone, for a global admin).
func (s *Service) ReportPeople(ctx context.Context, p authz.Principal) ([]domain.Member, error) {
	viewer := p.UserID
	if p.Super {
		viewer = 0
	}
	return s.Repo.ReportPeople(ctx, viewer)
}
