package telemetry

import (
	"errors"
	"net"
	"reflect"
	"testing"
)

func ipNet(s string) net.Addr {
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	n.IP = ip
	return n
}

func TestHostGlobalIPv6KeepsOnly2000Slash3(t *testing.T) {
	addrs := func() ([]net.Addr, error) {
		return []net.Addr{
			ipNet("127.0.0.1/8"), ipNet("::1/128"),
			ipNet("203.0.113.7/24"), ipNet("2A12:5940:E3D1::2/64"), ipNet("fe80::1/64"),
			ipNet("2001:db8::5/64"), ipNet("2a12:5940:e3d1::2/64"),
			ipNet("fd00:100::2/64"), ipNet("fc00::1/7"), ipNet("ff02::1/128"), ipNet("::ffff:1.2.3.4/128"),
			&net.IPAddr{IP: net.ParseIP("3fff::1")},
		}, nil
	}
	got, reported := HostGlobalIPv6(addrs)
	want := []string{"2001:db8::5", "2a12:5940:e3d1::2", "3fff::1"}
	if !reported || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v reported=%v, want %v reported=true", got, reported, want)
	}
}

func TestHostGlobalIPv6NoIPv6IsReportedEmpty(t *testing.T) {
	got, reported := HostGlobalIPv6(func() ([]net.Addr, error) { return []net.Addr{ipNet("203.0.113.7/24")}, nil })
	if !reported || len(got) != 0 || got == nil {
		t.Fatalf("got %#v reported=%v, want empty non-nil and reported", got, reported)
	}
}

func TestHostGlobalIPv6ListingFailureIsNotReported(t *testing.T) {
	got, reported := HostGlobalIPv6(func() ([]net.Addr, error) { return nil, errors.New("boom") })
	if reported || len(got) != 0 {
		t.Fatalf("got %v reported=%v, want empty and not reported", got, reported)
	}
}
