// Package jira implements providers.BoardProvider against the Atlassian
// Cloud REST API v3. Auth is HTTP Basic with email + API token.
package jira

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/httpclient"
	"code-agent/pkg/providers"
)

// Provider holds per-board config and an HTTP client.
type Provider struct {
	board  config.Board
	http   *httpclient.Client
	logger zerolog.Logger
	disp   *providers.Dispatcher
	// email is parsed from the auth env var ("email:token").
	email string
}

// New constructs a Jira provider. The auth env var must be of the form
// "email@example.com:api_token" (Atlassian's Basic auth scheme).
func New(b config.Board, logger zerolog.Logger, disp *providers.Dispatcher) (*Provider, error) {
	auth := b.Auth.Value()
	if auth == "" {
		return nil, fmt.Errorf("board %s: auth env %s is empty", b.ID, b.Auth.Env)
	}
	parts := strings.SplitN(auth, ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("board %s: auth env %s must be 'email:token'", b.ID, b.Auth.Env)
	}
	email, token := parts[0], parts[1]
	basic := base64.StdEncoding.EncodeToString([]byte(email + ":" + token))

	hc := httpclient.New(httpclient.Options{
		BaseURL:    strings.TrimRight(b.URL, "/"),
		Timeout:    30 * time.Second,
		MaxRetries: 3,
		DefaultHeaders: map[string]string{
			"Authorization": "Basic " + basic,
		},
	}, logger.With().Str("provider", "jira").Str("board", b.ID).Logger())

	return &Provider{
		board:  b,
		http:   hc,
		logger: logger.With().Str("provider", "jira").Str("board", b.ID).Logger(),
		disp:   disp,
		email:  email,
	}, nil
}

func (p *Provider) ID() string       { return p.board.ID }
func (p *Provider) Provider() string { return "jira" }

func init() {
	providers.Register("jira", func(b config.Board, logger zerolog.Logger, disp *providers.Dispatcher) (providers.BoardProvider, error) {
		return New(b, logger, disp)
	})
}

// --- API types (only the fields we use) ---

type searchResp struct {
	Issues []issue `json:"issues"`
}

type issue struct {
	Key    string `json:"key"`
	Self   string `json:"self"`
	Fields struct {
		Summary     string    `json:"summary"`
		Description any       `json:"description"`
		Updated     time.Time `json:"updated"`
		Status      struct {
			Name string `json:"name"`
		} `json:"status"`
		Assignee *struct {
			DisplayName  string `json:"displayName"`
			EmailAddress string `json:"emailAddress"`
		} `json:"assignee"`
		Labels []string `json:"labels"`
	} `json:"fields"`
}

type transitionsResp struct {
	Transitions []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		To   struct {
			Name string `json:"name"`
		} `json:"to"`
	} `json:"transitions"`
}

func (p *Provider) ticketURL(key string) string {
	return strings.TrimRight(p.board.URL, "/") + "/browse/" + key
}

func (p *Provider) toTicket(i issue) providers.Ticket {
	desc := descToString(i.Fields.Description)
	assignee := ""
	if i.Fields.Assignee != nil {
		assignee = i.Fields.Assignee.DisplayName
	}
	return providers.Ticket{
		BoardID:     p.board.ID,
		ExternalID:  i.Key,
		Title:       i.Fields.Summary,
		Description: desc,
		URL:         p.ticketURL(i.Key),
		Status:      i.Fields.Status.Name,
		Labels:      i.Fields.Labels,
		Assignee:    assignee,
		Updated:     i.Fields.Updated,
	}
}

func descToString(d any) string {
	switch v := d.(type) {
	case nil:
		return ""
	case string:
		return v
	case map[string]any:
		// ADF (Atlassian Document Format) — extract text nodes.
		var sb strings.Builder
		walkADF(v, &sb)
		return strings.TrimSpace(sb.String())
	default:
		return fmt.Sprintf("%v", v)
	}
}

func walkADF(node any, sb *strings.Builder) {
	switch n := node.(type) {
	case map[string]any:
		if t, _ := n["type"].(string); t == "text" {
			if s, _ := n["text"].(string); s != "" {
				sb.WriteString(s)
			}
		}
		if c, ok := n["content"].([]any); ok {
			for _, ch := range c {
				walkADF(ch, sb)
			}
			// add a newline after block-level container nodes
			if t, _ := n["type"].(string); t == "paragraph" || t == "heading" || t == "listItem" {
				sb.WriteString("\n")
			}
		}
	case []any:
		for _, ch := range n {
			walkADF(ch, sb)
		}
	}
}

// jql builds the search query from board filters.
func (p *Provider) jql(extra string) string {
	clauses := []string{}
	if p.board.ProjectKey != "" {
		clauses = append(clauses, fmt.Sprintf("project = %q", p.board.ProjectKey))
	}
	for _, l := range p.board.Filters.Labels {
		clauses = append(clauses, fmt.Sprintf("labels = %q", l))
	}
	for _, l := range p.board.Filters.ExcludeLabels {
		clauses = append(clauses, fmt.Sprintf("labels != %q", l))
	}
	if extra != "" {
		clauses = append(clauses, extra)
	}
	return strings.Join(clauses, " AND ") + " ORDER BY updated DESC"
}

// ListTasks fetches all issues matching the board filter.
func (p *Provider) ListTasks(ctx context.Context) ([]providers.Ticket, error) {
	q := p.jql("")
	path := "/rest/api/3/search?maxResults=100&jql=" + urlQuery(q)
	var resp searchResp
	if err := p.http.Do(ctx, http.MethodGet, path, nil, &resp, nil); err != nil {
		return nil, err
	}
	out := make([]providers.Ticket, 0, len(resp.Issues))
	for _, i := range resp.Issues {
		out = append(out, p.toTicket(i))
	}
	return out, nil
}

func (p *Provider) Get(ctx context.Context, externalID string) (providers.Ticket, error) {
	var i issue
	if err := p.http.Do(ctx, http.MethodGet, "/rest/api/3/issue/"+externalID, nil, &i, nil); err != nil {
		return providers.Ticket{}, err
	}
	return p.toTicket(i), nil
}

// Transition moves the issue to the named status by looking up the matching
// transition id.
func (p *Provider) Transition(ctx context.Context, externalID, targetStatus string) error {
	var ts transitionsResp
	if err := p.http.Do(ctx, http.MethodGet, "/rest/api/3/issue/"+externalID+"/transitions", nil, &ts, nil); err != nil {
		return fmt.Errorf("list transitions: %w", err)
	}
	var id string
	for _, t := range ts.Transitions {
		if strings.EqualFold(t.To.Name, targetStatus) || strings.EqualFold(t.Name, targetStatus) {
			id = t.ID
			break
		}
	}
	if id == "" {
		return fmt.Errorf("no transition to %q for %s", targetStatus, externalID)
	}
	body := map[string]any{"transition": map[string]any{"id": id}}
	return p.http.Do(ctx, http.MethodPost, "/rest/api/3/issue/"+externalID+"/transitions", body, nil, nil)
}

// Comment posts a plain-text comment as ADF.
func (p *Provider) Comment(ctx context.Context, externalID string, c providers.Comment) error {
	body := map[string]any{
		"body": map[string]any{
			"type":    "doc",
			"version": 1,
			"content": []any{
				map[string]any{
					"type": "paragraph",
					"content": []any{
						map[string]any{"type": "text", "text": c.Body},
					},
				},
			},
		},
	}
	return p.http.Do(ctx, http.MethodPost, "/rest/api/3/issue/"+externalID+"/comment", body, nil, nil)
}

// AddTag adds a label to a Jira issue. Jira calls these "labels" rather
// than tags; we present them under the same interface.
func (p *Provider) AddTag(ctx context.Context, externalID, tag string) error {
	body := map[string]any{
		"update": map[string]any{
			"labels": []any{
				map[string]any{"add": tag},
			},
		},
	}
	return p.http.Do(ctx, http.MethodPut, "/rest/api/3/issue/"+externalID, body, nil, nil)
}

// urlQuery is a minimal escaper for the JQL query string. We keep it local to
// avoid pulling in net/url for a single call site.
func urlQuery(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r == ' ':
			sb.WriteString("%20")
		case r == '"':
			sb.WriteString("%22")
		case r == '=':
			sb.WriteString("%3D")
		case r == '!':
			sb.WriteString("%21")
		case r == ',':
			sb.WriteString("%2C")
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
