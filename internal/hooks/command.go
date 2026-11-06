package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/procenv"
)

// Command output bounds: a hook answers with a small JSON object; anything past
// these is a misbehaving program, not an answer.
const (
	maxCommandStdout = 1 << 20
	maxCommandStderr = 8 << 10
	// commandWaitDelay bounds how long Wait lingers on the pipes after the
	// deadline kills the process (a grandchild holding stdout open).
	commandWaitDelay = 500 * time.Millisecond
)

// Command is the external command hook (kind command): the program runs once per
// event with the JSON HookEvent on stdin and answers with a JSON HookResult on
// stdout (empty stdout = continue). A non-zero exit, unparseable output or the
// deadline is a hook FAILURE, resolved by on_failure.
//
// It is exec'd from an argv — never through a shell — with the deny-by-default
// procenv environment (OS essentials plus the declared env names, secret-shaped
// names stripped even if named): a hook runs with the principal's authority and
// gets none of the kernel's credentials. The deadline comes from ctx (the chain
// applies the hook's timeout) and kills the process.
type Command struct {
	spec domain.HookSpec
	argv []string
	env  []string
}

// NewCommand builds a command hook.
func NewCommand(spec domain.HookSpec, argv, env []string) (*Command, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return nil, fmt.Errorf("hook %q: command needs an argv", spec.Name)
	}
	return &Command{spec: spec, argv: append([]string(nil), argv...), env: append([]string(nil), env...)}, nil
}

// Spec implements domain.Hook.
func (c *Command) Spec() domain.HookSpec { return c.spec }

// Run implements domain.Hook.
func (c *Command) Run(ctx context.Context, ev domain.HookEvent) (domain.HookResult, error) {
	in, err := json.Marshal(ev)
	if err != nil {
		return domain.HookResult{}, fmt.Errorf("encode event: %w", err)
	}
	cmd := exec.CommandContext(ctx, c.argv[0], c.argv[1:]...)
	cmd.Env = procenv.Allowlisted(c.env)
	cmd.Stdin = bytes.NewReader(in)
	stdout := &cappedBuffer{max: maxCommandStdout}
	stderr := &cappedBuffer{max: maxCommandStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = commandWaitDelay

	runErr := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return domain.HookResult{}, fmt.Errorf("command %q: %w", c.argv[0], ctxErr)
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return domain.HookResult{}, fmt.Errorf("command %q exited %d: %s",
				c.argv[0], exitErr.ExitCode(), strings.TrimSpace(stderr.String()))
		}
		return domain.HookResult{}, fmt.Errorf("command %q: %w", c.argv[0], runErr)
	}
	if stdout.overflow {
		return domain.HookResult{}, fmt.Errorf("command %q: stdout exceeds %d bytes", c.argv[0], maxCommandStdout)
	}
	body := bytes.TrimSpace(stdout.Bytes())
	if len(body) == 0 {
		return domain.HookResult{}, nil
	}
	var res domain.HookResult
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields() // a typo'd "verdct" must fail, not read as continue
	if err := dec.Decode(&res); err != nil {
		return domain.HookResult{}, fmt.Errorf("command %q: stdout is not a hook result: %w", c.argv[0], err)
	}
	return res, nil
}

// cappedBuffer keeps at most max bytes and records that more arrived.
type cappedBuffer struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	room := b.max - b.Len()
	if room <= 0 {
		b.overflow = b.overflow || len(p) > 0
		return len(p), nil
	}
	if len(p) > room {
		b.overflow = true
		b.Buffer.Write(p[:room])
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
