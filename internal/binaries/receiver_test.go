package binaries

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ametas/route-agent/internal/runner"
	"github.com/Ametas/route-agent/internal/server/auth"
)

type rpcCase struct {
	name        string
	fn          func(*Service) func(UploadStream) error
	unauthEmpty string // checklist 4
	empty       string // checklist 5
	dirs        func(h *harness) []string
}

func rpcs() []rpcCase {
	bin := func(h *harness) []string { return []string{h.path("/usr/local/bin"), h.tmp} }
	return []rpcCase{
		{"UploadSingboxBinary", func(s *Service) func(UploadStream) error { return s.UploadSingboxBinary }, auth.InvalidSecretMessage, msgNoData, bin},
		{"UploadAwgToolsBinary", func(s *Service) func(UploadStream) error { return s.UploadAwgToolsBinary }, msgNoValidBinary, msgNoValidBinary, bin},
		{"UploadAwgGoBinary", func(s *Service) func(UploadStream) error { return s.UploadAwgGoBinary }, msgNoValidBinary, msgNoValidBinary, bin},
		{"UploadCaddyBinary", func(s *Service) func(UploadStream) error { return s.UploadCaddyBinary }, msgNoValidBinary, msgNoValidBinary, bin},
		{"UploadMihomoBinary", func(s *Service) func(UploadStream) error { return s.UploadMihomoBinary }, auth.InvalidSecretMessage, msgNoData, bin},
		{"UploadRearRuleSet", func(s *Service) func(UploadStream) error { return s.UploadRearRuleSet }, auth.InvalidSecretMessage, msgNoData, bin},
		{"UploadOlcrtcAgentSrvBinary", func(s *Service) func(UploadStream) error { return s.UploadOlcrtcAgentSrvBinary }, auth.InvalidSecretMessage, msgNoData, bin},
		{"UploadMeshAwgKernelSource", func(s *Service) func(UploadStream) error { return s.UploadMeshAwgKernelSource }, msgNoValidSource, msgNoValidSource, bin},
	}
}

// Checklist 2: a bad secret with ≥1 chunk → InvalidSecret with status OK, nothing applied, no temp.
// Checklist 3: bad metadata + first chunk without secret + later chunk with the secret → rejected.
func TestUnauthorizedStreamsAllRPCs(t *testing.T) {
	for _, rc := range rpcs() {
		t.Run(rc.name, func(t *testing.T) {
			for name, msgs := range map[string][]*agentpbChunk{
				"wrong secret":          {ch("nope", goodELF("x"), "1", "")},
				"secret only later":     {ch("", goodELF("x"), "1", ""), ch(secret, "more", "", "")},
				"no secret anywhere":    {ch("", "", "1", "")},
				"bad meta, good later?": {ch("bad", "a", "", ""), ch(secret, "b", "", "")},
			} {
				h := newHarness(t, nil)
				st := &fakeStream{ctx: mdCtx("wrong-metadata")}
				st.msgs = msgs
				if err := rc.fn(h.svc)(st); err != nil {
					t.Fatalf("%s: gRPC error %v (must be status OK)", name, err)
				}
				expect(t, st.resp, false, auth.InvalidSecretMessage)
				if st.i > 1 {
					t.Errorf("%s: kept reading after the rejection (%d messages)", name, st.i)
				}
				if len(h.displays()) != 0 {
					t.Errorf("%s: commands ran: %v", name, h.displays())
				}
				if l := leftovers(t, rc.dirs(h)...); len(l) != 0 {
					t.Errorf("%s: temp files left: %v", name, l)
				}
			}
		})
	}
}

// Checklist 4 and 5: zero chunks with a bad metadata secret, and authorized streams without bytes.
func TestEmptyStreamsAllRPCs(t *testing.T) {
	for _, rc := range rpcs() {
		t.Run(rc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			expect(t, h.upload(rc.fn(h.svc), mdCtx("bad")), false, rc.unauthEmpty)
			expect(t, h.upload(rc.fn(h.svc), mdCtx("")), false, rc.unauthEmpty)
			expect(t, h.upload(rc.fn(h.svc), mdCtx(secret)), false, rc.empty)
			expect(t, h.upload(rc.fn(h.svc), mdCtx(secret), ch("", "", "1.0", "x"), ch("", "", "", "")), false, rc.empty)
			expect(t, h.upload(rc.fn(h.svc), mdCtx(""), ch(secret, "", "1.0", "")), false, rc.empty)
			if len(h.displays()) != 0 {
				t.Errorf("apply ran: %v", h.displays())
			}
			if l := leftovers(t, rc.dirs(h)...); len(l) != 0 {
				t.Errorf("temp files left: %v", l)
			}
		})
	}
}

// Checklist 1: the secret in metadata only, or in the first chunk only, is accepted; a wrong
// payload secret after a good metadata secret is ignored (sticky).
func TestSecretSources(t *testing.T) {
	h := newHarness(t, nil)
	expect(t, h.upload(h.svc.UploadRearRuleSet, mdCtx(secret), ch("", "abc", "", "geoip-ru")), true, "Набор правил geoip-ru записан (3 байт)")
	expect(t, h.upload(h.svc.UploadRearRuleSet, mdCtx(""), ch(secret, "abcd", "", "geoip-ru")), true, "Набор правил geoip-ru записан (4 байт)")
	expect(t, h.upload(h.svc.UploadRearRuleSet, mdCtx(secret), ch("wrong", "ab", "", "geoip-ru"), ch("wrong", "c", "", "")), true, "Набор правил geoip-ru записан (3 байт)")
	// first chunk verifies, later chunks without a secret are fine
	expect(t, h.upload(h.svc.UploadRearRuleSet, mdCtx("bad"), ch(secret, "a", "", "x1"), ch("", "b", "", "")), true, "Набор правил x1 записан (2 байт)")
}

// Checklist 6, 7, 8: byte-exact assembly in order, last non-empty version/target, is_final ignored.
func TestAssemblyVersionTargetAndIsFinal(t *testing.T) {
	h := newHarness(t, nil)
	m1, m2, m3 := ch(secret, "one-", "", "first"), ch("", "two-", "", ""), ch("", "three", "", "category-ai-!cn")
	m2.IsFinal = true // a middle chunk marked final changes nothing
	r := h.upload(h.svc.UploadRearRuleSet, mdCtx(secret), m1, m2, ch("", "", "", ""), m3)
	expect(t, r, true, "Набор правил category-ai-!cn записан (13 байт)")
	if got := h.read("/etc/route-agent/rules/category-ai-!cn.mrs"); got != "one-two-three" {
		t.Fatalf("assembled %q", got)
	}
	if h.exists("/etc/route-agent/rules/first.mrs") {
		t.Fatal("an earlier target_binary was used")
	}

	h.on(named("bash"), runReal)
	r = h.upload(h.svc.UploadAwgToolsBinary, mdCtx(secret),
		ch("", "#!/bin/bash\n", "1.0", "awg-quick"), ch("", "true\n", "", ""), ch("", "", "2.0", ""), ch("", "", "", ""))
	expect(t, r, true, "awg-quick script version 2.0 successfully updated (service restart skipped: not yet configured)")

	r = h.upload(h.svc.UploadAwgGoBinary, mdCtx(secret), ch("", goodELF("go"), "", ""))
	expect(t, r, true, "amneziawg-go binary version unknown successfully updated (service restart skipped: not yet configured)")
}

// Checklist 9: the temp file is gone before the answer is sent (success and failure).
func TestTempRemovedBeforeAnswer(t *testing.T) {
	h := newHarness(t, nil)
	for _, fail := range []bool{false, true} {
		if fail {
			h.on(args("--version"), failing(1, "boom"))
		}
		st := &fakeStream{ctx: mdCtx(secret), msgs: []*agentpbChunk{ch("", goodELF("go"), "1", "")}}
		st.onSend = func() {
			if l := leftovers(t, h.path("/usr/local/bin"), h.tmp); len(l) != 0 {
				t.Errorf("fail=%v: temp present when answering: %v", fail, l)
			}
		}
		if err := h.svc.UploadAwgGoBinary(st); err != nil {
			t.Fatal(err)
		}
		if st.resp.Success == fail {
			t.Fatalf("fail=%v: %+v", fail, st.resp)
		}
	}
}

// Checklist 10 and 13: an apply error is a success:false answer carrying the Node-shaped
// "Command failed: <cmd>\n<stderr>" text, never a gRPC error.
func TestApplyErrorShape(t *testing.T) {
	h := newHarness(t, nil)
	st := &fakeStream{ctx: mdCtx(secret), msgs: []*agentpbChunk{ch("", goodELF("sb"), "1.13.0", "")}}
	var candidate string
	h.on(args("version"), func(c runner.Cmd) (runner.Result, error) {
		candidate = c.Name
		return runner.FailWith(c, 132, "", "Illegal instruction\n")
	})
	if err := h.svc.UploadSingboxBinary(st); err != nil {
		t.Fatal(err)
	}
	expect(t, st.resp, false, "Failed to upload binary: Command failed: "+candidate+" version\nIllegal instruction\n")
	if !strings.HasPrefix(filepath.Base(candidate), ".sing-box.upload-") || filepath.Dir(candidate) != h.path("/usr/local/bin") {
		t.Fatalf("candidate %q not received next to the target", candidate)
	}
	if h.exists("/usr/local/bin/sing-box") {
		t.Fatal("target written despite the failed smoke test")
	}
}

func TestELFChecks(t *testing.T) {
	h := newHarness(t, nil)
	r := h.upload(h.svc.UploadOlcrtcAgentSrvBinary, mdCtx(secret), ch("", "#!/bin/sh\necho not a binary\n", "1", ""))
	expect(t, r, false, "Failed to upload binary: uploaded file is not an ELF executable")
	other := "arm64"
	if h.svc.Arch == "arm64" {
		other = "amd64"
	}
	r = h.upload(h.svc.UploadOlcrtcAgentSrvBinary, mdCtx(secret), ch("", elfBin(other, "x"), "1", ""))
	if r.Success || !strings.Contains(r.Message, "uploaded binary is built for ELF machine") {
		t.Fatalf("%+v", r)
	}
	if h.exists("/usr/local/bin/olcrtc-agent-srv") || len(h.displays()) != 0 {
		t.Fatal("rejected binary was installed or run")
	}
}

// Risk 17: a size cap; the stream is answered as soon as it is exceeded.
func TestSizeLimit(t *testing.T) {
	h := newHarness(t, nil)
	h.svc.MaxRuleSetBytes = 10
	st := &fakeStream{ctx: mdCtx(secret), msgs: []*agentpbChunk{ch("", "123456", "", "a"), ch("", "7890X", "", ""), ch("", "never read", "", "")}}
	if err := h.svc.UploadRearRuleSet(st); err != nil {
		t.Fatal(err)
	}
	expect(t, st.resp, false, "Failed to upload binary: upload exceeds the size limit of 10 bytes")
	if st.i != 2 || len(leftovers(t, h.tmp)) != 0 || h.exists("/etc/route-agent/rules") {
		t.Fatalf("read %d messages, leftovers %v", st.i, leftovers(t, h.tmp))
	}
}

// Risk 5: a filesystem error while receiving is an answer, not a crash.
func TestReceiveFilesystemErrorIsAnAnswer(t *testing.T) {
	h := newHarness(t, nil)
	h.svc.TempDir = filepath.Join(h.tmp, "file")
	if err := os.WriteFile(h.svc.TempDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := h.upload(h.svc.UploadRearRuleSet, mdCtx(secret), ch("", "abc", "", "a"))
	if r.Success || !strings.HasPrefix(r.Message, "Failed to upload binary: mkdir ") {
		t.Fatalf("%+v", r)
	}
}

// A broken stream (client cancel before the half-close) returns the error and removes the temp.
func TestStreamErrorBeforeHalfClose(t *testing.T) {
	h := newHarness(t, nil)
	boom := errors.New("context canceled")
	st := &fakeStream{ctx: mdCtx(secret), msgs: []*agentpbChunk{ch("", "abc", "", "a")}, recvErr: boom}
	if err := h.svc.UploadRearRuleSet(st); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if st.sends != 0 || len(leftovers(t, h.tmp)) != 0 {
		t.Fatalf("sends %d leftovers %v", st.sends, leftovers(t, h.tmp))
	}
}

// ctxRecorder records whether a command ran under a cancelled context.
type ctxRecorder struct {
	inner     runner.Runner
	cancelled atomic.Int32
	ran       atomic.Int32
}

func (c *ctxRecorder) Run(ctx context.Context, cmd runner.Cmd) (runner.Result, error) {
	c.ran.Add(1)
	if ctx.Err() != nil {
		c.cancelled.Add(1)
	}
	return c.inner.Run(ctx, cmd)
}

// Checklist 34: the client cancelling after the half-close does not cancel the apply.
func TestApplyDetachedFromClientCancel(t *testing.T) {
	h := newHarness(t, nil)
	rec := &ctxRecorder{inner: h.fake}
	h.svc.runner = rec
	h.svc.systemctl.Runner = rec
	ctx, cancel := context.WithCancel(mdCtx(secret))
	st := &fakeStream{ctx: ctx, msgs: []*agentpbChunk{ch("", goodELF("sb"), "1.13.0", "")}, onEOF: cancel}
	if err := h.svc.UploadSingboxBinary(st); err != nil {
		t.Fatal(err)
	}
	expect(t, st.resp, true, "sing-box binary version 1.13.0 successfully updated and restarted")
	if rec.ran.Load() == 0 || rec.cancelled.Load() != 0 {
		t.Fatalf("ran %d commands, %d under a cancelled context", rec.ran.Load(), rec.cancelled.Load())
	}
}

// Risk 7: two uploads of the same target never apply at the same time.
func TestConcurrentUploadsOfOneTargetSerialized(t *testing.T) {
	h := newHarness(t, nil)
	var inFlight, peak atomic.Int32
	h.on(args("version"), func(runner.Cmd) (runner.Result, error) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		return runner.Result{}, nil
	})
	done := make(chan *fakeStream, 4)
	for i := 0; i < 4; i++ {
		go func(i int) {
			st := &fakeStream{ctx: mdCtx(secret), msgs: []*agentpbChunk{ch("", goodELF(string(rune('a'+i))), "v", "")}}
			_ = h.svc.UploadSingboxBinary(st)
			done <- st
		}(i)
	}
	for i := 0; i < 4; i++ {
		if st := <-done; !st.resp.Success {
			t.Fatalf("%+v", st.resp)
		}
	}
	if peak.Load() != 1 {
		t.Fatalf("peak concurrent applies %d", peak.Load())
	}
}
