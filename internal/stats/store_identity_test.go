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

func TestBindIdentityByMACCanClearSiteIdentity(t *testing.T) {
	store := NewStore()
	store.BindIdentityByMAC("28:70:4e:e1:e8:b5", "172.20.66.7", 89, 12)
	store.BindIdentityByMAC("28:70:4e:e1:e8:b5", "172.20.66.7", 89, 0)

	got := store.GetByMAC("28:70:4e:e1:e8:b5")
	if got == nil {
		t.Fatal("expected stats row")
	}
	if got.SiteID != 0 {
		t.Fatalf("SiteID = %d, want 0 after explicit clear", got.SiteID)
	}
}


func TestSetStatusByMACChangedReportsVisibleStateChanges(t *testing.T) {
	store := NewStore()
	const mac = "28:70:4e:e1:e8:b5"
	const ip = "172.20.66.7"

	store.BindIdentityByMAC(mac, ip, 89, 12)

	leftOnline, changed := store.SetStatusByMACChanged(mac, ip, StatusOffline, "not_associated", "", false)
	if leftOnline {
		t.Fatal("initial unknown->offline transition must not report leftOnline")
	}
	if !changed {
		t.Fatal("initial offline/not_associated state must report changed")
	}

	leftOnline, changed = store.SetStatusByMACChanged(mac, ip, StatusOffline, "not_associated", "", false)
	if leftOnline {
		t.Fatal("repeated offline state must not report leftOnline")
	}
	if changed {
		t.Fatal("identical repeated status/reason must not report changed")
	}

	_, changed = store.SetStatusByMACChanged(mac, ip, StatusOffline, "parent_offline", "", false)
	if !changed {
		t.Fatal("reason-only change must report changed")
	}

	store.SetStatusByMAC(mac, ip, StatusOnline, "", "", true)
	leftOnline, changed = store.SetStatusByMACChanged(mac, ip, StatusOffline, "not_associated", "", false)
	if !leftOnline || !changed {
		t.Fatalf("online->offline = leftOnline %v, changed %v; want true,true", leftOnline, changed)
	}
}
