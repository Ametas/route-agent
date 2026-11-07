package binaries_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/binaries"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
)

const sec = "grpc-secret-grpc-secret"

// Uploads through a real gRPC server and client (the transport TLS is covered in
// internal/server; here the point is the stream framing): 64 KiB chunks like the orchestrator's
// GrpcBinaryUploader, status OK on rejection, the 4 MiB per-message limit (checklist 2, 6, 11).
func TestUploadsOverRealGRPCStream(t *testing.T) {
	cfg, err := config.Parse(map[string]string{"EGRESS_CONTROL_SECRET": sec})
	if err != nil {
		t.Fatal(err)
	}
	fs := fsx.FS{Root: t.TempDir()}
	fake := &runner.Fake{}
	svc := binaries.New(binaries.Deps{
		Auth: auth.New(sec), Runner: fake, FS: fs, Systemctl: systemd.Systemctl{Runner: fake}, Cfg: cfg,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	svc.TempDir = t.TempDir()
	srv := server.New(&server.Handlers{UploadRearRuleSet: svc.UploadRearRuleSet, UploadMeshAwgKernelSource: svc.UploadMeshAwgKernelSource}, server.Options{Keepalive: server.DefaultKeepalive()})
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
	c := agentpb.NewEgressAgentServiceClient(conn)
	ctx := func(secret string) context.Context {
		cx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		if secret != "" {
			cx = metadata.AppendToOutgoingContext(cx, auth.MetadataKey, secret)
		}
		return cx
	}

	// 300 KiB in 64 KiB chunks, every chunk carrying the secret like the orchestrator.
	payload := bytes.Repeat([]byte("0123456789abcdef"), 300*1024/16)
	st, err := c.UploadRearRuleSet(ctx(sec))
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off < len(payload); off += 64 << 10 {
		end := min(off+64<<10, len(payload))
		if err := st.Send(&agentpb.BinaryChunkPayload{OrchestratorSecret: sec, Chunk: payload[off:end], TargetBinary: "geosite-ru", IsFinal: end == len(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := st.CloseAndRecv()
	if err != nil || !resp.Success || resp.Message != "Набор правил geosite-ru записан (307200 байт)" {
		t.Fatalf("%v %+v", err, resp)
	}
	if got, _ := os.ReadFile(fs.Path("/etc/route-agent/rules/geosite-ru.mrs")); !bytes.Equal(got, payload) {
		t.Fatal("content differs")
	}

	// Unauthorized: success:false with status OK, even while the client keeps sending.
	st, _ = c.UploadRearRuleSet(ctx("wrong"))
	for i := 0; i < 5; i++ {
		_ = st.Send(&agentpb.BinaryChunkPayload{OrchestratorSecret: "nope", Chunk: payload[:64<<10], TargetBinary: "x"})
	}
	resp, err = st.CloseAndRecv()
	if err != nil || resp.Success || resp.Message != auth.InvalidSecretMessage {
		t.Fatalf("unauthorized: %v %+v", err, resp)
	}

	// Zero chunks with a bad metadata secret: the per-RPC end-of-stream message.
	ms, _ := c.UploadMeshAwgKernelSource(ctx(""))
	resp, err = ms.CloseAndRecv()
	if err != nil || resp.Success || resp.Message != "No valid source data received or unauthorized." {
		t.Fatalf("mesh empty: %v %+v", err, resp)
	}

	// A single message over 4 MiB is refused by the transport.
	st, _ = c.UploadRearRuleSet(ctx(sec))
	_ = st.Send(&agentpb.BinaryChunkPayload{Chunk: bytes.Repeat([]byte{1}, 5<<20), TargetBinary: "big"})
	_, err = st.CloseAndRecv()
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("over 4 MiB: %v", err)
	}
}
