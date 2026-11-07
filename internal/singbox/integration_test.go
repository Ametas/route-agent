package singbox_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/caddy"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/singbox"
	"github.com/Ametas/route-agent/internal/systemd"
)

// TestGroupBOverGRPC drives the four group B RPCs through the real server (internal/server,
// without mTLS — that is covered by its own tests) and a real gRPC client: the failure shapes the
// orchestrator distinguishes (status OK + success:false, empty repeated fields) survive the wire.
func TestGroupBOverGRPC(t *testing.T) {
	const secret = "s3cret"
	cfg, err := config.Parse(map[string]string{"EGRESS_CONTROL_SECRET": secret})
	if err != nil {
		t.Fatal(err)
	}
	fake := &runner.Fake{Handler: func(c runner.Cmd) (runner.Result, error) {
		if c.Display() == "systemctl is-active sing-box" || c.Display() == "systemctl is-active caddy" {
			return runner.Result{Stdout: "active\n"}, nil
		}
		return runner.Result{}, nil
	}}
	fs := fsx.FS{Root: t.TempDir()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sc := systemd.Systemctl{Runner: fake}
	sb := &singbox.Service{Auth: auth.New(secret), Runner: fake, FS: fs, Systemctl: sc, Cfg: cfg, Log: log,
		LookPath: func(string) (string, error) { return "", os.ErrNotExist }}
	cd := &caddy.Service{Auth: auth.New(secret), Runner: fake, FS: fs, Systemctl: sc, Cfg: cfg, Log: log}
	h := &server.Handlers{
		ApplyConfig: sb.ApplyConfig, GetSingBoxUserTraffic: sb.GetSingBoxUserTraffic,
		GetSingBoxConnections: sb.GetSingBoxConnections, ConfigureCaddy: cd.ConfigureCaddy,
	}
	srv := server.New(h, server.Options{Keepalive: server.DefaultKeepalive()})
	lis, err := server.Listen("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cl := agentpb.NewEgressAgentServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bad := metadata.AppendToOutgoingContext(ctx, "x-orchestrator-secret", "bad")
	good := metadata.AppendToOutgoingContext(ctx, "x-orchestrator-secret", secret)

	a, err := cl.ApplyConfig(bad, &agentpb.ConfigPayload{ConfigJson: "{}"})
	if err != nil || a.Success || a.Message != auth.InvalidSecretMessage {
		t.Fatal(a, err)
	}
	cy, err := cl.ConfigureCaddy(bad, &agentpb.CaddyConfigPayload{CaddyfileContent: "example.com {}"})
	if err != nil || cy.Success || cy.Message != auth.InvalidSecretMessage {
		t.Fatal(cy, err)
	}
	tr, err := cl.GetSingBoxUserTraffic(bad, &agentpb.SingBoxUserTrafficRequest{})
	if err != nil || tr.Success || len(tr.Entries) != 0 {
		t.Fatal(tr, err)
	}
	cn, err := cl.GetSingBoxConnections(bad, &agentpb.SingBoxConnectionsRequest{})
	if err != nil || cn.Success || len(cn.Connections) != 0 {
		t.Fatal(cn, err)
	}

	a, err = cl.ApplyConfig(good, &agentpb.ConfigPayload{ConfigJson: `{"log":{"level":"warn"}}`})
	if err != nil || !a.Success || a.Message != singbox.MsgApplied {
		t.Fatal(a, err)
	}
	a, _ = cl.ApplyConfig(good, &agentpb.ConfigPayload{ConfigJson: `{"log":{"level":"warn"}}`})
	if a.Message != singbox.MsgAlreadyCurrent {
		t.Fatal(a)
	}
	a, _ = cl.ApplyConfig(good, &agentpb.ConfigPayload{ConfigJson: `{`})
	if a.Success || a.Message != "Internal Agent Error: Expected property name or '}' in JSON at position 1 (line 1 column 2)" {
		t.Fatal(a)
	}
	cy, err = cl.ConfigureCaddy(good, &agentpb.CaddyConfigPayload{CaddyfileContent: "example.com {}\n"})
	if err != nil || !cy.Success || cy.Message != caddy.MsgApplied {
		t.Fatal(cy, err)
	}
	tr, _ = cl.GetSingBoxUserTraffic(good, &agentpb.SingBoxUserTrafficRequest{})
	if !tr.Success || tr.Message != "clash_api block absent from the applied sing-box config" {
		t.Fatal(tr)
	}
	cn, _ = cl.GetSingBoxConnections(good, &agentpb.SingBoxConnectionsRequest{})
	if !cn.Success || cn.Message != "clash_api not configured on this node" {
		t.Fatal(cn)
	}
}
