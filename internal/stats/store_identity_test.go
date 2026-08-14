package stats

import "testing"

func TestBindIdentityByMACPreservesDeviceIDOnFailureStatus(t *testing.T) {
	store := NewStore()
	store.BindIdentityByMAC("28:70:4E:E1:E8:B5", "172.20.66.7", 89, 12)
	store.SetOfflineWithReasonByMAC("28:70:4E:E1:E8:B5", "172.20.66.7", "timeout", "poll timeout")

	got := store.Get("172.20.66.7")
	if got == nil {
		t.Fatal("expected stats row")
	}
	if got.DeviceID != 89 {
		t.Fatalf("DeviceID = %d, want 89", got.DeviceID)
	}
	if got.SiteID != 12 {
		t.Fatalf("SiteID = %d, want 12", got.SiteID)
	}
	if got.Status != StatusOffline {
		t.Fatalf("Status = %q, want offline", got.Status)
	}
}

func TestUpdatePreservesIdentityWhenMACReplacesIPPlaceholder(t *testing.T) {
	store := NewStore()
	store.BindIdentityByMAC("", "172.20.66.7", 89, 12)

	store.Update("172.20.66.7", &DeviceStats{
		MAC:      "28:70:4E:E1:E8:B5",
		Hostname: "TAYLOR2",
	})

	got := store.Get("172.20.66.7")
	if got == nil {
		t.Fatal("expected stats row")
	}
	if got.DeviceID != 89 {
		t.Fatalf("DeviceID = %d, want 89", got.DeviceID)
	}
	if got.SiteID != 12 {
		t.Fatalf("SiteID = %d, want 12", got.SiteID)
	}
	if got.MAC != "28:70:4e:e1:e8:b5" {
		t.Fatalf("MAC = %q, want normalized MAC", got.MAC)
	}
}
