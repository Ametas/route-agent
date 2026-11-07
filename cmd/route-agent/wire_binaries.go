package main

import (
	"github.com/Ametas/route-agent/internal/binaries"
	"github.com/Ametas/route-agent/internal/server"
)

// Group C wiring lives in its own file so wire.go only gains two call lines (and no import that
// would collide with the other groups' edits of the import block).

// newBinaries builds the group C service. Groups D/E/F plug their implementations in here when
// they land: Rear (RearHooks), Olcrtc (OlcrtcHooks), Mesh (MeshKernelInstaller) and the Telemetry
// cache-invalidation callbacks; nil keeps the defaults, which reproduce the TS behaviour.
func newBinaries(d deps) *binaries.Service {
	return binaries.New(binaries.Deps{
		Auth:      d.auth,
		Runner:    d.runner,
		FS:        d.fs,
		Systemctl: d.systemctl,
		Cfg:       d.cfg,
		Log:       d.log,
	})
}

// wireBinaries assigns the group C handlers.
func wireBinaries(h *server.Handlers, b *binaries.Service) {
	h.UpgradeSingbox = b.UpgradeSingbox
	h.UploadSingboxBinary = b.UploadSingboxBinary
	h.UploadAwgToolsBinary = b.UploadAwgToolsBinary
	h.UploadAwgGoBinary = b.UploadAwgGoBinary
	h.UploadCaddyBinary = b.UploadCaddyBinary
	h.UploadMihomoBinary = b.UploadMihomoBinary
	h.UploadRearRuleSet = b.UploadRearRuleSet
	h.UploadOlcrtcAgentSrvBinary = b.UploadOlcrtcAgentSrvBinary
	h.UploadMeshAwgKernelSource = b.UploadMeshAwgKernelSource
}
