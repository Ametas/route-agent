package telemetry

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Ametas/route-agent/internal/runner"
)

// JournalArgs is the per-stream log tail (telemetry.service.ts:40). The unit name is hard-coded
// `sing-box`, as in TS (not derived from SINGBOX_UNIT_FILE_PATH).
var JournalArgs = []string{"-u", "sing-box", "-n", "10", "-f", "--output", "cat"}

// LogLimit is the system_logs cap: the LAST 8192 JavaScript characters (UTF-16 code units).
const LogLimit = 8192

// keepBytes bounds the raw buffer: 8192 UTF-16 units never need more than 3 bytes each.
const keepBytes = 4 * LogLimit

// JournalTail starts the log follower of one stream; the process must stop when ctx is done.
type JournalTail interface {
	Start(ctx context.Context) (stdout io.ReadCloser, err error)
}

// ExecJournal is the real JournalTail: `journalctl <JournalArgs>` without a shell, SIGTERM when
// the stream ends (the TS cleanup), stderr discarded (TS left it unread).
type ExecJournal struct{}

// Start implements JournalTail. The process is reaped in the background once stdout ends.
func (ExecJournal) Start(ctx context.Context) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "journalctl", JournalArgs...)
	cmd.Env = runner.ChildEnv()
	cmd.Stderr = io.Discard
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &reaper{ReadCloser: out, wait: cmd.Wait}, nil
}

// reaper waits for the process after the pipe is closed, off the caller's goroutine.
type reaper struct {
	io.ReadCloser
	wait func() error
	once sync.Once
}

func (r *reaper) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(func() { go func() { _ = r.wait() }() })
	return err
}

// logBuffer is the per-stream system_logs buffer: journal output received since the last sent
// frame. Unlike TS (chunk.toString() per chunk), a UTF-8 sequence split across reads is kept
// whole.
type logBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *logBuffer) write(p []byte) {
	b.mu.Lock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - keepBytes; over > 0 {
		cut := over
		for cut < len(b.buf) && !utf8.RuneStart(b.buf[cut]) {
			cut++
		}
		b.buf = append(b.buf[:0], b.buf[cut:]...)
	}
	b.mu.Unlock()
}

// take returns the buffered text, capped to the last LogLimit UTF-16 units, and empties the
// buffer. An incomplete UTF-8 sequence at the end stays for the next frame. TS cleared the buffer
// right after a successful write; taking it just before the send is the same, because a failed
// send ends the stream.
func (b *logBuffer) take() string {
	b.mu.Lock()
	keep := incompleteTail(b.buf)
	text := string(b.buf[:len(b.buf)-keep])
	b.buf = append(b.buf[:0], b.buf[len(b.buf)-keep:]...)
	b.mu.Unlock()
	return lastUTF16(strings.ToValidUTF8(text, "�"), LogLimit)
}

// incompleteTail is the length of a trailing, not yet complete UTF-8 sequence (0..3).
func incompleteTail(p []byte) int {
	for n := 1; n <= 3 && n <= len(p); n++ {
		c := p[len(p)-n]
		if !utf8.RuneStart(c) {
			continue
		}
		need := 0
		switch {
		case c&0xE0 == 0xC0:
			need = 2
		case c&0xF0 == 0xE0:
			need = 3
		case c&0xF8 == 0xF0:
			need = 4
		}
		if need > n {
			return n
		}
		return 0
	}
	return 0
}

// lastUTF16 is s.slice(-limit) in UTF-16 code units, cut at a code-point boundary.
func lastUTF16(s string, limit int) string {
	units := 0
	i := len(s)
	for i > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:i])
		n := utf16.RuneLen(r)
		if n < 0 {
			n = 1
		}
		if units+n > limit {
			break
		}
		units += n
		i -= size
	}
	return s[i:]
}

// pump copies the journal into b until EOF or error.
func pump(r io.Reader, b *logBuffer) {
	chunk := make([]byte, 16<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			b.write(chunk[:n])
		}
		if err != nil {
			return
		}
	}
}
