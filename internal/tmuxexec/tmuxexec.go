// Package tmuxexec implements session.Tmux using the tmux CLI.
package tmuxexec

import (
	"context"
	"os/exec"

	"github.com/eunomie/quack/internal/session"
)

// Tmux runs tmux commands on the host.
type Tmux struct {
	// Socket, if set, passes "-L <socket>" (used by tests).
	Socket string
}

// New returns a Tmux adapter on the default server.
func New() *Tmux { return &Tmux{} }

func (t Tmux) base() []string {
	if t.Socket != "" {
		return []string{"-L", t.Socket}
	}
	return nil
}

// SessionExists reports whether a tmux session with the given name exists.
func (t Tmux) SessionExists(name string) bool {
	args := append(t.base(), "has-session", "-t", "="+name)
	return exec.Command("tmux", args...).Run() == nil
}

// NewSession creates a detached session running o.Argv in o.Dir with o.Env,
// then enables remain-on-exit so the pane is inspectable after the agent exits.
func (t Tmux) NewSession(ctx context.Context, o session.NewSessionOpts) error {
	args := append(t.base(), "new-session", "-d", "-s", o.Name, "-c", o.Dir)
	for _, e := range o.Env {
		args = append(args, "-e", e)
	}
	args = append(args, "--")
	args = append(args, o.Argv...)

	if out, err := exec.CommandContext(ctx, "tmux", args...).CombinedOutput(); err != nil {
		return wrap(err, out)
	}

	remain := append(t.base(), "set-option", "-t", o.Name, "remain-on-exit", "on")
	_ = exec.CommandContext(ctx, "tmux", remain...).Run()
	return nil
}

func wrap(err error, out []byte) error {
	if len(out) == 0 {
		return err
	}
	return &cmdError{err: err, out: string(out)}
}

type cmdError struct {
	err error
	out string
}

func (e *cmdError) Error() string { return e.err.Error() + ": " + e.out }
