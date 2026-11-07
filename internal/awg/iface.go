package awg

import (
	"context"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/systemd"
)

// Command timeouts (spec D §11: TS had none here; architecture §2).
const (
	IPTimeout       = 10 * time.Second
	StripTimeout    = 15 * time.Second
	SyncconfTimeout = 30 * time.Second
	ShowTimeout     = 10 * time.Second
)

// DefaultToolsPath is the AWG_TOOLS_BINARY_PATH fallback; awg-quick is derived as <tools>-quick,
// exactly as the route-awg@.service template derives it (systemd.AwgUnit).
const DefaultToolsPath = "/usr/local/bin/awg"

// Live is what `ip -o addr show dev <iface>` reports about an interface.
type Live struct {
	Exists bool
	// Addrs are the global-scope inet/inet6 addresses with their prefix length, normalized.
	Addrs []string
	// FirstV4 is the first inet address without the mask ("" when none) — the TS mesh check
	// `/inet\s+(\d+\.\d+\.\d+\.\d+)\//`.
	FirstV4 string
}

// Probe runs `ip -o addr show dev <iface>`. A failure (no such device) is "does not exist".
func Probe(ctx context.Context, r runner.Runner, iface string) Live {
	res, err := r.Run(ctx, runner.Cmd{Name: "ip", Args: []string{"-o", "addr", "show", "dev", iface}, Timeout: IPTimeout})
	if err != nil {
		return Live{}
	}
	live := Live{Exists: true}
	for _, line := range strings.Split(res.Stdout, "\n") {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] != "inet" && f[i] != "inet6" {
				continue
			}
			if strings.Contains(line, " scope link") || strings.Contains(line, " scope host") {
				break
			}
			if f[i] == "inet" && live.FirstV4 == "" {
				live.FirstV4, _, _ = strings.Cut(f[i+1], "/")
			}
			if p, ok := normPrefix(f[i+1]); ok {
				live.Addrs = append(live.Addrs, p)
			}
			break
		}
	}
	return live
}

// normPrefix normalizes "addr/len" (or a bare address, which awg-quick treats as a host route).
func normPrefix(s string) (string, bool) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.String(), true
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return netip.PrefixFrom(a, a.BitLen()).String(), true
	}
	return "", false
}

// AddressesMatch reports whether the live addresses are exactly the Address directive's list
// (comma-separated, as written to the config). An unparsable entry never matches.
func AddressesMatch(live []string, address string) bool {
	var want []string
	for _, part := range strings.Split(address, ",") {
		part = jsjson.Trim(part)
		if part == "" {
			continue
		}
		p, ok := normPrefix(part)
		if !ok {
			return false
		}
		want = append(want, p)
	}
	got := slices.Clone(live)
	slices.Sort(want)
	slices.Sort(got)
	return slices.Equal(slices.Compact(want), slices.Compact(got))
}

// ReadMTU reads /sys/class/net/<iface>/mtu; ok is false when unreadable or not a number.
func ReadMTU(f fsx.FS, iface string) (int, bool) {
	raw, err := f.ReadFile("/sys/class/net/" + iface + "/mtu")
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(jsjson.Trim(string(raw)))
	if err != nil {
		return 0, false
	}
	return n, true
}

// Applier brings a running route-awg@<iface> unit to a config already written to disk.
//
// Why not `systemctl reload` (spec D R1, architecture §6 item 5): the unit's ExecReload is
// `/bin/sh -c 'awg syncconf %i <(awg-quick strip %i)'`, bash process substitution under dash —
// it can never succeed on Debian/Ubuntu, so every TS "reload" fell back to a restart (dropping every
// session), and that accidental restart is the only reason ConfigureAwg applied Address changes at
// all. The unit text stays byte-exact (internal/systemd); the agent no longer calls reload.
// Instead the callers decide explicitly: an interface whose own section changed (address, port,
// MTU, keys, mimicry), or whose live addresses differ from the file, is restarted; a pure peer-list
// change is applied live with `awg-quick strip <conf>` → temp file (0600) → `awg syncconf`.
// A failed syncconf falls back to a restart, exactly where the TS reload fell back.
type Applier struct {
	Runner    runner.Runner
	FS        fsx.FS
	Systemctl systemd.Systemctl
	Log       *slog.Logger
	// ToolsPath is AWG_TOOLS_BINARY_PATH ("" → DefaultToolsPath).
	ToolsPath string
}

func (a Applier) tools() string {
	if a.ToolsPath == "" {
		return DefaultToolsPath
	}
	return a.ToolsPath
}

// Syncconf applies the peer section of confPath to the live interface without taking it down.
func (a Applier) Syncconf(ctx context.Context, iface, confPath string) error {
	res, err := a.Runner.Run(ctx, runner.Cmd{Name: a.tools() + "-quick", Args: []string{"strip", confPath}, Timeout: StripTimeout})
	if err != nil {
		return err
	}
	dir := a.FS.Path(filepath.Dir(confPath))
	tmp, err := os.CreateTemp(dir, "."+iface+".syncconf-*") // 0600: the stripped text holds the private key
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(res.Stdout); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_, err = a.Runner.Run(ctx, runner.Cmd{Name: a.tools(), Args: []string{"syncconf", iface, name}, Timeout: SyncconfTimeout})
	return err
}

// UnitActive reports `systemctl is-active <unit>` == active. A live interface whose unit is not
// active (brought up by something else) gets a restart, as the TS reload of an inactive unit did.
func (a Applier) UnitActive(ctx context.Context, unit string) bool {
	st, err := a.Systemctl.IsActive(ctx, unit)
	return err == nil && st == "active"
}
