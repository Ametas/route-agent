package runner

import (
	"context"
	"sync"
)

// Fake is a Runner for tests: it records every call and answers from Handler (or from the
// Results queue, then a zero Result). Safe for concurrent use.
type Fake struct {
	mu    sync.Mutex
	calls []Cmd
	// Handler, if set, decides the outcome of each call.
	Handler func(c Cmd) (Result, error)
	// Results is consumed in order when Handler is nil.
	Results []FakeResult
}

// FakeResult is one scripted outcome.
type FakeResult struct {
	Result Result
	Err    error
}

// Run implements Runner. Like Exec, it refuses a call without a timeout.
func (f *Fake) Run(_ context.Context, c Cmd) (Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	h := f.Handler
	var next *FakeResult
	if h == nil && len(f.Results) > 0 {
		r := f.Results[0]
		f.Results = f.Results[1:]
		next = &r
	}
	f.mu.Unlock()
	if c.Timeout <= 0 {
		return Result{ExitCode: -1}, ErrNoTimeout
	}
	if h != nil {
		return h(c)
	}
	if next != nil {
		return next.Result, next.Err
	}
	return Result{}, nil
}

// Calls returns a copy of the recorded calls.
func (f *Fake) Calls() []Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Cmd(nil), f.calls...)
}

// Displays returns Cmd.Display() of every recorded call, in order.
func (f *Fake) Displays() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.Display()
	}
	return out
}

// FailWith builds the *Error a real command with this exit code and stderr would produce.
func FailWith(c Cmd, code int, stdout, stderr string) (Result, error) {
	return Result{Stdout: stdout, Stderr: stderr, ExitCode: code},
		&Error{Cmd: c.Display(), Stdout: stdout, Stderr: stderr, Code: code}
}
