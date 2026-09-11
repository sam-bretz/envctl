// Package daemon exposes the same revision-checked control API to CLI and TUI.
// The Unix socket is private to its owner. No agent completion endpoint exists:
// checkpoint acceptance belongs to the coordinator's verifier.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/sam-bretz/envctl/internal/plugin"
	"github.com/sam-bretz/envctl/internal/review"
	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type Coordinator interface{ Run(context.Context) error }
type Server struct {
	Store       *runstore.Store
	Coordinator Coordinator
}
type CreateRequest struct {
	OperationID string          `json:"operation_id"`
	Name        string          `json:"name"`
	Task        string          `json:"task"`
	TaskRef     string          `json:"task_ref,omitempty"`
	Owner       string          `json:"owner"`
	Config      workflow.Config `json:"config"`
}
type ActionRequest struct {
	OperationID     string              `json:"operation_id"`
	ExpectedVersion int64               `json:"expected_version"`
	Revision        string              `json:"revision"`
	Action          string              `json:"action"`
	Node            string              `json:"node,omitempty"`
	Attempt         string              `json:"attempt,omitempty"`
	Task            string              `json:"task,omitempty"`
	Recipient       string              `json:"recipient,omitempty"`
	Message         string              `json:"message,omitempty"`
	Actor           string              `json:"actor,omitempty"`
	WorkDigest      string              `json:"work_digest,omitempty"`
	Priority        int                 `json:"priority,omitempty"`
	Config          *workflow.Config    `json:"config,omitempty"`
	Plugin          *workflow.PluginRef `json:"plugin,omitempty"`
	PluginID        string              `json:"plugin_id,omitempty"`
}
type APIError struct {
	Message string `json:"error"`
	Code    string `json:"code"`
}

func Socket(dir string) string { return filepath.Join(dir, "daemon.sock") }
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]any{"api_version": 1, "pid": os.Getpid()})
	})
	mux.HandleFunc("GET /v1/runs", func(w http.ResponseWriter, r *http.Request) {
		runs, err := s.Store.List(r.Context())
		reply(w, runs, err)
	})
	mux.HandleFunc("POST /v1/runs", func(w http.ResponseWriter, r *http.Request) {
		var req CreateRequest
		if !decodeRequest(w, r, &req) {
			return
		}
		if prior, err := s.Store.Replay(r.Context(), req.OperationID, "", 0, "create", req); err != nil || prior != nil {
			reply(w, prior, err)
			return
		}
		config, _, err := plugin.Freeze(filepath.Join(s.Store.Dir, "plugins"), req.Config)
		if err != nil {
			reply(w, nil, err)
			return
		}
		run, err := workflow.NewRun(req.Name, req.Task, req.Owner, config, time.Now().UTC())
		if err == nil {
			run.TaskRef = req.TaskRef
			run, err = s.Store.Create(r.Context(), req.OperationID, req, run)
		}
		reply(w, run, err)
	})
	mux.HandleFunc("GET /v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		run, err := s.Store.Get(r.Context(), r.PathValue("id"))
		reply(w, run, err)
	})
	mux.HandleFunc("POST /v1/runs/{id}/actions", s.action)
	mux.HandleFunc("GET /v1/runs/{id}/diff", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		comparison, err := review.Compare(r.Context(), s.Store, r.PathValue("id"), review.Request{Revision: q.Get("revision"), Node: q.Get("node"), From: q.Get("from")})
		reply(w, comparison, err)
	})
	mux.HandleFunc("GET /v1/events", s.events)
	mux.HandleFunc("GET /v1/artifacts/{digest}", func(w http.ResponseWriter, r *http.Request) {
		b, err := s.Store.Artifact(r.PathValue("digest"))
		if err != nil {
			reply(w, nil, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(b)
	})
	return mux
}
func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	var req ActionRequest
	if !decodeRequest(w, r, &req) {
		return
	}
	if prior, err := s.Store.Replay(r.Context(), req.OperationID, r.PathValue("id"), req.ExpectedVersion, "run."+req.Action, req); err != nil || prior != nil {
		reply(w, prior, err)
		return
	}
	var config *workflow.Config
	if req.Action == "plugin-attach" || req.Action == "plugin-remove" || (req.Action == "rewind" && req.Config != nil) {
		current, err := s.Store.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			reply(w, nil, err)
			return
		}
		if current.Version != req.ExpectedVersion || current.CurrentRevision != req.Revision {
			reply(w, nil, workflow.ErrConflict)
			return
		}
		c := workflow.Clone(current.Current().Config)
		if req.Action == "rewind" {
			c = workflow.Clone(*req.Config)
		} else {
			id := req.PluginID
			if req.Action == "plugin-attach" {
				if req.Plugin == nil {
					reply(w, nil, errors.New("plugin reference required"))
					return
				}
				id = req.Plugin.ID
			}
			var refs []workflow.PluginRef
			found := false
			for _, p := range c.Plugins {
				if p.ID != id {
					refs = append(refs, p)
				} else {
					found = true
					if req.Action == "plugin-attach" {
						refs = append(refs, *req.Plugin)
					}
				}
			}
			if req.Action == "plugin-remove" && !found {
				reply(w, nil, errors.New("plugin is not attached to this invocation"))
				return
			}
			if req.Action == "plugin-attach" && !found {
				refs = append(refs, *req.Plugin)
			}
			c.Plugins = refs
		}
		if err = c.Validate(); err != nil {
			reply(w, nil, err)
			return
		}
		frozen, _, err := plugin.Freeze(filepath.Join(s.Store.Dir, "plugins"), c)
		if err != nil {
			reply(w, nil, err)
			return
		}
		config = &frozen
	}
	run, err := s.Store.Mutate(r.Context(), r.PathValue("id"), req.ExpectedVersion, req.OperationID, "run."+req.Action, req, func(run *workflow.Run) error {
		if req.Revision != run.CurrentRevision {
			return workflow.ErrConflict
		}
		now := time.Now().UTC()
		switch req.Action {
		case "message":
			return run.Message(req.Revision, req.Node, req.Recipient, req.Message, now)
		case "priority":
			run.Priority = req.Priority
			return nil
		case "cancel":
			run.Cancel(now)
			return nil
		case "rewind":
			_, err := run.Rewind(req.Node, req.Task, config, now)
			return err
		case "plugin-attach", "plugin-remove":
			if workflow.Digest(run.Current().Config) == workflow.Digest(*config) {
				return nil
			}
			_, err := run.Rewind(run.Current().Config.Workflow.PlanID(), "", config, now)
			return err
		case "approve":
			return run.Approve(req.Revision, req.Attempt, req.Actor, req.WorkDigest, now)
		default:
			return fmt.Errorf("unknown action %q", req.Action)
		}
	})
	reply(w, run, err)
}
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	after := int64(0)
	if a := r.URL.Query().Get("after"); a != "" {
		var err error
		after, err = strconv.ParseInt(a, 10, 64)
		if err != nil || after < 0 {
			reply(w, nil, errors.New("invalid event cursor"))
			return
		}
	}
	if r.URL.Query().Get("follow") != "true" {
		events, err := s.Store.Events(r.Context(), r.URL.Query().Get("run"), after, 1000)
		reply(w, events, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		reply(w, nil, errors.New("streaming unavailable"))
		return
	}
	flusher.Flush()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		events, err := s.Store.Events(r.Context(), r.URL.Query().Get("run"), after, 1000)
		if err != nil {
			return
		}
		for _, e := range events {
			if err = json.NewEncoder(w).Encode(e); err != nil {
				return
			}
			after = e.Sequence
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
func decodeRequest(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		reply(w, nil, err)
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		reply(w, nil, errors.New("expected one JSON request"))
		return false
	}
	return true
}
func respond(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func reply(w http.ResponseWriter, value any, err error) {
	if err == nil {
		respond(w, value)
		return
	}
	status := http.StatusBadRequest
	code := "invalid_request"
	if errors.Is(err, workflow.ErrConflict) || errors.Is(err, runstore.ErrOperationReuse) {
		status = http.StatusConflict
		code = "conflict"
	}
	if errors.Is(err, runstore.ErrNotFound) {
		status = http.StatusNotFound
		code = "not_found"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(APIError{Message: err.Error(), Code: code})
}

// Serve holds an OS lock, so stale sockets are removed only after proving that
// no other coordinator owns this state directory. The lock dies with the process.
func (s *Server) Serve(ctx context.Context) error {
	lock, err := os.OpenFile(filepath.Join(s.Store.Dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("coordinator already owns this state directory: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	path := Socket(s.Store.Dir)
	if len(path) > 100 {
		return fmt.Errorf("coordinator socket path exceeds platform limit; use a shorter --state-dir: %s", path)
	}
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(path)
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	ctx, cancelCoordinator := context.WithCancel(ctx)
	var coordinatorDone chan struct{}
	var coordinatorErr error
	if s.Coordinator != nil {
		coordinatorDone = make(chan struct{})
		go func() {
			coordinatorErr = s.Coordinator.Run(ctx)
			if coordinatorErr == nil && ctx.Err() == nil {
				coordinatorErr = errors.New("execution coordinator exited unexpectedly")
			}
			close(coordinatorDone)
		}()
	}
	defer func() {
		cancelCoordinator()
		if coordinatorDone != nil {
			<-coordinatorDone
		}
	}()
	srv := &http.Server{Handler: s.Handler(), BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-coordinatorDone:
		case <-done:
			return
		}
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	err = srv.Serve(listener)
	cancelCoordinator()
	if coordinatorDone != nil {
		<-coordinatorDone
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, coordinatorErr)
}
