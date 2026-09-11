package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"

	"github.com/sam-bretz/envctl/internal/workflow"
	"gopkg.in/yaml.v3"
)

// Pins are from Lima 2.2.0's published Ubuntu LTS template. No mutable-image
// fallback is allowed: an unavailable pin is an actionable preparation failure.
const armImage = "https://cloud-images.ubuntu.com/releases/resolute/release-20260720/ubuntu-26.04-server-cloudimg-arm64.img"
const armDigest = "sha256:7bcf159e29ad0000bfed9c57875908c39268f5ed1257f4958fa6a9f5f60edd54"
const amdImage = "https://cloud-images.ubuntu.com/releases/resolute/release-20260720/ubuntu-26.04-server-cloudimg-amd64.img"
const amdDigest = "sha256:117816726abbdefc5ef3e38902e81a76f1c76c3610e709999d0885f9d5d9b477"

var instanceID = regexp.MustCompile(`^envctl-[a-z0-9_-]{1,64}$`)
var imageSHA = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var ErrMissing = errors.New("runtime does not exist")

type Lima struct {
	Dir    string
	Binary string
	Log    io.Writer
}

func NewLima(dir string) *Lima { return &Lima{Dir: dir, Binary: "limactl"} }
func DefaultSpec(id string) Spec {
	spec := Spec{ID: id, CPUs: 2, MemoryGiB: 4, DiskGiB: 30}
	if goruntime.GOARCH == "arm64" {
		spec.Arch = "aarch64"
		spec.Image = armImage
		spec.ImageDigest = armDigest
	} else {
		spec.Arch = "x86_64"
		spec.Image = amdImage
		spec.ImageDigest = amdDigest
	}
	return spec
}
func validateSpec(s Spec) error {
	if !instanceID.MatchString(s.ID) {
		return errors.New("runtime ID must be an envctl-owned identifier")
	}
	if s.CPUs < 1 || s.MemoryGiB < 1 || s.DiskGiB < 4 {
		return errors.New("invalid VM resources")
	}
	if !strings.HasPrefix(s.Image, "https://") || !imageSHA.MatchString(s.ImageDigest) {
		return errors.New("VM image requires HTTPS URL and immutable sha256 digest")
	}
	if s.Arch != "aarch64" && s.Arch != "x86_64" {
		return errors.New("supported guests are aarch64 and x86_64")
	}
	return nil
}

// Guest firewall rules block new connections to host/private network addresses.
// Established SSH replies remain allowed. Docker traffic inside the guest's
// bridges remains local; only traffic leaving the guest is filtered.
const provision = `#!/bin/bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
if ! command -v docker >/dev/null || ! docker compose version >/dev/null 2>&1; then
  apt-get -o Acquire::Retries=3 -o Acquire::ForceIPv4=true update
  apt-get -o Acquire::Retries=3 -o Acquire::ForceIPv4=true install -y --no-install-recommends docker.io docker-compose-v2 git git-lfs curl ca-certificates jq python3 iptables
fi
systemctl enable --now docker
mkdir -p /opt/envctl /var/lib/envctl
chmod 755 /opt/envctl
cat > /opt/envctl/network-policy <<'POLICY'
#!/bin/bash
set -euo pipefail
wan=$(ip -4 route show default | awk '{print $5; exit}')
[ -n "$wan" ]
iptables -N ENVCTL-EGRESS 2>/dev/null || true
iptables -F ENVCTL-EGRESS
iptables -A ENVCTL-EGRESS -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
for subnet in 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 169.254.0.0/16; do
  iptables -A ENVCTL-EGRESS -d "$subnet" -j REJECT
done
iptables -A ENVCTL-EGRESS -j RETURN
iptables -C OUTPUT -o "$wan" -j ENVCTL-EGRESS 2>/dev/null || iptables -I OUTPUT 1 -o "$wan" -j ENVCTL-EGRESS
iptables -N DOCKER-USER 2>/dev/null || true
iptables -C DOCKER-USER -o "$wan" -j ENVCTL-EGRESS 2>/dev/null || iptables -I DOCKER-USER 1 -o "$wan" -j ENVCTL-EGRESS
# Block guest-originated IPv6 until a provider has an equivalent explicit policy.
ip6tables -C OUTPUT -o "$wan" -m conntrack --ctstate NEW -j REJECT 2>/dev/null || ip6tables -A OUTPUT -o "$wan" -m conntrack --ctstate NEW -j REJECT
POLICY
chmod 755 /opt/envctl/network-policy
cat > /etc/systemd/system/envctl-network.service <<'UNIT'
[Unit]
After=docker.service network-online.target
Requires=docker.service
[Service]
Type=oneshot
ExecStart=/opt/envctl/network-policy
RemainAfterExit=yes
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now envctl-network
# The manifest records the exact installed package versions for later replay.
dpkg-query -W -f='${Package}=${Version}\n' > /var/lib/envctl/toolchain.lock
`

func RenderLima(s Spec) ([]byte, error) {
	if err := validateSpec(s); err != nil {
		return nil, err
	}
	vmType := "qemu"
	if goruntime.GOOS == "darwin" {
		vmType = "vz"
	}
	config := map[string]any{
		"minimumLimaVersion": "2.0.0", "vmType": vmType, "arch": s.Arch, "cpus": s.CPUs, "memory": fmt.Sprintf("%dGiB", s.MemoryGiB), "disk": fmt.Sprintf("%dGiB", s.DiskGiB),
		"images": []any{map[string]any{"location": s.Image, "arch": s.Arch, "digest": s.ImageDigest}},
		"mounts": []any{}, "containerd": map[string]bool{"system": false, "user": false},
		"ssh":          map[string]bool{"forwardAgent": false, "loadDotSSHPubKeys": false},
		"hostResolver": map[string]bool{"enabled": false}, "dns": []string{"1.1.1.1", "8.8.8.8"},
		"portForwards": []any{map[string]any{"guestIP": "0.0.0.0", "guestIPMustBeZero": false, "guestPortRange": []int{1, 65535}, "proto": "any", "ignore": true}},
		"param":        map[string]string{"internal_netplanOptional": "true"},
		"provision":    []any{map[string]any{"mode": "system", "script": provision}},
	}
	return yaml.Marshal(config)
}
func (l *Lima) cmd(ctx context.Context, args ...string) *exec.Cmd {
	binary := l.Binary
	if binary == "" {
		binary = "limactl"
	}
	return exec.CommandContext(ctx, binary, args...)
}
func (l *Lima) output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := l.cmd(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if l.Log != nil {
		cmd.Stderr = io.MultiWriter(&stderr, l.Log)
	}
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("limactl %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return b, nil
}
func (l *Lima) Inspect(ctx context.Context, id string) (Instance, error) {
	if !instanceID.MatchString(id) {
		return Instance{}, errors.New("invalid runtime ID")
	}
	b, err := l.output(ctx, "list", "--json")
	if err != nil {
		return Instance{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		var row struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if err = dec.Decode(&row); err == io.EOF {
			break
		} else if err != nil {
			return Instance{}, err
		}
		if row.Name == id {
			return Instance{ID: id, State: strings.ToLower(row.Status)}, nil
		}
	}
	return Instance{}, ErrMissing
}
func (l *Lima) Ensure(ctx context.Context, s Spec) (Instance, error) {
	raw, err := RenderLima(s)
	if err != nil {
		return Instance{}, err
	}
	dir := filepath.Join(l.Dir, "runtimes", s.ID)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return Instance{}, err
	}
	specPath := filepath.Join(dir, "spec.json")
	reservation := struct {
		Spec           Spec   `json:"spec"`
		TemplateDigest string `json:"template_digest"`
	}{s, workflow.Digest(string(raw))}
	digest := workflow.Digest(reservation)
	if previous, err := os.ReadFile(specPath); err == nil {
		var old struct {
			Spec           Spec   `json:"spec"`
			TemplateDigest string `json:"template_digest"`
		}
		if err = json.Unmarshal(previous, &old); err != nil {
			return Instance{}, err
		}
		if workflow.Digest(old) != digest {
			return Instance{}, errors.New("runtime ID already reserved for a different spec")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Instance{}, err
	} else {
		if _, inspectErr := l.Inspect(ctx, s.ID); !errors.Is(inspectErr, ErrMissing) {
			if inspectErr != nil {
				return Instance{}, inspectErr
			}
			return Instance{}, errors.New("refusing to adopt a runtime without this coordinator's reservation")
		}
		b, _ := json.Marshal(reservation)
		temp, e := os.CreateTemp(dir, ".reservation-")
		if e != nil {
			return Instance{}, e
		}
		defer os.Remove(temp.Name())
		if _, e = temp.Write(b); e != nil {
			temp.Close()
			return Instance{}, e
		}
		if e = temp.Sync(); e != nil {
			temp.Close()
			return Instance{}, e
		}
		if e = temp.Close(); e != nil {
			return Instance{}, e
		}
		if e = os.Link(temp.Name(), specPath); e != nil {
			return Instance{}, fmt.Errorf("reserve runtime (retry to reconcile): %w", e)
		}
		directory, e := os.Open(dir)
		if e != nil {
			return Instance{}, e
		}
		e = directory.Sync()
		directory.Close()
		if e != nil {
			return Instance{}, e
		}
	}
	configPath := filepath.Join(dir, "lima.yaml")
	if err = os.WriteFile(configPath, raw, 0600); err != nil {
		return Instance{}, err
	}
	inst, err := l.Inspect(ctx, s.ID)
	if errors.Is(err, ErrMissing) {
		if _, err = l.output(ctx, "start", "--tty=false", "--name", s.ID, "--timeout=30m", configPath); err != nil {
			return Instance{}, err
		}
	} else if err != nil {
		return Instance{}, err
	} else if inst.State != "running" {
		if err = l.Start(ctx, s.ID); err != nil {
			return Instance{}, err
		}
	}
	// Lima's boot-complete signal does not prove that a user provisioning script
	// succeeded. Repair an interrupted bootstrap before claiming runtime readiness.
	if _, err = l.guestOutput(ctx, s.ID, "sudo", "sh", "-c", "test -s /var/lib/envctl/toolchain.lock && docker info >/dev/null && docker compose version >/dev/null"); err != nil {
		var diagnostic tailOutput
		if err = l.Exec(ctx, s.ID, Command{Args: []string{"sudo", "bash", "-s"}, Stdin: strings.NewReader(provision), Stdout: &diagnostic, Stderr: &diagnostic}); err != nil {
			return Instance{}, fmt.Errorf("guest bootstrap incomplete: %w: %s", err, string(diagnostic.data))
		}
	}
	var b bytes.Buffer
	rawID, err := l.guestOutput(ctx, s.ID, "sudo", "docker", "info", "--format", "{{.ID}}")
	if err != nil {
		return Instance{}, err
	}
	inst = Instance{ID: s.ID, State: "running", DaemonID: strings.TrimSpace(string(rawID)), SpecDigest: digest}
	if inst.DaemonID == "" {
		return Instance{}, errors.New("guest Docker daemon has no identity")
	}
	b.Reset()
	if err = l.Exec(ctx, s.ID, Command{Args: []string{"sudo", "docker", "version", "--format", "{{.Server.Version}}"}, Stdout: &b}); err != nil {
		return Instance{}, err
	}
	inst.DockerVersion = strings.TrimSpace(b.String())
	b.Reset()
	if err = l.Exec(ctx, s.ID, Command{Args: []string{"sudo", "docker", "compose", "version", "--short"}, Stdout: &b}); err != nil {
		return Instance{}, err
	}
	inst.ComposeVersion = strings.TrimSpace(b.String())
	return inst, nil
}

type tailOutput struct{ data []byte }

func (t *tailOutput) Write(p []byte) (int, error) {
	n := len(p)
	const limit = 32 << 10
	if len(p) >= limit {
		t.data = append(t.data[:0], p[len(p)-limit:]...)
		return n, nil
	}
	if len(t.data)+len(p) > limit {
		t.data = t.data[len(t.data)+len(p)-limit:]
	}
	t.data = append(t.data, p...)
	return n, nil
}
func (l *Lima) guestOutput(ctx context.Context, id string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	var diagnostic tailOutput
	if err := l.Exec(ctx, id, Command{Args: args, Stdout: &out, Stderr: &diagnostic}); err != nil {
		return nil, fmt.Errorf("guest %s failed: %w: %s", args[0], err, string(diagnostic.data))
	}
	return out.Bytes(), nil
}
func (l *Lima) Exec(ctx context.Context, id string, c Command) error {
	if !instanceID.MatchString(id) || len(c.Args) == 0 {
		return errors.New("invalid guest command")
	}
	if err := l.owned(id); err != nil {
		return err
	}
	args := append([]string{"shell", id, "--"}, c.Args...)
	cmd := l.cmd(ctx, args...)
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	return cmd.Run()
}
func (l *Lima) Start(ctx context.Context, id string) error {
	if !instanceID.MatchString(id) {
		return errors.New("invalid runtime ID")
	}
	if err := l.owned(id); err != nil {
		return err
	}
	_, err := l.output(ctx, "start", "--tty=false", "--timeout=30m", id)
	return err
}
func (l *Lima) Stop(ctx context.Context, id string) error {
	if !instanceID.MatchString(id) {
		return errors.New("invalid runtime ID")
	}
	if err := l.owned(id); err != nil {
		return err
	}
	_, err := l.output(ctx, "stop", id)
	return err
}
func (l *Lima) Destroy(ctx context.Context, id string) error {
	if !instanceID.MatchString(id) {
		return errors.New("refusing to delete an unowned runtime")
	}
	_, err := l.Inspect(ctx, id)
	if errors.Is(err, ErrMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = l.owned(id); err != nil {
		return err
	}
	_, err = l.output(ctx, "delete", "--force", id)
	return err
}

func (l *Lima) owned(id string) error {
	if !instanceID.MatchString(id) {
		return errors.New("invalid runtime ID")
	}
	raw, err := os.ReadFile(filepath.Join(l.Dir, "runtimes", id, "spec.json"))
	if err != nil {
		return errors.New("runtime has no readable coordinator ownership receipt")
	}
	var reservation struct {
		Spec           Spec   `json:"spec"`
		TemplateDigest string `json:"template_digest"`
	}
	if json.Unmarshal(raw, &reservation) != nil || reservation.Spec.ID != id || validateSpec(reservation.Spec) != nil {
		return errors.New("runtime ownership receipt is invalid")
	}
	return nil
}
