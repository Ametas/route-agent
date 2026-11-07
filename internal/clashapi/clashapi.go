// Package clashapi reads sing-box's local Clash API (src/utils/singboxConnections.ts, spec B §7):
// where it listens (from the APPLIED config, not from agent settings, so the two can never
// disagree), one GET /connections, and the tolerant parse of its records. GetSingBoxConnections
// and GetSingBoxUserTraffic (internal/singbox) use it; the rear core's clash API (group F) can
// reuse Parse and Fetch.
package clashapi

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/jsjson"
)

// FetchTimeout bounds the whole GET (AbortSignal.timeout(5000) in TS).
const FetchTimeout = 5 * time.Second

// maxBody caps the /connections response kept in memory.
const maxBody = 256 << 20

// Endpoint is the external_controller address and secret of a clash_api block.
type Endpoint struct {
	Address string
	Secret  string
}

// ReadEndpoint reads the config at path and returns experimental.clash_api, or nil (with the
// reason) when the file is missing or unparsable or the block/address is absent or blank.
func ReadEndpoint(f fsx.FS, path string) (*Endpoint, error) {
	raw, err := f.ReadFile(path)
	if err != nil {
		return nil, err
	}
	parsed, err := jsjson.Parse(string(raw))
	if err != nil {
		return nil, err
	}
	api := get(get(parsed, "experimental"), "clash_api")
	addr, ok := get(api, "external_controller").(string)
	if !ok || jsjson.Trim(addr) == "" {
		return nil, nil
	}
	secret, _ := get(api, "secret").(string)
	return &Endpoint{Address: jsjson.Trim(addr), Secret: secret}, nil
}

// Record is one connection as parsed (numbers are JS doubles until they go on the wire).
type Record struct {
	ID                string
	User              string
	SourceIP          string
	DestinationIP     string
	DestinationDomain string
	DestinationPort   float64
	UploadBytes       float64
	DownloadBytes     float64
	Network           string
	InboundTag        string
	StartedAtUnixMs   float64
	Closed            bool
}

// Proto converts a record the way protobufjs did: uint32 by ToUint32, int64 by truncation;
// the internal id is not on the wire.
func (r Record) Proto() *agentpb.SingBoxConnection {
	return &agentpb.SingBoxConnection{
		User:              r.User,
		SourceIp:          r.SourceIP,
		DestinationIp:     r.DestinationIP,
		DestinationDomain: r.DestinationDomain,
		DestinationPort:   jsjson.ToUint32(r.DestinationPort),
		UploadBytes:       jsjson.ToInt64(r.UploadBytes),
		DownloadBytes:     jsjson.ToInt64(r.DownloadBytes),
		Network:           r.Network,
		InboundTag:        r.InboundTag,
		StartedAtUnixMs:   jsjson.ToInt64(r.StartedAtUnixMs),
		Closed:            r.Closed,
	}
}

// ToProto converts a list of records.
func ToProto(records []Record) []*agentpb.SingBoxConnection {
	out := make([]*agentpb.SingBoxConnection, 0, len(records))
	for _, r := range records {
		out = append(out, r.Proto())
	}
	return out
}

// get is optional chaining `v?.[k]`: a missing key or a non-object yields nil (undefined).
func get(v any, k string) any {
	o, ok := v.(*jsjson.Object)
	if !ok {
		return nil
	}
	x, _ := o.Get(k)
	return x
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asNumber(v any) float64 {
	switch t := v.(type) {
	case float64:
		if !math.IsNaN(t) && !math.IsInf(t, 0) {
			return t
		}
	case string:
		n := config.JSNumber(t)
		if !math.IsNaN(n) && !math.IsInf(n, 0) {
			return n
		}
	}
	return 0
}

func parseStart(v any) float64 {
	s, ok := v.(string)
	if !ok || s == "" {
		return 0
	}
	ms, ok := DateParse(s)
	if !ok {
		return 0
	}
	return ms
}

// Parse is parseClashConnections: payload.connections (anything else → empty list), each entry
// coerced field by field — a bad field zeroes only itself.
func Parse(payload any, closed bool) []Record {
	list, ok := get(payload, "connections").([]any)
	if !ok {
		return []Record{}
	}
	out := make([]Record, 0, len(list))
	for _, raw := range list {
		meta := get(raw, "metadata")
		out = append(out, Record{
			ID:                asString(get(raw, "id")),
			User:              asString(get(meta, "user")),
			SourceIP:          asString(get(meta, "sourceIP")),
			DestinationIP:     asString(get(meta, "destinationIP")),
			DestinationDomain: asString(get(meta, "host")),
			DestinationPort:   asNumber(get(meta, "destinationPort")),
			UploadBytes:       asNumber(get(raw, "upload")),
			DownloadBytes:     asNumber(get(raw, "download")),
			Network:           asString(get(meta, "network")),
			InboundTag:        asString(get(meta, "inboundTag")),
			StartedAtUnixMs:   parseStart(get(raw, "start")),
			Closed:            closed,
		})
	}
	return out
}

// Client fetches /connections.
type Client struct {
	// HTTP is the client to use; nil means a plain client (the per-call timeout still applies).
	HTTP *http.Client
	// Timeout overrides FetchTimeout (tests).
	Timeout time.Duration
}

// Fetch is fetchConnections: exactly ONE `GET http://<address>/connections` (there is no
// ?closed=true endpoint — asking for it double-counted every connection until 2026-09-15),
// `authorization: Bearer <secret>` only with a non-empty secret, a 5 s bound on the whole call.
// Error texts follow undici's so log lines read as before: "clash api responded <code> for
// /connections", "fetch failed", "The operation was aborted due to timeout", V8's JSON error.
func (c Client) Fetch(ctx context.Context, ep Endpoint) ([]Record, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = FetchTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	const p = "/connections"
	rawURL := "http://" + ep.Address + p
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, errors.New("Failed to parse URL from " + rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("Failed to parse URL from " + rawURL)
	}
	if ep.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+ep.Secret)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, transportError(ctx)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, errors.New("clash api responded " + strconv.Itoa(resp.StatusCode) + " for " + p)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, transportError(ctx)
	}
	payload, err := jsjson.Parse(string(body))
	if err != nil {
		return nil, err
	}
	return Parse(payload, false), nil
}

func transportError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("The operation was aborted due to timeout")
	}
	return errors.New("fetch failed")
}
