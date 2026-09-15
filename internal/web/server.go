// Package web serves the workflow dashboard to a browser on this machine. Like
// the terminal dashboard it is only a client: it reads and changes runs
// through the coordinator and owns no scheduling or process lifetimes.
package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/plugin"
	"github.com/sam-bretz/envctl/internal/review"
	"github.com/sam-bretz/envctl/internal/tui"
	"github.com/sam-bretz/envctl/internal/workflow"
)

//go:embed assets
var assets embed.FS

// API is the coordinator surface the page uses; *daemon.Client provides it.
type API interface {
	List(context.Context) ([]workflow.Run, error)
	Create(context.Context, daemon.CreateRequest) (*workflow.Run, error)
	Action(context.Context, string, daemon.ActionRequest) (*workflow.Run, error)
	Artifact(context.Context, string) ([]byte, error)
	Diff(context.Context, string, review.Request) (review.Comparison, error)
}

type Server struct {
	API API
	// Root is the repository new runs are created from.
	Root string
	// Token authenticates the browser; the printed URL carries it once and
	// the page keeps it in a same-site cookie.
	Token string
	// Hosts are the Host header values the server answers, which blocks DNS
	// rebinding: another site's name that resolves to 127.0.0.1 is refused.
	Hosts    []string
	Owner    string
	Interval time.Duration
	Now      func() time.Time
}

const cookieName = "envctl_web"

var digestPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// LoadToken returns this state directory's dashboard token, creating it once
// so bookmarked sessions survive restarts.
func LoadToken(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "web-token")
	if raw, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(raw)) >= 32 {
		return string(bytes.TrimSpace(raw)), nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	return token, os.WriteFile(path, []byte(token+"\n"), 0o600)
}

// LoopbackHosts lists the Host values a listener on addr answers to.
func LoopbackHosts(addr net.Addr) []string {
	_, port, _ := net.SplitHostPort(addr.String())
	return []string{"127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port}
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "assets")
	files := http.StripPrefix("/assets/", http.FileServerFS(static))
	mux.Handle("GET /assets/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Embedded files have no modification time; revalidate so an upgraded
		// binary never runs against a cached older script.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/state", s.authed(s.state))
	mux.HandleFunc("GET /api/stream", s.authed(s.stream))
	mux.HandleFunc("GET /api/workflows", s.authed(s.workflows))
	mux.HandleFunc("GET /api/themes", s.authed(s.themes))
	mux.HandleFunc("POST /api/runs", s.authed(s.create))
	mux.HandleFunc("POST /api/runs/{id}/actions", s.authed(s.action))
	mux.HandleFunc("GET /api/runs/{id}/diff", s.authed(s.diff))
	mux.HandleFunc("GET /api/artifacts/{digest}", s.authed(s.artifact))
	return s.guard(mux)
}

// guard applies the checks every response needs: a known Host, and headers
// that keep the page from loading or framing anything outside this server.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(s.Hosts, r.Host) {
			http.Error(w, "envctl web answers only on this machine's loopback address", http.StatusMisdirectedRequest)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validToken(t string) bool {
	return s.Token != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) == 1
}

func (s *Server) signedIn(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && s.validToken(c.Value)
}

// authed requires the session cookie, and for changes also the token header
// the page sends and a same-origin request, so other sites cannot act.
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.signedIn(r) {
			writeError(w, http.StatusUnauthorized, errors.New("open the link envctl web printed to sign in"))
			return
		}
		if r.Method != http.MethodGet {
			if !s.validToken(r.Header.Get("X-Envctl-Token")) || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				writeError(w, http.StatusForbidden, errors.New("request is missing the page token"))
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && !slices.Contains(s.Hosts, strings.TrimPrefix(origin, "http://")) {
				writeError(w, http.StatusForbidden, errors.New("request came from another site"))
				return
			}
		}
		next(w, r)
	}
}

var page = template.Must(template.ParseFS(assets, "assets/index.html"))

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if t := r.URL.Query().Get("token"); t != "" {
		if !s.validToken(t) {
			http.Error(w, "This link has an unknown token. Run envctl web again and open the link it prints.", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 30 * 24 * 3600})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.signedIn(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintln(w, "Sign in by opening the link envctl web printed in your terminal.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = page.Execute(w, map[string]string{"Token": s.Token})
}

func (s *Server) snapshot(ctx context.Context) State {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st := State{Root: s.Root, Runs: []RunView{}, At: s.now()}
	runs, err := s.API.List(ctx)
	if err != nil {
		st.Error = "The coordinator is not answering: " + err.Error()
		return st
	}
	for _, r := range runs {
		st.Runs = append(st.Runs, runView(r, s.now()))
	}
	return st
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.snapshot(r.Context()))
}

// stream sends the dashboard state whenever it changes, as server-sent events.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming is unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	interval := s.Interval
	if interval <= 0 {
		interval = time.Second
	}
	var last []byte
	quiet := 0
	for {
		st := s.snapshot(r.Context())
		st.At = time.Time{} // unchanged state must not look changed
		raw, _ := json.Marshal(st)
		switch {
		case !bytes.Equal(raw, last):
			if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", raw); err != nil {
				return
			}
			last, quiet = raw, 0
			flusher.Flush()
		case quiet >= 15:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			quiet = 0
			flusher.Flush()
		default:
			quiet++
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(interval):
		}
	}
}

type workflowChoice struct {
	Name     string   `json:"name"`
	Template string   `json:"template,omitempty"`
	Stages   []string `json:"stages"`
}

func (s *Server) workflows(w http.ResponseWriter, r *http.Request) {
	config, err := workflow.Load(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, fmt.Errorf("there is no envctl.yaml in %s yet; add a version 2 workflow configuration (see https://sam-bretz.github.io/envctl/first-workflow/)", s.Root))
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("workflow configuration: %w", err))
		return
	}
	out := []workflowChoice{}
	for _, name := range config.WorkflowNames() {
		selected, err := config.SelectWorkflow(name)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		stages, _ := selected.Workflow.Order()
		out = append(out, workflowChoice{Name: name, Template: selected.Workflow.Template, Stages: stages})
	}
	writeJSON(w, map[string]any{"root": s.Root, "workflows": out})
}

func (s *Server) themes(w http.ResponseWriter, r *http.Request) {
	out := []tui.Palette{}
	for _, p := range tui.Themes() {
		if strings.HasPrefix(p.Background, "#") {
			out = append(out, p)
		}
	}
	writeJSON(w, out)
}

type createInput struct {
	OperationID string `json:"operation_id"`
	Task        string `json:"task"`
	Name        string `json:"name"`
	TaskRef     string `json:"task_ref"`
	Workflow    string `json:"workflow"`
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var in createInput
	if !readJSON(w, r, &in) {
		return
	}
	in.Task = strings.TrimSpace(in.Task)
	if in.Task == "" {
		writeError(w, http.StatusBadRequest, errors.New("describe the task for the run"))
		return
	}
	config, err := workflow.Load(s.Root)
	if err == nil {
		config, err = config.SelectWorkflow(in.Workflow)
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("workflow configuration: %w", err))
		return
	}
	if in.Name == "" {
		in.Name = firstLine(in.Task)
	}
	if in.OperationID == "" {
		in.OperationID = workflow.ID("op")
	}
	run, err := s.API.Create(r.Context(), daemon.CreateRequest{OperationID: in.OperationID, Name: in.Name, Task: in.Task, TaskRef: in.TaskRef, Owner: s.Owner, Config: config})
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, runView(*run, s.now()))
}

type actionInput struct {
	OperationID     string `json:"operation_id"`
	Action          string `json:"action"`
	ExpectedVersion int64  `json:"expected_version"`
	Revision        string `json:"revision"`
	Node            string `json:"node"`
	Attempt         string `json:"attempt"`
	WorkDigest      string `json:"work_digest"`
	Recipient       string `json:"recipient"`
	Message         string `json:"message"`
	Task            string `json:"task"`
	Priority        int    `json:"priority"`
	PluginPath      string `json:"plugin_path"`
	PluginID        string `json:"plugin_id"`
}

var actions = []string{"message", "rewind", "cancel", "approve", "priority", "plugin-attach", "plugin-remove"}

// action forwards a change fenced to the run version and revision the page
// showed, so it never applies to state the person did not see.
func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	var in actionInput
	if !readJSON(w, r, &in) {
		return
	}
	if !slices.Contains(actions, in.Action) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown action %q", in.Action))
		return
	}
	if in.ExpectedVersion < 1 || in.Revision == "" {
		writeError(w, http.StatusBadRequest, errors.New("the action is missing the run version it was made against; reload the page"))
		return
	}
	if in.OperationID == "" {
		in.OperationID = workflow.ID("op")
	}
	req := daemon.ActionRequest{OperationID: in.OperationID, ExpectedVersion: in.ExpectedVersion, Revision: in.Revision, Action: in.Action,
		Node: in.Node, Attempt: in.Attempt, WorkDigest: in.WorkDigest, Recipient: in.Recipient, Message: strings.TrimSpace(in.Message),
		Task: strings.TrimSpace(in.Task), Priority: in.Priority, PluginID: in.PluginID}
	switch in.Action {
	case "approve":
		if in.Attempt == "" || in.WorkDigest == "" {
			writeError(w, http.StatusBadRequest, errors.New("approval must name the attempt and the exact result shown"))
			return
		}
		req.Actor = s.Owner
	case "message":
		if req.Message == "" {
			writeError(w, http.StatusBadRequest, errors.New("write a message first"))
			return
		}
	case "plugin-attach":
		ref, err := plugin.ReadRef(in.PluginPath)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		req.Plugin = &ref
	}
	run, err := s.API.Action(r.Context(), r.PathValue("id"), req)
	if err != nil {
		if errors.Is(err, workflow.ErrConflict) || strings.Contains(err.Error(), "stale run version") {
			err = errors.New("the run changed before this reached it; review the latest state and try again")
		}
		writeError(w, http.StatusConflict, err)
		return
	}
	if run == nil {
		writeJSON(w, map[string]string{})
		return
	}
	writeJSON(w, runView(*run, s.now()))
}

func (s *Server) diff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := s.API.Diff(r.Context(), r.PathValue("id"), review.Request{Revision: q.Get("revision"), Node: q.Get("node"), From: q.Get("from")})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, c)
}

func (s *Server) artifact(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("digest")
	if !digestPattern.MatchString(digest) {
		writeError(w, http.StatusBadRequest, errors.New("invalid artifact digest"))
		return
	}
	raw, err := s.API.Artifact(r.Context(), digest)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, renderArtifact(raw, r.URL.Query().Get("media_type")))
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	if r := []rune(line); len(r) > 80 {
		line = string(r[:80]) + "…"
	}
	return line
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request: %w", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
