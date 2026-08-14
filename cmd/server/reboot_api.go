package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/yellowman/wavecontrol/internal/websocket"
)

// RebootDevice immediately sends the platform-appropriate reboot command to the
// selected radio. This is intentionally separate from scheduled/async reboot
// jobs so the host detail pane button maps to a direct operator action.
func (a *API) RebootDevice(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil || id <= 0 {
		http.Error(w, "invalid device id", http.StatusBadRequest)
		return
	}

	result, err := a.Firmware.RebootDeviceByID(r.Context(), int64(id))
	if err != nil {
		status := http.StatusBadGateway
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}

	claims := getClaims(r)
	var userID int64
	if claims != nil {
		userID = claims.UserID
	}
	if result.DeviceMAC != "" {
		a.logChangelogDevice(result.DeviceMAC, fmt.Sprintf("Manual reboot requested via %s", result.API), userID)
	} else {
		a.logChangelog(fmt.Sprintf("Manual reboot requested for device %d via %s", id, result.API), userID)
	}

	if a.WSHub != nil {
		a.WSHub.BroadcastDeviceUpdate(id, result.DeviceIP, map[string]any{
			"status":        "unknown",
			"status_reason": "reboot_requested",
			"reboot_api":    result.API,
		})
		a.WSHub.Broadcast(websocket.Message{
			Type:     "device_reboot",
			DeviceID: id,
			DeviceIP: result.DeviceIP,
			Data:     result,
		})
	}

	writeJSON(w, result)
}
