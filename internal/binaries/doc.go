// Package binaries implements group C (docs/go-rewrite/spec/c-binaries.md): every upload RPC
// (UploadSingboxBinary, UploadAwgToolsBinary, UploadAwgGoBinary, UploadCaddyBinary,
// UploadMihomoBinary, UploadRearRuleSet, UploadOlcrtcAgentSrvBinary, UploadMeshAwgKernelSource),
// UpgradeSingbox and the boot step sanitizeAwgToolsSymlink.
//
// # Shape
//
// One generic receiver (receiver.go) replaces the five copies of the TS receive loop: it
// authenticates the stream (auth.StreamGate: metadata, else the first chunk's secret), writes
// chunks synchronously into a temp file (natural backpressure), enforces a size cap, fsyncs, turns
// a write error into a success:false answer (TS crashed the agent, spec C risk 5), removes the
// temp on every exit path before the answer is sent, and keeps the per-RPC message strings of
// spec C §0.2/§11. Completion is the client half-close; is_final is never read (spec C §0.1).
//
// Each RPC then runs its apply step (validate → setcap → atomic swap keeping <target>.prev →
// restart/reload → post-check → roll back to .prev if the post-check fails), under a per-target
// lock so two uploads of the same artifact never interleave (spec C risk 7).
//
// # Apply is detached from the client stream
//
// Once the file is completely received (the client half-closed the stream), the apply step runs
// under context.WithoutCancel(stream.Context()): a client that disconnects after sending does not
// abort a swap, a restart or a DKMS build half-way (spec C §12 item 34, risk 5/8). Every external
// command still carries its own timeout (runner). Unlike TS, the cancellation no longer deletes
// the temp file under a running apply: the receiver owns the temp and removes it only after apply
// returned. A cancel before the half-close aborts the receive and removes the temp.
//
// # Interfaces for other groups
//
// Parts of some apply steps belong to other groups' domains. They are reached through small
// interfaces whose default implementations here reproduce the current TS behaviour; the owning
// group replaces them in cmd/route-agent/wire.go when it lands:
//
//   - RearHooks (group F): VerifyMihomoConfig (candidate mihomo against the live rear.yaml),
//     RestartMihomo (after a mihomo swap), ReloadAfterRuleSet (after a rule set is stored).
//     Default: DefaultRear (spec C §6, §7).
//   - OlcrtcHooks (group F): EnsureUnit (the olcrtc-agent-srv@.service template) and
//     RestartRunning (restart every running instance after a binary swap). Default:
//     DefaultOlcrtc (spec C §8).
//   - MeshKernelInstaller (group D): builds and loads the AmneziaWG kernel module from the
//     extracted source tree. Default: dkms.Installer (internal/binaries/dkms, spec C §9).
//   - Telemetry (group E): cache invalidation callbacks fired after a sing-box swap, an AWG
//     tools/go/quick swap and a kernel-module install, so the next telemetry tick re-reads the
//     version (spec C §1 step 9, §3 step 5, §9 step 4f). Nil callbacks are skipped.
package binaries
