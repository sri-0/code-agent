// cmd/cli is an operator tool that talks to a running code-agent
// orchestrator over its JSON API. It is deliberately thin — the
// orchestrator owns authoritative state.
//
// Usage:
//
//	code-agent [--addr http://host:8080] <command>
//	  boards
//	  tasks [--board ID] [--stage NAME]
//	  task <task-id>
//	  cancel <task-id>
//	  transcript <session-id> [--limit N]
//	  health
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	var (
		addr    string
		board   string
		stage   string
		limit   int
		help    bool
		timeout time.Duration
	)
	flag.StringVar(&addr, "addr", envDefault("CODE_AGENT_ADDR", "http://localhost:8080"), "orchestrator base URL")
	flag.StringVar(&board, "board", "", "filter by board id (tasks command)")
	flag.StringVar(&stage, "stage", "", "filter by stage name (tasks command)")
	flag.IntVar(&limit, "limit", 200, "max events (transcript command)")
	flag.DurationVar(&timeout, "timeout", 15*time.Second, "HTTP timeout")
	flag.BoolVar(&help, "h", false, "help")
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if help || len(args) == 0 {
		usage()
		if help {
			return
		}
		os.Exit(2)
	}

	c := newClient(addr, timeout)
	switch args[0] {
	case "boards":
		must(c.boards())
	case "tasks":
		must(c.tasks(board, stage))
	case "task":
		if len(args) < 2 {
			die("task requires a task id")
		}
		must(c.task(args[1]))
	case "cancel":
		if len(args) < 2 {
			die("cancel requires a task id")
		}
		must(c.cancel(args[1]))
	case "transcript":
		if len(args) < 2 {
			die("transcript requires a session id")
		}
		must(c.transcript(args[1], limit))
	case "health":
		must(c.health())
	default:
		die("unknown command %q", args[0])
	}
}

type client struct {
	base string
	http *http.Client
}

func newClient(base string, timeout time.Duration) *client {
	return &client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: timeout},
	}
}

func (c *client) get(path string, v any) error {
	resp, err := c.http.Get(c.base + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (c *client) post(path string, v any) error {
	req, err := http.NewRequest(http.MethodPost, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if v != nil {
		return json.NewDecoder(resp.Body).Decode(v)
	}
	return nil
}

// ---- commands ----

func (c *client) health() error {
	var v map[string]any
	if err := c.get("/health", &v); err != nil {
		return err
	}
	return printJSON(v)
}

type boardsResp struct {
	Boards []struct {
		ID         string `json:"id"`
		Provider   string `json:"provider"`
		StagesRef  string `json:"stages_ref"`
		RuntimeRef string `json:"runtime_ref"`
	} `json:"boards"`
}

func (c *client) boards() error {
	var r boardsResp
	if err := c.get("/api/boards", &r); err != nil {
		return err
	}
	fmt.Printf("%-20s %-10s %-16s %-16s\n", "ID", "PROVIDER", "STAGES", "RUNTIME")
	for _, b := range r.Boards {
		fmt.Printf("%-20s %-10s %-16s %-16s\n", b.ID, b.Provider, b.StagesRef, b.RuntimeRef)
	}
	return nil
}

type tasksResp struct {
	Count int              `json:"count"`
	Tasks []map[string]any `json:"tasks"`
}

func (c *client) tasks(board, stage string) error {
	q := url.Values{}
	if board != "" {
		q.Set("board", board)
	}
	if stage != "" {
		q.Set("stage", stage)
	}
	path := "/api/tasks"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var r tasksResp
	if err := c.get(path, &r); err != nil {
		return err
	}
	fmt.Printf("%-14s %-12s %-14s %-14s %-6s %s\n", "EXT", "BOARD", "STAGE", "RUNTIME", "REPOS", "TITLE")
	for _, t := range r.Tasks {
		repos := 0
		if rs, ok := t["repos"].([]any); ok {
			repos = len(rs)
		}
		fmt.Printf("%-14s %-12s %-14s %-14s %-6d %s\n",
			str(t, "external_id"),
			str(t, "board_id"),
			str(t, "stage"),
			str(t, "runtime_mode"),
			repos,
			str(t, "title"),
		)
	}
	fmt.Printf("%d tasks\n", r.Count)
	return nil
}

func (c *client) task(id string) error {
	var v map[string]any
	if err := c.get("/api/tasks/"+id, &v); err != nil {
		return err
	}
	return printJSON(v)
}

func (c *client) cancel(id string) error {
	var v map[string]any
	if err := c.post("/api/tasks/"+id+"/cancel", &v); err != nil {
		return err
	}
	return printJSON(v)
}

type transcriptResp struct {
	Session string         `json:"session"`
	Meta    map[string]any `json:"meta"`
	Events  []string       `json:"events"`
}

func (c *client) transcript(sid string, limit int) error {
	path := "/api/transcript/" + sid + "?limit=" + strconv.Itoa(limit)
	var r transcriptResp
	if err := c.get(path, &r); err != nil {
		return err
	}
	fmt.Printf("session %s — %d events\n", r.Session, len(r.Events))
	for _, e := range r.Events {
		fmt.Println(e)
	}
	return nil
}

// ---- misc ----

func usage() {
	fmt.Fprintf(os.Stderr, `code-agent — operator CLI

Usage:
  code-agent [--addr URL] <command> [args]

Commands:
  health
  boards
  tasks [--board ID] [--stage NAME]
  task <task-id>
  cancel <task-id>
  transcript <session-id> [--limit N]
`)
}

func envDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func must(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(2)
}
