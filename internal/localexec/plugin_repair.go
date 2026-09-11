package localexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/plugin"
)

// A repair intent names the failed probe and the lifecycle receipt it replaces.
// Persisting both before dispatch lets recovery distinguish a new repair from
// reconnecting to one whose acknowledgement was lost.
type pluginRepair struct {
	Trigger   string    `json:"trigger"`
	Operation string    `json:"operation"`
	Previous  string    `json:"previous"`
	Count     int       `json:"count"`
	Complete  bool      `json:"complete"`
	RetryAt   time.Time `json:"retry_at,omitzero"`
}

func (b *Backend) readPluginRepair(a engine.Assignment, binding plugin.Binding) (pluginRepair, string, error) {
	filename, err := b.pluginPath(a, binding, "probe")
	if err != nil {
		return pluginRepair{}, "", err
	}
	filename = filepath.Join(filepath.Dir(filename), "repair.json")
	raw, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		return pluginRepair{}, filename, nil
	}
	var repair pluginRepair
	if err == nil {
		err = json.Unmarshal(raw, &repair)
	}
	return repair, filename, err
}

func (b *Backend) requestPluginRepair(a engine.Assignment, binding plugin.Binding, probe pluginOperation) error {
	repair, filename, err := b.readPluginRepair(a, binding)
	if err != nil {
		return err
	}
	if probe.Response == nil {
		return nil
	}
	if probe.Response.OK {
		if repair.Count == 0 {
			return nil
		}
		// A verified recovery starts a new incident budget. The operation history
		// and failed probe evidence remain retained independently of this cursor.
		repair.Complete, repair.Count = true, 0
		return atomicJSON(filename, repair)
	}
	if probe.Response.Recovery == "" || repair.Trigger == probe.ID {
		return nil
	}
	limit := a.Revision.Config.Limits.MaxAttempts
	if limit <= 0 {
		limit = 3
	}
	if repair.Count >= limit {
		return errors.New("plugin resource repair exhausted its configured attempts; amend Plan to continue")
	}
	prior, err := b.readPluginOperation(a, binding, probe.Response.Recovery)
	if err != nil {
		return err
	}
	if repair.Trigger != "" {
		if err = atomicJSON(filepath.Join(filepath.Dir(filename), "history", "repair_"+repair.Trigger+".json"), repair); err != nil {
			return err
		}
	}
	repair = pluginRepair{Trigger: probe.ID, Operation: probe.Response.Recovery, Previous: prior.ID, Count: repair.Count + 1}
	repair.RetryAt = time.Now().UTC().Add(time.Second * time.Duration(1<<min(repair.Count, 6)))
	return atomicJSON(filename, repair)
}

func (b *Backend) continuePluginRepair(ctx context.Context, a engine.Assignment, binding plugin.Binding, repair pluginRepair, filename string) (pluginOperation, error) {
	if time.Now().Before(repair.RetryAt) {
		return pluginOperation{}, errors.New("plugin resource repair is waiting for recovery backoff")
	}
	prior, err := b.readPluginOperation(a, binding, repair.Operation)
	if err != nil {
		return pluginOperation{}, err
	}
	// A different ID means the repair was submitted already. In particular,
	// transport errors must never cause another prepare/renew operation.
	record, err := b.callPlugin(ctx, a, binding, repair.Operation, prior.ID != repair.Previous)
	if err != nil {
		return record, err
	}
	if record.Response == nil || !record.Response.OK {
		return record, errors.New("plugin resource repair did not succeed; amend Plan to continue")
	}
	probe, err := b.readPluginOperation(a, binding, "probe")
	if err != nil {
		return pluginOperation{}, err
	}
	// Force fresh verification after successful repair. A new pending probe
	// retains its job ID if the coordinator loses contact during verification.
	probe, err = b.callPlugin(ctx, a, binding, "probe", probe.ID != repair.Trigger)
	if err != nil {
		return probe, err
	}
	repair.Complete = true
	if err := atomicJSON(filename, repair); err != nil {
		return probe, err
	}
	return probe, nil
}
