package awg

import (
	"reflect"
	"testing"
)

// Peers one by one (2026-11-14): handshake, transfer and endpoint for the node and front windows.
func TestPeerStats(t *testing.T) {
	dump := "PRIVKEY=\tPUBKEY=\t51821\toff\n" +
		"live1=\t(none)\t146.19.128.73:51821\t100.100.0.5/32\t1790399973\t118000\t164000\t25\n" +
		"noaddr=\t(none)\t(none)\t100.100.0.9/32\t0\t0\t0\toff\n" +
		"broken=\t(none)\n"
	want := []PeerStat{
		{PublicKey: "live1=", LatestHandshakeSec: 1790399973, RxBytes: 118000, TxBytes: 164000, Endpoint: "146.19.128.73:51821"},
		{PublicKey: "noaddr=", LatestHandshakeSec: 0, RxBytes: 0, TxBytes: 0, Endpoint: ""},
	}
	if got := PeerStats(dump); !reflect.DeepEqual(got, want) {
		t.Fatalf("PeerStats = %+v, want %+v", got, want)
	}
	if got := PeerStats("PRIVKEY=\tPUBKEY=\t51821\toff\n"); got == nil || len(got) != 0 {
		t.Fatalf("no peers must be an empty, non-nil slice: %#v", got)
	}
}
