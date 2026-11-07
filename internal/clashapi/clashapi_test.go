package clashapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/jsjson"
)

func mustParse(t *testing.T, s string) any {
	t.Helper()
	v, err := jsjson.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func iso(t *testing.T, s string) float64 {
	t.Helper()
	ms, ok := DateParse(s)
	if !ok {
		t.Fatalf("DateParse(%q) failed", s)
	}
	return ms
}

// singboxConnections.test.ts: field mapping, directions kept apart, port coerced from a string.
func TestParseMapsFields(t *testing.T) {
	recs := Parse(mustParse(t, `{"connections":[{"id":"c1","upload":1200,"download":340,"start":"2026-08-30T10:00:00.000Z",
		"metadata":{"network":"tcp","sourceIP":"203.0.113.9","destinationIP":"198.51.100.4","destinationPort":"443",
		"host":"example.com","inboundTag":"tuic-in-own","user":"user-uuid-1"}}]}`), false)
	r := recs[0]
	if r.ID != "c1" || r.User != "user-uuid-1" || r.SourceIP != "203.0.113.9" || r.DestinationIP != "198.51.100.4" ||
		r.DestinationDomain != "example.com" || r.DestinationPort != 443 || r.UploadBytes != 1200 || r.DownloadBytes != 340 ||
		r.Network != "tcp" || r.InboundTag != "tuic-in-own" || r.StartedAtUnixMs != 1788084000000 || r.Closed {
		t.Fatalf("%+v", r)
	}
	p := r.Proto()
	if p.DestinationPort != 443 || p.UploadBytes != 1200 || p.DownloadBytes != 340 || p.StartedAtUnixMs != 1788084000000 || p.User != "user-uuid-1" {
		t.Fatalf("proto %+v", p)
	}
}

func TestParseTolerance(t *testing.T) {
	// An empty user is a normal front record.
	r := Parse(mustParse(t, `{"connections":[{"upload":10,"download":20,"metadata":{"sourceIP":"203.0.113.77"}}]}`), false)[0]
	if r.User != "" || r.SourceIP != "203.0.113.77" {
		t.Fatal(r)
	}
	if !Parse(mustParse(t, `{"connections":[{"metadata":{}}]}`), true)[0].Closed {
		t.Fatal("closed flag")
	}
	// A bad field zeroes one record, not the response.
	recs := Parse(mustParse(t, `{"connections":[{"upload":"не число","download":null,"metadata":{"destinationPort":"abc"}},
		{"upload":500,"download":100,"metadata":{"destinationPort":80}}, null, 5, {"metadata":null,"upload":1e400}]}`), false)
	if len(recs) != 5 || recs[0].UploadBytes != 0 || recs[0].DestinationPort != 0 || recs[1].UploadBytes != 500 ||
		recs[1].DestinationPort != 80 || recs[4].UploadBytes != 0 {
		t.Fatalf("%+v", recs)
	}
	// JS Number() coercions of string fields.
	r = Parse(mustParse(t, `{"connections":[{"upload":" 7 ","download":"1e3","metadata":{"destinationPort":"0x1BB"}}]}`), false)[0]
	if r.UploadBytes != 7 || r.DownloadBytes != 1000 || r.DestinationPort != 443 {
		t.Fatal(r)
	}
	if Parse(mustParse(t, `{"connections":[{"start":"вчера","metadata":{}}]}`), false)[0].StartedAtUnixMs != 0 {
		t.Fatal("unparsable time must be 0")
	}
	for _, p := range []string{`{}`, `{"connections":null}`, `null`} {
		if got := Parse(mustParse(t, p), false); len(got) != 0 {
			t.Fatalf("%s → %v", p, got)
		}
	}
}

func TestDateParse(t *testing.T) {
	// V8 truncates nanoseconds to milliseconds.
	if got := iso(t, "2026-08-30T10:00:00.123456789+03:00"); got != 1788073200123 {
		t.Fatal(got)
	}
	cases := map[string]float64{
		"2026-08-30T10:00Z": 1788084000000, "2026-08": 1785542400000, "2026": 1767225600000,
		"2026-08-30t10:00:00z": 1788084000000, "+002026-08-30T10:00:00Z": 1788084000000,
		"2026-08-30T10:00:00.5Z": 1788084000500, "2026-08-30T10:00:00+0300": 1788073200000,
		"2026-02-31": 1772496000000, "2026-08-30T24:00:00Z": 1788134400000, "2026-08-30 10:00:00Z": 1788084000000,
	}
	for s, want := range cases {
		if got := iso(t, s); got != want {
			t.Errorf("%s = %v, want %v", s, got, want)
		}
	}
	for _, bad := range []string{"вчера", "2026-13-01", "2026-08-30T10:00:60Z", "2026-08-30T24:00:01Z", "2026-08-30T10:00:00+24:00", ""} {
		if _, ok := DateParse(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	// No offset with a time part → local time (as V8).
	want := float64(time.Date(2026, 8, 30, 10, 0, 0, 0, time.Local).UnixMilli())
	if got := iso(t, "2026-08-30T10:00:00"); got != want {
		t.Errorf("local: %v want %v", got, want)
	}
}

func TestReadEndpoint(t *testing.T) {
	f := fsx.FS{Root: t.TempDir()}
	write := func(s string) {
		_ = os.MkdirAll(f.Path("/etc/sing-box"), 0o755)
		if err := os.WriteFile(f.Path("/etc/sing-box/config.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if ep, _ := ReadEndpoint(f, "/etc/sing-box/config.json"); ep != nil {
		t.Fatal("missing file")
	}
	write(`{"experimental":{"clash_api":{"external_controller":" 127.0.0.1:28080 ","secret":"abc"}}}`)
	if ep, _ := ReadEndpoint(f, "/etc/sing-box/config.json"); ep == nil || *ep != (Endpoint{Address: "127.0.0.1:28080", Secret: "abc"}) {
		t.Fatal(ep)
	}
	write(`{"experimental":{"clash_api":{"external_controller":"127.0.0.1:1","secret":5}}}`)
	if ep, _ := ReadEndpoint(f, "/etc/sing-box/config.json"); ep == nil || ep.Secret != "" {
		t.Fatal(ep)
	}
	for _, s := range []string{`{"inbounds":[]}`, `{ это не json`, `{"experimental":{"clash_api":{"external_controller":"  "}}}`, `"str"`} {
		write(s)
		if ep, _ := ReadEndpoint(f, "/etc/sing-box/config.json"); ep != nil {
			t.Fatalf("%s → %v", s, ep)
		}
	}
}

// Checklist 31: exactly one GET /connections (no ?closed=true), bearer only with a secret,
// non-2xx text, transport and timeout texts.
func TestFetch(t *testing.T) {
	var mu sync.Mutex
	var urls, auths []string
	status := http.StatusOK
	body := `{"connections":[{"id":"x","metadata":{"user":"u"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		urls = append(urls, r.URL.String())
		auths = append(auths, r.Header.Get("Authorization"))
		st, b := status, body
		mu.Unlock()
		w.WriteHeader(st)
		_, _ = w.Write([]byte(b))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	c := Client{}

	recs, err := c.Fetch(context.Background(), Endpoint{Address: addr, Secret: "s"})
	if err != nil || len(recs) != 1 || recs[0].User != "u" {
		t.Fatal(recs, err)
	}
	if len(urls) != 1 || urls[0] != "/connections" || auths[0] != "Bearer s" {
		t.Fatalf("urls %q auth %q", urls, auths)
	}
	_, err = c.Fetch(context.Background(), Endpoint{Address: addr})
	mu.Lock()
	second := auths[1]
	mu.Unlock()
	if err != nil || second != "" {
		t.Fatalf("no secret must send no header: %q %v", auths, err)
	}
	mu.Lock()
	status = http.StatusUnauthorized
	mu.Unlock()
	if _, err := c.Fetch(context.Background(), Endpoint{Address: addr}); err == nil || err.Error() != "clash api responded 401 for /connections" {
		t.Fatal(err)
	}
	mu.Lock()
	status, body = http.StatusOK, "<html>"
	mu.Unlock()
	if _, err := c.Fetch(context.Background(), Endpoint{Address: addr}); err == nil || !strings.HasPrefix(err.Error(), "Unexpected token '<'") {
		t.Fatal(err)
	}
	if _, err := c.Fetch(context.Background(), Endpoint{Address: "127.0.0.1:1"}); err == nil || err.Error() != "fetch failed" {
		t.Fatal(err)
	}

	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()
	start := time.Now()
	_, err = Client{Timeout: 100 * time.Millisecond}.Fetch(context.Background(), Endpoint{Address: strings.TrimPrefix(hang.URL, "http://")})
	if err == nil || err.Error() != "The operation was aborted due to timeout" || time.Since(start) > 3*time.Second {
		t.Fatal(err)
	}
	if FetchTimeout != 5*time.Second {
		t.Fatal("default timeout must be 5 s")
	}
}

func TestGetPutAndBearer(t *testing.T) {
	type seen struct{ method, path, auth, ctype, body string }
	var mu sync.Mutex
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		mu.Lock()
		got = append(got, seen{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b[:n])})
		mu.Unlock()
		switch r.URL.Path {
		case "/proxies":
			_, _ = w.Write([]byte(`{"proxies":{"warp":{"now":"a"}}}`))
		case "/proxies/warp":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	v, err := Client{}.Get(context.Background(), Endpoint{Address: addr}, "/proxies")
	if err != nil || jsjson.Stringify(v, "") != `{"proxies":{"warp":{"now":"a"}}}` {
		t.Fatalf("get: %v %v", err, v)
	}
	if err := (Client{}).Put(context.Background(), Endpoint{Address: addr, Secret: "s"}, "/proxies/warp", []byte(`{"name":"b"}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	_, err = Client{AlwaysBearer: true}.Get(context.Background(), Endpoint{Address: addr}, "/providers/rules")
	if code, ok := StatusCode(err); !ok || code != 503 || err.Error() != "clash api responded 503 for /providers/rules" {
		t.Fatalf("status: %v", err)
	}
	if _, ok := StatusCode(nil); ok {
		t.Fatal("StatusCode(nil)")
	}
	want := []seen{
		{"GET", "/proxies", "", "", ""},
		{"PUT", "/proxies/warp", "Bearer s", "application/json", `{"name":"b"}`},
		{"GET", "/providers/rules", "Bearer", "", ""}, // "Bearer " on the wire; net/http trims it
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("requests %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}
