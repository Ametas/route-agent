package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func perm(t *testing.T, p string) fs.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestRootPrefix(t *testing.T) {
	root := t.TempDir()
	f := FS{Root: root}
	if err := f.WriteFile("/etc/route-agent/x.conf", []byte("a"), WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "etc/route-agent/x.conf"))
	if err != nil || string(data) != "a" {
		t.Fatalf("%v %q", err, data)
	}
	if (FS{}).Path("/etc/x") != "/etc/x" || (FS{Root: "/"}).Path("/etc/x") != "/etc/x" {
		t.Fatal("zero FS must map to the real path")
	}
}

func TestWriteFileModeAndDirs(t *testing.T) {
	f := FS{Root: t.TempDir()}
	if err := f.WriteFile("/a/b/key", []byte("secret"), WriteOptions{Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if m := perm(t, f.Path("/a/b/key")); m != 0o600 {
		t.Fatalf("mode %o", m)
	}
	if m := perm(t, f.Path("/a/b")); m != DirMode {
		t.Fatalf("dir mode %o", m)
	}
	// No mode: a new file gets 0644, an existing file keeps its mode.
	if err := f.WriteFile("/a/new", []byte("x"), WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if m := perm(t, f.Path("/a/new")); m != 0o644 {
		t.Fatalf("default mode %o", m)
	}
	if err := os.Chmod(f.Path("/a/new"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteFile("/a/new", []byte("y"), WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if m := perm(t, f.Path("/a/new")); m != 0o640 {
		t.Fatalf("kept mode %o", m)
	}
}

func TestWriteFileIsAtomicAndLeavesNoTemp(t *testing.T) {
	f := FS{Root: t.TempDir()}
	p := "/d/file"
	if err := f.WriteFile(p, []byte("old"), WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(f.Path(p))
	if err := f.WriteFile(p, []byte("new"), WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(f.Path(p))
	if os.SameFile(before, after) {
		t.Fatal("expected a new inode (rename), got an in-place write")
	}
	entries, _ := os.ReadDir(f.Path("/d"))
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}
}

func TestWriteFileFollowsSymlink(t *testing.T) {
	f := FS{Root: t.TempDir()}
	if err := f.WriteFile("/real/target", []byte("a"), WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.Path("/real/target"), f.Path("/link")); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteFile("/link", []byte("b"), WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(f.Path("/link")); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced")
	}
	if data, _ := os.ReadFile(f.Path("/real/target")); string(data) != "b" {
		t.Fatalf("target %q", data)
	}
}

// Port of tests/plannedWrites.test.ts.
func TestPlannedWrites(t *testing.T) {
	f := FS{Root: t.TempDir()}
	if f.AllWritesAlreadyOnDisk(nil) {
		t.Fatal("empty plan must not count as verified")
	}
	plan := []PlannedWrite{
		{Path: "/etc/caddy/Caddyfile", Content: []byte("site {}\n")},
		{Path: "/var/lib/caddy/x/x.key", Content: []byte("KEY"), Mode: 0o600},
	}
	if f.AllWritesAlreadyOnDisk(plan) {
		t.Fatal("missing files must not match")
	}
	if err := f.ApplyPlannedWrites(plan); err != nil {
		t.Fatal(err)
	}
	if !f.AllWritesAlreadyOnDisk(plan) {
		t.Fatal("just-written plan must match")
	}
	// One differing file is a mismatch.
	changed := append([]PlannedWrite(nil), plan...)
	changed[0].Content = []byte("site { other }\n")
	if f.AllWritesAlreadyOnDisk(changed) {
		t.Fatal("changed content must not match")
	}
	// Right bytes, drifted mode: mismatch, and apply restores the mode.
	if err := os.Chmod(f.Path("/var/lib/caddy/x/x.key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if f.AllWritesAlreadyOnDisk(plan) {
		t.Fatal("0644 vs expected 0600 must not match")
	}
	if err := f.ApplyPlannedWrites(plan); err != nil {
		t.Fatal(err)
	}
	if m := perm(t, f.Path("/var/lib/caddy/x/x.key")); m != 0o600 {
		t.Fatalf("mode not enforced on existing file: %o", m)
	}
}

func TestWriteIfChanged(t *testing.T) {
	f := FS{Root: t.TempDir()}
	opt := WriteOptions{Mode: 0o600}
	if w, err := f.WriteIfChanged("/k", []byte("v"), opt); err != nil || !w {
		t.Fatalf("first write: %v %v", w, err)
	}
	if w, err := f.WriteIfChanged("/k", []byte("v"), opt); err != nil || w {
		t.Fatalf("unchanged must skip: %v %v", w, err)
	}
	_ = os.Chmod(f.Path("/k"), 0o644)
	if w, err := f.WriteIfChanged("/k", []byte("v"), opt); err != nil || !w {
		t.Fatalf("mode drift must rewrite: %v %v", w, err)
	}
}

func TestNodeError(t *testing.T) {
	_, err := os.ReadFile("/definitely/missing.crt")
	if got := NodeError("open", "/definitely/missing.crt", err); got != "ENOENT: no such file or directory, open '/definitely/missing.crt'" {
		t.Fatalf("got %q", got)
	}
	dir := t.TempDir()
	_, err = os.ReadFile(dir)
	if got := NodeError("open", dir, err); got != "EISDIR: illegal operation on a directory, read" {
		t.Fatalf("got %q", got)
	}
	if got := NodeError("open", "/x", errors.New("plain")); got != "plain" {
		t.Fatalf("got %q", got)
	}
	if !strings.HasPrefix(NodeError("open", "/x", &fs.PathError{Op: "open", Path: "/x", Err: syscall.EACCES}), "EACCES: permission denied") {
		t.Fatal("EACCES")
	}
}
