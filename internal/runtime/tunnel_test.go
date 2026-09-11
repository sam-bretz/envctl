package runtime

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDialGuestUsesExplicitSSHStdioChannel(t *testing.T) {
	dir := t.TempDir()
	id := "envctl-tunnel"
	config := filepath.Join(dir, "ssh.config")
	limactl := filepath.Join(dir, "limactl")
	ssh := filepath.Join(dir, "ssh")
	args := filepath.Join(dir, "args")
	if err := os.WriteFile(limactl, []byte("#!/bin/sh\necho '{\"name\":\""+id+"\",\"status\":\"Running\",\"hostname\":\"lima-"+id+"\",\"sshConfigFile\":\""+config+"\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	// The fake ssh records its arguments and echoes the tunneled stream.
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\necho \"$@\" > '"+args+"'\nexec cat\n"), 0700); err != nil {
		t.Fatal(err)
	}
	l := NewLima(dir)
	l.Binary, l.SSH = limactl, ssh
	if _, err := l.DialGuest(context.Background(), id, 8080); err == nil {
		t.Fatal("dialed a runtime without a coordinator ownership receipt")
	}
	receipt, _ := json.Marshal(map[string]any{"spec": DefaultSpec(id), "template_digest": "test"})
	if err := os.MkdirAll(filepath.Join(dir, "runtimes", id), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runtimes", id, "spec.json"), receipt, 0600); err != nil {
		t.Fatal(err)
	}
	for _, port := range []int{0, 65536} {
		if _, err := l.DialGuest(context.Background(), id, port); err == nil {
			t.Fatal("invalid guest port accepted", port)
		}
	}
	stream, err := l.DialGuest(context.Background(), id, 18089)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 5)
	if _, err = io.ReadFull(stream, reply); err != nil || string(reply) != "ping\n" {
		t.Fatal("stream did not carry bytes", err, string(reply))
	}
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(raw))
	want := "-F " + config + " -o BatchMode=yes -o ClearAllForwardings=yes -W 127.0.0.1:18089 lima-" + id
	if got != want {
		t.Fatalf("ssh arguments %q, want %q", got, want)
	}
}
