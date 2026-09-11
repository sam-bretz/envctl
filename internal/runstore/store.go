// Package runstore persists coordinator-owned workflow state. An operation
// receipt, state mutation, and its event commit together or not at all.
package runstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("run not found")
var ErrOperationReuse = errors.New("operation ID reused with different request")
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Store struct {
	db     *sql.DB
	Dir    string
	guards sync.Map
}
type Event struct {
	Sequence int64     `json:"sequence"`
	RunID    string    `json:"run_id"`
	Revision string    `json:"revision"`
	Version  int64     `json:"version"`
	Type     string    `json:"type"`
	At       time.Time `json:"at"`
}

func DefaultDir() (string, error) {
	if d := os.Getenv("ENVCTL_STATE_DIR"); d != "" {
		return filepath.Abs(d)
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		h, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		base = filepath.Join(h, ".local", "state")
	}
	return filepath.Join(base, "envctl"), nil
}
func Open(dir string) (*Store, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "runs.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY,value TEXT NOT NULL);
 INSERT OR IGNORE INTO metadata(key,value) VALUES ('schema','1');
 CREATE TABLE IF NOT EXISTS runs (id TEXT PRIMARY KEY,version INTEGER NOT NULL,priority INTEGER NOT NULL,created TEXT NOT NULL,body BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS operations (id TEXT PRIMARY KEY,request_digest TEXT NOT NULL,response BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS events (sequence INTEGER PRIMARY KEY AUTOINCREMENT,run_id TEXT NOT NULL REFERENCES runs(id),revision TEXT NOT NULL,version INTEGER NOT NULL,type TEXT NOT NULL,at TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS events_run ON events(run_id,sequence);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	var schema string
	err = db.QueryRow("SELECT value FROM metadata WHERE key='schema'").Scan(&schema)
	if err != nil || schema != "1" {
		db.Close()
		return nil, fmt.Errorf("unsupported state schema %q", schema)
	}
	return &Store{db: db, Dir: dir}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func decode(body []byte) (*workflow.Run, error) {
	var r workflow.Run
	err := json.Unmarshal(body, &r)
	return &r, err
}
func (s *Store) Get(ctx context.Context, id string) (*workflow.Run, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, "SELECT body FROM runs WHERE id=?", id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decode(b)
}
func (s *Store) List(ctx context.Context) ([]workflow.Run, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT body FROM runs ORDER BY priority DESC,created DESC,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []workflow.Run{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		r, err := decode(b)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}
func receipt(ctx context.Context, tx *sql.Tx, op, hash string) (*workflow.Run, error) {
	var h string
	var b []byte
	err := tx.QueryRowContext(ctx, "SELECT request_digest,response FROM operations WHERE id=?", op).Scan(&h, &b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if hash != h {
		return nil, ErrOperationReuse
	}
	return decode(b)
}

// Replay returns a committed command response before resolving mutable input
// files. The final Create/Mutate still checks the same receipt transactionally.
func (s *Store) Replay(ctx context.Context, op, id string, expected int64, kind string, request any) (*workflow.Run, error) {
	if op == "" {
		return nil, errors.New("operation ID required")
	}
	var hash string
	if kind == "create" {
		hash = workflow.Digest(struct {
			Kind    string
			Request any
		}{kind, request})
	} else {
		hash = workflow.Digest(struct {
			ID       string
			Expected int64
			Kind     string
			Request  any
		}{id, expected, kind, request})
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return receipt(ctx, tx, op, hash)
}
func record(ctx context.Context, tx *sql.Tx, r *workflow.Run, op, hash, kind string, body []byte, scope ...string) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO operations(id,request_digest,response) VALUES(?,?,?)", op, hash, body); err != nil {
		return err
	}
	revision := r.CurrentRevision
	if len(scope) > 0 && scope[0] != "" {
		revision = scope[0]
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO events(run_id,revision,version,type,at) VALUES(?,?,?,?,?)", r.ID, revision, r.Version, kind, r.UpdatedAt.Format(time.RFC3339Nano))
	return err
}
func (s *Store) Create(ctx context.Context, op string, request any, r *workflow.Run) (*workflow.Run, error) {
	if op == "" {
		return nil, errors.New("operation ID required")
	}
	hash := workflow.Digest(struct {
		Kind    string
		Request any
	}{"create", request})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if prior, e := receipt(ctx, tx, op, hash); e != nil || prior != nil {
		return prior, e
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO runs(id,version,priority,created,body) VALUES(?,?,?,?,?)", r.ID, r.Version, r.Priority, r.CreatedAt.Format(time.RFC3339Nano), b)
	if err != nil {
		return nil, err
	}
	if err = record(ctx, tx, r, op, hash, "run.created", b); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return workflow.Clone(r), nil
}

// Mutate executes fn only once for an operation and rejects stale clients.
// fn must be pure state work: external effects use separately journaled receipts.
func (s *Store) Mutate(ctx context.Context, id string, expected int64, op, kind string, request any, fn func(*workflow.Run) error) (*workflow.Run, error) {
	unlock, err := s.guard(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.mutate(ctx, id, expected, op, kind, request, fn)
}

// MutateRevision attributes engine events to the revision actually changed,
// including an older worker draining after rewind. Scope participates in the
// receipt identity without changing receipts for existing unscoped commands.
func (s *Store) MutateRevision(ctx context.Context, id, revision string, expected int64, op, kind string, request any, fn func(*workflow.Run) error) (*workflow.Run, error) {
	if revision == "" {
		return nil, errors.New("event revision is required")
	}
	unlock, err := s.guard(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	scoped := struct {
		Revision string
		Request  any
	}{revision, request}
	return s.mutate(ctx, id, expected, op, kind, scoped, func(r *workflow.Run) error {
		if r.Revision(revision) == nil {
			return errors.New("event revision does not exist")
		}
		return fn(r)
	}, revision)
}

// GuardedEffect serializes publication with rewind/cancel inside the single
// coordinator. The broker must also reconcile its own external operation ID:
// a crash after remote success but before this transaction cannot undo a PR.
// fn may perform external work and update its supplied snapshot, but must not
// call Mutate recursively. No SQLite transaction is held during network I/O.
func (s *Store) GuardedEffect(ctx context.Context, id, revision, op, kind string, fn func(*workflow.Run) error) (*workflow.Run, error) {
	unlock, err := s.guard(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	run, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if run.CurrentRevision != revision || run.Current().State != "active" {
		return nil, workflow.ErrConflict
	}
	if err = fn(run); err != nil {
		return nil, err
	}
	return s.mutate(ctx, id, run.Version, op, kind, revision, func(current *workflow.Run) error { *current = *workflow.Clone(run); return nil })
}

func (s *Store) guard(ctx context.Context, id string) (func(), error) {
	value, _ := s.guards.LoadOrStore(id, make(chan struct{}, 1))
	ch := value.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Store) mutate(ctx context.Context, id string, expected int64, op, kind string, request any, fn func(*workflow.Run) error, scope ...string) (*workflow.Run, error) {
	if op == "" {
		return nil, errors.New("operation ID required")
	}
	hash := workflow.Digest(struct {
		ID       string
		Expected int64
		Kind     string
		Request  any
	}{id, expected, kind, request})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if prior, e := receipt(ctx, tx, op, hash); e != nil || prior != nil {
		return prior, e
	}
	var b []byte
	if err = tx.QueryRowContext(ctx, "SELECT body FROM runs WHERE id=?", id).Scan(&b); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	r, err := decode(b)
	if err != nil {
		return nil, err
	}
	if r.Version != expected {
		return nil, workflow.ErrConflict
	}
	if err = fn(r); err != nil {
		return nil, err
	}
	r.Version++
	r.UpdatedAt = time.Now().UTC()
	b, err = json.Marshal(r)
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, "UPDATE runs SET version=?,priority=?,body=? WHERE id=? AND version=?", r.Version, r.Priority, b, id, expected)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return nil, workflow.ErrConflict
	}
	if err = record(ctx, tx, r, op, hash, kind, b, scope...); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return r, nil
}
func (s *Store) Events(ctx context.Context, run string, after int64, limit int) ([]Event, error) {
	if limit < 1 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,run_id,revision,version,type,at FROM events WHERE sequence>? AND (?='' OR run_id=?) ORDER BY sequence LIMIT ?`, after, run, run, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var at string
		if err = rows.Scan(&e.Sequence, &e.RunID, &e.Revision, &e.Version, &e.Type, &at); err != nil {
			return nil, err
		}
		e.At, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) PutArtifact(name, mediaType string, data []byte) (workflow.Artifact, error) {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	dir := filepath.Join(s.Dir, "artifacts", digest[:2])
	if err := os.MkdirAll(dir, 0700); err != nil {
		return workflow.Artifact{}, err
	}
	path := filepath.Join(dir, digest)
	f, err := os.CreateTemp(dir, ".incoming-")
	if err != nil {
		return workflow.Artifact{}, err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return workflow.Artifact{}, err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return workflow.Artifact{}, err
	}
	if err = f.Close(); err != nil {
		return workflow.Artifact{}, err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return workflow.Artifact{}, err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return workflow.Artifact{}, err
	}
	if err = directory.Sync(); err != nil {
		directory.Close()
		return workflow.Artifact{}, err
	}
	if err = directory.Close(); err != nil {
		return workflow.Artifact{}, err
	}
	return workflow.Artifact{Name: name, Digest: digest, Size: int64(len(data)), MediaType: mediaType}, nil
}
func (s *Store) Artifact(digest string) ([]byte, error) {
	if !digestPattern.MatchString(digest) {
		return nil, errors.New("invalid artifact digest")
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, "artifacts", digest[:2], digest))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != digest {
		return nil, errors.New("artifact checksum mismatch")
	}
	return b, nil
}
