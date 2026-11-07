package olcrtc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/clashapi"
	"github.com/Ametas/route-agent/internal/config"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/runner"
)

// JournalArgs is the journalctl invocation (olcrtc.service.ts:292): the glob is interpreted by
// journalctl; `-n 0` means no backlog, only live entries.
var JournalArgs = []string{"-u", "olcrtc-agent-srv@*", "-n", "0", "-f", "--output", "json"}

// maxLine caps one journal line kept in memory; longer lines are skipped.
const maxLine = 1 << 20

// JournalTail starts the journal follower of one stream. The process must stop when ctx is done.
// wait returns once it has exited (call it after stdout hit EOF).
type JournalTail interface {
	Start(ctx context.Context) (stdout io.ReadCloser, wait func() error, err error)
}

// ExecJournal is the real JournalTail: `journalctl <JournalArgs>` without a shell, SIGTERM on
// cancellation (as the TS cleanup), stderr drained (TS never read it, so a chatty journalctl could
// block on a full pipe).
type ExecJournal struct{}

// Start implements JournalTail.
func (ExecJournal) Start(ctx context.Context) (io.ReadCloser, func() error, error) {
	cmd := exec.CommandContext(ctx, "journalctl", JournalArgs...)
	cmd.Env = runner.ChildEnv()
	cmd.Stderr = io.Discard
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return out, cmd.Wait, nil
}

func (s *Service) journal() JournalTail {
	if s.Journal != nil {
		return s.Journal
	}
	return ExecJournal{}
}

// StreamOlcrtcEvents implements the RPC (spec F §6.5): auth M|B (failure: UNAUTHENTICATED);
// every live journal entry of an olcrtc-agent-srv@ instance whose MESSAGE is a JSON event becomes
// one OlcrtcEvent. Malformed lines are skipped without ending the stream; the client's
// cancellation stops journalctl.
//
// Unlike TS, the stream ENDS with UNAVAILABLE when journalctl cannot be started or exits: TS kept
// it open and silent forever, and the orchestrator reconnects only on error/end (spec F R3,
// architecture §6 item 12).
func (s *Service) StreamOlcrtcEvents(req *agentpb.OlcrtcEventsRequest, stream grpc.ServerStreamingServer[agentpb.OlcrtcEvent]) error {
	ctx := stream.Context()
	if !s.Auth.MetadataOrBody(ctx, req.GetOrchestratorSecret()) {
		s.Log.Warn("Unauthorized StreamOlcrtcEvents request blocked")
		return status.Error(codes.Unauthenticated, "UNAUTHENTICATED: Invalid orchestrator secret token.")
	}
	s.Log.Info("OLCRTC events stream opened by orchestrator")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	out, wait, err := s.journal().Start(ctx)
	if err != nil {
		s.Log.Warn("Failed to spawn journalctl process for OLCRTC events", "err", err.Error())
		return status.Error(codes.Unavailable, "olcrtc event journal could not be started: "+err.Error())
	}
	waited := false
	defer func() {
		cancel()
		_ = out.Close()
		if !waited {
			_ = wait()
		}
		s.Log.Info("OLCRTC events stream closed and resources safely released")
	}()

	readErr := readLines(out, func(line string) error {
		ev, ok := ParseJournalLine(line, time.Now())
		if !ok {
			return nil
		}
		return stream.Send(ev)
	})
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	var sendErr *sendError
	if errors.As(readErr, &sendErr) {
		return sendErr.err
	}
	waited = true
	why := "exited"
	if werr := wait(); werr != nil {
		why = werr.Error()
	} else if readErr != nil {
		why = readErr.Error()
	}
	s.Log.Warn("journalctl for OLCRTC events ended; closing the stream so the orchestrator reconnects", "reason", why)
	return status.Error(codes.Unavailable, "olcrtc event journal (journalctl) ended: "+why)
}

type sendError struct{ err error }

func (e *sendError) Error() string { return e.err.Error() }

// readLines calls fn for every complete line (UTF-8 reassembled across reads, unlike TS's
// per-chunk toString). A line longer than maxLine is dropped. It returns nil at EOF, the read
// error, or a *sendError wrapping fn's error.
func readLines(r io.Reader, fn func(string) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	oversized := false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(buf)+len(chunk) > maxLine {
			oversized, buf = true, buf[:0]
		} else if !oversized {
			buf = append(buf, chunk...)
		}
		switch {
		case err == nil:
			if !oversized {
				line := strings.ToValidUTF8(string(buf[:len(buf)-1]), "�")
				if ferr := fn(line); ferr != nil {
					return &sendError{err: ferr}
				}
			}
			buf, oversized = buf[:0], false
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return nil
		default:
			return err
		}
	}
}

// unitPattern is /^olcrtc-agent-srv@(.+)\.service$/ (JS `.` excludes line terminators).
var unitPattern = regexp.MustCompile(`^olcrtc-agent-srv@([^\n\r\x{2028}\x{2029}]+)\.service$`)

// truthy is JS truthiness for the value model.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0 && t == t
	case string:
		return t != ""
	}
	return true
}

// str is `v || ”` as protobufjs fromObject then saw it: String(v) of a truthy value.
func str(v any) string {
	if !truthy(v) {
		return ""
	}
	return jsjson.String(v)
}

// u64 is `v || 0` encoded into a uint64 field (Long.fromValue): numbers truncated, numeric
// strings parsed, anything else 0.
func u64(v any) uint64 {
	switch t := v.(type) {
	case float64:
		return uint64(jsjson.ToInt64(t))
	case string:
		n := config.JSNumber(t)
		if n != n {
			return 0
		}
		return uint64(jsjson.ToInt64(n))
	}
	return 0
}

func get(v any, k string) any {
	o, ok := v.(*jsjson.Object)
	if !ok {
		return nil
	}
	x, _ := o.Get(k)
	return x
}

// ParseJournalLine turns one `journalctl --output json` line into an event (olcrtc.service.ts:
// 318-348); ok=false skips the line (blank, not JSON, another unit, MESSAGE not a string — journald
// emits byte arrays for non-UTF-8 —, MESSAGE not JSON, or a JSON null event).
func ParseJournalLine(line string, now time.Time) (*agentpb.OlcrtcEvent, bool) {
	if jsjson.Trim(line) == "" {
		return nil, false
	}
	entry, err := jsjson.Parse(line)
	if err != nil || entry == nil {
		return nil, false
	}
	m := unitPattern.FindStringSubmatch(str(get(entry, "_SYSTEMD_UNIT")))
	if m == nil {
		return nil, false
	}
	msg, ok := get(entry, "MESSAGE").(string)
	if !ok || msg == "" {
		return nil, false
	}
	ev, err := jsjson.Parse(msg)
	if err != nil || ev == nil {
		return nil, false
	}
	ts := now.UnixMilli()
	if t := get(ev, "ts"); truthy(t) {
		if ms, ok := clashapi.DateParse(jsjson.String(t)); ok && ms != 0 {
			ts = jsjson.ToInt64(ms)
		}
	}
	health := ""
	if h := get(ev, "health"); truthy(h) {
		health = jsjson.Stringify(h, "")
	}
	return &agentpb.OlcrtcEvent{
		UserShortUuid: m[1],
		Type:          str(get(ev, "type")),
		Timestamp:     ts,
		SessionId:     str(get(ev, "session_id")),
		DeviceId:      str(get(ev, "device_id")),
		Reason:        str(get(ev, "reason")),
		Addr:          str(get(ev, "addr")),
		BytesIn:       u64(get(ev, "bytes_in")),
		BytesOut:      u64(get(ev, "bytes_out")),
		HealthJson:    health,
	}, true
}
