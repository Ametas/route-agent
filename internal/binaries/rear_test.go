package binaries

import (
	"os"
	"strings"
	"testing"

	"github.com/Ametas/route-agent/internal/runner"
)

// Checklist 26: -v smoke test; -t -d <dir> -f <cfg> only if cfg exists; no setcap; restart of
// route-rear-mihomo only if its unit file exists.
func TestMihomoUpload(t *testing.T) {
	h := newHarness(t, nil)
	r := h.upload(h.svc.UploadMihomoBinary, mdCtx(secret), ch("", goodELF("m"), "v1.19.0", "mihomo"))
	expect(t, r, true, "mihomo binary version v1.19.0 successfully updated")
	if h.read("/usr/local/bin/mihomo") != goodELF("m") {
		t.Fatal("target")
	}
	if h.calls(" -v") != 1 || h.calls(" -t -d ") != 0 || h.calls("setcap") != 0 || h.calls("restart") != 0 {
		t.Fatalf("%q", h.displays())
	}

	h.write("/etc/route-agent/rear.yaml", "mode: rule\n", 0o644)
	h.write("/etc/systemd/system/route-rear-mihomo.service", "[Unit]\n", 0o644)
	r = h.upload(h.svc.UploadMihomoBinary, mdCtx(secret), ch("", goodELF("m2"), "", ""))
	expect(t, r, true, "mihomo binary version unknown successfully updated")
	cfg := h.path("/etc/route-agent/rear.yaml")
	if h.calls(" -t -d "+h.path("/etc/route-agent")+" -f "+cfg) != 1 || h.calls("systemctl restart route-rear-mihomo") != 1 {
		t.Fatalf("%q", h.displays())
	}

	h.on(func(c runner.Cmd) bool { return len(c.Args) > 0 && c.Args[0] == "-t" }, failing(1, strings.Repeat("y", 900)))
	r = h.upload(h.svc.UploadMihomoBinary, mdCtx(secret), ch("", goodELF("m3"), "v2", ""))
	expect(t, r, false, "Бинарь mihomo v2 отвергнут: он не принимает уже применённый конфиг /etc/route-agent/rear.yaml. Прежний бинарь оставлен на месте. "+strings.Repeat("y", 400))
	if h.read("/usr/local/bin/mihomo") != goodELF("m2") {
		t.Fatal("rejected binary installed")
	}

	h.on(args("-v"), failing(132, "Illegal instruction"))
	r = h.upload(h.svc.UploadMihomoBinary, mdCtx(secret), ch("", goodELF("m4"), "v3", ""))
	if r.Success || !strings.HasPrefix(r.Message, "Полученный бинарь mihomo не запускается: Command failed: ") {
		t.Fatalf("%+v", r)
	}
	r = h.upload(h.svc.UploadMihomoBinary, mdCtx(secret), ch("", "garbage", "v3", ""))
	expect(t, r, false, "Полученный бинарь mihomo не запускается: uploaded file is not an ELF executable")
}

// Checklist 27: name allowlist, nothing created for rejected names, mode 0644, no staging
// leftovers, reload (not restart) only when the unit is active.
func TestRuleSetUpload(t *testing.T) {
	h := newHarness(t, nil)
	for _, bad := range []string{"../../etc/passwd", "..", "a/b", "/etc/passwd", "", ".hidden", strings.Repeat("a", 200), "a b", "a\x00"} {
		r := h.upload(h.svc.UploadRearRuleSet, mdCtx(secret), ch("", "data", "", bad))
		expect(t, r, false, "Недопустимое имя набора правил.")
	}
	if h.exists("/etc/route-agent/rules") || h.exists("/etc/passwd") {
		t.Fatal("a rejected name created something")
	}
	if len(leftovers(t, h.tmp)) != 0 {
		t.Fatal(leftovers(t, h.tmp))
	}

	r := h.upload(h.svc.UploadRearRuleSet, mdCtx(secret), ch("", "MRS1", "", "category-ai-!cn"))
	expect(t, r, true, "Набор правил category-ai-!cn записан (4 байт)")
	r = h.upload(h.svc.UploadRearRuleSet, mdCtx(secret), ch("", "media", "", "category-media"))
	expect(t, r, true, "Набор правил category-media записан (5 байт)")
	st, err := os.Stat(h.path("/etc/route-agent/rules/category-media.mrs"))
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Fatal(err, st)
	}
	entries, _ := os.ReadDir(h.path("/etc/route-agent/rules"))
	if len(entries) != 2 {
		t.Fatalf("staging leftovers: %v", entries)
	}
	if h.calls("is-active route-rear-mihomo") != 2 || h.calls("reload") != 0 {
		t.Fatalf("%q", h.displays())
	}
	h.on(display("systemctl is-active route-rear-mihomo"), stdout("active\n"))
	h.upload(h.svc.UploadRearRuleSet, mdCtx(secret), ch("", "x", "", "geoip-ru"))
	if h.calls("systemctl reload route-rear-mihomo") != 1 || h.calls("restart") != 0 {
		t.Fatalf("%q", h.displays())
	}
	// a reload failure does not fail the upload
	h.on(display("systemctl reload route-rear-mihomo"), failing(1, "x"))
	expect(t, h.upload(h.svc.UploadRearRuleSet, mdCtx(secret), ch("", "x", "", "geoip-ru")), true, "Набор правил geoip-ru записан (1 байт)")
}
