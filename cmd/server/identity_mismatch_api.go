package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/lib/pq"
)

type identityMismatchResponse struct {
	DeviceID     int64     `json:"device_id"`
	ExpectedMAC  string    `json:"expected_mac"`
	ObservedMACs []string  `json:"observed_macs"`
	ObservedIP   string    `json:"observed_ip"`
	Source       string    `json:"source,omitempty"`
	ObservedAt   time.Time `json:"observed_at"`
	LastError    string    `json:"last_error,omitempty"`
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func normalizeCanonicalMAC(raw string) (string, error) {
	hw, err := net.ParseMAC(strings.TrimSpace(raw))
	if err != nil || len(hw) != 6 {
		return "", fmt.Errorf("invalid mac")
	}
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", hw[0], hw[1], hw[2], hw[3], hw[4], hw[5]), nil
}

func containsMAC(macs []string, mac string) bool {
	for _, m := range macs {
		canon, err := normalizeCanonicalMAC(m)
		if err == nil && canon == mac {
			return true
		}
	}
	return false
}

func scanIdentityMismatch(row *sql.Row, deviceID int64) (*identityMismatchResponse, error) {
	var observed pq.StringArray
	var source, lastError sql.NullString
	var out identityMismatchResponse
	out.DeviceID = deviceID
	err := row.Scan(&out.ExpectedMAC, &observed, &out.ObservedIP, &source, &out.ObservedAt, &lastError)
	if err != nil {
		return nil, err
	}
	out.ObservedMACs = make([]string, 0, len(observed))
	for _, m := range observed {
		if canon, err := normalizeCanonicalMAC(m); err == nil {
			out.ObservedMACs = append(out.ObservedMACs, canon)
		}
	}
	if source.Valid {
		out.Source = source.String
	}
	if lastError.Valid {
		out.LastError = lastError.String
	}
	if canon, err := normalizeCanonicalMAC(out.ExpectedMAC); err == nil {
		out.ExpectedMAC = canon
	}
	return &out, nil
}

func (a *API) GetDeviceIdentityMismatch(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) {
		return
	}
	id, err := strconvParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid device id", http.StatusBadRequest)
		return
	}
	mm, err := scanIdentityMismatch(a.DB.QueryRowContext(r.Context(), `
		SELECT lower(expected_mac), observed_macs, host(observed_ip), source, observed_at, last_error
		FROM device_identity_mismatches
		WHERE device_id = $1`, id), id)
	if err == sql.ErrNoRows {
		http.Error(w, "identity mismatch not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, mm)
}

func (a *API) LearnDeviceMAC(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := strconvParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid device id", http.StatusBadRequest)
		return
	}

	var req struct {
		NewMAC string `json:"new_mac"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	var oldMAC, ipAddr, statusReason, role, hostname sql.NullString
	var parentID sql.NullInt64
	err = tx.QueryRowContext(r.Context(), `
		SELECT lower(mac), host(ip_address), status_reason, role, hostname, parent_id
		FROM devices
		WHERE id = $1
		FOR UPDATE`, id).Scan(&oldMAC, &ipAddr, &statusReason, &role, &hostname, &parentID)
	if err == sql.ErrNoRows {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var observed pq.StringArray
	var expectedMAC, observedIP string
	var source, lastError sql.NullString
	var observedAt time.Time
	err = tx.QueryRowContext(r.Context(), `
		SELECT lower(expected_mac), observed_macs, host(observed_ip), source, observed_at, last_error
		FROM device_identity_mismatches
		WHERE device_id = $1
		FOR UPDATE`, id).Scan(&expectedMAC, &observed, &observedIP, &source, &observedAt, &lastError)
	if err == sql.ErrNoRows {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error":   "identity_mismatch_not_recorded",
			"message": "refresh the device to capture the observed replacement MAC before learning it",
		})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !strings.EqualFold(statusReason.String, "mac_mismatch") {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error":         "device_not_in_mac_mismatch",
			"status_reason": statusReason.String,
		})
		return
	}

	oldCanon, err := normalizeCanonicalMAC(oldMAC.String)
	if err != nil {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "current_device_mac_invalid"})
		return
	}
	expectedCanon, err := normalizeCanonicalMAC(expectedMAC)
	if err != nil || expectedCanon != oldCanon {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error":        "identity_mismatch_stale",
			"expected_mac": expectedMAC,
			"current_mac":  oldMAC.String,
		})
		return
	}
	if observedIP != ipAddr.String {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error":       "identity_mismatch_ip_changed",
			"observed_ip": observedIP,
			"current_ip":  ipAddr.String,
		})
		return
	}

	newMAC := strings.TrimSpace(req.NewMAC)
	if newMAC == "" && len(observed) == 1 {
		newMAC = observed[0]
	}
	newCanon, err := normalizeCanonicalMAC(newMAC)
	if err != nil {
		http.Error(w, "invalid new_mac", http.StatusBadRequest)
		return
	}
	if newCanon == oldCanon {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "new_mac_matches_current_mac"})
		return
	}
	if !containsMAC([]string(observed), newCanon) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error":         "new_mac_not_observed",
			"new_mac":       newCanon,
			"observed_macs": []string(observed),
		})
		return
	}

	var conflictID int64
	var conflictHost, conflictIP sql.NullString
	err = tx.QueryRowContext(r.Context(), `
		SELECT id, hostname, host(ip_address)
		FROM devices
		WHERE lower(mac) = $1 AND id <> $2
		LIMIT 1`, newCanon, id).Scan(&conflictID, &conflictHost, &conflictIP)
	if err == nil {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error":      "observed_mac_already_exists",
			"device_id":  conflictID,
			"hostname":   conflictHost.String,
			"ip_address": conflictIP.String,
		})
		return
	}
	if err != sql.ErrNoRows {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	effectiveRole := strings.ToLower(strings.TrimSpace(role.String))
	if effectiveRole != "ap" && effectiveRole != "sta" {
		if parentID.Valid {
			effectiveRole = "sta"
		} else {
			effectiveRole = "ap"
		}
	}

	if _, err := tx.ExecContext(r.Context(), `
		UPDATE devices
		SET mac = $2, status = 'unknown', status_reason = NULL, updated_at = NOW()
		WHERE id = $1`, id, newCanon); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	childParentRefsUpdated := int64(0)
	if effectiveRole == "ap" {
		res, err := tx.ExecContext(r.Context(), `
			UPDATE devices
			SET parent_mac = $1, updated_at = NOW()
			WHERE parent_mac = $2`, newCanon, oldCanon)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		childParentRefsUpdated, _ = res.RowsAffected()
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM device_identity_mismatches WHERE device_id = $1`, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		if effectiveRole == "sta" {
			reason = "sta_replaced"
		} else {
			reason = "ap_replaced"
		}
	}
	change := fmt.Sprintf("learned replacement MAC: old=%s new=%s reason=%s", oldCanon, newCanon, reason)
	if _, err := tx.ExecContext(r.Context(), `INSERT INTO changelog (device_mac, change, "user") VALUES ($1, $2, $3)`, newCanon, change, claims.UserID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if a.Stats != nil {
		a.Stats.RemoveByMAC(oldCanon)
	}
	if a.WSHub != nil {
		a.WSHub.BroadcastStatsUpdate(int(id), newCanon, ipAddr.String, map[string]any{
			"id":                id,
			"mac":               newCanon,
			"old_mac":           oldCanon,
			"role":              effectiveRole,
			"status":            "unknown",
			"db_status":         "unknown",
			"status_reason":     "",
			"db_status_reason":  "",
			"identity_mismatch": nil,
			"online":            false,
		})
	}
	if a.Poller != nil {
		_ = a.Poller.RefreshDeviceByID(id)
	}

	writeJSON(w, map[string]any{
		"ok":                        true,
		"device_id":                 id,
		"old_mac":                   oldCanon,
		"new_mac":                   newCanon,
		"ip_address":                ipAddr.String,
		"hostname":                  hostname.String,
		"role":                      effectiveRole,
		"child_parent_refs_updated": childParentRefsUpdated,
		"observed_at":               observedAt,
	})
}

func strconvParseID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}

func (a *API) attachIdentityMismatch(device map[string]any, deviceID int64) {
	if device == nil || a == nil || a.DB == nil || deviceID == 0 {
		return
	}
	mm, err := scanIdentityMismatch(a.DB.QueryRow(`
		SELECT lower(expected_mac), observed_macs, host(observed_ip), source, observed_at, last_error
		FROM device_identity_mismatches
		WHERE device_id = $1`, deviceID), deviceID)
	if err == nil && mm != nil {
		device["identity_mismatch"] = mm
	}
}

func (a *API) attachIdentityMismatches(devices []map[string]any) {
	if len(devices) == 0 || a == nil || a.DB == nil {
		return
	}
	ids := make([]int64, 0, len(devices))
	for _, d := range devices {
		switch v := d["id"].(type) {
		case int64:
			ids = append(ids, v)
		case int:
			ids = append(ids, int64(v))
		}
	}
	if len(ids) == 0 {
		return
	}

	rows, err := a.DB.Query(`
		SELECT device_id, lower(expected_mac), observed_macs, host(observed_ip), source, observed_at, last_error
		FROM device_identity_mismatches
		WHERE device_id = ANY($1)`, pq.Array(ids))
	if err != nil {
		return
	}
	defer rows.Close()

	byID := make(map[int64]*identityMismatchResponse)
	for rows.Next() {
		var id int64
		var expected, observedIP string
		var observed pq.StringArray
		var source, lastError sql.NullString
		var observedAt time.Time
		if err := rows.Scan(&id, &expected, &observed, &observedIP, &source, &observedAt, &lastError); err != nil {
			continue
		}
		mm := &identityMismatchResponse{DeviceID: id, ExpectedMAC: expected, ObservedIP: observedIP, ObservedAt: observedAt}
		for _, m := range observed {
			if canon, err := normalizeCanonicalMAC(m); err == nil {
				mm.ObservedMACs = append(mm.ObservedMACs, canon)
			}
		}
		if canon, err := normalizeCanonicalMAC(mm.ExpectedMAC); err == nil {
			mm.ExpectedMAC = canon
		}
		if source.Valid {
			mm.Source = source.String
		}
		if lastError.Valid {
			mm.LastError = lastError.String
		}
		byID[id] = mm
	}

	for _, d := range devices {
		var id int64
		switch v := d["id"].(type) {
		case int64:
			id = v
		case int:
			id = int64(v)
		}
		if mm := byID[id]; mm != nil {
			d["identity_mismatch"] = mm
		}
	}
}
