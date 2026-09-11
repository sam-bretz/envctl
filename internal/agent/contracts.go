package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/sam-bretz/envctl/internal/workflow"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Proposal contains only worker-authored material. It cannot claim commits,
// passing checks, supervisor acceptance, human approval, or publication receipts.
type Proposal struct {
	Summary      string                 `json:"summary"`
	Artifacts    map[string]string      `json:"artifacts"`
	Data         map[string]any         `json:"data"`
	Requirements []workflow.Requirement `json:"requirements,omitempty"`
}
type Assessment struct {
	Accepted   bool   `json:"accepted"`
	Summary    string `json:"summary"`
	Correction string `json:"correction"`
}

func ProposalSchema(node workflow.Node) map[string]any {
	properties := map[string]any{}
	for _, name := range node.Outputs {
		properties[name] = map[string]any{"type": "string", "minLength": 1}
	}
	data := workflow.Clone(node.OutputSchema)
	if len(data) == 0 {
		data = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false, "required": []string{}}
	}
	required := []string{"summary", "artifacts", "data"}
	fields := map[string]any{
		"summary":   map[string]any{"type": "string", "minLength": 1},
		"artifacts": map[string]any{"type": "object", "properties": properties, "required": node.Outputs, "additionalProperties": false},
		"data":      data,
	}
	if node.Kind == "plan" {
		required = append(required, "requirements")
		fields["requirements"] = map[string]any{"type": "array", "maxItems": 256, "items": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"capability", "nodes", "reason"},
			"properties": map[string]any{
				"capability": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
				"nodes":      map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}},
				"reason":     map[string]any{"type": "string", "minLength": 1, "maxLength": 4096},
			},
		}}
	}
	return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": fields}
}
func AssessmentSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"accepted", "summary", "correction"}, "properties": map[string]any{
		"accepted": map[string]any{"type": "boolean"}, "summary": map[string]any{"type": "string", "minLength": 1}, "correction": map[string]any{"type": "string"},
	}}
}
func decodeContract(raw []byte, schema map[string]any, target any) error {
	if len(raw) > 1<<20 {
		return errors.New("harness result exceeds contract limit")
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return errors.New("harness result is not valid JSON")
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("urn:envctl:harness-result", workflow.Clone(schema)); err != nil {
		return errors.New("invalid harness result schema")
	}
	compiled, err := compiler.Compile("urn:envctl:harness-result")
	if err != nil {
		return errors.New("invalid harness result schema")
	}
	if compiled.Validate(value) != nil {
		return errors.New("harness result does not satisfy its declared output contract")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return errors.New("harness result has unsupported fields or types")
	}
	if decoder.Decode(&value) != io.EOF {
		return errors.New("expected exactly one harness result")
	}
	return nil
}
func ParseProposal(raw []byte, node workflow.Node) (Proposal, error) {
	return ParseProposalWithSchema(raw, ProposalSchema(node))
}

// Resume against the frozen invocation contract. Older pending jobs need not
// change their submitted request to adopt fields introduced by a new binary.
func ParseProposalWithSchema(raw []byte, schema map[string]any) (Proposal, error) {
	var result Proposal
	err := decodeContract(raw, schema, &result)
	return result, err
}
func ParseAssessment(raw []byte) (Assessment, error) {
	var result Assessment
	err := decodeContract(raw, AssessmentSchema(), &result)
	return result, err
}
