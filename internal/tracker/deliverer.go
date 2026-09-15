package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/secrets"
	"github.com/sam-bretz/envctl/internal/workflow"
)

const maxTrackerAttempts = 10
const maxCommentBytes = 16 << 10 // 16 KiB

// Deliverer posts pending Run.TrackerLog entries in the background,
// independently of the engine: a tracker outage must never block or fail
// the workflow. Exactly-once delivery is achieved with content-addressed
// receipt files (the same freeze/load pattern internal/publication uses)
// plus Comment's own list-before-create reconciliation against the tracker
// itself, so a crash at any point never produces a duplicate remote comment.
type Deliverer struct {
	Store *runstore.Store
	// Now lets tests inject a fixed clock; defaults to time.Now.
	Now func() time.Time
	// Interval between delivery passes; defaults to 5 seconds.
	Interval time.Duration
	// OnError reports infrastructure-level errors only (e.g. Store.List
	// failing); per-entry failures are recorded on the entry itself and
	// never reported here.
	OnError func(error)
	// newTracker constructs a Tracker for a config; overridden by tests to
	// avoid any real network dependency.
	newTracker func(workflow.TrackerConfig) (Tracker, error)
}

func (d *Deliverer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}
func (d *Deliverer) tracker(cfg workflow.TrackerConfig) (Tracker, error) {
	if d.newTracker != nil {
		return d.newTracker(cfg)
	}
	return New(cfg)
}
func (d *Deliverer) report(err error) {
	if d.OnError != nil {
		d.OnError(err)
	}
}

// Run polls for pending entries until ctx is cancelled.
func (d *Deliverer) Run(ctx context.Context) error {
	interval := d.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		d.tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (d *Deliverer) tick(ctx context.Context) {
	runs, err := d.Store.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			d.report(err)
		}
		return
	}
	for _, run := range runs {
		if ctx.Err() != nil {
			return
		}
		if len(run.TrackerLog) == 0 {
			continue
		}
		d.deliverRun(ctx, run)
	}
}

// deliverRun attempts delivery of the first non-terminal entry only: later
// entries wait their turn so the cumulative "so far" recap they build is
// always drawn from a strict, fully-resolved prefix.
func (d *Deliverer) deliverRun(ctx context.Context, run workflow.Run) {
	for _, entry := range run.TrackerLog {
		if entry.Status == "posted" || entry.Status == "failed" {
			continue
		}
		if !entry.RetryAt.IsZero() && entry.RetryAt.After(d.now()) {
			return
		}
		d.deliverOne(ctx, run, entry)
		return
	}
}

type trackerReceipt struct {
	CommentID  string `json:"comment_id"`
	BodyDigest string `json:"body_digest"`
}

func (d *Deliverer) receiptPath(runID, entryID string) string {
	return filepath.Join(d.Store.Dir, "tracker", runID, entryID+".json")
}

func (d *Deliverer) deliverOne(ctx context.Context, run workflow.Run, entry workflow.TrackerLogEntry) {
	rev := run.Revision(entry.Revision)
	if rev == nil || rev.Config.Tracker == nil {
		d.recordFailure(ctx, run.ID, entry.ID, errors.New("tracker log entry has no tracker configuration"))
		return
	}
	cfg := *rev.Config.Tracker
	client, err := d.tracker(cfg)
	if err != nil {
		d.recordFailure(ctx, run.ID, entry.ID, err)
		return
	}
	body, err := Render(&run, entry)
	if err != nil {
		d.recordFailure(ctx, run.ID, entry.ID, err)
		return
	}
	body = string(secrets.Redact(rev.Config, []byte(body)))
	body = bound(body, maxCommentBytes)
	marker := "envctl:" + entry.ID
	digest := workflow.Digest(body)
	receiptPath := d.receiptPath(run.ID, entry.ID)
	var receipt trackerReceipt
	if load(receiptPath, &receipt) == nil && receipt.BodyDigest == digest {
		d.markPosted(ctx, run.ID, entry.ID, receipt.CommentID)
		return
	}
	ref, err := ParseRef(run.TaskRef)
	if err != nil {
		d.recordFailure(ctx, run.ID, entry.ID, err)
		return
	}
	commentID, err := client.Comment(ctx, ref, marker, body)
	if err != nil {
		d.recordFailure(ctx, run.ID, entry.ID, err)
		return
	}
	if err := freeze(receiptPath, trackerReceipt{CommentID: commentID, BodyDigest: digest}); err != nil {
		d.recordFailure(ctx, run.ID, entry.ID, err)
		return
	}
	d.markPosted(ctx, run.ID, entry.ID, commentID)
}

// bound caps a comment body, applied after redaction (so truncation never
// exposes half a secret) and before the marker is appended by Comment (so
// the marker used for dedup is never truncated away).
func bound(body string, limit int) string {
	if len(body) <= limit {
		return body
	}
	return body[:limit] + "\n\n…(truncated)"
}

func (d *Deliverer) mutate(ctx context.Context, runID string, fn func(*workflow.Run) error) error {
	for i := 0; i < 8; i++ {
		run, err := d.Store.Get(ctx, runID)
		if err != nil {
			return err
		}
		_, err = d.Store.Mutate(ctx, runID, run.Version, workflow.ID("tracker"), "tracker.delivery", nil, fn)
		if !errors.Is(err, workflow.ErrConflict) {
			return err
		}
	}
	return workflow.ErrConflict
}
func (d *Deliverer) markPosted(ctx context.Context, runID, entryID, commentID string) {
	if err := d.mutate(ctx, runID, func(run *workflow.Run) error {
		for i := range run.TrackerLog {
			if run.TrackerLog[i].ID == entryID && run.TrackerLog[i].Status == "pending" {
				run.TrackerLog[i].Status = "posted"
				run.TrackerLog[i].CommentID = commentID
			}
		}
		return nil
	}); err != nil && ctx.Err() == nil {
		d.report(err)
	}
}
func (d *Deliverer) recordFailure(ctx context.Context, runID, entryID string, cause error) {
	if err := d.mutate(ctx, runID, func(run *workflow.Run) error {
		for i := range run.TrackerLog {
			e := &run.TrackerLog[i]
			if e.ID != entryID || e.Status != "pending" {
				continue
			}
			e.Attempts++
			e.Error = cause.Error()
			if e.Attempts >= maxTrackerAttempts {
				e.Status = "failed"
			} else {
				e.RetryAt = d.now().Add(backoff(e.Attempts))
			}
		}
		return nil
	}); err != nil && ctx.Err() == nil {
		d.report(err)
	}
}

func backoff(failures int) time.Duration {
	return time.Second * time.Duration(1<<min(max(failures, 1), 8))
}

// freeze and load are lifted from internal/publication's identically-named,
// content-addressed atomic-create-once receipt helpers.
func freeze(filename string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(filename); err == nil {
		if !bytes.Equal(old, raw) {
			return errors.New("tracker receipt conflicts with immutable inputs")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(filename), ".receipt-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Link(f.Name(), filename); err != nil {
		if errors.Is(err, os.ErrExist) {
			return freeze(filename, value)
		}
		return err
	}
	d, err := os.Open(filepath.Dir(filename))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func load(filename string, out any) error {
	raw, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	if json.Unmarshal(raw, out) != nil {
		return errors.New("invalid tracker receipt")
	}
	return nil
}
