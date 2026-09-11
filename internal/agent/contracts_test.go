package agent

import (
	"github.com/sam-bretz/envctl/internal/workflow"
	"testing"
)

func TestWorkerCannotClaimAuthoritativeEvidence(t *testing.T) {
	node := workflow.Node{Outputs: []string{"implementation"}, OutputSchema: map[string]any{"type": "object", "required": []string{"count"}, "properties": map[string]any{"count": map[string]any{"type": "integer", "minimum": 1}}, "additionalProperties": false}}
	good := []byte(`{"summary":"Completed implementation","artifacts":{"implementation":"Description of the changes"},"data":{"count":1}}`)
	if _, err := ParseProposal(good, node); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"summary":"Done","artifacts":{},"data":{"count":1}}`,
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
