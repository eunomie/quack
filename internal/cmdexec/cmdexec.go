// Package cmdexec runs configured fast-command binaries via os/exec. It is the
// concrete adapter for session.Runner, injected by main.go, keeping the session
// core free of os/exec — mirroring gitexec/tmuxexec.
package cmdexec

import (
	"context"
	"errors"
	"os/exec"
)

// Runner execs commands in a working directory, returning combined output.
type Runner struct{}

// New builds a Runner.
func New() *Runner { return &Runner{} }

// Run execs argv with cwd=dir and returns its combined stdout+stderr. The
// context bounds the run (callers apply a timeout); an empty argv is an error.
func (r *Runner) Run(ctx context.Context, dir string, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}
