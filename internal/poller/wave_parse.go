package poller

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/yellowman/wavecontrol/internal/stats"
)

// parseStats extracts stats from API response
func (p *Poller) parseStats(data []byte, platform string) (*stats.DeviceStats, []*stats.PeerStats) {
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil || len(raw) == 0 {
		return &stats.DeviceStats{}, nil
	}

	r := raw[0]
	ds := &stats.DeviceStats{
		Online:   true,
		LastSeen: time.Now(),
	}
	// Prefer the device-provided statistics timestamp when available.
	// The Wave /statistics endpoint typically returns milliseconds since epoch.
	if ts := int64(getFloat(r, "timestamp")); ts > 0 {
		ds.LastSeen = time.UnixMilli(ts)
	}

	// Device stats
	if device, ok := r["device"].(map[string]any); ok {
		ds.Uptime = int64(getFloat(device, "uptime"))
		ds.PowerTime = int64(getFloat(device, "powerTime"))

		// MAC address (authoritative identifier) - normalize to lowercase
		if mac := getString(device, "mac"); mac != "" {
			ds.MAC = strings.ToLower(mac)
		}

		// Extract hostname/name from device block
		if hostname := getString(device, "hostname"); hostname != "" {
			ds.Hostname = hostname
		} else if name := getString(device, "name"); name != "" {
			ds.Hostname = name
		}

		// RAM
		if ram, ok := device["ram"].(map[string]any); ok {
			ds.RAM.Total = int64(getFloat(ram, "total"))
			ds.RAM.Free = int64(getFloat(ram, "free"))
			ds.RAM.Usage = int(getFloat(ram, "usage"))
		}

		// CPU
		if cpus, ok := device["cpu"].([]any); ok {
			for _, c := range cpus {
				if cm, ok := c.(map[string]any); ok {
					ds.CPU = append(ds.CPU, stats.CPUCore{
						ID:    getString(cm, "identifier"),
						Usage: int(getFloat(cm, "usage")),
					})
				}
			}
		}

		// Temperatures
		if temps, ok := device["temperatures"].([]any); ok {
			for _, t := range temps {
				if tm, ok := t.(map[string]any); ok {
					name := getString(tm, "name")
					val := getFloat(tm, "value")
					switch name {
					case "cpu":
						ds.Temperature.CPU = val
					case "main":
						ds.Temperature.Radio60 = val
					case "backup":
						ds.Temperature.Radio5 = val
					case "board":
						ds.Temperature.Board = val
					}
				}
			}
		}

		// GPS - check inside device block
		if gps, ok := device["gps"].(map[string]any); ok {
			ds.GPS.Fix = getBool(gps, "fix")
			ds.GPS.Lat = getFloat(gps, "lat")
			ds.GPS.Lon = getFloat(gps, "lon")
			ds.GPS.Alt = getFloat(gps, "alt")
			ds.GPS.Sats = int(getFloat(gps, "sats"))
		}

		// Orientation - check inside device block
		if orient, ok := device["orientation"].(map[string]any); ok {
			ds.Orientation = &stats.OrientationStats{
				Tilt:   getFloat(orient, "tilt"),
				Roll:   getFloat(orient, "roll"),
				Tilt24: getFloat(orient, "tilt24"),
				Roll24: getFloat(orient, "roll24"),
			}
		}
	}

	// GPS fallback: check root level if not found in device block
	if ds.GPS.Lat == 0 && ds.GPS.Lon == 0 {
		if gps, ok := r["gps"].(map[string]any); ok {
			ds.GPS.Fix = getBool(gps, "fix")
			ds.GPS.Lat = getFloat(gps, "lat")
			ds.GPS.Lon = getFloat(gps, "lon")
			ds.GPS.Alt = getFloat(gps, "alt")
			if alt := getFloat(gps, "altitude"); alt != 0 {
				ds.GPS.Alt = alt
			}
			ds.GPS.Sats = int(getFloat(gps, "sats"))
			if sats := int(getFloat(gps, "satellites")); sats != 0 {
				ds.GPS.Sats = sats
			}
		}
	}

	// Orientation fallback: check root level if not found in device block
	if ds.Orientation == nil {
		if orient, ok := r["orientation"].(map[string]any); ok {
			ds.Orientation = &stats.OrientationStats{
				Tilt:   getFloat(orient, "tilt"),
				Roll:   getFloat(orient, "roll"),
				Tilt24: getFloat(orient, "tilt24"),
				Roll24: getFloat(orient, "roll24"),
			}
		}
	}

	// Identification fallback: some Wave/LTU stats responses provide MAC/hostname under root identification
	if ds.MAC == "" || ds.Hostname == "" {
		if ident, ok := r["identification"].(map[string]any); ok {
			if ds.MAC == "" {
				if mac := getString(ident, "mac"); mac != "" {
					ds.MAC = strings.ToLower(mac)
				}
			}
			if ds.Hostname == "" {
				if hn := getString(ident, "hostname"); hn != "" {
					ds.Hostname = hn
				} else if hn := getString(ident, "name"); hn != "" {
					ds.Hostname = hn
				}
			}
		}
	}

	// Wireless stats
	var peers []*stats.PeerStats
	if wireless, ok := r["wireless"].(map[string]any); ok {
		radioSlotByID := map[string]string{}
		ds.Wireless.ServiceUptime = int64(getFloat(wireless, "serviceUptime"))
		ds.Wireless.ServiceDowntime = int64(getFloat(wireless, "serviceDowntime"))

		// Device-level signal (LTU STA reports signal at wireless root)
		if sig := int(getFloat(wireless, "signal")); sig != 0 {
			// Create RadioLTU if it doesn't exist
			if ds.Wireless.RadioLTU == nil && platform == "ltu" {
				ds.Wireless.RadioLTU = &stats.RadioStats{
					ID:        "main",
					LinkState: "connected",
				}
			}
			radio := ds.Wireless.RadioLTU
			if radio == nil {
				radio = ds.Wireless.Radio5GHz
			}
			if radio != nil {
				radio.Signal = sig
				radio.NoiseFloor = int(getFloat(wireless, "noise"))

				// Per-chain signals
				if chains, ok := wireless["signalPerChain"].([]any); ok {
					for _, c := range chains {
						if cv, ok := c.(float64); ok {
							radio.SignalPerChain = append(radio.SignalPerChain, int(cv))
						}
					}
				}

				// Compute MRC combined signal (Go > JS)
				if len(radio.SignalPerChain) > 1 {
					radio.SignalCombined = stats.CombineSignals(radio.SignalPerChain)
				} else {
					radio.SignalCombined = sig
				}
				radio.SignalQuality = stats.SignalQuality5GHz(radio.SignalCombined)
			}
		}

		// Link quality
		if lq, ok := wireless["linkQuality"].(map[string]any); ok {
			if counters, ok := lq["counters"].(map[string]any); ok {
				ds.Wireless.TxRate = int64(getFloat(counters, "txRate"))
				ds.Wireless.RxRate = int64(getFloat(counters, "rxRate"))
			}
			if ls, ok := lq["linkScore"].(map[string]any); ok {
				ds.Wireless.LinkScore = parseLinkScore(ls)
			}
		}

		// Radios
		if radios, ok := wireless["radios"].([]any); ok {
			// Some Wave devices (e.g. Wave MLO5) can have *two* 5GHz radios.
			// Our stats model has a single dedicated 5GHz slot, so we keep a
			// candidate for a second 5GHz radio and, if no true 6GHz radio exists,
			// we store that second 5GHz radio into the Radio6GHz slot with an
			// explicit display-band override.
			var second5Candidate *stats.RadioStats
			var second5ID string
			for _, r := range radios {
				rm, ok := r.(map[string]any)
				if !ok {
					continue
				}
				radio := parseRadioStats(rm)
				id := getString(rm, "id")

				// Keep a flat list of radios for debugging / UI.
				ds.Wireless.Radios = append(ds.Wireless.Radios, *radio)

				// Assign based on platform and inferred band.
				if platform == "ltu" {
					// LTU: main = 5 GHz, backup = 60 GHz
					switch id {
					case "main":
						if ds.Wireless.RadioLTU == nil {
							ds.Wireless.RadioLTU = radio
						}
						radioSlotByID[id] = "ltu"
					case "backup":
						if ds.Wireless.Radio60GHz == nil {
							ds.Wireless.Radio60GHz = radio
						}
						radioSlotByID[id] = "60"
					}
					continue
				}

				// When disabled, keep the legacy Wave mapping (main=60GHz, backup=5GHz).
				// This preserves historical behavior for Wave LR/Pro deployments until
				// MLO multi-radio parsing is enabled.
				if !p.cfgSnapshot().waveMLOMultiRadio {
					switch strings.ToLower(id) {
					case "main":
						if ds.Wireless.Radio60GHz == nil {
							ds.Wireless.Radio60GHz = radio
						}
						if id != "" {
							radioSlotByID[id] = "60"
						}
					case "backup":
						if ds.Wireless.Radio5GHz == nil {
							ds.Wireless.Radio5GHz = radio
						}
						if id != "" {
							radioSlotByID[id] = "5"
						}
					}
					continue
				}

				band := inferWaveBand(rm, radio)
				recordWaveBand(band)
				switch band {
				case waveBand60GHz:
					if ds.Wireless.Radio60GHz == nil {
						ds.Wireless.Radio60GHz = radio
					}
					if id != "" {
						radioSlotByID[id] = "60"
					}
				case waveBand6GHz:
					if ds.Wireless.Radio6GHz == nil {
						ds.Wireless.Radio6GHz = radio
					}
					if id != "" {
						radioSlotByID[id] = "6"
					}
				case waveBand5GHz:
					if ds.Wireless.Radio5GHz == nil {
						ds.Wireless.Radio5GHz = radio
						if id != "" {
							radioSlotByID[id] = "5"
						}
					} else if second5Candidate == nil {
						second5Candidate = radio
						second5ID = id
						if id != "" {
							radioSlotByID[id] = "second5"
						}
					}
				default:
					// Legacy fallback when we can't infer band (e.g. missing frequency on older firmwares).
					// Historically Wave devices used: main = 60 GHz, backup = 5 GHz.
					switch id {
					case "main":
						if ds.Wireless.Radio60GHz == nil {
							ds.Wireless.Radio60GHz = radio
						}
						if id != "" {
							radioSlotByID[id] = "60"
						}
					case "backup":
						if ds.Wireless.Radio5GHz == nil {
							ds.Wireless.Radio5GHz = radio
						}
						if id != "" {
							radioSlotByID[id] = "5"
						}
					}
				}

			}

			// MLO5: if we saw a second 5GHz radio and there is no true 6GHz
			// radio, store the second 5GHz radio into the Radio6GHz slot with
			// an explicit display label so the UI can render it as "5 GHz #2".
			if ds.Wireless.Radio6GHz == nil && second5Candidate != nil {
				second5Candidate.DisplayBandOverride = "5 GHz #2"
				ds.Wireless.Radio6GHz = second5Candidate
				if second5ID != "" {
					radioSlotByID[second5ID] = "6"
				}
			} else if second5Candidate != nil && second5ID != "" {
				// If there *is* a true 6GHz radio, we don't have a dedicated slot for a
				// second 5GHz radio. Route it into the 5GHz slot so we don't lose peer data.
				if radioSlotByID[second5ID] == "second5" {
					radioSlotByID[second5ID] = "5"
				}
			}
		}

		// Peers
		if peersRaw, ok := wireless["peers"].([]any); ok {
			for _, pr := range peersRaw {
				pm, ok := pr.(map[string]any)
				if !ok {
					continue
				}
				peer := p.parsePeer(pm, platform, radioSlotByID)
				if peer != nil {
					// Use the stats timestamp (when available) to keep peers aligned
					// with the sample time.
					peers = append(peers, peer)
				}
			}
		}
	}

	// AirView (spectrum utilization)
	if av, ok := r["airview"].(map[string]any); ok {
		if utilRaw, ok := av["utilization"].([]any); ok {
			ds.Wireless.AirViewUtilization = parseAirViewUtilization(utilRaw)
		}
	}

	// Interfaces
	if ifaces, ok := r["interfaces"].([]any); ok {
		for _, i := range ifaces {
			im, ok := i.(map[string]any)
			if !ok {
				continue
			}
			iface := stats.InterfaceStats{
				ID:   getString(im, "id"),
				Name: getString(im, "name"),
			}
			if status, ok := im["status"].(map[string]any); ok {
				iface.Type = getString(status, "type")
				iface.Enabled = getBool(status, "enabled")
				iface.Plugged = getBool(status, "plugged")
				iface.Description = getString(status, "description")
				iface.ConfiguredSpeed = getString(status, "speed")
				iface.CurrentSpeed = getString(status, "currentSpeed")

				// Derive a simple up/down status when not explicitly provided
				if iface.Status == "" {
					if iface.Plugged {
						iface.Status = "up"
					} else if iface.Enabled {
						iface.Status = "down"
					}
				}

				// If type is missing, infer it from the interface ID/name
				if iface.Type == "" {
					name := strings.ToLower(iface.Name)
					id := strings.ToLower(iface.ID)
					switch {
					case strings.HasPrefix(name, "eth") || strings.HasPrefix(id, "lan/") || strings.HasPrefix(id, "eth") || strings.HasPrefix(id, "br"):
						iface.Type = "ethernet"
					case strings.HasPrefix(name, "wifi") || id == "main" || id == "backup":
						iface.Type = "wireless"
					}
				}
			}
			if st, ok := im["statistics"].(map[string]any); ok {
				iface.TxBytes = int64(getFloat(st, "txBytes"))
				iface.RxBytes = int64(getFloat(st, "rxBytes"))
				iface.TxRate = int64(getFloat(st, "txRate"))
				iface.RxRate = int64(getFloat(st, "rxRate"))
				iface.TxPackets = int64(getFloat(st, "txPackets"))
				iface.RxPackets = int64(getFloat(st, "rxPackets"))
				iface.TxErrors = int64(getFloat(st, "txErrors"))
				iface.RxErrors = int64(getFloat(st, "rxErrors"))
			}
			ds.Interfaces = append(ds.Interfaces, iface)
		}
	}

	return ds, peers
}

func (p *Poller) parsePeer(pm map[string]any, platform string, radioSlotByID map[string]string) *stats.PeerStats {
	common, ok := pm["common"].(map[string]any)
	if !ok {
		return nil
	}

	peer := &stats.PeerStats{
		IP:       getString(common, "mgmtIp"),
		Hostname: getString(common, "hostname"),
		Distance: getFloat(common, "distance"),
		Uptime:   int64(getFloat(common, "uptime")),
	}

	if ident, ok := common["identification"].(map[string]any); ok {
		peer.MAC = strings.ToLower(getString(ident, "mac"))
		peer.Model = getString(ident, "model")
		// Preserve the raw firmware string (often includes flavor/platform) for later detection,
		// but prefer firmwareVersion for display/storage.
		peer.FirmwareFull = getString(ident, "firmware")
		if fwVersion := getString(ident, "firmwareVersion"); fwVersion != "" {
			peer.Firmware = fwVersion
		} else {
			peer.Firmware = peer.FirmwareFull
		}
	}

	if counters, ok := common["counters"].(map[string]any); ok {
		peer.TxBytes = int64(getFloat(counters, "txBytes"))
		peer.RxBytes = int64(getFloat(counters, "rxBytes"))
		peer.TxRate = int64(getFloat(counters, "txRate"))
		peer.RxRate = int64(getFloat(counters, "rxRate"))
		peer.TxPackets = int64(getFloat(counters, "txPackets"))
		peer.RxPackets = int64(getFloat(counters, "rxPackets"))
	}

	if ts, ok := common["trafficShaping"].(map[string]any); ok {
		peer.DLRateLimit = int64(getFloat(ts, "dlRate") * 1000) // kbps to bps
		peer.ULRateLimit = int64(getFloat(ts, "ulRate") * 1000)
	}

	if ls, ok := common["linkQuality"].(map[string]any); ok {
		if lsm, ok := ls["linkScore"].(map[string]any); ok {
			peer.LinkScore = parseLinkScore(lsm)
		}
	}

	// GPS from peer device
	if gps, ok := common["gps"].(map[string]any); ok {
		peer.GPS.Fix = getBool(gps, "fix")
		peer.GPS.Lat = getFloat(gps, "lat")
		peer.GPS.Lon = getFloat(gps, "lon")
		peer.GPS.Alt = getFloat(gps, "alt")
		peer.GPS.Sats = int(getFloat(gps, "sats"))
	}

	// Orientation
	if orient, ok := common["orientation"].(map[string]any); ok {
		peer.Orientation = &stats.OrientationStats{
			Tilt:   getFloat(orient, "tilt"),
			Roll:   getFloat(orient, "roll"),
			Tilt24: getFloat(orient, "tilt24"),
			Roll24: getFloat(orient, "roll24"),
		}
	}

	// Network mode
	peer.NetMode = getString(common, "netMode")

	// Power and service times
	peer.PowerTime = int64(getFloat(common, "powerTime"))
	peer.ServiceUptime = int64(getFloat(common, "serviceUptime"))

	// Carrier drop
	if cd, ok := common["carrierDrop"].(map[string]any); ok {
		peer.CarrierDrop = getBool(cd, "dropped")
	}

	// CPU from peer
	if cpus, ok := common["cpu"].([]any); ok {
		for _, c := range cpus {
			if cm, ok := c.(map[string]any); ok {
				peer.CPU = append(peer.CPU, stats.CPUCore{
					ID:    getString(cm, "identifier"),
					Usage: int(getFloat(cm, "usage")),
				})
			}
		}
	}

	// RAM from peer
	if ram, ok := common["ram"].(map[string]any); ok {
		peer.RAM = stats.RAMStats{
			Total: int64(getFloat(ram, "total")),
			Free:  int64(getFloat(ram, "free")),
			Usage: int(getFloat(ram, "usage")),
		}
	}

	// Interfaces from peer
	if ifaces, ok := common["interfaces"].([]any); ok {
		for _, i := range ifaces {
			im, ok := i.(map[string]any)
			if !ok {
				continue
			}
			iface := stats.InterfaceStats{
				ID:      getString(im, "id"),
				Name:    getString(im, "name"),
				Enabled: true,
			}
			if status, ok := im["status"].(map[string]any); ok {
				iface.Type = getString(status, "type")
				iface.Enabled = getBool(status, "enabled")
				iface.Plugged = getBool(status, "plugged")
				iface.Description = getString(status, "description")
				iface.ConfiguredSpeed = getString(status, "speed")
				iface.CurrentSpeed = getString(status, "currentSpeed")

				// Derive a simple up/down status when not explicitly provided
				if iface.Status == "" {
					if iface.Plugged {
						iface.Status = "up"
					} else if iface.Enabled {
						iface.Status = "down"
					}
				}

				// If type is missing, infer it from the interface ID/name
				if iface.Type == "" {
					name := strings.ToLower(iface.Name)
					id := strings.ToLower(iface.ID)
					switch {
					case strings.HasPrefix(name, "eth") || strings.HasPrefix(id, "lan/") || strings.HasPrefix(id, "eth") || strings.HasPrefix(id, "br"):
						iface.Type = "ethernet"
					case strings.HasPrefix(name, "wifi") || id == "main" || id == "backup":
						iface.Type = "wireless"
					}
				}
			}
			if st, ok := im["statistics"].(map[string]any); ok {
				iface.TxBytes = int64(getFloat(st, "txBytes"))
				iface.RxBytes = int64(getFloat(st, "rxBytes"))
				iface.TxRate = int64(getFloat(st, "txRate"))
				iface.RxRate = int64(getFloat(st, "rxRate"))
				iface.TxPackets = int64(getFloat(st, "txPackets"))
				iface.RxPackets = int64(getFloat(st, "rxPackets"))
				iface.TxErrors = int64(getFloat(st, "txErrors"))
				iface.RxErrors = int64(getFloat(st, "rxErrors"))
			}
			peer.Interfaces = append(peer.Interfaces, iface)
		}
	}

	// Local radio stats (AP's RX - what AP receives from this STA)
	if local, ok := pm["local"].([]any); ok {
		for _, l := range local {
			lm, ok := l.(map[string]any)
			if !ok {
				continue
			}
			radioStats := parsePeerRadioStats(lm)
			id := getString(lm, "id")

			slot := ""
			if radioSlotByID != nil {
				slot = radioSlotByID[id]
			}
			if slot == "" {
				// Fallback to historical behaviour when we don't have a device-level mapping.
				if platform == "ltu" {
					if id == "main" {
						slot = "ltu"
					} else if id == "backup" {
						slot = "60"
					}
				} else {
					if id == "main" {
						slot = "60"
					} else if id == "backup" {
						slot = "5"
					}
				}
			}
			if slot == "second5" {
				slot = "5"
			}

			switch slot {
			case "ltu":
				peer.RadioLTU = radioStats
			case "60":
				peer.Radio60GHz = radioStats
			case "6":
				peer.Radio6GHz = radioStats
			case "5":
				peer.Radio5GHz = radioStats
			}
		}
	}

	// Remote radio stats (STA's RX - what STA receives from AP)
	if remote, ok := pm["remote"].([]any); ok {
		for _, r := range remote {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			id := getString(rm, "id")

			// Parse remote signal from linkQuality
			var remoteSignal, remoteIdealSignal, remoteNoiseFloor int
			var remoteChains []int
			if lq, ok := rm["linkQuality"].(map[string]any); ok {
				remoteSignal = int(getFloat(lq, "signal"))
				remoteIdealSignal = int(getFloat(lq, "idealSignal"))
				remoteNoiseFloor = int(getFloat(lq, "noiseFloor"))
				if chains, ok := lq["signalPerChain"].([]any); ok {
					for _, c := range chains {
						if cv, ok := c.(float64); ok {
							remoteChains = append(remoteChains, int(cv))
						}
					}
				}
			}

			// Pre-compute combined signal (done here in Go, not JS)
			remoteCombined := remoteSignal
			if len(remoteChains) > 1 {
				remoteCombined = stats.CombineSignals(remoteChains)
			}

			// Estimate remote SNR (STA RX - what the STA receives from the AP).
			remoteSNR := 0
			if remoteNoiseFloor != 0 {
				sig := remoteCombined
				if sig == 0 {
					sig = remoteSignal
				}
				if sig != 0 {
					remoteSNR = sig - remoteNoiseFloor
				}
			}

			// Store in appropriate radio stats. MLO devices break the historical
			// assumption that "main" is always the 60GHz radio and "backup" is always
			// 5GHz.
			slot := ""
			if radioSlotByID != nil {
				slot = radioSlotByID[id]
			}
			if slot == "" {
				// Fallback to legacy behavior if we can't determine the mapping.
				if platform == "ltu" {
					if id == "main" {
						slot = "ltu"
					} else if id == "backup" {
						slot = "60"
					}
				} else {
					if id == "main" {
						slot = "60"
					} else if id == "backup" {
						slot = "5"
					}
				}
			}
			if slot == "second5" {
				slot = "5"
			}

			// Choose quality curve based on band.
			quality := stats.SignalQuality5GHz(remoteCombined)
			if slot == "60" {
				quality = stats.SignalQuality60GHz(remoteCombined)
			}

			var pr *stats.PeerRadioStats
			switch slot {
			case "ltu":
				pr = peer.RadioLTU
			case "60":
				pr = peer.Radio60GHz
			case "6":
				pr = peer.Radio6GHz
			case "5":
				pr = peer.Radio5GHz
			}
			if pr != nil {
				pr.RemoteSignal = remoteSignal
				pr.RemoteSignalCombined = remoteCombined
				pr.RemoteSignalQuality = quality
				pr.RemoteIdealSignal = remoteIdealSignal
				pr.RemoteNoiseFloor = remoteNoiseFloor
				pr.RemoteSNR = remoteSNR
				pr.RemoteSignalPerChain = remoteChains
			}

			// Legacy fields for backward compatibility: keep using the peer's "main"
			// radio as the default summary.
			if id == "main" {
				peer.RemoteSignal = remoteSignal
				peer.RemoteNoiseFloor = remoteNoiseFloor
				peer.RemoteSignalPerChain = remoteChains
			}
		}
	}

	return peer
}

func parsePeerRadioStats(lm map[string]any) *stats.PeerRadioStats {
	rs := &stats.PeerRadioStats{
		ID:             getString(lm, "id"),
		Active:         getBool(lm, "active"),
		Connected:      getBool(lm, "connected"),
		LinkState:      getString(lm, "linkState"),
		ConnectionTime: int64(getFloat(lm, "connectionTime")),
	}

	if lq, ok := lm["linkQuality"].(map[string]any); ok {
		rs.Signal = int(getFloat(lq, "signal"))
		rs.SignalDay = int(getFloat(lq, "signalDay"))
		rs.IdealSignal = int(getFloat(lq, "idealSignal"))
		rs.NoiseFloor = int(getFloat(lq, "noiseFloor"))

		// Per-chain signals
		if chains, ok := lq["signalPerChain"].([]any); ok {
			for _, c := range chains {
				if cv, ok := c.(float64); ok {
					rs.SignalPerChain = append(rs.SignalPerChain, int(cv))
				}
			}
		}

		// Signal histogram
		if sh, ok := lq["signalHistogram"].(map[string]any); ok {
			hist := &stats.SignalHistogram{
				MinSignal: int(getFloat(sh, "minSignal")),
				MaxSignal: int(getFloat(sh, "maxSignal")),
				Period:    int(getFloat(sh, "period")),
			}
			if histArr, ok := sh["histogram"].([]any); ok {
				for _, v := range histArr {
					if fv, ok := v.(float64); ok {
						hist.Histogram = append(hist.Histogram, int(fv))
					}
				}
			}
			if len(hist.Histogram) > 0 {
				rs.SignalHistogram = hist
			}
		}

		// Calculate SNR
		// SNR is computed after we derive SignalCombined.

		// CINR (LTU-specific)
		if cinr, ok := lq["cinr"].(map[string]any); ok {
			rs.CINR = &stats.CINRStats{
				DL: int(getFloat(cinr, "dl")),
				UL: int(getFloat(cinr, "ul")),
			}
		}

		// MCS
		if mcs, ok := lq["mcs"].(map[string]any); ok {
			rs.MCS = &stats.MCSStats{
				TxIdx:      int(getFloat(mcs, "txIdx")),
				RxIdx:      int(getFloat(mcs, "rxIdx")),
				TxLabel:    getString(mcs, "txLabel"),
				RxLabel:    getString(mcs, "rxLabel"),
				TxRate:     int(getFloat(mcs, "txRate")),
				RxRate:     int(getFloat(mcs, "rxRate")),
				TxIdxIdeal: int(getFloat(mcs, "txIdxIdeal")),
				RxIdxIdeal: int(getFloat(mcs, "rxIdxIdeal")),
			}
		}

		// Airtime
		if airtime, ok := lq["airtime"].(map[string]any); ok {
			rs.AirtimeDL = getFloat(airtime, "dl")
			rs.AirtimeUL = getFloat(airtime, "ul")
		}

		// Capacity
		if cap, ok := lq["capacity"].(map[string]any); ok {
			rs.Capacity = parseCapacity(cap)
		}

		// Link score
		if ls, ok := lq["linkScore"].(map[string]any); ok {
			rs.LinkScore = parseLinkScore(ls)
		}
	}

	// Compute combined signal from per-chain values (done here in Go, not JS)
	if len(rs.SignalPerChain) > 1 {
		rs.SignalCombined = stats.CombineSignals(rs.SignalPerChain)
	} else if rs.Signal != 0 {
		rs.SignalCombined = rs.Signal
	}

	// Set signal quality based on combined signal
	if rs.SignalCombined != 0 {
		rs.SignalQuality = stats.SignalQuality5GHz(rs.SignalCombined)
	}

	// Estimate SNR (dB) when noise floor is available.
	//
	// Note: on Wave, the reported noise floor is a long-term average, so treat this
	// as an estimate rather than an instantaneous SNR.
	if rs.NoiseFloor != 0 {
		sig := rs.SignalCombined
		if sig == 0 {
			sig = rs.Signal
		}
		if sig != 0 {
			rs.SNR = sig - rs.NoiseFloor
		}
	}

	return rs
}

func parseRadioStats(rm map[string]any) *stats.RadioStats {
	rs := &stats.RadioStats{
		ID:        getString(rm, "id"),
		LinkState: getString(rm, "linkState"),
	}

	// Frequency - check both formats
	if freq, ok := rm["frequency"].(map[string]any); ok {
		// LTU/Wave format: { tx: 5620, rx: 5620 }
		rs.Frequency = int(getFloat(freq, "tx"))
		if rs.Frequency == 0 {
			rs.Frequency = int(getFloat(freq, "center"))
		}
	}
	if cw, ok := rm["channelWidth"].(map[string]any); ok {
		rs.ChannelWidth = int(getFloat(cw, "tx"))
	}
	if op, ok := rm["outputPower"].(map[string]any); ok {
		rs.TxPower = int(getFloat(op, "conducted"))
		rs.TxPowerEIRP = int(getFloat(op, "eirp"))
	}
	if ant, ok := rm["antenna"].(map[string]any); ok {
		rs.Antenna.Name = getString(ant, "name")
		rs.Antenna.Gain = int(getFloat(ant, "gain"))
	}

	// NoiseFloor - at root level for LTU APs
	if nf := int(getFloat(rm, "noiseFloor")); nf != 0 {
		rs.NoiseFloor = nf
	}

	// DL ratio and frame length (LTU AP specific)
	rs.DLRatio = int(getFloat(rm, "dlRatio"))
	rs.FrameLength = getFloat(rm, "frameLength")
	rs.RxEfficiency = getFloat(rm, "rxEfficiency")

	if lq, ok := rm["linkQuality"].(map[string]any); ok {
		if cap, ok := lq["capacity"].(map[string]any); ok {
			rs.Capacity = parseCapacity(cap)
		}
		// NoiseFloor can also be in linkQuality
		if nf := int(getFloat(lq, "noiseFloor")); nf != 0 && rs.NoiseFloor == 0 {
			rs.NoiseFloor = nf
		}
	}
	if cu, ok := rm["channelUtilization"].(map[string]any); ok {
		// Wave reports channel utilization (airtime) as percentages. Some firmware versions
		// place DL/UL/interference under "common", while others place DL/UL at the root and
		// interference under "common".
		dl := 0.0
		ul := 0.0
		interf := 0.0

		if common, ok := cu["common"].(map[string]any); ok {
			if _, ok := common["dl"]; ok {
				dl = getFloat(common, "dl")
			} else {
				dl = getFloat(cu, "dl")
			}
			if _, ok := common["ul"]; ok {
				ul = getFloat(common, "ul")
			} else {
				ul = getFloat(cu, "ul")
			}
			if _, ok := common["interference"]; ok {
				interf = getFloat(common, "interference")
			} else {
				interf = getFloat(cu, "interference")
			}
		} else {
			dl = getFloat(cu, "dl")
			ul = getFloat(cu, "ul")
			interf = getFloat(cu, "interference")
		}

		rs.Utilization = &stats.Utilization{
			DL:           dl,
			UL:           ul,
			Interference: interf,
		}
	}
	rs.GPSSyncState = int(getFloat(rm, "gpsSyncState"))

	if dfs, ok := rm["dfs"].(map[string]any); ok {
		rs.DFS = &stats.DFSStats{
			Enabled:      getBool(dfs, "enabled"),
			CACDuration:  int(getFloat(dfs, "cacDuration")),
			CACRemaining: int(getFloat(dfs, "cacRemaining")),
		}
	}

	if afc, ok := rm["afc"].(map[string]any); ok {
		rs.AFC = parseAFCStats(afc)
	}

	return rs
}

func parseCapacity(cap map[string]any) *stats.CapacityStats {
	cs := &stats.CapacityStats{}

	// API returns capacity in kbps, convert to bps
	// Try dl/ul first (LTU/Wave format), then tx/rx (alternate format)
	if dl := getFloat(cap, "dl"); dl != 0 {
		cs.DL = int64(dl * 1000)
	} else {
		cs.DL = int64(getFloat(cap, "tx") * 1000)
	}

	if ul := getFloat(cap, "ul"); ul != 0 {
		cs.UL = int64(ul * 1000)
	} else {
		cs.UL = int64(getFloat(cap, "rx") * 1000)
	}

	cs.Combined = int64(getFloat(cap, "combined") * 1000)

	// Ideal values
	if dlIdeal := getFloat(cap, "dlIdeal"); dlIdeal != 0 {
		cs.DLIdeal = int64(dlIdeal * 1000)
	} else {
		cs.DLIdeal = int64(getFloat(cap, "txIdeal") * 1000)
	}

	if ulIdeal := getFloat(cap, "ulIdeal"); ulIdeal != 0 {
		cs.ULIdeal = int64(ulIdeal * 1000)
	} else {
		cs.ULIdeal = int64(getFloat(cap, "rxIdeal") * 1000)
	}

	cs.CombinedIdeal = int64(getFloat(cap, "combinedIdeal") * 1000)

	return cs
}

func parseLinkScore(ls map[string]any) *stats.LinkScore {
	return &stats.LinkScore{
		DL:   int(getFloat(ls, "dl")),
		UL:   int(getFloat(ls, "ul")),
		DL2:  int(getFloat(ls, "dl2")),
		UL2:  int(getFloat(ls, "ul2")),
		DL24: int(getFloat(ls, "dl24")),
		UL24: int(getFloat(ls, "ul24")),
	}
}

func parseAFCStats(am map[string]any) *stats.AFCStats {
	afc := &stats.AFCStats{
		Label:    getString(am, "label"),
		Status:   getString(am, "status"),
		Type:     getString(am, "type"),
		Detail:   getString(am, "detail"),
		Reason:   getString(am, "reason"),
		ExpiryMs: int64(getFloat(am, "expiry")),
	}

	if regs, ok := am["regulatory"].([]any); ok {
		for _, r := range regs {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			reg := stats.AFCRegulatory{
				ChannelWidthMHz: int64(getFloat(rm, "channelWidth")),
			}

			if chs, ok := rm["channels"].([]any); ok {
				for _, c := range chs {
					cm, ok := c.(map[string]any)
					if !ok {
						continue
					}
					reg.Channels = append(reg.Channels, stats.AFCChannel{
						CenterMHz:  int64(getFloat(cm, "center")),
						MaxEIRPDbm: int64(getFloat(cm, "maxEirp")),
					})
				}
			}

			if frs, ok := rm["freqRanges"].([]any); ok {
				for _, fr := range frs {
					fm, ok := fr.(map[string]any)
					if !ok {
						continue
					}
					reg.FreqRanges = append(reg.FreqRanges, stats.AFCFreqRange{
						StartMHz: int64(getFloat(fm, "start")),
						EndMHz:   int64(getFloat(fm, "end")),
					})
				}
			}

			afc.Regulatory = append(afc.Regulatory, reg)
		}
	}

	// If we have no meaningful data, return nil to avoid UI clutter.
	if afc.Label == "" && afc.Status == "" && afc.Type == "" && afc.ExpiryMs == 0 && len(afc.Regulatory) == 0 {
		return nil
	}
	return afc
}

func parseAirViewUtilization(raw []any) []stats.AirViewUtilizationPoint {
	const maxAirViewUtilizationPoints = 512

	capHint := len(raw)
	if capHint > maxAirViewUtilizationPoints {
		capHint = maxAirViewUtilizationPoints
	}

	out := make([]stats.AirViewUtilizationPoint, 0, capHint)
	for _, r := range raw {
		if len(out) >= maxAirViewUtilizationPoints {
			break
		}
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		freq := int64(getFloat(m, "frequency"))
		usage := int64(getFloat(m, "usage"))
		if freq == 0 {
			continue
		}
		out = append(out, stats.AirViewUtilizationPoint{
			FrequencyMHz: freq,
			UsagePct:     usage,
		})
	}
	return out
}
