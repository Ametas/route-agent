package dkms

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Checklist 32: the cases of tests/dkmsKernelStatus.test.ts.

const running = "6.8.0-139-generic"

func TestIsDkmsInstalledForKernel(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{"amneziawg/1.0.20250901, 6.8.0-100-generic, x86_64: installed\n", false},
		{"amneziawg/1.0.20250901, " + running + ", x86_64: installed\n", true},
		{"amneziawg, 1.0.20250901, " + running + ", x86_64: installed\n", true},
		{"amneziawg/1.0.20250901, " + running + ", x86_64: built\n", false},
		{"amneziawg/1.0.20250901: added\n", false},
		{"", false},
		{"amneziawg/1.0.20250901, 6.8.0-100-generic, x86_64: installed\namneziawg/1.0.20250901, " + running + ", x86_64: installed", true},
		{"amneziawg/1.0, 6.8.0-139-generic-64k, x86_64: installed", false},
		{"amneziawg/1.0, " + running + ", x86_64: Installed (original_module exists)", true},
	} {
		if got := IsDkmsInstalledForKernel(tc.out, running); got != tc.want {
			t.Errorf("%q: %v", tc.out, got)
		}
	}
}

func TestDkmsKnownVersions(t *testing.T) {
	out := strings.Join([]string{
		"amneziawg/1.0.0, 7.0.0-22-generic, x86_64: installed",
		"amneziawg/1.0.0: added",
		"amneziawg, 1.0.20250901, 7.0.0-38-generic, x86_64: built",
		"other/2.0, 7.0.0-38-generic, x86_64: installed",
		"",
	}, "\n")
	if got := DkmsKnownVersions(out, "amneziawg"); !slices.Equal(got, []string{"1.0.0", "1.0.20250901"}) {
		t.Fatal(got)
	}
	if got := DkmsKnownVersions("", "amneziawg"); len(got) != 0 {
		t.Fatal(got)
	}
}

var compat = strings.Join([]string{
	"#if LINUX_VERSION_CODE < KERNEL_VERSION(7, 1, 5)",
	"#include <net/udp_tunnel.h>",
	"#define setup_udp_tunnel_sock(net, sk, sock_cfg) setup_udp_tunnel_sock(net, sk->sk_socket, sock_cfg)",
	"#define udp_tunnel_sock_release(sk) udp_tunnel_sock_release(sk->sk_socket)",
	"#endif",
	"",
}, "\n")

func TestPatchCompatForKernelHeaders(t *testing.T) {
	newHdr := "void setup_udp_tunnel_sock(struct net *net, struct sock *sk,\n\t\t\t   struct udp_tunnel_sock_cfg *sock_cfg);\nvoid udp_tunnel_sock_release(struct sock *sk);"
	src, dropped := PatchCompatForKernelHeaders(compat, newHdr)
	if !slices.Equal(dropped, []string{"setup_udp_tunnel_sock", "udp_tunnel_sock_release"}) || strings.Contains(src, "sk->sk_socket") ||
		!strings.Contains(src, "#include <net/udp_tunnel.h>") || !strings.Contains(src, "#endif") {
		t.Fatalf("%v\n%s", dropped, src)
	}
	oldHdr := "void setup_udp_tunnel_sock(struct net *net, struct socket *sock,\n\t\t\t   struct udp_tunnel_sock_cfg *sock_cfg);\nvoid udp_tunnel_sock_release(struct socket *sock);"
	if src, dropped := PatchCompatForKernelHeaders(compat, oldHdr); len(dropped) != 0 || src != compat {
		t.Fatal(dropped)
	}
	one := "void setup_udp_tunnel_sock(struct net *net, struct sock *sk, struct udp_tunnel_sock_cfg *cfg);\nvoid udp_tunnel_sock_release(struct socket *sock);"
	src, dropped = PatchCompatForKernelHeaders(compat, one)
	if !slices.Equal(dropped, []string{"setup_udp_tunnel_sock"}) || !strings.Contains(src, "udp_tunnel_sock_release(sk->sk_socket)") {
		t.Fatal(dropped, src)
	}
}

func TestFindAndParseDkmsConf(t *testing.T) {
	d := t.TempDir()
	if _, err := FindDkmsConf(d); err == nil || err.Error() != "dkms.conf not found in extracted source tarball under "+d {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(d, "zzz"), 0o755)
	_ = os.WriteFile(filepath.Join(d, "zzz", "dkms.conf"), []byte("x"), 0o644)
	if p, _ := FindDkmsConf(d); p != filepath.Join(d, "zzz", "dkms.conf") {
		t.Fatal(p)
	}
	_ = os.WriteFile(filepath.Join(d, "dkms.conf"), []byte("x"), 0o644)
	if p, _ := FindDkmsConf(d); p != filepath.Join(d, "dkms.conf") {
		t.Fatal(p)
	}
	_ = os.MkdirAll(filepath.Join(d, "src"), 0o755)
	_ = os.WriteFile(filepath.Join(d, "src", "dkms.conf"), []byte("x"), 0o644)
	if p, _ := FindDkmsConf(d); p != filepath.Join(d, "src", "dkms.conf") {
		t.Fatal(p)
	}

	m, fb := ParseDkmsConf("PACKAGE_NAME=\"amneziawg\"\nPACKAGE_VERSION = \"9.9.9-test\"\n")
	if m.Name != "amneziawg" || m.Version != "9.9.9-test" || fb {
		t.Fatal(m, fb)
	}
	if m, fb = ParseDkmsConf("PACKAGE_NAME=\"x\"\n"); m.Name != "x" || m.Version != FallbackVersion || !fb {
		t.Fatal(m, fb)
	}
	if m, fb = ParseDkmsConf(""); m.Name != FallbackName || m.Version != FallbackVersion || !fb {
		t.Fatal(m, fb)
	}
}
