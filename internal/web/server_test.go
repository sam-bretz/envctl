package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/review"
	"github.com/sam-bretz/envctl/internal/workflow"
)

const testToken = "0123456789abcdef0123456789abcdef"

type fakeAPI struct {
	runs    []workflow.Run
	created []daemon.CreateRequest
	actions []daemon.ActionRequest
	raw     []byte
}

func (f *fakeAPI) List(context.Context) ([]workflow.Run, error) { return f.runs, nil }
func (f *fakeAPI) Create(_ context.Context, req daemon.CreateRequest) (*workflow.Run, error) {
	f.created = append(f.created, req)
	return workflow.NewRun(req.Name, req.Task, req.Owner, req.Config, time.Now())
}
func (f *fakeAPI) Action(_ context.Context, _ string, req daemon.ActionRequest) (*workflow.Run, error) {
	f.actions = append(f.actions, req)
	if len(f.runs) == 0 {
		return nil, errors.New("no run")
	}
	return &f.runs[0], nil
}
func (f *fakeAPI) Artifact(context.Context, string) ([]byte, error) { return f.raw, nil }
func (f *fakeAPI) Diff(context.Context, string, review.Request) (review.Comparison, error) {
	return review.Comparison{}, nil
}

const config = "version: 2\nproject: shop\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\nworkflows: {small: {template: small, nodes: {build: {checks: [{name: unit, command: [go, test, ./...]}]}}}}\n"

func fixture(t *testing.T) (*Server, *fakeAPI, http.Handler) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "envctl.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{}
	s := &Server{API: api, Root: root, Token: testToken, Hosts: []string{"127.0.0.1:4777", "localhost:4777"}, Owner: "tester", Interval: 10 * time.Millisecond}
	return s, api, s.Handler()
}

func request(method, path, body string, signedIn bool, headers map[string]string) *http.Request {
	r := httptest.NewRequest(method, "http://127.0.0.1:4777"+path, strings.NewReader(body))
	if signedIn {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: testToken})
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestOnlyThisMachineWithTheTokenCanUseTheDashboard(t *testing.T) {
	_, api, h := fixture(t)
	mutation := map[string]string{"Content-Type": "application/json", "X-Envctl-Token": testToken}

	if w := serve(h, request("GET", "/api/state", "", false, nil)); w.Code != http.StatusUnauthorized {
		t.Fatalf("state without a session: %d", w.Code)
	}
	rebinding := request("GET", "/api/state", "", true, nil)
	rebinding.Host = "attacker.example:4777"
	if w := serve(h, rebinding); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("a foreign Host header was answered: %d", w.Code)
	}
	if w := serve(h, request("GET", "/?token=wrong", "", false, nil)); w.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong token signed in: %d", w.Code)
	}
	w := serve(h, request("GET", "/?token="+testToken, "", false, nil))
	cookie := w.Result().Cookies()
	if w.Code != http.StatusSeeOther || len(cookie) != 1 || !cookie[0].HttpOnly || cookie[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("token link did not set a strict session cookie: %d %+v", w.Code, cookie)
	}
	page := serve(h, request("GET", "/", "", true, nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), testToken) || !strings.Contains(page.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("signed-in page: %d csp %q", page.Code, page.Header().Get("Content-Security-Policy"))
	}

	body := `{"task":"Add CSV export"}`
	if w := serve(h, request("POST", "/api/runs", body, true, map[string]string{"Content-Type": "application/json"})); w.Code != http.StatusForbidden {
		t.Fatalf("a change without the page token was accepted: %d", w.Code)
	}
	crossSite := map[string]string{"Content-Type": "application/json", "X-Envctl-Token": testToken, "Origin": "http://attacker.example"}
	if w := serve(h, request("POST", "/api/runs", body, true, crossSite)); w.Code != http.StatusForbidden {
		t.Fatalf("a cross-site change was accepted: %d", w.Code)
	}
	if w := serve(h, request("POST", "/api/runs", body, true, map[string]string{"X-Envctl-Token": testToken, "Content-Type": "text/plain"})); w.Code != http.StatusForbidden {
		t.Fatalf("a non-JSON change was accepted: %d", w.Code)
	}
	if len(api.created) != 0 {
		t.Fatal("a refused request reached the coordinator")
	}
	if w := serve(h, request("POST", "/api/runs", body, true, mutation)); w.Code != 200 || len(api.created) != 1 {
		t.Fatalf("a valid change was refused: %d %s", w.Code, w.Body)
	}
}

func TestCreateSelectsTheWorkflowAndActionsKeepTheShownVersion(t *testing.T) {
	_, api, h := fixture(t)
	headers := map[string]string{"Content-Type": "application/json", "X-Envctl-Token": testToken}

	w := serve(h, request("POST", "/api/runs", `{"task":"Add a flag\nwith details","workflow":"small"}`, true, headers))
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	req := api.created[0]
	if req.Config.WorkflowName != "small" || len(req.Config.Workflow.Nodes) != 3 || req.Name != "Add a flag" || req.Owner != "tester" {
		t.Fatalf("create request: workflow %q stages %d name %q owner %q", req.Config.WorkflowName, len(req.Config.Workflow.Nodes), req.Name, req.Owner)
	}
	var view RunView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || len(view.Revisions) != 1 || len(view.Revisions[0].Stages) != 3 {
		t.Fatalf("created run view: %v %+v", err, view)
	}
	if w := serve(h, request("POST", "/api/runs", `{"task":"x","workflow":"huge"}`, true, headers)); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "default, small") {
		t.Fatalf("unknown workflow: %d %s", w.Code, w.Body)
	}

	run, _ := api.Create(context.Background(), req)
	api.runs = []workflow.Run{*run}
	if w := serve(h, request("POST", "/api/runs/"+run.ID+"/actions", `{"action":"approve","expected_version":1,"revision":"rev_x","attempt":"attempt_1"}`, true, headers)); w.Code != http.StatusBadRequest {
		t.Fatalf("approval without the shown result digest: %d", w.Code)
	}
	if w := serve(h, request("POST", "/api/runs/"+run.ID+"/actions", `{"action":"message","message":"hi"}`, true, headers)); w.Code != http.StatusBadRequest {
		t.Fatalf("action without the shown version: %d", w.Code)
	}
	if w := serve(h, request("POST", "/api/runs/"+run.ID+"/actions", `{"action":"publish","expected_version":1,"revision":"rev_x"}`, true, headers)); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown action: %d", w.Code)
	}
	if len(api.actions) != 0 {
		t.Fatal("an invalid action reached the coordinator")
	}
	w = serve(h, request("POST", "/api/runs/"+run.ID+"/actions", `{"action":"approve","expected_version":7,"revision":"rev_x","node":"approved-change","attempt":"attempt_1","work_digest":"abc"}`, true, headers))
	a := api.actions
	if w.Code != 200 || len(a) != 1 || a[0].ExpectedVersion != 7 || a[0].Revision != "rev_x" || a[0].WorkDigest != "abc" || a[0].Actor != "tester" || a[0].OperationID == "" {
		t.Fatalf("approval not forwarded as shown: %d %+v", w.Code, a)
	}
}

func run(t *testing.T, c workflow.Config) *workflow.Run {
	t.Helper()
	r, err := workflow.NewRun("exports", "Add exports", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r.Current().State = "active"
	return r
}

func TestLanesSayWhatARunNeeds(t *testing.T) {
	c, err := workflow.Parse([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	result := &workflow.Result{Summary: "ready", Commits: map[string]string{"app": strings.Repeat("a", 40)}}

	waiting := run(t, c)
	waiting.Current().Attempts = []workflow.Attempt{{ID: "attempt_1", Node: "approved-change", State: "awaiting-approval", Number: 1, Result: result}}
	v := runView(*waiting, now)
	stage := v.Revisions[0].Stages[len(v.Revisions[0].Stages)-1]
	if v.Lane != "attention" || !strings.Contains(v.Attention, "approved-change is waiting") || stage.Awaiting == nil || stage.Awaiting.WorkDigest != result.WorkDigest() {
		t.Fatalf("waiting run: lane %q %q awaiting %+v", v.Lane, v.Attention, stage.Awaiting)
	}

	blocked := run(t, c)
	blocked.Current().Attempts = []workflow.Attempt{{ID: "attempt_1", Node: "approved-change", State: "verifying", Result: result, Approval: &workflow.Approval{Actor: "dev"}}}
	blocked.Current().Recovery = &workflow.Recovery{Phase: "publication", Detail: "main moved"}
	v = runView(*blocked, now)
	if v.Lane != "attention" || v.Revisions[0].Blocked == nil || v.Revisions[0].Stages[5].Status != "approved, not published" {
		t.Fatalf("blocked publication: lane %q blocked %+v", v.Lane, v.Revisions[0].Blocked)
	}

	stalled := run(t, c)
	stalled.Current().Checkpoints = map[string]workflow.Checkpoint{"plan": {ID: "cp_plan", Node: "plan"}}
	stalled.Current().Readiness = []workflow.Probe{{Capability: "publication.pr", Passed: false, Detail: "main moved on GitHub after this run planned"}}
	v = runView(*stalled, now)
	if v.Lane != "attention" || !strings.Contains(v.Revisions[0].Stalled, "main moved") {
		t.Fatalf("stalled run: lane %q stalled %q", v.Lane, v.Revisions[0].Stalled)
	}
	stalled.Current().Attempts = []workflow.Attempt{{ID: "attempt_2", Node: "design", State: "running"}}
	if v = runView(*stalled, now); v.Lane != "running" || v.Revisions[0].Stalled != "" {
		t.Fatalf("a run with work in progress was reported blocked: %q", v.Lane)
	}

	done := run(t, c)
	done.Current().State = "completed"
	if v = runView(*done, now); v.Lane != "finished" {
		t.Fatalf("completed run lane %q", v.Lane)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "credential") || strings.Contains(string(raw), `"config"`) {
		t.Fatal("the run view exposes configuration")
	}
}

func TestArtifactsRenderWithoutActiveContent(t *testing.T) {
	md := renderArtifact([]byte("# Plan\n\n<script>alert(1)</script>\n\n[bad](javascript:alert(1)) [good](https://example.com)\n\n| a | b |\n| - | - |\n| 1 | 2 |\n"), "text/markdown")
	if md.Kind != "markdown" || strings.Contains(md.HTML, "<script") || strings.Contains(md.HTML, "javascript:") || !strings.Contains(md.HTML, `href="https://example.com"`) || !strings.Contains(md.HTML, "<table>") {
		t.Fatalf("markdown rendered unsafely or incompletely: %s", md.HTML)
	}
	js := renderArtifact([]byte(`{"steps":[{"screenshot_png":"iVBORw0KGgo="}]}`), "application/json")
	if js.Kind != "json" || len(js.Images) != 1 || !strings.HasPrefix(js.Images[0], "data:image/png;base64,") || strings.Contains(js.Text, "iVBOR") {
		t.Fatalf("screenshot not extracted: %+v", js)
	}
	if bin := renderArtifact([]byte{0xff, 0xfe, 0x00}, ""); bin.Kind != "binary" {
		t.Fatalf("binary artifact kind %q", bin.Kind)
	}
}

func TestStreamSendsStateOnlyWhenItChanges(t *testing.T) {
	s, api, _ := fixture(t)
	c, _ := workflow.Parse([]byte(config))
	api.runs = []workflow.Run{*run(t, c)}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	s.Hosts = []string{strings.TrimPrefix(srv.URL, "http://")}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/stream", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: testToken})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	events := 0
	deadline := time.After(300 * time.Millisecond)
	lines := make(chan string)
	go func() {
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed")
			}
			if strings.HasPrefix(line, "data: ") {
				var st State
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &st); err != nil || len(st.Runs) != 1 {
					t.Fatalf("state event: %v %s", err, line)
				}
				events++
			}
		case <-deadline:
			if events != 1 {
				t.Fatalf("an unchanged state was sent %d times", events)
			}
			return
		}
	}
}
