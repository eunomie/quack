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
