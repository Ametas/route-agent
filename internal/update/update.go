// Package update implements SelfUpdate (spec A §8) the way architecture §3 decided for the Go
// agent: no git, npm or build on the node; the release binary is downloaded from GitHub,
// verified and swapped in. Asset naming and the release contract: docs/go-rewrite/update.md.
//
// What stays as in TS (src/services/system.service.ts:94-120):
//   - auth: metadata only; failure → {success:false, message:"Invalid orchestrator secret token."};
//   - the ack is sent first, the work starts about 1 s later, and ends with
//     `systemctl restart route-agent` (the orchestrator then waits ≤180 s for the next telemetry
//     stream and re-checks GetAgentInfo);
//   - a failed update logs an error and leaves the running agent serving.
//
// What changed (docs/go-rewrite/deviations.md): a lock (a second call while one runs is answered
// "already in progress" and starts nothing — TS ran two pipelines in parallel, R-5), timeouts on
// every download and command, SHA-256 verification against the release checksums file, a
// `--version` smoke test of the candidate, an atomic swap, and route-agent.prev kept for rollback.
//
// Channels (docs/go-rewrite/update.md): `stable` takes the latest release, `nightly` the moving
// prerelease built from branch alpha. The orchestrator picks one with the metadata header
// x-agent-channel (the proto stays as it is); without it the channel persisted next to the binary
// is used. A reply to a call with the header carries the token `channel=<value>`, by which the
// orchestrator tells an agent that understood the header from an old one that ignored it.
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/metadata"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
	"github.com/Ametas/route-agent/internal/systemd"
	"github.com/Ametas/route-agent/internal/version"
)

// Messages of the ack. The initiated text is kept verbatim from TS (the orchestrator shows it,
// repository tests match "Self-update sequence initiated").
const (
	MsgInitiated      = "Self-update sequence initiated. Agent will pull code and restart in 1 second."
	MsgAlreadyRunning = "Self-update sequence already in progress."
)

// Update channels and the metadata header that selects one (exact lowercase values).
const (
	ChannelStable  = "stable"
	ChannelNightly = "nightly"
	ChannelHeader  = "x-agent-channel"
)

// DefaultReleaseBaseURL serves the latest release's assets by name (GitHub redirects
// /releases/latest/download/<asset> to the asset of the newest non-prerelease release).
const DefaultReleaseBaseURL = "https://github.com/Ametas/route-agent/releases/latest/download"

// NightlyReleaseBaseURL serves the assets of the prerelease `nightly`, which CI rebuilds on every
// push to branch alpha.
const NightlyReleaseBaseURL = "https://github.com/Ametas/route-agent/releases/download/nightly"

// ChannelFileName is the file next to the installed binary that holds the channel of the
// installed build: one line, `stable` or `nightly`. bridge-to-go.sh and install-go.sh write it too.
const ChannelFileName = "channel"

// ChecksumsAsset is the sha256sum-format file attached to every release.
const ChecksumsAsset = "checksums.txt"

// Limits and timeouts.
const (
	MaxBinarySize      = 256 << 20
	MaxChecksumsSize   = 1 << 20
	ChecksumsTimeout   = 30 * time.Second
	DownloadTimeout    = 3 * time.Minute
	VersionTimeout     = 15 * time.Second
	RestartTimeout     = 60 * time.Second
	DefaultTotalBudget = 5 * time.Minute
)

// AssetName is the release asset for a GOARCH: route-agent-linux-<arch>.
func AssetName(goarch string) string { return "route-agent-linux-" + goarch }

// PrevPath is where the replaced binary is kept: <binary>.prev.
func PrevPath(binaryPath string) string { return binaryPath + ".prev" }

// ChannelPath is the persisted channel file for a binary: <dir of binary>/channel.
func ChannelPath(binaryPath string) string {
	return filepath.Join(filepath.Dir(binaryPath), ChannelFileName)
}

// ValidChannel reports whether ch is a known channel.
func ValidChannel(ch string) bool { return ch == ChannelStable || ch == ChannelNightly }

// ReleaseBaseURLFor is the default asset base of a channel.
func ReleaseBaseURLFor(channel string) string {
	if channel == ChannelNightly {
		return NightlyReleaseBaseURL
	}
	return DefaultReleaseBaseURL
}

// ReadChannel returns the persisted channel; a missing, unreadable or garbage file means stable.
func ReadChannel(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 64 {
		return ChannelStable
	}
	if ch := strings.TrimSpace(string(data)); ValidChannel(ch) {
		return ch
	}
	return ChannelStable
}

// WriteChannel persists channel atomically (temp + rename), mode 0644.
func WriteChannel(path, channel string) error {
	if !ValidChannel(channel) {
		return fmt.Errorf("unknown update channel %q", channel)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".new-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := io.WriteString(tmp, channel+"\n"); err != nil {
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	syncDir(dir)
	return nil
}

// channelFromMetadata returns the x-agent-channel header and whether it was sent at all. An
// empty value counts as sent (and is refused as an unknown channel).
func channelFromMetadata(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	vals := md.Get(ChannelHeader)
	if len(vals) == 0 {
		return "", false
	}
	return vals[0], true
}

// unknownChannelMessage quotes at most 32 characters of the refused value, kept valid UTF-8 so
// the reply still marshals.
func unknownChannelMessage(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if utf8.RuneCountInString(value) > 32 {
		value = string([]rune(value)[:32])
	}
	return fmt.Sprintf("Unknown update channel '%s'.", value)
}

func initiatedMessage(channel string) string {
	return fmt.Sprintf("Self-update sequence initiated (channel=%s). Agent will pull code and restart in 1 second.", channel)
}

func alreadyRunningMessage(channel string) string {
	return fmt.Sprintf("Self-update sequence already in progress (channel=%s).", channel)
}

// Updater performs SelfUpdate.
type Updater struct {
	Auth      *auth.Verifier
	Runner    runner.Runner
	Systemctl systemd.Systemctl
	HTTP      *http.Client
	Log       *slog.Logger

	// BinaryPath is the installed agent binary that gets replaced (the running executable).
	BinaryPath string
	// ReleaseBaseURLs overrides the asset base per channel (tests); a channel without an entry
	// uses ReleaseBaseURLFor.
	ReleaseBaseURLs map[string]string
	// Arch is the GOARCH whose asset is fetched.
	Arch string
	// Unit is restarted at the end ("route-agent").
	Unit string
	// Delay between the ack and the start of the work (1 s, as in TS).
	Delay time.Duration
	// Budget bounds the whole background run; 0 means DefaultTotalBudget.
	Budget time.Duration
	// OnDone, if set, receives the outcome of each background run (tests).
	OnDone func(error)

	mu             sync.Mutex
	running        bool
	runningChannel string
}

// SelfUpdate is the RPC handler.
func (u *Updater) SelfUpdate(ctx context.Context, _ *agentpb.SelfUpdatePayload) (*agentpb.SelfUpdateResponse, error) {
	if !u.Auth.Metadata(ctx) {
		u.Log.Warn("Unauthorized SelfUpdate request blocked")
		return &agentpb.SelfUpdateResponse{Success: false, Message: auth.InvalidSecretMessage}, nil
	}
	requested, sent := channelFromMetadata(ctx)
	if sent && !ValidChannel(requested) {
		msg := unknownChannelMessage(requested)
		u.Log.Warn("SelfUpdate rejected: unknown update channel", "message", msg)
		return &agentpb.SelfUpdateResponse{Success: false, Message: msg}, nil
	}
	channel := requested
	if !sent {
		channel = ReadChannel(ChannelPath(u.BinaryPath))
	}

	u.mu.Lock()
	if u.running {
		runningChannel := u.runningChannel
		u.mu.Unlock()
		u.Log.Warn("SelfUpdate requested while an update is already running; ignoring", "channel", runningChannel)
		msg := MsgAlreadyRunning
		if sent {
			msg = alreadyRunningMessage(runningChannel)
		}
		return &agentpb.SelfUpdateResponse{Success: true, Message: msg}, nil
	}
	u.running, u.runningChannel = true, channel
	u.mu.Unlock()

	go u.background(channel)
	msg := MsgInitiated
	if sent {
		msg = initiatedMessage(channel)
	}
	return &agentpb.SelfUpdateResponse{Success: true, Message: msg}, nil
}

func (u *Updater) background(channel string) {
	defer func() {
		u.mu.Lock()
		u.running, u.runningChannel = false, ""
		u.mu.Unlock()
	}()
	time.Sleep(u.Delay)
	budget := u.Budget
	if budget <= 0 {
		budget = DefaultTotalBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	u.Log.Info("Starting agent self-update sequence...", "channel", channel)
	err := u.Run(ctx, channel)
	if err != nil {
		u.Log.Error("Failed to execute self-update sequence", "err", err.Error())
	}
	if u.OnDone != nil {
		u.OnDone(err)
	}
}

// Run does the update synchronously from channel: fetch checksums, download and verify the
// binary, smoke test it, swap it in (keeping .prev), persist the channel and restart the unit. Any
// failure before the swap leaves the installed binary and the channel file untouched.
func (u *Updater) Run(ctx context.Context, channel string) error {
	if !ValidChannel(channel) {
		return fmt.Errorf("unknown update channel %q", channel)
	}
	base := strings.TrimRight(u.ReleaseBaseURLs[channel], "/")
	if base == "" {
		base = ReleaseBaseURLFor(channel)
	}
	asset := AssetName(u.Arch)

	sums, err := u.fetch(ctx, base+"/"+ChecksumsAsset, ChecksumsTimeout, MaxChecksumsSize)
	if err != nil {
		return fmt.Errorf("download %s: %w", ChecksumsAsset, err)
	}
	want, err := lookupChecksum(sums, asset)
	if err != nil {
		return err
	}

	dir := filepath.Dir(u.BinaryPath)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(u.BinaryPath)+".new-*")
	if err != nil {
		return fmt.Errorf("create temp binary: %w", err)
	}
	tmpName := tmp.Name()
	swapped := false
	defer func() {
		_ = tmp.Close()
		if !swapped {
			_ = os.Remove(tmpName)
		}
	}()

	got, err := u.download(ctx, base+"/"+asset, tmp)
	if err != nil {
		return fmt.Errorf("download %s: %w", asset, err)
	}
	if got != want {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", asset, want, got)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	res, err := u.Runner.Run(ctx, runner.Cmd{Name: tmpName, Args: []string{"--version"}, Timeout: VersionTimeout})
	if err != nil {
		return fmt.Errorf("new binary failed its --version check: %w", err)
	}
	newVersion := strings.TrimSpace(res.Stdout)
	if !version.Valid(newVersion) {
		return fmt.Errorf("new binary reports version %q, which is not vX.Y.Z[-N-gHASH]", newVersion)
	}

	if err := keepPrevious(u.BinaryPath); err != nil {
		return fmt.Errorf("keep previous binary: %w", err)
	}
	if err := os.Rename(tmpName, u.BinaryPath); err != nil {
		return fmt.Errorf("swap binary: %w", err)
	}
	swapped = true
	syncDir(dir)
	// The channel file follows the installed binary. A failure here does not stop the restart:
	// the new binary is already in place, and a half-done update (new file on disk, old process
	// running) is worse than a stale channel, which the next update with the header corrects.
	if err := WriteChannel(ChannelPath(u.BinaryPath), channel); err != nil {
		u.Log.Error("Could not persist the update channel", "channel", channel, "err", err.Error())
	}
	u.Log.Info("Agent binary replaced; restarting", "version", newVersion, "channel", channel, "prev", PrevPath(u.BinaryPath))

	sc := u.Systemctl
	if sc.Timeout <= 0 {
		sc.Timeout = RestartTimeout
	}
	// The restart kills this process; systemd has the job queued by then (spec A §1.4).
	if err := sc.Restart(ctx, u.Unit); err != nil {
		return fmt.Errorf("restart %s: %w", u.Unit, err)
	}
	return nil
}

func (u *Updater) client() *http.Client {
	if u.HTTP != nil {
		return u.HTTP
	}
	return http.DefaultClient
}

func (u *Updater) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "route-agent-selfupdate")
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func (u *Updater) fetch(ctx context.Context, url string, timeout time.Duration, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := u.get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return data, nil
}

// download streams url into w and returns the lower-case hex SHA-256 of what was written.
func (u *Updater) download(ctx context.Context, url string, w io.Writer) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, DownloadTimeout)
	defer cancel()
	resp, err := u.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, MaxBinarySize+1))
	if err != nil {
		return "", err
	}
	if n > MaxBinarySize {
		return "", fmt.Errorf("larger than %d bytes", MaxBinarySize)
	}
	if n == 0 {
		return "", errors.New("empty body")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// lookupChecksum finds asset in a sha256sum-format file ("<hex>  <name>" or "<hex> *<name>").
func lookupChecksum(sums []byte, asset string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == asset {
			sum := strings.ToLower(fields[0])
			if len(sum) != 64 {
				return "", fmt.Errorf("malformed checksum for %s", asset)
			}
			if _, err := hex.DecodeString(sum); err != nil {
				return "", fmt.Errorf("malformed checksum for %s", asset)
			}
			return sum, nil
		}
	}
	return "", fmt.Errorf("%s has no entry for %s", ChecksumsAsset, asset)
}

// keepPrevious copies the installed binary to <binary>.prev atomically (temp + rename). A missing
// installed binary is not an error: there is nothing to keep.
func keepPrevious(binaryPath string) error {
	src, err := os.Open(binaryPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer src.Close()
	dir := filepath.Dir(binaryPath)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(binaryPath)+".prev-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(tmp, src); err != nil {
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, PrevPath(binaryPath)); err != nil {
		return err
	}
	ok = true
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
