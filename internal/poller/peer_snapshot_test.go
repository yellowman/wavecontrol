package poller

import (
	"testing"

	"github.com/yellowman/wavecontrol/internal/stats"
)

func TestAcceptPeerSnapshotRequiresTwoConsecutiveEmpties(t *testing.T) {
	p := &Poller{emptyPeerPolls: make(map[int64]int)}
	const apID int64 = 42

	if p.acceptPeerSnapshot(apID, nil) {
		t.Fatal("first empty snapshot was accepted")
	}
	if !p.acceptPeerSnapshot(apID, nil) {
		t.Fatal("second consecutive empty snapshot was rejected")
	}
	if !p.acceptPeerSnapshot(apID, nil) {
		t.Fatal("confirmed empty snapshot should remain accepted")
	}

	if !p.acceptPeerSnapshot(apID, []*stats.PeerStats{{MAC: "00:11:22:33:44:55"}}) {
		t.Fatal("non-empty snapshot was rejected")
	}
	if p.acceptPeerSnapshot(apID, nil) {
		t.Fatal("non-empty snapshot did not reset the empty counter")
	}
}

func TestAcceptPeerSnapshotTracksAPsIndependently(t *testing.T) {
	p := &Poller{emptyPeerPolls: make(map[int64]int)}
	if p.acceptPeerSnapshot(1, nil) || p.acceptPeerSnapshot(2, nil) {
		t.Fatal("first empty snapshot should be rejected independently for each AP")
	}
	if !p.acceptPeerSnapshot(1, nil) {
		t.Fatal("second empty snapshot for AP 1 was rejected")
	}
	if !p.acceptPeerSnapshot(2, nil) {
		t.Fatal("second empty snapshot for AP 2 was rejected")
	}
}
