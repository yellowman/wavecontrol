package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yellowman/wavecontrol/internal/stats"
)

const reportSchemaVersion = 2

var reportTypeLabels = map[string]string{
	"health":      "Network Health",
	"inventory":   "Device Inventory",
	"performance": "Performance Summary",
	"chain":       "Chain Imbalance",
	"rx_mismatch": "RX Level Mismatch",
}

func normalizeReportType(value string) (string, bool) {
	reportType := strings.ToLower(strings.TrimSpace(value))
	if reportType == "" {
		reportType = "health"
	}
	_, ok := reportTypeLabels[reportType]
	return reportType, ok
}

type reportInventoryDevice struct {
	ID             int
	Hostname       string
	IP             string
	MAC            string
	Product        string
	Firmware       string
	Flavor         string
	Status         string
	Platform       string
	Region         string
	Site           string
	ParentID       int
	ParentHostname string
	ParentIP       string
	LastSeen       time.Time
	HasLastSeen    bool
}

func (d reportInventoryDevice) IsSTA() bool { return d.ParentID > 0 }

func (d reportInventoryDevice) DisplayName() string {
	if strings.TrimSpace(d.Hostname) != "" {
		return d.Hostname
	}
	if strings.TrimSpace(d.IP) != "" {
		return d.IP
	}
	return d.MAC
}

func normalizeReportStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "online":
		return "online"
	case "offline":
		return "offline"
	default:
		return "unknown"
	}
}

func (a *API) loadReportInventory(ctx context.Context) ([]reportInventoryDevice, error) {
	rows, err := a.DB.QueryContext(ctx, `
		SELECT d.id, d.hostname, host(d.ip_address), d.mac, d.product, d.firmware,
		       d.flavor, d.status, d.platform, d.parent_id,
		       p.hostname, host(p.ip_address), r.name, s.name, d.last_seen
		FROM devices d
		LEFT JOIN devices p ON d.parent_id = p.id
		LEFT JOIN sites s ON d.site_id = s.id
		LEFT JOIN regions r ON s.region_id = r.id
		ORDER BY COALESCE(r.name, ''), COALESCE(s.name, ''), COALESCE(d.hostname, ''), host(d.ip_address)
	`)
	if err != nil {
		return nil, fmt.Errorf("inventory query failed: %w", err)
	}
	defer rows.Close()

	devices := make([]reportInventoryDevice, 0)
	for rows.Next() {
		var d reportInventoryDevice
		var hostname, product, firmware, flavor, status, platform sql.NullString
		var parentID sql.NullInt64
		var parentHostname, parentIP, region, site sql.NullString
		var lastSeen sql.NullTime
		if err := rows.Scan(
			&d.ID, &hostname, &d.IP, &d.MAC, &product, &firmware,
			&flavor, &status, &platform, &parentID,
			&parentHostname, &parentIP, &region, &site, &lastSeen,
		); err != nil {
			return nil, fmt.Errorf("inventory row scan failed: %w", err)
		}
		d.Hostname = hostname.String
		d.Product = product.String
		d.Firmware = firmware.String
		d.Flavor = flavor.String
		d.Status = normalizeReportStatus(status.String)
		d.Platform = platform.String
		d.Region = region.String
		d.Site = site.String
		if parentID.Valid {
			d.ParentID = int(parentID.Int64)
			d.ParentHostname = parentHostname.String
			d.ParentIP = parentIP.String
		}
		if lastSeen.Valid {
			d.LastSeen = lastSeen.Time
			d.HasLastSeen = true
		}
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inventory iteration failed: %w", err)
	}
	return devices, nil
}

func reportStatsByMAC(store *stats.Store) map[string]*stats.DeviceStats {
	result := make(map[string]*stats.DeviceStats)
	if store == nil {
		return result
	}
	for _, snapshot := range store.List() {
		if snapshot == nil || strings.TrimSpace(snapshot.MAC) == "" {
			continue
		}
		result[strings.ToLower(strings.TrimSpace(snapshot.MAC))] = snapshot
	}
	return result
}

type reportRadioMetric struct {
	Band      string
	Signal    int
	Capacity  int64
	HasSignal bool
}

func reportRadioSignal(r *stats.RadioStats) int {
	if r == nil {
		return 0
	}
	if r.SignalCombined != 0 {
		return r.SignalCombined
	}
	if r.Signal != 0 {
		return r.Signal
	}
	if len(r.SignalPerChain) > 0 {
		return stats.CombineSignals(r.SignalPerChain)
	}
	if r.RSSI < 0 {
		return r.RSSI
	}
	return 0
}

func validReportSignal(signal int) bool {
	return signal < 0 && signal > -120
}

func reportPrimaryRadio(w stats.WirelessStats) reportRadioMetric {
	type candidate struct {
		label string
		radio *stats.RadioStats
	}
	candidates := []candidate{
		{label: "60 GHz", radio: w.Radio60GHz},
		{label: "6 GHz", radio: w.Radio6GHz},
		{label: "5 GHz", radio: w.Radio5GHz},
		{label: "LTU", radio: w.RadioLTU},
	}
	for i := range w.Radios {
		candidates = append(candidates, candidate{label: "Radio", radio: &w.Radios[i]})
	}

	fallback := reportRadioMetric{}
	for _, item := range candidates {
		if item.radio == nil {
			continue
		}
		metric := reportRadioMetric{
			Band:   radioBandLabel(item.radio, item.label),
			Signal: reportRadioSignal(item.radio),
		}
		if item.radio.Capacity != nil {
			metric.Capacity = item.radio.Capacity.Combined
		}
		metric.HasSignal = validReportSignal(metric.Signal)
		if fallback.Band == "" {
			fallback = metric
		}
		if metric.HasSignal {
			return metric
		}
	}
	return fallback
}

func reportSignalQuality(signal int, band string) string {
	if !validReportSignal(signal) {
		return "no_signal"
	}
	goodThreshold := stats.Signal5GHzGood
	fairThreshold := stats.Signal5GHzFair
	if strings.Contains(strings.ToLower(band), "60") {
		goodThreshold = stats.Signal60GHzGood
		fairThreshold = stats.Signal60GHzFair
	}
	switch {
	case signal > goodThreshold:
		return "good"
	case signal > fairThreshold:
		return "fair"
	default:
		return "poor"
	}
}

func classifyReportPlatform(flavor, platform string) string {
	value := strings.ToUpper(strings.TrimSpace(flavor))
	switch {
	case strings.HasPrefix(value, "GMC"), strings.HasPrefix(value, "GMP"), strings.HasPrefix(value, "MGMP"), strings.HasPrefix(value, "GP"):
		return "wave60"
	case strings.HasPrefix(value, "MW"):
		return "wavemlo"
	case strings.HasPrefix(value, "AFLTU"), strings.HasPrefix(value, "AF5XHD"):
		return "ltu"
	case strings.HasPrefix(value, "XC"), strings.HasPrefix(value, "2XC"), strings.HasPrefix(value, "WA"), strings.HasPrefix(value, "2WA"):
		return "airmaxac"
	case strings.HasPrefix(value, "XM"), strings.HasPrefix(value, "XW"):
		return "airmaxm"
	case strings.HasPrefix(value, "AF11"):
		return "af11"
	case strings.HasPrefix(value, "AF24"):
		return "af24"
	case strings.HasPrefix(value, "AF2"), strings.HasPrefix(value, "AF3"):
		return "af2x"
	case strings.HasPrefix(value, "AF5"):
		return "af5x"
	}

	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "wave":
		return "wave"
	case "ltu":
		return "ltu"
	case "airmax":
		return "airmax"
	default:
		return "other"
	}
}

func reportPlatformName(key string) string {
	names := map[string]string{
		"wave60":   "Wave 60 GHz",
		"wavemlo":  "Wave MLO",
		"wave":     "Wave",
		"ltu":      "LTU / AF5XHD",
		"airmaxac": "airMAX AC",
		"airmaxm":  "airMAX M",
		"airmax":   "airMAX",
		"af11":     "airFiber 11",
		"af24":     "airFiber 24",
		"af2x":     "airFiber 2/3",
		"af5x":     "airFiber 5",
		"other":    "Other",
	}
	if name := names[key]; name != "" {
		return name
	}
	return key
}

func reportPlatformMetricType(key string) string {
	switch key {
	case "wave60", "wavemlo", "wave":
		return "throughput"
	case "ltu", "airmaxac", "airmaxm", "airmax", "af11", "af24", "af2x", "af5x":
		return "capacity"
	default:
		return "unknown"
	}
}

type reportSiteAggregate struct {
	Name          string
	Region        string
	Total         int
	Online        int
	Offline       int
	Unknown       int
	APs           int
	STAs          int
	Metrics       int
	SignalSamples int
	PoorSignal    int
	HighCPU       int
	TxRate        int64
	RxRate        int64
}

func reportSiteKey(site string) string {
	if strings.TrimSpace(site) == "" {
		return "Unassigned"
	}
	return site
}

func reportSiteRows(aggregates map[string]*reportSiteAggregate) []map[string]any {
	keys := make([]string, 0, len(aggregates))
	for key := range aggregates {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i] == "Unassigned" {
			return false
		}
		if keys[j] == "Unassigned" {
			return true
		}
		return strings.ToLower(keys[i]) < strings.ToLower(keys[j])
	})
	rows := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		s := aggregates[key]
		coverage := 0.0
		if s.Total > 0 {
			coverage = float64(s.Metrics) / float64(s.Total) * 100
		}
		rows = append(rows, map[string]any{
			"site":           s.Name,
			"region":         s.Region,
			"total":          s.Total,
			"online":         s.Online,
			"offline":        s.Offline,
			"unknown":        s.Unknown,
			"ap_count":       s.APs,
			"sta_count":      s.STAs,
			"metrics":        s.Metrics,
			"coverage_pct":   coverage,
			"signal_samples": s.SignalSamples,
			"poor_signal":    s.PoorSignal,
			"high_cpu":       s.HighCPU,
			"tx_rate":        s.TxRate,
			"rx_rate":        s.RxRate,
		})
	}
	return rows
}

func baseReportData(reportType string, generatedAt time.Time) map[string]any {
	return map[string]any{
		"report_version": reportSchemaVersion,
		"report_type":    reportType,
		"report_name":    reportTypeLabels[reportType],
		"scope":          "all",
		"generated_at":   generatedAt,
	}
}

func reportCoverage(inventoryDevices, metricDevices, signalSamples int) map[string]any {
	missing := inventoryDevices - metricDevices
	if missing < 0 {
		missing = 0
	}
	coveragePct := 0.0
	if inventoryDevices > 0 {
		coveragePct = float64(metricDevices) / float64(inventoryDevices) * 100
	}
	return map[string]any{
		"inventory_devices": inventoryDevices,
		"metrics_devices":   metricDevices,
		"missing_metrics":   missing,
		"signal_samples":    signalSamples,
		"coverage_pct":      coveragePct,
	}
}

func (a *API) buildHealthReport(ctx context.Context) (map[string]any, error) {
	generatedAt := time.Now().UTC()
	inventory, err := a.loadReportInventory(ctx)
	if err != nil {
		return nil, err
	}
	liveByMAC := reportStatsByMAC(a.Stats)

	statusCounts := map[string]int{"online": 0, "offline": 0, "unknown": 0}
	apCount, staCount, apOnline, staOnline := 0, 0, 0, 0
	metricDevices, signalSamples := 0, 0
	goodSignal, fairSignal, poorSignal, noSignal := 0, 0, 0, 0
	highCPU, highMem, highTemp := 0, 0, 0
	firmwareDistribution := make(map[string]int)
	siteAggregates := make(map[string]*reportSiteAggregate)

	offlineDevices := make([]map[string]any, 0)
	poorSignalDevices := make([]map[string]any, 0)
	highCPUDevices := make([]map[string]any, 0)
	inventoryByIP := make(map[string]reportInventoryDevice)

	for _, device := range inventory {
		inventoryByIP[device.IP] = device
		status := normalizeReportStatus(device.Status)
		statusCounts[status]++
		if device.IsSTA() {
			staCount++
			if status == "online" {
				staOnline++
			}
		} else {
			apCount++
			if status == "online" {
				apOnline++
			}
		}

		firmware := strings.TrimSpace(device.Firmware)
		if firmware == "" {
			firmware = "Unknown"
		}
		firmwareDistribution[firmware]++

		siteKey := reportSiteKey(device.Site)
		site := siteAggregates[siteKey]
		if site == nil {
			site = &reportSiteAggregate{Name: siteKey, Region: device.Region}
			siteAggregates[siteKey] = site
		}
		site.Total++
		switch status {
		case "online":
			site.Online++
		case "offline":
			site.Offline++
		default:
			site.Unknown++
		}
		if device.IsSTA() {
			site.STAs++
		} else {
			site.APs++
		}

		live := liveByMAC[strings.ToLower(device.MAC)]
		if live != nil {
			metricDevices++
			site.Metrics++
		}

		if status != "online" {
			lastSeen := device.LastSeen
			if live != nil && !live.LastSeen.IsZero() && (lastSeen.IsZero() || live.LastSeen.After(lastSeen)) {
				lastSeen = live.LastSeen
			}
			row := map[string]any{
				"hostname": device.DisplayName(),
				"ip":       device.IP,
				"site":     siteKey,
				"region":   device.Region,
				"status":   status,
				"is_sta":   device.IsSTA(),
			}
			if !lastSeen.IsZero() {
				row["last_seen"] = lastSeen
			}
			offlineDevices = append(offlineDevices, row)
		}

		if live == nil {
			continue
		}

		cpuUsage := 0
		if len(live.CPU) > 0 {
			cpuUsage = live.CPU[0].Usage
		} else if live.CPUUsage > 0 {
			cpuUsage = int(live.CPUUsage)
		}
		memUsage := live.RAM.Usage
		if memUsage == 0 && live.MemUsage > 0 {
			memUsage = int(live.MemUsage)
		}
		if cpuUsage > 80 {
			highCPU++
			site.HighCPU++
			highCPUDevices = append(highCPUDevices, map[string]any{
				"hostname": device.DisplayName(), "ip": device.IP, "site": siteKey,
				"cpu": cpuUsage, "is_sta": device.IsSTA(),
			})
		}
		if memUsage > 80 {
			highMem++
		}
		if live.Temperature.CPU > 70 || live.Temperature.Board > 70 {
			highTemp++
		}

		if !device.IsSTA() {
			continue
		}
		radio := reportPrimaryRadio(live.Wireless)
		quality := reportSignalQuality(radio.Signal, radio.Band)
		if radio.HasSignal {
			signalSamples++
			site.SignalSamples++
		}
		switch quality {
		case "good":
			goodSignal++
		case "fair":
			fairSignal++
		case "poor":
			poorSignal++
			site.PoorSignal++
			poorSignalDevices = append(poorSignalDevices, map[string]any{
				"hostname": device.DisplayName(), "ip": device.IP, "site": siteKey,
				"signal": radio.Signal, "band": radio.Band,
				"parent_hostname": device.ParentHostname,
			})
		default:
			noSignal++
		}
	}

	sort.Slice(offlineDevices, func(i, j int) bool {
		statusI, statusJ := getString(offlineDevices[i], "status"), getString(offlineDevices[j], "status")
		if statusI != statusJ {
			return statusI == "offline"
		}
		ti, _ := offlineDevices[i]["last_seen"].(time.Time)
		tj, _ := offlineDevices[j]["last_seen"].(time.Time)
		if ti.IsZero() != tj.IsZero() {
			return ti.IsZero()
		}
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return strings.ToLower(getString(offlineDevices[i], "hostname")) < strings.ToLower(getString(offlineDevices[j], "hostname"))
	})
	sort.Slice(poorSignalDevices, func(i, j int) bool {
		left, right := getInt64(poorSignalDevices[i], "signal"), getInt64(poorSignalDevices[j], "signal")
		if left != right {
			return left < right
		}
		return getString(poorSignalDevices[i], "hostname") < getString(poorSignalDevices[j], "hostname")
	})
	sort.Slice(highCPUDevices, func(i, j int) bool {
		left, right := getInt64(highCPUDevices[i], "cpu"), getInt64(highCPUDevices[j], "cpu")
		if left != right {
			return left > right
		}
		return getString(highCPUDevices[i], "hostname") < getString(highCPUDevices[j], "hostname")
	})

	stabilityStats := a.Stats.GetStabilityStats()
	flappingDevices := make([]map[string]any, 0)
	rebootingDevices := make([]map[string]any, 0)
	totalFlaps1h, totalFlaps24h, totalReboots1h, totalReboots24h := 0, 0, 0, 0
	for _, stability := range stabilityStats {
		if stability == nil {
			continue
		}
		totalFlaps1h += stability.Flaps1h
		totalFlaps24h += stability.Flaps24h
		totalReboots1h += stability.Reboots1h
		totalReboots24h += stability.Reboots24h
		device := inventoryByIP[stability.IP]
		hostname := strings.TrimSpace(stability.Hostname)
		if hostname == "" {
			hostname = device.DisplayName()
		}
		site := reportSiteKey(device.Site)
		if stability.Flaps1h > 0 || stability.Flaps24h > 0 {
			flappingDevices = append(flappingDevices, map[string]any{
				"hostname": hostname, "ip": stability.IP, "site": site,
				"flaps_1h": stability.Flaps1h, "flaps_24h": stability.Flaps24h,
			})
		}
		if stability.Reboots1h > 0 || stability.Reboots24h > 0 {
			rebootingDevices = append(rebootingDevices, map[string]any{
				"hostname": hostname, "ip": stability.IP, "site": site,
				"reboots_1h": stability.Reboots1h, "reboots_24h": stability.Reboots24h,
			})
		}
	}
	sort.Slice(flappingDevices, func(i, j int) bool {
		left, right := getInt64(flappingDevices[i], "flaps_1h"), getInt64(flappingDevices[j], "flaps_1h")
		if left == right {
			left, right = getInt64(flappingDevices[i], "flaps_24h"), getInt64(flappingDevices[j], "flaps_24h")
		}
		return left > right
	})
	sort.Slice(rebootingDevices, func(i, j int) bool {
		left, right := getInt64(rebootingDevices[i], "reboots_1h"), getInt64(rebootingDevices[j], "reboots_1h")
		if left == right {
			left, right = getInt64(rebootingDevices[i], "reboots_24h"), getInt64(rebootingDevices[j], "reboots_24h")
		}
		return left > right
	})

	truncateMaps := func(values []map[string]any, limit int) []map[string]any {
		if len(values) <= limit {
			return values
		}
		return values[:limit]
	}

	total := len(inventory)
	uptime := 0.0
	if total > 0 {
		uptime = float64(statusCounts["online"]) / float64(total) * 100
	}
	data := baseReportData("health", generatedAt)
	data["summary"] = map[string]any{
		"total": total, "online": statusCounts["online"], "offline": statusCounts["offline"],
		"unknown": statusCounts["unknown"], "uptime": uptime,
		"ap_count": apCount, "sta_count": staCount, "ap_online": apOnline, "sta_online": staOnline,
	}
	data["coverage"] = reportCoverage(total, metricDevices, signalSamples)
	data["link_quality"] = map[string]any{
		"good": goodSignal, "fair": fairSignal, "poor": poorSignal, "no_signal": noSignal,
	}
	data["system_health"] = map[string]any{"high_cpu": highCPU, "high_mem": highMem, "high_temp": highTemp}
	data["stability"] = map[string]any{
		"flaps_1h": totalFlaps1h, "flaps_24h": totalFlaps24h,
		"reboots_1h": totalReboots1h, "reboots_24h": totalReboots24h,
	}
	data["firmware_distribution"] = firmwareDistribution
	data["site_summary"] = reportSiteRows(siteAggregates)
	data["top_offenders"] = map[string]any{
		"offline": truncateMaps(offlineDevices, 50), "poor_signal": truncateMaps(poorSignalDevices, 50),
		"high_cpu": truncateMaps(highCPUDevices, 50), "flapping": truncateMaps(flappingDevices, 50),
		"rebooting": truncateMaps(rebootingDevices, 50),
	}
	return data, nil
}

func (a *API) buildInventoryReport(ctx context.Context) (map[string]any, error) {
	generatedAt := time.Now().UTC()
	inventory, err := a.loadReportInventory(ctx)
	if err != nil {
		return nil, err
	}

	devices := make([]map[string]any, 0, len(inventory))
	statusCounts := map[string]int{"online": 0, "offline": 0, "unknown": 0}
	platformDistribution := make(map[string]int)
	firmwareDistribution := make(map[string]int)
	sites, regions := make(map[string]struct{}), make(map[string]struct{})
	apCount, staCount, unassigned := 0, 0, 0
	siteAggregates := make(map[string]*reportSiteAggregate)

	for _, device := range inventory {
		status := normalizeReportStatus(device.Status)
		statusCounts[status]++
		if device.IsSTA() {
			staCount++
		} else {
			apCount++
		}
		if device.Site == "" {
			unassigned++
		} else {
			sites[device.Site] = struct{}{}
		}
		if device.Region != "" {
			regions[device.Region] = struct{}{}
		}
		platform := classifyReportPlatform(device.Flavor, device.Platform)
		platformDistribution[platform]++
		firmware := strings.TrimSpace(device.Firmware)
		if firmware == "" {
			firmware = "Unknown"
		}
		firmwareDistribution[firmware]++

		siteKey := reportSiteKey(device.Site)
		site := siteAggregates[siteKey]
		if site == nil {
			site = &reportSiteAggregate{Name: siteKey, Region: device.Region}
			siteAggregates[siteKey] = site
		}
		site.Total++
		switch status {
		case "online":
			site.Online++
		case "offline":
			site.Offline++
		default:
			site.Unknown++
		}
		if device.IsSTA() {
			site.STAs++
		} else {
			site.APs++
		}

		row := map[string]any{
			"id": device.ID, "hostname": device.Hostname, "ip": device.IP, "mac": device.MAC,
			"product": device.Product, "firmware": device.Firmware, "flavor": device.Flavor,
			"status": status, "platform": device.Platform, "platform_family": platform,
			"region": device.Region, "site": device.Site, "is_sta": device.IsSTA(),
		}
		if device.ParentID > 0 {
			row["parent_id"] = device.ParentID
			row["parent_hostname"] = device.ParentHostname
			row["parent_ip"] = device.ParentIP
		}
		if device.HasLastSeen {
			row["last_seen"] = device.LastSeen
		}
		devices = append(devices, row)
	}

	data := baseReportData("inventory", generatedAt)
	data["summary"] = map[string]any{
		"total": len(inventory), "online": statusCounts["online"], "offline": statusCounts["offline"],
		"unknown": statusCounts["unknown"], "ap_count": apCount, "sta_count": staCount,
		"site_count": len(sites), "region_count": len(regions), "unassigned_site": unassigned,
		"firmware_versions": len(firmwareDistribution), "platform_families": len(platformDistribution),
	}
	data["coverage"] = reportCoverage(len(inventory), len(inventory), 0)
	data["status_distribution"] = statusCounts
	data["platform_distribution"] = platformDistribution
	data["firmware_distribution"] = firmwareDistribution
	data["site_summary"] = reportSiteRows(siteAggregates)
	data["devices"] = devices
	return data, nil
}

type reportPlatformAggregate struct {
	Key         string
	Name        string
	MetricType  string
	TxRate      int64
	RxRate      int64
	APCount     int
	STACount    int
	Good        int
	Fair        int
	Poor        int
	NoSignal    int
	SignalSum   int
	SignalCount int
}

func (a *API) buildPerformanceReport(ctx context.Context) (map[string]any, error) {
	generatedAt := time.Now().UTC()
	inventory, err := a.loadReportInventory(ctx)
	if err != nil {
		return nil, err
	}
	liveByMAC := reportStatsByMAC(a.Stats)
	parentNames := make(map[int]string, len(inventory))
	clientCounts := make(map[int]int)
	inventoryAPs, inventorySTAs := 0, 0
	for _, device := range inventory {
		parentNames[device.ID] = device.DisplayName()
		if device.IsSTA() {
			inventorySTAs++
			clientCounts[device.ParentID]++
		} else {
			inventoryAPs++
		}
	}

	apDevices := make([]map[string]any, 0)
	staDevices := make([]map[string]any, 0)
	missingDevices := make([]map[string]any, 0)
	platformAggregates := make(map[string]*reportPlatformAggregate)
	siteAggregates := make(map[string]*reportSiteAggregate)
	measuredClients, poorClients := make(map[int]int), make(map[int]int)
	metricDevices, signalSamples := 0, 0
	goodSignal, fairSignal, poorSignal, noSignal := 0, 0, 0, 0
	var totalTx, totalRx, apTx, apRx, staTx, staRx int64
	signalSum := 0

	for _, device := range inventory {
		siteKey := reportSiteKey(device.Site)
		site := siteAggregates[siteKey]
		if site == nil {
			site = &reportSiteAggregate{Name: siteKey, Region: device.Region}
			siteAggregates[siteKey] = site
		}
		site.Total++
		if device.IsSTA() {
			site.STAs++
		} else {
			site.APs++
		}

		live := liveByMAC[strings.ToLower(device.MAC)]
		if live == nil {
			missingDevices = append(missingDevices, map[string]any{
				"id": device.ID, "hostname": device.DisplayName(), "ip": device.IP,
				"site": siteKey, "status": device.Status, "is_sta": device.IsSTA(),
			})
			continue
		}
		metricDevices++
		site.Metrics++

		platformKey := classifyReportPlatform(device.Flavor, device.Platform)
		platform := platformAggregates[platformKey]
		if platform == nil {
			platform = &reportPlatformAggregate{
				Key: platformKey, Name: reportPlatformName(platformKey), MetricType: reportPlatformMetricType(platformKey),
			}
			platformAggregates[platformKey] = platform
		}

		ip := strings.TrimSpace(live.IP)
		if ip == "" {
			ip = device.IP
		}
		hostname := strings.TrimSpace(live.Hostname)
		if hostname == "" {
			hostname = device.DisplayName()
		}
		radio := reportPrimaryRadio(live.Wireless)
		quality := "no_signal"
		if device.IsSTA() {
			quality = reportSignalQuality(radio.Signal, radio.Band)
			if radio.HasSignal {
				signalSamples++
				signalSum += radio.Signal
				site.SignalSamples++
				platform.SignalSum += radio.Signal
				platform.SignalCount++
			}
			switch quality {
			case "good":
				goodSignal++
				platform.Good++
			case "fair":
				fairSignal++
				platform.Fair++
			case "poor":
				poorSignal++
				platform.Poor++
				site.PoorSignal++
				poorClients[device.ParentID]++
			default:
				noSignal++
				platform.NoSignal++
			}
			measuredClients[device.ParentID]++
		}

		cpuUsage := 0
		if len(live.CPU) > 0 {
			cpuUsage = live.CPU[0].Usage
		} else if live.CPUUsage > 0 {
			cpuUsage = int(live.CPUUsage)
		}
		memUsage := live.RAM.Usage
		if memUsage == 0 && live.MemUsage > 0 {
			memUsage = int(live.MemUsage)
		}

		status := normalizeReportStatus(device.Status)
		row := map[string]any{
			"id": device.ID, "ip": ip, "hostname": hostname, "product": device.Product,
			"flavor": device.Flavor, "platform": platformKey, "site": siteKey, "region": device.Region,
			"is_sta": device.IsSTA(), "status": status, "online": status == "online",
			"tx_rate": live.Wireless.TxRate, "rx_rate": live.Wireless.RxRate,
			"signal": radio.Signal, "signal_quality": quality, "capacity": radio.Capacity,
			"cpu": cpuUsage, "ram": memUsage, "band": radio.Band, "last_seen": live.LastSeen,
		}
		if device.IsSTA() {
			row["parent_id"] = device.ParentID
			row["parent_hostname"] = parentNames[device.ParentID]
		}

		totalTx += live.Wireless.TxRate
		totalRx += live.Wireless.RxRate
		site.TxRate += live.Wireless.TxRate
		site.RxRate += live.Wireless.RxRate
		platform.TxRate += live.Wireless.TxRate
		platform.RxRate += live.Wireless.RxRate
		if device.IsSTA() {
			staTx += live.Wireless.TxRate
			staRx += live.Wireless.RxRate
			platform.STACount++
			staDevices = append(staDevices, row)
		} else {
			apTx += live.Wireless.TxRate
			apRx += live.Wireless.RxRate
			platform.APCount++
			apDevices = append(apDevices, row)
		}
	}

	capacityRisk := make([]map[string]any, 0)
	for _, ap := range apDevices {
		apID := int(getInt64(ap, "id"))
		clients := clientCounts[apID]
		measured := measuredClients[apID]
		poor := poorClients[apID]
		poorPct := 0.0
		if measured > 0 {
			poorPct = float64(poor) / float64(measured) * 100
		}
		ap["client_count"] = clients
		ap["measured_clients"] = measured
		ap["poor_clients"] = poor
		ap["poor_pct"] = poorPct
		if measured > 0 && poorPct >= 20 {
			capacityRisk = append(capacityRisk, ap)
		}
	}
	sort.Slice(capacityRisk, func(i, j int) bool {
		left, right := getFloat64(capacityRisk[i], "poor_pct"), getFloat64(capacityRisk[j], "poor_pct")
		if left != right {
			return left > right
		}
		return getString(capacityRisk[i], "hostname") < getString(capacityRisk[j], "hostname")
	})
	if len(capacityRisk) > 50 {
		capacityRisk = capacityRisk[:50]
	}

	sort.Slice(apDevices, func(i, j int) bool {
		return strings.ToLower(getString(apDevices[i], "hostname")) < strings.ToLower(getString(apDevices[j], "hostname"))
	})
	sort.Slice(staDevices, func(i, j int) bool {
		left, right := getInt64(staDevices[i], "signal"), getInt64(staDevices[j], "signal")
		if left != right {
			if left == 0 {
				return false
			}
			if right == 0 {
				return true
			}
			return left < right
		}
		return strings.ToLower(getString(staDevices[i], "hostname")) < strings.ToLower(getString(staDevices[j], "hostname"))
	})
	sort.Slice(missingDevices, func(i, j int) bool {
		return strings.ToLower(getString(missingDevices[i], "hostname")) < strings.ToLower(getString(missingDevices[j], "hostname"))
	})
	if len(missingDevices) > 100 {
		missingDevices = missingDevices[:100]
	}

	platformKeys := make([]string, 0, len(platformAggregates))
	for key := range platformAggregates {
		platformKeys = append(platformKeys, key)
	}
	sort.Strings(platformKeys)
	platformBreakdown := make(map[string]map[string]any, len(platformKeys))
	platformRows := make([]map[string]any, 0, len(platformKeys))
	for _, key := range platformKeys {
		platform := platformAggregates[key]
		avgSignal := 0
		if platform.SignalCount > 0 {
			avgSignal = platform.SignalSum / platform.SignalCount
		}
		row := map[string]any{
			"key": key, "name": platform.Name, "metric_type": platform.MetricType,
			"tx_rate": platform.TxRate, "rx_rate": platform.RxRate,
			"ap_count": platform.APCount, "sta_count": platform.STACount,
			"good": platform.Good, "fair": platform.Fair, "poor": platform.Poor,
			"no_signal": platform.NoSignal, "avg_signal": avgSignal,
		}
		platformRows = append(platformRows, row)
		platformBreakdown[key] = row
	}

	avgSignal := 0
	if signalSamples > 0 {
		avgSignal = signalSum / signalSamples
	}
	data := baseReportData("performance", generatedAt)
	data["summary"] = map[string]any{
		"total_tx_rate": totalTx, "total_rx_rate": totalRx,
		"ap_tx_rate": apTx, "ap_rx_rate": apRx, "sta_tx_rate": staTx, "sta_rx_rate": staRx,
		"avg_signal": avgSignal, "device_count": len(inventory),
		"metrics_device_count": metricDevices, "ap_count": len(apDevices), "sta_count": len(staDevices),
		"inventory_ap_count": inventoryAPs, "inventory_sta_count": inventorySTAs,
	}
	data["coverage"] = reportCoverage(len(inventory), metricDevices, signalSamples)
	data["signal_quality"] = map[string]any{
		"good": goodSignal, "fair": fairSignal, "poor": poorSignal, "no_signal": noSignal,
	}
	data["platform_breakdown"] = platformBreakdown
	data["platforms"] = platformRows
	data["site_summary"] = reportSiteRows(siteAggregates)
	data["throughput_history"] = a.Stats.GetThroughputHistory()
	data["ap_devices"] = apDevices
	data["sta_devices"] = staDevices
	data["missing_devices"] = missingDevices
	data["capacity_risk"] = capacityRisk
	return data, nil
}

func getBool(m map[string]any, key string) bool {
	value, _ := m[key].(bool)
	return value
}

func anyToString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case time.Time:
		return typed.Format(time.RFC3339)
	default:
		return fmt.Sprint(typed)
	}
}

func mapFromAny(value any) map[string]any {
	result, _ := value.(map[string]any)
	if result == nil {
		return map[string]any{}
	}
	return result
}

func writeReportCSV(output io.Writer, reportType string, reportData map[string]any) error {
	writer := csv.NewWriter(output)
	defer writer.Flush()
	write := func(row ...string) error { return writer.Write(row) }

	switch reportType {
	case "health":
		if err := write("Section", "Metric", "Value", "Hostname", "IP", "Site", "Detail"); err != nil {
			return err
		}
		sections := []struct {
			name string
			key  string
		}{
			{name: "Summary", key: "summary"},
			{name: "Coverage", key: "coverage"},
			{name: "Link Quality", key: "link_quality"},
			{name: "System Health", key: "system_health"},
			{name: "Stability", key: "stability"},
		}
		for _, section := range sections {
			values := getMap(reportData, section.key)
			keys := make([]string, 0, len(values))
			for key := range values {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if err := write(section.name, key, anyToString(values[key]), "", "", "", ""); err != nil {
					return err
				}
			}
		}
		offenders := getMap(reportData, "top_offenders")
		categories := []string{"offline", "poor_signal", "high_cpu", "flapping", "rebooting"}
		for _, category := range categories {
			rows, _ := offenders[category].([]any)
			for _, raw := range rows {
				item := mapFromAny(raw)
				detailParts := make([]string, 0)
				for _, key := range []string{"status", "signal", "band", "cpu", "flaps_1h", "flaps_24h", "reboots_1h", "reboots_24h", "last_seen"} {
					if value, ok := item[key]; ok && anyToString(value) != "" {
						detailParts = append(detailParts, key+"="+anyToString(value))
					}
				}
				if err := write("Top Offender", category, "1", getString(item, "hostname"), getString(item, "ip"), getString(item, "site"), strings.Join(detailParts, "; ")); err != nil {
					return err
				}
			}
		}

	case "inventory":
		if err := write("Hostname", "IP", "MAC", "Product", "Flavor", "Firmware", "Platform", "Platform Family", "Status", "Region", "Site", "Type", "Parent", "Last Seen"); err != nil {
			return err
		}
		for _, raw := range getSlice(reportData, "devices") {
			device := mapFromAny(raw)
			deviceType, parent := "AP", ""
			if getBool(device, "is_sta") {
				deviceType = "STA"
				parent = getString(device, "parent_hostname")
				if parent == "" {
					parent = getString(device, "parent_ip")
				}
			}
			if err := write(
				getString(device, "hostname"), getString(device, "ip"), getString(device, "mac"),
				getString(device, "product"), getString(device, "flavor"), getString(device, "firmware"),
				getString(device, "platform"), getString(device, "platform_family"), getString(device, "status"),
				getString(device, "region"), getString(device, "site"), deviceType, parent,
				anyToString(device["last_seen"]),
			); err != nil {
				return err
			}
		}

	case "performance":
		if err := write("Type", "Hostname", "IP", "Site", "Parent AP", "Product", "Flavor", "Platform", "Band", "Status", "Online", "TX Rate bps", "RX Rate bps", "Signal dBm", "Signal Quality", "Capacity bps", "CPU %", "RAM %", "Last Seen"); err != nil {
			return err
		}
		devices := append([]any{}, getSlice(reportData, "ap_devices")...)
		devices = append(devices, getSlice(reportData, "sta_devices")...)
		for _, raw := range devices {
			device := mapFromAny(raw)
			deviceType := "AP"
			if getBool(device, "is_sta") {
				deviceType = "STA"
			}
			if err := write(
				deviceType, getString(device, "hostname"), getString(device, "ip"), getString(device, "site"),
				getString(device, "parent_hostname"), getString(device, "product"), getString(device, "flavor"),
				getString(device, "platform"), getString(device, "band"), getString(device, "status"),
				anyToString(device["online"]), anyToString(device["tx_rate"]), anyToString(device["rx_rate"]),
				anyToString(device["signal"]), getString(device, "signal_quality"), anyToString(device["capacity"]),
				anyToString(device["cpu"]), anyToString(device["ram"]), anyToString(device["last_seen"]),
			); err != nil {
				return err
			}
		}

	case "chain":
		if err := write("Scope", "Band", "Hostname", "Affected IP", "Site", "Parent AP", "Parent IP", "MAC", "Side", "AP Spread dB", "STA Spread dB", "Max Spread dB", "AP Chains", "STA Chains"); err != nil {
			return err
		}
		for _, raw := range getSlice(reportData, "issues") {
			issue := mapFromAny(raw)
			joinValues := func(value any) string {
				array, _ := value.([]any)
				parts := make([]string, 0, len(array))
				for _, item := range array {
					parts = append(parts, anyToString(item))
				}
				return strings.Join(parts, " / ")
			}
			if err := write(
				getString(issue, "scope"), getString(issue, "band"), getString(issue, "hostname"),
				getString(issue, "affected_ip"), getString(issue, "site"), getString(issue, "parent_hostname"),
				getString(issue, "parent_ip"), getString(issue, "mac"), getString(issue, "mismatch_side"),
				anyToString(issue["ap_spread_db"]), anyToString(issue["sta_spread_db"]), anyToString(issue["spread_db"]),
				joinValues(issue["ap_chains"]), joinValues(issue["sta_chains"]),
			); err != nil {
				return err
			}
		}

	case "rx_mismatch":
		if err := write("Band", "AP Hostname", "AP IP", "STA Hostname", "STA IP", "Affected IP", "Site", "MAC", "AP RX dBm", "STA RX dBm", "Delta dB", "Stronger Side"); err != nil {
			return err
		}
		for _, raw := range getSlice(reportData, "issues") {
			issue := mapFromAny(raw)
			if err := write(
				getString(issue, "band"), getString(issue, "ap_hostname"), getString(issue, "ap_ip"),
				getString(issue, "sta_hostname"), getString(issue, "sta_ip"), getString(issue, "affected_ip"),
				getString(issue, "site"), getString(issue, "mac"), anyToString(issue["ap_rx"]),
				anyToString(issue["sta_rx"]), anyToString(issue["delta_db"]), getString(issue, "stronger_side"),
			); err != nil {
				return err
			}
		}

	default:
		return fmt.Errorf("unsupported report type %q", reportType)
	}

	writer.Flush()
	return writer.Error()
}
