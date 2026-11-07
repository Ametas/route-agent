package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestErrorTextMatchesNode(t *testing.T) {
	// Node: `Command failed: ${[file, ...args].join(' ')}\n${stderr}` (spec C §0.5).
	_, err := Exec{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo out; echo boom >&2; exit 3"}, Timeout: 5 * time.Second})
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("want *Error, got %v", err)
	}
	if got, want := err.Error(), "Command failed: sh -c echo out; echo boom >&2; exit 3\nboom\n"; got != want {
		t.Fatalf("message:\n got %q\nwant %q", got, want)
	}
	if e.Code != 3 || e.Stdout != "out\n" || e.Stderr != "boom\n" || e.Killed {
		t.Fatalf("fields: %+v", e)
	}
	if StderrOrMessage(err) != "boom\n" {
		t.Fatalf("StderrOrMessage = %q", StderrOrMessage(err))
	}
}

func TestShellDisplayIsTheScript(t *testing.T) {
	// For exec(cmd) Node prints the shell string itself, not "/bin/sh -c …".
	_, err := Exec{}.Run(context.Background(), Cmd{Name: "echo x >&2; false", Shell: true, Timeout: 5 * time.Second})
	if err == nil || err.Error() != "Command failed: echo x >&2; false\nx\n" {
		t.Fatalf("got %v", err)
	}
}

func TestEmptyStderrKeepsTrailingNewline(t *testing.T) {
	_, err := Exec{}.Run(context.Background(), Cmd{Name: "false", Timeout: 5 * time.Second})
	if err == nil || err.Error() != "Command failed: false\n" {
		t.Fatalf("got %q", err)
	}
	if StderrOrMessage(err) != "Command failed: false\n" {
		t.Fatalf("StderrOrMessage fallback = %q", StderrOrMessage(err))
	}
}

func TestSpawnFailure(t *testing.T) {
	_, err := Exec{}.Run(context.Background(), Cmd{Name: "/nonexistent/bin/tool", Args: []string{"-v"}, Timeout: time.Second})
	var e *Error
	if !errors.As(err, &e) || e.Spawn == nil {
		t.Fatalf("want spawn error, got %v", err)
	}
	if err.Error() != "spawn /nonexistent/bin/tool ENOENT" {
		t.Fatalf("got %q", err.Error())
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	start := time.Now()
	_, err := Exec{KillGrace: time.Second}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "sleep 30 & sleep 30"}, Timeout: 200 * time.Millisecond})
	var e *Error
	if !errors.As(err, &e) || !e.Killed {
		t.Fatalf("want killed error, got %#v", err)
	}
	if e.Signal != "SIGTERM" {
		t.Fatalf("signal = %q", e.Signal)
	}
	if !strings.HasPrefix(err.Error(), "Command failed: sh -c sleep 30 & sleep 30\n") {
		t.Fatalf("message %q", err.Error())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v; grandchild kept the pipes open", d)
	}
}

func TestTimeoutIsMandatory(t *testing.T) {
	if _, err := (Exec{}).Run(context.Background(), Cmd{Name: "true"}); !errors.Is(err, ErrNoTimeout) {
		t.Fatalf("Exec: got %v", err)
	}
	if _, err := (&Fake{}).Run(context.Background(), Cmd{Name: "true"}); !errors.Is(err, ErrNoTimeout) {
		t.Fatalf("Fake: got %v", err)
	}
}

func TestStdinAndOutputCap(t *testing.T) {
	res, err := Exec{MaxOutput: 4}.Run(context.Background(), Cmd{Name: "cat", Stdin: []byte("abcdefgh"), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "abcd" {
		t.Fatalf("stdout %q", res.Stdout)
	}
}

func TestChildEnvDropsSecret(t *testing.T) {
	t.Setenv("EGRESS_CONTROL_SECRET", "s3cret")
	t.Setenv("ROUTE_AGENT_TEST_VAR", "kept")
	res, err := Exec{}.Run(context.Background(), Cmd{Name: "env", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Stdout, "EGRESS_CONTROL_SECRET") || !strings.Contains(res.Stdout, "ROUTE_AGENT_TEST_VAR=kept") {
		t.Fatalf("env: %s", res.Stdout)
	}
}

func TestFakeRecordsAndScripts(t *testing.T) {
	f := &Fake{Results: []FakeResult{{Result: Result{Stdout: "active\n"}}}}
	res, err := f.Run(context.Background(), Cmd{Name: "systemctl", Args: []string{"is-active", "caddy"}, Timeout: time.Second})
	if err != nil || res.Stdout != "active\n" {
		t.Fatalf("%v %q", err, res.Stdout)
	}
	f.Handler = func(c Cmd) (Result, error) { return FailWith(c, 1, "", "nope\n") }
	_, err = f.Run(context.Background(), Cmd{Name: "dkms", Args: []string{"status"}, Timeout: time.Second})
	if err == nil || err.Error() != "Command failed: dkms status\nnope\n" {
		t.Fatalf("got %v", err)
	}
	if got := f.Displays(); len(got) != 2 || got[0] != "systemctl is-active caddy" || got[1] != "dkms status" {
		t.Fatalf("calls %v", got)
	}
}

func TestPerCallCapOverridesRunnerCap(t *testing.T) {
	res, err := Exec{MaxOutput: 4}.Run(context.Background(), Cmd{Name: "cat", Stdin: []byte("abcdefgh"), MaxOutput: 6, Timeout: 5 * time.Second})
	if err != nil || res.Stdout != "abcdef" {
		t.Fatalf("%v %q", err, res.Stdout)
	}
	if got := (Cmd{}).Limit(0); got != DefaultMaxOutput {
		t.Fatalf("default limit %d", got)
	}
	if got := (Cmd{}).Limit(9); got != 9 {
		t.Fatalf("runner limit %d", got)
	}
}

func TestOverflowTruncatesByDefault(t *testing.T) {
	// The command finishes and reports its own outcome; only the kept output is cut.
	res, err := Exec{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "printf 0123456789; echo done >&2; exit 2"}, MaxOutput: 4, Timeout: 5 * time.Second})
	var e *Error
	if !errors.As(err, &e) || e.Overflow != "" || e.Code != 2 || IsOverflow(err) {
		t.Fatalf("want plain exit error, got %#v", err)
	}
	if res.Stdout != "0123" || res.Stderr != "done" || err.Error() != "Command failed: sh -c printf 0123456789; echo done >&2; exit 2\ndone" {
		t.Fatalf("res %+v err %q", res, err.Error())
	}
}

func TestFailOnOverflowKillsLikeNode(t *testing.T) {
	// Node: maxBuffer exceeded → child killed, reject with "stdout maxBuffer length exceeded".
	start := time.Now()
	res, err := Exec{KillGrace: time.Second}.Run(context.Background(), Cmd{
		Name: "sh", Args: []string{"-c", "printf 0123456789; sleep 30"},
		MaxOutput: 4, FailOnOverflow: true, Timeout: 20 * time.Second,
	})
	var e *Error
	if !errors.As(err, &e) || e.Overflow != "stdout" || !IsOverflow(err) {
		t.Fatalf("want overflow error, got %#v", err)
	}
	if err.Error() != "stdout maxBuffer length exceeded" || e.Killed || e.Signal != "SIGTERM" {
		t.Fatalf("err %q fields %+v", err.Error(), e)
	}
	if res.Stdout != "0123" || e.Stdout != "0123" {
		t.Fatalf("kept %q", res.Stdout)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v; overflow did not kill the child", d)
	}

	_, err = Exec{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "printf 0123456789 >&2"}, MaxOutput: 4, FailOnOverflow: true, Timeout: 5 * time.Second})
	if err == nil || err.Error() != "stderr maxBuffer length exceeded" {
		t.Fatalf("stderr: %v", err)
	}
	// Within the cap nothing changes.
	res, err = Exec{}.Run(context.Background(), Cmd{Name: "printf", Args: []string{"0123"}, MaxOutput: 4, FailOnOverflow: true, Timeout: 5 * time.Second})
	if err != nil || res.Stdout != "0123" {
		t.Fatalf("%v %q", err, res.Stdout)
	}
}

func TestFakeAppliesCap(t *testing.T) {
	f := &Fake{Handler: func(Cmd) (Result, error) { return Result{Stdout: "0123456789"}, nil }}
	res, err := f.Run(context.Background(), Cmd{Name: "x", MaxOutput: 4, Timeout: time.Second})
	if err != nil || res.Stdout != "0123" {
		t.Fatalf("truncate: %v %q", err, res.Stdout)
	}
	_, err = f.Run(context.Background(), Cmd{Name: "x", MaxOutput: 4, FailOnOverflow: true, Timeout: time.Second})
	if !IsOverflow(err) || err.Error() != "stdout maxBuffer length exceeded" {
		t.Fatalf("overflow: %v", err)
	}
	res, err = f.Run(context.Background(), Cmd{Name: "x", MaxOutput: 10, FailOnOverflow: true, Timeout: time.Second})
	if err != nil || res.Stdout != "0123456789" {
		t.Fatalf("fits: %v %q", err, res.Stdout)
	}
}
