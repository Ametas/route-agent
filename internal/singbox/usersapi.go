package singbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/jsjson"
)

// UsersAPITimeout bounds one PUT to the users-api socket (TS had none, spec B R5: a hung fork
// socket blocked ApplyConfig forever).
const UsersAPITimeout = 10 * time.Second

// UsersAPI is the client of the fork's users-api (putRoster/applyRosterUpdates,
// hotUsers.ts:242-281).
type UsersAPI struct {
	// SocketPath is the agent path of the socket; "" means UsersAPISocketPath.
	SocketPath string
	// Timeout per request; 0 means UsersAPITimeout.
	Timeout time.Duration
}

// encodeURIComponent keeps A–Z a–z 0–9 - _ . ! ~ * ' ( ) and percent-encodes every other UTF-8
// byte (uppercase hex), as JavaScript does.
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// Put sends one roster: `PUT /inbounds/<encodeURIComponent(tag)>/users` with the compact JSON
// array as body. Only 200 is success; anything else is
// "users-api ответила <status> на инбаунд <tag>: <trimmed body>".
func (u UsersAPI) Put(ctx context.Context, f fsx.FS, upd RosterUpdate) error {
	socket := f.Path(orDefault(u.SocketPath, UsersAPISocketPath))
	timeout := u.Timeout
	if timeout <= 0 {
		timeout = UsersAPITimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()

	enc := encodeURIComponent(upd.Tag)
	reqURL := &url.URL{Scheme: "http", Host: "localhost", Path: "/inbounds/" + upd.Tag + "/users", RawPath: "/inbounds/" + enc + "/users"}
	body := []byte(jsjson.Stringify(upd.Users, ""))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("users-api ответила %d на инбаунд %s: %s", resp.StatusCode, upd.Tag, jsjson.Trim(string(detail)))
}

// Apply sends the updates sequentially; the first failure stops the rest (the caller then
// reloads from the file it already wrote, so a partial application converges).
func (u UsersAPI) Apply(ctx context.Context, f fsx.FS, log *slog.Logger, plan []RosterUpdate) error {
	for _, upd := range plan {
		if err := u.Put(ctx, f, upd); err != nil {
			return err
		}
		log.Info("Набор абонентов инбаунда заменён на живую, без перезагрузки ядра", "tag", upd.Tag, "users", len(upd.Users))
	}
	return nil
}
