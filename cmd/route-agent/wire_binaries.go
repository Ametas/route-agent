package main

import (
	"github.com/Ametas/route-agent/internal/binaries"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/server"
)

// Group C wiring lives in its own file so wire.go only gains two call lines (and no import that
// would collide with the other groups' edits of the import block).

// newBinaries builds the group C service with the other groups' hooks: Rear and Olcrtc (group F),
// the Telemetry cache invalidations (group E: sing-box and AWG version caches, the mesh kernel
// status); Mesh (MeshKernelInstaller) keeps its default, dkms.Installer.
func newBinaries(d deps) *binaries.Service {
	return binaries.New(binaries.Deps{
		Auth:      d.auth,
		Runner:    d.runner,
		FS:        d.fs,
		Systemctl: d.systemctl,
		Cfg:       d.cfg,
		Log:       d.log,
		Locks:     d.locks,
		Rear:      newRear(d),            // group F
		Olcrtc:    newOlcrtc(d),          // group F
		Telemetry: telemetryCallbacks(d), // group E (wire_telemetry.go)
	})
}

// withLocks gives deps built without a registry (tests) one, so the services buildHandlers wires
// still share a single instance. run() always sets it.
func withLocks(d deps) deps {
	if d.locks == nil {
		d.locks = locks.New()
	}
	return d
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
