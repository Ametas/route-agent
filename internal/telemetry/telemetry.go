// Package telemetry implements StreamTelemetry and GetTelemetry (spec E §2): the frame the
// orchestrator's telemetry watchdog, node tiles and gating logic read.
//
// Contract with the orchestrator (architecture §1, spec E §2.3): the first frame well within 5 s
// of the stream opening (its watchdog), a frame every 2 s (online = a frame within 20 s),
// timestamp in epoch milliseconds and strictly increasing per stream (host traffic rates are keyed
// by it), mesh_tunnel_active_peers == len(mesh_tunnel_live_peer_keys), and the sentinel strings
// of spec E §2.4. GetTelemetry must answer within the orchestrator's 1 s display deadline.
//
// Differences from TS (deviations.md): ticks never overlap (the next one is scheduled after the
// previous frame was sent); every command-backed source has a budget and shares in-flight probes
// (source.go); no frame ever runs the AWG self-heal (awg.Health's own loop does).
package telemetry

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/server/auth"
)

// Timing of the stream and of the per-source budgets.
const (
	// Interval is the gap between the end of one frame and the start of the next
	// (setInterval(…, 2000) after the first awaited tick, telemetry.service.ts:139-141).
	Interval = 2 * time.Second
	// StreamBudget bounds how long a stream frame waits for one source: a frame is sent at most
	// this long after its tick starts, so the first one arrives far inside the 5 s watchdog.
	StreamBudget = 1500 * time.Millisecond
	// UnaryBudget is the same for GetTelemetry, inside the orchestrator's 1 s display deadline.
	UnaryBudget = 700 * time.Millisecond
)

// Service holds the telemetry handlers.
type Service struct {
	Auth      *auth.Verifier
	Collector *Collector
	Log       *slog.Logger
	// Journal starts the per-stream log tail; nil means ExecJournal.
	Journal JournalTail
	// Interval, StreamBudget, UnaryBudget override the defaults (tests); zero means default.
	Interval     time.Duration
	StreamBudget time.Duration
	UnaryBudget  time.Duration
	// Now is the clock of the timestamp field; nil means time.Now.
	Now func() time.Time
}

func pick(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) journal() JournalTail {
	if s.Journal != nil {
		return s.Journal
	}
	return ExecJournal{}
}

// StreamTelemetry is streamTelemetryHandler (telemetry.service.ts:26-142). Auth M|B (the
// orchestrator sends both; a payload-only secret works too); failure is UNAUTHENTICATED with no
// frame and no journalctl. Then a `journalctl -u sing-box -f` tail feeds system_logs, the first
// frame is sent at once and the next ones Interval after the previous frame was sent, until the
// client cancels or a send fails. The handler never ends the stream on its own.
func (s *Service) StreamTelemetry(req *agentpb.TelemetryRequest, stream grpc.ServerStreamingServer[agentpb.TelemetryResponse]) error {
	ctx := stream.Context()
	if !s.Auth.MetadataOrBody(ctx, req.GetOrchestratorSecret()) {
		s.Log.Warn("Unauthorized gRPC telemetry stream requested")
		return auth.Unauthenticated()
	}
	s.Log.Info("Dynamic telemetry binary stream opened by orchestrator")
	ctx, cancel := context.WithCancel(ctx)

	logs := &logBuffer{}
	out, err := s.journal().Start(ctx)
	if err != nil {
		s.Log.Warn("Failed to spawn journalctl process", "err", err.Error())
	} else {
		go pump(out, logs)
	}
	defer func() {
		cancel() // SIGTERM to journalctl (exec.CommandContext Cancel)
		if out != nil {
			_ = out.Close()
		}
		s.Log.Info("Telemetry binary stream closed and resources safely released")
	}()

	interval := pick(s.Interval, Interval)
	budget := pick(s.StreamBudget, StreamBudget)
	var prev CPUStats
	var lastTs int64
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-timer.C:
		}
		frame, next := s.Collector.Frame(ctx, prev, budget)
		prev = next
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		ts := s.now().UnixMilli()
		if ts <= lastTs {
			ts = lastTs + 1 // strictly increasing even if the wall clock steps back
		}
		lastTs = ts
		frame.Timestamp = ts
		frame.SystemLogs = logs.take()
		if err := stream.Send(frame); err != nil {
			s.Log.Debug("Telemetry frame could not be sent; closing the stream", "err", err.Error())
			return err
		}
		timer.Reset(interval)
	}
}

// GetTelemetry is getTelemetryUnaryHandler (telemetry.service.ts:147-234): one frame with a
// zero CPU baseline (cpu_usage is the average since boot, as in TS) and system_logs "OK".
func (s *Service) GetTelemetry(ctx context.Context, req *agentpb.TelemetryRequest) (resp *agentpb.TelemetryResponse, err error) {
	if !s.Auth.MetadataOrBody(ctx, req.GetOrchestratorSecret()) {
		s.Log.Warn("Unauthorized GetTelemetry Unary request blocked")
		// Architecture §6 item 13, owner decision 2026-11-07: UNAUTHENTICATED, like StreamTelemetry.
		// TS answered status OK with placeholder values, and the orchestrator cached them as live
		// telemetry, so a node with a wrong secret showed as online. fetchTelemetryUnary
		// (orch GrpcTelemetryStreamManager.ts) takes the error path: success:false, lastError set.
		return nil, auth.Unauthenticated()
	}
	defer func() {
		if r := recover(); r != nil {
			s.Log.Error("Failed to process GetTelemetry Unary request", "err", r)
			resp, err = internalErrorBody(s.now(), r), nil
		}
	}()
	frame, _ := s.Collector.Frame(ctx, CPUStats{}, pick(s.UnaryBudget, UnaryBudget))
	frame.SystemLogs = "OK"
	frame.Timestamp = s.now().UnixMilli()
	return frame, nil
}

// internalErrorBody is the catch branch of telemetry.service.ts:211-233 (a panicking source).
func internalErrorBody(now time.Time, r any) *agentpb.TelemetryResponse {
	msg := "unknown error"
	switch v := r.(type) {
	case error:
		msg = v.Error()
	case string:
		msg = v
	}
	return &agentpb.TelemetryResponse{
		SystemLogs:                "Internal Error: " + msg,
		Timestamp:                 now.UnixMilli(),
		WebrtcStatus:              "error",
		SingboxVersion:            "error",
		AwgVersion:                "error",
		AwgStatus:                 "error",
		AwgToolsVersion:           "error",
		AwgGoVersion:              "error",
		MeshAwgKernelStatus:       "error",
		MeshTunnelInterfaceStatus: "error",
		MeshTunnelLivePeerKeys:    []string{},
	}
}
