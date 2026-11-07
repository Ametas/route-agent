package binaries

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/Ametas/route-agent/internal/binaries/dkms"
	"github.com/Ametas/route-agent/internal/locks"
)

const tarTimeout = 60 * time.Second

// KernelModule names the module a MeshKernelInstaller built and loaded.
type KernelModule = dkms.Module

// MeshKernelInstaller builds and loads the AmneziaWG kernel module from an extracted source tree
// (group D's domain; default dkms.Installer, spec C §9 step 4).
type MeshKernelInstaller interface {
	Install(ctx context.Context, buildDir string) (KernelModule, error)
}

func newDefaultMeshInstaller(d Deps) MeshKernelInstaller {
	return &dkms.Installer{Runner: d.Runner, FS: d.FS, Log: d.Log, Invalidate: d.Telemetry.MeshKernelChanged}
}

// UploadMeshAwgKernelSource implements the RPC (spec C §9): the payload is a source tarball,
// extracted with --strip-components=1 and built against the running kernel via DKMS. The
// version and target_binary fields are ignored, as in TS.
func (s *Service) UploadMeshAwgKernelSource(stream UploadStream) error {
	return s.receive(stream, receiveSpec{
		rpc: "UploadMeshAwgKernelSource", dir: s.tempDir(), prefix: "amneziawg_kernel_src_", suffix: ".tar.gz",
		maxBytes: s.MaxKernelSrcBytes, unauthEmpty: msgNoValidSource, empty: msgNoValidSource,
		failPrefix: prefixKernelFailed, area: locks.Mesh,
	}, s.applyMeshKernel)
}

func (s *Service) applyMeshKernel(ctx context.Context, r received) (Result, error) {
	id := strings.TrimSuffix(strings.TrimPrefix(baseName(r.path), "amneziawg_kernel_src_"), ".tar.gz")
	buildDir, err := os.MkdirTemp(s.tempDir(), "amneziawg_kernel_build_"+id+"_")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(buildDir)
	if _, err := s.run(ctx, tarTimeout, "tar", "-xzf", r.path, "--strip-components=1", "-C", buildDir); err != nil {
		return Result{}, err
	}
	_ = os.Remove(r.path)
	mod, err := s.mesh.Install(ctx, buildDir)
	if err != nil {
		s.log.Error("Failed to install AmneziaWG kernel module via DKMS", "err", err.Error())
		return Result{}, err
	}
	s.log.Info("AmneziaWG kernel module built and loaded via DKMS", "name", mod.Name, "version", mod.Version)
	return ok("AmneziaWG kernel module (" + mod.Name + " v" + mod.Version + ") installed and loaded"), nil
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
