package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

const linearEndpoint = "https://api.linear.app/graphql"

// HTTPError reports a non-2xx Linear response whose body could not be
// decoded as a GraphQL error payload.
type HTTPError struct{ Code int }

func (e HTTPError) Error() string { return fmt.Sprintf("linear request failed (HTTP %d)", e.Code) }

type linearClient struct {
	// endpoint is linearEndpoint in production; tests override it to point
	// at an httptest.Server so the suite never dials api.linear.app.
	endpoint string
	key      string
	http     *http.Client
}

var _ Tracker = (*linearClient)(nil)
var _ StatusUpdater = (*linearClient)(nil)

func newLinearClient(cfg workflow.TrackerConfig) (*linearClient, error) {
	key, err := ResolveCredential(cfg)
	if err != nil {
		return nil, err
	}
	return &linearClient{endpoint: linearEndpoint, key: key, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

type graphQLRequest struct {
	Query     string `json:"query"`
	Variables any    `json:"variables,omitempty"`
}
type graphQLError struct {
	Message string `json:"message"`
}
type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphQLError  `json:"errors"`
}

// limitedReader mirrors publication's bounded response cap: a compromised or
// misbehaving endpoint cannot make the coordinator buffer unbounded memory.
const maxLinearResponseBytes = 4 << 20

func (c *linearClient) call(ctx context.Context, query string, variables any, out any) error {
	raw, err := json.Marshal(graphQLRequest{Query: query, Variables: variables})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLinearResponseBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxLinearResponseBytes {
		return errors.New("linear response exceeds limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var parsed graphQLResponse
		if json.Unmarshal(body, &parsed) == nil && len(parsed.Errors) > 0 {
			return fmt.Errorf("linear: %s", parsed.Errors[0].Message)
		}
		return HTTPError{resp.StatusCode}
	}
	var parsed graphQLResponse
	if err = json.Unmarshal(body, &parsed); err != nil {
		return errors.New("invalid linear response")
	}
	if len(parsed.Errors) > 0 {
		return fmt.Errorf("linear: %s", parsed.Errors[0].Message)
	}
	if out == nil {
		return nil
	}
	if err = json.Unmarshal(parsed.Data, out); err != nil {
		return errors.New("invalid linear response data")
	}
	return nil
}

// viewer authenticates the credential and returns the caller's identity.
func (c *linearClient) viewer(ctx context.Context) (string, error) {
	var out struct {
		Viewer struct {
			ID string `json:"id"`
		} `json:"viewer"`
	}
	if err := c.call(ctx, `query { viewer { id } }`, nil, &out); err != nil {
		return "", err
	}
	if out.Viewer.ID == "" {
		return "", errors.New("linear credential rejected")
	}
	return out.Viewer.ID, nil
}

// Viewer checks the credential with a read, for the setup wizard.
func (c *linearClient) Viewer(ctx context.Context) (string, error) { return c.viewer(ctx) }

// Teams lists the teams the credential can see, for the setup wizard.
func (c *linearClient) Teams(ctx context.Context) ([]Team, error) {
	var out struct {
		Teams struct {
			Nodes []struct {
				ID   string `json:"id"`
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := c.call(ctx, `query { teams { nodes { id key name } } }`, nil, &out); err != nil {
		return nil, err
	}
	teams := make([]Team, 0, len(out.Teams.Nodes))
	for _, t := range out.Teams.Nodes {
		if t.ID != "" {
			teams = append(teams, Team{ID: t.ID, Key: t.Key, Name: t.Name})
		}
	}
	return teams, nil
}

// States lists a team's workflow states with their type. workflowStates
// serves readiness, which only needs names; the wizard also needs the type to
// suggest a mapping.
func (c *linearClient) States(ctx context.Context, teamID string) ([]State, error) {
	var out struct {
		Team *struct {
			States struct {
				Nodes []struct {
					Name     string  `json:"name"`
					Type     string  `json:"type"`
					Position float64 `json:"position"`
				} `json:"nodes"`
			} `json:"states"`
		} `json:"team"`
	}
	if err := c.call(ctx, `query($id: String!) { team(id: $id) { states { nodes { name type position } } } }`, map[string]string{"id": teamID}, &out); err != nil {
		return nil, err
	}
	if out.Team == nil {
		return nil, errors.New("linear team not found")
	}
	nodes := out.Team.States.Nodes
	// Position is the order the team's board shows, which is the order a
	// person expects to choose from.
	slices.SortStableFunc(nodes, func(a, b struct {
		Name     string  `json:"name"`
		Type     string  `json:"type"`
		Position float64 `json:"position"`
	}) int {
		switch {
		case a.Position < b.Position:
			return -1
		case a.Position > b.Position:
			return 1
		}
		return 0
	})
	states := make([]State, 0, len(nodes))
	for _, n := range nodes {
		if n.Name != "" {
			states = append(states, State{Name: n.Name, Type: n.Type})
		}
	}
	return states, nil
}

// issue resolves ref to its internal ID and owning team.
func (c *linearClient) issue(ctx context.Context, ref string) (string, string, error) {
	var out struct {
		Issue *struct {
			ID   string `json:"id"`
			Team struct {
				ID string `json:"id"`
			} `json:"team"`
		} `json:"issue"`
	}
	if err := c.call(ctx, `query($id: String!) { issue(id: $id) { id team { id } } }`, map[string]string{"id": ref}, &out); err != nil {
		return "", "", err
	}
	if out.Issue == nil {
		return "", "", fmt.Errorf("linear issue %s not found", ref)
	}
	return out.Issue.ID, out.Issue.Team.ID, nil
}

func (c *linearClient) workflowStates(ctx context.Context, teamID string) (map[string]string, []string, error) {
	var out struct {
		Team *struct {
			States struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"states"`
		} `json:"team"`
	}
	if err := c.call(ctx, `query($id: String!) { team(id: $id) { states { nodes { id name } } } }`, map[string]string{"id": teamID}, &out); err != nil {
		return nil, nil, err
	}
	if out.Team == nil {
		return nil, nil, errors.New("linear team not found")
	}
	byName := map[string]string{}
	valid := make([]string, 0, len(out.Team.States.Nodes))
	for _, state := range out.Team.States.Nodes {
		if state.ID == "" || state.Name == "" {
			continue
		}
		byName[strings.ToLower(state.Name)] = state.ID
		valid = append(valid, state.Name)
	}
	slices.Sort(valid)
	return byName, valid, nil
}

// issueState returns the issue and its current state in one read. It is used
// by SetStatus so a retry after a coordinator crash does not repeat a state
// mutation that already reached Linear.
func (c *linearClient) issueState(ctx context.Context, ref string) (string, string, string, error) {
	var out struct {
		Issue *struct {
			ID   string `json:"id"`
			Team struct {
				ID string `json:"id"`
			} `json:"team"`
			State struct {
				Name string `json:"name"`
			} `json:"state"`
		} `json:"issue"`
	}
	if err := c.call(ctx, `query($id: String!) { issue(id: $id) { id team { id } state { name } } }`, map[string]string{"id": ref}, &out); err != nil {
		return "", "", "", err
	}
	if out.Issue == nil {
		return "", "", "", fmt.Errorf("linear issue %s not found", ref)
	}
	return out.Issue.ID, out.Issue.Team.ID, out.Issue.State.Name, nil
}

// SetStatus applies the configured state by name. Linear exposes state IDs in
// mutations, so the team state list is resolved immediately before changing
// the issue and also supplies the authoritative names used by readiness.
func (c *linearClient) SetStatus(ctx context.Context, ref, statusName string) error {
	issueID, teamID, current, err := c.issueState(ctx, ref)
	if err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(current), strings.TrimSpace(statusName)) {
		return nil
	}
	// Mapping is intentionally unconditional: a later envctl event is the
	// authoritative writer, so manual moves are corrected by the next event.
	states, valid, err := c.workflowStates(ctx, teamID)
	if err != nil {
		return err
	}
	stateID := states[strings.ToLower(strings.TrimSpace(statusName))]
	if stateID == "" {
		return fmt.Errorf("linear status %q not found; valid statuses are %s", statusName, strings.Join(valid, ", "))
	}
	var out struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	mutation := `mutation($id: String!, $stateId: String!) { issueUpdate(id: $id, input: {stateId: $stateId}) { success } }`
	if err := c.call(ctx, mutation, map[string]string{"id": issueID, "stateId": stateID}, &out); err != nil {
		return err
	}
	if !out.IssueUpdate.Success {
		return errors.New("linear issue status update did not succeed")
	}
	return nil
}

// canComment reports whether viewerID is a member of teamID, the signal
// used to check comment permission without posting anything.
func (c *linearClient) canComment(ctx context.Context, teamID, viewerID string) (bool, error) {
	var out struct {
		Team *struct {
			Members struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
			} `json:"members"`
		} `json:"team"`
	}
	if err := c.call(ctx, `query($id: String!) { team(id: $id) { members(first: 250) { nodes { id } } } }`, map[string]string{"id": teamID}, &out); err != nil {
		return false, err
	}
	if out.Team == nil {
		return false, errors.New("linear team not found")
	}
	for _, n := range out.Team.Members.Nodes {
		if n.ID == viewerID {
			return true, nil
		}
	}
	return false, nil
}

// findComment reports the ID of an existing comment on issueID whose body
// contains marker, if any.
func (c *linearClient) findComment(ctx context.Context, issueID, marker string) (string, bool, error) {
	var out struct {
		Issue *struct {
			Comments struct {
				Nodes []struct {
					ID   string `json:"id"`
					Body string `json:"body"`
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"issue"`
	}
	if err := c.call(ctx, `query($id: String!) { issue(id: $id) { comments(last: 50) { nodes { id body } } } }`, map[string]string{"id": issueID}, &out); err != nil {
		return "", false, err
	}
	if out.Issue == nil {
		return "", false, fmt.Errorf("linear issue %s not found", issueID)
	}
	for _, n := range out.Issue.Comments.Nodes {
		if strings.Contains(n.Body, marker) {
			return n.ID, true, nil
		}
	}
	return "", false, nil
}

// createComment posts a new comment on issueID.
func (c *linearClient) createComment(ctx context.Context, issueID, body string) (string, error) {
	var out struct {
		CommentCreate struct {
			Success bool `json:"success"`
			Comment struct {
				ID string `json:"id"`
			} `json:"comment"`
		} `json:"commentCreate"`
	}
	mutation := `mutation($issueId: String!, $body: String!) { commentCreate(input: {issueId: $issueId, body: $body}) { success comment { id } } }`
	if err := c.call(ctx, mutation, map[string]string{"issueId": issueID, "body": body}, &out); err != nil {
		return "", err
	}
	if !out.CommentCreate.Success || out.CommentCreate.Comment.ID == "" {
		return "", errors.New("linear comment creation did not succeed")
	}
	return out.CommentCreate.Comment.ID, nil
}

// Comment resolves ref to its issue, reconciles against any comment already
// carrying marker, and otherwise creates one. Checking for the marker first
// is what makes Comment safe to call again after a crash between a
// successful remote create and a local receipt being recorded.
func (c *linearClient) Comment(ctx context.Context, ref, marker, body string) (string, error) {
	issueID, _, err := c.issue(ctx, ref)
	if err != nil {
		return "", err
	}
	if id, found, err := c.findComment(ctx, issueID, marker); err != nil {
		return "", err
	} else if found {
		return id, nil
	}
	return c.createComment(ctx, issueID, body+"\n\n<!-- "+marker+" -->")
}
