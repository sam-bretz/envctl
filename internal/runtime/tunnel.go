package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GuestDialer opens a byte stream to one TCP port on a guest's loopback.
// Providers implement it with an explicit per-connection channel; provider
// automatic port forwarding stays disabled.
type GuestDialer interface {
	DialGuest(ctx context.Context, id string, port int) (io.ReadWriteCloser, error)
}

var _ GuestDialer = (*Lima)(nil)

// sshTarget reads the instance's Lima-generated SSH configuration. Lima
// multiplexes these connections over the instance's control socket.
func (l *Lima) sshTarget(ctx context.Context, id string) (config, host string, err error) {
	b, err := l.output(ctx, "list", "--json", id)
	if err != nil {
		return "", "", err
	}
	var row struct {
		Name     string `json:"name"`
		Status   string `json:"status"`
		Hostname string `json:"hostname"`
		Config   string `json:"sshConfigFile"`
	}
	if err = json.NewDecoder(bytes.NewReader(b)).Decode(&row); err != nil || row.Name != id {
		return "", "", ErrMissing
	}
	if !strings.EqualFold(row.Status, "running") {
		return "", "", errors.New("runtime is not running")
	}
	if !filepath.IsAbs(row.Config) || row.Hostname != "lima-"+id {
		return "", "", errors.New("runtime has no usable SSH configuration")
	}
	return row.Config, row.Hostname, nil
}

// DialGuest connects to 127.0.0.1:port inside the guest through `ssh -W`, a
// stdio channel that forwards exactly one connection and opens no listener.
func (l *Lima) DialGuest(ctx context.Context, id string, port int) (io.ReadWriteCloser, error) {
	if !instanceID.MatchString(id) || port < 1 || port > 65535 {
		return nil, errors.New("invalid guest dial target")
	}
	if err := l.owned(id); err != nil {
		return nil, err
	}
	config, host, err := l.sshTarget(ctx, id)
	if err != nil {
		return nil, err
	}
	binary := l.SSH
	if binary == "" {
		binary = "ssh"
	}
	cmd := exec.Command(binary, "-F", config, "-o", "BatchMode=yes", "-o", "ClearAllForwardings=yes", "-W", fmt.Sprintf("127.0.0.1:%d", port), host)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, errors.New("guest tunnel could not start ssh")
	}
	return &sshStream{cmd: cmd, in: in, out: out}, nil
}

type sshStream struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out io.ReadCloser
}

func (s *sshStream) Read(p []byte) (int, error)  { return s.out.Read(p) }
func (s *sshStream) Write(p []byte) (int, error) { return s.in.Write(p) }
func (s *sshStream) Close() error {
	_ = s.in.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.cmd.Wait()
	return nil
}
