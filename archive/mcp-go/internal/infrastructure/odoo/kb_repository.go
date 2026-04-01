package odoo

import (
	"context"
	"fmt"
	"github.com/amp/mcp-go/internal/domain/models"
	"github.com/amp/mcp-go/internal/domain/repository"
)

// kbRepository implements repository.KBRepository
type kbRepository struct {
	client *Client
}

// NewKBRepository creates a new KB repository
func NewKBRepository(client *Client) repository.KBRepository {
	return &kbRepository{client: client}
}

func (r *kbRepository) Create(ctx context.Context, req models.CreateKBEntryRequest) (*models.KBEntry, error) {
	vals := map[string]interface{}{
		"title":      req.Title,
		"content":    req.Content,
		"project_id": req.ProjectID,
		"entry_type": req.EntryType,
	}

	if req.EpicID != nil {
		vals["epic_id"] = *req.EpicID
	}
	if req.StoryID != nil {
		vals["story_id"] = *req.StoryID
	}
	if req.TaskID != nil {
		vals["task_id"] = *req.TaskID
	}
	if len(req.Tags) > 0 {
		// Join tags with comma
		tags := ""
		for i, tag := range req.Tags {
			if i > 0 {
				tags += ","
			}
			tags += tag
		}
		vals["tags"] = tags
	}
	if req.CreatedByAgent != "" {
		vals["created_by_agent"] = req.CreatedByAgent
	}

	result, err := r.client.Execute(ctx, "amp.knowledge.entry", "create", vals)
	if err != nil {
		return nil, fmt.Errorf("failed to create KB entry: %w", err)
	}

	entryID := int(result.(int64))
	return r.GetByID(ctx, entryID)
}

func (r *kbRepository) GetByID(ctx context.Context, id int) (*models.KBEntry, error) {
	result, err := r.client.ExecuteKw(ctx, "amp.knowledge.entry", "read",
		[]interface{}{[]int{id}},
		map[string]interface{}{
			"fields": []string{
				"title", "content", "content_text", "entry_type", "project_id",
				"epic_id", "story_id", "task_id", "created_by_agent", "create_date", "tags",
			},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to get KB entry: %w", err)
	}

	entries := result.([]interface{})
	if len(entries) == 0 {
		return nil, fmt.Errorf("KB entry not found")
	}

	return mapToKBEntry(entries[0].(map[string]interface{})), nil
}

func (r *kbRepository) Search(ctx context.Context, req models.SearchKBRequest) ([]models.KBEntry, error) {
	// Empty query = list all (filtered only by project/type if provided)
	var domain []interface{}
	if req.Query != "" {
		domain = []interface{}{
			"|", "|",
			[]interface{}{"title", "ilike", req.Query},
			[]interface{}{"content_text", "ilike", req.Query},
			[]interface{}{"tags", "ilike", req.Query},
		}
	}

	if req.ProjectID != nil {
		domain = append(domain, []interface{}{"project_id", "=", *req.ProjectID})
	}
	if req.EntryType != "" {
		domain = append(domain, []interface{}{"entry_type", "=", req.EntryType})
	}
	if len(domain) == 0 {
		domain = []interface{}{[]interface{}{"id", ">", 0}}
	}

	limit := req.Limit
	if limit == 0 {
		limit = 20
	}

	result, err := r.client.ExecuteKw(ctx, "amp.knowledge.entry", "search_read",
		[]interface{}{domain},
		map[string]interface{}{
			"fields": []string{
				"title", "entry_type", "content_text", "project_id", "epic_id",
				"story_id", "task_id", "created_by_agent", "create_date", "tags",
			},
			"limit": limit,
			"order": "create_date desc",
		})
	if err != nil {
		return nil, fmt.Errorf("failed to search KB: %w", err)
	}

	entries := result.([]interface{})
	resultList := make([]models.KBEntry, len(entries))
	for i, e := range entries {
		resultList[i] = *mapToKBEntry(e.(map[string]interface{}))
	}

	return resultList, nil
}

func (r *kbRepository) ListByProject(ctx context.Context, projectID int, limit int) ([]models.KBEntry, error) {
	if limit == 0 {
		limit = 20
	}

	result, err := r.client.ExecuteKw(ctx, "amp.knowledge.entry", "search_read",
		[]interface{}{[][]interface{}{{"project_id", "=", projectID}}},
		map[string]interface{}{
			"fields": []string{
				"title", "entry_type", "content_text", "epic_id", "story_id",
				"task_id", "created_by_agent", "create_date",
			},
			"limit": limit,
			"order": "create_date desc",
		})
	if err != nil {
		return nil, fmt.Errorf("failed to list KB entries: %w", err)
	}

	entries := result.([]interface{})
	resultList := make([]models.KBEntry, len(entries))
	for i, e := range entries {
		resultList[i] = *mapToKBEntry(e.(map[string]interface{}))
	}

	return resultList, nil
}

func (r *kbRepository) ListByTask(ctx context.Context, taskID int) ([]models.KBEntry, error) {
	// Get task info first to get story_id, epic_id, and project_id
	taskResult, err := r.client.ExecuteKw(ctx, "amp.task", "read",
		[]interface{}{[]int{taskID}},
		map[string]interface{}{
			"fields": []string{"story_id", "epic_id", "project_id"},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to get task info: %w", err)
	}

	tasks := taskResult.([]interface{})
	if len(tasks) == 0 {
		return nil, fmt.Errorf("task not found")
	}

	task := tasks[0].(map[string]interface{})

	// Collect all OR conditions first, then prepend the correct number of "|" operators.
	// Odoo domain syntax requires exactly N-1 OR operators for N conditions.
	conditions := []interface{}{
		[]interface{}{"task_id", "=", taskID},
	}

	if v, ok := task["story_id"]; ok && v != false {
		if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
			conditions = append(conditions, []interface{}{"story_id", "=", int(arr[0].(int64))})
		}
	}
	if v, ok := task["epic_id"]; ok && v != false {
		if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
			conditions = append(conditions, []interface{}{"epic_id", "=", int(arr[0].(int64))})
		}
	}
	if v, ok := task["project_id"]; ok && v != false {
		if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
			conditions = append(conditions, []interface{}{"project_id", "=", int(arr[0].(int64))})
		}
	}

	// Build domain: prepend (N-1) "|" operators for N conditions
	domain := []interface{}{}
	for i := 0; i < len(conditions)-1; i++ {
		domain = append(domain, "|")
	}
	domain = append(domain, conditions...)

	result, err := r.client.ExecuteKw(ctx, "amp.knowledge.entry", "search_read",
		[]interface{}{domain},
		map[string]interface{}{
			"fields": []string{
				"title", "entry_type", "content_text", "created_by_agent", "create_date",
			},
			"limit": 20,
			"order": "create_date desc",
		})
	if err != nil {
		return nil, fmt.Errorf("failed to list KB entries by task: %w", err)
	}

	entries := result.([]interface{})
	resultList := make([]models.KBEntry, len(entries))
	for i, e := range entries {
		resultList[i] = *mapToKBEntry(e.(map[string]interface{}))
	}

	return resultList, nil
}

func mapToKBEntry(data map[string]interface{}) *models.KBEntry {
	e := &models.KBEntry{
		ID:             int(data["id"].(int64)),
		Title:          getString(data, "title"),
		Content:        getString(data, "content"),
		ContentText:    getString(data, "content_text"),
		EntryType:      getString(data, "entry_type"),
		CreatedByAgent: getString(data, "created_by_agent"),
		CreateDate:     getString(data, "create_date"),
		Tags:           getString(data, "tags"),
	}

	if v, ok := data["project_id"]; ok && v != false {
		if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
			e.ProjectID = int(arr[0].(int64))
		}
	}
	if v, ok := data["epic_id"]; ok && v != false {
		if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
			e.EpicID = int(arr[0].(int64))
		}
	}
	if v, ok := data["story_id"]; ok && v != false {
		if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
			e.StoryID = int(arr[0].(int64))
		}
	}
	if v, ok := data["task_id"]; ok && v != false {
		if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
			e.TaskID = int(arr[0].(int64))
		}
	}

	return e
}
