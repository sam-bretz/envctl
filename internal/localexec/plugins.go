package localexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/plugin"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func invocationPlugins(a engine.Assignment) (plugin.Lock, error) {
	var bindings []plugin.Binding
	for _, ref := range a.Revision.Config.Plugins {
		if ref.Digest == "" {
			return plugin.Lock{}, errors.New("invocation plugin must be frozen before runtime preparation")
		}
		b, err := plugin.Load(a.Revision.Config.Dir, ref)
		if err != nil {
			return plugin.Lock{}, err
		}
		if !slices.Equal(ref.Requires, b.Descriptor.Requires) {
			return plugin.Lock{}, errors.New("plugin requirements differ from the frozen descriptor")
		}
		bindings = append(bindings, b)
	}
	return plugin.Resolve(bindings, plugin.Builtins())
}

type pluginOperation struct {
	Identity    string            `json:"identity"`
	ID          string            `json:"id"`
	Response    *plugin.Response  `json:"response,omitempty"`
	Evidence    workflow.Artifact `json:"evidence"`
	CompletedAt time.Time         `json:"completed_at,omitempty"`
	Generation  int               `json:"generation,omitempty"`
	Failures    []pluginFailure   `json:"failures,omitempty"`
	RetryAt     time.Time         `json:"retry_at,omitzero"`
}

type pluginFailure struct {
	Generation int               `json:"generation"`
	At         time.Time         `json:"at"`
	Detail     string            `json:"detail"`
	Evidence   workflow.Artifact `json:"evidence"`
}

func (b *Backend) pluginPath(a engine.Assignment, binding plugin.Binding, op string) (string, error) {
	dir, err := b.dir(a)
	if err != nil {
		return "", err
	}
	if !idPattern.MatchString(binding.Ref.ID) || !slices.Contains(binding.Descriptor.Operations, op) {
		return "", errors.New("invalid plugin operation")
	}
	return filepath.Join(dir, "plugins", binding.Ref.ID, op+".json"), nil
}

func (b *Backend) readPluginOperation(a engine.Assignment, binding plugin.Binding, op string) (pluginOperation, error) {
	filename, err := b.pluginPath(a, binding, op)
	if err != nil {
		return pluginOperation{}, err
	}
	raw, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		return pluginOperation{}, nil
	}
	if err != nil {
		return pluginOperation{}, err
	}
	var record pluginOperation
	if json.Unmarshal(raw, &record) != nil {
		return record, errors.New("invalid plugin operation receipt")
	}
	return record, nil
}

// A durable operation ID is written before the guest submission. Transport
// failures keep that ID, so reconnecting observes the same guest journal.
func (b *Backend) callPlugin(ctx context.Context, a engine.Assignment, binding plugin.Binding, op string, reuse bool) (pluginOperation, error) {
	return b.callPluginInput(ctx, a, binding, op, op, nil, reuse)
}

func (b *Backend) callPluginInput(ctx context.Context, a engine.Assignment, binding plugin.Binding, op, key string, input map[string]any, reuse bool, timeouts ...int) (pluginOperation, error) {
	timeout := 0
	if len(timeouts) > 0 {
		timeout = timeouts[0]
	}
	filename, err := b.pluginPath(a, binding, op)
	if err != nil {
		return pluginOperation{}, err
	}
	if !idPattern.MatchString(key) {
		return pluginOperation{}, errors.New("invalid plugin operation key")
	}
	filename = filepath.Join(filepath.Dir(filename), key+".json")
	var record pluginOperation
	if raw, e := os.ReadFile(filename); e == nil {
		if json.Unmarshal(raw, &record) != nil {
			return record, errors.New("invalid plugin operation receipt")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return record, e
	}
	identity := workflow.Digest(struct {
		Runtime, Revision string
		Binding           plugin.Binding
		Operation         string
	}{a.Revision.Runtime.ID, a.Revision.ID, binding, op})
	// Preserve lifecycle receipt identities from earlier protocol versions.
	if input != nil || key != op {
		identity = workflow.Digest(struct {
			Base, Key string
			Input     map[string]any
			Timeout   int
		}{identity, key, input, timeout})
	}
	if record.ID != "" && record.Identity != identity {
		return record, errors.New("plugin operation binding changed")
	}
	if record.Response != nil && reuse {
		return record, nil
	}
	limit := a.Revision.Config.Limits.MaxAttempts
	if limit <= 0 {
		limit = 3
	}
	if len(record.Failures) >= limit {
		return record, errors.New("plugin operation exhausted its configured recovery attempts; amend Plan to continue")
	}
	if time.Now().Before(record.RetryAt) {
		return record, errors.New("plugin operation is waiting for its recovery backoff")
	}
	var initialCredentials map[string]string
	fresh := record.ID == "" || record.Response != nil
	if fresh {
		initialCredentials, err = plugin.Credentials(a.Revision.Config.Dir, binding)
		if err != nil {
			return record, err
		}
	}
	if record.ID == "" || record.Response != nil {
		if record.ID != "" {
			if err := atomicJSON(filepath.Join(filepath.Dir(filename), "history", record.ID+".json"), record); err != nil {
				return record, err
			}
		}
		record = pluginOperation{ID: workflow.ID("operation"), Identity: identity}
		if err = atomicJSON(filename, record); err != nil {
			return record, err
		}
	}
	guest := plugin.Guest{Client: b.guest(a), RunID: a.Run.ID, Revision: a.Revision.ID, Generation: record.Generation,
		ResolveCredentials: func() (map[string]string, error) {
			if fresh {
				return initialCredentials, nil
			}
			return plugin.Credentials(a.Revision.Config.Dir, binding)
		}}
	runtimeID := ""
	if a.Child != "" {
		runtimeID = a.Revision.Runtime.ID
	}
	response, evidence, err := plugin.Call(ctx, guest.Execute, binding, plugin.Request{Protocol: plugin.ProtocolVersion, Operation: op, OperationID: record.ID, RunID: a.Run.ID, Revision: a.Revision.ID, RuntimeID: runtimeID, Config: binding.Ref.Config, Input: input, TimeoutSeconds: timeout})
	if err != nil {
		var terminal *plugin.TerminalFailure
		if errors.As(err, &terminal) {
			failure := pluginFailure{Generation: record.Generation, At: time.Now().UTC(), Detail: terminal.Detail}
			failure.Evidence, err = b.Store.PutArtifact("plugin."+binding.Ref.ID+".failure", "application/json", mustJSON(map[string]any{"operation": record.ID, "generation": record.Generation, "detail": terminal.Detail, "terminal": true, "at": failure.At}))
			if err != nil {
				return record, err
			}
			record.Failures = append(record.Failures, failure)
			record.Generation++
			record.RetryAt = failure.At.Add(time.Second * time.Duration(1<<min(record.Generation, 6)))
			if err = atomicJSON(filename, record); err != nil {
				return record, err
			}
			return record, terminal
		}
		return record, err
	}
	record.Evidence, err = b.Store.PutArtifact("plugin."+binding.Ref.ID+"."+op, "application/json", evidence)
	if err != nil {
		return record, err
	}
	record.Response, record.CompletedAt = &response, time.Now().UTC()
	return record, atomicJSON(filename, record)
}

func (b *Backend) pluginCheck(ctx context.Context, a engine.Assignment, check workflow.Check, index int) (pluginOperation, error) {
	lock, err := invocationPlugins(a)
	if err != nil {
		return pluginOperation{}, err
	}
	for _, binding := range lock.Bindings {
		if binding.Ref.ID == check.Plugin {
			key := "execute_" + workflow.Digest(struct {
				Attempt string
				Index   int
			}{a.Attempt.ID, index})[:24]
			return b.callPluginInput(ctx, a, binding, "execute", key, check.Input, true, check.TimeoutSeconds)
		}
	}
	return pluginOperation{}, errors.New("check plugin is not attached to the invocation")
}

func pluginChecksReady(a engine.Assignment) (bool, string) {
	lock, err := invocationPlugins(a)
	if err != nil {
		return false, "invocation plugin lock needs recovery"
	}
	for _, node := range a.Revision.Config.Workflow.Nodes {
		if node.Kind == "qa" && len(node.Checks) == 0 {
			return false, "Plan must declare executable checks for every QA stage"
		}
		for _, check := range node.Checks {
			if check.Plugin != "" {
				found := false
				for _, binding := range lock.Bindings {
					if binding.Ref.ID == check.Plugin && slices.Contains(binding.Descriptor.Operations, "execute") {
						found = true
					}
				}
				if !found {
					return false, "Plan must attach an executable check plugin: " + check.Plugin
				}
			}
		}
	}
	return true, "acceptance checks have executable commands or invocation plugin bindings"
}

func currentPluginOperation(record pluginOperation, now time.Time) bool {
	if record.Response == nil {
		return false
	}
	ttl := record.Response.ExpiresSeconds
	if ttl <= 0 {
		ttl = 60
	}
	return now.Before(record.CompletedAt.Add(time.Duration(ttl) * time.Second))
}

func (b *Backend) pluginProbes(ctx context.Context, a engine.Assignment, probes []workflow.Probe) ([]workflow.Probe, error) {
	lock, err := invocationPlugins(a)
	if err != nil {
		return nil, err
	}
	passed := map[string]bool{}
	for _, probe := range probes {
		passed[probe.Capability] = probe.Passed && time.Now().Before(probe.ExpiresAt)
	}
	for _, binding := range lock.Bindings {
		err = nil
		now := time.Now().UTC()
		detail := "plugin dependency readiness is unresolved"
		ready := true
		for _, required := range binding.Descriptor.Requires {
			if !passed[required] {
				ready = false
			}
		}
		var record pluginOperation
		probed := false
		repair, repairFile, e := b.readPluginRepair(a, binding)
		if e != nil {
			return nil, e
		}
		if ready && repair.Trigger != "" && !repair.Complete {
			probed = true
			record, err = b.continuePluginRepair(ctx, a, binding, repair, repairFile)
			ready = err == nil && record.Response != nil && record.Response.OK
			detail = "plugin resource repair requires fresh verification"
			if err == nil && record.Response != nil {
				detail = record.Response.Detail
			}
		} else {
			if ready && slices.Contains(binding.Descriptor.Operations, "prepare") {
				prior, e := b.readPluginOperation(a, binding, "prepare")
				if e != nil {
					return nil, e
				}
				reuse := prior.Response != nil && (prior.Response.OK || currentPluginOperation(prior, now))
				record, err = b.callPlugin(ctx, a, binding, "prepare", reuse)
				ready = err == nil && record.Response != nil && record.Response.OK
				detail = "plugin preparation needs attention"
				if err == nil && record.Response != nil {
					detail = record.Response.Detail
				}
				if ready && record.Response.ExpiresSeconds > 0 && !currentPluginOperation(record, now) {
					if slices.Contains(binding.Descriptor.Operations, "renew") {
						last, e := b.readPluginOperation(a, binding, "renew")
						if e != nil {
							return nil, e
						}
						record, err = b.callPlugin(ctx, a, binding, "renew", currentPluginOperation(last, now))
						ready = err == nil && record.Response != nil && record.Response.OK
						detail = "plugin renewal needs attention"
					} else {
						ready = false
						detail = "plugin preparation expired; no renewal operation is available"
					}
				}
			}
			if ready {
				prior, e := b.readPluginOperation(a, binding, "probe")
				if e != nil {
					return nil, e
				}
				probed = true
				record, err = b.callPlugin(ctx, a, binding, "probe", currentPluginOperation(prior, now))
				ready = err == nil && record.Response != nil && record.Response.OK
				detail = "plugin probe needs attention; check its connection and guest operation"
				if err == nil && record.Response != nil {
					detail = record.Response.Detail
				}
			}
		}
		if probed && err == nil && record.Response != nil {
			if e := b.requestPluginRepair(a, binding, record); e != nil {
				err, ready = e, false
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			detail += ": " + err.Error()
		}
		// Failed transport/protocol attempts never become passing readiness prose.
		for _, capability := range binding.Ref.Provides {
			proof := workflow.Probe{Capability: capability, Binding: binding.Ref.ID + "@" + binding.Digest, ConfigDigest: workflow.Digest(a.Revision.Config), RuntimeID: a.Revision.Runtime.ID, Passed: ready, Detail: detail, CheckedAt: now, ExpiresAt: now.Add(30 * time.Second)}
			if ready {
				proof = plugin.ProbeResult(binding, *record.Response, record.Evidence, a.Revision.Config, a.Revision.Runtime.ID, capability, record.CompletedAt)
			} else {
				evidence, e := b.Store.PutArtifact("plugin."+binding.Ref.ID+".readiness", "application/json", mustJSON(map[string]any{"capability": capability, "detail": detail, "passed": false, "checked_at": now}))
				if e != nil {
					return nil, e
				}
				proof.EvidenceDigest = evidence.Digest
			}
			for i := range probes {
				if probes[i].Capability == capability {
					probes[i] = proof
				}
			}
			passed[capability] = ready
		}
	}
	return probes, nil
}

func (b *Backend) cleanupPlugins(ctx context.Context, a engine.Assignment) error {
	lock, err := invocationPlugins(a)
	if err != nil {
		return err
	}
	for i := len(lock.Bindings) - 1; i >= 0; i-- {
		binding := lock.Bindings[i]
		if !slices.Contains(binding.Descriptor.Operations, "cleanup") {
			continue
		}
		// Never install a plugin just to clean up a runtime that never prepared it.
		if slices.Contains(binding.Descriptor.Operations, "prepare") {
			prior, err := b.readPluginOperation(a, binding, "prepare")
			if err != nil {
				return err
			}
			if prior.ID == "" {
				continue
			}
		}
		record, err := b.callPlugin(ctx, a, binding, "cleanup", true)
		if err != nil {
			return err
		}
		if record.Response == nil || !record.Response.OK {
			return errors.New("invocation plugin cleanup needs attention")
		}
	}
	return nil
}
