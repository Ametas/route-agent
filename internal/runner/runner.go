// Package runner runs external commands for the agent.
//
// It replaces src/utils/exec.ts (spec A §6, B §0.4, C §0.5, D §0.3) with three rules the TS
// agent did not have (architecture §2, §6 item 4):
//
//   - No shell. A command is a binary plus an argv slice, run with exec.CommandContext. The only
//     exception is Cmd.Shell, kept for the user-configurable command strings that are themselves
//     contract (RELOAD_COMMAND, SINGBOX_RESTART_COMMAND, CADDY_RELOAD_COMMAND, spec A §6).
//   - A timeout on every call. A Cmd without Timeout is refused (ErrNoTimeout) instead of running
//     unbounded. On expiry the whole process group gets SIGTERM, then SIGKILL after KillGrace.
//   - The failure text keeps the Node child_process shape the orchestrator parses
//     (orch:src/shared/utils/failureDigest.ts takes line 1 as "what was run"):
//     "Command failed: <cmd>\n<stderr>", where <cmd> is the shell string for Shell commands and
//     "<name> <args joined by space>" otherwise; a spawn failure is "spawn <name> ENOENT".
//
// Runner is an interface so tests substitute Fake (fake.go), which records calls and returns
// scripted results.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrNoTimeout is returned for a Cmd without a positive Timeout: every external command must be
// bounded (architecture §2).
var ErrNoTimeout = errors.New("runner: command has no timeout")

// DefaultMaxOutput caps how much of stdout and of stderr is kept in memory per call. By default
// output past the cap is read and discarded (the child is not killed, unlike Node's 1 MiB
// maxBuffer, which spec A §6 calls a bug not worth emulating). A caller whose result is useless
// when cut short sets Cmd.FailOnOverflow to get Node's behaviour instead (see Cmd).
const DefaultMaxOutput = 8 << 20

// DefaultKillGrace is how long a timed-out command gets between SIGTERM and SIGKILL.
const DefaultKillGrace = 5 * time.Second

// Cmd describes one external command.
type Cmd struct {
	// Name is the binary (looked up in PATH when it has no slash). For Shell commands it is the
	// whole shell string, run as /bin/sh -c Name.
	Name string
	Args []string
	// Shell runs Name through /bin/sh -c. Only for configurable command strings that are part of
	// the contract (spec A §6); never build a shell string from request data.
	Shell bool
	// Timeout is mandatory.
	Timeout time.Duration
	// Stdin, if non-nil, is written to the child's stdin.
	Stdin []byte
	// Env, if non-nil, replaces the child environment entirely. Nil means ChildEnv().
	Env []string
	// Dir is the working directory; empty means the agent's cwd.
	Dir string
	// MaxOutput caps kept stdout and stderr bytes each for this call; 0 means the runner's cap
	// (Exec.MaxOutput, else DefaultMaxOutput).
	MaxOutput int
	// FailOnOverflow selects what happens when stdout or stderr outgrows the cap:
	//   - false (default): the excess is read and discarded, the command runs to completion and its
	//     outcome is reported as usual with the output truncated at the cap;
	//   - true: Node's maxBuffer behaviour — the process group gets SIGTERM at once and the call
	//     fails with an *Error whose Overflow is "stdout" or "stderr" and whose Error() is Node's
	//     "<stream> maxBuffer length exceeded" (ERR_CHILD_PROCESS_STDIO_MAXBUFFER). Use it where a
	//     partial result would be silently wrong (e.g. `conntrack -L`, awgFlows.ts:194).
	FailOnOverflow bool
}

// Limit is the effective per-stream cap for c given the runner's own cap (0 = DefaultMaxOutput).
func (c Cmd) Limit(runnerMax int) int {
	switch {
	case c.MaxOutput > 0:
		return c.MaxOutput
	case runnerMax > 0:
		return runnerMax
	}
	return DefaultMaxOutput
}

// Display is the command text Node would put after "Command failed: ".
func (c Cmd) Display() string {
	if c.Shell || len(c.Args) == 0 {
		return c.Name
	}
	return c.Name + " " + strings.Join(c.Args, " ")
}

// Result is what a finished command produced.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner runs commands. Implementations must be safe for concurrent use.
type Runner interface {
	// Run executes c. On a non-zero exit, a signal, a timeout or a spawn failure it returns the
	// partial Result together with a *Error.
	Run(ctx context.Context, c Cmd) (Result, error)
}

// Error is a failed command. Error() reproduces Node's child_process rejection message.
type Error struct {
	Cmd    string // Cmd.Display()
	Stdout string
	Stderr string
	// Code is the exit code, or -1 when the child was killed by a signal or never started.
	Code int
	// Signal is the terminating signal name ("SIGTERM"), empty otherwise.
	Signal string
	// Killed is true when the runner killed the child because the timeout expired.
	Killed bool
	// Spawn is set when the command could not be started; Error() is then "spawn <name> <CODE>".
	Spawn error
	// SpawnCode is the Node-style errno name for a spawn failure ("ENOENT", "EACCES").
	SpawnCode string
	// Overflow is "stdout" or "stderr" when Cmd.FailOnOverflow was set and that stream outgrew
	// the cap; Error() is then Node's "<stream> maxBuffer length exceeded". Stdout and Stderr hold
	// what was kept up to the cap.
	Overflow string
}

// IsOverflow reports whether err is a FailOnOverflow failure.
func IsOverflow(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Overflow != ""
}

func (e *Error) Error() string {
	if e.Overflow != "" {
		return e.Overflow + " maxBuffer length exceeded"
	}
	if e.Spawn != nil {
		name := e.Cmd
		if i := strings.IndexByte(name, ' '); i >= 0 {
			name = name[:i]
		}
		return "spawn " + name + " " + e.SpawnCode
	}
	return "Command failed: " + e.Cmd + "\n" + e.Stderr
}

// StderrOrMessage mirrors the TS idiom `err.stderr || err.message` (e.g. system.service.ts:117).
func StderrOrMessage(err error) string {
	var e *Error
	if errors.As(err, &e) && e.Stderr != "" {
		return e.Stderr
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// Exec is the real Runner.
type Exec struct {
	// MaxOutput caps kept stdout/stderr bytes each when the Cmd sets none; 0 means
	// DefaultMaxOutput.
	MaxOutput int
	// KillGrace is the SIGTERM→SIGKILL delay on timeout; 0 means DefaultKillGrace.
	KillGrace time.Duration
}

// ChildEnv is the environment given to children by default: the agent's own environment minus
// EGRESS_CONTROL_SECRET (spec A R-9: npm scripts, caddy validate and every other child inherited
// the node secret; see docs/go-rewrite/deviations.md).
func ChildEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if strings.HasPrefix(kv, "EGRESS_CONTROL_SECRET=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Run implements Runner.
func (x Exec) Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Timeout <= 0 {
		return Result{ExitCode: -1}, ErrNoTimeout
	}
	maxOut := c.Limit(x.MaxOutput)
	grace := x.KillGrace
	if grace <= 0 {
		grace = DefaultKillGrace
	}

	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	// runCtx is ctx plus the overflow kill: FailOnOverflow cancels it, which signals the group
	// through cmd.Cancel exactly like a timeout, but ctx.Err() stays nil so it is not "Killed".
	runCtx, overflowKill := context.WithCancel(ctx)
	defer overflowKill()

	var cmd *exec.Cmd
	if c.Shell {
		cmd = exec.CommandContext(runCtx, "/bin/sh", "-c", c.Name)
	} else {
		cmd = exec.CommandContext(runCtx, c.Name, c.Args...)
	}
	if c.Env != nil {
		cmd.Env = c.Env
	} else {
		cmd.Env = ChildEnv()
	}
	cmd.Dir = c.Dir
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	var overflow overflowMark
	stdout := &capBuffer{max: maxOut}
	stderr := &capBuffer{max: maxOut}
	if c.FailOnOverflow {
		stdout.onOverflow = func() { overflow.set("stdout"); overflowKill() }
		stderr.onOverflow = func() { overflow.set("stderr"); overflowKill() }
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return signalGroup(cmd, syscall.SIGTERM) }
	cmd.WaitDelay = grace

	runErr := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: 0}
	if stream := overflow.get(); stream != "" {
		// Node rejects with the maxBuffer error whatever the exit status turned out to be.
		e := &Error{Cmd: c.Display(), Stdout: res.Stdout, Stderr: res.Stderr, Code: -1, Overflow: stream}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				e.Signal = signalName(ws.Signal())
			} else {
				e.Code = exitErr.ExitCode()
			}
		} else if runErr == nil {
			e.Code = 0
		}
		res.ExitCode = e.Code
		return res, e
	}
	if runErr == nil {
		return res, nil
	}
	e := &Error{Cmd: c.Display(), Stdout: res.Stdout, Stderr: res.Stderr, Code: -1}

	var exitErr *exec.ExitError
	switch {
	case errors.As(runErr, &exitErr):
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			e.Signal = signalName(ws.Signal())
		} else {
			e.Code = exitErr.ExitCode()
		}
		if ctx.Err() != nil {
			e.Killed = true
		}
	case ctx.Err() != nil && cmd.ProcessState != nil:
		e.Killed = true
	default:
		e.Spawn = runErr
		e.SpawnCode = spawnCode(runErr)
	}
	res.ExitCode = e.Code
	return res, e
}

func spawnCode(err error) string {
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return "ENOENT"
	case errors.Is(err, os.ErrPermission):
		return "EACCES"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("E%d", int(errno))
	}
	return "UNKNOWN"
}

func signalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGHUP:
		return "SIGHUP"
	}
	return s.String()
}

// overflowMark records the first stream that overflowed (stdout and stderr are copied by
// separate goroutines).
type overflowMark struct {
	mu     sync.Mutex
	stream string
}

func (o *overflowMark) set(s string) {
	o.mu.Lock()
	if o.stream == "" {
		o.stream = s
	}
	o.mu.Unlock()
}

func (o *overflowMark) get() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stream
}

// capBuffer keeps the first max bytes written and drops the rest; onOverflow, if set, is called
// once when the first dropped byte arrives. It keeps accepting (and dropping) writes so the child
// never blocks on a full pipe while it is being killed.
type capBuffer struct {
	buf        bytes.Buffer
	max        int
	onOverflow func()
	overflowed bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	room := b.max - b.buf.Len()
	if room < 0 {
		room = 0
	}
	if len(p) <= room {
		b.buf.Write(p)
		return len(p), nil
	}
	b.buf.Write(p[:room])
	if !b.overflowed {
		b.overflowed = true
		if b.onOverflow != nil {
			b.onOverflow()
		}
	}
	return len(p), nil
}

func (b *capBuffer) String() string { return b.buf.String() }

var _ io.Writer = (*capBuffer)(nil)
