package zabbix

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yellowman/wavecontrol/internal/stats"
)

// Bridge implements Zabbix agent protocol for device stats
type Bridge struct {
	store    *stats.Store
	listener net.Listener
	addr     string

	// Security: allowed source IPs/CIDRs (empty = allow all - NOT RECOMMENDED)
	allowedNets []*net.IPNet
	allowedIPs  map[string]bool

	mu      sync.RWMutex
	running bool
}

// NewBridge creates a new Zabbix bridge
func NewBridge(store *stats.Store, addr string) *Bridge {
	return &Bridge{
		store:      store,
		addr:       addr,
		allowedIPs: make(map[string]bool),
	}
}

// SetAllowedHosts sets the list of allowed source IPs/CIDRs
// Format: "10.0.0.1,192.168.1.0/24,zabbix.example.com"
// Empty string or "0.0.0.0/0" allows all (insecure)
func (b *Bridge) SetAllowedHosts(hosts string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.allowedNets = nil
	b.allowedIPs = make(map[string]bool)

	if hosts == "" {
		log.Printf("Zabbix bridge: WARNING - no allowed hosts configured, accepting all connections")
		return
	}

	for _, h := range strings.Split(hosts, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}

		// Check if it's a CIDR
		if strings.Contains(h, "/") {
			_, ipnet, err := net.ParseCIDR(h)
			if err != nil {
				log.Printf("Zabbix bridge: invalid CIDR %q: %v", h, err)
				continue
			}
			b.allowedNets = append(b.allowedNets, ipnet)
			log.Printf("Zabbix bridge: allowing network %s", ipnet)
		} else {
			// Try to parse as IP first
			ip := net.ParseIP(h)
			if ip != nil {
				b.allowedIPs[ip.String()] = true
				log.Printf("Zabbix bridge: allowing IP %s", ip)
			} else {
				// Try DNS resolution
				addrs, err := net.LookupIP(h)
				if err != nil {
					log.Printf("Zabbix bridge: cannot resolve %q: %v", h, err)
					continue
				}
				for _, addr := range addrs {
					b.allowedIPs[addr.String()] = true
					log.Printf("Zabbix bridge: allowing %s (%s)", h, addr)
				}
			}
		}
	}

	if len(b.allowedNets) == 0 && len(b.allowedIPs) == 0 {
		log.Printf("Zabbix bridge: WARNING - no valid allowed hosts, accepting all connections")
	}
}

// isAllowed checks if a remote address is allowed to connect
func (b *Bridge) isAllowed(remoteAddr net.Addr) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	// No restrictions configured = allow all (legacy behavior, with warning at startup)
	if len(b.allowedNets) == 0 && len(b.allowedIPs) == 0 {
		return true
	}

	// Extract IP from remote address
	host, _, err := net.SplitHostPort(remoteAddr.String())
	if err != nil {
		host = remoteAddr.String()
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}

	// Check explicit IPs
	if b.allowedIPs[ip.String()] {
		return true
	}

	// Check CIDRs
	for _, ipnet := range b.allowedNets {
		if ipnet.Contains(ip) {
			return true
		}
	}

	return false
}

// Start begins listening for Zabbix connections
func (b *Bridge) Start() error {
	var err error
	b.listener, err = net.Listen("tcp", b.addr)
	if err != nil {
		return fmt.Errorf("zabbix listen: %w", err)
	}

	b.mu.Lock()
	b.running = true
	b.mu.Unlock()

	log.Printf("Zabbix bridge listening on %s", b.addr)

	go b.acceptLoop()
	return nil
}

// Stop stops the bridge
func (b *Bridge) Stop() {
	b.mu.Lock()
	b.running = false
	b.mu.Unlock()

	if b.listener != nil {
		b.listener.Close()
	}
}

func (b *Bridge) acceptLoop() {
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			b.mu.RLock()
			running := b.running
			b.mu.RUnlock()
			if !running {
				return
			}
			log.Printf("Zabbix accept error: %v", err)
			continue
		}

		// Check if source is allowed
		if !b.isAllowed(conn.RemoteAddr()) {
			log.Printf("Zabbix bridge: rejected connection from %s (not in allowed hosts)", conn.RemoteAddr())
			conn.Close()
			continue
		}

		go b.handleConnection(conn)
	}
}

func (b *Bridge) handleConnection(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	reader := bufio.NewReader(conn)

	// Read Zabbix protocol header
	header := make([]byte, 5)
	if _, err := io.ReadFull(reader, header); err != nil {
		return
	}

	// Check header "ZBXD\x01"
	if string(header[:4]) != "ZBXD" || header[4] != 0x01 {
		// Might be plain text request (older protocol)
		// Read rest of line
		line, _ := reader.ReadString('\n')
		key := string(header) + strings.TrimSpace(line)
		b.sendResponse(conn, b.processKey(key))
		return
	}

	// Read data length (8 bytes, little endian)
	lenBuf := make([]byte, 8)
	if _, err := io.ReadFull(reader, lenBuf); err != nil {
		return
	}
	dataLen := binary.LittleEndian.Uint64(lenBuf)

	if dataLen > 65536 {
		return // Too large
	}

	// Read data
	data := make([]byte, dataLen)
	if _, err := io.ReadFull(reader, data); err != nil {
		return
	}

	// Process request
	key := strings.TrimSpace(string(data))
	response := b.processKey(key)
	b.sendResponse(conn, response)
}

func (b *Bridge) sendResponse(conn net.Conn, data string) {
	// Zabbix protocol response: "ZBXD\x01" + 8-byte length + data
	header := []byte("ZBXD\x01")
	lenBuf := make([]byte, 8)
	binary.LittleEndian.PutUint64(lenBuf, uint64(len(data)))

	conn.Write(header)
	conn.Write(lenBuf)
	conn.Write([]byte(data))
}

// processKey handles a Zabbix item key request
// Format: wavecontrol.device[IP,metric] or wavecontrol.discovery
func (b *Bridge) processKey(key string) string {
	key = strings.TrimSpace(key)

	// Discovery request
	if key == "wavecontrol.discovery" || key == "wavecontrol.discovery[]" {
		return b.discovery()
	}

	// Parse wavecontrol.device[IP,metric]
	if !strings.HasPrefix(key, "wavecontrol.") {
		return "ZBX_NOTSUPPORTED"
	}

	// Extract function and args
	openBracket := strings.Index(key, "[")
	closeBracket := strings.LastIndex(key, "]")

	if openBracket == -1 || closeBracket == -1 {
		return "ZBX_NOTSUPPORTED"
	}

	funcName := key[len("wavecontrol."):openBracket]
	args := strings.Split(key[openBracket+1:closeBracket], ",")

	switch funcName {
	case "device":
		if len(args) < 2 {
			return "ZBX_NOTSUPPORTED"
		}
		return b.deviceMetric(args[0], args[1])

	case "peer":
		if len(args) < 3 {
			return "ZBX_NOTSUPPORTED"
		}
		return b.peerMetric(args[0], args[1], args[2])

	case "count":
		return b.count(args)

	default:
		return "ZBX_NOTSUPPORTED"
	}
}

// discovery returns LLD (Low-Level Discovery) JSON for all devices
func (b *Bridge) discovery() string {
	devices := b.store.List()

	type lldDevice struct {
		IP       string `json:"{#IP}"`
		MAC      string `json:"{#MAC}"`
		Hostname string `json:"{#HOSTNAME}"`
		Platform string `json:"{#PLATFORM}"`
		IsAP     string `json:"{#ISAP}"`
	}

	var data []lldDevice
	for _, d := range devices {
		isAP := "0"
		if d.ParentIP == "" {
			isAP = "1"
		}
		data = append(data, lldDevice{
			IP:       d.IP,
			MAC:      d.MAC,
			Hostname: d.Hostname,
			Platform: detectPlatform(d),
			IsAP:     isAP,
		})
	}

	result := map[string]any{"data": data}
	out, _ := json.Marshal(result)
	return string(out)
}

func detectPlatform(d *stats.DeviceStats) string {
	if d.Wireless.Radio60GHz != nil {
		return "wave"
	}
	if d.Wireless.RadioLTU != nil {
		return "ltu"
	}
	return "unknown"
}

// deviceMetric returns a specific metric for a device
func (b *Bridge) deviceMetric(ip, metric string) string {
	// Accept either IP or MAC as the first argument.
	// MAC is preferred for stable identity when IPs are reused.
	var d *stats.DeviceStats
	if strings.Contains(ip, ":") || strings.Contains(ip, "-") {
		d = b.store.GetByMAC(ip)
	}
	if d == nil {
		d = b.store.Get(ip)
	}
	if d == nil {
		return "ZBX_NOTSUPPORTED"
	}

	switch metric {
	// Status
	case "online":
		return boolToStr(d.Online)
	case "uptime":
		return strconv.FormatInt(d.Uptime, 10)
	case "last_seen":
		return strconv.FormatInt(d.LastSeen.Unix(), 10)

	// System
	case "cpu":
		if len(d.CPU) > 0 {
			total := 0
			for _, c := range d.CPU {
				total += c.Usage
			}
			return strconv.Itoa(total / len(d.CPU))
		}
		return "0"
	case "ram.usage":
		return strconv.Itoa(d.RAM.Usage)
	case "ram.free":
		return strconv.FormatInt(d.RAM.Free, 10)
	case "temp.cpu":
		return strconv.FormatFloat(d.Temperature.CPU, 'f', 1, 64)
	case "temp.radio60":
		return strconv.FormatFloat(d.Temperature.Radio60, 'f', 1, 64)
	case "temp.radio5":
		return strconv.FormatFloat(d.Temperature.Radio5, 'f', 1, 64)

	// GPS
	case "gps.fix":
		return boolToStr(d.GPS.Fix)
	case "gps.lat":
		return strconv.FormatFloat(d.GPS.Lat, 'f', 6, 64)
	case "gps.lon":
		return strconv.FormatFloat(d.GPS.Lon, 'f', 6, 64)
	case "gps.sats":
		return strconv.Itoa(d.GPS.Sats)

	// Wireless aggregate
	case "wireless.tx_rate":
		return strconv.FormatInt(d.Wireless.TxRate, 10)
	case "wireless.rx_rate":
		return strconv.FormatInt(d.Wireless.RxRate, 10)
	case "wireless.service_uptime":
		return strconv.FormatInt(d.Wireless.ServiceUptime, 10)

	// 60GHz radio
	case "radio60.capacity":
		if d.Wireless.Radio60GHz != nil && d.Wireless.Radio60GHz.Capacity != nil {
			return strconv.FormatInt(d.Wireless.Radio60GHz.Capacity.Combined, 10)
		}
		return "0"
	case "radio60.capacity.ideal":
		if d.Wireless.Radio60GHz != nil && d.Wireless.Radio60GHz.Capacity != nil {
			return strconv.FormatInt(d.Wireless.Radio60GHz.Capacity.CombinedIdeal, 10)
		}
		return "0"
	case "radio60.frequency":
		if d.Wireless.Radio60GHz != nil {
			return strconv.Itoa(d.Wireless.Radio60GHz.Frequency)
		}
		return "0"
	case "radio60.channel_width":
		if d.Wireless.Radio60GHz != nil {
			return strconv.Itoa(d.Wireless.Radio60GHz.ChannelWidth)
		}
		return "0"

	// 5GHz radio
	case "radio5.capacity":
		if d.Wireless.Radio5GHz != nil && d.Wireless.Radio5GHz.Capacity != nil {
			return strconv.FormatInt(d.Wireless.Radio5GHz.Capacity.Combined, 10)
		}
		return "0"
	case "radio5.frequency":
		if d.Wireless.Radio5GHz != nil {
			return strconv.Itoa(d.Wireless.Radio5GHz.Frequency)
		}
		return "0"

	// LTU radio
	case "radioltu.capacity":
		if d.Wireless.RadioLTU != nil && d.Wireless.RadioLTU.Capacity != nil {
			return strconv.FormatInt(d.Wireless.RadioLTU.Capacity.Combined, 10)
		}
		return "0"
	case "radioltu.frequency":
		if d.Wireless.RadioLTU != nil {
			return strconv.Itoa(d.Wireless.RadioLTU.Frequency)
		}
		return "0"

	// Link scores
	case "link_score.dl":
		if d.Wireless.LinkScore != nil {
			return strconv.Itoa(d.Wireless.LinkScore.DL)
		}
		return "0"
	case "link_score.ul":
		if d.Wireless.LinkScore != nil {
			return strconv.Itoa(d.Wireless.LinkScore.UL)
		}
		return "0"

	// Peer count
	case "peer_count":
		return strconv.Itoa(d.PeerCount)

	default:
		return "ZBX_NOTSUPPORTED"
	}
}

// peerMetric returns a metric for a specific peer (STA) of an AP
func (b *Bridge) peerMetric(apIP, staMAC, metric string) string {
	ap := b.store.Get(apIP)
	if ap == nil {
		return "ZBX_NOTSUPPORTED"
	}

	// Find peer
	var peer *stats.PeerStats
	for _, p := range ap.Peers {
		if strings.EqualFold(p.MAC, staMAC) || p.IP == staMAC {
			peer = p
			break
		}
	}

	if peer == nil {
		return "ZBX_NOTSUPPORTED"
	}

	switch metric {
	// Identity
	case "ip":
		return peer.IP
	case "hostname":
		return peer.Hostname
	case "firmware":
		return peer.Firmware

	// Connection
	case "distance":
		return strconv.FormatFloat(peer.Distance, 'f', 0, 64)
	case "uptime":
		return strconv.FormatInt(peer.Uptime, 10)

	// Traffic
	case "tx_bytes":
		return strconv.FormatInt(peer.TxBytes, 10)
	case "rx_bytes":
		return strconv.FormatInt(peer.RxBytes, 10)
	case "tx_rate":
		return strconv.FormatInt(peer.TxRate, 10)
	case "rx_rate":
		return strconv.FormatInt(peer.RxRate, 10)

	// 60GHz Signal (Wave) - ALWAYS separate from 5GHz
	case "signal.60ghz":
		if peer.Radio60GHz != nil {
			return strconv.Itoa(peer.Radio60GHz.Signal)
		}
		return "0"
	case "signal.60ghz.chain0":
		if peer.Radio60GHz != nil && len(peer.Radio60GHz.SignalPerChain) > 0 {
			return strconv.Itoa(peer.Radio60GHz.SignalPerChain[0])
		}
		return "0"
	case "signal.60ghz.chain1":
		if peer.Radio60GHz != nil && len(peer.Radio60GHz.SignalPerChain) > 1 {
			return strconv.Itoa(peer.Radio60GHz.SignalPerChain[1])
		}
		return "0"
	case "signal.60ghz.combined":
		if peer.Radio60GHz != nil && len(peer.Radio60GHz.SignalPerChain) >= 2 {
			return strconv.Itoa(combineSignals(peer.Radio60GHz.SignalPerChain))
		}
		if peer.Radio60GHz != nil {
			return strconv.Itoa(peer.Radio60GHz.Signal)
		}
		return "0"

	// 5GHz Signal (unified for Wave backup, AirMAX AC, LTU) - ALWAYS separate from 60GHz
	case "signal.5ghz":
		return strconv.Itoa(get5GHzSignal(peer))
	case "signal.5ghz.chain0":
		chains := get5GHzChains(peer)
		if len(chains) > 0 {
			return strconv.Itoa(chains[0])
		}
		return "0"
	case "signal.5ghz.chain1":
		chains := get5GHzChains(peer)
		if len(chains) > 1 {
			return strconv.Itoa(chains[1])
		}
		return "0"
	case "signal.5ghz.combined":
		chains := get5GHzChains(peer)
		if len(chains) >= 2 {
			return strconv.Itoa(combineSignals(chains))
		}
		return strconv.Itoa(get5GHzSignal(peer))

	// Legacy combined signal (deprecated - use signal.60ghz or signal.5ghz)
	case "signal":
		return strconv.Itoa(getPeerSignal(peer))
	case "signal.chain0":
		chains := getPeerSignalChains(peer)
		if len(chains) > 0 {
			return strconv.Itoa(chains[0])
		}
		return "0"
	case "signal.chain1":
		chains := getPeerSignalChains(peer)
		if len(chains) > 1 {
			return strconv.Itoa(chains[1])
		}
		return "0"

	// LTU CINR
	case "cinr.dl":
		if peer.RadioLTU != nil && peer.RadioLTU.CINR != nil {
			return strconv.Itoa(peer.RadioLTU.CINR.DL)
		}
		return "0"
	case "cinr.ul":
		if peer.RadioLTU != nil && peer.RadioLTU.CINR != nil {
			return strconv.Itoa(peer.RadioLTU.CINR.UL)
		}
		return "0"

	// MCS
	case "mcs.tx":
		if mcs := getPeerMCS(peer); mcs != nil {
			return strconv.Itoa(mcs.TxIdx)
		}
		return "0"
	case "mcs.rx":
		if mcs := getPeerMCS(peer); mcs != nil {
			return strconv.Itoa(mcs.RxIdx)
		}
		return "0"

	// Airtime
	case "airtime.dl":
		return strconv.FormatFloat(getPeerAirtime(peer, "dl"), 'f', 1, 64)
	case "airtime.ul":
		return strconv.FormatFloat(getPeerAirtime(peer, "ul"), 'f', 1, 64)

	// Link scores
	case "link_score.dl":
		if peer.LinkScore != nil {
			return strconv.Itoa(peer.LinkScore.DL)
		}
		return "0"
	case "link_score.ul":
		if peer.LinkScore != nil {
			return strconv.Itoa(peer.LinkScore.UL)
		}
		return "0"

	// Capacity
	case "capacity":
		if cap := getPeerCapacity(peer); cap != nil {
			return strconv.FormatInt(cap.Combined, 10)
		}
		return "0"

	default:
		return "ZBX_NOTSUPPORTED"
	}
}

// count returns counts of devices
func (b *Bridge) count(args []string) string {
	filter := ""
	if len(args) > 0 {
		filter = strings.ToLower(args[0])
	}

	online, offline, unknown, total := b.store.CountsByStatus()

	switch filter {
	case "online":
		return strconv.Itoa(online)
	case "offline":
		return strconv.Itoa(offline)
	case "unknown":
		return strconv.Itoa(unknown)
	case "ap", "aps":
		aps := b.store.ListAPs()
		return strconv.Itoa(len(aps))
	case "":
		return strconv.Itoa(total)
	default:
		return "ZBX_NOTSUPPORTED"
	}
}

// Helper functions
func boolToStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func getPeerSignal(peer *stats.PeerStats) int {
	if peer.Radio60GHz != nil && peer.Radio60GHz.Active {
		return peer.Radio60GHz.Signal
	}
	if peer.RadioLTU != nil {
		return peer.RadioLTU.Signal
	}
	if peer.Radio5GHz != nil {
		return peer.Radio5GHz.Signal
	}
	return 0
}

func getPeerSignalChains(peer *stats.PeerStats) []int {
	if peer.Radio5GHz != nil && len(peer.Radio5GHz.SignalPerChain) > 0 {
		return peer.Radio5GHz.SignalPerChain
	}
	if peer.RadioLTU != nil && len(peer.RadioLTU.SignalPerChain) > 0 {
		return peer.RadioLTU.SignalPerChain
	}
	return nil
}

// get5GHzSignal returns the 5GHz signal from any source (Wave backup, AirMAX, LTU)
func get5GHzSignal(peer *stats.PeerStats) int {
	// Wave 5GHz backup
	if peer.Radio5GHz != nil && peer.Radio5GHz.Signal != 0 {
		return peer.Radio5GHz.Signal
	}
	// LTU
	if peer.RadioLTU != nil && peer.RadioLTU.Signal != 0 {
		return peer.RadioLTU.Signal
	}
	// AirMAX AC
	if peer.Signal != 0 {
		return peer.Signal
	}
	return 0
}

// get5GHzChains returns per-chain 5GHz signals from any source
func get5GHzChains(peer *stats.PeerStats) []int {
	// Wave 5GHz backup
	if peer.Radio5GHz != nil && len(peer.Radio5GHz.SignalPerChain) > 0 {
		return peer.Radio5GHz.SignalPerChain
	}
	// LTU
	if peer.RadioLTU != nil && len(peer.RadioLTU.SignalPerChain) > 0 {
		return peer.RadioLTU.SignalPerChain
	}
	// AirMAX AC (uses 5GHz radio stats)
	if peer.Radio5GHz != nil && len(peer.Radio5GHz.SignalPerChain) > 0 {
		return peer.Radio5GHz.SignalPerChain
	}
	return nil
}

// combineSignals combines multiple chain signals using power addition formula
// Two -63dBm signals combine to -60dBm (3dB gain from MRC)
func combineSignals(chains []int) int {
	if len(chains) == 0 {
		return 0
	}
	if len(chains) == 1 {
		return chains[0]
	}

	// Filter out zero/invalid values
	var linearSum float64
	var validCount int
	for _, dBm := range chains {
		if dBm != 0 && dBm < 0 {
			// dBm to linear power: 10^(dBm/10)
			linearSum += math.Pow(10, float64(dBm)/10)
			validCount++
		}
	}

	if validCount == 0 {
		return 0
	}
	if validCount == 1 {
		// Return the single valid value
		for _, dBm := range chains {
			if dBm != 0 && dBm < 0 {
				return dBm
			}
		}
	}

	// Linear to dBm: 10 * log10(linear)
	return int(math.Round(10 * math.Log10(linearSum)))
}

func getPeerMCS(peer *stats.PeerStats) *stats.MCSStats {
	if peer.Radio60GHz != nil && peer.Radio60GHz.Active && peer.Radio60GHz.MCS != nil {
		return peer.Radio60GHz.MCS
	}
	if peer.RadioLTU != nil && peer.RadioLTU.MCS != nil {
		return peer.RadioLTU.MCS
	}
	if peer.Radio5GHz != nil && peer.Radio5GHz.MCS != nil {
		return peer.Radio5GHz.MCS
	}
	return nil
}

func getPeerAirtime(peer *stats.PeerStats, dir string) float64 {
	var radio *stats.PeerRadioStats
	if peer.Radio60GHz != nil && peer.Radio60GHz.Active {
		radio = peer.Radio60GHz
	} else if peer.RadioLTU != nil {
		radio = peer.RadioLTU
	} else if peer.Radio5GHz != nil {
		radio = peer.Radio5GHz
	}

	if radio == nil {
		return 0
	}

	if dir == "dl" {
		return radio.AirtimeDL
	}
	return radio.AirtimeUL
}

func getPeerCapacity(peer *stats.PeerStats) *stats.CapacityStats {
	if peer.Radio60GHz != nil && peer.Radio60GHz.Active && peer.Radio60GHz.Capacity != nil {
		return peer.Radio60GHz.Capacity
	}
	if peer.RadioLTU != nil && peer.RadioLTU.Capacity != nil {
		return peer.RadioLTU.Capacity
	}
	if peer.Radio5GHz != nil && peer.Radio5GHz.Capacity != nil {
		return peer.Radio5GHz.Capacity
	}
	return nil
}
