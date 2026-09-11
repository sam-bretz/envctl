package workflow

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDatasetContractRequiresCompleteReviewedRestorationEvidence(t *testing.T) {
	r := live(t)
	v := r.Current()
	d := Dataset{ID: "billing", Adapter: "postgres", Service: "db", Database: "billing", User: "postgres", SeedRepository: "app", SeedFile: "seed.sql", VerifySQL: "SELECT true", VerifyEquals: "t"}
	v.Config.Data = DataConfig{Datasets: []Dataset{d}}
	result := output(v, "code")
	if err := v.ValidateResult("code", result, time.Now()); err == nil {
		t.Fatal("source-only checkpoint admitted with required data")
	}
	result.Datasets = map[string]DatasetSnapshot{"billing": {Adapter: "postgres", BindingDigest: Digest(d), Format: "postgres-custom-v1", ToolVersion: "17.6", Source: Artifact{Digest: Digest("data"), Size: 30, MediaType: "application/vnd.envctl.postgres-dump"}, Evidence: Artifact{Digest: Digest("verification"), Size: 10, MediaType: "application/json"}}}
	result.DatasetDigest = Digest(result.Datasets)
	review(&result)
	if err := v.ValidateResult("code", result, time.Now()); err != nil {
		t.Fatal(err)
	}
	qa := output(v, "qa")
	qa.Datasets = Clone(result.Datasets)
	qa.DatasetDigest = result.DatasetDigest
	review(&qa)
	v.Checkpoints["qa"] = Checkpoint{ID: "cp_qa", Result: qa}
	change := output(v, "approved-change")
	change.Datasets = Clone(qa.Datasets)
	change.DatasetDigest = qa.DatasetDigest
	review(&change)
	if err := v.ValidateResult("approved-change", change, time.Now()); err != nil {
		t.Fatal("matching QA dataset rejected", err)
	}
	other := change.Datasets["billing"]
	other.Source.Digest = Digest("unreviewed application state")
	change.Datasets["billing"] = other
	change.DatasetDigest = Digest(change.Datasets)
	review(&change)
	if err := v.ValidateResult("approved-change", change, time.Now()); err == nil {
		t.Fatal("approved change substituted data that QA did not accept")
	}
	changed := result.Datasets["billing"]
	changed.Source.Digest = Digest("other data")
	result.Datasets["billing"] = changed
	if err := v.ValidateResult("code", result, time.Now()); err == nil {
		t.Fatal("changed data reused old review")
	}
	review(&result)
	if err := v.ValidateResult("code", result, time.Now()); err == nil {
		t.Fatal("manifest digest mismatch accepted")
	}
	result.DatasetDigest = Digest(result.Datasets)
	review(&result)
	d.Database = "different"
	v.Config.Data.Datasets[0] = d
	if err := v.ValidateResult("code", result, time.Now()); err == nil {
		t.Fatal("snapshot rebound to different database")
	}
}

func TestAbsentDataConfigPreservesExistingInvocationDigests(t *testing.T) {
	c := fixture(t)
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"data":`) {
		t.Fatal("empty data field changes existing receipt identity")
	}
}

func TestDatasetConfigRejectsUnsafeTargetsAndUncoordinatedGroups(t *testing.T) {
	d := Dataset{ID: "billing", Adapter: "postgres", Service: "db", Database: "billing", User: "postgres", SeedRepository: "app", SeedFile: "seed.sql", VerifySQL: "SELECT true", VerifyEquals: "t"}
	for _, database := range []string{"postgres", "template1", "host=external dbname=x", "--help"} {
		bad := d
		bad.Database = database
		if err := bad.Validate(); err == nil {
			t.Fatal("unsafe database target accepted")
		}
	}
	bad := d
	bad.SeedFile = "../outside"
	if err := bad.Validate(); err == nil {
		t.Fatal("seed escape accepted")
	}
	second := d
	second.ID = "other"
	if err := (DataConfig{Datasets: []Dataset{d, second}}).Validate([]Repository{{ID: "app"}}); err == nil {
		t.Fatal("cross-service consistency claimed without writer procedure")
	}
}

func TestHTTPFixtureCheckpointContract(t *testing.T) {
	r := live(t)
	v := r.Current()
	d := Dataset{ID: "emulator", Adapter: "http-fixture", Service: "emulator", SeedRepository: "app", SeedFile: "fixtures/seed.json", VerifyKey: "tax-rate", VerifyEquals: `{"rate":0.2}`}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	v.Config.Data = DataConfig{Datasets: []Dataset{d}}
	result := output(v, "code")
	result.Datasets = map[string]DatasetSnapshot{d.ID: {Adapter: d.Adapter, BindingDigest: Digest(d), Format: d.SnapshotFormat(), ToolVersion: "envctl-http-fixture/1.0.0", Source: Artifact{Digest: Digest("records"), Size: 40, MediaType: d.SnapshotMedia()}, Evidence: Artifact{Digest: Digest("verified"), Size: 20, MediaType: "application/json"}}}
	result.DatasetDigest = Digest(result.Datasets)
	review(&result)
	if err := v.ValidateResult("code", result, time.Now()); err != nil {
		t.Fatal(err)
	}
	snapshot := result.Datasets[d.ID]
	snapshot.Format = "postgres-custom-v1"
	result.Datasets[d.ID] = snapshot
	result.DatasetDigest = Digest(result.Datasets)
	review(&result)
	if err := v.ValidateResult("code", result, time.Now()); err == nil {
		t.Fatal("foreign adapter snapshot admitted")
	}
	for _, mutate := range []func(*Dataset){
		func(d *Dataset) { d.VerifyEquals = "not JSON" },
		func(d *Dataset) { d.VerifyKey = "../outside" },
		func(d *Dataset) { d.Database = "billing" },
		func(d *Dataset) { d.User = "postgres" },
		func(d *Dataset) { d.VerifySQL = "SELECT true" },
	} {
		bad := d
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatal("invalid fixture verification accepted")
		}
	}
}
