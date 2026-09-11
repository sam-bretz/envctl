package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/sam-bretz/envctl/internal/review"
	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type Client struct {
	HTTP    *http.Client
	BaseURL string
}

func NewClient(dir string) *Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", Socket(dir))
	}}
	return &Client{HTTP: &http.Client{Transport: transport, Timeout: 15 * time.Second}, BaseURL: "http://envctl"}
}
func (c *Client) request(ctx context.Context, method, path string, body, out any) error {
	var input io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, input)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		var e APIError
		if err = json.NewDecoder(res.Body).Decode(&e); err != nil {
			return fmt.Errorf("daemon HTTP %d", res.StatusCode)
		}
		switch e.Code {
		case "conflict":
			return fmt.Errorf("%w: %s", workflow.ErrConflict, e.Message)
		case "not_found":
			return fmt.Errorf("%w: %s", runstore.ErrNotFound, e.Message)
		}
		return errors.New(e.Message)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, res.Body)
		return err
	}
	return json.NewDecoder(res.Body).Decode(out)
}
func (c *Client) Health(ctx context.Context) error {
	return c.request(ctx, "GET", "/v1/health", nil, nil)
}
func (c *Client) List(ctx context.Context) ([]workflow.Run, error) {
	var out []workflow.Run
	err := c.request(ctx, "GET", "/v1/runs", nil, &out)
	return out, err
}
func (c *Client) Get(ctx context.Context, id string) (*workflow.Run, error) {
	var out workflow.Run
	err := c.request(ctx, "GET", "/v1/runs/"+url.PathEscape(id), nil, &out)
	return &out, err
}
func (c *Client) Create(ctx context.Context, req CreateRequest) (*workflow.Run, error) {
	var out workflow.Run
	err := c.request(ctx, "POST", "/v1/runs", req, &out)
	return &out, err
}
func (c *Client) Action(ctx context.Context, id string, req ActionRequest) (*workflow.Run, error) {
	var out workflow.Run
	err := c.request(ctx, "POST", "/v1/runs/"+url.PathEscape(id)+"/actions", req, &out)
	return &out, err
}
func (c *Client) Events(ctx context.Context, id string, after int64) ([]runstore.Event, error) {
	var out []runstore.Event
	err := c.request(ctx, "GET", "/v1/events?run="+url.QueryEscape(id)+"&after="+strconv.FormatInt(after, 10), nil, &out)
	return out, err
}
func (c *Client) Diff(ctx context.Context, id string, req review.Request) (review.Comparison, error) {
	query := url.Values{"revision": {req.Revision}, "node": {req.Node}, "from": {req.From}}
	var out review.Comparison
	err := c.request(ctx, "GET", "/v1/runs/"+url.PathEscape(id)+"/diff?"+query.Encode(), nil, &out)
	return out, err
}
func (c *Client) Artifact(ctx context.Context, digest string) ([]byte, error) {
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != digest {
		return nil, errors.New("invalid artifact digest")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/v1/artifacts/"+url.PathEscape(digest), nil)
	if err != nil {
		return nil, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("artifact HTTP %d", res.StatusCode)
	}
	const limit = 64 << 20
	raw, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, errors.New("artifact exceeds the 64 MiB transfer limit")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != digest {
		return nil, errors.New("artifact response failed its checksum verification")
	}
	return raw, nil
}
