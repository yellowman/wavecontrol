package firmware

import (
	"errors"
	"testing"
)

func TestNormalizeUpgradeResultCreatesFailure(t *testing.T) {
	err := errors.New("device lookup failed")
	got := normalizeUpgradeResult(nil, 42, err)
	if got == nil {
		t.Fatal("expected a concrete failure result")
	}
	if got.DeviceID != 42 || got.Status != "failed" || got.Message != err.Error() {
		t.Fatalf("unexpected result: %#v", got)
	}
}

func TestNormalizeUpgradeResultPreservesExistingResult(t *testing.T) {
	want := &UpgradeResult{DeviceID: 7, Status: "failed", Message: "specific failure"}
	got := normalizeUpgradeResult(want, 42, errors.New("outer failure"))
	if got != want {
		t.Fatalf("expected existing result to be preserved: got %#v want %#v", got, want)
	}
}
