// Package runtime owns the VM boundary. Providers execute commands inside a
// dedicated guest; the host Docker daemon is never a workflow provider.
package runtime

import (
	"context"
	"io"
)

type Spec struct {
	ID          string `json:"id"`
	CPUs        int    `json:"cpus"`
	MemoryGiB   int    `json:"memory_gib"`
	DiskGiB     int    `json:"disk_gib"`
	Image       string `json:"image"`
	ImageDigest string `json:"image_digest"`
	Arch        string `json:"arch"`
}
type Instance struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	DaemonID       string `json:"daemon_id"`
	DockerVersion  string `json:"docker_version"`
	ComposeVersion string `json:"compose_version"`
	SpecDigest     string `json:"spec_digest"`
}
type Command struct {
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}
type Provider interface {
	Ensure(context.Context, Spec) (Instance, error)
	Inspect(context.Context, string) (Instance, error)
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Destroy(context.Context, string) error
	Exec(context.Context, string, Command) error
}
