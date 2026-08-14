package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/yellowman/wavecontrol/internal/push"
)

func (a *API) requirePush(w http.ResponseWriter) bool {
	if a.Push == nil {
		http.Error(w, "mobile push service is not available", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (a *API) RegisterMobileDevice(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) || !a.requirePush(w) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req push.RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	dev, err := a.Push.RegisterDevice(r.Context(), claims.UserID, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "device": dev})
}

func (a *API) UnregisterMobileDevice(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) || !a.requirePush(w) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Platform string `json:"platform"`
		Provider string `json:"provider"`
		Token    string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := a.Push.UnregisterDevice(r.Context(), claims.UserID, req.Platform, req.Provider, req.Token); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) ListMobileDevices(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) || !a.requirePush(w) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	devices, err := a.Push.ListDevices(r.Context(), claims.UserID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if devices == nil {
		devices = []push.MobileDevice{}
	}
	writeJSON(w, devices)
}

func (a *API) GetMobilePreferences(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) || !a.requirePush(w) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	pref, err := a.Push.GetPreferences(r.Context(), claims.UserID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, pref)
}

func (a *API) UpdateMobilePreferences(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) || !a.requirePush(w) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var pref push.Preferences
	if err := json.NewDecoder(r.Body).Decode(&pref); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	updated, err := a.Push.UpdatePreferences(r.Context(), claims.UserID, pref)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, updated)
}

func (a *API) MobileBootstrap(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) || !a.requirePush(w) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sinceID := parseQueryInt(r, "since_alert_id", 0)
	limit := parseQueryInt(r, "limit", 100)
	devices, err := a.Push.ListDevices(r.Context(), claims.UserID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pref, err := a.Push.GetPreferences(r.Context(), claims.UserID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	alerts, err := a.Alerts.ListAlertsSince("", sinceID, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if devices == nil {
		devices = []push.MobileDevice{}
	}
	var alertsOut any = alerts
	if alerts == nil {
		alertsOut = []any{}
	}
	writeJSON(w, map[string]any{
		"server_time":    time.Now().UTC().Format(time.RFC3339),
		"mobile_devices": devices,
		"preferences":    pref,
		"alerts":         alertsOut,
		"live_stats":     a.Stats.List(),
		"ws_path":        "/api/wavecontrol/ws",
	})
}

func (a *API) MobileAlerts(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) {
		return
	}
	sinceID := parseQueryInt(r, "since", 0)
	limit := parseQueryInt(r, "limit", 100)
	status := r.URL.Query().Get("status")
	alerts, err := a.Alerts.ListAlertsSince(status, sinceID, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if alerts == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, alerts)
}

func (a *API) SendMobileTestPush(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) || !a.requirePush(w) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := a.Push.EnqueueTest(r.Context(), claims.UserID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func parseQueryInt(r *http.Request, key string, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return def
	}
	return v
}
