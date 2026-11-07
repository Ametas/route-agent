package main

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
	"github.com/Ametas/route-agent/internal/telemetry"
	"github.com/Ametas/route-agent/internal/version"
)

func groupEDeps(t *testing.T) (deps, *runner.Fake) {
	t.Helper()
	cfg, err := config.Parse(map[string]string{"EGRESS_CONTROL_SECRET": "x"})
	if err != nil {
		t.Fatal(err)
	}
	f := &runner.Fake{}
	return deps{
		cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), runner: f, fs: fsx.FS{Root: t.TempDir()},
		systemctl: systemd.Systemctl{Runner: f}, auth: auth.New("x"), version: &version.Resolver{Runner: f}, exe: "/x",
	}, f
}

// After group E nothing may reach the Unimplemented stub (internal/server/stubs.go): every one of
// the 31 Handlers fields is set, and they are exactly the 31 RPCs of the proto and the auth matrix.
func TestAllRPCsWired(t *testing.T) {
	d, _ := groupEDeps(t)
	h := buildHandlers(d)
	v := reflect.ValueOf(h).Elem()
	var fields []string
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		fields = append(fields, name)
		if v.Field(i).IsNil() {
			t.Errorf("%s is not wired (would answer Unimplemented)", name)
		}
	}
	var rpcs []string
	for _, m := range agentpb.EgressAgentService_ServiceDesc.Methods {
		rpcs = append(rpcs, m.MethodName)
	}
	for _, s := range agentpb.EgressAgentService_ServiceDesc.Streams {
		rpcs = append(rpcs, s.StreamName)
	}
	matrix := auth.Methods()
	sort.Strings(fields)
	sort.Strings(rpcs)
	sort.Strings(matrix)
	if len(rpcs) != 31 || !reflect.DeepEqual(fields, rpcs) || !reflect.DeepEqual(rpcs, matrix) {
		t.Fatalf("handlers %v\nproto %v\nmatrix %v", fields, rpcs, matrix)
	}
}

// The same through the real server and transport: every RPC, called with a wrong secret (and an
// empty request), answers something other than codes.Unimplemented.
func TestNoRPCAnswersUnimplemented(t *testing.T) {
	d, _ := groupEDeps(t)
	srv := server.New(buildHandlers(d), server.Options{Keepalive: server.DefaultKeepalive()})
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
	t.Cleanup(func() { conn.Close() })
	svc := agentpb.EgressAgentService_ServiceDesc.ServiceName
	call := func() (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		return metadata.AppendToOutgoingContext(ctx, auth.MetadataKey, "wrong"), cancel
	}
	for _, m := range agentpb.EgressAgentService_ServiceDesc.Methods {
		ctx, cancel := call()
		err := conn.Invoke(ctx, "/"+svc+"/"+m.MethodName, &emptypb.Empty{}, &emptypb.Empty{})
		cancel()
		if status.Code(err) == codes.Unimplemented {
			t.Errorf("%s: %v", m.MethodName, err)
		}
	}
	for _, s := range agentpb.EgressAgentService_ServiceDesc.Streams {
		ctx, cancel := call()
		st, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: s.StreamName, ServerStreams: s.ServerStreams, ClientStreams: s.ClientStreams},
			"/"+svc+"/"+s.StreamName)
		if err == nil {
			if err = st.SendMsg(&emptypb.Empty{}); err == nil {
				_ = st.CloseSend()
				err = st.RecvMsg(&emptypb.Empty{})
			}
		}
		cancel()
		if status.Code(err) == codes.Unimplemented {
			t.Errorf("%s: %v", s.StreamName, err)
		}
	}
}

// Group E shares state with groups C and D: one awg.Health (loop and frame), one mesh.Status, one
// telemetry collector whose caches the binary uploads invalidate.
func TestGroupEShared(t *testing.T) {
	d, _ := groupEDeps(t)
	d = withShared(d)
	if d.telemetry.AwgHealth != d.awgHealth || d.telemetry.Mesh != d.meshStatus || d.meshStatus.LinkExists == nil {
		t.Fatal("telemetry does not read the shared instances")
	}
	if again := withShared(d); again.awgHealth != d.awgHealth || again.telemetry != d.telemetry || again.meshStatus != d.meshStatus {
		t.Fatal("withShared is not idempotent")
	}
	cb := telemetryCallbacks(d)
	if cb.SingboxSwapped == nil || cb.AwgSwapped == nil || cb.MeshKernelChanged == nil {
		t.Fatal("binaries.Telemetry callbacks not wired")
	}

	// Observable: a swap callback makes the next frame re-probe.
	f := &runner.Fake{Handler: func(c runner.Cmd) (runner.Result, error) {
		return runner.Result{Stdout: "sing-box version 1.13.0\nversion: 1\n"}, nil
	}}
	d, _ = groupEDeps(t)
	d.runner = f
	d.systemctl = systemd.Systemctl{Runner: f}
	d = withShared(d)
	cb = telemetryCallbacks(d)
	count := func(name string) int {
		n := 0
		for _, c := range f.Calls() {
			if c.Name == name {
				n++
			}
		}
		return n
	}
	frame := func() { d.telemetry.Frame(context.Background(), telemetry.CPUStats{}, time.Second) }
	frame()
	frame()
	base := count("modinfo")
	cb.MeshKernelChanged()
	frame()
	if count("modinfo") != base+1 {
		t.Fatal("MeshKernelChanged does not invalidate the kernel status")
	}
	awgBase := count("/usr/local/bin/awg")
	cb.AwgSwapped()
	frame()
	if count("/usr/local/bin/awg") != awgBase+1 {
		t.Fatal("AwgSwapped does not invalidate the AWG versions")
	}
}
