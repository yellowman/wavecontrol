package airmax

import "testing"

func TestStationGetChainSignalsDropsTrailingZeroPlaceholder(t *testing.T) {
	sta := &Station{ChainRSSI: []int{36, 34, 0}, NoiseFloor: -83}
	got := sta.GetChainSignals()
	want := []int{-59, -61}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chain %d = %d, want %d: %#v", i, got[i], want[i], got)
		}
	}
}

func TestStationGetChainSignalsKeepsNegativeChainNearNoiseFloor(t *testing.T) {
	sta := &Station{ChainRSSI: []int{38, -86}, NoiseFloor: -86}
	got := sta.GetChainSignals()
	want := []int{-57, -86}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chain %d = %d, want %d: %#v", i, got[i], want[i], got)
		}
	}
}

func TestRemoteGetChainSignalsKeepsNegativeChainNearNoiseFloor(t *testing.T) {
	remote := &RemoteInfo{ChainRSSI: []int{34, -91}, NoiseFloor: -91}
	got := remote.GetChainSignals()
	want := []int{-61, -91}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chain %d = %d, want %d: %#v", i, got[i], want[i], got)
		}
	}
}

func TestGetChainSignalsKeepsLegitimateNegativeDbm(t *testing.T) {
	sta := &Station{ChainRSSI: []int{-60, -66}, NoiseFloor: -95}
	got := sta.GetChainSignals()
	want := []int{-60, -66}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chain %d = %d, want %d: %#v", i, got[i], want[i], got)
		}
	}
}
