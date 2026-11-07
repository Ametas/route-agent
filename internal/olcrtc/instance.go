package olcrtc

import (
	"context"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/grpc/status"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
)

const msgBadName = "Invalid or missing user_short_uuid."

// configMode: the instance YAML carries the tunnel's crypto key (spec F R4: TS wrote it 0644,
// in place). The instances run as root.
const configMode = 0o600

// safeInstanceName is isSafeInstanceName: the name becomes a unit instance and a file name.
var safeInstanceName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Sanitize is sanitizeConfigInput for strings: CR and LF removed, then JS trim.
func Sanitize(v string) string {
	return jsjson.Trim(strings.NewReplacer("\r", "", "\n", "").Replace(v))
}

// ConfigPath is <OLCRTC_AGENT_SRV_CONFIG_DIR>/<u>.yaml.
func (s *Service) ConfigPath(u string) string {
	return path.Join(s.Cfg.OlcrtcAgentSrvConfigDir, u+".yaml")
}

// UnitName is olcrtc-agent-srv@<u> (no .service).
func UnitName(u string) string { return "olcrtc-agent-srv@" + u }

// plainScalar: values written unquoted by TS that are safe as YAML plain scalars stay unquoted, so
// every config the orchestrator generates today is byte-identical.
var plainScalar = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// quoted is the inside of a YAML double-quoted scalar. TS interpolated the raw value between
// quotes (spec F R10): a `"` or `\` broke the scalar. Ordinary values are unchanged.
func quoted(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case r < 0x20 && r != '\t', r == 0x7F:
			b.WriteString(`\x`)
			b.WriteString(strconv.FormatInt(int64(r)>>4, 16))
			b.WriteString(strconv.FormatInt(int64(r)&0xF, 16))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// plain writes a value TS left unquoted: as is when empty (TS wrote `provider: `) or a safe plain
// scalar, double-quoted otherwise.
func plain(v string) string {
	if v == "" || plainScalar.MatchString(v) {
		return v
	}
	return `"` + quoted(v) + `"`
}

// int32Text is `Number(v) | 0` (ToInt32: a uint32 above 2^31-1 wraps negative, as in TS).
func int32Text(v uint32) string { return strconv.FormatInt(int64(int32(v)), 10) }

// BuildYAML is buildInstanceYaml (olcrtc.service.ts:50-89), byte for byte for ordinary values:
// contentChanged decides between restart and start.
func BuildYAML(p *agentpb.OlcrtcInstanceConfigPayload) string {
	carrier := Sanitize(p.GetCarrier())
	transport := Sanitize(p.GetTransport())
	dns := Sanitize(p.GetDns())
	if dns == "" {
		dns = "8.8.8.8:53"
	}
	var b strings.Builder
	b.WriteString("mode: srv\n")
	b.WriteString("auth:\n")
	b.WriteString("  provider: " + plain(carrier) + "\n")
	b.WriteString("room:\n")
	b.WriteString(`  id: "` + quoted(Sanitize(p.GetRoomId())) + "\"\n")
	b.WriteString("crypto:\n")
	b.WriteString(`  key: "` + quoted(Sanitize(p.GetCryptoKeyHex())) + "\"\n")
	b.WriteString("net:\n")
	b.WriteString("  transport: " + plain(transport) + "\n")
	b.WriteString(`  dns: "` + quoted(dns) + "\"\n")

	if transport == "vp8channel" && (p.GetVp8Fps() != 0 || p.GetVp8BatchSize() != 0) {
		b.WriteString("vp8:\n")
		if v := p.GetVp8Fps(); v != 0 {
			b.WriteString("  fps: " + int32Text(v) + "\n")
		}
		if v := p.GetVp8BatchSize(); v != 0 {
			b.WriteString("  batch_size: " + int32Text(v) + "\n")
		}
	}
	if transport == "seichannel" && (p.GetSeiFps() != 0 || p.GetSeiBatchSize() != 0 || p.GetSeiFragmentSize() != 0 || p.GetSeiAckTimeoutMs() != 0) {
		b.WriteString("sei:\n")
		if v := p.GetSeiFps(); v != 0 {
			b.WriteString("  fps: " + int32Text(v) + "\n")
		}
		if v := p.GetSeiBatchSize(); v != 0 {
			b.WriteString("  batch_size: " + int32Text(v) + "\n")
		}
		if v := p.GetSeiFragmentSize(); v != 0 {
			b.WriteString("  fragment_size: " + int32Text(v) + "\n")
		}
		if v := p.GetSeiAckTimeoutMs(); v != 0 {
			b.WriteString("  ack_timeout_ms: " + int32Text(v) + "\n")
		}
	}
	return b.String()
}

// lock takes locks.Olcrtc, waiting with the client's context; the held context is detached.
func (s *Service) lock(ctx context.Context, rpc string) (context.Context, func(), error) {
	held, unlock, err := s.locks().Lock(ctx, locks.Olcrtc)
	if err != nil {
		if ctx.Err() != nil {
			s.Log.Warn(rpc+" abandoned while waiting for the olcrtc lock", "err", err.Error())
			return nil, nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, nil, err
	}
	return context.WithoutCancel(held), unlock, nil
}

// SyncOlcrtcInstance implements the RPC (spec F §6.3): auth by metadata only.
func (s *Service) SyncOlcrtcInstance(ctx context.Context, req *agentpb.OlcrtcInstanceConfigPayload) (*agentpb.OlcrtcInstanceConfigResponse, error) {
	if !s.Auth.Metadata(ctx) {
		s.Log.Warn("Unauthorized SyncOlcrtcInstance request blocked")
		return &agentpb.OlcrtcInstanceConfigResponse{Success: false, Message: auth.InvalidSecretMessage}, nil
	}
	u := Sanitize(req.GetUserShortUuid())
	if !safeInstanceName.MatchString(u) {
		s.Log.Warn("Rejected SyncOlcrtcInstance with unsafe/empty user_short_uuid", "userShortUuid", u)
		return &agentpb.OlcrtcInstanceConfigResponse{Success: false, Message: msgBadName}, nil
	}
	held, unlock, err := s.lock(ctx, "SyncOlcrtcInstance")
	if err != nil {
		if _, isStatus := status.FromError(err); isStatus {
			return nil, err
		}
		return &agentpb.OlcrtcInstanceConfigResponse{Success: false, Message: "Olcrtc instance sync error: " + err.Error()}, nil
	}
	defer unlock()
	msg, ok, err := s.sync(held, u, req)
	if err != nil {
		s.Log.Error("Failed to sync olcrtc instance", "err", err.Error(), "userShortUuid", u)
		return &agentpb.OlcrtcInstanceConfigResponse{Success: false, Message: "Olcrtc instance sync error: " + err.Error()}, nil
	}
	return &agentpb.OlcrtcInstanceConfigResponse{Success: ok, Message: msg}, nil
}

func (s *Service) sync(ctx context.Context, u string, req *agentpb.OlcrtcInstanceConfigPayload) (string, bool, error) {
	s.EnsureUnit(ctx)
	p := s.ConfigPath(u)
	yaml := BuildYAML(req)
	cur, err := s.FS.ReadFile(p)
	changed := err != nil || string(cur) != yaml
	// TS rewrote the file on every sync, in place; written now only when the bytes or the mode
	// differ (so a 0644 file left by TS becomes 0600 on the next sync).
	if _, err := s.FS.WriteIfChanged(p, []byte(yaml), fsx.WriteOptions{Mode: configMode}); err != nil {
		return "", false, err
	}

	unit := UnitName(u)
	if err := s.Systemctl.Enable(ctx, unit); err != nil {
		s.Log.Warn("Failed to enable olcrtc-agent-srv instance for auto-start", "err", err.Error(), "unitName", unit)
	}
	if changed {
		if err := s.Systemctl.Restart(ctx, unit); err != nil {
			msg := restartErrorText(err)
			s.Log.Error("Failed to (re)start olcrtc-agent-srv instance", "err", msg, "unitName", unit)
			return "Failed to start olcrtc instance (" + unit + "): " + msg, false, nil
		}
	} else if err := s.Systemctl.Start(ctx, unit); err != nil {
		// Unchanged config: just make sure it runs (after a crash loop or a partial failure).
		s.Log.Debug("systemctl start no-op (likely already running)", "err", err.Error(), "unitName", unit)
	}
	word := "unchanged"
	if changed {
		word = "restarted"
	}
	return "olcrtc instance " + u + " configured (" + word + ").", true, nil
}

// restartErrorText is `(err.stderr || err.stdout || err.message || 'systemctl restart error').trim()`.
func restartErrorText(err error) string {
	text := ""
	if e, ok := err.(*runner.Error); ok {
		text = e.Stderr
		if text == "" {
			text = e.Stdout
		}
	}
	if text == "" {
		text = err.Error()
	}
	if text == "" {
		text = "systemctl restart error"
	}
	return jsjson.Trim(text)
}

// DeleteOlcrtcInstance implements the RPC (spec F §6.4): auth by metadata only; stop and disable
// the instance (failure ignored — TS ran it with `|| true`), remove its config; success even if
// nothing existed. The template unit stays.
func (s *Service) DeleteOlcrtcInstance(ctx context.Context, req *agentpb.OlcrtcInstanceDeletePayload) (*agentpb.OlcrtcInstanceConfigResponse, error) {
	if !s.Auth.Metadata(ctx) {
		s.Log.Warn("Unauthorized DeleteOlcrtcInstance request blocked")
		return &agentpb.OlcrtcInstanceConfigResponse{Success: false, Message: auth.InvalidSecretMessage}, nil
	}
	u := Sanitize(req.GetUserShortUuid())
	if !safeInstanceName.MatchString(u) {
		s.Log.Warn("Rejected DeleteOlcrtcInstance with unsafe/empty user_short_uuid", "userShortUuid", u)
		return &agentpb.OlcrtcInstanceConfigResponse{Success: false, Message: msgBadName}, nil
	}
	held, unlock, err := s.lock(ctx, "DeleteOlcrtcInstance")
	if err != nil {
		if _, isStatus := status.FromError(err); isStatus {
			return nil, err
		}
		return &agentpb.OlcrtcInstanceConfigResponse{Success: false, Message: "Olcrtc instance delete error: " + err.Error()}, nil
	}
	defer unlock()
	unit := UnitName(u)
	if err := s.Systemctl.Disable(held, "--now", unit); err != nil {
		s.Log.Debug("systemctl disable --now failed (ignored, as `|| true` in TS)", "err", err.Error(), "unitName", unit)
	}
	_ = os.Remove(s.FS.Path(s.ConfigPath(u)))
	return &agentpb.OlcrtcInstanceConfigResponse{Success: true, Message: "olcrtc instance " + u + " removed."}, nil
}
