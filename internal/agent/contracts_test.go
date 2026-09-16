package agent

import (
	"reflect"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestWorkerCannotClaimAuthoritativeEvidence(t *testing.T) {
	node := workflow.Node{Outputs: []string{"implementation"}, OutputSchema: map[string]any{"type": "object", "required": []string{"count"}, "properties": map[string]any{"count": map[string]any{"type": "integer", "minimum": 1}}, "additionalProperties": false}}
	good := []byte(`{"summary":"Completed implementation","artifacts":{"implementation":"Description of the changes"},"data":{"count":1}}`)
	if _, err := ParseProposal(good, node); err != nil {
		t.Fatal(err)
	}
	// Output documents may arrive as files the backend reads, so the result
	// need not carry them; the backend refuses a stage with neither.
	if _, err := ParseProposal([]byte(`{"summary":"Done","data":{"count":1}}`), node); err != nil {
		t.Fatal("result without inline documents rejected:", err)
	}
	for _, raw := range []string{
		`{"summary":"Done","artifacts":{"unknown":"x"},"data":{"count":1}}`,
		`{"summary":"Done","artifacts":{"implementation":"notes"},"data":{"count":0}}`,
		`{"summary":"Done","artifacts":{"implementation":"notes"},"data":{"count":1},"commits":{"app":"claimed"}}`,
		`{"summary":"Done","artifacts":{"implementation":"notes"},"data":{"count":1},"checks":[{"passed":true}]}`,
		`{"summary":"Done","artifacts":{"implementation":"notes"},"data":{"count":1},"review":{"accepted":true}}`,
	} {
		if _, err := ParseProposal([]byte(raw), node); err == nil {
			t.Errorf("accepted invalid worker authority: %s", raw)
		}
	}
}
func TestSupervisorContractIncludesCorrection(t *testing.T) {
	r, err := ParseAssessment([]byte(`{"accepted":false,"summary":"Missing an acceptance case","correction":"Add the empty-input test"}`))
	if err != nil || r.Accepted || r.Correction == "" {
		t.Fatal(err)
	}
	if _, err = ParseAssessment([]byte(`{"accepted":true}`)); err == nil {
		t.Fatal("accepted a claim without review evidence")
	}
	// An approval has no correction; models omit the empty field.
	if r, err = ParseAssessment([]byte(`{"accepted":true,"summary":"Verified"}`)); err != nil || !r.Accepted {
		t.Fatal("approval without correction rejected:", err)
	}
	if r, err = ParseAssessment([]byte(`{"accepted":false,"summary":"The empty-input test is missing"}`)); err != nil || r.Correction != "The empty-input test is missing" {
		t.Fatal("rejection without correction lost its reason:", r, err)
	}
}

func TestStrictSchemaRequiresEveryPropertyAndKeepsStrictSchemas(t *testing.T) {
	node := workflow.Node{Kind: "plan", Outputs: []string{"plan"}}
	strict := StrictSchema(ProposalSchema(node))
	if got := strict["required"].([]string); len(got) != 4 || got[0] != "summary" || got[1] != "data" {
		t.Fatalf("root required %v", got)
	}
	artifacts := strict["properties"].(map[string]any)["artifacts"].(map[string]any)
	if got := artifacts["required"].([]string); len(got) != 1 || got[0] != "plan" {
		t.Fatalf("artifacts required %v", got)
	}
	items := strict["properties"].(map[string]any)["requirements"].(map[string]any)["items"].(map[string]any)
	if got := items["required"].([]string); len(got) != 3 {
		t.Fatalf("requirement items required %v", got)
	}
	// A schema that already requires everything is unchanged, so reconnecting
	// to an in-flight Codex job writes identical bytes.
	legacy := map[string]any{"type": "object", "required": []string{"summary", "artifacts"}, "properties": map[string]any{"summary": map[string]any{"type": "string"}, "artifacts": map[string]any{"type": "object", "required": []string{}, "properties": map[string]any{}}}}
	if a, b := workflow.Digest(StrictSchema(legacy)), workflow.Digest(legacy); a != b {
		t.Fatal("strict schema changed an already-strict schema")
	}
	if _, ok := ProposalSchema(node)["properties"].(map[string]any)["artifacts"].(map[string]any)["required"]; ok {
		t.Fatal("StrictSchema mutated its input")
	}
}

func TestPlanRequiresStructuredInventoryAndFrozenContractsRemainReadable(t *testing.T) {
	node := workflow.Node{Kind: "plan", Outputs: []string{"plan"}}
	legacy := []byte(`{"summary":"Spec","artifacts":{"plan":"Plan text"},"data":{}}`)
	if _, err := ParseProposal(legacy, node); err == nil {
		t.Fatal("new Plan omitted structured inventory")
	}
	current := []byte(`{"summary":"Spec","artifacts":{"plan":"Plan text"},"data":{},"requirements":[{"capability":"browser.test","nodes":["qa"],"reason":"Test the user flow"}]}`)
	proposal, err := ParseProposal(current, node)
	if err != nil || len(proposal.Requirements) != 1 {
		t.Fatal("structured Plan contract", err)
	}
	frozen := ProposalSchema(workflow.Node{Outputs: node.Outputs})
	if _, err := ParseProposalWithSchema(legacy, frozen); err != nil {
		t.Fatal("existing submitted invocation changed its contract", err)
	}
	if _, err := ParseProposalWithSchema(current, frozen); err == nil {
		t.Fatal("legacy contract accepted new undeclared fields")
	}
}

func TestOnlyANodeThatOptsInOffersVariationsToItsSupervisor(t *testing.T) {
	off := AssessmentSchemaFor(workflow.Node{})
	if _, ok := off["properties"].(map[string]any)["variations"]; ok {
		t.Fatal("a node that did not opt in offers variations")
	}
	// Every other caller of the plain schema must be unaffected.
	if !reflect.DeepEqual(off, AssessmentSchema()) {
		t.Fatal("opting out changed the supervisor contract")
	}
	on := AssessmentSchemaFor(workflow.Node{Variations: 3})
	v, ok := on["properties"].(map[string]any)["variations"].(map[string]any)
	if !ok || v["maxItems"] != 3 {
		t.Fatalf("variations not bounded by the node's maximum: %v", v)
	}
}

func TestASupervisorsVariationsAreRecordedOnlyWhenTheyAreAChoice(t *testing.T) {
	node := workflow.Node{Variations: 3}
	two := `{"accepted":true,"summary":"good","correction":"","variations":[{"name":"event sourcing","rationale":"if audit history matters"},{"name":"polling","rationale":"if the upstream has no webhooks"}]}`
	got, err := ParseAssessmentFor(node, []byte(two))
	if err != nil || len(got.Variations) != 2 || got.Variations[1].Name != "polling" {
		t.Fatalf("valid variations rejected: %v %+v", err, got)
	}

	for name, raw := range map[string]string{
		// One alternative is not a choice.
		"a lone variation": `{"accepted":true,"summary":"s","variations":[{"name":"a","rationale":"r"}]}`,
		// On a rejection the correction is the way forward.
		"variations on a rejection": `{"accepted":false,"summary":"s","correction":"fix it","variations":[{"name":"a","rationale":"r"},{"name":"b","rationale":"r"}]}`,
		// A person could not tell these apart when choosing.
		"a repeated name": `{"accepted":true,"summary":"s","variations":[{"name":"Polling","rationale":"r"},{"name":"polling","rationale":"r"}]}`,
		// The configured maximum bounds how many can be proposed.
		"more than the maximum": `{"accepted":true,"summary":"s","variations":[{"name":"a","rationale":"r"},{"name":"b","rationale":"r"},{"name":"c","rationale":"r"},{"name":"d","rationale":"r"}]}`,
		// A rationale is what makes a variation reviewable.
		"a missing rationale": `{"accepted":true,"summary":"s","variations":[{"name":"a","rationale":""},{"name":"b","rationale":"r"}]}`,
	} {
		if _, err := ParseAssessmentFor(node, []byte(raw)); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}

	// Proposing none is the right answer when one approach is clearly best.
	if got, err := ParseAssessmentFor(node, []byte(`{"accepted":true,"summary":"s","variations":[]}`)); err != nil || len(got.Variations) != 0 {
		t.Fatalf("an empty proposal was refused: %v", err)
	}
	// A node that did not opt in cannot smuggle variations through.
	if _, err := ParseAssessmentFor(workflow.Node{}, []byte(two)); err == nil {
		t.Fatal("variations accepted from a node that did not opt in")
	}
}
