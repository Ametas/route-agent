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
	"sync/atomic"
	"time"

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

// DefaultReleaseBaseURL serves the latest release's assets by name (GitHub redirects
// /releases/latest/download/<asset> to the asset of the newest non-prerelease release).
const DefaultReleaseBaseURL = "https://github.com/Ametas/route-agent/releases/latest/download"

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

// Updater performs SelfUpdate.
type Updater struct {
	Auth      *auth.Verifier
	Runner    runner.Runner
	Systemctl systemd.Systemctl
	HTTP      *http.Client
	Log       *slog.Logger

	// BinaryPath is the installed agent binary that gets replaced (the running executable).
	BinaryPath string
	// ReleaseBaseURL is where assets are fetched from; empty means DefaultReleaseBaseURL.
	ReleaseBaseURL string
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

	running atomic.Bool
}

// SelfUpdate is the RPC handler.
func (u *Updater) SelfUpdate(ctx context.Context, _ *agentpb.SelfUpdatePayload) (*agentpb.SelfUpdateResponse, error) {
	if !u.Auth.Metadata(ctx) {
		u.Log.Warn("Unauthorized SelfUpdate request blocked")
		return &agentpb.SelfUpdateResponse{Success: false, Message: auth.InvalidSecretMessage}, nil
	}
	if !u.running.CompareAndSwap(false, true) {
		u.Log.Warn("SelfUpdate requested while an update is already running; ignoring")
		return &agentpb.SelfUpdateResponse{Success: true, Message: MsgAlreadyRunning}, nil
	}
	go u.background()
	return &agentpb.SelfUpdateResponse{Success: true, Message: MsgInitiated}, nil
}

func (u *Updater) background() {
	defer u.running.Store(false)
	time.Sleep(u.Delay)
	budget := u.Budget
	if budget <= 0 {
		budget = DefaultTotalBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	u.Log.Info("Starting agent self-update sequence...")
	err := u.Run(ctx)
	if err != nil {
		u.Log.Error("Failed to execute self-update sequence", "err", err.Error())
	}
	if u.OnDone != nil {
		u.OnDone(err)
	}
}

// Run does the update synchronously: fetch checksums, download and verify the binary, smoke
// test it, swap it in (keeping .prev) and restart the unit. Any failure before the swap leaves
// the installed binary untouched.
func (u *Updater) Run(ctx context.Context) error {
	base := strings.TrimRight(u.ReleaseBaseURL, "/")
	if base == "" {
		base = DefaultReleaseBaseURL
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
	u.Log.Info("Agent binary replaced; restarting", "version", newVersion, "prev", PrevPath(u.BinaryPath))

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
