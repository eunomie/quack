package agentproc

import (
	"context"
	"os"
	"os/exec"
)

// Launcher turns a command (program + args, working dir, extra env) into the
// *exec.Cmd the driver runs. DirectLauncher runs it on the host — quack's
// original behavior. A sandbox launcher (ContainerLauncher) wraps it so a guest
// turn runs inside a container. The driver builds the same argv either way; the
// launcher only decides where it executes.
type Launcher interface {
	Command(ctx context.Context, program string, args []string, dir string, env []string) *exec.Cmd
}

// DirectLauncher runs the command directly on the host.
type DirectLauncher struct{}

func (DirectLauncher) Command(ctx context.Context, program string, args []string, dir string, env []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd
}

// ContainerLauncher runs the command inside an already-running container via
// `docker exec`. Workdir is the in-container path (the host dir passed by the
// driver is ignored — the clone is mounted at a fixed path inside the box).
type ContainerLauncher struct {
	Container string
	Workdir   string
	DockerCmd string // defaults to "docker"; injectable for tests
}

func (c ContainerLauncher) Command(ctx context.Context, program string, args []string, dir string, env []string) *exec.Cmd {
	docker := c.DockerCmd
	if docker == "" {
		docker = "docker"
	}
	full := []string{"exec", "-i", "-w", c.Workdir}
	for _, e := range env {
		full = append(full, "-e", e)
	}
	full = append(full, c.Container, program)
	full = append(full, args...)
	return exec.CommandContext(ctx, docker, full...)
}
