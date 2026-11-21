package telemetry

import (
	"net"
	"sort"
)

// HostGlobalIPv6 is the host_global_ipv6 field and its "reported" flag (2026-11-21): the
// interface addresses in 2000::/3. The orchestrator recognises a front that reached it through
// Cloudflare over IPv6 by them and trusts that front's client-address header. One condition drops
// loopback, link-local (fe80::/10), ULA (fc00::/7 — mesh and Docker), multicast and IPv4-mapped
// addresses; listing the exclusions one by one would miss the next one. A failed listing reports
// false, so the orchestrator keeps "unknown" apart from "no IPv6". Same rule as the TS agent
// (src/utils/hostNetwork.ts, isGlobalUnicastIpv6).
func HostGlobalIPv6(addrs func() ([]net.Addr, error)) ([]string, bool) {
	if addrs == nil {
		addrs = net.InterfaceAddrs
	}
	list, err := addrs()
	if err != nil {
		return []string{}, false
	}
	seen := map[string]bool{}
	out := []string{}
	for _, a := range list {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}
		if !isGlobalUnicastIPv6(ip) {
			continue
		}
		s := ip.String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out, true
}

func isGlobalUnicastIPv6(ip net.IP) bool {
	if ip.To4() != nil {
		return false
	}
	ip16 := ip.To16()
	return ip16 != nil && ip16[0]&0xe0 == 0x20
}
