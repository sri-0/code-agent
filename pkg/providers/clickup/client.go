// Package clickup implements providers.BoardProvider against the ClickUp v2
// REST API. Auth is a personal/api token in the Authorization header.
package clickup

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/httpclient"
	"code-agent/pkg/providers"
)

const baseURL = "https://api.clickup.com/api/v2"

type Provider struct {
	board  config.Board
	http   *httpclient.Client
	logger zerolog.Logger
	disp   *providers.Dispatcher

	// statusToID is populated lazily so Transition can map a status name
	// (e.g. "in progress") to the value ClickUp expects.
}

func New(b config.Board, logger zerolog.Logger, disp *providers.Dispatcher) (*Provider, error) {
	token := b.Auth.Value()
	if token == "" {
		return nil, fmt.Errorf("board %s: auth env %s is empty", b.ID, b.Auth.Env)
	}
	hc := httpclient.New(httpclient.Options{
		BaseURL:    baseURL,
		Timeout:    30 * time.Second,
		MaxRetries: 3,
		DefaultHeaders: map[string]string{
			"Authorization": token,
		},
	}, logger.With().Str("provider", "clickup").Str("board", b.ID).Logger())
	return &Provider{
		board:  b,
		http:   hc,
		logger: logger.With().Str("provider", "clickup").Str("board", b.ID).Logger(),
		disp:   disp,
	}, nil
}

func (p *Provider) ID() string       { return p.board.ID }
func (p *Provider) Provider() string { return "clickup" }

func init() {
	providers.Register("clickup", func(b config.Board, logger zerolog.Logger, disp *providers.Dispatcher) (providers.BoardProvider, error) {
		return New(b, logger, disp)
	})
}

// --- API types ---

type cuTask struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`
	DateUpdated string `json:"date_updated"`
	Status      struct {
		Status string `json:"status"`
	} `json:"status"`
	Tags []struct {
		Name string `json:"name"`
	} `json:"tags"`
	Assignees []struct {
		Username string `json:"username"`
	} `json:"assignees"`
}

type listTasksResp struct {
	Tasks []cuTask `json:"tasks"`
}

func (p *Provider) toTicket(t cuTask) providers.Ticket {
	tags := make([]string, 0, len(t.Tags))
	for _, tg := range t.Tags {
		tags = append(tags, tg.Name)
	}
	assignee := ""
	if len(t.Assignees) > 0 {
		assignee = t.Assignees[0].Username
	}
	updated := time.Time{}
	if t.DateUpdated != "" {
		if ms, err := strconv.ParseInt(t.DateUpdated, 10, 64); err == nil {
			updated = time.UnixMilli(ms)
		}
	}
	return providers.Ticket{
		BoardID:     p.board.ID,
		ExternalID:  t.ID,
		Title:       t.Name,
		Description: t.Description,
		URL:         t.URL,
		Status:      t.Status.Status,
		Labels:      tags,
		Assignee:    assignee,
		Updated:     updated,
	}
}

func (p *Provider) matchesFilters(t providers.Ticket) bool {
	if len(p.board.Filters.Tags) > 0 {
		want := map[string]bool{}
		for _, l := range p.board.Filters.Tags {
			want[strings.ToLower(l)] = true
		}
		ok := false
		for _, l := range t.Labels {
			if want[strings.ToLower(l)] {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(p.board.Filters.ExcludeTags) > 0 {
		bad := map[string]bool{}
		for _, l := range p.board.Filters.ExcludeTags {
			bad[strings.ToLower(l)] = true
		}
		for _, l := range t.Labels {
			if bad[strings.ToLower(l)] {
				return false
			}
		}
	}
	return true
}

// ListTasks iterates the configured list_ids.
func (p *Provider) ListTasks(ctx context.Context) ([]providers.Ticket, error) {
	if len(p.board.ListIDs) == 0 {
		return nil, fmt.Errorf("board %s: clickup needs at least one list_id", p.board.ID)
	}
	out := []providers.Ticket{}
	for _, listID := range p.board.ListIDs {
		path := fmt.Sprintf("/list/%s/task?include_closed=false&subtasks=false", listID)
		var resp listTasksResp
		if err := p.http.Do(ctx, http.MethodGet, path, nil, &resp, nil); err != nil {
			return nil, fmt.Errorf("list %s: %w", listID, err)
		}
		for _, t := range resp.Tasks {
			tk := p.toTicket(t)
			if p.matchesFilters(tk) {
				out = append(out, tk)
			}
		}
	}
	return out, nil
}

func (p *Provider) Get(ctx context.Context, externalID string) (providers.Ticket, error) {
	var t cuTask
	if err := p.http.Do(ctx, http.MethodGet, "/task/"+externalID, nil, &t, nil); err != nil {
		return providers.Ticket{}, err
	}
	return p.toTicket(t), nil
}

// Transition sets the task status (ClickUp accepts a status string directly).
func (p *Provider) Transition(ctx context.Context, externalID, targetStatus string) error {
	body := map[string]any{"status": targetStatus}
	return p.http.Do(ctx, http.MethodPut, "/task/"+externalID, body, nil, nil)
}

func (p *Provider) Comment(ctx context.Context, externalID string, c providers.Comment) error {
	body := map[string]any{
		"comment_text": c.Body,
		"notify_all":   false,
	}
	return p.http.Do(ctx, http.MethodPost, "/task/"+externalID+"/comment", body, nil, nil)
}

// AddTag attaches an existing space-level tag to the task. ClickUp returns
// success even if the tag was already attached. If the tag doesn't exist at
// space level, this errors — the caller should pre-create the tag.
func (p *Provider) AddTag(ctx context.Context, externalID, tag string) error {
	// URL-escape the tag name (spaces, etc.)
	path := "/task/" + externalID + "/tag/" + url.PathEscape(tag)
	return p.http.Do(ctx, http.MethodPost, path, nil, nil, nil)
}

// UpdateDescription replaces the task body with the given markdown string.
// ClickUp stores description as plain markdown in the "description" field;
// PUT /task/{id} with {"description": "..."} overwrites it.
func (p *Provider) UpdateDescription(ctx context.Context, externalID, body string) error {
	req := map[string]any{"description": body}
	return p.http.Do(ctx, http.MethodPut, "/task/"+externalID, req, nil, nil)
}
