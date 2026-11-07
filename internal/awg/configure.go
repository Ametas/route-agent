package awg

import (
	"context"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/firewall"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

// Defaults (src/config.ts:81, src/utils/awg.ts:12-16).
const (
	DefaultConfigPath = "/etc/amnezia/amneziawg/awg0.conf"
	DefaultIface      = "awg0"
	// NATIface is the interface name the TS agent hard-coded into the iptables rules, the success
	// message, GetAwgObservation and the active-peer count (spec D R5) — kept, see deviations.md.
	NATIface = "awg0"
	// MarkerDir holds the "disabled by the orchestrator" marker the health loop respects.
	MarkerDir = "/etc/route-agent"

	sysctlTimeout   = 10 * time.Second
	iptablesTimeout = 15 * time.Second
	stopTimeout     = 60 * time.Second
	defaultPortText = "51820"
)

// ConfigPath is AWG_CONFIG_PATH with its fallback.
func ConfigPath(cfg *config.Config) string {
	if cfg == nil || cfg.AwgConfigPath == "" {
		return DefaultConfigPath
	}
	return cfg.AwgConfigPath
}

// Iface is getAwgInterfaceName().
func Iface(cfg *config.Config) string {
	if cfg == nil {
		return DefaultIface
	}
	return InterfaceName(cfg.AwgConfigPath, DefaultIface)
}

// DisabledMarker is the file ConfigureAwg(enabled=false) leaves so the health loop does not bring
// the interface back (spec D R3; architecture §6 item 11). ConfigureAwg(enabled=true) removes it.
func DisabledMarker(iface string) string { return MarkerDir + "/awg-" + iface + ".disabled" }

// Service implements ConfigureAwg and GetAwgObservation.
type Service struct {
	Auth      *auth.Verifier
	Runner    runner.Runner
	FS        fsx.FS
	Systemctl systemd.Systemctl
	Cfg       *config.Config
	Log       *slog.Logger
	Firewall  *firewall.Service
	// Locks is the agent-wide area lock registry; ConfigureAwg holds locks.AWG (shared with the
	// AWG binary uploads and the health loop) and nests locks.Firewall for ufw. nil means a
	// registry private to this Service (tests).
	Locks    *locks.Registry
	ownLocks locks.Registry
}

func (s *Service) locks() *locks.Registry {
	if s.Locks != nil {
		return s.Locks
	}
	return &s.ownLocks
}

func (s *Service) applier() Applier {
	tools := ""
	if s.Cfg != nil {
		tools = s.Cfg.AwgToolsBinaryPath
	}
	return Applier{Runner: s.Runner, FS: s.FS, Systemctl: s.Systemctl, Log: s.Log, ToolsPath: tools}
}

func awgResp(ok bool, msg string) *agentpb.AwgConfigResponse {
	return &agentpb.AwgConfigResponse{Success: ok, Message: msg}
}

// RenderConfig builds the awg0.conf text exactly as config.service.ts:599-637 does.
func RenderConfig(req *agentpb.AwgConfigPayload) string {
	var b strings.Builder
	b.WriteString("[Interface]\n")
	if k := Sanitize(req.GetServerPrivateKey()); k != "" {
		b.WriteString("PrivateKey = " + k + "\n")
	}
	if p := req.GetPort(); p != 0 {
		b.WriteString("ListenPort = " + strconv.FormatUint(uint64(p), 10) + "\n")
	}
	if addr := AwgAddress(req); addr != "" {
		b.WriteString("Address = " + addr + "\n")
	}
	Obfuscation{
		Jc: req.GetJc(), Jmin: req.GetJmin(), Jmax: req.GetJmax(),
		S:   [4]string{req.GetS1(), req.GetS2(), req.GetS3(), req.GetS4()},
		H:   [4]string{req.GetH1(), req.GetH2(), req.GetH3(), req.GetH4()},
		I:   [5]string{req.GetI1(), req.GetI2(), req.GetI3(), req.GetI4(), req.GetI5()},
		HPK: req.GetHeaderProtectionKey(),
	}.Render(&b)
	b.WriteString(BoolDirective("RandomTrailers", req.GetRandomTrailers()))
	b.WriteString(BoolDirective("DisableCookies", req.GetDisableCookies()))
	for _, p := range req.GetPeers() {
		if p == nil {
			continue
		}
		pk := Sanitize(p.GetPublicKey())
		if pk == "" {
			continue
		}
		b.WriteString("\n[Peer]\nPublicKey = " + pk + "\n")
		if psk := Sanitize(p.GetPresharedKey()); psk != "" {
			b.WriteString("PresharedKey = " + psk + "\n")
		}
		if ips := Sanitize(p.GetAllowedIps()); ips != "" {
			b.WriteString("AllowedIPs = " + ips + "\n")
		}
	}
	return b.String()
}

// AwgAddress is the Address value: v4, then v6 unless ipv6Mode is "ipv4-only".
func AwgAddress(req *agentpb.AwgConfigPayload) string {
	var addrs []string
	if v4 := Sanitize(req.GetAddressV4()); v4 != "" {
		addrs = append(addrs, v4)
	}
	if v6 := Sanitize(req.GetAddressV6()); v6 != "" && req.GetIpv6Mode() != "ipv4-only" {
		addrs = append(addrs, v6)
	}
	return strings.Join(addrs, ", ")
}

// ConfigureAwg implements the RPC (spec D §1). Auth: metadata only. The work runs under
// locks.AWG and is detached from the client deadline once started (the orchestrator gives up at
// 60 s; a half-applied interface would be worse than finishing).
func (s *Service) ConfigureAwg(ctx context.Context, req *agentpb.AwgConfigPayload) (*agentpb.AwgConfigResponse, error) {
	if !s.Auth.Metadata(ctx) {
		s.Log.Warn("Unauthorized ConfigureAwg request blocked")
		return awgResp(false, auth.InvalidSecretMessage), nil
	}
	held, unlock, err := s.locks().Lock(ctx, locks.AWG)
	if err != nil {
		return awgResp(false, "AmneziaWG configuration error: "+err.Error()), nil
	}
	defer unlock()
	work := context.WithoutCancel(held)
	if !req.GetEnabled() {
		return s.disable(work, req), nil
	}
	return s.enable(work, req), nil
}

func (s *Service) unit() string { return "route-awg@" + Iface(s.Cfg) }

// disable is config.service.ts:542-564 plus `systemctl disable` and the marker (spec D R3).
func (s *Service) disable(ctx context.Context, req *agentpb.AwgConfigPayload) *agentpb.AwgConfigResponse {
	iface := Iface(s.Cfg)
	unit := s.unit()
	// `systemctl stop route-awg@X || systemctl stop awg-quick@X || awg-quick down X || true`
	stops := []runner.Cmd{
		{Name: "systemctl", Args: []string{"stop", unit}, Timeout: stopTimeout},
		{Name: "systemctl", Args: []string{"stop", "awg-quick@" + iface}, Timeout: stopTimeout},
		{Name: "awg-quick", Args: []string{"down", iface}, Timeout: stopTimeout},
	}
	for _, c := range stops {
		if _, err := s.Runner.Run(ctx, c); err == nil {
			break
		}
	}
	if err := s.Systemctl.Disable(ctx, unit); err != nil {
		s.Log.Warn("Failed to disable AWG service "+unit, "err", CmdText(err, "systemctl disable error"))
	}
	marker := DisabledMarker(iface)
	if err := s.FS.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), fsx.WriteOptions{Mode: 0o644}); err != nil {
		s.Log.Warn("Failed to record the AWG disabled marker; the health loop may bring the interface back", "path", marker, "err", err.Error())
	}
	if port := req.GetPort(); port != 0 && s.Firewall != nil && s.Firewall.UfwInstalled() {
		s.Firewall.CloseUDPAndReload(ctx, port)
	}
	return awgResp(true, "AmneziaWG (AWG3) service successfully disabled and stopped.")
}

var defaultRouteDev = regexp.MustCompile(`dev\s+(\S+)`)

// enable is config.service.ts:566-724 with the explicit apply decision (Applier).
func (s *Service) enable(ctx context.Context, req *agentpb.AwgConfigPayload) *agentpb.AwgConfigResponse {
	iface := Iface(s.Cfg)
	unit := s.unit()
	path := ConfigPath(s.Cfg)

	if _, err := s.sysctl(ctx, "net.ipv4.ip_forward=1"); err != nil {
		s.Log.Warn("Failed to set sysctl net.ipv4.ip_forward=1", "err", err.Error())
	}
	if m := req.GetIpv6Mode(); m == "dual-stack" || m == "trap-ipv6" {
		if _, err := s.sysctl(ctx, "net.ipv6.conf.all.forwarding=1"); err != nil {
			s.Log.Debug("IPv6 disabled in kernel; skipping net.ipv6.conf.all.forwarding sysctl setting", "err", err.Error())
		}
	}

	text := RenderConfig(req)
	prev, prevErr := s.FS.ReadFile(path)
	// The private key is in this file: 0600 and atomic (architecture §6 item 3; TS wrote 0644 in place).
	if err := s.FS.WriteFile(path, []byte(text), fsx.WriteOptions{Mode: 0o600}); err != nil {
		msg := fsx.NodeError("open", path, err)
		s.Log.Error("Failed to configure AmneziaWG service", "err", msg)
		return awgResp(false, "AmneziaWG configuration error: "+msg)
	}
	if err := os.Remove(s.FS.Path(DisabledMarker(iface))); err != nil && !os.IsNotExist(err) {
		s.Log.Warn("Failed to remove the AWG disabled marker", "err", err.Error())
	}

	a := s.applier()
	live := Probe(ctx, s.Runner, iface)
	if !live.Exists {
		if err := s.Systemctl.Start(ctx, unit); err != nil {
			msg := CmdText(err, "systemctl start error")
			s.Log.Error("Failed to start AWG service "+unit, "err", msg)
			return awgResp(false, "Failed to start AWG service ("+unit+"): "+msg)
		}
	} else {
		restart := prevErr != nil || InterfaceSection(string(prev)) != InterfaceSection(text) ||
			!AddressesMatch(live.Addrs, AwgAddress(req)) || !a.UnitActive(ctx, unit)
		if !restart {
			if err := a.Syncconf(ctx, iface, path); err != nil {
				s.Log.Warn("Failed awg syncconf for "+unit+", attempting restart", "err", CmdText(err, "awg syncconf error"))
				restart = true
			}
		} else {
			s.Log.Info("AWG interface identity changed (or unit not active); restarting " + unit)
		}
		if restart {
			if err := s.Systemctl.Restart(ctx, unit); err != nil {
				msg := CmdText(err, "systemctl restart error")
				s.Log.Error("Failed to restart AWG service "+unit, "err", msg)
				return awgResp(false, "Failed to reload or restart AWG service ("+unit+"): "+msg)
			}
		}
	}

	if err := s.Systemctl.Enable(ctx, unit); err != nil {
		s.Log.Warn("Failed to enable AWG service "+unit+" for auto-start", "err", err.Error())
	}
	s.nat(ctx, req.GetIpv6Mode())
	if port := req.GetPort(); port != 0 && s.Firewall != nil && s.Firewall.UfwInstalled() {
		if err := s.Firewall.OpenUDPAndReload(ctx, port); err != nil {
			s.Log.Warn("Failed to configure UFW port for AmneziaWG", "err", err.Error())
		}
	}

	portText := defaultPortText
	if p := req.GetPort(); p != 0 {
		portText = strconv.FormatUint(uint64(p), 10)
	}
	return awgResp(true, "AmneziaWG (AWG3) interface "+NATIface+" configured and active on port "+portText+".")
}

func (s *Service) sysctl(ctx context.Context, kv string) (runner.Result, error) {
	return s.Runner.Run(ctx, runner.Cmd{Name: "sysctl", Args: []string{"-w", kv}, Timeout: sysctlTimeout})
}

// ensureRule is `<bin> [-t table] -C <rule> || <bin> [-t table] -A <rule>`.
func (s *Service) ensureRule(ctx context.Context, bin string, table []string, rule ...string) error {
	check := append(append(append([]string{}, table...), "-C"), rule...)
	if _, err := s.Runner.Run(ctx, runner.Cmd{Name: bin, Args: check, Timeout: iptablesTimeout}); err == nil {
		return nil
	}
	add := append(append(append([]string{}, table...), "-A"), rule...)
	_, err := s.Runner.Run(ctx, runner.Cmd{Name: bin, Args: add, Timeout: iptablesTimeout})
	return err
}

// nat is config.service.ts:682-708: FORWARD accept and MASQUERADE on the default-route interface;
// for trap-ipv6 an ip6tables REJECT. Errors never fail the RPC. The interface stays hard-coded to
// awg0 (spec D R5).
func (s *Service) nat(ctx context.Context, ipv6Mode string) {
	if err := s.ensureRule(ctx, "iptables", nil, "FORWARD", "-i", NATIface, "-j", "ACCEPT"); err != nil {
		s.Log.Warn("Failed to configure iptables NAT rules for awg0", "err", err.Error())
	} else if res, err := s.Runner.Run(ctx, runner.Cmd{Name: "ip", Args: []string{"route", "show", "default"}, Timeout: IPTimeout}); err == nil {
		if m := defaultRouteDev.FindStringSubmatch(res.Stdout); m != nil {
			_ = s.ensureRule(ctx, "iptables", []string{"-t", "nat"}, "POSTROUTING", "-o", m[1], "-j", "MASQUERADE")
		}
	}
	if ipv6Mode == "trap-ipv6" {
		if err := s.ensureRule(ctx, "ip6tables", nil, "FORWARD", "-i", NATIface, "-j", "REJECT", "--reject-with", "icmp6-adm-prohibited"); err != nil {
			s.Log.Debug("IPv6/ip6tables disabled in kernel or not available; skipping trap-ipv6 rules", "err", err.Error())
		}
	}
}
