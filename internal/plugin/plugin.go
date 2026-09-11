// Package plugin resolves invocation-scoped capabilities and enforces a
// versioned executable protocol. Plugin processes run through a guest executor.
package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const ProtocolVersion = 1

type Descriptor struct {
	ID             string         `json:"id"`
	Version        string         `json:"version"`
	Protocol       int            `json:"protocol"`
	Provides       []string       `json:"provides"`
	Requires       []string       `json:"requires,omitempty"`
	Command        []string       `json:"command"`
	Operations     []string       `json:"operations"`
	ConfigSchema   map[string]any `json:"config_schema,omitempty"`
	Credentials    []string       `json:"credentials,omitempty"`
	Platforms      []string       `json:"platforms,omitempty"`
	PrepareSeconds int            `json:"prepare_seconds,omitempty"`
}
type Binding struct {
	Ref        workflow.PluginRef `json:"ref"`
	Descriptor Descriptor         `json:"descriptor"`
	Digest     string             `json:"digest"`
	SourceDir  string             `json:"source_dir"`
}
type Lock struct {
	Bindings     []Binding         `json:"bindings"`
	Capabilities map[string]string `json:"capabilities"`
}
type Request struct {
	Protocol       int               `json:"protocol"`
	Operation      string            `json:"operation"`
	OperationID    string            `json:"operation_id"`
	RunID          string            `json:"run_id"`
	Revision       string            `json:"revision"`
	RuntimeID      string            `json:"runtime_id,omitempty"` // child isolation namespace; absent in legacy revision-only invocations
	Config         map[string]any    `json:"config"`
	Credentials    map[string]string `json:"credentials,omitempty"`
	Input          map[string]any    `json:"input,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}
type Response struct {
	Protocol       int            `json:"protocol"`
	OK             bool           `json:"ok"`
	Detail         string         `json:"detail"`
	ExpiresSeconds int            `json:"expires_seconds,omitempty"`
	Output         map[string]any `json:"output,omitempty"`
	// Recovery requests a declared lifecycle operation after a failed probe.
	// It is an instruction to reconcile resources, never readiness evidence.
	Recovery string `json:"recovery,omitempty"`
}

// Execute must deliver stdin directly to the guest, not a shell argument or log.
// The caller receives stdout/stderr to redact before persisting any evidence.
type Execute func(context.Context, Binding, []byte) (stdout, stderr []byte, err error)

func Load(root string, ref workflow.PluginRef) (Binding, error) {
	if ref.Source == "" || ref.Version == "" || ref.ID == "" {
		return Binding{}, errors.New("plugin requires id, source, and pinned version")
	}
	source := ref.Source
	if !filepath.IsAbs(source) {
		source = filepath.Join(root, source)
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return Binding{}, err
	}
	raw, err := os.ReadFile(filepath.Join(source, "plugin.json"))
	if err != nil {
		return Binding{}, err
	}
	var descriptor Descriptor
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&descriptor); err != nil {
		return Binding{}, err
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return Binding{}, errors.New("plugin descriptor must contain one JSON object")
	}
	if descriptor.ID != ref.ID || descriptor.Version != ref.Version || descriptor.Protocol != ProtocolVersion {
		return Binding{}, errors.New("plugin identity, version, or protocol mismatch")
	}
	if descriptor.PrepareSeconds < 0 || descriptor.PrepareSeconds > 1800 {
		return Binding{}, errors.New("plugin preparation timeout must be between 1 and 1800 seconds")
	}
	if len(descriptor.Provides) == 0 || len(descriptor.Command) == 0 || !slices.Contains(descriptor.Operations, "probe") {
		return Binding{}, errors.New("plugin requires capabilities, a command, and probe operation")
	}
	allowed := []string{"describe", "prepare", "probe", "execute", "renew", "cleanup"}
	for _, op := range descriptor.Operations {
		if !slices.Contains(allowed, op) {
			return Binding{}, fmt.Errorf("unknown plugin operation %s", op)
		}
	}
	for _, capability := range ref.Provides {
		if !slices.Contains(descriptor.Provides, capability) {
			return Binding{}, fmt.Errorf("plugin cannot supply undeclared capability %s", capability)
		}
	}
	for _, credential := range descriptor.Credentials {
		if ref.Credentials[credential] == "" {
			return Binding{}, fmt.Errorf("plugin %s needs credential reference %s", ref.ID, credential)
		}
	}
	for _, credential := range ref.Credentials {
		if !strings.HasPrefix(credential, "env:") && !strings.HasPrefix(credential, "file:") {
			return Binding{}, errors.New("credential values must be references")
		}
	}
	if len(descriptor.ConfigSchema) > 0 {
		compiler := jsonschema.NewCompiler()
		if err = compiler.AddResource("urn:envctl:plugin-config", descriptor.ConfigSchema); err != nil {
			return Binding{}, err
		}
		schema, err := compiler.Compile("urn:envctl:plugin-config")
		if err != nil {
			return Binding{}, err
		}
		config := ref.Config
		if config == nil {
			config = map[string]any{}
		}
		// YAML and programmatic callers use native integers. Validate the same
		// JSON representation that the executable protocol will receive.
		raw, err := json.Marshal(config)
		if err != nil {
			return Binding{}, err
		}
		var jsonConfig any
		if err = json.Unmarshal(raw, &jsonConfig); err != nil {
			return Binding{}, err
		}
		if err = schema.Validate(jsonConfig); err != nil {
			return Binding{}, fmt.Errorf("plugin %s configuration: %w", ref.ID, err)
		}
	}
	digest, err := SourceDigest(source)
	if err != nil {
		return Binding{}, err
	}
	if ref.Digest != "" && ref.Digest != digest {
		return Binding{}, errors.New("plugin source changed since it was pinned")
	}
	ref.Digest = digest
	return Binding{Ref: ref, Descriptor: descriptor, Digest: digest, SourceDir: source}, nil
}

// SourceDigest includes relative paths, file modes and bytes in deterministic
// order. Symlinks are rejected to prevent packaging files outside the plugin.
func SourceDigest(root string) (string, error) {
	h := sha256.New()
	var total int64
	var files int
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("plugin source must not contain symlinks")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("plugin source contains a non-regular file")
		}
		total += info.Size()
		files++
		if total > 32<<20 || files > 4096 {
			return errors.New("plugin package exceeds 32 MiB or 4096 files")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%d:%s:%o:%d:", len(rel), rel, info.Mode().Perm(), info.Size())
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		return errors.Join(copyErr, closeErr)
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Resolve rejects ambiguous capabilities and cycles. Requirements can resolve
// to built-ins supplied by the caller; no project configuration is mutated.
func Resolve(bindings []Binding, builtins map[string]string) (Lock, error) {
	lock := Lock{Capabilities: map[string]string{}}
	for capability, id := range builtins {
		lock.Capabilities[capability] = id
	}
	byID := map[string]Binding{}
	for _, binding := range bindings {
		if _, ok := byID[binding.Ref.ID]; ok {
			return Lock{}, errors.New("duplicate invocation plugin")
		}
		byID[binding.Ref.ID] = binding
		provides := binding.Ref.Provides
		if len(provides) == 0 {
			provides = binding.Descriptor.Provides
		}
		for _, c := range provides {
			if prior, ok := lock.Capabilities[c]; ok {
				return Lock{}, fmt.Errorf("capability %s conflicts between %s and %s", c, prior, binding.Ref.ID)
			}
			lock.Capabilities[c] = binding.Ref.ID
		}
	}
	visiting, done := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if done[id] {
			return nil
		}
		if visiting[id] {
			return errors.New("plugin dependency cycle")
		}
		visiting[id] = true
		b := byID[id]
		for _, required := range b.Descriptor.Requires {
			provider, ok := lock.Capabilities[required]
			if !ok {
				return fmt.Errorf("plugin %s requires unavailable capability %s", id, required)
			}
			if _, ok = byID[provider]; ok {
				if err := visit(provider); err != nil {
					return err
				}
			}
		}
		visiting[id] = false
		done[id] = true
		lock.Bindings = append(lock.Bindings, b)
		return nil
	}
	ids := []string{}
	for id := range byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return Lock{}, err
		}
	}
	return lock, nil
}
func Credentials(root string, b Binding) (map[string]string, error) {
	values := map[string]string{}
	for name, ref := range b.Ref.Credentials {
		kind, path, ok := strings.Cut(ref, ":")
		if !ok || path == "" {
			return nil, fmt.Errorf("invalid credential reference %s", name)
		}
		switch kind {
		case "env":
			value, ok := os.LookupEnv(path)
			if !ok || value == "" {
				return nil, fmt.Errorf("credential %s requires environment variable %s", name, path)
			}
			values[name] = value
		case "file":
			if !filepath.IsAbs(path) {
				path = filepath.Join(root, path)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("credential %s file unavailable", name)
			}
			values[name] = strings.TrimSpace(string(raw))
			if values[name] == "" {
				return nil, fmt.Errorf("credential %s is empty", name)
			}
		default:
			return nil, fmt.Errorf("unsupported credential reference for %s", name)
		}
	}
	return values, nil
}
func redact(text string, secrets map[string]string) string {
	values := []string{}
	for _, v := range secrets {
		if v != "" {
			values = append(values, v)
		}
	}
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	for _, v := range values {
		text = strings.ReplaceAll(text, v, "[redacted]")
	}
	return text
}
func sanitize(value any, secrets map[string]string) any {
	switch v := value.(type) {
	case string:
		return redact(v, secrets)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = sanitize(item, secrets)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, item := range v {
			out[redact(k, secrets)] = sanitize(item, secrets)
		}
		return out
	default:
		return value
	}
}
func Call(ctx context.Context, execute Execute, b Binding, req Request) (Response, []byte, error) {
	if req.TimeoutSeconds < 0 || req.TimeoutSeconds > 1800 {
		return Response{}, nil, errors.New("plugin timeout exceeds protocol bounds")
	}
	if req.Protocol != ProtocolVersion || req.OperationID == "" || !slices.Contains(b.Descriptor.Operations, req.Operation) {
		return Response{}, nil, errors.New("invalid plugin request")
	}
	// Recheck package identity at each invocation, including retries.
	digest, err := SourceDigest(b.SourceDir)
	if err != nil {
		return Response{}, nil, err
	}
	if digest != b.Digest {
		return Response{}, nil, errors.New("plugin source no longer matches invocation lock")
	}
	input, err := json.Marshal(req)
	if err != nil {
		return Response{}, nil, err
	}
	stdout, stderr, err := execute(ctx, b, input)
	if err != nil {
		detail := fmt.Sprintf("plugin %s %s failed: %s", b.Ref.ID, req.Operation, redact(string(stderr), req.Credentials))
		var terminal *TerminalFailure
		if errors.As(err, &terminal) {
			return Response{}, nil, &TerminalFailure{Detail: detail + ": " + redact(terminal.Detail, req.Credentials)}
		}
		return Response{}, nil, errors.New(detail)
	}
	if len(stdout) > 4<<20 {
		return Response{}, nil, &TerminalFailure{Detail: "plugin response exceeds 4 MiB"}
	}
	var response Response
	dec := json.NewDecoder(bytes.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&response); err != nil {
		return Response{}, nil, &TerminalFailure{Detail: "plugin returned invalid protocol JSON"}
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return Response{}, nil, &TerminalFailure{Detail: "plugin returned multiple responses"}
	}
	response.Detail = redact(response.Detail, req.Credentials)
	if response.Output != nil {
		response.Output = sanitize(response.Output, req.Credentials).(map[string]any)
	}
	if response.Protocol != ProtocolVersion || response.ExpiresSeconds < 0 {
		return Response{}, nil, &TerminalFailure{Detail: "plugin response version or expiry invalid"}
	}
	if response.Recovery != "" && (req.Operation != "probe" || response.OK ||
		!slices.Contains([]string{"prepare", "renew"}, response.Recovery) ||
		!slices.Contains(b.Descriptor.Operations, response.Recovery)) {
		return Response{}, nil, &TerminalFailure{Detail: "plugin recovery must name a declared prepare or renew operation in a failed probe"}
	}
	evidence, err := json.Marshal(response)
	return response, evidence, err
}
func ProbeResult(b Binding, response Response, evidence workflow.Artifact, config workflow.Config, runtimeID, capability string, now time.Time) workflow.Probe {
	ttl := response.ExpiresSeconds
	if ttl <= 0 {
		ttl = 300
	}
	return workflow.Probe{Capability: capability, Binding: b.Ref.ID + "@" + b.Digest, ConfigDigest: workflow.Digest(config), RuntimeID: runtimeID, Passed: response.OK, Detail: response.Detail, EvidenceDigest: evidence.Digest, CheckedAt: now, ExpiresAt: now.Add(time.Duration(ttl) * time.Second)}
}
