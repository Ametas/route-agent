package firewall

import (
	"context"
	"io"
	"log/slog"
	"os"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
)

const secret = "0123456789abcdef0123456789abcdef"

func svc(fake *runner.Fake, ufw bool) *Service {
	return &Service{Auth: auth.New(secret), Runner: fake, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		LookPath: func(string) (string, error) {
			if ufw {
				return "/usr/sbin/ufw", nil
			}
			return "", os.ErrNotExist
		}}
}

func md(s string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, s))
}

// Item 32.
func TestManageFirewall(t *testing.T) {
	fake := &runner.Fake{Handler: func(c runner.Cmd) (runner.Result, error) {
		if c.Display() == "ufw allow 53/udp" {
			return runner.FailWith(c, 1, "", "ERROR")
		}
		if c.Timeout <= 0 || c.Timeout > UfwTimeout {
			t.Errorf("timeout %v", c.Timeout)
		}
		return runner.Result{}, nil
	}}
	s := svc(fake, false) // no ufw-installed check in ManageFirewall
	r, err := s.ManageFirewall(md(secret), &agentpb.FirewallPayload{OpenUdpPorts: []uint32{0, 53, 51820, 70000}, OpenTcpPorts: []uint32{443, 65535, 65536}})
	if err != nil || !r.Success || r.Message != "Successfully updated firewall rules: opened 2 UDP and 2 TCP ports." {
		t.Fatalf("%+v %v", r, err)
	}
	want := []string{"ufw allow 53/udp", "ufw allow 51820/udp", "ufw allow 443/tcp", "ufw allow 65535/tcp", "ufw reload"}
	if got := fake.Displays(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}

	fake2 := &runner.Fake{}
	r, _ = svc(fake2, true).ManageFirewall(md("bad"), &agentpb.FirewallPayload{OpenUdpPorts: []uint32{1}})
	if r.Success || r.Message != "Invalid orchestrator secret token." || len(fake2.Calls()) != 0 {
		t.Fatalf("%+v", r)
	}
	r, _ = svc(fake2, true).ManageFirewall(md(secret), &agentpb.FirewallPayload{})
	if !r.Success || r.Message != "Successfully updated firewall rules: opened 0 UDP and 0 TCP ports." {
		t.Fatalf("%+v", r)
	}
}

// Item 29 (mesh port): range check, ufw check, allow without reload.
func TestAllowMeshUDPPort(t *testing.T) {
	fake := &runner.Fake{}
	svc(fake, true).AllowMeshUDPPort(context.Background(), 51821)
	svc(fake, true).AllowMeshUDPPort(context.Background(), 0)
	svc(fake, true).AllowMeshUDPPort(context.Background(), 65536)
	svc(fake, false).AllowMeshUDPPort(context.Background(), 51821)
	if got := fake.Displays(); !reflect.DeepEqual(got, []string{"ufw allow 51821/udp"}) {
		t.Fatalf("%v", got)
	}
}

// ConfigureAwg's ufw steps: reload skipped after a failed allow; close ignores errors.
func TestOpenCloseUDP(t *testing.T) {
	fake := &runner.Fake{Handler: func(c runner.Cmd) (runner.Result, error) { return runner.FailWith(c, 1, "", "boom") }}
	s := svc(fake, true)
	if err := s.OpenUDPAndReload(context.Background(), 51820); err == nil {
		t.Fatal("error expected")
	}
	s.CloseUDPAndReload(context.Background(), 51820)
	want := []string{"ufw allow 51820/udp", "ufw delete allow 51820/udp", "ufw reload"}
	if got := fake.Displays(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

// ManageFirewall waits for locks.Firewall (shared with ApplyConfig's egress sync).
func TestManageFirewallTakesLock(t *testing.T) {
	reg := locks.New()
	fake := &runner.Fake{}
	s := svc(fake, true)
	s.Locks = reg
	_, unlock, _ := reg.Lock(context.Background(), locks.Firewall)
	ctx, cancel := context.WithTimeout(md(secret), 30*time.Millisecond)
	defer cancel()
	r, _ := s.ManageFirewall(ctx, &agentpb.FirewallPayload{OpenUdpPorts: []uint32{1}})
	if r.Success || len(fake.Calls()) != 0 {
		t.Fatalf("ran under a held firewall lock: %+v", r)
	}
	unlock()
	r, _ = s.ManageFirewall(md(secret), &agentpb.FirewallPayload{OpenUdpPorts: []uint32{1}})
	if !r.Success {
		t.Fatal(r)
	}
}
