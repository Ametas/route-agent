// Package fsx writes files the way the agent needs them written.
//
// It replaces the in-place fs.writeFile calls of the TS agent and src/utils/plannedWrites.ts
// (spec A §9.6) with:
//
//   - WriteFile: atomic (temp file in the same directory + fsync + rename + fsync of the
//     directory), with an explicit mode applied by chmod (umask-proof) and an optional owner.
//     Secrets go through here with mode 0600 (architecture §6 item 3).
//   - UpToDate / WriteIfChanged: "skip if unchanged" by bytes AND mode (plannedWrites semantics:
//     a correct key with drifted permissions must still be rewritten).
//   - AllWritesAlreadyOnDisk / ApplyPlannedWrites: the plannedWrites API used by ConfigureCaddy.
//   - FS.Root: a root prefix, so tests write into t.TempDir() while code keeps using the real
//     absolute paths (/etc/..., /var/lib/caddy/...).
package fsx

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// DefaultFileMode is used for a new file written without an explicit mode (Node's 0666 under
// systemd's default umask 022).
const DefaultFileMode fs.FileMode = 0o644

// DirMode is used for parent directories created on demand (mkdir -p under umask 022).
const DirMode fs.FileMode = 0o755

// FS resolves agent paths under Root. The zero value works on the real filesystem.
type FS struct {
	// Root is prepended to every absolute path; "" or "/" means the real root.
	Root string
}

// Path maps an agent path to the real path on disk.
func (f FS) Path(p string) string {
	if f.Root == "" || f.Root == "/" {
		return p
	}
	return filepath.Join(f.Root, p)
}

// Owner is an optional uid/gid for written files.
type Owner struct {
	UID, GID int
}

// WriteOptions control WriteFile.
type WriteOptions struct {
	// Mode is the permission bits of the written file. Zero means: keep the mode of the existing
	// file, or DefaultFileMode for a new one (the TS writeFile-without-mode behaviour).
	Mode fs.FileMode
	// Owner, if set, is applied with chown. Nil means: keep the owner of the existing file
	// (an in-place write kept it; a rename would otherwise make it root's), or the process owner.
	Owner *Owner
}

// ReadFile reads an agent path.
func (f FS) ReadFile(p string) ([]byte, error) { return os.ReadFile(f.Path(p)) }

// MkdirAll creates an agent directory with DirMode.
func (f FS) MkdirAll(p string) error { return os.MkdirAll(f.Path(p), DirMode) }

// WriteFile atomically replaces the agent path p with data. If p is a symlink, the file it points
// to is replaced (as Node's writeFile followed the link), not the link itself.
func (f FS) WriteFile(p string, data []byte, opt WriteOptions) error {
	real := f.Path(p)
	if target, err := filepath.EvalSymlinks(real); err == nil {
		real = target
	}
	dir := filepath.Dir(real)
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return err
	}

	mode := opt.Mode
	owner := opt.Owner
	if st, err := os.Stat(real); err == nil {
		if mode == 0 {
			mode = st.Mode().Perm()
		}
		if owner == nil {
			if sys, ok := st.Sys().(*syscall.Stat_t); ok {
				owner = &Owner{UID: int(sys.Uid), GID: int(sys.Gid)}
			}
		}
	}
	if mode == 0 {
		mode = DefaultFileMode
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(real)+".tmp-*")
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
	// chmod before any byte of a secret lands in the file (CreateTemp makes it 0600 anyway).
	if err := tmp.Chmod(mode.Perm()); err != nil {
		return err
	}
	if owner != nil && (owner.UID != os.Getuid() || owner.GID != os.Getgid()) {
		if err := tmp.Chown(owner.UID, owner.GID); err != nil && opt.Owner != nil {
			return err // an explicitly requested owner must apply; an inherited one is best effort
		}
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, real); err != nil {
		return err
	}
	ok = true
	syncDir(dir)
	return nil
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// UpToDate reports whether p already holds exactly data and, when mode is non-zero, has exactly
// that permission mode. A missing or unreadable file is not up to date.
func (f FS) UpToDate(p string, data []byte, mode fs.FileMode) bool {
	cur, err := os.ReadFile(f.Path(p))
	if err != nil || !bytes.Equal(cur, data) {
		return false
	}
	if mode != 0 {
		st, err := os.Stat(f.Path(p))
		if err != nil || st.Mode().Perm() != mode.Perm() {
			return false
		}
	}
	return true
}

// WriteIfChanged writes p only when UpToDate is false. It reports whether it wrote.
func (f FS) WriteIfChanged(p string, data []byte, opt WriteOptions) (bool, error) {
	if f.UpToDate(p, data, opt.Mode) {
		return false, nil
	}
	if err := f.WriteFile(p, data, opt); err != nil {
		return false, err
	}
	return true, nil
}

// PlannedWrite is one file of a set that is written together (src/utils/plannedWrites.ts).
type PlannedWrite struct {
	Path    string
	Content []byte
	// Mode, if non-zero, is enforced and compared (perm & 0777).
	Mode fs.FileMode
}

// AllWritesAlreadyOnDisk is true only when every planned file already has the planned content
// and (where Mode is set) mode. An empty plan is false: "nothing to write" is not "verified"
// (plannedWrites.ts:38-52).
func (f FS) AllWritesAlreadyOnDisk(writes []PlannedWrite) bool {
	if len(writes) == 0 {
		return false
	}
	for _, w := range writes {
		if !f.UpToDate(w.Path, w.Content, w.Mode) {
			return false
		}
	}
	return true
}

// ApplyPlannedWrites writes every file in order (each one atomically). A failure stops the
// sequence and is returned; earlier files stay written, as in TS (plannedWrites.ts:55-70).
func (f FS) ApplyPlannedWrites(writes []PlannedWrite) error {
	for _, w := range writes {
		if err := f.WriteFile(w.Path, w.Content, WriteOptions{Mode: w.Mode}); err != nil {
			return err
		}
	}
	return nil
}

// NodeError renders a filesystem error the way Node's fs promises API words it, e.g.
// "ENOENT: no such file or directory, open '/x/y.crt'". Responses that surfaced err.message in
// TS (GetManagedCertificate, spec A §11.1 step 5) use it. op is the Node syscall name ("open").
func NodeError(op, path string, err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	code, text := "", ""
	switch errno {
	case syscall.ENOENT:
		code, text = "ENOENT", "no such file or directory"
	case syscall.EACCES:
		code, text = "EACCES", "permission denied"
	case syscall.EPERM:
		code, text = "EPERM", "operation not permitted"
	case syscall.EISDIR:
		// Node opens a directory fine and fails on read(2), without a path.
		return "EISDIR: illegal operation on a directory, read"
	case syscall.ENOTDIR:
		code, text = "ENOTDIR", "not a directory"
	case syscall.ELOOP:
		code, text = "ELOOP", "too many symbolic links encountered"
	case syscall.EMFILE:
		code, text = "EMFILE", "too many open files"
	default:
		return err.Error()
	}
	return fmt.Sprintf("%s: %s, %s '%s'", code, text, op, path)
}
