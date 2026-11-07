// Package mesh implements the server-to-server AWG3 mesh tunnel of group D
// (docs/go-rewrite/spec/d-awg-mesh-firewall-shaping.md §7, §4.3–§4.4): the ConfigureMeshTunnel RPC
// and the telemetry readers for the mesh (kernel module status, interface status with live peer
// keys). The DKMS build (UploadMeshAwgKernelSource) lives in internal/binaries/dkms; the mesh is
// deliberately never self-healed (telemetry.ts:343-347).
package mesh

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/awg"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/firewall"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

// Defaults (src/config.ts, src/utils/awg.ts:22-26).
const (
	DefaultConfigPath = "/etc/amnezia/amneziawg/awgmesh0.conf"
	DefaultIface      = "awgmesh0"
)

// ConfigPath is MESH_AWG_CONFIG_PATH with its fallback.
func ConfigPath(cfg *config.Config) string {
	if cfg == nil || cfg.MeshAwgConfigPath == "" {
		return DefaultConfigPath
	}
	return cfg.MeshAwgConfigPath
}

// Iface is getMeshAwgInterfaceName().
func Iface(cfg *config.Config) string {
	if cfg == nil {
		return DefaultIface
	}
	return awg.InterfaceName(cfg.MeshAwgConfigPath, DefaultIface)
}

// Service implements ConfigureMeshTunnel.
type Service struct {
	Auth      *auth.Verifier
	Runner    runner.Runner
	FS        fsx.FS
	Systemctl systemd.Systemctl
	Cfg       *config.Config
	Log       *slog.Logger
	Firewall  *firewall.Service
	// Locks: ConfigureMeshTunnel holds locks.Mesh (shared with the kernel-module upload) and nests
	// locks.Firewall for the UDP port. nil means a private registry (tests).
	Locks    *locks.Registry
	ownLocks locks.Registry
}

func (s *Service) locks() *locks.Registry {
	if s.Locks != nil {
		return s.Locks
	}
	return &s.ownLocks
}

func resp(ok bool, msg string) *agentpb.MeshTunnelConfigResponse {
	return &agentpb.MeshTunnelConfigResponse{Success: ok, Message: msg}
}

// Address is the mesh Address value: v4 and v6, each if non-empty (no ipv6Mode filter here).
func Address(req *agentpb.MeshTunnelConfigPayload) string {
	var addrs []string
	if v4 := awg.Sanitize(req.GetAddressV4()); v4 != "" {
		addrs = append(addrs, v4)
	}
	if v6 := awg.Sanitize(req.GetAddressV6()); v6 != "" {
		addrs = append(addrs, v6)
	}
	return strings.Join(addrs, ", ")
}

// RenderConfig builds awgmesh0.conf exactly as meshTunnel.service.ts:395-468 does and returns the
// number of [Peer] blocks written. The MTU is a uint32 on the wire, so the TS "positive finite,
// truncated" rule reduces to "non-zero".
func RenderConfig(req *agentpb.MeshTunnelConfigPayload) (string, int) {
	var b strings.Builder
	b.WriteString("[Interface]\n")
	if k := awg.Sanitize(req.GetPrivateKey()); k != "" {
		b.WriteString("PrivateKey = " + k + "\n")
	}
	if p := req.GetListenPort(); p != 0 {
		b.WriteString("ListenPort = " + strconv.FormatUint(uint64(p), 10) + "\n")
	}
	if addr := Address(req); addr != "" {
		b.WriteString("Address = " + addr + "\n")
	}
	if mtu := req.GetMtu(); mtu != 0 {
		b.WriteString("MTU = " + strconv.FormatUint(uint64(mtu), 10) + "\n")
	}
	awg.Obfuscation{
		Jc: req.GetJc(), Jmin: req.GetJmin(), Jmax: req.GetJmax(),
		S:   [4]string{req.GetS1(), req.GetS2(), req.GetS3(), req.GetS4()},
		H:   [4]string{req.GetH1(), req.GetH2(), req.GetH3(), req.GetH4()},
		I:   [5]string{req.GetI1(), req.GetI2(), req.GetI3(), req.GetI4(), req.GetI5()},
		HPK: req.GetHeaderProtectionKey(),
	}.Render(&b)
	peers := 0
	for _, p := range req.GetPeers() {
		if p == nil {
			continue
		}
		pk := awg.Sanitize(p.GetPublicKey())
		if pk == "" {
			continue
		}
		b.WriteString("\n[Peer]\nPublicKey = " + pk + "\n")
		if ips := awg.Sanitize(p.GetAllowedIps()); ips != "" {
			b.WriteString("AllowedIPs = " + ips + "\n")
		}
		if ep := awg.Sanitize(p.GetEndpoint()); ep != "" {
			b.WriteString("Endpoint = " + ep + "\n")
		}
		if ka := p.GetPersistentKeepalive(); ka != 0 {
			b.WriteString("PersistentKeepalive = " + strconv.FormatUint(uint64(ka), 10) + "\n")
		}
		peers++
	}
	return b.String(), peers
}

// ConfigureMeshTunnel implements the RPC (spec D §7). Auth: metadata only (the payload's
// orchestrator_secret is ignored, as in TS). Runs under locks.Mesh, detached from the client
// deadline once started.
func (s *Service) ConfigureMeshTunnel(ctx context.Context, req *agentpb.MeshTunnelConfigPayload) (*agentpb.MeshTunnelConfigResponse, error) {
	if !s.Auth.Metadata(ctx) {
		s.Log.Warn("Unauthorized ConfigureMeshTunnel request blocked")
		return resp(false, auth.InvalidSecretMessage), nil
	}
	held, unlock, err := s.locks().Lock(ctx, locks.Mesh)
	if err != nil {
		return resp(false, "Mesh tunnel configuration error: "+err.Error()), nil
	}
	defer unlock()
	return s.configure(context.WithoutCancel(held), req), nil
}

func (s *Service) configure(ctx context.Context, req *agentpb.MeshTunnelConfigPayload) *agentpb.MeshTunnelConfigResponse {
	path := ConfigPath(s.Cfg)
	iface := Iface(s.Cfg)
	unit := "route-awg@" + iface

	text, peers := RenderConfig(req)
	prev, prevErr := s.FS.ReadFile(path)
	// Private key inside: 0600, atomic (architecture §6 item 3).
	if err := s.FS.WriteFile(path, []byte(text), fsx.WriteOptions{Mode: 0o600}); err != nil {
		msg := fsx.NodeError("open", path, err)
		s.Log.Error("Failed to configure mesh AWG tunnel", "err", msg)
		return resp(false, "Mesh tunnel configuration error: "+msg)
	}

	if s.Firewall != nil {
		s.Firewall.AllowMeshUDPPort(ctx, req.GetListenPort())
	}

	tools := ""
	if s.Cfg != nil {
		tools = s.Cfg.AwgToolsBinaryPath
	}
	a := awg.Applier{Runner: s.Runner, FS: s.FS, Systemctl: s.Systemctl, Log: s.Log, ToolsPath: tools}

	// Identity check (meshTunnel.service.ts:476-516): an existing interface whose IPv4 and (when one
	// is requested) MTU already match may be resynced live; anything else needs a full restart,
	// because awg-quick strip drops Address/MTU before syncconf. Go adds the full address set
	// (IPv6 included, spec D R9), the whole [Interface] section against the previous file (an MTU
	// that was removed, a new port or key) and an active unit.
	live := awg.Probe(ctx, s.Runner, iface)
	newV4, _, _ := strings.Cut(awg.Sanitize(req.GetAddressV4()), "/")
	identityUnchanged := live.Exists && live.FirstV4 != "" && live.FirstV4 == newV4
	if identityUnchanged && req.GetMtu() != 0 {
		mtu, ok := awg.ReadMTU(s.FS, iface)
		identityUnchanged = ok && mtu == int(req.GetMtu())
	}
	identityUnchanged = identityUnchanged && prevErr == nil &&
		awg.InterfaceSection(string(prev)) == awg.InterfaceSection(text) &&
		awg.AddressesMatch(live.Addrs, Address(req)) && a.UnitActive(ctx, unit)

	if identityUnchanged {
		if err := a.Syncconf(ctx, iface, path); err != nil {
			s.Log.Warn("Failed awg syncconf for "+unit+", attempting restart", "err", awg.CmdText(err, "awg syncconf error"))
			if err := s.Systemctl.Restart(ctx, unit); err != nil {
				msg := awg.CmdText(err, "systemctl restart error")
				s.Log.Error("Failed to restart mesh AWG service "+unit, "err", msg)
				return resp(false, "Failed to reload or restart mesh tunnel service ("+unit+"): "+msg)
			}
		}
	} else if err := s.Systemctl.Restart(ctx, unit); err != nil {
		msg := awg.CmdText(err, "systemctl restart error")
		s.Log.Error("Failed to (re)start mesh AWG service "+unit+" after identity change", "err", msg)
		return resp(false, "Failed to apply new tunnel identity to mesh service ("+unit+"): "+msg)
	}

	if err := s.Systemctl.Enable(ctx, unit); err != nil {
		s.Log.Warn("Failed to enable mesh AWG service "+unit+" for auto-start", "err", err.Error())
	}
	return resp(true, "Mesh AWG tunnel interface "+iface+" configured with "+strconv.Itoa(peers)+" peer(s)")
}
