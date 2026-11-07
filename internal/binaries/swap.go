package binaries

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// PrevPath is where the replaced file is kept for rollback: <target>.prev.
func PrevPath(target string) string { return target + ".prev" }

// binaryTempSpec is where a binary for target is received: next to the target (same filesystem:
// the swap is a plain rename, capabilities set on the candidate survive it, and the smoke test
// runs from an exec-mounted directory instead of a possibly noexec /tmp — spec C risk 9/10).
func (s *Service) binaryTempDir(target string) string {
	return filepath.Dir(s.fs.Path(target))
}

func tempPrefix(target string) string { return "." + filepath.Base(target) + ".upload-" }

// install puts the candidate (a real path) at the agent path target, atomically, keeping the
// file it replaces as <target>.prev. It reports whether a previous file existed (rollback is
// possible). Final mode 0755. Replaces replaceBinaryAtomically (spec C §0.4) and fixes its gaps:
// fsync of the file and the directory, and a previous version to roll back to (risk 4, 6).
func (s *Service) install(candidate, target string) (hadPrev bool, err error) {
	real := s.fs.Path(target)
	dir := filepath.Dir(real)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	hadPrev, err = copyAtomic(real, PrevPath(real))
	if err != nil {
		return false, fmt.Errorf("keep previous %s: %w", target, err)
	}
	if filepath.Dir(candidate) == dir {
		if err := os.Chmod(candidate, 0o755); err != nil {
			return hadPrev, err
		}
		if err := os.Rename(candidate, real); err != nil {
			return hadPrev, err
		}
		syncDir(dir)
		return hadPrev, nil
	}
	if _, err := copyAtomic(candidate, real); err != nil {
		return hadPrev, err
	}
	return hadPrev, nil
}

// rollback puts <target>.prev back in place (the .prev copy stays).
func (s *Service) rollback(target string) error {
	real := s.fs.Path(target)
	found, err := copyAtomic(PrevPath(real), real)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("no previous version to roll back to")
	}
	return nil
}

// copyAtomic copies src (following symlinks) to dst through a temp file in dst's directory:
// chmod 0755, fsync, rename, fsync of the directory. A missing src is (false, nil).
func copyAtomic(src, dst string) (bool, error) {
	in, err := os.Open(src)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer in.Close()
	if st, err := in.Stat(); err == nil && st.IsDir() {
		return false, fmt.Errorf("%s is a directory", src)
	}
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dst)+"_*.tmp")
	if err != nil {
		return true, err
	}
	name := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		return true, err
	}
	if err := tmp.Chmod(0o755); err != nil {
		return true, err
	}
	if err := tmp.Sync(); err != nil {
		return true, err
	}
	if err := tmp.Close(); err != nil {
		return true, err
	}
	if err := os.Rename(name, dst); err != nil {
		return true, err
	}
	done = true
	syncDir(dir)
	return true, nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// elfMachines maps GOARCH to the ELF e_machine value.
var elfMachines = map[string]uint16{"amd64": 62, "arm64": 183, "386": 3, "arm": 40, "riscv64": 243}

// checkELF is the integrity check every prebuilt binary gets before anything runs it (spec C
// risk 2: TS accepted any bytes and only some targets had a smoke test). It catches a truncated
// or wrong upload and a binary built for another architecture, which would otherwise surface as
// a dead service after the swap.
func checkELF(path, goarch string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var hdr [20]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil || string(hdr[:4]) != "\x7fELF" {
		return errors.New("uploaded file is not an ELF executable")
	}
	want, known := elfMachines[goarch]
	if goarch == "" || !known {
		return nil
	}
	var got uint16
	switch hdr[5] { // EI_DATA
	case 1:
		got = binary.LittleEndian.Uint16(hdr[18:])
	case 2:
		got = binary.BigEndian.Uint16(hdr[18:])
	default:
		return errors.New("uploaded file is not an ELF executable")
	}
	if got != want {
		return fmt.Errorf("uploaded binary is built for ELF machine %d, this node is %s (machine %d)", got, goarch, want)
	}
	return nil
}
