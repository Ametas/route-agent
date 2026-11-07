// Package dkms builds and loads the AmneziaWG kernel module from a source tree via DKMS: the Go
// port of installDkmsKernelModule and its helpers (src/services/meshTunnel.service.ts:44-264,
// spec C §9 step 4). It is the default binaries.MeshKernelInstaller; the mesh domain (group D)
// owns it and may move or replace it.
//
// Differences from TS (docs/go-rewrite/deviations.md): no shell (`lsmod | grep -w amneziawg` is
// lsmod plus a word match in Go, keeping the same failure text; `linux-headers-$(uname -r)` gets
// the release from the kernel), and `dkms status -m <name> -v <ver>` has a 30 s timeout (TS: none).
// All other per-command timeouts are the TS ones.
package dkms

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/runner"
)

// Fallbacks when dkms.conf lacks PACKAGE_NAME / PACKAGE_VERSION.
const (
	FallbackName    = "amneziawg"
	FallbackVersion = "1.0.0"
	// ModulesLoadPath persists the module across reboots (systemd-modules-load).
	ModulesLoadPath = "/etc/modules-load.d/amneziawg.conf"
)

// Per-command timeouts (spec C §9; statusTimeout is new, TS had none for the version probe).
const (
	statusTimeout     = 30 * time.Second
	removeTimeout     = 60 * time.Second
	dpkgTimeout       = 300 * time.Second
	aptUpdateTimeout  = 120 * time.Second
	aptInstallTimeout = 300 * time.Second
	makeTimeout       = 60 * time.Second
	addTimeout        = 30 * time.Second
	buildTimeout      = 180 * time.Second
	installTimeout    = 60 * time.Second
	modprobeTimeout   = 15 * time.Second
	lsmodTimeout      = 5 * time.Second
	logTailBytes      = 2000
)

// Module is the package dkms.conf describes.
type Module struct {
	Name    string
	Version string
	// SrcDir is the directory holding dkms.conf (the `make dkms-install` directory).
	SrcDir string
}

// Installer builds and loads the module.
type Installer struct {
	Runner runner.Runner
	// FS roots the fixed system paths (/lib/modules, /var/lib/dkms, /etc/modules-load.d).
	FS  fsx.FS
	Log *slog.Logger
	// KernelRelease returns `uname -r`; nil reads /proc/sys/kernel/osrelease.
	KernelRelease func() string
	// Invalidate is called after a successful install (telemetry kernel-status cache).
	Invalidate func()
}

func (in *Installer) release() string {
	if in.KernelRelease != nil {
		return in.KernelRelease()
	}
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (in *Installer) run(ctx context.Context, t time.Duration, name string, args ...string) (runner.Result, error) {
	return in.Runner.Run(ctx, runner.Cmd{Name: name, Args: args, Timeout: t})
}

// Install builds (unless already installed for the running kernel) and loads the module found
// under buildDir (a real path).
func (in *Installer) Install(ctx context.Context, buildDir string) (Module, error) {
	confPath, err := FindDkmsConf(buildDir)
	if err != nil {
		return Module{}, err
	}
	mod, err := in.parse(confPath)
	if err != nil {
		return Module{}, err
	}
	rel := in.release()
	if in.installed(ctx, mod, rel) {
		in.Log.Info("DKMS module already installed for this version and the running kernel, skipping rebuild",
			"name", mod.Name, "version", mod.Version, "kernel", rel)
	} else if err := in.build(ctx, mod, rel); err != nil {
		return Module{}, err
	}

	// The module name is hard-coded, as in TS, regardless of PACKAGE_NAME.
	if _, err := in.run(ctx, modprobeTimeout, "modprobe", "amneziawg"); err != nil {
		return Module{}, err
	}
	if err := in.lsmodHas(ctx, "amneziawg"); err != nil {
		return Module{}, err
	}
	if err := in.FS.WriteFile(ModulesLoadPath, []byte("amneziawg\n"), fsx.WriteOptions{Mode: 0o644}); err != nil {
		in.Log.Warn("Failed to write "+ModulesLoadPath+" (boot-persistence best-effort)", "err", err.Error())
	}
	if in.Invalidate != nil {
		in.Invalidate()
	}
	return mod, nil
}

func (in *Installer) build(ctx context.Context, mod Module, rel string) error {
	in.removeStaleTrees(ctx, mod.Name)
	in.Log.Info("Installing DKMS build dependencies (dkms/build-essential/linux-headers)...", "name", mod.Name, "version", mod.Version)
	if _, err := in.run(ctx, aptUpdateTimeout, "apt-get", "update", "-y"); err != nil {
		return err
	}
	if _, err := in.run(ctx, aptInstallTimeout, "apt-get", "install", "-y", "dkms", "build-essential", "linux-headers-"+rel); err != nil {
		return err
	}
	// Before `make dkms-install`: it copies the source to /usr/src, where dkms builds from.
	in.patchCompatForRunningKernel(mod.SrcDir, rel)
	if _, err := in.run(ctx, makeTimeout, "make", "-C", mod.SrcDir, "dkms-install"); err != nil {
		return err
	}
	if res, err := in.run(ctx, addTimeout, "dkms", "add", "-m", mod.Name, "-v", mod.Version); err != nil {
		if !alreadyAdded(res, err) {
			return err
		}
		in.Log.Info("DKMS module was already added (previous partial attempt), continuing to build/install", "name", mod.Name, "version", mod.Version)
	}
	_, err := in.run(ctx, buildTimeout, "dkms", "build", "-m", mod.Name, "-v", mod.Version)
	if err == nil {
		_, err = in.run(ctx, installTimeout, "dkms", "install", "-m", mod.Name, "-v", mod.Version)
	}
	if err != nil {
		if tail := in.buildLogTail(mod); tail != "" {
			return fmt.Errorf("%s\n\n--- dkms build log (tail) ---\n%s", err.Error(), tail)
		}
		return err
	}
	return nil
}

// alreadyAdded: `dkms add` on a tree left by a previous partial attempt is fine; the lowercased
// stderr||stdout||message must mention both "already" and "add".
func alreadyAdded(res runner.Result, err error) bool {
	out := res.Stderr
	if out == "" {
		out = res.Stdout
	}
	if out == "" {
		out = err.Error()
	}
	out = strings.ToLower(out)
	return strings.Contains(out, "already") && strings.Contains(out, "add")
}

func (in *Installer) installed(ctx context.Context, mod Module, rel string) bool {
	res, err := in.run(ctx, statusTimeout, "dkms", "status", "-m", mod.Name, "-v", mod.Version)
	if err != nil {
		return false
	}
	return IsDkmsInstalledForKernel(res.Stdout, rel)
}

// removeStaleTrees drops every known tree of the module, then finishes any half-configured
// package (a hosting kernel update made every apt-get install fail, 2026-10-18).
func (in *Installer) removeStaleTrees(ctx context.Context, name string) {
	res, err := in.run(ctx, statusTimeout, "dkms", "status", "-m", name)
	if err != nil {
		res.Stdout = ""
	}
	for _, v := range DkmsKnownVersions(res.Stdout, name) {
		if _, err := in.run(ctx, removeTimeout, "dkms", "remove", "-m", name, "-v", v, "--all"); err != nil {
			in.Log.Warn("Failed to remove stale DKMS tree", "name", name, "version", v, "err", err.Error())
		} else {
			in.Log.Info("Removed stale DKMS tree before reinstall", "name", name, "version", v)
		}
	}
	if _, err := in.run(ctx, dpkgTimeout, "dpkg", "--configure", "-a"); err != nil {
		in.Log.Warn("dpkg --configure -a failed; apt-get install below will report the cause", "err", err.Error())
	}
}

var wordAmneziawg = regexp.MustCompile(`(^|[^A-Za-z0-9_])amneziawg($|[^A-Za-z0-9_])`)

// lsmodHas replaces `lsmod | grep -w <name>`, keeping its failure text.
func (in *Installer) lsmodHas(ctx context.Context, name string) error {
	res, err := in.run(ctx, lsmodTimeout, "lsmod")
	if err != nil {
		return err
	}
	re := wordAmneziawg
	if name != "amneziawg" {
		re = regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(name) + `($|[^A-Za-z0-9_])`)
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if re.MatchString(line) {
			return nil
		}
	}
	return &runner.Error{Cmd: "lsmod | grep -w " + name, Code: 1}
}

func (in *Installer) buildLogTail(mod Module) string {
	b, err := in.FS.ReadFile("/var/lib/dkms/" + mod.Name + "/" + mod.Version + "/build/make.log")
	if err != nil {
		return ""
	}
	if len(b) > logTailBytes {
		b = b[len(b)-logTailBytes:]
	}
	return string(b)
}

func (in *Installer) patchCompatForRunningKernel(srcDir, rel string) {
	compatPath := filepath.Join(srcDir, "compat", "compat.h")
	compat, err1 := os.ReadFile(compatPath)
	header, err2 := in.FS.ReadFile("/lib/modules/" + rel + "/build/include/net/udp_tunnel.h")
	if err1 != nil || err2 != nil {
		return
	}
	src, dropped := PatchCompatForKernelHeaders(string(compat), string(header))
	if len(dropped) == 0 {
		return
	}
	if err := os.WriteFile(compatPath, []byte(src), 0o644); err != nil {
		in.Log.Warn("Failed to patch compat.h", "err", err.Error())
		return
	}
	in.Log.Info("Dropped compat macros: the running kernel headers already use struct sock *", "kernel", rel, "dropped", dropped)
}

// FindDkmsConf looks for <b>/src/dkms.conf, <b>/dkms.conf, then <b>/<subdir>/dkms.conf.
func FindDkmsConf(buildDir string) (string, error) {
	for _, c := range []string{filepath.Join(buildDir, "src", "dkms.conf"), filepath.Join(buildDir, "dkms.conf")} {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	entries, _ := os.ReadDir(buildDir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c := filepath.Join(buildDir, e.Name(), "dkms.conf")
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", errors.New("dkms.conf not found in extracted source tarball under " + buildDir)
}

var (
	reName    = regexp.MustCompile(`PACKAGE_NAME\s*=\s*"([^"]+)"`)
	reVersion = regexp.MustCompile(`PACKAGE_VERSION\s*=\s*"([^"]+)"`)
)

func (in *Installer) parse(confPath string) (Module, error) {
	b, err := os.ReadFile(confPath)
	if err != nil {
		return Module{}, err
	}
	m, warn := ParseDkmsConf(string(b))
	if warn {
		in.Log.Warn("Could not parse PACKAGE_NAME/PACKAGE_VERSION from dkms.conf, using fallback values", "dkmsConfPath", confPath)
	}
	m.SrcDir = filepath.Dir(confPath)
	return m, nil
}

// ParseDkmsConf extracts PACKAGE_NAME and PACKAGE_VERSION, falling back to amneziawg / 1.0.0.
// fellBack reports that at least one was missing.
func ParseDkmsConf(content string) (m Module, fellBack bool) {
	m = Module{Name: FallbackName, Version: FallbackVersion}
	n, v := reName.FindStringSubmatch(content), reVersion.FindStringSubmatch(content)
	if n != nil {
		m.Name = n[1]
	}
	if v != nil {
		m.Version = v[1]
	}
	return m, n == nil || v == nil
}

var (
	reStateSep   = regexp.MustCompile(`:\s*`)
	reInstalled  = regexp.MustCompile(`(?i)^installed\b`)
	reSetupSock  = regexp.MustCompile(`setup_udp_tunnel_sock\s*\(\s*struct\s+net\s*\*\s*\w*\s*,\s*struct\s+sock\s*\*`)
	reReleaseSoc = regexp.MustCompile(`udp_tunnel_sock_release\s*\(\s*struct\s+sock\s*\*`)
)

// IsDkmsInstalledForKernel: a `dkms status` line counts only when its state starts with
// "installed" and its comma-separated head contains exactly the running kernel release (both
// line formats: `name/ver, kernel, arch: installed` and `name, ver, kernel, arch: installed`).
func IsDkmsInstalledForKernel(statusOutput, kernelRelease string) bool {
	for _, line := range strings.Split(statusOutput, "\n") {
		parts := reStateSep.Split(line, -1)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		if !reInstalled.MatchString(strings.TrimSpace(parts[1])) {
			continue
		}
		for _, p := range strings.Split(parts[0], ",") {
			if strings.TrimSpace(p) == kernelRelease {
				return true
			}
		}
	}
	return false
}

// DkmsKnownVersions lists every version of name in `dkms status -m <name>` output, any kernel
// and state, in first-seen order (both formats, plus the short `name/ver: added`).
func DkmsKnownVersions(statusOutput, name string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, line := range strings.Split(statusOutput, "\n") {
		head := strings.TrimSpace(strings.SplitN(line, ":", 2)[0])
		parts := strings.Split(head, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		switch {
		case strings.HasPrefix(parts[0], name+"/"):
			add(parts[0][len(name)+1:])
		case parts[0] == name && len(parts) > 1:
			add(parts[1])
		}
	}
	return out
}

// PatchCompatForKernelHeaders drops the `#define setup_udp_tunnel_sock(` and
// `#define udp_tunnel_sock_release(` compat macros when the running kernel's udp_tunnel.h already
// takes `struct sock *` (Ubuntu 7.0.0-38 backport). Only the first matching macro line of each is
// removed, as String.replace did.
func PatchCompatForKernelHeaders(compatSource, udpTunnelHeader string) (string, []string) {
	var dropped []string
	src := compatSource
	for _, fn := range []struct {
		name string
		sig  *regexp.Regexp
	}{{"setup_udp_tunnel_sock", reSetupSock}, {"udp_tunnel_sock_release", reReleaseSoc}} {
		if !fn.sig.MatchString(udpTunnelHeader) {
			continue
		}
		macro := regexp.MustCompile(`(?m)^[ \t]*#define[ \t]+` + fn.name + `\(.*\n?`)
		loc := macro.FindStringIndex(src)
		if loc == nil {
			continue
		}
		src = src[:loc[0]] + src[loc[1]:]
		dropped = append(dropped, fn.name)
	}
	return src, dropped
}
