// Package sandbox provisions and tears down the per-guest-session Docker
// containers (agent + dind sidecar + egress proxy) that confine a guest agent.
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %v: %w: %s", name, args, err, out.String())
	}
	return out.Bytes(), nil
}

// Docker is a thin wrapper over the docker CLI (shelled out, like gitexec).
type Docker struct{ run runner }

func NewDocker() *Docker { return &Docker{run: execRun} }

func (d *Docker) CreateNetwork(ctx context.Context, name string, internal bool) error {
	args := []string{"network", "create"}
	if internal {
		args = append(args, "--internal")
	}
	args = append(args, name)
	_, err := d.run(ctx, "docker", args...)
	return err
}

// ConnectNetwork attaches container to network, optionally under extra DNS
// aliases. The dind sidecar is aliased "docker" on the agent's network so the
// agent reaches it at the hostname dind's own TLS cert is issued for.
func (d *Docker) ConnectNetwork(ctx context.Context, network, container string, aliases ...string) error {
	args := []string{"network", "connect"}
	for _, a := range aliases {
		args = append(args, "--alias", a)
	}
	args = append(args, network, container)
	_, err := d.run(ctx, "docker", args...)
	return err
}

func (d *Docker) RemoveNetwork(ctx context.Context, name string) error {
	_, err := d.run(ctx, "docker", "network", "rm", name)
	return err
}

func (d *Docker) CreateVolume(ctx context.Context, name string) error {
	_, err := d.run(ctx, "docker", "volume", "create", name)
	return err
}

func (d *Docker) RemoveVolume(ctx context.Context, name string) error {
	_, err := d.run(ctx, "docker", "volume", "rm", "-f", name)
	return err
}

// Run starts a detached container; args are the full `docker run` arguments
// after `run -d`.
func (d *Docker) Run(ctx context.Context, args ...string) error {
	_, err := d.run(ctx, "docker", append([]string{"run", "-d"}, args...)...)
	return err
}

func (d *Docker) Start(ctx context.Context, container string) error {
	_, err := d.run(ctx, "docker", "start", container)
	return err
}

func (d *Docker) Exec(ctx context.Context, container string, argv ...string) ([]byte, error) {
	return d.run(ctx, "docker", append([]string{"exec", container}, argv...)...)
}

func (d *Docker) Remove(ctx context.Context, container string) error {
	_, err := d.run(ctx, "docker", "rm", "-f", container)
	return err
}

func (d *Docker) Exists(ctx context.Context, container string) bool {
	_, err := d.run(ctx, "docker", "inspect", "--type", "container", container)
	return err == nil
}
