package server_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/Ametas/route-agent/internal/agentinfo"
	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/certs"
	"github.com/Ametas/route-agent/internal/contract"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/server"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/testcerts"
)

const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type env struct {
	set  *testcerts.Set
	addr string
}

// start runs the real server with mTLS from generated certificates on 127.0.0.1:0.
func start(t *testing.T, ka server.Keepalive) *env {
	t.Helper()
	set, err := testcerts.New()
	if err != nil {
		t.Fatal(err)
	}
	ca, cert, key, err := set.WriteServerFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := server.LoadTLS(server.TLSPaths{CA: ca, Cert: cert, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	info := &agentinfo.Service{Version: func(context.Context) string { return "v1.4.0-3-gabc1234" }, Log: log}
	crt := &certs.Service{Auth: auth.New(secret), FS: fsx.FS{Root: t.TempDir()}, Log: log}
	h := &server.Handlers{GetAgentInfo: info.GetAgentInfo, GetManagedCertificate: crt.GetManagedCertificate}
	srv := server.New(h, server.Options{TLS: tlsCfg, Keepalive: ka})
	lis, err := server.Listen("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return &env{set: set, addr: lis.Addr().String()}
}

// clientTLS behaves like the orchestrator (GrpcConnectionManager.ts): SNI = node UUID, chain
// verified against the CA, CN must equal the node UUID, leaf fingerprint pinned.
func (e *env) clientTLS(t *testing.T, client testcerts.Pair) *tls.Config {
	t.Helper()
	pair, err := tls.X509KeyPair(client.CertPEM, client.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(e.set.CA.Cert)
	pin := testcerts.Fingerprint(e.set.Server.Cert.Raw)
	return &tls.Config{
		Certificates:       []tls.Certificate{pair},
		ServerName:         e.set.NodeUUID,
		InsecureSkipVerify: true, // replaced by VerifyConnection: the node cert has no SAN, only CN
		VerifyConnection: func(cs tls.ConnectionState) error {
			leaf := cs.PeerCertificates[0]
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
				return err
			}
			if leaf.Subject.CommonName != e.set.NodeUUID {
				return errors.New("CN mismatch")
			}
			if testcerts.Fingerprint(leaf.Raw) != pin {
				return errors.New("fingerprint mismatch")
			}
			return nil
		},
		NextProtos: []string{"h2"},
	}
}

func (e *env) dial(t *testing.T, client testcerts.Pair) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(e.addr,
		grpc.WithTransportCredentials(credentials.NewTLS(e.clientTLS(t, client))),
		grpc.WithAuthority(e.set.NodeUUID),
		// The orchestrator's client keepalive (GrpcConnectionManager.ts:294-308).
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 10 * time.Second, Timeout: 20 * time.Second, PermitWithoutStream: true}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func ctx5(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestGetAgentInfoOverMTLS(t *testing.T) {
	e := start(t, server.DefaultKeepalive())
	c := agentpb.NewEgressAgentServiceClient(e.dial(t, e.set.Client))
	resp, err := c.GetAgentInfo(ctx5(t), &agentpb.AgentInfoRequest{}) // no metadata, no body secret
	if err != nil {
		t.Fatal(err)
	}
	if resp.ProtoContractHash != contract.Expected || resp.ProtoContractSource != "canonical-json" || resp.AgentVersion != "v1.4.0-3-gabc1234" {
		t.Fatalf("%+v", resp)
	}
	// M|B through the real transport: body secret alone authenticates.
	cr, err := c.GetManagedCertificate(ctx5(t), &agentpb.CertificateRequest{OrchestratorSecret: secret, Domain: " "})
	if err != nil || cr.Message != "Missing domain parameter." {
		t.Fatalf("%v %+v", err, cr)
	}
	cr, _ = c.GetManagedCertificate(ctx5(t), &agentpb.CertificateRequest{Domain: "a.com"})
	if cr.Success || cr.Message != auth.InvalidSecretMessage {
		t.Fatalf("%+v", cr)
	}
}

func TestAllMethodsRegistered(t *testing.T) {
	e := start(t, server.DefaultKeepalive())
	conn := e.dial(t, e.set.Client)
	implemented := map[string]bool{"GetAgentInfo": true, "GetManagedCertificate": true}
	sd := agentpb.File_agent_proto.Services().Get(0)
	if sd.Methods().Len() != 31 || len(agentpb.EgressAgentService_ServiceDesc.Methods)+len(agentpb.EgressAgentService_ServiceDesc.Streams) != 31 {
		t.Fatal("expected 31 RPCs")
	}
	for i := 0; i < sd.Methods().Len(); i++ {
		m := sd.Methods().Get(i)
		name := string(m.Name())
		if implemented[name] {
			continue
		}
		full := "/agent.EgressAgentService/" + name
		var err error
		if !m.IsStreamingClient() && !m.IsStreamingServer() {
			err = conn.Invoke(ctx5(t), full, &emptypb.Empty{}, &emptypb.Empty{})
		} else {
			var s grpc.ClientStream
			s, err = conn.NewStream(ctx5(t), &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}, full)
			if err == nil {
				_ = s.SendMsg(&emptypb.Empty{})
				_ = s.CloseSend()
				err = s.RecvMsg(&emptypb.Empty{})
			}
		}
		st, _ := status.FromError(err)
		if st.Code() != codes.Unimplemented || !strings.Contains(st.Message(), "porting group") {
			t.Errorf("%s: %v (want the Unimplemented stub)", name, err)
		}
	}
	err := conn.Invoke(ctx5(t), "/agent.EgressAgentService/NoSuchMethod", &emptypb.Empty{}, &emptypb.Empty{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("unknown method: %v", err)
	}
}

func TestClientCertificateChecks(t *testing.T) {
	e := start(t, server.DefaultKeepalive())
	for name, pair := range map[string]testcerts.Pair{"other CA": e.set.Untrusted, "serverAuth-only EKU": e.set.ServerEKU} {
		c := agentpb.NewEgressAgentServiceClient(e.dial(t, pair))
		if _, err := c.GetAgentInfo(ctx5(t), &agentpb.AgentInfoRequest{}); err == nil {
			t.Errorf("%s: client certificate accepted", name)
		}
	}
	// No client certificate at all.
	cfg := e.clientTLS(t, e.set.Client)
	cfg.Certificates = nil
	conn, err := grpc.NewClient(e.addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)), grpc.WithAuthority(e.set.NodeUUID))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := agentpb.NewEgressAgentServiceClient(conn).GetAgentInfo(ctx5(t), &agentpb.AgentInfoRequest{}); err == nil {
		t.Error("connection without client certificate accepted")
	}
}

func TestMessageSizeLimit(t *testing.T) {
	e := start(t, server.DefaultKeepalive())
	c := agentpb.NewEgressAgentServiceClient(e.dial(t, e.set.Client))
	if _, err := c.GetAgentInfo(ctx5(t), &agentpb.AgentInfoRequest{OrchestratorSecret: strings.Repeat("x", 4<<20-64)}); err != nil {
		t.Fatalf("just under 4 MiB must pass: %v", err)
	}
	_, err := c.GetAgentInfo(ctx5(t), &agentpb.AgentInfoRequest{OrchestratorSecret: strings.Repeat("x", 5<<20)})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("over 4 MiB: %v", err)
	}
}

func TestLoadTLSErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := server.LoadTLS(server.TLSPaths{CA: filepath.Join(dir, "ca"), Cert: filepath.Join(dir, "c"), Key: filepath.Join(dir, "k")})
	if err == nil || !strings.Contains(err.Error(), "mTLS certificates are required but unavailable") ||
		!strings.Contains(err.Error(), "MISSING at "+filepath.Join(dir, "ca")) || !strings.Contains(err.Error(), "refuses to start") {
		t.Fatalf("missing: %v", err)
	}
	set, _ := testcerts.New()
	ca, cert, key, _ := set.WriteServerFiles(dir)
	_ = os.Remove(key)
	_ = os.Mkdir(key, 0o700) // key path is a directory: exists, unreadable
	_, err = server.LoadTLS(server.TLSPaths{CA: ca, Cert: cert, Key: key})
	if err == nil || !strings.Contains(err.Error(), "mTLS certificates were found but failed to load") || !strings.Contains(err.Error(), "refuses to start") {
		t.Fatalf("unreadable: %v", err)
	}
	_ = os.Remove(key)
	_ = os.WriteFile(key, []byte("not pem"), 0o600)
	if _, err = server.LoadTLS(server.TLSPaths{CA: ca, Cert: cert, Key: key}); err == nil || !strings.Contains(err.Error(), "failed to load") {
		t.Fatalf("bad pem: %v", err)
	}
}

func TestListenAddr(t *testing.T) {
	if a, err := server.ListenAddr("0.0.0.0", 8081); err != nil || a != "0.0.0.0:8081" {
		t.Fatal(a, err)
	}
	for _, p := range []float64{-1, 70000, 1.5} {
		if _, err := server.ListenAddr("0.0.0.0", p); err == nil {
			t.Errorf("port %v accepted", p)
		}
	}
}

// --- keepalive -------------------------------------------------------------------------------

const scale = 50 // production durations / scale

func scaled(k server.Keepalive) server.Keepalive {
	return server.Keepalive{
		Time: k.Time / scale, Timeout: k.Timeout / scale,
		MinClientPingInterval: k.MinClientPingInterval / scale, PermitWithoutStream: k.PermitWithoutStream,
	}
}

type goAway struct {
	code  http2.ErrCode
	debug string
}

type pingResult struct {
	goAway      *goAway
	serverPings int
	closed      bool
}

// rawPinger opens a bare HTTP/2 connection (no gRPC calls, no streams — the orchestrator's idle
// channel) and sends a PING every interval for d. It acks the server's pings when ack is true.
func rawPinger(t *testing.T, e *env, interval, d time.Duration, ack bool) pingResult {
	t.Helper()
	conn, err := tls.Dial("tcp", e.addr, e.clientTLS(t, e.set.Client))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(conn, conn)
	var wmu sync.Mutex
	write := func(f func() error) { wmu.Lock(); defer wmu.Unlock(); _ = f() }
	write(func() error { return fr.WriteSettings() })

	var mu sync.Mutex
	res := pingResult{}
	go func() {
		for {
			f, err := fr.ReadFrame()
			if err != nil {
				mu.Lock()
				res.closed = true
				mu.Unlock()
				return
			}
			switch f := f.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					write(func() error { return fr.WriteSettingsAck() })
				}
			case *http2.PingFrame:
				if !f.IsAck() {
					mu.Lock()
					res.serverPings++
					mu.Unlock()
					if ack {
						data := f.Data
						write(func() error { return fr.WritePing(true, data) })
					}
				}
			case *http2.GoAwayFrame:
				mu.Lock()
				if res.goAway == nil {
					// Frame data is only valid until the next ReadFrame: copy it.
					res.goAway = &goAway{code: f.ErrCode, debug: string(f.DebugData())}
				}
				mu.Unlock()
			}
		}
	}()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		write(func() error { return fr.WritePing(false, [8]byte{1, 2, 3, 4, 5, 6, 7, 8}) })
		time.Sleep(interval)
	}
	mu.Lock()
	defer mu.Unlock()
	return res
}

// The orchestrator pings every 10 s on idle channels (permit_without_calls). With the production
// policy scaled down 50× that is a ping every 200 ms against MinTime 20 ms: never GOAWAY.
func TestKeepaliveNoTooManyPings(t *testing.T) {
	e := start(t, scaled(server.DefaultKeepalive()))
	res := rawPinger(t, e, (10*time.Second)/scale, 2*time.Second, true)
	if res.goAway != nil {
		t.Fatalf("GOAWAY %v %q", res.goAway.code, res.goAway.debug)
	}
	if res.closed {
		t.Fatal("connection closed")
	}
	// grpc-go pings only an idle connection (grpc-js pinged regardless of activity); the
	// dead-peer test below covers the server ping and the timeout.
	// And the real client still works with orchestrator keepalive settings.
	c := agentpb.NewEgressAgentServiceClient(e.dial(t, e.set.Client))
	if _, err := c.GetAgentInfo(ctx5(t), &agentpb.AgentInfoRequest{}); err != nil {
		t.Fatal(err)
	}
}

// Control: grpc-go's default enforcement (MinTime 5 min, no pings without streams), scaled the
// same way, does send "too_many_pings" — so the test above really exercises the policy.
func TestKeepaliveDefaultPolicyWouldGoAway(t *testing.T) {
	def := server.Keepalive{Time: 10 * time.Second, Timeout: 20 * time.Second, MinClientPingInterval: 5 * time.Minute}
	e := start(t, scaled(def))
	res := rawPinger(t, e, (10*time.Second)/scale, 2*time.Second, true)
	if res.goAway == nil || res.goAway.code != http2.ErrCodeEnhanceYourCalm || !strings.Contains(res.goAway.debug, "too_many_pings") {
		t.Fatalf("expected GOAWAY too_many_pings with the grpc-go default policy, got %+v", res.goAway)
	}
}

// No ping ack within Timeout after a server ping closes the connection (20 s in production).
func TestKeepaliveTimeoutClosesDeadPeer(t *testing.T) {
	e := start(t, scaled(server.DefaultKeepalive()))
	conn, err := tls.Dial("tcp", e.addr, e.clientTLS(t, e.set.Client))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(http2.ClientPreface))
	fr := http2.NewFramer(conn, conn)
	_ = fr.WriteSettings()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	sawPing := false
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatal("server kept a peer that never acks pings")
			}
			break // closed by the server
		}
		if s, ok := f.(*http2.SettingsFrame); ok && !s.IsAck() {
			_ = fr.WriteSettingsAck()
		}
		if p, ok := f.(*http2.PingFrame); ok && !p.IsAck() {
			sawPing = true // deliberately not acked
		}
		if _, ok := f.(*http2.GoAwayFrame); ok {
			break
		}
	}
	if !sawPing {
		t.Fatal("server never pinged the idle connection")
	}
	// grpc-go clamps the server ping interval to ≥1 s, so scaled Time is 1 s here: 1 s + 0.4 s.
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("took %v to drop the dead peer", d)
	}
}
