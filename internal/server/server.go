// Package server is the agent's gRPC transport: the Go port of startServer and
// getGrpcServerCredentials (src/index.ts, spec A §1.3, §3).
//
//   - credentials.go: mTLS exactly as spec A §3.2 (required client cert, no insecure fallback,
//     the same error texts).
//   - server.go: keepalive (spec A §3.1) including the relaxed EnforcementPolicy, message limits,
//     listen address.
//   - handlers.go: Handlers, one func field per RPC; nil fields fall through to stubs.go
//     (codes.Unimplemented). Porting groups wire their packages in cmd/route-agent/wire.go.
//   - auth/: the secret check and the per-RPC auth matrix (spec A §4.3).
package server

import (
	"crypto/tls"
	"fmt"
	"math"
	"net"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/Ametas/route-agent/internal/agentpb"
)

// Keepalive mirrors the grpc-js server options of src/index.ts:126-145.
type Keepalive struct {
	// Time: the server pings every connection this often, regardless of activity (10 s).
	Time time.Duration
	// Timeout: no ping ack within this closes the connection (20 s; mirrored on the orchestrator
	// client — change both ends together, spec A §13).
	Timeout time.Duration
	// MinClientPingInterval is EnforcementPolicy.MinTime. grpc-js enforced nothing; grpc-go's
	// default (5 min, no pings without streams) would GOAWAY the orchestrator, which pings every
	// 10 s with permit_without_calls. 1 s is far below that and still stops a ping flood.
	MinClientPingInterval time.Duration
	// PermitWithoutStream allows client pings on a connection with no active calls.
	PermitWithoutStream bool
}

// DefaultKeepalive returns the production values.
func DefaultKeepalive() Keepalive {
	return Keepalive{
		Time:                  10 * time.Second,
		Timeout:               20 * time.Second,
		MinClientPingInterval: time.Second,
		PermitWithoutStream:   true,
	}
}

// MaxRecvMsgSize is grpc-js's (and grpc-go's) default receive limit: 4 MiB. Larger unary requests
// get RESOURCE_EXHAUSTED; uploads arrive in 64 KiB chunks. Send size stays unlimited.
const MaxRecvMsgSize = 4 << 20

// Options configure New.
type Options struct {
	TLS       *tls.Config
	Keepalive Keepalive
}

// New builds the gRPC server with every RPC of agent.EgressAgentService registered.
func New(h *Handlers, opt Options) *grpc.Server {
	if h == nil {
		h = &Handlers{}
	}
	ka := opt.Keepalive
	srvOpts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: ka.Time, Timeout: ka.Timeout}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             ka.MinClientPingInterval,
			PermitWithoutStream: ka.PermitWithoutStream,
		}),
		grpc.MaxRecvMsgSize(MaxRecvMsgSize),
	}
	if opt.TLS != nil {
		srvOpts = append(srvOpts, grpc.Creds(credentials.NewTLS(opt.TLS)))
	}
	s := grpc.NewServer(srvOpts...)
	agentpb.RegisterEgressAgentServiceServer(s, &service{h: h})
	return s
}

// ListenAddr builds HOST:PORT. PORT is the JS-coerced number from config; anything that is not
// an integer port fails here, as bindAsync failed in TS (exit 1). PORT=0 binds a random port,
// exactly like TS (spec A §2.2).
func ListenAddr(host string, port float64) (string, error) {
	if math.IsNaN(port) || math.IsInf(port, 0) || port != math.Trunc(port) || port < 0 || port > 65535 {
		return "", fmt.Errorf("invalid PORT %v: not a TCP port", port)
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}

// Listen opens the TCP listener for host and port.
func Listen(host string, port float64) (net.Listener, error) {
	addr, err := ListenAddr(host, port)
	if err != nil {
		return nil, err
	}
	network := "tcp"
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		network = "tcp4" // 0.0.0.0 is IPv4-only in grpc-js; Go's "tcp" would also take IPv6
	}
	return net.Listen(network, addr)
}
