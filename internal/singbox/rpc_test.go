package singbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/Ametas/route-agent/internal/agentpb"
)

// clash serves /connections from a mutable body.
type clash struct {
	mu     sync.Mutex
	body   string
	status int
	hits   int
	auth   string
}

func (h *harness) startClash(secretVal string) *clash {
	h.t.Helper()
	c := &clash{status: http.StatusOK, body: `{"connections":[]}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.hits++
		c.auth = r.Header.Get("Authorization")
		if r.URL.String() != "/connections" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(c.status)
		_, _ = w.Write([]byte(c.body))
	}))
	h.t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	h.writeConfig(`{"experimental":{"clash_api":{"external_controller":"` + addr + `","secret":"` + secretVal + `"}}}`)
	return c
}

func (c *clash) set(status int, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status, c.body = status, body
}

func bodyOf(conns ...string) string { return `{"connections":[` + strings.Join(conns, ",") + `]}` }

func connJSON(id, user string, up, down int) string {
	return `{"id":"` + id + `","upload":` + itoa(up) + `,"download":` + itoa(down) +
		`,"start":"2026-08-30T10:00:00.000Z","metadata":{"user":"` + user + `","sourceIP":"203.0.113.9","destinationIP":"198.51.100.4","destinationPort":"443","host":"example.com","network":"tcp","inboundTag":"tuic-in"}}`
}

func authedCtx() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-orchestrator-secret", secret))
}

// Checklist 1, 2 for the observation RPCs: bad secret → OK + success:false + empty lists; the
// body secret is accepted (M|B).
func TestObservationAuth(t *testing.T) {
	h := newHarness(t)
	bad := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-orchestrator-secret", "bad_secret"))
	tr, err := h.svc.GetSingBoxUserTraffic(bad, &agentpb.SingBoxUserTrafficRequest{})
	if err != nil || tr.Success || tr.Message != "Invalid orchestrator secret token." || len(tr.Entries) != 0 {
		t.Fatal(tr, err)
	}
	cn, err := h.svc.GetSingBoxConnections(bad, &agentpb.SingBoxConnectionsRequest{})
	if err != nil || cn.Success || cn.Message != "Invalid orchestrator secret token." || len(cn.Connections) != 0 {
		t.Fatal(cn, err)
	}
	tr, _ = h.svc.GetSingBoxUserTraffic(context.Background(), &agentpb.SingBoxUserTrafficRequest{OrchestratorSecret: secret})
	cn, _ = h.svc.GetSingBoxConnections(context.Background(), &agentpb.SingBoxConnectionsRequest{OrchestratorSecret: secret})
	if !tr.Success || !cn.Success {
		t.Fatal("body secret must be accepted")
	}
}

// Checklist 30.
func TestObservationNoClashAPI(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(`{"log":{"level":"info"}}`)
	tr, _ := h.svc.GetSingBoxUserTraffic(authedCtx(), &agentpb.SingBoxUserTrafficRequest{})
	if !tr.Success || tr.Message != "clash_api block absent from the applied sing-box config" || len(tr.Entries) != 0 {
		t.Fatal(tr)
	}
	cn, _ := h.svc.GetSingBoxConnections(authedCtx(), &agentpb.SingBoxConnectionsRequest{})
	if !cn.Success || cn.Message != "clash_api not configured on this node" || len(cn.Connections) != 0 {
		t.Fatal(cn)
	}
}

// Checklist 31, 32: one GET with the bearer, mapping on the wire, non-2xx text.
func TestGetSingBoxConnections(t *testing.T) {
	h := newHarness(t)
	c := h.startClash("ab12")
	c.set(http.StatusOK, bodyOf(connJSON("c1", "user-a", 1200, 340)))
	cn, _ := h.svc.GetSingBoxConnections(authedCtx(), &agentpb.SingBoxConnectionsRequest{})
	c.mu.Lock()
	hits, authz := c.hits, c.auth
	c.mu.Unlock()
	if !cn.Success || cn.Message != "OK" || len(cn.Connections) != 1 || hits != 1 || authz != "Bearer ab12" {
		t.Fatal(cn, hits, authz)
	}
	got := cn.Connections[0]
	if got.User != "user-a" || got.DestinationPort != 443 || got.UploadBytes != 1200 || got.DownloadBytes != 340 ||
		got.StartedAtUnixMs != 1788084000000 || got.Closed || got.InboundTag != "tuic-in" || got.DestinationDomain != "example.com" {
		t.Fatalf("%+v", got)
	}
	c.set(http.StatusServiceUnavailable, "")
	cn, _ = h.svc.GetSingBoxConnections(authedCtx(), &agentpb.SingBoxConnectionsRequest{})
	if cn.Success || cn.Message != "clash api responded 503 for /connections" || len(cn.Connections) != 0 {
		t.Fatal(cn)
	}
}

// Checklist 33, 34: first call primes and returns []; then increments; an HTTP error or a missing
// clash_api block leaves the baseline untouched.
func TestGetSingBoxUserTraffic(t *testing.T) {
	h := newHarness(t)
	c := h.startClash("")
	call := func() *agentpb.SingBoxUserTrafficResponse {
		r, err := h.svc.GetSingBoxUserTraffic(authedCtx(), &agentpb.SingBoxUserTrafficRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	c.set(http.StatusOK, bodyOf(connJSON("c1", "user-a", 1000, 2000), connJSON("f1", "", 5, 5)))
	if r := call(); !r.Success || r.Message != "OK" || len(r.Entries) != 0 {
		t.Fatalf("first call primes: %+v", r)
	}
	c.set(http.StatusInternalServerError, "")
	if r := call(); r.Success || r.Message != "clash api responded 500 for /connections" {
		t.Fatal(r)
	}
	c.set(http.StatusOK, bodyOf(connJSON("c1", "user-a", 1500, 2600), connJSON("c2", "user-b", 7, 3)))
	r := call()
	if !r.Success || len(r.Entries) != 2 ||
		r.Entries[0].UserUuid != "user-a" || r.Entries[0].UplinkBytes != 500 || r.Entries[0].DownlinkBytes != 600 ||
		r.Entries[1].UserUuid != "user-b" || r.Entries[1].UplinkBytes != 7 || r.Entries[1].DownlinkBytes != 3 {
		t.Fatalf("increments against the pre-error baseline: %+v", r.Entries)
	}
	saved := h.read(DefaultConfigPath)
	h.writeConfig(`{}`)
	if r := call(); r.Message != msgNoClashTraffic {
		t.Fatal(r)
	}
	h.writeConfig(saved)
	c.set(http.StatusOK, bodyOf(connJSON("c1", "user-a", 1600, 2600)))
	if r := call(); len(r.Entries) != 1 || r.Entries[0].UplinkBytes != 100 || r.Entries[0].DownlinkBytes != 0 {
		t.Fatalf("baseline survives a no-clash pass: %+v", r.Entries)
	}
}
