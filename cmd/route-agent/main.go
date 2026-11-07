// Command route-agent is the node agent: a gRPC server (agent.EgressAgentService over mTLS) that
// the orchestrator drives. Go port of src/index.ts (spec A §1).
//
// Boot order (spec A §1.3): config → logger → pre-bind boot steps → mTLS credentials (fatal if
// missing) → gRPC server bind → listening line → post-bind tasks and background loops. Any
// startup failure logs the fatal line and exits 1; systemd (Restart=always) retries every 5 s.
// SIGINT/SIGTERM stop the server gracefully, bounded by shutdownGrace, then hard (the telemetry
// and Olcrtc event streams never end on their own, spec A §1.4).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/contract"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/lifecycle"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
	"github.com/Ametas/route-agent/internal/version"
)

const shutdownGrace = 3 * time.Second

// levelFatal mirrors pino's fatal (60) for the startup-failure line.
const levelFatal = slog.Level(12)

func levelNames(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.LevelKey {
		if lvl, ok := a.Value.Any().(slog.Level); ok && lvl >= levelFatal {
			a.Value = slog.StringValue("FATAL")
		}
	}
	return a
}

func main() {
	showVersion := flag.Bool("version", false, "print the agent version and exit")
	envFile := flag.String("env-file", ".env", "dotenv file read for variables absent from the environment")
	flag.Parse()

	if *showVersion {
		// SelfUpdate runs the candidate binary with --version before swapping it in.
		r := &version.Resolver{Runner: runner.Exec{}}
		fmt.Println(r.Get(context.Background()))
		return
	}

	loaded, err := config.Load(os.Environ(), *envFile)
	if err != nil {
		os.Stderr.WriteString(config.StderrMessage(err))
		os.Exit(1)
	}
	// dotenv populated process.env, so children saw .env-only keys too; keep that.
	for k, v := range loaded.DotenvAdded {
		_ = os.Setenv(k, v)
	}
	cfg := loaded.Config

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel, ReplaceAttr: levelNames}))
	slog.SetDefault(log)

	if err := run(cfg, log); err != nil {
		log.Log(context.Background(), levelFatal, "💥 Route Agent failed to start and is exiting; systemd (Restart=always) will retry, "+
			"but this will loop until the underlying issue is fixed", "err", err.Error())
		os.Exit(1)
	}
}

// deps are the shared services every group's handlers are built from.
type deps struct {
	cfg       *config.Config
	log       *slog.Logger
	runner    runner.Runner
	fs        fsx.FS
	systemctl systemd.Systemctl
	auth      *auth.Verifier
	version   *version.Resolver
	exe       string
	// locks is the ONE area lock registry of the process (internal/locks): every group's service
	// gets this instance, so RPCs of different groups on the same area serialize.
	locks *locks.Registry
}

func run(cfg *config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	r := runner.Exec{}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own executable: %w", err)
	}
	d := deps{
		cfg:       cfg,
		log:       log,
		runner:    r,
		fs:        fsx.FS{},
		systemctl: systemd.Systemctl{Runner: r},
		auth:      auth.New(cfg.EgressControlSecret),
		version:   &version.Resolver{Runner: r},
		exe:       exe,
		locks:     locks.New(),
	}

	bg := &lifecycle.Group{Log: log}
	lifecycle.RunSteps(ctx, log, preBindSteps(d))
	startPreBindLoops(ctx, bg, d)

	tlsCfg, err := server.LoadTLS(server.TLSPaths{CA: cfg.CACertPath, Cert: cfg.AgentCertPath, Key: cfg.AgentKeyPath})
	if err != nil {
		return err
	}
	log.Info("🔒 Enabling mTLS with strict client certificate verification", "caPath", cfg.CACertPath, "certPath", cfg.AgentCertPath)

	srv := server.New(buildHandlers(d), server.Options{TLS: tlsCfg, Keepalive: server.DefaultKeepalive()})
	lis, err := server.Listen(cfg.Host, cfg.Port)
	if err != nil {
		log.Error("Failed to bind gRPC server", "err", err.Error())
		return err
	}
	hash := contract.Hash()
	port := lis.Addr().(*net.TCPAddr).Port
	log.Info(fmt.Sprintf("🚀 gRPC Route Agent actively listening at h2://%s:%d (Proto Contract SHA256: %s)", cfg.Host, port, hash),
		"protoHash", hash, "len", len(hash), "goarch", runtime.GOARCH)

	startPostBind(ctx, bg, d)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(lis) }()

	select {
	case err := <-serveErr:
		stop()
		bg.Wait()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("gRPC server stopped: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	log.Info("Shutdown signal received; stopping gRPC server")
	done := make(chan struct{})
	go func() { srv.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
		srv.Stop()
		<-done
	}
	bg.Wait()
	return nil
}
