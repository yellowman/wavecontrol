package poller

import (
	"crypto/tls"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yellowman/wavecontrol/internal/airmax"
	"github.com/yellowman/wavecontrol/internal/stats"
	"github.com/yellowman/wavecontrol/internal/udebug"
)

// pollDeviceAirMAX polls using AirMAX API (login.cgi + status.cgi)
func (p *Poller) pollDeviceAirMAX(job pollJob) pollResult {

	// Get TLS-verified transport for this device (credentials will be sent)
	var transport http.RoundTripper
	if p.tlsManager != nil {
		transport = p.tlsManager.GetTransport(job.DeviceID)
	}

	// Ultra debug: when enabled for this device, wrap the transport to capture
	// request/response details into the per-device ring buffer.
	//
	// If no TLS manager is configured, AirMAX clients normally create their own
	// insecure transport. For ultra-debug we explicitly create one so we can wrap it.
	baseTransport := transport
	if baseTransport == nil && p.ultraDebug != nil && p.ultraDebug.IsEnabled(job.DeviceID) {
		baseTransport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	if p.ultraDebug != nil && p.ultraDebug.IsEnabled(job.DeviceID) {
		baseTransport = udebug.WrapTransport(p.ultraDebug, job.DeviceID, baseTransport, "airmax", udebug.DefaultCaptureLimit)
	}

	client := airmax.NewClientWithTransport(job.IP, 10*time.Second, baseTransport)

	// Build list of credentials to try (job credentials first, then AP credential pairs)
	var creds []airmax.Credential
	if job.Username != "" && job.Password != "" {
		creds = append(creds, airmax.Credential{Username: job.Username, Password: job.Password})
	}
	cfg := p.cfgSnapshot()
	for _, c := range cfg.apCreds {
		// Skip if already added from job
		if c.Username == job.Username && c.Password == job.Password {
			continue
		}
		creds = append(creds, airmax.Credential{Username: c.Username, Password: c.Password})
	}

	// Log credential count for debugging
	p.logDebug("AirMAX %s: attempting login with %d credentials (job.Username=%q, apCreds=%d)",
		job.IP, len(creds), job.Username, len(cfg.apCreds))

	if len(creds) == 0 {
		log.Printf("WARN: AirMAX %s: no credentials available", job.IP)
		return pollNotThisType
	}

	// Try to authenticate with all credentials
	err := client.LoginWithCredentials(creds)

	p.logDebug("AirMAX %s: LoginWithCredentials returned err=%v", job.IP, err)

	if err != nil {
		// Capture previous in-memory status before we mutate it. This lets us reliably persist the
		// offline-threshold transition (unknown -> offline) to the DB exactly once.
		prevStatus := stats.StatusUnknown
		if old := func() *stats.DeviceStats {
			if s := p.store.GetByMAC(job.MAC); s != nil {
				return s
			}
			return p.store.Get(job.IP)
		}(); old != nil {
			prevStatus = old.Status
			if prevStatus == "" {
				if old.Online {
					prevStatus = stats.StatusOnline
				} else {
					prevStatus = stats.StatusUnknown
				}
			}
		}

		errStr := err.Error()
		unreachable := isNetworkUnreachable(err)
		status := p.getDeviceStatus(job.IP, unreachable)
		reason := truncateForDB("status_reason", job.IP, job.DeviceID, statusReasonFromError(err), 128)
		p.logDebug("AirMAX %s: auth failed: %v (errStr=%q, unreachable=%v, targetStatus=%s)", job.IP, err, errStr, unreachable, status)
		p.recordFailure(job.IP, unreachable)
		errMsg := fmt.Sprintf("airmax auth failed: %v", err)
		leftOnline := false
		if status == "offline" {
			leftOnline = p.store.SetOfflineWithReasonByMAC(job.MAC, job.IP, reason, errMsg)
			p.store.TrackStabilityStatus(job.IP, "", stats.StatusOffline, 0)
		} else {
			// Unknown until offline threshold is reached. If unreachable, do not advance LastSeen.
			leftOnline = p.store.SetStatusByMAC(job.MAC, job.IP, stats.StatusUnknown, reason, errMsg, !unreachable)
			p.store.TrackStabilityStatus(job.IP, "", stats.StatusUnknown, 0)
		}
		// Broadcast status via WebSocket
		if p.wsHub != nil {
			p.wsHub.BroadcastStatsUpdate(int(job.DeviceID), job.MAC, job.IP, map[string]any{"online": false, "status": status, "db_status": status, "status_reason": reason, "last_error": err.Error()})
		}

		// Persist status to DB when:
		//  1) we just left "online" (online -> unknown/offline),
		//  2) the device responded and should be "unknown" (offline -> unknown), OR
		//  3) we just crossed the offline threshold (unknown -> offline).
		becameOffline := prevStatus != stats.StatusOffline && status == "offline"
		shouldUpdate := leftOnline || becameOffline || (!unreachable && status == "unknown")
		if shouldUpdate {
			p.logDebug("AirMAX %s: updating DB to '%s' (leftOnline=%v, becameOffline=%v, unreachable=%v)", job.IP, status, leftOnline, becameOffline, unreachable)
			// Only advance last_seen when the device actually responded (e.g. auth failure, TCP RST).
			// For truly unreachable failures we intentionally do NOT advance last_seen.
			var result sql.Result
			var dbErr error
			if unreachable {
				result, dbErr = dbExecCtx(p.db, dbCtxForJob(job, "airmax_update_status_auth_fail_unreachable"), `UPDATE devices SET status = $1, status_reason = $3 WHERE id = $2`, status, job.DeviceID, reason)
			} else {
				result, dbErr = dbExecCtx(p.db, dbCtxForJob(job, "airmax_update_status_auth_fail"), `UPDATE devices SET status = $1, status_reason = $3, last_seen = NOW() WHERE id = $2`, status, job.DeviceID, reason)
			}
			if dbErr != nil {
				log.Printf("WARN: AirMAX %s: DB update failed: %v", job.IP, dbErr)
			} else if rows, _ := result.RowsAffected(); rows == 0 {
				log.Printf("WARN: AirMAX %s: DB update affected 0 rows (device ID %d)", job.IP, job.DeviceID)
			}
			// Also update children (STAs) to same status
			p.updateChildrenStatus(job.DeviceID, status)
		} else {
			p.logDebug("AirMAX %s: NOT updating DB (leftOnline=%v, unreachable=%v, status=%s)", job.IP, leftOnline, unreachable, status)
		}
		return pollNotThisType // Auth failed - might not be AirMAX
	}

	p.logDebug("AirMAX %s: auth success, fetching status", job.IP)

	// Fetch status
	status, err := client.GetStatus()
	if err != nil {
		p.logDebug("AirMAX %s: status fetch failed: %v", job.IP, err)
		// Device authenticated so it's reachable - mark as unknown, not offline
		p.recordFailure(job.IP, false) // false = device responded (authenticated)
		errMsg := fmt.Sprintf("status failed: %v", err)
		leftOnline := p.store.SetUnknownByMAC(job.MAC, job.IP, "status_failed", errMsg)
		p.store.TrackStabilityStatus(job.IP, "", stats.StatusUnknown, 0)
		// Broadcast unknown status via WebSocket
		if p.wsHub != nil {
			p.wsHub.BroadcastStatsUpdate(int(job.DeviceID), job.MAC, job.IP, map[string]any{"online": false, "status": "unknown", "db_status": "unknown", "status_reason": "status_failed", "last_error": err.Error()})
		}
		// Always update to "unknown" when device authenticated - it's clearly reachable
		if leftOnline {
			p.updateChildrenStatus(job.DeviceID, "unknown")
		}
		dbExecIgnoreCtx(p.db, dbCtxForJob(job, "airmax_mark_unknown_status_failed"), `UPDATE devices SET status = 'unknown', status_reason = $2, last_seen = NOW() WHERE id = $1`, job.DeviceID, "status_failed")
		return pollFailed // Auth succeeded but status failed
	}

	// Convert to unified DeviceStats
	deviceStats := p.convertAirMAXStats(status)
	deviceStats.IP = job.IP

	// Canonicalize MAC identity.
	//
	// IMPORTANT: The canonical MAC is the Ethernet/management MAC.
	// Some devices also expose a wireless interface MAC; that is *not* preferred.
	// If the device responds with a different Ethernet MAC than what the DB
	// expects, do not apply stats to the wrong device (common when an IP is
	// reassigned).
	eth0MAC := ""
	if e := status.GetEth0(); e != nil {
		eth0MAC = e.MAC
	}
	macRes := canonicalizeDeviceMACPreferred(job.MAC, eth0MAC, status.Wireless.APMAC)
	if macRes.Mismatch {
		return p.handleMACMismatch(job, "airmax", macRes.Observed)
	}
	if macRes.Canonical != "" {
		deviceStats.MAC = macRes.Canonical
	}

	// Parse config from status data (no extra API call needed)
	// Only update periodically to match Wave behavior
	if p.shouldFetchConfig(job.IP) {
		if cfg := p.parseAirMAXConfig(status); cfg != nil {
			deviceStats.Config = cfg
			p.logDebug("pollDeviceAirMAX %s: parsed config: mode=%s-%s frame=%s sync=%v",
				job.IP, cfg.Mode, cfg.NetMode, cfg.FrameMode, cfg.GPSSync)
		}
	} else {
		// Preserve existing config from store
		if oldStats := func() *stats.DeviceStats {
			if s := p.store.GetByMAC(job.MAC); s != nil {
				return s
			}
			return p.store.Get(job.IP)
		}(); oldStats != nil && oldStats.Config != nil {
			deviceStats.Config = oldStats.Config
		}
	}

	// Check for changes before updating store.
	oldStats := p.store.GetByMAC(job.MAC)
	if oldStats == nil {
		oldStats = p.store.Get(job.IP)
	}
	hostnameChanged := deviceStats.Hostname != "" && (oldStats == nil || oldStats.Hostname != deviceStats.Hostname)

	var peers []*stats.PeerStats
	peerSnapshotAccepted := true
	if status.IsAP() {
		peers = p.convertAirMAXPeers(status)
		peerSnapshotAccepted = p.acceptPeerSnapshot(job.DeviceID, peers)
		if !peerSnapshotAccepted && oldStats != nil {
			deviceStats.PeerCount = oldStats.PeerCount
			deviceStats.Peers = oldStats.Peers
		}
	}

	// Update memory store - returns true if state changed (offline->online).
	becameOnline := p.store.Update(job.IP, deviceStats)
	p.store.TrackStabilityStatus(job.IP, deviceStats.Hostname, stats.StatusOnline, deviceStats.Uptime)

	var ipChanges map[string]string
	if status.IsAP() {
		if peerSnapshotAccepted {
			p.enrichPeersWithMAC(peers)
			ipChanges = p.store.UpdatePeers(deviceStats.MAC, job.IP, peers)
		}
	} else {
		// A directly polled STA must not retain or create child rows.
		ipChanges = p.store.UpdatePeers(deviceStats.MAC, job.IP, nil)
	}

	// Broadcast via WebSocket (after peer update)
	if p.wsHub != nil {
		p.wsHub.BroadcastStatsUpdate(int(job.DeviceID), deviceStats.MAC, job.IP, deviceStats)
	}

	// Persist/update STA rows in DB (AP only). Run after broadcast to keep UI responsive.
	// NOTE: When peers is empty/nil for an AP, this call will mark all child STAs as offline
	// in the DB ("not_associated").
	if status.IsAP() && peerSnapshotAccepted {
		p.updateSTAsInDB(job.DeviceID, peers, ipChanges)
	}

	// Only write to DB on state transition or info change (not every poll)
	// If this device looks like LTU but the DB/platform was previously marked airmax,
	// update static info so future polls use the correct Wave/LTU logic and UI shows LTU fields.
	needsPlatformUpdate := false
	if strings.ToLower(job.Platform) != "ltu" {
		modelLower := strings.ToLower(status.GetModel())
		fwLower := strings.ToLower(status.Host.FWVersion)
		if strings.Contains(modelLower, "ltu") || strings.HasPrefix(fwLower, "afltu") || strings.HasPrefix(fwLower, "af5xhd") {
			needsPlatformUpdate = true
		}
	}

	if becameOnline || hostnameChanged || needsPlatformUpdate {
		// Device came online or info changed - full sync to DB
		p.updateAirMAXDeviceInfo(job.DeviceID, job.IP, status, client, deviceStats.MAC)
		if becameOnline {
			p.clearIdentityMismatch(job.DeviceID)
			dbExecIgnoreCtx(p.db, dbCtxForJob(job, "airmax_mark_online"), `UPDATE devices SET status = 'online', status_reason = NULL, last_seen = NOW() WHERE id = $1`, job.DeviceID)
		}
	}
	// If already online and nothing changed, no DB write needed

	return pollSuccess
}

// convertAirMAXStats converts AirMAX status to unified DeviceStats
func (p *Poller) convertAirMAXStats(status *airmax.Status) *stats.DeviceStats {
	ds := &stats.DeviceStats{
		MAC:      strings.ToLower(status.GetMAC()),
		Hostname: status.Host.Hostname,
		Online:   true,
		LastSeen: time.Now(),
		Uptime:   status.Host.Uptime,
	}

	// RAM stats
	ds.RAM.Total = status.Host.TotalRAM
	ds.RAM.Free = status.Host.FreeRAM
	if ds.RAM.Total > 0 {
		ds.RAM.Usage = int((float64(ds.RAM.Total-ds.RAM.Free) / float64(ds.RAM.Total)) * 100)
	}

	// CPU
	ds.CPU = []stats.CPUCore{{
		ID:    "main",
		Usage: int(status.Host.CPULoad),
	}}

	// Temperature
	ds.Temperature.CPU = status.Host.Temperature

	// GPS
	if status.GPS != nil && status.GPS.GetFix() > 0 {
		ds.GPS.Fix = true
		ds.GPS.Lat = status.GPS.GetLat()
		ds.GPS.Lon = status.GPS.GetLon()
		ds.GPS.Alt = status.GPS.Alt
		ds.GPS.Sats = status.GPS.Sats
	}

	// Standard AirMAX wireless stats (also used for AirFiber links)
	// NoiseF is the general radio noise floor (always present)
	ds.Wireless.Radio5GHz = &stats.RadioStats{
		ID:         "ath0",
		Name:       "5GHz",
		LinkState:  "up",
		Frequency:  status.Wireless.GetFrequency(),
		TxPower:    status.Wireless.GetTXPower(),
		NoiseFloor: status.Wireless.GetNoiseF(),
		ChannelBW:  status.Wireless.GetChanBW(),
	}
	// AirFiber exposes a separate linkstate string (e.g. connected/disconnected). Map it into a
	// stable up/down state when present.
	if status.IsAirFiber() && status.Airfiber != nil && status.Airfiber.LinkState != "" {
		ls := strings.ToLower(strings.TrimSpace(status.Airfiber.LinkState))
		switch ls {
		case "connected", "up", "link_up":
			ds.Wireless.Radio5GHz.LinkState = "up"
		case "disconnected", "down", "link_down":
			ds.Wireless.Radio5GHz.LinkState = "down"
		default:
			ds.Wireless.Radio5GHz.LinkState = ls
		}
	}

	// For STA mode, use the more accurate STA-specific signal values
	if status.IsSTA() || status.IsAirFiber() {
		ds.Wireless.Radio5GHz.Signal = status.Wireless.GetSignal()
		ds.Wireless.Radio5GHz.SignalCombined = status.Wireless.GetSignal() // Will be updated from peer if per-chain available
		ds.Wireless.Radio5GHz.SignalQuality = stats.SignalQuality5GHz(ds.Wireless.Radio5GHz.Signal)
		ds.Wireless.Radio5GHz.RSSI = status.Wireless.GetRSSI()
		// wireless.noisefloor is STA-specific, more accurate than general noisef
		if status.Wireless.GetNoiseFloor() != 0 {
			ds.Wireless.Radio5GHz.NoiseFloor = status.Wireless.GetNoiseFloor()
		}
	}

	// Polling/AirMAX stats
	if polling := status.Wireless.GetPollingInfo(); polling != nil {
		ds.Wireless.TxRate = int64(polling.DCap * 1000) // Convert to bps
		ds.Wireless.RxRate = int64(polling.UCap * 1000)
	}

	// Interface stats
	for name, iface := range status.Interfaces {
		// Derive interface type from name
		ifaceType := "unknown"
		switch {
		case strings.HasPrefix(name, "eth"):
			ifaceType = "ethernet"
		case strings.HasPrefix(name, "ath"):
			ifaceType = "wireless"
		case strings.HasPrefix(name, "br"):
			ifaceType = "bridge"
		case strings.HasPrefix(name, "wifi"):
			ifaceType = "wireless"
		case strings.HasPrefix(name, "wlan"):
			ifaceType = "wireless"
		}

		// Map AirOS interface data into our normalized InterfaceStats schema.
		speedMbps := parseSpeed(iface.CurrentSpeed)
		if speedMbps == 0 {
			speedMbps = parseSpeed(iface.Speed)
		}
		currentSpeed := iface.CurrentSpeed
		if currentSpeed == "" {
			currentSpeed = iface.Speed
		}

		// airmax.InterfaceInfo does not expose every field our UI schema has.
		// Populate what we can and infer the rest conservatively.
		enabled := iface.Status != "disabled"
		plugged := speedMbps > 0 || iface.Status == "up" || iface.Status == "connected"
		if !enabled {
			plugged = false
		}

		statusStr := iface.Status
		switch statusStr {
		case "", "enabled":
			statusStr = ifStatus(plugged)
		case "disabled":
			// keep
		default:
			// keep
		}

		ds.Interfaces = append(ds.Interfaces, stats.InterfaceStats{
			ID:              name,
			Name:            name,
			Type:            ifaceType,
			Enabled:         enabled,
			Plugged:         plugged,
			Status:          statusStr,
			Description:     "",
			Speed:           speedMbps,
			CurrentSpeed:    currentSpeed,
			ConfiguredSpeed: iface.Speed,
			RxBytes:         iface.RxBytes,
			TxBytes:         iface.TxBytes,
			RxPackets:       0,
			TxPackets:       0,
			RxErrors:        iface.RxErrors,
			TxErrors:        iface.TxErrors,
		})
	}

	// Peer count
	ds.PeerCount = len(status.GetStations())

	return ds
}

// convertAirMAXPeers converts AirMAX station list to PeerStats
func (p *Poller) convertAirMAXPeers(status *airmax.Status) []*stats.PeerStats {
	var peers []*stats.PeerStats

	// AirOS station list rate fields are not always consistent across firmware.
	// On many devices they are reported in Mbps (e.g. 150), but some variants
	// report in kbps (e.g. 150000). Some firmwares/devices also report bps
	// (e.g. 150000000). Convert conservatively using a heuristic.
	rateToBps := func(v int) int64 {
		if v <= 0 {
			return 0
		}
		// Heuristic:
		//  - >= 10,000,000: assume already bps (AirMAX PHY rates are well below this)
		//  - >= 10,000: assume kbps
		//  - else: assume Mbps
		if v >= 10000000 {
			return int64(v)
		}
		if v >= 10000 {
			return int64(v) * 1000
		}
		return int64(v) * 1000000
	}

	for _, sta := range status.GetStations() {
		peer := &stats.PeerStats{
			MAC:        strings.ToLower(sta.MAC), // Normalize MAC to lowercase
			Hostname:   sta.Name,
			IP:         sta.LastIP,
			Signal:     sta.GetSignal(),
			RSSI:       sta.GetRSSI(),
			NoiseFloor: sta.GetNoiseFloor(),
			Distance:   float64(sta.GetDistance()),
			TxBytes:    sta.GetTXBytes(),
			RxBytes:    sta.GetRXBytes(),
			TxRate:     rateToBps(sta.TX),
			RxRate:     rateToBps(sta.RX),
			TxPackets:  sta.GetTXPackets(),
			RxPackets:  sta.GetRXPackets(),
			Uptime:     sta.GetUptime(),
		}

		// Per-chain signal for 5GHz radio
		// Note: sta.GetChainSignals() converts positive RSSI to dBm using rssiToDbm().
		// AirMAX ChainRSSI may be [35, 33] (positive RSSI) → [-60, -62] (dBm)
		// The MRC combined signal is then computed from the dBm values.
		peer.Radio5GHz = &stats.PeerRadioStats{
			ID:        "ath0",
			Active:    true,
			Connected: true,
			LinkState: "connected",
			Signal:    sta.GetSignal(),
		}
		if len(sta.ChainRSSI) > 0 {
			peer.Radio5GHz.SignalPerChain = sta.GetChainSignals() // Already converted to dBm
			peer.Radio5GHz.SignalCombined = stats.CombineSignals(peer.Radio5GHz.SignalPerChain)
		} else {
			peer.Radio5GHz.SignalCombined = sta.GetSignal()
		}
		peer.Radio5GHz.SignalQuality = stats.SignalQuality5GHz(peer.Radio5GHz.SignalCombined)

		// SNR calculation: both values must be in dBm
		if sta.GetNoiseFloor() != 0 {
			peer.Radio5GHz.NoiseFloor = sta.GetNoiseFloor()
			peer.Radio5GHz.SNR = sta.GetSignal() - sta.GetNoiseFloor()
		}

		// Modulation / MCS (AirOS 8+ exposes tx_idx/rx_idx + tx_nss/rx_nss on each STA)
		// Note: status.Wireless is a struct (not a pointer). If the wireless field is missing in
		// a given status.cgi response, it will remain its zero value and IEEEMode will be empty.
		ieeeMode := status.Wireless.IEEEMode
		// tx_idx/rx_idx are valid at 0 (MCS0). When the field is missing, our
		// JSON unmarshal sets it to -1 so we can distinguish "absent" from "0".
		if sta.TXIdx >= 0 && sta.RXIdx >= 0 {
			mcs := &stats.MCSStats{
				TxIdx: sta.TXIdx,
				RxIdx: sta.RXIdx,
			}
			// Optional per-direction labels include NSS when available (keeps UI readable)
			if sta.TXIdx >= 0 {
				if sta.TXNSS > 0 {
					mcs.TxLabel = fmt.Sprintf("MCS %d (%dSS)", sta.TXIdx, sta.TXNSS)
					mcs.TxIdxIdeal = airmaxIdealMCSIdx(ieeeMode, sta.TXNSS)
				} else {
					mcs.TxLabel = fmt.Sprintf("MCS %d", sta.TXIdx)
					mcs.TxIdxIdeal = airmaxIdealMCSIdx(ieeeMode, 0)
				}
			}
			if sta.RXIdx >= 0 {
				if sta.RXNSS > 0 {
					mcs.RxLabel = fmt.Sprintf("MCS %d (%dSS)", sta.RXIdx, sta.RXNSS)
					mcs.RxIdxIdeal = airmaxIdealMCSIdx(ieeeMode, sta.RXNSS)
				} else {
					mcs.RxLabel = fmt.Sprintf("MCS %d", sta.RXIdx)
					mcs.RxIdxIdeal = airmaxIdealMCSIdx(ieeeMode, 0)
				}
			}
			peer.Radio5GHz.MCS = mcs
		}

		// AirMax capacity stats (in kbps, convert to bps)
		if sta.AirMax != nil {
			peer.Radio5GHz.Capacity = &stats.CapacityStats{
				DL:       int64(sta.AirMax.DownlinkCapacity * 1000),
				UL:       int64(sta.AirMax.UplinkCapacity * 1000),
				Combined: int64((sta.AirMax.DownlinkCapacity + sta.AirMax.UplinkCapacity) * 1000),
			}
			// CINR / airtime stats (AirMAX reports rx/tx from the AP perspective)
			//   tx -> downlink (AP -> station), rx -> uplink (station -> AP)
			peer.Radio5GHz.CINR = &stats.CINRStats{
				DL: sta.AirMax.TX.CINR,
				UL: sta.AirMax.RX.CINR,
			}
			peer.Radio5GHz.AirtimeDL = float64(sta.AirMax.TX.Usage)
			peer.Radio5GHz.AirtimeUL = float64(sta.AirMax.RX.Usage)

			// EVM (if present) is returned as a per-chain time series.
			// We store a simple averaged "last sample" value per direction.
			dlEVM, dlOK := avgLastEVM(sta.AirMax.TX.EVM)
			ulEVM, ulOK := avgLastEVM(sta.AirMax.RX.EVM)
			if dlOK || ulOK {
				peer.Radio5GHz.EVM = &stats.EVMStats{}
				if dlOK {
					peer.Radio5GHz.EVM.DL = dlEVM
				}
				if ulOK {
					peer.Radio5GHz.EVM.UL = ulEVM
				}
			}
		}

		// Counters from nested stats or direct fields
		if sta.Stats != nil {
			peer.TxBytes = sta.Stats.TXBytes
			peer.RxBytes = sta.Stats.RXBytes
			peer.TxPackets = sta.Stats.TXPackets
			peer.RxPackets = sta.Stats.RXPackets
		} else {
			peer.TxBytes = sta.TXBytes
			peer.RxBytes = sta.RXBytes
			peer.TxPackets = sta.TXPackets
			peer.RxPackets = sta.RXPackets
		}

		// Remote device info (what the STA reports about itself)
		// sta.Remote contains the STA's perspective - NOT a separate radio
		if sta.Remote != nil {
			if sta.Remote.Hostname != "" {
				peer.Hostname = sta.Remote.Hostname
			}
			peer.Model = sta.Remote.Platform
			peer.Firmware = sta.Remote.Version
			peer.Temperature = float64(sta.Remote.Temperature)
			peer.TXPower = sta.Remote.TXPower

			// Remote signal is what STA sees from AP (STA's RX perspective).
			// Keep the legacy top-level fields for API/UI compatibility and mirror them
			// into the 5 GHz peer radio so server-side reports can compare AP-side and
			// STA-side chains through the same radio record.
			if sta.Remote.Signal != 0 {
				peer.RemoteSignal = sta.Remote.GetSignal()
				peer.RemoteNoiseFloor = sta.Remote.GetNoiseFloor()
				if len(sta.Remote.ChainRSSI) > 0 {
					peer.RemoteSignalPerChain = sta.Remote.GetChainSignals()
					peer.RemoteSignalCombined = stats.CombineSignals(peer.RemoteSignalPerChain)
				} else {
					peer.RemoteSignalCombined = peer.RemoteSignal
				}
				peer.RemoteSignalQuality = stats.SignalQuality5GHz(peer.RemoteSignalCombined)
				if peer.Radio5GHz != nil {
					peer.Radio5GHz.RemoteSignal = peer.RemoteSignal
					peer.Radio5GHz.RemoteNoiseFloor = peer.RemoteNoiseFloor
					peer.Radio5GHz.RemoteSignalPerChain = peer.RemoteSignalPerChain
					peer.Radio5GHz.RemoteSignalCombined = peer.RemoteSignalCombined
					peer.Radio5GHz.RemoteSignalQuality = peer.RemoteSignalQuality
					if peer.RemoteNoiseFloor != 0 {
						peer.Radio5GHz.RemoteSNR = peer.RemoteSignal - peer.RemoteNoiseFloor
					}
				}
			}
		}

		peers = append(peers, peer)
	}

	return peers
}

// airmaxIdealMCSIdx returns the best-case MCS index for modulation classification.
//
// AirOS exposes per-station tx_idx/rx_idx values; the ideal varies by radio mode.
// For 11ac/VHT, MCS ranges 0..9 (per spatial stream). For 11n/HT, MCS ranges
// 0..(8*NSS-1) where NSS is 1..4.
func airmaxIdealMCSIdx(ieeeMode string, nss int) int {
	m := strings.ToUpper(ieeeMode)
	if strings.Contains(m, "11AC") || strings.Contains(m, "VHT") {
		return 9
	}
	if strings.Contains(m, "11N") || strings.Contains(m, "HT") {
		if nss <= 0 {
			// Most AirMAX M gear is 2x2; treat as best-case MCS15 if we don't know NSS.
			return 15
		}
		if nss > 4 {
			nss = 4
		}
		return 8*nss - 1
	}
	if nss > 0 {
		if nss > 4 {
			nss = 4
		}
		return 8*nss - 1
	}
	return 0
}

// updateAirMAXDeviceInfo updates static device info from AirMAX status.
//
// IMPORTANT: canonicalMAC should be the authoritative MAC for this device record
// (typically the DB/job MAC). We should not silently change identity based on
// whichever interface MAC the device happens to report.
func (p *Poller) updateAirMAXDeviceInfo(deviceID int64, ip string, status *airmax.Status, client *airmax.Client, canonicalMAC string) {
	// Determine role
	role := "sta"
	if status.IsAP() {
		role = "ap"
	}

	// Determine platform and flavor
	platform := "airmax"

	// Use firmware platform prefix as flavor (XC, XM, XW, WA, AF11, etc.)
	// This is most useful for firmware matching
	flavor := status.DetectFirmwarePlatform()
	if flavor == "" {
		// Fall back to model-based detection
		flavor = status.DetectFlavor()
	}

	// Firmware: store full string + extracted version separately
	firmwareFull := status.Host.FWVersion
	firmwareVersion := status.ExtractVersion()
	product := status.GetModel()

	// NOTE: Some AirOS builds expose a Wave-style /api/v1.0/device endpoint, but many do not (404).
	// Probing it here causes confusing 404s in ultra logs and adds overhead, so we intentionally skip it.

	// Detect LTU devices - they use airMAX API but should have platform "ltu"
	// Check product name first, then firmware prefix
	productLower := strings.ToLower(product)
	if strings.Contains(productLower, "ltu") {
		platform = "ltu"
		// Set flavor for firmware matching - only Rocket uses AFLTUROCKET
		if strings.Contains(productLower, "rocket") {
			flavor = "AFLTUROCKET"
		} else {
			// LTU-Lite, LTU-LR, etc all use AFLTU
			flavor = "AFLTU"
		}
	} else {
		fwLower := strings.ToLower(firmwareFull)
		if strings.HasPrefix(fwLower, "afltu") || strings.HasPrefix(fwLower, "af5xhd") {
			platform = "ltu"
		}
	}

	// Host IP for context (provided by caller)
	hostIP := ip

	// Enforce DB column limits to avoid pq: value too long errors
	hostname := truncateForDB("hostname", hostIP, deviceID, status.Host.Hostname, 128)
	mac := canonicalMAC
	if mac == "" {
		mac = status.GetMAC()
	}
	mac = stats.NormalizeMAC(mac)
	mac = truncateForDB("mac", hostIP, deviceID, mac, 17)
	product = truncateForDB("product", hostIP, deviceID, product, 64)
	firmwareFull = truncateForDB("firmware", hostIP, deviceID, firmwareFull, 128)
	firmwareVersion = truncateForDB("firmware_version", hostIP, deviceID, firmwareVersion, 32)
	platform = truncateForDB("platform", hostIP, deviceID, platform, 16)
	flavor = truncateForDB("flavor", hostIP, deviceID, flavor, 16)
	role = truncateForDB("role", hostIP, deviceID, role, 8)
	ssid := truncateForDB("ssid", hostIP, deviceID, status.Wireless.ESSID, 64)

	// Detect MAC conflicts to avoid unique constraint spam (warn once, update everything else)
	macConflict := false
	if mac != "" {
		var conflictID int64
		var conflictIP, conflictHost sql.NullString
		err := p.db.QueryRow(`
			SELECT id, host(ip_address), hostname
			FROM devices
			WHERE mac = $1 AND id <> $2
			LIMIT 1
		`, mac, deviceID).Scan(&conflictID, &conflictIP, &conflictHost)
		if err == nil {
			macConflict = true
			key := fmt.Sprintf("%d|%s", deviceID, mac)
			if _, loaded := macConflictWarned.LoadOrStore(key, struct{}{}); !loaded {
				log.Printf("WARN: device %s id=%d attempted to claim MAC %s already used by device id=%d ip=%s hostname=%s; skipping MAC update",
					hostIP, deviceID, mac, conflictID, conflictIP.String, conflictHost.String)
			}
		}
	}

	lat := nullFloat(status.GPS, func(g *airmax.GPSInfo) float64 { return g.GetLat() })
	lon := nullFloat(status.GPS, func(g *airmax.GPSInfo) float64 { return g.GetLon() })

	// Only update if values actually changed (avoid unnecessary DB writes every poll)
	res, err := dbExecCtx(p.db, dbCtxForMAC(mac, hostIP, "airmax_device_info_update", deviceID), `
		UPDATE devices SET
			hostname = $1,
			mac = CASE
				WHEN NULLIF($2, '') IS NULL THEN mac
				WHEN EXISTS (SELECT 1 FROM devices d2 WHERE d2.mac = $2 AND d2.id <> $14) THEN mac
				ELSE $2
			END,
			product = $3,
			firmware = $4,
			firmware_version = $5,
			platform = $6,
			flavor = $7,
			role = $8,
			parent_id = CASE WHEN $8 = 'ap' THEN NULL ELSE parent_id END,
			parent_mac = CASE WHEN $8 = 'ap' THEN NULL ELSE parent_mac END,
			ssid = $9,
			frequency = $10,
			channel_width = $11,
			gps_lat = $12,
			gps_lon = $13
		WHERE id = $14 AND (
			hostname IS DISTINCT FROM $1 OR
			product IS DISTINCT FROM $3 OR
			firmware IS DISTINCT FROM $4 OR
			firmware_version IS DISTINCT FROM $5 OR
			platform IS DISTINCT FROM $6 OR
			flavor IS DISTINCT FROM $7 OR
			role IS DISTINCT FROM $8 OR
			ssid IS DISTINCT FROM $9 OR
			frequency IS DISTINCT FROM $10 OR
			channel_width IS DISTINCT FROM $11 OR
			gps_lat IS DISTINCT FROM $12 OR
			gps_lon IS DISTINCT FROM $13 OR
			(
				NULLIF($2, '') IS NOT NULL
				AND NOT EXISTS (SELECT 1 FROM devices d2 WHERE d2.mac = $2 AND d2.id <> $14)
				AND mac IS DISTINCT FROM $2
			)
		)
	`,
		hostname,
		mac,
		product,
		firmwareFull,
		firmwareVersion,
		platform,
		flavor,
		role,
		ssid,
		status.Wireless.GetFrequency(),
		status.Wireless.GetChanBW(),
		lat,
		lon,
		deviceID,
	)
	if err != nil {
		p.logDebug("updateAirMAXDeviceInfo: device %d ip %s: %v", deviceID, hostIP, err)
		return
	}

	// Propagate static DB changes to UI without forcing a full refresh
	if res != nil && p.wsHub != nil {
		if rows, _ := res.RowsAffected(); rows > 0 {
			patch := map[string]any{
				"id":               deviceID,
				"hostname":         hostname,
				"product":          product,
				"firmware":         firmwareFull,
				"firmware_version": firmwareVersion,
				"platform":         platform,
				"flavor":           flavor,
				"role":             role,
				"ssid":             ssid,
				"frequency":        status.Wireless.GetFrequency(),
				"channel_width":    status.Wireless.GetChanBW(),
				"gps_lat":          lat,
				"gps_lon":          lon,
			}
			if !macConflict && mac != "" {
				patch["mac"] = mac
			}
			p.wsHub.BroadcastDeviceUpdate(int(deviceID), hostIP, patch)
		}
	}
}

func ifStatus(plugged bool) string {
	if plugged {
		return "up"
	}
	return "down"
}

func parseSpeed(s string) int {
	// Parse speed strings like "1000", "100Mbps", or negotiated values like "1000-full".
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "Mbps")
	s = strings.TrimSuffix(s, "mbps")
	s = strings.TrimSpace(s)

	// Extract the leading integer portion.
	i := 0
	for i < len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			break
		}
		i++
	}
	if i == 0 {
		return 0
	}
	v, _ := strconv.Atoi(s[:i])
	return v
}

// avgLastEVM computes a simple average of the most recent EVM sample across chains.
//
// AirMAX returns EVM as a per-chain time series; we want a single, stable scalar
// for dashboards and issue detection.
func avgLastEVM(evm [][]float64) (float64, bool) {
	if len(evm) == 0 {
		return 0, false
	}

	sum := 0.0
	n := 0
	for _, chain := range evm {
		if len(chain) == 0 {
			continue
		}
		sum += chain[len(chain)-1]
		n++
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

func nullFloat[T any](ptr *T, getter func(*T) float64) *float64 {
	if ptr == nil {
		return nil
	}
	v := getter(ptr)
	if v == 0 {
		return nil
	}
	return &v
}
