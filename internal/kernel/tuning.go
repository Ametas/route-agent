// Package kernel applies the network sysctl profile at boot (docs/go-rewrite/spec/d-awg-mesh-firewall-shaping.md
// §10, spec A §10.5; src/utils/kernelTuning.ts): written to /etc/sysctl.d so it survives a reboot,
// and applied key by key with `sysctl -w`, then read back with `sysctl -n`.
package kernel

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
)

// DefaultProfilePath is SYSCTL_PROFILE_PATH's default (src/config.ts:115).
const DefaultProfilePath = "/etc/sysctl.d/99-route-agent.conf"

const sysctlTimeout = 10 * time.Second

// Entry is one profile key with the reason written above it in the file.
type Entry struct {
	Key, Value, Why string
}

// Profile is KERNEL_TUNING (kernelTuning.ts:79-110), in application order. Deliberately absent
// (tests/kernelTuning.test.ts): udp_mem/tcp_rmem/tcp_wmem (RAM-derived), netdev_max_backlog
// (softnet drops were zero), ip_local_port_range (would swallow the fixed service ports).
var Profile = []Entry{
	{"net.core.rmem_default", "1048576", "ГЛАВНАЯ строка: буфер сокетов, которые его не запрашивают. Именно они теряют пакеты (по 500-780 тысяч на сокет), сидя на умолчании 212992 при уже поднятом потолке"},
	{"net.core.wmem_default", "1048576", "то же для отправки: SndbufErrors на нагруженной ноде 316 тысяч"},
	{"net.core.rmem_max", "7500000", "потолок для тех, кто буфер запрашивает явно. Значение из вики quic-go; сам по себе теряющие сокеты не спасает — они до потолка не тянутся"},
	{"net.core.wmem_max", "7500000", "то же для отправки"},
	{"net.ipv4.tcp_congestion_control", "bbr", "на нетюненой ноде cubic; bbr доступен в ядре (tcp_available_congestion_control: reno cubic bbr)"},
	{"net.core.default_qdisc", "fq", "пара к bbr: пейсинг делает очередь, а не ядро. На нетюненой ноде fq_codel. Действует на вновь создаваемые qdisc, то есть с перезагрузки"},
}

// RenderSysctlFile is renderSysctlFile: a header, then "# <why>\n<key> = <value>" blocks joined
// by a blank line, trailing newline. Byte-equal to TS (golden SHA in the test), so the first Go
// start does not rewrite the file.
func RenderSysctlFile(entries []Entry) string {
	blocks := make([]string, len(entries))
	for i, e := range entries {
		blocks[i] = "# " + e.Why + "\n" + e.Key + " = " + e.Value
	}
	return "# Управляется route-agent, правки будут перезаписаны при следующем старте агента.\n\n" + strings.Join(blocks, "\n\n") + "\n"
}

// KeyResult is one key's outcome.
type KeyResult struct {
	Key     string
	Applied bool
	// Effective is what the kernel reports after the write; "" with EffectiveKnown false when the
	// read-back failed (that does not make the apply a failure).
	Effective      string
	EffectiveKnown bool
	Error          string
}

// Result is the outcome of Apply.
type Result struct {
	FileChanged bool
	FileError   string
	Keys        []KeyResult
}

// Tuning applies the profile.
type Tuning struct {
	Runner runner.Runner
	FS     fsx.FS
	Log    *slog.Logger
	// ProfilePath is SYSCTL_PROFILE_PATH ("" → DefaultProfilePath).
	ProfilePath string
	// Entries overrides Profile (tests).
	Entries []Entry
}

// Apply is applyKernelTuning. Per key, not `sysctl -p`: on OpenVZ and in containers some keys are
// read-only and one of them must not take the others down; a failure is a warning, never fatal.
// A failed write of the profile file is logged and the keys are still applied (TS let the
// rejection escape a `void` call, which crashes Node — spec D R14).
func (t *Tuning) Apply(ctx context.Context) Result {
	entries := t.Entries
	if entries == nil {
		entries = Profile
	}
	path := t.ProfilePath
	if path == "" {
		path = DefaultProfilePath
	}
	var res Result
	changed, err := t.FS.WriteIfChanged(path, []byte(RenderSysctlFile(entries)), fsx.WriteOptions{})
	if err != nil {
		res.FileError = err.Error()
		t.Log.Warn("Kernel tuning: sysctl profile could not be written", "path", path, "err", err.Error())
	} else if changed {
		res.FileChanged = true
		t.Log.Info("Kernel tuning: sysctl profile written", "path", path)
	}

	var failed []string
	for _, e := range entries {
		if _, err := t.Runner.Run(ctx, runner.Cmd{Name: "sysctl", Args: []string{"-w", e.Key + "=" + e.Value}, Timeout: sysctlTimeout}); err != nil {
			t.Log.Warn("Kernel tuning: sysctl key could not be set", "key", e.Key, "err", err.Error())
			res.Keys = append(res.Keys, KeyResult{Key: e.Key, Error: err.Error()})
			failed = append(failed, e.Key)
			continue
		}
		kr := KeyResult{Key: e.Key, Applied: true}
		if out, err := t.Runner.Run(ctx, runner.Cmd{Name: "sysctl", Args: []string{"-n", e.Key}, Timeout: sysctlTimeout}); err == nil {
			kr.Effective = strings.Join(strings.Fields(out.Stdout), " ")
			kr.EffectiveKnown = true
		}
		res.Keys = append(res.Keys, kr)
	}
	t.Log.Info("Kernel tuning: network buffer profile applied",
		"fileChanged", res.FileChanged, "applied", len(entries)-len(failed), "failed", failed)
	return res
}

// Run is Apply for lifecycle.Group.Go (result only logged, as in TS).
func (t *Tuning) Run(ctx context.Context) { t.Apply(ctx) }
