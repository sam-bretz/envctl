package tracker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordingServer captures every GraphQL request body and lets a test choose
// the response by inspecting the query. The suite never dials api.linear.app:
// every linearClient in this file is pointed at server.URL.
type recordingServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []graphQLRequest
	handler  func(graphQLRequest) (int, string)
}

func newRecordingServer(t *testing.T) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "test-key" {
			t.Fatalf("unexpected Authorization header: %q", got)
		}
		var req graphQLRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("invalid request body: %v", err)
		}
		rs.mu.Lock()
		rs.requests = append(rs.requests, req)
		rs.mu.Unlock()
		code, body := 200, `{"data":{}}`
		if rs.handler != nil {
			code, body = rs.handler(req)
		}
		w.WriteHeader(code)
		w.Write([]byte(body))
	}))
	t.Cleanup(rs.Close)
	return rs
}
func (rs *recordingServer) count() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.requests)
}
func (rs *recordingServer) last() graphQLRequest {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.requests[len(rs.requests)-1]
}
func newClient(rs *recordingServer) *linearClient {
	return &linearClient{endpoint: rs.URL, key: "test-key", http: rs.Client()}
}

func TestLinearViewer(t *testing.T) {
	rs := newRecordingServer(t)
	rs.handler = func(graphQLRequest) (int, string) { return 200, `{"data":{"viewer":{"id":"user_1"}}}` }
	id, err := newClient(rs).viewer(context.Background())
	if err != nil || id != "user_1" {
		t.Fatalf("viewer() = %q, %v", id, err)
	}
	if !strings.Contains(rs.last().Query, "viewer") {
		t.Fatal("request did not send a viewer query")
	}
}

func TestLinearViewerRejected(t *testing.T) {
	rs := newRecordingServer(t)
	rs.handler = func(graphQLRequest) (int, string) { return 200, `{"data":{"viewer":{"id":""}}}` }
	if _, err := newClient(rs).viewer(context.Background()); err == nil {
		t.Fatal("expected an error for an empty viewer ID")
	}
}

func TestLinearIssueFoundAndNotFound(t *testing.T) {
	rs := newRecordingServer(t)
	rs.handler = func(graphQLRequest) (int, string) {
		return 200, `{"data":{"issue":{"id":"issue_1","team":{"id":"team_1"}}}}`
	}
	id, team, err := newClient(rs).issue(context.Background(), "ENG-1")
	if err != nil || id != "issue_1" || team != "team_1" {
		t.Fatalf("issue() = %q, %q, %v", id, team, err)
	}

	rs.handler = func(graphQLRequest) (int, string) { return 200, `{"data":{"issue":null}}` }
	if _, _, err := newClient(rs).issue(context.Background(), "ENG-404"); err == nil {
		t.Fatal("expected an error for a missing issue")
	}
}

func TestLinearCanComment(t *testing.T) {
	rs := newRecordingServer(t)
	rs.handler = func(graphQLRequest) (int, string) {
		return 200, `{"data":{"team":{"members":{"nodes":[{"id":"user_1"},{"id":"user_2"}]}}}}`
	}
	c := newClient(rs)
	ok, err := c.canComment(context.Background(), "team_1", "user_2")
	if err != nil || !ok {
		t.Fatalf("canComment() = %v, %v", ok, err)
	}
	ok, err = c.canComment(context.Background(), "team_1", "user_9")
	if err != nil || ok {
		t.Fatalf("canComment() for absent member = %v, %v", ok, err)
	}
}

func TestLinearFindCommentAndCreateComment(t *testing.T) {
	rs := newRecordingServer(t)
	rs.handler = func(req graphQLRequest) (int, string) {
		if strings.Contains(req.Query, "comments(") {
			return 200, `{"data":{"issue":{"comments":{"nodes":[{"id":"c1","body":"hello <!-- envctl:tlog_1 -->"}]}}}}`
		}
		return 200, `{"data":{"commentCreate":{"success":true,"comment":{"id":"c2"}}}}`
	}
	c := newClient(rs)
	id, found, err := c.findComment(context.Background(), "issue_1", "envctl:tlog_1")
	if err != nil || !found || id != "c1" {
		t.Fatalf("findComment() = %q, %v, %v", id, found, err)
	}
	id, found, err = c.findComment(context.Background(), "issue_1", "envctl:tlog_2")
	if err != nil || found || id != "" {
		t.Fatalf("findComment() unexpectedly found: %q, %v, %v", id, found, err)
	}
	id, err = c.createComment(context.Background(), "issue_1", "a new comment")
	if err != nil || id != "c2" {
		t.Fatalf("createComment() = %q, %v", id, err)
	}
}

func TestLinearCommentReconcilesWithoutDuplicateCreate(t *testing.T) {
	rs := newRecordingServer(t)
	var createCalls int
	rs.handler = func(req graphQLRequest) (int, string) {
		switch {
		case strings.Contains(req.Query, "issue(id: $id) { id team"):
			return 200, `{"data":{"issue":{"id":"issue_1","team":{"id":"team_1"}}}}`
		case strings.Contains(req.Query, "comments("):
			return 200, `{"data":{"issue":{"comments":{"nodes":[{"id":"c1","body":"hi <!-- envctl:tlog_1 -->"}]}}}}`
		case strings.Contains(req.Query, "commentCreate"):
			createCalls++
			return 200, `{"data":{"commentCreate":{"success":true,"comment":{"id":"c2"}}}}`
		}
		return 200, `{"data":{}}`
	}
	id, err := newClient(rs).Comment(context.Background(), "ENG-1", "envctl:tlog_1", "body")
	if err != nil || id != "c1" {
		t.Fatalf("Comment() = %q, %v", id, err)
	}
	if createCalls != 0 {
		t.Fatalf("Comment() created a duplicate comment: %d calls", createCalls)
	}
}

func TestLinearCommentCreatesWhenMarkerAbsent(t *testing.T) {
	rs := newRecordingServer(t)
	rs.handler = func(req graphQLRequest) (int, string) {
		switch {
		case strings.Contains(req.Query, "issue(id: $id) { id team"):
			return 200, `{"data":{"issue":{"id":"issue_1","team":{"id":"team_1"}}}}`
		case strings.Contains(req.Query, "comments("):
			return 200, `{"data":{"issue":{"comments":{"nodes":[]}}}}`
		case strings.Contains(req.Query, "commentCreate"):
			return 200, `{"data":{"commentCreate":{"success":true,"comment":{"id":"c9"}}}}`
		}
		return 200, `{"data":{}}`
	}
	id, err := newClient(rs).Comment(context.Background(), "ENG-1", "envctl:tlog_1", "body")
	if err != nil || id != "c9" {
		t.Fatalf("Comment() = %q, %v", id, err)
	}
}

func TestLinearSetStatusResolvesTheTeamStateAndUpdatesTheIssue(t *testing.T) {
	rs := newRecordingServer(t)
	rs.handler = func(req graphQLRequest) (int, string) {
		switch {
		case strings.Contains(req.Query, "state { name }"):
			return 200, `{"data":{"issue":{"id":"issue_1","team":{"id":"team_1"},"state":{"name":"Todo"}}}}`
		case strings.Contains(req.Query, "states { nodes"):
			return 200, `{"data":{"team":{"states":{"nodes":[{"id":"state_todo","name":"Todo"},{"id":"state_progress","name":"In Progress"}]}}}}`
		case strings.Contains(req.Query, "issueUpdate"):
			return 200, `{"data":{"issueUpdate":{"success":true}}}`
		}
		return 200, `{"data":{}}`
	}
	if err := newClient(rs).SetStatus(context.Background(), "ENG-1", "In Progress"); err != nil {
		t.Fatal(err)
	}
	if rs.count() != 3 || !strings.Contains(rs.last().Query, "issueUpdate") {
		t.Fatalf("status update did not read issue/state and mutate it: %d requests, last %q", rs.count(), rs.last().Query)
	}
}

func TestLinearSetStatusDoesNotMutateAnIssueAlreadyInTheRequestedState(t *testing.T) {
	rs := newRecordingServer(t)
	rs.handler = func(req graphQLRequest) (int, string) {
		if strings.Contains(req.Query, "state { name }") {
			return 200, `{"data":{"issue":{"id":"issue_1","team":{"id":"team_1"},"state":{"name":"Done"}}}}`
		}
		return 200, `{"data":{}}`
	}
	if err := newClient(rs).SetStatus(context.Background(), "ENG-1", "Done"); err != nil {
		t.Fatal(err)
	}
	if rs.count() != 1 {
		t.Fatalf("already reconciled status made extra API calls: %d", rs.count())
	}
}

func TestLinearGraphQLErrorsAndHTTPErrors(t *testing.T) {
	rs := newRecordingServer(t)
	rs.handler = func(graphQLRequest) (int, string) { return 200, `{"errors":[{"message":"boom"}]}` }
	if _, err := newClient(rs).viewer(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected a graphql error surfaced, got %v", err)
	}
	rs.handler = func(graphQLRequest) (int, string) { return 500, `not json` }
	if _, err := newClient(rs).viewer(context.Background()); err == nil {
		t.Fatal("expected an HTTP error")
	} else if httpErr, ok := err.(HTTPError); !ok || httpErr.Code != 500 {
		t.Fatalf("expected HTTPError{500}, got %v (%T)", err, err)
	}
}

func TestLinearNeverDialsRealEndpoint(t *testing.T) {
	c := &linearClient{endpoint: "http://127.0.0.1:0", key: "x", http: http.DefaultClient}
	if c.endpoint == linearEndpoint {
		t.Fatal("test client accidentally points at the real endpoint")
	}
}
