// Package warnings implements PullNodeWarnings (spec E §3): degradation classes found in the
// journal of the managed units and the kernel, plus two live checks owned by other groups — the
// front core not listening on its declared ports (singbox.DetectCoreNotListening, group B) and
// rear rule providers loaded empty (rear.DetectEmptyRuleProviders, group F).
//
// The agent is stateless here by design (journalWarnings.ts:20-23): it reports everything in the
// lookback window every time; deduplication (12 h re-notify per node|kind|source) is the
// orchestrator's.
package warnings

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
)

// Service holds the PullNodeWarnings handler.
type Service struct {
	Auth   *auth.Verifier
	Runner runner.Runner
	Cfg    *config.Config
	Log    *slog.Logger
	// CoreNotListening is (*singbox.Service).DetectCoreNotListening; nil skips the check.
	CoreNotListening func(ctx context.Context) *agentpb.NodeWarning
	// EmptyRuleProviders is (*rear.Service).DetectEmptyRuleProviders; nil skips the check.
	EmptyRuleProviders func(ctx context.Context) *agentpb.NodeWarning
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) lookbackHours() uint32 { return jsjson.ToUint32(s.Cfg.NodeWarningLookbackHours) }

// PullNodeWarnings is pullNodeWarningsHandler (nodeWarnings.service.ts:22-84). Auth M|B; failure
// answers status OK with success:false and lookback 0. The three sources run concurrently; the
// list is [core_not_listening, rear_rule_provider_empty, ...journal classes by lastSeen desc].
func (s *Service) PullNodeWarnings(ctx context.Context, req *agentpb.NodeWarningsRequest) (resp *agentpb.NodeWarningsResponse, err error) {
	if !s.Auth.MetadataOrBody(ctx, req.GetOrchestratorSecret()) {
		s.Log.Warn("Unauthorized PullNodeWarnings request blocked")
		return &agentpb.NodeWarningsResponse{Success: false, Message: auth.InvalidSecretMessage, Warnings: []*agentpb.NodeWarning{}}, nil
	}
	defer func() {
		if r := recover(); r != nil {
			s.Log.Error("Failed to collect journal warnings", "err", r)
			resp, err = &agentpb.NodeWarningsResponse{
				Success: false, Message: "Internal Agent Error: " + panicText(r),
				Warnings: []*agentpb.NodeWarning{}, LookbackHours: s.lookbackHours(),
			}, nil
		}
	}()

	var journal []Warning
	var notListening, emptyProviders *agentpb.NodeWarning
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		journal = CollectJournalWarnings(ctx, s.Runner, s.Cfg, s.Log, s.Cfg.NodeWarningLookbackHours, s.now())
	}()
	go func() {
		defer wg.Done()
		if s.CoreNotListening != nil {
			notListening = s.CoreNotListening(ctx)
		}
	}()
	go func() {
		defer wg.Done()
		if s.EmptyRuleProviders != nil {
			emptyProviders = s.EmptyRuleProviders(ctx)
		}
	}()
	wg.Wait()

	out := []*agentpb.NodeWarning{}
	for _, w := range []*agentpb.NodeWarning{notListening, emptyProviders} {
		if w != nil {
			out = append(out, w)
		}
	}
	for _, w := range journal {
		out = append(out, &agentpb.NodeWarning{
			Kind: w.Kind, Source: w.Source, Sample: w.Sample, Occurrences: w.Occurrences, LastSeenUnixMs: w.LastSeenUnixMs,
		})
	}
	if len(out) > 0 {
		kinds := make([]string, len(out))
		for i, w := range out {
			kinds[i] = w.Kind + "@" + w.Source
		}
		s.Log.Info("Journal warnings found and reported to the orchestrator", "kinds", kinds)
	}
	msg := "No degradation warnings in the journal."
	if len(out) > 0 {
		msg = strconv.Itoa(len(out)) + " warning class(es) found."
	}
	return &agentpb.NodeWarningsResponse{Success: true, Message: msg, Warnings: out, LookbackHours: s.lookbackHours()}, nil
}

func panicText(r any) string {
	switch v := r.(type) {
	case error:
		return v.Error()
	case string:
		return v
	}
	return "unknown error"
}
