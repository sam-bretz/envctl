package localexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/sam-bretz/envctl/internal/checkpoint"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/gueststack"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type dataBaseline struct {
	Binding   string                              `json:"binding"`
	Seeds     map[string]workflow.Artifact        `json:"seeds"`
	Snapshots map[string]workflow.DatasetSnapshot `json:"snapshots"`
}

func (b *Backend) dataFile(a engine.Assignment, name string) (string, error) {
	dir, err := b.dir(a)
	return filepath.Join(dir, name+".json"), err
}
func (b *Backend) dataBusy(a engine.Assignment) bool {
	file, err := b.dataFile(a, "data-operation")
	if err != nil {
		return true
	}
	_, err = os.Stat(file)
	return !os.IsNotExist(err)
}

func dataOperationBinding(a engine.Assignment, p gueststack.Prepared, operation string) string {
	return workflow.Digest(struct {
		Operation string
		Stack     gueststack.Prepared
		Data      workflow.DataConfig
	}{operation, p, a.Revision.Config.Data})
}
func (b *Backend) ownsDataOperation(a engine.Assignment, p gueststack.Prepared, operation string) bool {
	file, err := b.dataFile(a, "data-operation")
	if err != nil {
		return false
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	var value string
	return json.Unmarshal(raw, &value) == nil && value == dataOperationBinding(a, p, operation)
}

// The marker stays durable while writers are stopped. Readiness and dispatch
// must not auto-start them during an interrupted multi-dataset operation.
func (b *Backend) beginData(ctx context.Context, a engine.Assignment, p gueststack.Prepared, operation string) error {
	for _, service := range a.Revision.Config.Data.Quiesce {
		if !slices.Contains(p.Services, service) {
			return errors.New("quiescence references an unavailable writer")
		}
	}
	for _, dataset := range a.Revision.Config.Data.Datasets {
		if slices.Contains(a.Revision.Config.Data.Quiesce, dataset.Service) {
			return errors.New("quiescence must stop writers, not the dataset service")
		}
	}
	file, err := b.dataFile(a, "data-operation")
	if err != nil {
		return err
	}
	binding := dataOperationBinding(a, p, operation)
	if raw, e := os.ReadFile(file); e == nil {
		var previous string
		if json.Unmarshal(raw, &previous) != nil || previous != binding {
			return errors.New("another dataset operation owns writer quiescence")
		}
	} else if !os.IsNotExist(e) {
		return e
	} else if err = atomicJSON(file, binding); err != nil {
		return err
	}
	return b.stack(a).SetWritersRunning(ctx, p, a.Revision.Config.Data.Quiesce, false)
}
func (b *Backend) endData(ctx context.Context, a engine.Assignment, p gueststack.Prepared) error {
	if err := b.stack(a).SetWritersRunning(ctx, p, a.Revision.Config.Data.Quiesce, true); err != nil {
		return err
	}
	file, err := b.dataFile(a, "data-operation")
	if err != nil {
		return err
	}
	if err = os.Remove(file); err != nil && !os.IsNotExist(err) {
		return err
	}
	dir, err := os.Open(filepath.Dir(file))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (b *Backend) dataAdapter(a engine.Assignment, p gueststack.Prepared) checkpoint.Client {
	return checkpoint.Client{Guest: b.guest(a), Stack: b.stack(a), Prepared: p, Store: b.Store, Revision: a.Revision.ID}
}

func (b *Backend) baselineData(ctx context.Context, a engine.Assignment, p gueststack.Prepared) (dataBaseline, error) {
	file, err := b.dataFile(a, "data-baseline")
	if err != nil {
		return dataBaseline{}, err
	}
	identity := workflow.Digest(struct {
		Revision, Runtime string
		Data              workflow.DataConfig
		Pins              map[string]string
	}{a.Revision.ID, a.Revision.Runtime.ID, a.Revision.Config.Data, a.Revision.SourcePins})
	var childInputs map[string]workflow.DatasetSnapshot
	if a.Child != "" {
		identity = workflow.Digest(struct {
			Base   string
			Inputs map[string]string
		}{identity, a.Attempt.Inputs})
		childInputs, err = inputDatasets(a)
		if err != nil {
			return dataBaseline{}, err
		}
	}
	baseline := dataBaseline{Binding: identity, Seeds: map[string]workflow.Artifact{}, Snapshots: map[string]workflow.DatasetSnapshot{}}
	if raw, e := os.ReadFile(file); e == nil {
		if json.Unmarshal(raw, &baseline) != nil || baseline.Binding != identity {
			return baseline, errors.New("dataset baseline identity changed")
		}
		if b.ownsDataOperation(a, p, "baseline") {
			return baseline, b.endData(ctx, a, p)
		}
		return baseline, nil
	} else if !os.IsNotExist(e) {
		return baseline, e
	}
	if err = b.beginData(ctx, a, p, "baseline"); err != nil {
		return baseline, err
	}
	for _, spec := range a.Revision.Config.Data.Datasets {
		var inherited *workflow.DatasetSnapshot
		if snapshot, ok := childInputs[spec.ID]; ok {
			if snapshot.BindingDigest != workflow.Digest(spec) {
				return baseline, errors.New("child dataset input does not match its configured binding")
			}
			inherited = &snapshot
		}
		for _, cp := range a.Revision.Checkpoints {
			if a.Child != "" {
				break
			}
			if cp.ID == a.Revision.FromCheckpoint {
				if snapshot, ok := cp.Result.Datasets[spec.ID]; ok && snapshot.BindingDigest == workflow.Digest(spec) {
					inherited = &snapshot
				}
			}
		}
		if inherited != nil {
			if err = b.dataAdapter(a, p).Restore(ctx, "baseline_restore_"+spec.ID, spec, *inherited); err != nil {
				return baseline, err
			}
			baseline.Snapshots[spec.ID] = *inherited
			continue
		}
		source, err := b.repos(a).SourceDir(spec.SeedRepository)
		if err != nil {
			return baseline, err
		}
		pin := a.Revision.SourcePins[spec.SeedRepository]
		if len(pin) != 40 && len(pin) != 64 {
			return baseline, errors.New("dataset seed repository has no immutable pin")
		}
		var seed seedBytes
		if err = b.Provider.Exec(ctx, a.Revision.Runtime.ID, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "git", "-C", source, "show", pin + ":" + spec.SeedFile}, Stdout: &seed, Stderr: io.Discard}); err != nil {
			return baseline, errors.New("dataset seed is unavailable at the pinned repository commit")
		}
		artifact, err := b.Store.PutArtifact("dataset."+spec.ID+".seed", spec.SeedMedia(), seed.Bytes())
		if err != nil {
			return baseline, err
		}
		baseline.Seeds[spec.ID] = artifact
		snapshot, err := b.dataAdapter(a, p).Seed(ctx, "baseline_seed_"+spec.ID, spec, artifact)
		if err != nil {
			return baseline, err
		}
		// Exercise the restore path during Plan rather than discover missing
		// tools/permissions only when the user first rewinds.
		if err = b.dataAdapter(a, p).Restore(ctx, "baseline_verify_"+spec.ID, spec, snapshot); err != nil {
			return baseline, err
		}
		baseline.Snapshots[spec.ID] = snapshot
	}
	if err = atomicJSON(file, baseline); err != nil {
		return baseline, err
	}
	return baseline, b.endData(ctx, a, p)
}

type seedBytes struct{ bytes.Buffer }

func (s *seedBytes) Write(raw []byte) (int, error) {
	if s.Len()+len(raw) > 1<<20 {
		return 0, errors.New("SQL seed exceeds 1 MiB")
	}
	return s.Buffer.Write(raw)
}

func (b *Backend) datasetReadiness(ctx context.Context, a engine.Assignment) (bool, string, error) {
	if len(a.Revision.Config.Data.Datasets) == 0 {
		return false, "Plan must configure a dataset adapter, seed and verification", nil
	}
	p, err := b.currentStack(a)
	if err != nil {
		return false, "dataset readiness requires a prepared Compose stack", nil
	}
	file, _ := b.dataFile(a, "data-baseline")
	if _, err = os.Stat(file); err == nil && b.dataBusy(a) && !b.ownsDataOperation(a, p, "baseline") {
		return false, "dataset operation is preserving stopped writers until recovery completes", nil
	}
	if _, err = b.baselineData(ctx, a, p); err != nil {
		return false, "dataset seed or restore verification needs recovery", err
	}
	for _, spec := range a.Revision.Config.Data.Datasets {
		op := "probe_" + workflow.Digest(time.Now().Unix() / 30)[:16] + "_" + spec.ID
		if err = b.dataAdapter(a, p).Probe(ctx, op, spec); err != nil {
			return false, "dataset application verification needs recovery", err
		}
	}
	return true, "dataset seeds, retained dumps, restore commands and application checks verified in the guest", nil
}

func inputDatasets(a engine.Assignment) (map[string]workflow.DatasetSnapshot, error) {
	result := map[string]workflow.DatasetSnapshot{}
	n := a.Revision.Config.Workflow.Nodes[a.Attempt.Node]
	for node, cpID := range a.Attempt.Inputs {
		cp, ok := a.Revision.Checkpoints[node]
		if !ok || cp.ID != cpID || cp.HistoricalOnly {
			return nil, errors.New("dataset input checkpoint lineage changed")
		}
		for key, snapshot := range cp.Result.Datasets {
			if n.Join != nil && n.Join.Datasets[key] != "" && n.Join.Datasets[key] != node {
				continue
			}
			if prior, ok := result[key]; ok && workflow.Digest(prior) != workflow.Digest(snapshot) {
				return nil, errors.New("dataset join requires an explicit merge or isolated fixture policy")
			}
			result[key] = snapshot
		}
	}
	if n.Join != nil {
		for dataset, parent := range n.Join.Datasets {
			cp, ok := a.Revision.Checkpoints[parent]
			if !ok || a.Attempt.Inputs[parent] != cp.ID || cp.HistoricalOnly {
				return nil, errors.New("dataset join is missing its declared input lineage")
			}
			if _, ok := cp.Result.Datasets[dataset]; !ok {
				return nil, errors.New("dataset join input lacks its declared snapshot")
			}
		}
	}
	return result, nil
}

func (b *Backend) restoreDataInputs(ctx context.Context, a engine.Assignment, p gueststack.Prepared) error {
	if len(a.Revision.Config.Data.Datasets) == 0 {
		return nil
	}
	baseline, err := b.baselineData(ctx, a, p)
	if err != nil {
		return err
	}
	inputs, err := inputDatasets(a)
	if err != nil {
		return err
	}
	if len(inputs) == 0 {
		inputs = baseline.Snapshots
	}
	if err = b.beginData(ctx, a, p, a.Attempt.ID+"_restore"); err != nil {
		return err
	}
	for _, spec := range a.Revision.Config.Data.Datasets {
		snapshot, ok := inputs[spec.ID]
		if !ok {
			return errors.New("dataset predecessor does not cover every required dataset")
		}
		if err = b.dataAdapter(a, p).Restore(ctx, a.Attempt.ID+"_restore_"+spec.ID, spec, snapshot); err != nil {
			return err
		}
	}
	return b.endData(ctx, a, p)
}

func (b *Backend) captureData(ctx context.Context, a engine.Assignment, r *attemptRecord) error {
	if len(a.Revision.Config.Data.Datasets) == 0 || r.Result.Datasets != nil {
		return nil
	}
	if a.Revision.Config.Workflow.Nodes[a.Attempt.Node].Kind == "change" {
		// Approved output retains the immutable QA dataset, just as it retains
		// the QA commit set. Dump timestamps must not invent a new data identity.
		inputs, err := inputDatasets(a)
		if err != nil {
			return err
		}
		if len(inputs) != len(a.Revision.Config.Data.Datasets) {
			return errors.New("approved change requires the complete QA dataset")
		}
		r.Result.Datasets = inputs
		r.Result.DatasetDigest = workflow.Digest(inputs)
		return b.save(a, r)
	}
	if r.Stack == nil {
		return errors.New("dataset checkpoint requires the stage stack")
	}
	if err := b.beginData(ctx, a, *r.Stack, a.Attempt.ID+"_capture"); err != nil {
		return err
	}
	snapshots := map[string]workflow.DatasetSnapshot{}
	for _, spec := range a.Revision.Config.Data.Datasets {
		snapshot, err := b.dataAdapter(a, *r.Stack).Capture(ctx, a.Attempt.ID+"_capture_"+spec.ID, spec)
		if err != nil {
			return err
		}
		snapshots[spec.ID] = snapshot
	}
	if err := b.endData(ctx, a, *r.Stack); err != nil {
		return err
	}
	r.Result.Datasets = snapshots
	r.Result.DatasetDigest = workflow.Digest(snapshots)
	return b.save(a, r)
}
