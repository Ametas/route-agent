// Package firewall implements group D's ufw parts (docs/go-rewrite/spec/d-awg-mesh-firewall-shaping.md
// §8, §7.3 step 1): the ManageFirewall RPC, the mesh UDP port, and the ufw open/close steps of
// ConfigureAwg. The egress port sync run by ApplyConfig lives with its caller (internal/singbox).
//
// Every ufw command runs as `sudo ufw …` (argv, no shell — the TS strings had sudo although the
// agent is root, and the unit keeps working with or without the sudoers drop-in) with a timeout,
// one at a time under locks.Firewall: ufw has no robust locking of its rule files (spec D R12).
package firewall

import (
	"context"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
)

// UfwTimeout bounds one ufw command (the same bound ApplyConfig's sync uses).
const UfwTimeout = 30 * time.Second

// Service implements ManageFirewall and the ufw helpers used by ConfigureAwg/ConfigureMeshTunnel.
type Service struct {
	Auth   *auth.Verifier
	Runner runner.Runner
	Log    *slog.Logger
	// LookPath finds ufw (`command -v ufw` in TS); nil means exec.LookPath.
	LookPath func(string) (string, error)
	// Locks is the agent-wide registry (locks.Firewall, shared with ApplyConfig's egress sync).
	// nil means a registry private to this Service (tests).
	Locks    *locks.Registry
	ownLocks locks.Registry
}

func (s *Service) locks() *locks.Registry {
	if s.Locks != nil {
		return s.Locks
	}
	return &s.ownLocks
}

// UfwInstalled is isUfwInstalled (src/utils/firewall.ts:23-30) without a shell.
func (s *Service) UfwInstalled() bool {
	look := s.LookPath
	if look == nil {
		look = exec.LookPath
	}
	_, err := look("ufw")
	return err == nil
}

func (s *Service) ufw(ctx context.Context, args ...string) (runner.Result, error) {
	return s.Runner.Run(ctx, runner.Cmd{Name: "sudo", Args: append([]string{"ufw"}, args...), Timeout: UfwTimeout})
}

// lock takes locks.Firewall; a failure (cancelled while waiting, out of order) is logged and the
// step skipped — none of the ufw steps ever failed their RPC in TS.
func (s *Service) lock(ctx context.Context) (context.Context, func(), bool) {
	held, unlock, err := s.locks().Lock(ctx, locks.Firewall)
	if err != nil {
		s.Log.Warn("Firewall step skipped: could not take the firewall lock", "err", err.Error())
		return ctx, func() {}, false
	}
	return held, unlock, true
}

// OpenUDPAndReload is ConfigureAwg's `sudo ufw allow <port>/udp` then `sudo ufw reload`; the
// reload is skipped when the allow failed (they shared one try block). The error is returned for
// the caller's warning.
func (s *Service) OpenUDPAndReload(ctx context.Context, port uint32) error {
	held, unlock, ok := s.lock(ctx)
	if !ok {
		return nil
	}
	defer unlock()
	if _, err := s.ufw(held, "allow", strconv.FormatUint(uint64(port), 10)+"/udp"); err != nil {
		return err
	}
	_, err := s.ufw(held, "reload")
	return err
}

// CloseUDPAndReload is the disable path's `sudo ufw delete allow <port>/udp || true` then
// `sudo ufw reload || true`: both errors ignored.
func (s *Service) CloseUDPAndReload(ctx context.Context, port uint32) {
	held, unlock, ok := s.lock(ctx)
	if !ok {
		return
	}
	defer unlock()
	_, _ = s.ufw(held, "delete", "allow", strconv.FormatUint(uint64(port), 10)+"/udp")
	_, _ = s.ufw(held, "reload")
}

// AllowMeshUDPPort is ensureMeshUdpPortAllowed (firewall.ts:44-56): only a port in 1..65535 and
// only with ufw installed; `sudo ufw allow <port>/udp`, no reload, not added to the egress
// active-ports cache (so the egress sync never closes it). Errors are warnings.
func (s *Service) AllowMeshUDPPort(ctx context.Context, port uint32) {
	if port < 1 || port > 65535 || !s.UfwInstalled() {
		return
	}
	held, unlock, ok := s.lock(ctx)
	if !ok {
		return
	}
	defer unlock()
	res, err := s.ufw(held, "allow", strconv.FormatUint(uint64(port), 10)+"/udp")
	if err != nil {
		s.Log.Warn("Failed to allow mesh tunnel UDP port in ufw", "port", port, "stderr", runner.StderrOrMessage(err))
		return
	}
	s.Log.Info("Mesh tunnel UDP port allowed in ufw", "port", port, "stdout", strings.TrimSpace(res.Stdout))
}

func validPorts(raw []uint32) []uint32 {
	out := make([]uint32, 0, len(raw))
	for _, p := range raw {
		if p > 0 && p <= 65535 {
			out = append(out, p)
		}
	}
	return out
}

// ManageFirewall implements the RPC (spec D §8; no orchestrator caller today, kept for the
// contract). Auth: metadata only. Opens every valid UDP then TCP port one by one (errors logged),
// reloads, and always reports the requested counts. There is no ufw-installed check, as in TS.
// The NODE_ENV=test "[TEST] Processed firewall ports…" answer is a TS test seam, not ported: Go
// tests substitute the runner instead.
func (s *Service) ManageFirewall(ctx context.Context, req *agentpb.FirewallPayload) (*agentpb.FirewallResponse, error) {
	if !s.Auth.Metadata(ctx) {
		s.Log.Warn("Unauthorized ManageFirewall request blocked")
		return &agentpb.FirewallResponse{Success: false, Message: auth.InvalidSecretMessage}, nil
	}
	udp := validPorts(req.GetOpenUdpPorts())
	tcp := validPorts(req.GetOpenTcpPorts())

	held, unlock, err := s.locks().Lock(ctx, locks.Firewall)
	if err != nil {
		return &agentpb.FirewallResponse{Success: false, Message: "ManageFirewall error: " + err.Error()}, nil
	}
	defer unlock()
	work := context.WithoutCancel(held)
	for _, p := range udp {
		if _, err := s.ufw(work, "allow", strconv.FormatUint(uint64(p), 10)+"/udp"); err != nil {
			s.Log.Error("Failed to open UDP port in firewall", "port", p, "err", err.Error())
		}
	}
	for _, p := range tcp {
		if _, err := s.ufw(work, "allow", strconv.FormatUint(uint64(p), 10)+"/tcp"); err != nil {
			s.Log.Error("Failed to open TCP port in firewall", "port", p, "err", err.Error())
		}
	}
	if _, err := s.ufw(work, "reload"); err != nil {
		s.Log.Warn("Failed to reload UFW", "err", err.Error())
	}
	return &agentpb.FirewallResponse{
		Success: true,
		Message: "Successfully updated firewall rules: opened " + strconv.Itoa(len(udp)) + " UDP and " + strconv.Itoa(len(tcp)) + " TCP ports.",
	}, nil
}
