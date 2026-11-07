package systemd

import (
	"context"
	"io"
	"log/slog"
	"os"
	"reflect"
	"testing"

	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
)

// Byte-exact expectations from spec A §9.3 / §9.4 with defaults substituted.
const wantAwg = `[Unit]
Description=AmneziaWG interface %i (managed by route-agent)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/awg-quick up %i
ExecStop=/usr/local/bin/awg-quick down %i
ExecReload=/bin/sh -c '/usr/local/bin/awg syncconf %i <(/usr/local/bin/awg-quick strip %i)'

[Install]
WantedBy=multi-user.target
`

const wantOlcrtc = `[Unit]
Description=olcrtc-agent-srv instance %i (managed by route-agent)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/olcrtc-agent-srv /etc/olcrtc-agent-srv/%i.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`

func TestUnitBytes(t *testing.T) {
	if got := AwgUnit("/usr/local/bin/awg"); got != wantAwg {
		t.Fatalf("AWG unit differs:\n%s", got)
	}
	if AwgUnit("") != wantAwg {
		t.Fatal("empty AWG_TOOLS_BINARY_PATH must fall back to /usr/local/bin/awg")
	}
	if got := OlcrtcAgentSrvUnit("/usr/local/bin/olcrtc-agent-srv", "/etc/olcrtc-agent-srv"); got != wantOlcrtc {
		t.Fatalf("olcrtc unit differs:\n%s", got)
	}
	if AwgUnitPath("") != "/etc/systemd/system/route-awg@.service" || AwgUnitPath("/x") != "/x" {
		t.Fatal("AwgUnitPath fallback")
	}
}

func TestEnsureUnitIdempotent(t *testing.T) {
	f := fsx.FS{Root: t.TempDir()}
	fake := &runner.Fake{}
	sc := Systemctl{Runner: fake}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := "/etc/systemd/system/route-awg@.service"

	if !EnsureUnit(context.Background(), f, sc, log, "AWG", path, wantAwg) {
		t.Fatal("first call must write")
	}
	data, _ := os.ReadFile(f.Path(path))
	if string(data) != wantAwg {
		t.Fatal("written bytes differ")
	}
	if st, _ := os.Stat(f.Path(path)); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if got := fake.Displays(); !reflect.DeepEqual(got, []string{"systemctl daemon-reload"}) {
		t.Fatalf("calls %v", got)
	}
	if EnsureUnit(context.Background(), f, sc, log, "AWG", path, wantAwg) {
		t.Fatal("second call must not write")
	}
	if len(fake.Calls()) != 1 {
		t.Fatal("second call must not daemon-reload")
	}
	// A changed file is rewritten; a daemon-reload failure is not fatal.
	_ = os.WriteFile(f.Path(path), []byte("old"), 0o644)
	fake.Handler = func(c runner.Cmd) (runner.Result, error) { return runner.FailWith(c, 1, "", "bus down\n") }
	if !EnsureUnit(context.Background(), f, sc, log, "AWG", path, wantAwg) {
		t.Fatal("changed file must be rewritten even if daemon-reload fails")
	}
}

func TestSystemctlArgsAndTimeouts(t *testing.T) {
	fake := &runner.Fake{}
	sc := Systemctl{Runner: fake}
	ctx := context.Background()
	_ = sc.Restart(ctx, "route-awg@awg0")
	_ = sc.Enable(ctx, "a", "b")
	fake.Handler = func(c runner.Cmd) (runner.Result, error) {
		return runner.FailWith(c, 3, "inactive\n", "")
	}
	state, err := sc.IsActive(ctx, "caddy")
	if err != nil || state != "inactive" {
		t.Fatalf("IsActive: %q %v", state, err)
	}
	calls := fake.Calls()
	want := []string{"systemctl restart route-awg@awg0", "systemctl enable a b", "systemctl is-active caddy"}
	if got := fake.Displays(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	for _, c := range calls {
		if c.Timeout != DefaultTimeout || c.Shell {
			t.Fatalf("call %+v: want argv with default timeout", c)
		}
	}
}
