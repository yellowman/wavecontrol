import { api, auth, ws, sync } from './api.js?v=27'
import { store } from './store.js?v=15'
import { renderDevices, renderTree, renderLogs, renderDeviceDetail, renderDirectionalCell, showToast, updateWarningsPanel,
         showJobPanel, hideJobPanel, toggleJobPanel, updateJobProgress, updateJobStatus,
         addJobEvent, startTrackedJob, trackJob, getActiveJobCount, cleanupVirtualTable } from './components.js?v=64'
import { 
  wsBatcher, shouldUseVirtualTable, setUpdateCountsCallback, scrollToDeviceById 
} from './virtual-integration.js?v=11'

// Debounced renderTree - prevents excessive re-renders with many devices
let renderTreeTimeout = null
function debouncedRenderTree(filter = '') {
  clearTimeout(renderTreeTimeout)
  renderTreeTimeout = setTimeout(() => {
    renderTree(filter || store.treeFilter || '')
  }, 100)
}

// HTML escaping to prevent XSS from device-controlled fields
function escapeHTML(str) {
  if (str === null || str === undefined) return ''
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;')
}

// Escape for use in HTML attributes
function escapeAttr(str) {
  if (str === null || str === undefined) return ''
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
}

// Route UI actions through one delegated listener so generated markup never needs
// inline event handlers.  This keeps the script CSP meaningful and centralizes
// validation of data attributes before privileged actions are invoked.
function positiveInt(value) {
  const parsed = Number.parseInt(value, 10)
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : null
}

document.addEventListener('click', async (event) => {
  const target = event.target.closest('[data-wc-action]')
  if (!target) return

  const action = target.dataset.wcAction
  const deviceId = positiveInt(target.dataset.deviceId)
  const listId = positiveInt(target.dataset.listId)
  const hostId = positiveInt(target.dataset.hostId)

  event.preventDefault()

  switch (action) {
    case 'close-modal': {
      const modalId = target.dataset.modalId
      if (modalId) document.getElementById(modalId)?.classList.add('hidden')
      break
    }
    case 'delete-firmware':
      await window.deleteFirmware?.(target.dataset.reference || '', target.dataset.displayName || '')
      break
    case 'select-device':
      if (deviceId) window.selectDevice?.(deviceId)
      break
    case 'download-config':
      if (target.dataset.path) window.downloadConfig?.(target.dataset.path)
      break
    case 'restore-config':
      if (deviceId && target.dataset.path) await window.restoreConfig?.(deviceId, target.dataset.path)
      break
    case 'create-drilldown-list':
      await window.createDrilldownList?.()
      break
    case 'add-drilldown-host':
      await window.addDrilldownHost?.()
      break
    case 'edit-drilldown-list':
      if (listId) await window.editDrilldownList?.(listId, target.dataset.listName || '')
      break
    case 'delete-drilldown-list':
      if (listId) await window.deleteDrilldownList?.(listId)
      break
    case 'remove-drilldown-host':
      if (listId && hostId) await window.removeDrilldownHost?.(listId, hostId)
      break
    case 'open-device-ui':
      if (deviceId) {
        const opened = window.open(`/api/wavecontrol/open-ui?device_id=${deviceId}`, '_blank', 'noopener')
        if (opened) opened.opener = null
      }
      break
    case 'refresh-device':
      if (deviceId) await window.refreshDevice?.(deviceId)
      break
    case 'reboot-device':
      if (deviceId) await window.rebootDevice?.(deviceId)
      break
    case 'learn-replacement-mac':
      if (deviceId) await window.learnReplacementMAC?.(deviceId)
      break
    case 'show-upgrade-modal':
      if (deviceId) await window.showUpgradeModal?.(deviceId)
      break
    case 'silence-device': {
      const seconds = positiveInt(target.dataset.seconds)
      if (deviceId && seconds) await window.updateDeviceAlerting?.(deviceId, { silence_seconds: seconds })
      break
    }
    case 'clear-device-silence':
      if (deviceId) await window.updateDeviceAlerting?.(deviceId, { clear_silence: true })
      break
    case 'edit-alert-notes':
      if (deviceId) await window.editDeviceAlertNotes?.(deviceId)
      break
    case 'backup-device-config':
      if (deviceId) await window.backupDeviceConfig?.(deviceId)
      break
    case 'show-device-backups':
      if (deviceId) await window.showDeviceBackups?.(deviceId)
      break
  }
})

document.addEventListener('change', async (event) => {
  const target = event.target.closest('[data-wc-change]')
  if (!target) return

  const deviceId = positiveInt(target.dataset.deviceId)
  const listId = positiveInt(target.dataset.listId)
  switch (target.dataset.wcChange) {
    case 'toggle-alertable':
      if (deviceId) await window.updateDeviceAlerting?.(deviceId, { alertable: Boolean(target.checked) })
      break
    case 'toggle-drilldown-list':
      if (listId) await window.toggleDrilldownList?.(listId, Boolean(target.checked))
      break
  }
})

// Convert JSON password array to one-per-line for display
function parsePasswordsForDisplay(jsonStr) {
  try {
    const arr = JSON.parse(jsonStr || '[]')
    return Array.isArray(arr) ? arr.join('\n') : ''
  } catch {
    return ''
  }
}

// Convert one-per-line passwords to JSON array for storage
function passwordsToJSON(text) {
  const lines = text.split('\n').map(l => l.trim()).filter(l => l.length > 0)
  return JSON.stringify(lines)
}

// Parse JSON array of prefixes for display (one per line)
function parsePrefixesForDisplay(jsonStr) {
  try {
    const arr = JSON.parse(jsonStr || '[]')
    return Array.isArray(arr) ? arr.join('\n') : ''
  } catch {
    return ''
  }
}

// Convert one-per-line prefixes to JSON array for storage
function prefixesToJSON(text) {
  const lines = text.split('\n').map(l => l.trim()).filter(l => l.length > 0)
  return JSON.stringify(lines)
}

// Scroll to device in table and highlight
function scrollToDevice(id) {
  // Try virtual table first (for large datasets)
  if (scrollToDeviceById(id)) {
    return
  }
  
  // Fall back to regular table row
  const row = document.querySelector(`tr[data-id="${id}"]`)
  if (row) {
    row.scrollIntoView({ behavior: 'smooth', block: 'center' })
    row.classList.add('highlighted')
    setTimeout(() => row.classList.remove('highlighted'), 2000)
  }
}

// Initialize theme
function initTheme() {
  const saved = localStorage.getItem('theme') || 'dark'
  document.documentElement.dataset.theme = saved
}
initTheme()

// Theme toggle
document.getElementById('themeToggle')?.addEventListener('click', () => {
  const html = document.documentElement
  const next = html.dataset.theme === 'dark' ? 'light' : 'dark'
  html.dataset.theme = next
  localStorage.setItem('theme', next)
})

// Jobs panel toggle
document.getElementById('jobPanelBtn')?.addEventListener('click', () => {
  toggleJobPanel()
})

// Update job badge count
function updateJobBadge() {
  const count = getActiveJobCount()
  const badge = document.getElementById('jobBadge')
  const btn = document.getElementById('jobPanelBtn')
  if (badge) {
    badge.textContent = count > 0 ? count : ''
  }
  if (btn) {
    btn.classList.toggle('has-active', count > 0)
  }
}

// Listen for job changes from components.js
window.addEventListener('jobsChanged', updateJobBadge)

// Update body class based on auth state
function updateAuthClass() {
  if (store.user) {
    document.body.classList.remove('not-authed')
  } else {
    document.body.classList.add('not-authed')
  }
}

// Update nav visibility based on user role
function updateNavVisibility() {
  const isAdmin = store.user?.roles?.includes('administrator')
  const settingsLink = document.querySelector('a[data-page="settings"]')
  if (settingsLink) {
    settingsLink.style.display = isAdmin ? '' : 'none'
  }
}

// Resume session
async function init() {
  try {
    // The server-side HttpOnly cookie is the sole source of session truth.
    const me = await api.me({ handleAuthFailure: false })
    const isAdmin = me?.roles?.includes('administrator')
    const [devices, settings] = await Promise.all([
      api.devices(),
      isAdmin ? api.settings().catch(() => ({})) : Promise.resolve({})
    ])
    store.set({
      user: me,
      settings: settings || {},
      devices: Array.isArray(devices) ? devices : [],
      currentPage: 'dashboard'
    })
    updateNavVisibility()
    updateCounts()
    renderTree()
    renderCurrentPage()
    refreshUltraDebugBadge()
    refreshAlertNavBadge()
    ws.connect()
    ws.startPing()
    lastFullReconcileAt = Date.now()
    setUpdateCountsCallback(updateCounts)
  } catch (e) {
    auth.clear()
    store.set({ user: null })
    showLoginPage()
  }
  updateAuthClass()
}

// Refresh device list from API
async function refreshDevices() {
  try {
    const devices = await api.devices()
    store.set({ devices: Array.isArray(devices) ? devices : [] })
    lastFullReconcileAt = Date.now()
    updateCounts()
    renderTree()
    scheduleAssociationsRefresh('refreshDevices')
  } catch (e) {
    console.error('Failed to refresh devices:', e)
  }
}

// Track last time we received a realtime update via WebSocket.
// Used to avoid disruptive full refresh polls while WS is healthy.
let lastRealtimeUpdateAt = 0

const deviceRealtimeAt = new Map()
let lastFullReconcileAt = 0
let authSyncInFlight = false
let selectedDeviceSyncInFlight = false

const FORCE_FULL_RECONCILE_MS = 120000
const SELECTED_DEVICE_SYNC_MS = 30000
const WS_SILENT_RECONNECT_MS = 70000
const AUTH_SYNC_CHECK_MS = 60000
const AUTH_STALE_CHECK_MS = 180000
const AUTO_LOGOUT_STALE_MS = 360000

function markDeviceRealtime(deviceId) {
  if (deviceId !== undefined && deviceId !== null) {
    deviceRealtimeAt.set(deviceId, Date.now())
  }
}

function getLastServerSyncAt() {
  const s = sync.getState()
  return Math.max(s.lastHttpAt || 0, s.lastWsAt || 0, s.lastPongAt || 0, s.lastAuthOkAt || 0)
}

function forceClientLogout(reason = 'Session is no longer synchronized with the server') {
  try { ws.disconnect() } catch (_) {}
  auth.clear()
  store.set({ user: null, devices: [], selectedDevice: null })
  updateAuthClass()
  showLoginPage()
  try { showToast(reason, 'warning') } catch (_) {}
}

window.addEventListener('wavecontrol-auth-failure', () => {
  if (store.user) forceClientLogout('Session ended. Please sign in again.')
})

async function reconcileSelectedDevice(force = false) {
  if (selectedDeviceSyncInFlight || !store.user || !networkOk) return
  const detailPanel = document.getElementById('detailPanel')
  if (!detailPanel || detailPanel.classList.contains('hidden')) return
  const selectedId = store.selectedDevice
  if (!selectedId) return

  const lastForDevice = deviceRealtimeAt.get(selectedId) || 0
  if (!force && lastForDevice && (Date.now() - lastForDevice) < SELECTED_DEVICE_SYNC_MS) return

  selectedDeviceSyncInFlight = true
  try {
    const fresh = await api.device(selectedId)
    if (!fresh || fresh.id === undefined || fresh.id === null) return
    const current = store.getDeviceById(fresh.id)
    if (!current) return

    const oldIp = current.ip_address
    const oldParent = current.parent_id
    Object.assign(current, fresh)
    if (fresh.ip_address && fresh.ip_address !== oldIp) store.reindexDevices()
    if ((oldParent || null) !== (fresh.parent_id || null)) {
      store.invalidateChildIndex()
      renderTree()
    }
    updateDeviceRow(current.id, current.ip_address || oldIp, fresh)
    updateCounts()
    updateDetailPanel(current)
    scheduleAssociationsRefresh('selected_device_sync')
    markDeviceRealtime(current.id)
  } catch (e) {
    console.warn('selected device reconcile failed', e)
  } finally {
    selectedDeviceSyncInFlight = false
  }
}

async function runSyncWatchdog() {
  if (!store.user) return

  if (auth.isExpired()) {
    forceClientLogout('Session expired. Please sign in again.')
    return
  }

  const now = Date.now()
  const lastSyncAt = getLastServerSyncAt()
  const syncState = sync.getState()
  const wsOpen = ws?.socket && ws.socket.readyState === WebSocket.OPEN

  // If the socket looks connected but has gone silent, force a reconnect.
  const lastWsSignal = Math.max(syncState.lastWsAt || 0, syncState.lastPongAt || 0, syncState.wsConnectedAt || 0)
  if (wsOpen && lastWsSignal && (now - lastWsSignal) > WS_SILENT_RECONNECT_MS) {
    console.warn('WebSocket appears stale; reconnecting')
    try { ws.disconnect() } catch (_) {}
    ws.connect()
    ws.startPing()
  }

  // Periodically revalidate auth when we have not heard from the server recently.
  if (!authSyncInFlight && (!lastSyncAt || (now - lastSyncAt) > AUTH_STALE_CHECK_MS)) {
    authSyncInFlight = true
    try {
      sync.markAuthCheck()
      await api.me()
      sync.markAuthOK()
    } catch (e) {
      // request() handles 401 by redirecting; only keep our own fallback for non-401 cases.
      console.warn('auth sync check failed', e)
    } finally {
      authSyncInFlight = false
    }
  }

  // If we are still not synchronized after retries, fail closed.
  if (networkOk && lastSyncAt && (now - lastSyncAt) > AUTO_LOGOUT_STALE_MS) {
    forceClientLogout('This tab lost synchronization with the server. Please sign in again.')
    return
  }

  // Keep the open detail panel converged even if one device misses a WS patch.
  await reconcileSelectedDevice(false)
}

// Handle websocket messages
ws.on(msg => {
  // Only log non-stats messages to reduce console spam
  if (msg.type !== 'stats_update') {
    console.log('WS received:', msg.type, msg)
  }

  // Record realtime device updates (used by pollDevices to avoid disruptive refreshes)
  if (msg && (msg.type === 'stats_update' || msg.type === 'device_update' || msg.type === 'device_add')) {
    lastRealtimeUpdateAt = Date.now()
  }
  try {
    switch (msg.type) {
      case 'pong':
        // Keepalive acknowledgement; sync tracking is handled in api.js.
        break

      case 'stats_update':
        // Update device in store with new stats
        // Identity: prefer device_id, then device_mac, then device_ip (fallback only)
        const rawStats = msg.data
        const deviceId = msg.device_id
        const deviceMac = msg.device_mac
        const deviceIp = msg.device_ip
        
        // Safety check - skip if no data or no usable identity
        if (!rawStats || (!deviceId && !deviceMac && !deviceIp)) {
          console.warn('WS stats_update missing data or device identity (device_id/device_mac/device_ip)')
          break
        }
        
        // Flatten stats to match device format expected by table.
        // IMPORTANT: treat stats_update as a *patch*. Many error/edge-case updates only
        // include a handful of fields. Never overwrite existing device fields with
        // undefined.
        const updatedDevice = {
          uptime: rawStats.uptime,
          power_time: rawStats.power_time,
          peer_count: rawStats.peer_count,
          temperature: rawStats.temperature?.cpu,
          cpu: rawStats.cpu,
          ram: rawStats.ram,
          interfaces: rawStats.interfaces,
          network: rawStats.network,
          live_stats: rawStats
        }

        // Status normalization: backend realtime stats primarily uses `status` (and often omits `db_status`).
        // The UI and counts prefer `db_status`, so mirror status -> db_status for consistency.
        const statusStr =
          (typeof rawStats.db_status === 'string' && rawStats.db_status) ? rawStats.db_status :
          (typeof rawStats.status === 'string' && rawStats.status) ? rawStats.status :
          (typeof rawStats.online === 'boolean') ? (rawStats.online ? 'online' : 'offline') :
          null

        if (statusStr) {
          updatedDevice.status = statusStr
          updatedDevice.db_status = statusStr
        }

        // Preserve direction when possible; fall back from statusStr if online is not present.
        if (typeof rawStats.online === 'boolean') {
          updatedDevice.online = rawStats.online
        } else if (statusStr) {
          updatedDevice.online = statusStr === 'online'
        }

        if (Object.prototype.hasOwnProperty.call(rawStats, 'status_reason')) updatedDevice.status_reason = rawStats.status_reason || ''
        if (Object.prototype.hasOwnProperty.call(rawStats, 'db_status_reason')) updatedDevice.db_status_reason = rawStats.db_status_reason || ''
        if (Object.prototype.hasOwnProperty.call(rawStats, 'identity_mismatch')) updatedDevice.identity_mismatch = rawStats.identity_mismatch || null
        if (rawStats.mac) updatedDevice.mac = rawStats.mac
        if (rawStats.last_error) updatedDevice.last_error = rawStats.last_error

        // Keep "Last Seen" fresh in the host panel and any tables that display it.
        // DeviceStats.last_seen is typically an ISO timestamp string; some error cases may
        // only include a unix `timestamp`.
        if (rawStats.last_seen) {
          updatedDevice.last_seen = rawStats.last_seen
        } else if (typeof rawStats.timestamp === 'number') {
          updatedDevice.last_seen = new Date(rawStats.timestamp * 1000).toISOString()
        } else if (typeof msg.timestamp === 'number') {
          // Fall back to the websocket envelope timestamp if nothing else is available.
          updatedDevice.last_seen = new Date(msg.timestamp * 1000).toISOString()
        }
        if (typeof rawStats.last_seen_ts === 'number') updatedDevice.last_seen_ts = rawStats.last_seen_ts

        // Remove any undefined keys so we never clobber existing device state.
        for (const k in updatedDevice) {
          if (updatedDevice[k] === undefined) delete updatedDevice[k]
        }
        
        // Only update hostname if websocket provides one (don't overwrite DB value with empty)
        if (rawStats.hostname) {
          updatedDevice.hostname = rawStats.hostname
        }
        
        // Flatten GPS
        if (rawStats.gps?.fix && (rawStats.gps.lat || rawStats.gps.lon)) {
          updatedDevice.gps_lat = rawStats.gps.lat
          updatedDevice.gps_lon = rawStats.gps.lon
        }
        
        // Flatten radio stats and add full radio objects for detail panel
        // Use signal_combined (MRC computed server-side) for sorting/display
        if (rawStats.wireless?.radio_60ghz) {
          const r60 = rawStats.wireless.radio_60ghz
          updatedDevice.signal_60ghz = r60.signal_combined || r60.signal || null
          updatedDevice.radio_60ghz = r60
          if (r60.capacity) {
            updatedDevice.capacity_60ghz = r60.capacity.combined
          }
        }
        if (rawStats.wireless?.radio_5ghz) {
          const r5 = rawStats.wireless.radio_5ghz
          updatedDevice.signal_5ghz = r5.signal_combined || r5.signal || null
          updatedDevice.radio_5ghz = r5
          if (r5.capacity) {
            updatedDevice.capacity_5ghz = r5.capacity.combined
          }
          if (r5.signal_per_chain) {
            updatedDevice.signal_per_chain = r5.signal_per_chain
          }
        }
        if (rawStats.wireless?.radio_ltu) {
          const rLtu = rawStats.wireless.radio_ltu
          updatedDevice.radio_ltu = rLtu
          // Use server-computed signal values (Go handles RSSI-to-dBm conversion and MRC combining)
          // signal_combined is pre-computed server-side, never compute MRC in JavaScript
          updatedDevice.signal_ltu = rLtu.signal_combined || rLtu.signal || null
          if (rLtu.capacity) {
            updatedDevice.capacity_ltu = rLtu.capacity.combined
          }
        }
        
        // Service uptime from wireless
        if (rawStats.wireless?.service_uptime) {
          updatedDevice.service_uptime = rawStats.wireless.service_uptime
        }
        
        // TX/RX rates
        if (rawStats.wireless?.tx_rate) {
          updatedDevice.tx_rate = rawStats.wireless.tx_rate
        }
        if (rawStats.wireless?.rx_rate) {
          updatedDevice.rx_rate = rawStats.wireless.rx_rate
        }
        
        // Link score
        if (rawStats.wireless?.link_score) {
          updatedDevice.link_score = rawStats.wireless.link_score
        }
        
        // Wireless config (waveai, auto freq, frame mode, etc)
        if (rawStats.config) {
          updatedDevice.config = rawStats.config
        }
        
        // Only update if we have devices loaded
        if (!store.devices || store.devices.length === 0) {
          console.warn('WS stats_update: no devices in store yet')
          break
        }
        
        // Resolve target device in store.
        // Prefer device_id/device_mac (authoritative). IP is fallback only.
        const macNorm = deviceMac ? String(deviceMac).toLowerCase() : ''
        let target = null
        if (deviceId) target = store.getDeviceById(deviceId)
        if (!target && macNorm) target = store.getDeviceByMac(macNorm)
        if (!target && deviceIp) target = store.getDeviceByIp(deviceIp)

        // If we matched by id but MAC disagrees, prefer the MAC match.
        // This prevents stale rows from being updated when an IP is reused for a different device.
        if (target && macNorm && target.mac && String(target.mac).toLowerCase() !== macNorm) {
          const byMac = store.getDeviceByMac(macNorm)
          if (byMac) target = byMac
        }

        // If this is an AP update, propagate peer-derived updates to STA rows.
        // This keeps STA metrics/status fresh in real time without requiring
        // a full /api/devices refresh (which would reset the virtual scroll).
        const applyAPPeerUpdates = (apDevice, useVT) => {
          if (!apDevice || apDevice.role !== 'ap') return

          const apStatus = (typeof rawStats.status === 'string' && rawStats.status)
            ? rawStats.status
            : (rawStats.online === true ? 'online' : (rawStats.db_status === 'offline' ? 'offline' : 'unknown'))

          const peers = Array.isArray(rawStats.peers) ? rawStats.peers : null
          const peerCount = (typeof rawStats.peer_count === 'number') ? rawStats.peer_count : null

          // Nothing to do if we have no peer info and the AP is online.
          if (!peers && peerCount === null && apStatus === 'online') return

          const peerMacs = new Set()

          // Update STAs present in the peer list
          if (peers) {
            for (const peer of peers) {
              if (!peer) continue
              const peerMac = peer.mac ? String(peer.mac).toLowerCase() : ''
              if (!peerMac) continue

              peerMacs.add(peerMac)

              const sta = store.getDeviceByMac(peerMac)
              if (!sta) continue

              const staUpdate = {
                online: true,
                status_reason: null,
              }

              // Basic metrics
              if (typeof peer.distance === 'number') staUpdate.distance = peer.distance
              if (typeof peer.tx_rate === 'number') staUpdate.tx_rate = peer.tx_rate
              if (typeof peer.rx_rate === 'number') staUpdate.rx_rate = peer.rx_rate
              if (typeof peer.uptime === 'number') staUpdate.uptime = peer.uptime
              if (typeof peer.power_time === 'number') staUpdate.power_time = peer.power_time
              if (typeof peer.service_uptime === 'number') staUpdate.service_uptime = peer.service_uptime
              if (typeof peer.net_mode === 'string') staUpdate.net_mode = peer.net_mode
              if (typeof peer.carrier_drop === 'number') staUpdate.carrier_drop = peer.carrier_drop

              // AirMAX-style signal fields
              if (typeof peer.signal === 'number') staUpdate.signal_airmax = peer.signal
              if (typeof peer.remote_signal === 'number') staUpdate.remote_signal = peer.remote_signal
              if (typeof peer.remote_noise_floor === 'number') staUpdate.remote_noise_floor = peer.remote_noise_floor
              if (Array.isArray(peer.remote_signal_per_chain)) staUpdate.remote_signal_per_chain = peer.remote_signal_per_chain

              // GPS
              if (peer.gps) {
                if (typeof peer.gps.lat === 'number') staUpdate.gps_lat = peer.gps.lat
                if (typeof peer.gps.lon === 'number') staUpdate.gps_lon = peer.gps.lon
              }

              // Radio metrics
              const r60 = peer.radio_60ghz
              if (r60) {
                staUpdate.radio_60ghz = r60
                const s = (typeof r60.signal_combined === 'number' && r60.signal_combined !== 0) ? r60.signal_combined : r60.signal
                if (typeof s === 'number') staUpdate.signal_60ghz = s
                if (r60.capacity && typeof r60.capacity.combined === 'number') staUpdate.capacity_60ghz = r60.capacity.combined
              }

              const r5 = peer.radio_5ghz
              if (r5) {
                staUpdate.radio_5ghz = r5
                const s = (typeof r5.signal_combined === 'number' && r5.signal_combined !== 0) ? r5.signal_combined : r5.signal
                if (typeof s === 'number') staUpdate.signal_5ghz = s
                if (r5.capacity && typeof r5.capacity.combined === 'number') staUpdate.capacity_5ghz = r5.capacity.combined
              }

              const rltu = peer.radio_ltu
              if (rltu) {
                staUpdate.radio_ltu = rltu
                const s = (typeof rltu.signal_combined === 'number' && rltu.signal_combined !== 0) ? rltu.signal_combined : rltu.signal
                if (typeof s === 'number') staUpdate.signal_ltu = s
                if (rltu.capacity && typeof rltu.capacity.combined === 'number') staUpdate.capacity_ltu = rltu.capacity.combined
              }

              Object.assign(sta, staUpdate)
              if (useVT) {
                wsBatcher.add(sta.id, staUpdate)
              } else {
                updateDeviceRow(sta.id, sta.ip_address, staUpdate)
              }

                markDeviceRealtime(sta.id)
              if (store.selectedDevice && sta.id === store.selectedDevice) {
                updateDetailPanel(sta)
              }
            }
          }

          // Mark child STAs not currently associated
          const shouldMarkMissing = !!peers || peerCount === 0 || apStatus !== 'online'
          if (!shouldMarkMissing) return

          const children = store.getSTAs(apDevice.id) || []
          for (const sta of children) {
            const m = sta.mac ? String(sta.mac).toLowerCase() : ''
            if (m && peerMacs.has(m)) continue

            // If we don't have the peer list and peer_count isn't explicitly 0, avoid guessing.
            if (!peers && peerCount !== 0 && apStatus === 'online') continue

            const missingUpdate = { online: false }
            if (apStatus === 'online') {
              missingUpdate.db_status = 'offline'
              missingUpdate.status_reason = 'not_associated'
            } else {
              missingUpdate.db_status = 'unknown'
              missingUpdate.status_reason = `parent_${apStatus}`
            }

            Object.assign(sta, missingUpdate)
            if (useVT) {
              wsBatcher.add(sta.id, missingUpdate)
            } else {
              updateDeviceRow(sta.id, sta.ip_address, missingUpdate)
            }

            markDeviceRealtime(sta.id)
            if (store.selectedDevice && sta.id === store.selectedDevice) {
              updateDetailPanel(sta)
            }
          }
        }

        // Fast path for large datasets: update the device object in place (O(1)) and let
        // the virtual table batcher handle DOM updates. Avoid store.set() per message.
        if (shouldUseVirtualTable()) {
          if (target) {
            Object.assign(target, updatedDevice)
            markDeviceRealtime(target.id)
            // Batch DOM updates - flushes after 100ms
            wsBatcher.add(target.id, updatedDevice)

            // If this is an AP update, also update child STAs from the peer list
            applyAPPeerUpdates(target, true)

            // Incremental update of detail panel if this device is selected
            if (store.selectedDevice && target.id === store.selectedDevice) {
              updateDetailPanel(target)
            }
	          }
	          // Derived views (e.g., Quality) are not row-based; ensure they refresh.
	          scheduleAssociationsRefresh('stats_update')
          break
        }

        // Small dataset: keep existing reactive update behavior.
        if (target) {
          Object.assign(target, updatedDevice)
          markDeviceRealtime(target.id)
          // Trigger store listeners (counts, detail panel) with minimal allocations.
          store.set({ devices: store.devices })

          // Apply peer-derived updates for APs (small dataset mode)
          applyAPPeerUpdates(target, false)
        } else {
          // Fallback - should be rare (e.g., device list not refreshed yet)
          const devices = store.devices.map(d => {
            if (deviceId && d.id === deviceId) return { ...d, ...updatedDevice }
            if (!deviceId && macNorm && String(d.mac || '').toLowerCase() === macNorm) return { ...d, ...updatedDevice }
            if (!deviceId && !macNorm && deviceIp && d.ip_address === deviceIp) return { ...d, ...updatedDevice }
            return d
          })
          store.set({ devices })
          // Try to resolve target after update
          if (deviceId) target = store.getDeviceById(deviceId)
          else if (macNorm) target = store.getDeviceByMac(macNorm)
          else if (deviceIp) target = store.getDeviceByIp(deviceIp)

          if (target) applyAPPeerUpdates(target, false)
        }

        // Incremental DOM update - only update the row that changed
        const rowId = target ? target.id : deviceId
        updateDeviceRow(rowId, deviceIp, updatedDevice)
        // Update counts (cheap operation)
	        updateCounts()
	        // Derived views (e.g., Quality) are not row-based; ensure they refresh.
	        scheduleAssociationsRefresh('stats_update')

        // Incremental update of detail panel if this device is selected
        if (store.selectedDevice && target && target.id === store.selectedDevice) {
          updateDetailPanel(target)
        }
        
        // For map page with GPS changes, just update the marker position (not full re-render)
        // Map updates are handled separately via updateMapMarker if needed
        break
      
    case 'device_update': {
      // Poller-driven metadata updates (e.g., STA roaming parent changes).
      // Historically this handler re-fetched /api/devices, which is extremely expensive
      // on large installs and can trigger HTTP rate limiting. Prefer applying the patch
      // in-place when the message contains an id.

      const patch = msg.data || {}

      // If we don't have an id, fall back to a full refresh (should be rare).
      if (!patch || !patch.id) {
        api.devices().then(newDevices => {
          const devices = Array.isArray(newDevices) ? newDevices : []
          store.set({ devices })
          debouncedRenderTree()
          if (['dashboard', 'devices', 'drilldown'].includes(store.currentPage)) {
            renderCurrentPage()
          }
          updateCounts()
        })
        break
      }

      const dev = store.getDeviceById(patch.id)
      if (!dev) {
        // Unknown id - refresh once to resync.
        api.devices().then(newDevices => {
          const devices = Array.isArray(newDevices) ? newDevices : []
          store.set({ devices })
          debouncedRenderTree()
          if (['dashboard', 'devices', 'drilldown'].includes(store.currentPage)) {
            renderCurrentPage()
          }
          updateCounts()
        })
        break
      }

      const oldIp = dev.ip_address
      const oldParent = dev.parent_id
      const oldHostname = dev.hostname
      const oldSite = dev.site_name

      Object.assign(dev, patch)
      markDeviceRealtime(dev.id)

      // Keep STA child index accurate for the Quality page and other hierarchy views
      if (patch.parent_id !== undefined && patch.parent_id !== oldParent) {
        store.invalidateChildIndex()
      }

      const hierarchyChanged =
        (patch.parent_id !== undefined && patch.parent_id !== oldParent) ||
        (patch.hostname !== undefined && patch.hostname !== oldHostname) ||
        (patch.site_name !== undefined && patch.site_name !== oldSite)

      if (shouldUseVirtualTable()) {
        // Keep IP index correct if the key changed (rare)
        if (patch.ip_address && patch.ip_address !== oldIp) {
          store.reindexDevices()
        }
        // Update any visible row without re-rendering the entire table
        wsBatcher.add(dev.id, patch)
        if (hierarchyChanged) debouncedRenderTree()
        if (store.selectedDevice && dev.id === store.selectedDevice) {
          updateDetailPanel(dev)
        }
        // Derived views (e.g., Quality) may need a re-render even when using virtual tables.
        scheduleAssociationsRefresh('device_update')
        break
      }

      // Non-virtual table: notify listeners + update the specific row.
      store.set({ devices: store.devices })
      updateDeviceRow(dev.id, dev.ip_address || oldIp, patch)
      if (hierarchyChanged) debouncedRenderTree()
      if (store.currentPage === 'drilldown') {
        renderDrilldownBody()
      }
      scheduleAssociationsRefresh('device_update')
      break
    }
      
    case 'device_add':
      // New device discovered - add to list and refresh
      const newDevice = msg.data
      if (newDevice) {
        const currentDevices = store.devices
        
        // Check if already exists (by MAC)
        if (!currentDevices.some(d => d.mac === newDevice.mac)) {
          store.set({ devices: [...currentDevices, newDevice] })
          renderTree()
          
          // Show toast notification
          const name = newDevice.hostname || newDevice.ip_address || newDevice.mac
          showToast(`Discovered new STA: ${name}`, 'info')
          
          // Update counts
          updateCounts()
        }
      }
      break
      
    case 'job_update':
      // Update job panel with status change
      console.log('WS job_update:', msg.data)
      if (msg.data) {
        updateJobStatus(msg.data)
        updateJobBadge()
        const status = msg.data.status
        const jobId = msg.data.job_id
        // Also show toast for key status changes
        if (status === 'completed') {
          showToast(`Job completed`, 'success')
        } else if (status === 'failed') {
          showToast(`Job failed: ${msg.data.error_message || 'Unknown error'}`, 'error')
        }
      }
      break
      
    case 'job_progress':
      // Update progress bar in job panel
      console.log('WS job_progress:', msg.data)
      if (msg.data) {
        updateJobProgress(msg.data)
        updateJobBadge()
      }
      break
      
    case 'job_event':
      // Add event to job's event log
      console.log('WS job_event:', msg.data)
      if (msg.data) {
        addJobEvent(msg.data)
      }
      break
  }
  } catch (e) {
    console.error('WebSocket handler error:', e)
  }
})

// Incremental update of a single device row - surgically updates DOM without re-render
// Prefer updating by unique device id; fall back to ip only when no id exists.
function updateDeviceRow(id, ip, data) {
  const statusVal = data.status || (data.online === true ? 'online' : (data.online === false ? 'offline' : 'unknown'))

  // Prefer the canonical device object from the store for derived UI (e.g. directional diagnosis)
  const fullDevice = (id !== undefined && id !== null) ? store.getDeviceById(id) : (ip ? store.getDeviceByIp(ip) : null)

  // Update tree nodes
  const treeSelector = (id !== undefined && id !== null) ? `.tree-node[data-id="${id}"]` : (ip ? `.tree-node[data-ip="${ip}"]` : null)
  if (treeSelector) {
    document.querySelectorAll(treeSelector).forEach(node => {
    const statusDot = node.querySelector('.tree-status')
    if (statusDot) {
			const newStatus = statusVal
      if (!statusDot.classList.contains(newStatus)) {
        statusDot.className = `tree-status ${newStatus}`
      }
    }
    // Update tree label if hostname provided
    if (data.hostname) {
      const label = node.querySelector('.tree-label')
      if (label && label.textContent !== data.hostname) {
        label.textContent = data.hostname
      }
    }
	})
  }
  
  // Update table rows
  const rowSelector = (id !== undefined && id !== null) ? `tr[data-id="${id}"]` : (ip ? `tr[data-ip="${ip}"]` : null)
  if (!rowSelector) return
  document.querySelectorAll(rowSelector).forEach(row => {
    // Update status dot
    const statusDot = row.querySelector('.status-dot')
    if (statusDot) {
		  const newStatus = statusVal
      if (!statusDot.classList.contains(newStatus)) {
        statusDot.className = `status-dot ${newStatus}`
      }
    }
    
    // Update device name if hostname provided
    if (data.hostname) {
      const nameEl = row.querySelector('.device-name')
      if (nameEl && nameEl.textContent !== data.hostname) {
        nameEl.textContent = data.hostname
      }
    }
    
    // Update 60GHz signal cell - prefer server-computed quality
    const signal60Cell = row.querySelector('.cell-signal-60')
    if (signal60Cell) {
      const sig60 = data.signal_60ghz
      if (typeof sig60 === 'number' && sig60 !== 0) {
        const newText = `${sig60} dBm`
        const quality = data.radio_60ghz?.signal_quality
        const cls = quality ? `signal-${quality}` : getSignalClass60(sig60)
        const newClass = `cell-signal cell-signal-60 ${cls}`
        if (signal60Cell.textContent !== newText) signal60Cell.textContent = newText
        if (signal60Cell.className !== newClass) signal60Cell.className = newClass
      }
    }
    
    // Update 5GHz combined signal cell - prefer server-computed quality
    const signal5Cell = row.querySelector('.cell-signal-5ghz')
    if (signal5Cell) {
      const sig5 = get5GHzCombined(data)
      if (sig5 && sig5 !== 0) {
        const newText = `${sig5} dBm`
        const quality = data.radio_5ghz?.signal_quality || data.radio_ltu?.signal_quality
        const cls = quality ? `signal-${quality}` : getSignalClass5(sig5)
        const newClass = `cell-signal cell-signal-5ghz ${cls}`
        if (signal5Cell.textContent !== newText) signal5Cell.textContent = newText
        if (signal5Cell.className !== newClass) signal5Cell.className = newClass
      }
    }
    
    // Update 5GHz chain cells (no server quality for per-chain)
    const c0Cell = row.querySelector('.cell-signal-c0')
    const c1Cell = row.querySelector('.cell-signal-c1')
    const chains = get5GHzChains(data)
    if (c0Cell && typeof chains[0] === 'number' && chains[0] !== 0) {
      const newText = `${chains[0]}`
      const newClass = `cell-signal cell-signal-c0 ${getSignalClass5(chains[0])}`
      if (c0Cell.textContent !== newText) c0Cell.textContent = newText
      if (c0Cell.className !== newClass) c0Cell.className = newClass
    }
    if (c1Cell && typeof chains[1] === 'number' && chains[1] !== 0) {
      const newText = `${chains[1]}`
      const newClass = `cell-signal cell-signal-c1 ${getSignalClass5(chains[1])}`
      if (c1Cell.textContent !== newText) c1Cell.textContent = newText
      if (c1Cell.className !== newClass) c1Cell.className = newClass
    }
    
    // Update health bars (based on primary signal)
    const healthCell = row.querySelector('.cell-health')
    if (healthCell) {
      const primarySignal = data.signal_60ghz || get5GHzCombined(data) || 0
      const band = data.signal_60ghz ? '60ghz' : '5ghz'
      if (primarySignal) {
        healthCell.innerHTML = getSignalBarsHTML(primarySignal, band)
      }
    }
    
    // Update distance
    const distCell = row.querySelector('.cell-distance')
    if (distCell && data.distance) {
      const newText = `${(data.distance / 1000).toFixed(2)} km`
      if (distCell.textContent !== newText) distCell.textContent = newText
    }
    
    // Update capacity
    const capCell = row.querySelector('.cell-capacity')
    const cap = data.capacity_60ghz || data.capacity_ltu || data.capacity_5ghz
    if (capCell && cap) {
      const newText = `${(cap / 1e6).toFixed(0)} Mbps`
      if (capCell.textContent !== newText) capCell.textContent = newText
    }
    
    // Update peer count
    const peerBadge = row.querySelector('.peer-count')
    if (peerBadge && data.peer_count !== undefined) {
      const newText = String(data.peer_count || '')
      if (peerBadge.textContent !== newText) {
        peerBadge.textContent = newText
        peerBadge.style.display = data.peer_count ? '' : 'none'
      }
    }

    // Update directional diagnosis (derived)
    if (store.columns.dir && fullDevice) {
      const dirCell = row.querySelector('.cell-dir')
      if (dirCell) {
        dirCell.innerHTML = renderDirectionalCell(fullDevice)
      }
    }
  })

  // If a STA changed, refresh the parent's directional summary (if visible)
  if (store.columns.dir && fullDevice && fullDevice.parent_id) {
    const parent = store.getDeviceById(fullDevice.parent_id)
    if (parent) {
      document.querySelectorAll(`tr[data-id="${parent.id}"]`).forEach(pRow => {
        const dirCell = pRow.querySelector('.cell-dir')
        if (dirCell) {
          dirCell.innerHTML = renderDirectionalCell(parent)
        }
      })
    }
  }
}

// Generate signal bars HTML for health column
function getSignalBarsHTML(level, band = '5ghz') {
  if (!level) return '<div class="signal-bars"></div>'
  const t = SIGNAL_THRESHOLDS[band] || SIGNAL_THRESHOLDS['5ghz']
  // 5 bars: excellent (>good+5), very good (>good), good (>good-5), fair (>fair), poor
  let bars = 0
  if (level >= t.good + 5) bars = 5
  else if (level >= t.good) bars = 4
  else if (level >= t.good - 5) bars = 3
  else if (level >= t.fair) bars = 2
  else bars = 1
  const cls = bars >= 4 ? 'excellent' : (bars >= 3 ? 'good' : (bars >= 2 ? 'fair' : 'poor'))
  return `<div class="signal-bars ${cls}">${[1,2,3,4,5].map(i => 
    `<div class="signal-bar ${i <= bars ? 'active' : ''}"></div>`).join('')}</div>`
}

// Incremental update of detail panel - updates values without full re-render
function updateDetailPanel(device) {
  const panel = document.getElementById('detailPanel')
  if (!panel || panel.classList.contains('hidden')) return

  // Re-render the detail panel for the selected device. This keeps identity fields
  // (e.g., firmware) and radio stats in sync with websocket updates.
  const prevScrollTop = panel.scrollTop
  try {
    renderDeviceDetail(panel, device)
  } catch (e) {
    console.warn('updateDetailPanel render error:', e)
    return
  }
  // Preserve scroll position so updates feel smooth while reading.
  panel.scrollTop = prevScrollTop
}

// Update info pane without full re-render
function updateInfoPane(device) {
  const pane = document.querySelector('.device-info-pane')
  if (!pane) return
  
  // Update individual values
  const updateValue = (selector, value) => {
    const el = pane.querySelector(selector)
    if (el) el.textContent = value
  }
  
  // Status
  const statusDot = pane.querySelector('.status-dot')
  if (statusDot) {
    const status = store.getStatus(device)
    statusDot.className = `status-dot ${status}`
    const reason = store.getStatusReason(device)
    statusDot.title = reason || ''
  }
  
  // Uptime
  const uptime = device.uptime || 0
  const days = Math.floor(uptime / 86400)
  const hours = Math.floor((uptime % 86400) / 3600)
  const mins = Math.floor((uptime % 3600) / 60)
  const uptimeStr = uptime ? `${days}d ${hours}h ${mins}m` : '-'
  pane.querySelectorAll('.info-item').forEach(item => {
    const label = item.querySelector('.info-label')
    const value = item.querySelector('.info-value')
    if (!label || !value) return
    
    const labelText = label.textContent
    switch (labelText) {
      case 'Uptime':
        value.textContent = uptimeStr
        break
      case 'Peers':
        value.textContent = device.peer_count || 0
        break
      case '60GHz Signal':
        if (device.signal_60ghz) {
          value.textContent = `${device.signal_60ghz} dBm`
          const quality = device.radio_60ghz?.signal_quality
          value.className = `info-value ${quality ? `signal-${quality}` : getSignalClass60(device.signal_60ghz)}`
        }
        break
      case '5GHz Signal':
        const sig5 = get5GHzCombined(device)
        if (sig5) {
          value.textContent = `${sig5} dBm`
          const quality = device.radio_5ghz?.signal_quality || device.radio_ltu?.signal_quality
          value.className = `info-value ${quality ? `signal-${quality}` : getSignalClass5(sig5)}`
        }
        break
    }
  })
}

// ============================================
// Unified Signal Quality Thresholds
// ============================================
// All signal quality determinations should use these thresholds
const SIGNAL_THRESHOLDS = {
  // 60GHz (Wave)
  '60ghz': {
    good: -55,    // > -55 dBm
    fair: -65,    // -55 to -65 dBm
    // poor: < -65 dBm
  },
  // 5GHz (Wave backup, LTU, airMAX)
  '5ghz': {
    good: -62,    // > -62 dBm
    fair: -70,    // -62 to -70 dBm
    // poor: < -70 dBm
  }
}

// Interference thresholds (Wave channel utilization, percent of airtime)
// Configurable via Settings. Defaults are used if settings are missing.
const DEFAULT_INTERFERENCE_THRESHOLDS = {
  warning: 10,
  critical: 25
};

function getInterferenceThresholds() {
  const settings = (store.getState && store.getState().settings) ? store.getState().settings : {};
  const warnRaw = settings.interference_warning_pct;
  const critRaw = settings.interference_critical_pct;

  let warning = parseFloat(warnRaw);
  let critical = parseFloat(critRaw);

  if (!Number.isFinite(warning)) warning = DEFAULT_INTERFERENCE_THRESHOLDS.warning;
  if (!Number.isFinite(critical)) critical = DEFAULT_INTERFERENCE_THRESHOLDS.critical;

  // Clamp to sane range
  warning = Math.max(0, Math.min(100, warning));
  critical = Math.max(0, Math.min(100, critical));

  // If user entered inverted thresholds, normalize to avoid confusing behavior.
  if (critical < warning) {
    const tmp = warning;
    warning = critical;
    critical = tmp;
  }

  return { warning, critical };
}

// Return the highest interference percentage across known radios on a device.
// Returns null if no interference stats are available.
function getMaxInterference(device) {
  if (!device) return null;

  // Prefer the flattened radio_* fields (these are kept up-to-date by WS updates),
  // but fall back to device.wireless.* for older snapshots.
  const radios = [
    device.radio_5ghz || device.wireless?.radio_5ghz,
    device.radio_6ghz || device.wireless?.radio_6ghz,
    device.radio_60ghz || device.wireless?.radio_60ghz,
    device.radio_ltu || device.wireless?.radio_ltu
  ].filter(Boolean);

  let best = null;
  for (const r of radios) {
    const v = r?.utilization?.interference;
    if (typeof v !== 'number' || !isFinite(v)) continue;

    if (!best || v > best.value) {
      best = {
        value: v,
        band: r.band || '',
        linkState: r.link_state || '',
        radio: r
      };
    }
  }
  return best;
}

function formatBandLabel(band) {
  if (!band) return 'unknown'
  const b = String(band).toLowerCase()
  if (b === '5ghz' || b === '5g' || b === '5') return '5 GHz'
  if (b === '6ghz' || b === '6g' || b === '6') return '6 GHz'
  if (b === '60ghz' || b === '60g' || b === '60') return '60 GHz'
  if (b === 'ltu') return 'LTU'
  return band
}

function computeActiveWarnings() {
  const warnings = []
  const thresholds = getInterferenceThresholds()

  // If thresholds are disabled, return no warnings.
  if (!thresholds || thresholds.warn <= 0) return warnings

  const devices = store.devices || []
  for (const d of devices) {
    if (!d || d.role !== 'ap') continue

    const max = getMaxInterference(d)
    if (!max || typeof max.value !== 'number') continue

    if (max.value < thresholds.warn) continue

    const severity = max.value >= thresholds.critical ? 'critical' : 'warning'
    const bandLabel = formatBandLabel(max.band)
    warnings.push({
      type: 'ap_interference',
      device_id: d.id,
      name: d.name || d.hostname || d.ssid || `Device ${d.id}`,
      ip: d.ip || d.host || '',
      severity,
      value: max.value,
      band: max.band || '',
      threshold_warn: thresholds.warn,
      threshold_critical: thresholds.critical,
      message: `Interference ${max.value.toFixed(1)}% (${bandLabel})`
    })
  }

  return warnings
}

// Determine which band a device primarily uses
function getDeviceBand(device) {
  if (device.signal_60ghz || device.radio_60ghz?.signal) return '60ghz'
  return '5ghz'  // Default to 5GHz for LTU, airMAX, Wave backup
}

// Get signal quality level: 'good', 'fair', 'poor', or '' if no signal
function getSignalQuality(level, band = '5ghz') {
  if (!level || level === 0) return ''
  const thresholds = SIGNAL_THRESHOLDS[band] || SIGNAL_THRESHOLDS['5ghz']
  if (level > thresholds.good) return 'good'
  if (level > thresholds.fair) return 'fair'
  return 'poor'
}

// Get CSS class for signal level (4-tier for dashboard display)
function getSignalClass(level, band = '5ghz') {
  if (!level || level === 0) return ''
  const thresholds = SIGNAL_THRESHOLDS[band] || SIGNAL_THRESHOLDS['5ghz']
  // 4-tier: excellent (well above good), good, fair, poor
  const excellentOffset = band === '60ghz' ? 5 : 5  // 5dB above good threshold
  if (level >= thresholds.good + excellentOffset) return 'signal-excellent'
  if (level > thresholds.good) return 'signal-good'
  if (level > thresholds.fair) return 'signal-fair'
  return 'signal-poor'
}

// Convenience wrappers for backward compatibility
function getSignalClass60(level) {
  return getSignalClass(level, '60ghz')
}

function getSignalClass5(level) {
  return getSignalClass(level, '5ghz')
}

// Get threshold description for legend display
function getThresholdLabel(band = '5ghz') {
  const t = SIGNAL_THRESHOLDS[band] || SIGNAL_THRESHOLDS['5ghz']
  return {
    good: `>${t.good}`,
    fair: `${t.fair} to ${t.good}`,
    poor: `<${t.fair}`
  }
}

function get5GHzChains(device) {
  if (device.radio_5ghz?.signal_per_chain?.length > 0) return device.radio_5ghz.signal_per_chain
  if (device.radio_ltu?.signal_per_chain?.length > 0) return device.radio_ltu.signal_per_chain
  if (device.signal_per_chain?.length > 0) return device.signal_per_chain
  return []
}

// Get combined 5GHz signal - uses server-computed value (Go handles MRC combining)
// IMPORTANT: Signal combining (MRC) is done server-side in Go to avoid
// repeated Math.pow/log10 calls in JavaScript. This function only retrieves
// the pre-computed value.
function get5GHzCombined(device) {
  // Prefer signal_combined (pre-computed by Go using MRC formula)
  if (device.radio_5ghz?.signal_combined) return device.radio_5ghz.signal_combined
  if (device.radio_ltu?.signal_combined) return device.radio_ltu.signal_combined
  // Fallback to legacy signal fields if signal_combined not present
  return device.signal_5ghz || device.signal_ltu || device.signal_airmax || 0
}

// Show login page
function showLoginPage() {
  const main = document.getElementById('app')
  main.innerHTML = `
    <div class="login-page">
      <div class="login-container">
        <div class="login-logo">
          <img src="waveControl_wordmark.svg" alt="waveControl" />
        </div>
        <div class="login-box">
          <div class="form-group">
            <label>Username</label>
            <input type="text" id="loginUsername" placeholder="Username" autocomplete="username" />
          </div>
          <div class="form-group">
            <label>Password</label>
            <input type="password" id="loginPassword" placeholder="password" autocomplete="current-password" />
          </div>
          <button class="login-btn" id="loginBtn">SIGN IN</button>
          <div class="login-error" id="loginError"></div>
        </div>
      </div>
      <footer class="login-footer">
        <span>waveControl &copy; ${new Date().getFullYear()}</span>
      </footer>
    </div>
  `
  
  const loginBtn = document.getElementById('loginBtn')
  const loginError = document.getElementById('loginError')
  const usernameInput = document.getElementById('loginUsername')
  const passwordInput = document.getElementById('loginPassword')
  
  let loginInProgress = false
  
  async function doLogin() {
    // Prevent double submission
    if (loginInProgress) return
    
    const username = usernameInput.value.trim()
    const password = passwordInput.value
    
    if (!username || !password) {
      loginError.textContent = 'Enter username and password'
      return
    }
    
    loginInProgress = true
    loginBtn.disabled = true
    loginBtn.textContent = 'SIGNING IN...'
    loginError.textContent = ''
    
    try {
      await api.login(username, password)
    } catch (e) {
      loginError.textContent = e.message || 'Login failed'
      loginBtn.disabled = false
      loginBtn.textContent = 'SIGN IN'
      loginInProgress = false
      return
    }
	    
	    // Force a full re-bootstrap so all pages start from fresh API data and
	    // no in-memory/UI state leaks across sessions.
	    loginBtn.textContent = 'Loading…'
	    location.reload()
	    return
  }
  
  loginBtn.addEventListener('click', doLogin)
  passwordInput.addEventListener('keydown', e => {
    if (e.key === 'Enter' && !loginInProgress) doLogin()
  })
  usernameInput.addEventListener('keydown', e => {
    if (e.key === 'Enter') passwordInput.focus()
  })
  
  usernameInput.focus()
}

// Update status counts in header
function updateCounts() {
  const counts = store.counts
  document.getElementById('countOnline').textContent = counts.online
  document.getElementById('countOffline').textContent = counts.offline
  document.getElementById('countUnknown').textContent = counts.unknown
}

// ===== Live Updates for Derived Pages =====
// Some pages (notably Quality/Associations) are derived/aggregated views over the
// underlying per-device state. The WebSocket updates the store, but those pages
// must re-render their derived view to avoid stale UI.
//
// IMPORTANT: We debounce these re-renders and preserve scroll position to avoid
// the "jumping"/"refresh" feel.
let __assocDirty = false
let __assocRefreshTimer = null
let __assocLastRefreshAt = 0

function scheduleAssociationsRefresh() {
  const state = store.getState()
  if (state.currentPage !== 'associations') return

  __assocDirty = true
  if (__assocRefreshTimer) return

  // Rendering the Quality view can be expensive on large installs.
  // Keep it "live" while open, but clamp refresh frequency.
  const now = Date.now()
  const minIntervalMs = 2000
  const delay = Math.max(200, minIntervalMs - (now - __assocLastRefreshAt))

  __assocRefreshTimer = setTimeout(() => {
    __assocRefreshTimer = null
    if (!__assocDirty) return
    __assocDirty = false
    if (store.getState().currentPage !== 'associations') return

	    // Preserve scroll for both the main app container and the Quality tab content
	    // (depending on CSS/layout, either may be the active scroll container).
	    const appEl = document.getElementById('app')
	    const assocEl = document.getElementById('assocTabContent')
	    const prevAppScroll = appEl ? appEl.scrollTop : 0
	    const prevAssocScroll = assocEl ? assocEl.scrollTop : 0
    try {
      renderAssociationsContent()
    } finally {
	      if (appEl) appEl.scrollTop = prevAppScroll
	      if (assocEl) assocEl.scrollTop = prevAssocScroll
      __assocLastRefreshAt = Date.now()
    }
  }, delay)
}

// ===== Ultra Debug UI =====
const ultraBtn = document.getElementById('ultraBtn')
const ultraModal = document.getElementById('ultraDebugModal')
const ultraCloseBtn = document.getElementById('ultraCloseBtn')
const ultraCloseX = document.getElementById('closeUltraDebug')
const ultraTargetSelect = document.getElementById('ultraTargetSelect')
const ultraEntriesEl = document.getElementById('ultraDebugEntries')
const ultraMetaEl = document.getElementById('ultraDebugMeta')
const ultraTailInput = document.getElementById('ultraTail')
const ultraAutoRefresh = document.getElementById('ultraAutoRefresh')
const ultraRefreshBtn = document.getElementById('ultraRefreshBtn')
const ultraClearBtn = document.getElementById('ultraClearBtn')
const ultraDisableBtn = document.getElementById('ultraDisableBtn')
const ultraDownloadBtn = document.getElementById('ultraDownloadBtn')
const ultraTabPretty = document.getElementById('ultraTabPretty')
const ultraTabRaw = document.getElementById('ultraTabRaw')
const ultraRawTextarea = document.getElementById('ultraDebugRaw')
const ultraCopyBtn = document.getElementById('ultraCopyBtn')

const ultraState = {
  buffers: [],
  targets: [],
  currentTargetId: '',
  modalOpen: false,
  refreshTimer: null,
  view: 'pretty', // 'pretty' | 'raw'
  lastLoaded: null, // { target, deviceSnap, hostSnap, combined }
  rawJSON: '',
  loading: false,
  lastRenderKey: null,
  lastEntrySigs: null,
}

function formatBytes(n) {
  const b = Number(n) || 0
  if (b < 1024) return b + ' B'
  if (b < 1024 * 1024) return (b / 1024).toFixed(1) + ' KB'
  if (b < 1024 * 1024 * 1024) return (b / (1024 * 1024)).toFixed(1) + ' MB'
  return (b / (1024 * 1024 * 1024)).toFixed(1) + ' GB'
}

function parseTime(v) {
  const t = Date.parse(v)
  return isNaN(t) ? 0 : t
}

function ultraEntrySig(e) {
  // A stable-ish signature used for change detection and restoring UI state.
  return `${e?.time || ''}|${e?.type || ''}|${e?.query?.method || ''}|${e?.query?.url || ''}|${e?.response?.status || ''}|${e?.error || ''}|${e?._scope || ''}`
}

function ultraRenderKey(targetId, entries) {
  const count = Array.isArray(entries) ? entries.length : 0
  const first = count ? ultraEntrySig(entries[0]) : ''
  const last = count ? ultraEntrySig(entries[count - 1]) : ''
  return { targetId: String(targetId || ''), count, first, last }
}

function sameUltraRenderKey(a, b) {
  if (!a || !b) return false
  return a.targetId === b.targetId && a.count === b.count && a.first === b.first && a.last === b.last
}

function buildUltraTargets(buffers) {
  const deviceBufByID = new Map()
  const hostBufByHost = new Map()

  buffers.forEach(b => {
    if (!b || !b.kind) return
    if (b.kind === 'device' && b.device_id) deviceBufByID.set(Number(b.device_id), b)
    if (b.kind === 'host' && b.host) hostBufByHost.set(String(b.host), b)
  })

  const targets = []

  // Device-scoped buffers (these are what the UI toggles from the dashboard)
  for (const [id, buf] of deviceBufByID.entries()) {
    const device = store.getDeviceById ? store.getDeviceById(id) : store.devices.find(d => d.id === id)
    const host = device?.ip_address ? String(device.ip_address) : ''
    const hostBuf = host ? hostBufByHost.get(host) : null
    if (host) hostBufByHost.delete(host)

    const name = device?.hostname || device?.name || device?.ssid || device?.ip_address || `Device ${id}`
    const label = host ? `${name} (${host})` : `${name} (#${id})`
    targets.push({
      id: `dev:${id}`,
      mode: 'device',
      device_id: id,
      host,
      deviceBufKey: buf.key,
      hostBufKey: hostBuf?.key || '',
      label,
    })
  }

  // Host-only buffers (not tied to a known device id)
  for (const [host, buf] of hostBufByHost.entries()) {
    const device = store.getDeviceByIP ? store.getDeviceByIP(host) : store.devices.find(d => d.ip_address === host)
    const name = device?.hostname || device?.name
    const label = name ? `${host} (${name})` : host
    targets.push({
      id: `host:${host}`,
      mode: 'host',
      host,
      hostBufKey: buf.key,
      label: `Host ${label}`,
    })
  }

  targets.sort((a, b) => a.label.localeCompare(b.label))
  return targets
}

function updateUltraBadgeFromBuffers(buffers) {
  const countEl = document.getElementById('countUltra')
  if (!ultraBtn || !countEl) return
  // Count *targets* (one per enabled device buffer, plus any host-only buffers).
  // This avoids double-counting when we enable both device+host capture for the same AP.
  const total = buildUltraTargets(buffers).length
  if (total > 0) {
    ultraBtn.classList.remove('hidden')
    countEl.textContent = String(total)
  } else {
    ultraBtn.classList.add('hidden')
    countEl.textContent = '0'
  }
}

async function refreshUltraDebugBadge() {
  if (!ultraBtn) return
  try {
    const resp = await api.ultraDebugList()
    const buffers = Array.isArray(resp?.buffers) ? resp.buffers : []
    ultraState.buffers = buffers
    updateUltraBadgeFromBuffers(buffers)

    // Mark devices with Ultra Debug enabled so context menus are accurate after a refresh
    try {
      // ListUltraDebug only returns buffers that are currently enabled.
      // (There is no separate "enabled" flag in the payload.)
      const enabledDeviceIds = new Set(buffers.filter(b => b?.kind === 'device' && b.device_id != null).map(b => String(b.device_id)))
      const enabledHosts = new Set(buffers.filter(b => b?.kind === 'host' && b.host).map(b => String(b.host)))
      if (Array.isArray(store.devices)) {
        for (const d of store.devices) {
          const id = d?.id != null ? String(d.id) : ''
          const ip = d?.ip_address ? String(d.ip_address) : ''
          d.ultra_debug = (id && enabledDeviceIds.has(id)) || (ip && enabledHosts.has(ip))
        }
      }
    } catch (_) {
      // Ignore ultra debug state mapping errors
    }

    // Refresh modal target list if it's open.
    if (ultraState.modalOpen) {
      ultraState.targets = buildUltraTargets(buffers)
      renderUltraTargetSelect(true)
    }
  } catch (e) {
    // If the API isn't configured or we lost auth, hide the badge.
    ultraBtn.classList.add('hidden')
  }
}

function renderUltraTargetSelect(preserveSelection) {
  if (!ultraTargetSelect) return
  const prev = preserveSelection ? (ultraTargetSelect.value || ultraState.currentTargetId) : ''
  ultraTargetSelect.innerHTML = ''

  if (!ultraState.targets.length) {
    const opt = document.createElement('option')
    opt.value = ''
    opt.textContent = 'No enabled ultra debug buffers'
    ultraTargetSelect.appendChild(opt)
    ultraTargetSelect.disabled = true
    if (ultraDownloadBtn) ultraDownloadBtn.disabled = true
    updateUltraActionButtons(null)
    return
  }

  ultraTargetSelect.disabled = false
  if (ultraDownloadBtn) ultraDownloadBtn.disabled = false

  ultraState.targets.forEach(t => {
    const opt = document.createElement('option')
    opt.value = t.id
    opt.textContent = t.label
    ultraTargetSelect.appendChild(opt)
  })

  if (prev && ultraState.targets.some(t => t.id === prev)) {
    ultraTargetSelect.value = prev
  } else {
    ultraTargetSelect.value = ultraState.targets[0].id
  }
  ultraState.currentTargetId = ultraTargetSelect.value
  updateUltraActionButtons()
}

function mergeUltraLogs(deviceSnap, hostSnap) {
  const combined = []
  const rawDevice = Array.isArray(deviceSnap?.log) ? deviceSnap.log : []
  const rawHost = Array.isArray(hostSnap?.log) ? hostSnap.log : []

  rawDevice.forEach(e => combined.push({ ...e, _scope: 'device' }))
  rawHost.forEach(e => combined.push({ ...e, _scope: 'host' }))

  combined.sort((a, b) => parseTime(a?.time) - parseTime(b?.time))

  // De-dupe identical entries if both buffers captured the same request.
  const out = []
  const seen = new Set()
  for (const e of combined) {
    const sig = `${e?.time}|${e?.type}|${e?.query?.method}|${e?.query?.url}|${e?.response?.status}|${e?.error || ''}`
    if (seen.has(sig)) continue
    seen.add(sig)
    out.push(e)
  }
  return { combined: out, rawDevice, rawHost }
}



function escapeCodeHTML(str) {
  if (str === null || str === undefined) return ''
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
}

function syntaxHighlightJson(jsonStr) {
  // Minimal syntax highlighting for JSON strings.
  // Returns SAFE HTML (escapes &,<,>) with spans for token types.
  const json = escapeCodeHTML(jsonStr)
  // Matches: strings (optionally keys), booleans/null, numbers
  const reTok = /("(\\u[a-fA-F0-9]{4}|\\[^u]|[^\\"])*"(\s*:)?|\btrue\b|\bfalse\b|\bnull\b|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)/g
  return json.replace(reTok, (match) => {
    let cls = 'json-number'
    if (match[0] === '"') {
      cls = match.endsWith(':') ? 'json-key' : 'json-string'
    } else if (match === 'true' || match === 'false') {
      cls = 'json-boolean'
    } else if (match === 'null') {
      cls = 'json-null'
    }
    return `<span class="${cls}">${match}</span>`
  })
}

function buildUltraExportObject(loaded) {
  if (!loaded?.target) return null
  const t = loaded.target
  const base = {
    generated_at: new Date().toISOString(),
  }

  if (t.mode === 'host') {
    return {
      ...base,
      host: t.host,
      host_log: Array.isArray(loaded.rawHost) ? loaded.rawHost : (Array.isArray(loaded.hostSnap?.log) ? loaded.hostSnap.log : []),
    }
  }

  return {
    ...base,
    device_id: t.device_id,
    host: t.host,
    device_log: Array.isArray(loaded.rawDevice) ? loaded.rawDevice : [],
    host_log: Array.isArray(loaded.rawHost) ? loaded.rawHost : [],
    combined_log: Array.isArray(loaded.combined)
      ? loaded.combined.map(e => {
          const c = { ...e }
          delete c._scope
          return c
        })
      : [],
  }
}

function refreshUltraRawView() {
  const exportObj = buildUltraExportObject(ultraState.lastLoaded)
  ultraState.rawJSON = exportObj ? JSON.stringify(exportObj, null, 2) : ''
  if (ultraRawTextarea && ultraState.view === 'raw') {
    const prevTop = ultraRawTextarea.scrollTop
    const prevLeft = ultraRawTextarea.scrollLeft
    ultraRawTextarea.value = ultraState.rawJSON || ''
    // Preserve scroll position during auto-refresh.
    ultraRawTextarea.scrollTop = prevTop
    ultraRawTextarea.scrollLeft = prevLeft
  }
}

function setUltraView(view) {
  ultraState.view = view === 'raw' ? 'raw' : 'pretty'

  // Tabs
  ultraTabPretty?.classList.toggle('active', ultraState.view === 'pretty')
  ultraTabRaw?.classList.toggle('active', ultraState.view === 'raw')

  // Panes
  if (ultraEntriesEl) ultraEntriesEl.classList.toggle('hidden', ultraState.view !== 'pretty')
  if (ultraRawTextarea) ultraRawTextarea.classList.toggle('hidden', ultraState.view !== 'raw')

  if (ultraState.view === 'raw') {
    refreshUltraRawView()
    if (ultraRawTextarea) ultraRawTextarea.scrollTop = 0
  }
}
function renderUltraEntries(entries, opts = {}) {
  if (!ultraEntriesEl) return

  const preserve = !!opts.preserve
  const prevTop = preserve ? ultraEntriesEl.scrollTop : 0
  const atBottom = preserve
    ? (ultraEntriesEl.scrollTop + ultraEntriesEl.clientHeight >= ultraEntriesEl.scrollHeight - 24)
    : false

  // Precompute stable signatures for incremental updates.
  const newEntries = Array.isArray(entries) ? entries : []
  const newSigs = newEntries.map(ultraEntrySig)

  // Fast-path: if the new list is an append-only extension of the previous list,
  // only append new rows instead of rebuilding the entire DOM.
  const prevSigs = Array.isArray(ultraState.lastEntrySigs) ? ultraState.lastEntrySigs : null
  const existingCount = preserve ? ultraEntriesEl.querySelectorAll('details.ultra-entry').length : 0
  const canAppend = preserve && prevSigs && existingCount === prevSigs.length && newSigs.length >= prevSigs.length
  if (canAppend) {
    let prefixOK = true
    for (let i = 0; i < prevSigs.length; i++) {
      if (newSigs[i] !== prevSigs[i]) { prefixOK = false; break }
    }
    if (prefixOK) {
      if (newSigs.length === prevSigs.length) {
        // No changes.
        return
      }
      // Append only the new entries.
      const start = prevSigs.length
      const html = newEntries.slice(start).map((e) => {
        const t = e?.time ? new Date(e.time) : null
        const timeStr = t && !isNaN(t.getTime()) ? t.toLocaleString() : (e?.time || '')
        const method = e?.query?.method || ''
        const url = e?.query?.url || ''
        const status = e?.response?.status || 0
        const err = e?.error || ''
        const statusText = err ? 'ERR' : (status ? String(status) : '-')
        const ok = !err && status && status < 400
        const statusClass = ok ? 'ok' : 'err'
        const scope = (e?._scope || '').toUpperCase()
        const dur = typeof e?.duration_ms === 'number' ? e.duration_ms : ''
        const typ = e?.type || ''

        const clean = { ...e }
        delete clean._scope
        const pretty = JSON.stringify(clean, null, 2)
        const summary = `${scope} ${method} ${url}`.trim()
        const sig = ultraEntrySig(e)

        return `
          <details class="ultra-entry" data-sig="${escapeAttr(sig)}">
            <summary>
              <span class="ultra-status ${statusClass}">${escapeHTML(statusText)}</span>
              <span>${escapeHTML(timeStr)}</span>
              <span>${escapeHTML(typ)}</span>
              <span>${escapeHTML(summary)}</span>
              <span>${dur !== '' ? escapeHTML(String(dur) + 'ms') : ''}</span>
            </summary>
            <pre class="json">${syntaxHighlightJson(pretty)}</pre>
          </details>
        `
      }).join('')

      // If the viewer is empty for some reason, fall back to rebuild.
      if (existingCount === 0) {
        // fall through to rebuild below
      } else {
        ultraEntriesEl.insertAdjacentHTML('beforeend', html)
        ultraState.lastEntrySigs = newSigs
        if (atBottom) ultraEntriesEl.scrollTop = ultraEntriesEl.scrollHeight
        return
      }
    }
  }

  // Preserve open <details> blocks by signature.
  const openSigs = new Set()
  if (preserve) {
    ultraEntriesEl.querySelectorAll('details.ultra-entry[open]').forEach(d => {
      const sig = d.getAttribute('data-sig') || ''
      if (sig) openSigs.add(sig)
    })
  }

  if (!Array.isArray(entries) || entries.length === 0) {
    ultraEntriesEl.innerHTML = '<div class="empty">No entries yet.</div>'
    ultraState.lastEntrySigs = []
    return
  }

  ultraEntriesEl.innerHTML = entries.map((e) => {
    const t = e?.time ? new Date(e.time) : null
    const timeStr = t && !isNaN(t.getTime()) ? t.toLocaleString() : (e?.time || '')
    const method = e?.query?.method || ''
    const url = e?.query?.url || ''
    const status = e?.response?.status || 0
    const err = e?.error || ''
    const statusText = err ? 'ERR' : (status ? String(status) : '-')
    const ok = !err && status && status < 400
    const statusClass = ok ? 'ok' : 'err'
    const scope = (e?._scope || '').toUpperCase()
    const dur = typeof e?.duration_ms === 'number' ? e.duration_ms : ''
    const typ = e?.type || ''

    const clean = { ...e }
    delete clean._scope
    const pretty = JSON.stringify(clean, null, 2)
    const summary = `${scope} ${method} ${url}`.trim()

    const sig = ultraEntrySig(e)

    return `
      <details class="ultra-entry" data-sig="${escapeAttr(sig)}">
        <summary>
          <span class="ultra-status ${statusClass}">${escapeHTML(statusText)}</span>
          <span>${escapeHTML(timeStr)}</span>
          <span>${escapeHTML(typ)}</span>
          <span>${escapeHTML(summary)}</span>
          <span>${dur !== '' ? escapeHTML(String(dur) + 'ms') : ''}</span>
        </summary>
        <pre class="json">${syntaxHighlightJson(pretty)}</pre>
      </details>
    `
  }).join('')

  if (preserve && openSigs.size) {
    ultraEntriesEl.querySelectorAll('details.ultra-entry').forEach(d => {
      const sig = d.getAttribute('data-sig') || ''
      if (sig && openSigs.has(sig)) d.open = true
    })
  }

  ultraState.lastEntrySigs = newSigs

  if (preserve) {
    if (atBottom) {
      ultraEntriesEl.scrollTop = ultraEntriesEl.scrollHeight
    } else {
      ultraEntriesEl.scrollTop = prevTop
    }
  }
}

async function loadUltraDebugTarget(opts = {}) {
  if (ultraState.loading) return
  if (!ultraTargetSelect) return
  const targetId = ultraTargetSelect.value
  ultraState.currentTargetId = targetId
  const target = ultraState.targets.find(t => t.id === targetId)
  if (!target) return

  const silent = !!opts.silent
  const force = !!opts.force
  const prevTargetId = ultraState.lastLoaded?.target?.id || ''
  const targetChanged = String(prevTargetId) !== String(target.id)

  const tail = Math.max(1, Math.min(5000, parseInt(ultraTailInput?.value || '500', 10) || 500))

  // Don't blank out the viewer on auto-refresh; it causes jarring resets.
  if (!silent && ultraEntriesEl && (targetChanged || !ultraState.lastLoaded)) {
    ultraEntriesEl.innerHTML = '<div class="loading">Loading...</div>'
  }
  ultraState.loading = true

  if (ultraRefreshBtn) ultraRefreshBtn.disabled = true

  try {
    let deviceSnap = null
    let hostSnap = null

    if (target.mode === 'device') {
      try {
        deviceSnap = await api.ultraDebugDevice(target.device_id, tail)
      } catch (e) {
        // ok
      }
      if (target.host) {
        try {
          hostSnap = await api.ultraDebugHost(target.host, tail)
        } catch (e) {
          // ok
        }
      }
    } else if (target.mode === 'host') {
      hostSnap = await api.ultraDebugHost(target.host, tail)
    }

    const merged = mergeUltraLogs(deviceSnap, hostSnap)

    const newKey = ultraRenderKey(target.id, merged.combined)
    const changed = force || targetChanged || !sameUltraRenderKey(newKey, ultraState.lastRenderKey)

    ultraState.lastLoaded = {
      target,
      deviceSnap,
      hostSnap,
      combined: merged.combined,
      rawDevice: merged.rawDevice,
      rawHost: merged.rawHost,
    }

    ultraState.lastRenderKey = newKey

    refreshUltraRawView()

    if (ultraMetaEl) {
      const parts = []
      if (deviceSnap && deviceSnap.kind === 'device') {
        parts.push(`Device: ${deviceSnap.entries} entries, ${formatBytes(deviceSnap.bytes)}, updated ${new Date(deviceSnap.updated).toLocaleString()}`)
      }
      if (hostSnap && hostSnap.kind === 'host') {
        parts.push(`Host: ${hostSnap.entries} entries, ${formatBytes(hostSnap.bytes)}, updated ${new Date(hostSnap.updated).toLocaleString()}`)
      }
      if (parts.length === 0) {
        parts.push('No data (buffer not enabled or no entries yet).')
      }
      ultraMetaEl.textContent = parts.join('  |  ')
    }

    if (changed) {
      renderUltraEntries(merged.combined, { preserve: !targetChanged })
    }
  } catch (e) {
    if (ultraEntriesEl) ultraEntriesEl.innerHTML = `<div class="error">${escapeHTML(e.message)}</div>`
  } finally {
    ultraState.loading = false
    if (ultraRefreshBtn) ultraRefreshBtn.disabled = false
  }
}

function openUltraDebugModal() {
  if (!ultraModal) return
  ultraModal.classList.remove('hidden')
  ultraState.modalOpen = true
  setUltraView(ultraState.view || 'pretty')
  // Pull latest list/targets before rendering.
  refreshUltraDebugBadge().then(() => {
    ultraState.targets = buildUltraTargets(ultraState.buffers)
    renderUltraTargetSelect(true)
    loadUltraDebugTarget()
  })

  // Start auto-refresh timer (kept lightweight by tail limiting)
  if (ultraState.refreshTimer) clearInterval(ultraState.refreshTimer)
  ultraState.refreshTimer = setInterval(() => {
    if (!ultraState.modalOpen) return
    if (ultraAutoRefresh && !ultraAutoRefresh.checked) return
    // Silent refresh avoids jarring UI resets while keeping the log view current.
    loadUltraDebugTarget({ silent: true })
  }, 2000)
}

function closeUltraDebugModal() {
  if (!ultraModal) return
  ultraModal.classList.add('hidden')
  ultraState.modalOpen = false
  if (ultraState.refreshTimer) {
    clearInterval(ultraState.refreshTimer)
    ultraState.refreshTimer = null
  }
}

function downloadBlob(blob, filename) {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

async function downloadUltraCurrent() {
  const loaded = ultraState.lastLoaded
  if (!loaded?.target) return

  try {
    const exportObj = buildUltraExportObject(loaded)
    if (!exportObj) {
      showToast('Nothing to download yet.', 'warning')
      return
    }
    const jsonText = JSON.stringify(exportObj, null, 2)
    const blob = new Blob([jsonText], { type: 'application/json' })

    const t = loaded.target
    if (t.mode === 'host') {
      const safeHost = (t.host || 'unknown').replace(/[^a-zA-Z0-9_.-]/g, '_')
      downloadBlob(blob, `wavecontrol-ultra-host-${safeHost}.json`)
      return
    }

    const safeHost = (t.host || 'unknown').replace(/[^a-zA-Z0-9_.-]/g, '_')
    downloadBlob(blob, `wavecontrol-ultra-${t.device_id}-${safeHost}.json`)
  } catch (e) {
    showToast('Download failed: ' + e.message, 'error')
  }
}


function getCurrentUltraTarget() {
  const targetId = ultraTargetSelect?.value
  if (!targetId) return ultraState.lastLoaded?.target || null
  return ultraState.targets.find(t => t.id === targetId) || ultraState.lastLoaded?.target || null
}

function updateUltraActionButtons() {
  const t = getCurrentUltraTarget()
  if (!ultraClearBtn || !ultraDisableBtn) return
  if (!t) {
    ultraClearBtn.disabled = true
    ultraDisableBtn.disabled = true
    ultraClearBtn.textContent = "Clear"
    ultraDisableBtn.textContent = "Disable"
    return
  }
  ultraClearBtn.disabled = false
  ultraDisableBtn.disabled = false
  if (t.mode === "host") {
    ultraClearBtn.textContent = "Clear Host"
    ultraDisableBtn.textContent = "Disable Host"
    ultraClearBtn.title = "Clear captured requests/responses for this host"
    ultraDisableBtn.title = "Stop capturing requests/responses for this host"
    return
  }
  ultraClearBtn.textContent = "Clear"
  ultraDisableBtn.textContent = "Disable"
  ultraClearBtn.title = "Clear captured requests/responses for this device and its host"
  ultraDisableBtn.title = "Stop capturing requests/responses for this device and its host"
}

async function clearUltraCurrent() {
  const t = getCurrentUltraTarget()
  if (!t) {
    showToast("No ultra debug buffer selected.", "warning")
    return
  }
  try {
    if (t.mode === "host") {
      await api.clearUltraDebugHost(t.host)
    } else {
      await api.clearUltraDebugDevice(t.device_id)
      if (t.host) await api.clearUltraDebugHost(t.host)
    }
    showToast("Ultra debug buffer cleared.", "success")
    await loadUltraDebugTarget({ force: true, preserveScroll: true })
  } catch (e) {
    showToast("Clear failed: " + e.message, "error")
  }
}

async function disableUltraCurrent() {
  const t = getCurrentUltraTarget()
  if (!t) {
    showToast("No ultra debug buffer selected.", "warning")
    return
  }
  try {
    if (t.mode === "host") {
      await api.setUltraDebugHost(t.host, false)
    } else {
      await api.setUltraDebugDevice(t.device_id, false)
      if (t.host) await api.setUltraDebugHost(t.host, false)
    }
    showToast("Ultra debug disabled.", "success")
    await refreshUltraDebugBadge()
    ultraState.targets = buildUltraTargets(ultraState.buffers)
    renderUltraTargetSelect(true)
    updateUltraActionButtons()
    if (ultraState.targets.length === 0) {
      closeUltraDebugModal()
      return
    }
    await loadUltraDebugTarget({ force: true, preserveScroll: true })
  } catch (e) {
    showToast("Disable failed: " + e.message, "error")
  }
}

// Wire UI events
ultraBtn?.addEventListener('click', (e) => {
  e.preventDefault()
  openUltraDebugModal()
})
ultraCloseBtn?.addEventListener('click', closeUltraDebugModal)
ultraCloseX?.addEventListener('click', closeUltraDebugModal)
ultraRefreshBtn?.addEventListener('click', () => loadUltraDebugTarget({ force: true }))
ultraClearBtn?.addEventListener('click', clearUltraCurrent)
ultraDisableBtn?.addEventListener('click', disableUltraCurrent)
ultraTargetSelect?.addEventListener('change', () => {
  updateUltraActionButtons()
  loadUltraDebugTarget({ force: true })
})
ultraTailInput?.addEventListener('change', () => loadUltraDebugTarget({ force: true }))
ultraDownloadBtn?.addEventListener('click', downloadUltraCurrent)
ultraTabPretty?.addEventListener('click', () => setUltraView('pretty'))
ultraTabRaw?.addEventListener('click', () => setUltraView('raw'))
ultraCopyBtn?.addEventListener('click', async () => {
  try {
    const text = ultraState.rawJSON || ''
    if (!text) {
      showToast('Nothing to copy yet.', 'warning')
      return
    }
    await navigator.clipboard.writeText(text)
    showToast('Copied ultra debug JSON.', 'success')
  } catch (e) {
    showToast('Copy failed: ' + e.message, 'error')
  }
})




// ===== Alerts Page =====
const ALERT_METRIC_OPTIONS = [
  { value: 'offline_duration', label: 'Offline duration', unit: 'seconds' },
  { value: 'signal_5ghz', label: '5 GHz signal', unit: 'dBm' },
  { value: 'signal_60ghz', label: '60 GHz signal', unit: 'dBm' },
  { value: 'signal_ltu', label: 'LTU signal', unit: 'dBm' },
  { value: 'cpu', label: 'CPU usage', unit: '%' },
  { value: 'temperature', label: 'Temperature', unit: '°C' },
  { value: 'ram', label: 'RAM usage', unit: '%' },
  { value: 'capacity', label: '60 GHz capacity', unit: 'Mbps' },
  { value: 'peer_count', label: 'Peer count', unit: 'peers' },
  { value: 'link_score', label: 'Link score', unit: 'score' },
]

const ALERT_OPERATOR_OPTIONS = [
  { value: 'lt', label: '<' },
  { value: 'lte', label: '<=' },
  { value: 'gt', label: '>' },
  { value: 'gte', label: '>=' },
  { value: 'eq', label: '=' },
  { value: 'ne', label: '!=' },
]

const ALERT_CHANNEL_OPTIONS = [
  { value: 'email', label: 'Email' },
  { value: 'webhook', label: 'Webhook' },
  { value: 'zabbix', label: 'Zabbix' },
]

const ALERT_PRESETS = [
  {
    key: 'host_down',
    title: 'Host down',
    description: 'Alert when an alertable AP has been unreachable for 3 minutes.',
    rule: { name: 'AP down', metric: 'offline_duration', operator: 'gte', threshold: 180, duration_seconds: 0, cooldown_seconds: 900, notify_channels: [], target_role: 'ap', require_alertable: true, enabled: true, scope: 'all' },
  },
  {
    key: 'weak_5ghz',
    title: 'Weak 5 GHz signal',
    description: 'Warn when an alertable STA 5 GHz link falls below -70 dBm.',
    rule: { name: 'Weak 5 GHz signal', metric: 'signal_5ghz', operator: 'lt', threshold: -70, duration_seconds: 120, cooldown_seconds: 900, notify_channels: [], target_role: 'sta', require_alertable: true, enabled: true, scope: 'all' },
  },
  {
    key: 'weak_60ghz',
    title: 'Weak 60 GHz signal',
    description: 'Warn when a 60 GHz link falls below -65 dBm.',
    rule: { name: 'Weak 60 GHz signal', metric: 'signal_60ghz', operator: 'lt', threshold: -65, duration_seconds: 120, cooldown_seconds: 900, notify_channels: [], target_role: 'all', require_alertable: true, enabled: true, scope: 'all' },
  },
  {
    key: 'weak_ltu',
    title: 'Weak LTU signal',
    description: 'Warn when an LTU link falls below -70 dBm.',
    rule: { name: 'Weak LTU signal', metric: 'signal_ltu', operator: 'lt', threshold: -70, duration_seconds: 120, cooldown_seconds: 900, notify_channels: [], target_role: 'sta', require_alertable: true, enabled: true, scope: 'all' },
  },
  {
    key: 'high_cpu',
    title: 'High CPU',
    description: 'Warn when a radio reports CPU usage above 90%.',
    rule: { name: 'High CPU', metric: 'cpu', operator: 'gt', threshold: 90, duration_seconds: 180, cooldown_seconds: 900, notify_channels: [], target_role: 'all', require_alertable: true, enabled: true, scope: 'all' },
  },
  {
    key: 'high_temp',
    title: 'High temperature',
    description: 'Alert when a radio CPU temperature rises above 75 °C.',
    rule: { name: 'High temperature', metric: 'temperature', operator: 'gt', threshold: 75, duration_seconds: 180, cooldown_seconds: 900, notify_channels: [], target_role: 'all', require_alertable: true, enabled: true, scope: 'all' },
  },
  {
    key: 'low_capacity',
    title: 'Low capacity',
    description: 'Warn when combined 60 GHz capacity falls below 100 Mbps.',
    rule: { name: 'Low 60 GHz capacity', metric: 'capacity', operator: 'lt', threshold: 100, duration_seconds: 300, cooldown_seconds: 1800, notify_channels: [], target_role: 'ap', require_alertable: true, enabled: true, scope: 'all' },
  },
  {
    key: 'peer_count',
    title: 'Peer count dropped',
    description: 'For a selected AP/site, alert when AP peer count drops below a threshold.',
    rule: { name: 'Peer count dropped', metric: 'peer_count', operator: 'lt', threshold: 1, duration_seconds: 120, cooldown_seconds: 900, notify_channels: [], target_role: 'ap', require_alertable: true, enabled: true, scope: 'all' },
  },
  {
    key: 'link_score',
    title: 'Low link score',
    description: 'Warn when a device reports link score below 50.',
    rule: { name: 'Low link score', metric: 'link_score', operator: 'lt', threshold: 50, duration_seconds: 180, cooldown_seconds: 900, notify_channels: [], target_role: 'all', require_alertable: true, enabled: true, scope: 'all' },
  },
]

let alertRuleDraft = null
let alertRuleEditingId = null
let alertStatusFilter = 'active'

function canEditAlerts() {
  const roles = store.user?.roles || []
  return roles.includes('editor') || roles.includes('administrator')
}

function defaultAlertRule() {
  return {
    name: 'Device down',
    enabled: true,
    scope: 'all',
    scope_id: null,
    target_role: 'all',
    require_alertable: true,
    metric: 'offline_duration',
    operator: 'gte',
    threshold: 180,
    duration_seconds: 0,
    notify_channels: [],
    notify_emails: [],
    webhook_url: '',
    cooldown_seconds: 900,
  }
}

function normalizedAlertRule(rule = {}) {
  return {
    ...defaultAlertRule(),
    ...rule,
    notify_channels: Array.isArray(rule.notify_channels) ? [...rule.notify_channels] : (Array.isArray(rule.NotifyChannels) ? [...rule.NotifyChannels] : []),
    notify_emails: Array.isArray(rule.notify_emails) ? [...rule.notify_emails] : [],
    webhook_url: rule.webhook_url || '',
    scope_id: rule.scope_id === undefined ? null : rule.scope_id,
    target_role: ['all', 'ap', 'sta'].includes(String(rule.target_role || '').toLowerCase()) ? String(rule.target_role || 'all').toLowerCase() : 'all',
    require_alertable: rule.require_alertable !== false,
    enabled: rule.enabled !== false,
  }
}

function alertMetricMeta(metric) {
  return ALERT_METRIC_OPTIONS.find(m => m.value === metric) || { value: metric, label: metric, unit: '' }
}

function alertOption(value, label, current) {
  return `<option value="${escapeAttr(value)}" ${String(value) === String(current) ? 'selected' : ''}>${escapeHTML(label)}</option>`
}

function formatAlertTime(ts) {
  if (!ts) return '-'
  const d = new Date(ts)
  if (Number.isNaN(d.getTime())) return String(ts)
  return d.toLocaleString()
}

function formatAlertNumber(value, metric = '') {
  const n = Number(value)
  if (!Number.isFinite(n)) return '-'
  if (metric === 'offline_duration' || metric === 'capacity') return n.toFixed(n >= 10 ? 0 : 1)
  if (metric.startsWith('signal_') || metric === 'temperature' || metric === 'cpu' || metric === 'ram') return n.toFixed(1).replace(/\.0$/, '')
  return n.toFixed(2).replace(/\.00$/, '')
}

function alertDeviceName(alert) {
  const dev = store.getDeviceById ? store.getDeviceById(alert.device_id) : null
  return dev?.hostname || dev?.ip_address || (alert.device_id ? `device #${alert.device_id}` : '-')
}

async function refreshAlertNavBadge() {
  const badge = document.getElementById('alertNavBadge')
  if (!badge || !store.user) return
  try {
    const active = await api.alerts('active', 101)
    const count = Array.isArray(active) ? active.length : 0
    if (count > 0) {
      badge.textContent = count > 99 ? '99+' : String(count)
      badge.classList.remove('hidden')
    } else {
      badge.classList.add('hidden')
    }
  } catch (_) {
    // Alert badge is non-critical; leave the last value alone on transient failures.
  }
}


function alertRulePresetDuplicateKey(rule = {}) {
  const r = normalizedAlertRule(rule)
  return [r.name, r.metric, r.operator, String(r.threshold), r.scope || 'all', r.scope_id || '', r.target_role || 'all', r.require_alertable !== false ? 'alertable' : 'any'].join('|')
}

function findMissingAlertPresets(rules = []) {
  const existing = new Set((Array.isArray(rules) ? rules : []).map(alertRulePresetDuplicateKey))
  return ALERT_PRESETS.filter(p => !existing.has(alertRulePresetDuplicateKey(p.rule)))
}

function renderAlertPresetCards() {
  return `
    <div class="alert-preset-grid">
      ${ALERT_PRESETS.map(p => `
        <button type="button" class="alert-preset-card" data-alert-preset="${escapeAttr(p.key)}">
          <span class="alert-preset-title">${escapeHTML(p.title)}</span>
          <span class="alert-preset-description">${escapeHTML(p.description)}</span>
        </button>
      `).join('')}
    </div>
  `
}

function renderActiveAlerts(alerts, canEdit) {
  if (!Array.isArray(alerts) || alerts.length === 0) {
    return `<div class="empty-state">No ${escapeHTML(alertStatusFilter)} alerts.</div>`
  }

  return `
    <div class="alert-list">
      ${alerts.map(a => {
        const metric = alertMetricMeta(a.metric)
        const sev = String(a.severity || 'warning').toLowerCase()
        const status = String(a.status || 'active').toLowerCase()
        return `
          <div class="alert-card severity-${escapeAttr(sev)} status-${escapeAttr(status)}">
            <div class="alert-card-main">
              <div class="alert-card-header">
                <span class="alert-severity">${escapeHTML(sev)}</span>
                <span class="alert-status">${escapeHTML(status)}</span>
                <span class="alert-time">${escapeHTML(formatAlertTime(a.triggered_at))}</span>
              </div>
              <div class="alert-message">${escapeHTML(a.message || '')}</div>
              <div class="alert-meta">
                <span>${escapeHTML(alertDeviceName(a))}</span>
                <span>${escapeHTML(metric.label)}: ${escapeHTML(formatAlertNumber(a.value, a.metric))} ${escapeHTML(metric.unit || '')}</span>
                <span>threshold ${escapeHTML(formatAlertNumber(a.threshold, a.metric))}</span>
              </div>
            </div>
            ${canEdit ? `
              <div class="alert-card-actions">
                ${status === 'active' ? `<button class="btn btn-sm btn-secondary" data-alert-action="ack" data-alert-id="${a.id}">Ack</button>` : ''}
                ${status !== 'resolved' ? `<button class="btn btn-sm btn-primary" data-alert-action="resolve" data-alert-id="${a.id}">Resolve</button>` : ''}
              </div>
            ` : ''}
          </div>
        `
      }).join('')}
    </div>
  `
}

function renderAlertRuleTable(rules, canEdit) {
  if (!Array.isArray(rules) || rules.length === 0) {
    return `<div class="empty-state">No alert rules configured yet. Pick a preset or create one below.</div>`
  }

  return `
    <div class="alert-rules-table-wrap">
      <table class="alert-rules-table">
        <thead>
          <tr>
            <th>Name</th>
            <th>Scope</th>
            <th>Targets</th>
            <th>Condition</th>
            <th>Delay</th>
            <th>Channels</th>
            <th>Cooldown</th>
            <th>Status</th>
            ${canEdit ? '<th></th>' : ''}
          </tr>
        </thead>
        <tbody>
          ${rules.map(r => {
            const metric = alertMetricMeta(r.metric)
            const channels = Array.isArray(r.notify_channels) ? r.notify_channels : []
            const op = ALERT_OPERATOR_OPTIONS.find(o => o.value === r.operator)?.label || r.operator
            return `
              <tr>
                <td><strong>${escapeHTML(r.name || '')}</strong></td>
                <td>${escapeHTML(r.scope || 'all')}${r.scope_id ? ` #${escapeHTML(String(r.scope_id))}` : ''}</td>
                <td>${escapeHTML((r.target_role || 'all').toUpperCase())}${r.require_alertable !== false ? ' · alertable only' : ' · ignores alertable'}</td>
                <td>${escapeHTML(metric.label)} ${escapeHTML(op)} ${escapeHTML(formatAlertNumber(r.threshold, r.metric))} ${escapeHTML(metric.unit || '')}</td>
                <td>${escapeHTML(String(r.duration_seconds || 0))}s</td>
                <td>${channels.map(c => `<span class="alert-channel-chip">${escapeHTML(c)}</span>`).join(' ') || '-'}</td>
                <td>${escapeHTML(String(r.cooldown_seconds || 0))}s</td>
                <td><span class="alert-enabled ${r.enabled ? 'enabled' : 'disabled'}">${r.enabled ? 'enabled' : 'disabled'}</span></td>
                ${canEdit ? `
                  <td class="alert-rule-actions">
                    <button class="btn btn-sm btn-secondary" data-alert-rule-edit="${r.id}">Edit</button>
                    <button class="btn btn-sm btn-secondary" data-alert-rule-copy="${r.id}">Copy</button>
                    <button class="btn btn-sm btn-danger" data-alert-rule-delete="${r.id}">Delete</button>
                  </td>
                ` : ''}
              </tr>
            `
          }).join('')}
        </tbody>
      </table>
    </div>
  `
}

function renderAlertRuleForm(rule, sites, devices, canEdit) {
  const r = normalizedAlertRule(rule)
  const channels = new Set(r.notify_channels || [])
  const metric = alertMetricMeta(r.metric)
  const sortedSites = [...(sites || [])].sort((a, b) => String(a.name || '').localeCompare(String(b.name || '')))
  const sortedDevices = [...(devices || [])].sort((a, b) => String(a.hostname || a.ip_address || '').localeCompare(String(b.hostname || b.ip_address || '')))

  return `
    <form id="alertRuleForm" class="alert-rule-form ${canEdit ? '' : 'readonly'}">
      <div class="alert-form-header">
        <div>
          <h4>${alertRuleEditingId ? 'Edit Alert Rule' : 'New Alert Rule'}</h4>
          <p class="settings-note">Rules are evaluated server-side against live stats, target role, and per-device alertability. Notification channels are optional; a rule without one still appears in the alert history.</p>
        </div>
        ${canEdit ? `<button type="button" class="btn btn-secondary" id="alertNewRule">New blank rule</button>` : ''}
      </div>

      <div class="form-row">
        <div class="form-group flex-2">
          <label>Name</label>
          <input id="alertRuleName" type="text" value="${escapeAttr(r.name)}" ${canEdit ? '' : 'disabled'} />
        </div>
        <div class="form-group compact-check">
          <label>Enabled</label>
          <div class="form-check inline-check">
            <input id="alertRuleEnabled" type="checkbox" ${r.enabled ? 'checked' : ''} ${canEdit ? '' : 'disabled'} />
            <label for="alertRuleEnabled">evaluate rule</label>
          </div>
        </div>
      </div>

      <div class="form-row">
        <div class="form-group">
          <label>Scope</label>
          <select id="alertScope" ${canEdit ? '' : 'disabled'}>
            ${alertOption('all', 'All devices', r.scope || 'all')}
            ${alertOption('site', 'Site', r.scope || 'all')}
            ${alertOption('device', 'Single device', r.scope || 'all')}
          </select>
        </div>
        <div class="form-group alert-scope-site ${r.scope === 'site' ? '' : 'hidden'}">
          <label>Site</label>
          <select id="alertScopeSite" ${canEdit ? '' : 'disabled'}>
            <option value="">Select site...</option>
            ${sortedSites.map(site => alertOption(site.id, site.name || `Site ${site.id}`, r.scope === 'site' ? r.scope_id : '')).join('')}
          </select>
        </div>
        <div class="form-group alert-scope-device ${r.scope === 'device' ? '' : 'hidden'}">
          <label>Device</label>
          <select id="alertScopeDevice" ${canEdit ? '' : 'disabled'}>
            <option value="">Select device...</option>
            ${sortedDevices.map(dev => alertOption(dev.id, `${dev.hostname || dev.ip_address || dev.mac || `Device ${dev.id}`} · ${dev.ip_address || dev.mac || ''}`, r.scope === 'device' ? r.scope_id : '')).join('')}
          </select>
        </div>
      </div>

      <div class="form-row">
        <div class="form-group">
          <label>Target devices</label>
          <select id="alertTargetRole" ${canEdit ? '' : 'disabled'}>
            ${alertOption('all', 'All roles', r.target_role || 'all')}
            ${alertOption('ap', 'APs only', r.target_role || 'all')}
            ${alertOption('sta', 'STAs only', r.target_role || 'all')}
          </select>
        </div>
        <div class="form-group compact-check flex-2">
          <label>Device alert policy</label>
          <div class="form-check inline-check">
            <input id="alertRequireAlertable" type="checkbox" ${r.require_alertable !== false ? 'checked' : ''} ${canEdit ? '' : 'disabled'} />
            <label for="alertRequireAlertable">only alert on devices marked alertable</label>
          </div>
        </div>
      </div>

      <div class="form-row">
        <div class="form-group flex-2">
          <label>Metric</label>
          <select id="alertMetric" ${canEdit ? '' : 'disabled'}>
            ${ALERT_METRIC_OPTIONS.map(opt => alertOption(opt.value, `${opt.label}${opt.unit ? ` (${opt.unit})` : ''}`, r.metric)).join('')}
          </select>
        </div>
        <div class="form-group small-field">
          <label>Operator</label>
          <select id="alertOperator" ${canEdit ? '' : 'disabled'}>
            ${ALERT_OPERATOR_OPTIONS.map(opt => alertOption(opt.value, opt.label, r.operator)).join('')}
          </select>
        </div>
        <div class="form-group small-field">
          <label>Threshold <small id="alertThresholdUnit">${escapeHTML(metric.unit || '')}</small></label>
          <input id="alertThreshold" type="number" step="any" value="${escapeAttr(r.threshold)}" ${canEdit ? '' : 'disabled'} />
        </div>
      </div>

      <div class="form-row">
        <div class="form-group small-field">
          <label>Must persist <small>seconds</small></label>
          <input id="alertDuration" type="number" min="0" step="1" value="${escapeAttr(r.duration_seconds || 0)}" ${canEdit ? '' : 'disabled'} />
        </div>
        <div class="form-group small-field">
          <label>Cooldown <small>seconds</small></label>
          <input id="alertCooldown" type="number" min="0" step="1" value="${escapeAttr(r.cooldown_seconds || 300)}" ${canEdit ? '' : 'disabled'} />
        </div>
      </div>

      <div class="form-group">
        <label>Notification channels</label>
        <div class="alert-channel-grid">
          ${ALERT_CHANNEL_OPTIONS.map(ch => `
            <label class="alert-channel-option">
              <input type="checkbox" data-alert-channel="${escapeAttr(ch.value)}" ${channels.has(ch.value) ? 'checked' : ''} ${canEdit ? '' : 'disabled'} />
              <span>${escapeHTML(ch.label)}</span>
            </label>
          `).join('')}
        </div>
      </div>

      <div class="form-row alert-email-row ${channels.has('email') ? '' : 'hidden'}">
        <div class="form-group flex-2">
          <label>Email recipients <small>comma or newline separated</small></label>
          <textarea id="alertEmails" rows="2" ${canEdit ? '' : 'disabled'}>${escapeHTML((r.notify_emails || []).join('\n'))}</textarea>
        </div>
      </div>

      <div class="form-row alert-webhook-row ${channels.has('webhook') ? '' : 'hidden'}">
        <div class="form-group flex-2">
          <label>Webhook URL</label>
          <input id="alertWebhook" type="url" value="${escapeAttr(r.webhook_url || '')}" ${canEdit ? '' : 'disabled'} />
        </div>
      </div>

      ${canEdit ? `
        <div class="settings-actions">
          <button type="submit" class="btn btn-primary">${alertRuleEditingId ? 'Save rule' : 'Create rule'}</button>
          ${alertRuleEditingId ? '<button type="button" class="btn btn-secondary" id="alertCancelEdit">Cancel edit</button>' : ''}
        </div>
      ` : '<p class="settings-note">View-only account. Editor or administrator role required to change alert rules.</p>'}
    </form>
  `
}

function readAlertRuleForm() {
  const get = id => document.getElementById(id)
  const name = get('alertRuleName')?.value.trim() || ''
  const scope = get('alertScope')?.value || 'all'
  let scopeID = null
  if (scope === 'site') {
    scopeID = parseInt(get('alertScopeSite')?.value || '', 10)
    if (!Number.isFinite(scopeID)) throw new Error('Select a site for this alert scope')
  } else if (scope === 'device') {
    scopeID = parseInt(get('alertScopeDevice')?.value || '', 10)
    if (!Number.isFinite(scopeID)) throw new Error('Select a device for this alert scope')
  }

  const metric = get('alertMetric')?.value || 'offline_duration'
  const threshold = parseFloat(get('alertThreshold')?.value)
  if (!name) throw new Error('Alert rule name is required')
  if (!Number.isFinite(threshold)) throw new Error('Threshold must be a number')

  const duration = Math.max(0, parseInt(get('alertDuration')?.value || '0', 10) || 0)
  const cooldown = Math.max(0, parseInt(get('alertCooldown')?.value || '0', 10) || 0)
  const channels = [...document.querySelectorAll('[data-alert-channel]:checked')].map(cb => cb.dataset.alertChannel)

  const emailsRaw = get('alertEmails')?.value || ''
  const notifyEmails = emailsRaw.split(/[\n,]+/).map(s => s.trim()).filter(Boolean)
  const webhookURL = get('alertWebhook')?.value.trim() || ''
  if (channels.includes('email') && notifyEmails.length === 0) throw new Error('Email channel selected but no recipients were entered')
  if (channels.includes('webhook') && !webhookURL) throw new Error('Webhook channel selected but no URL was entered')

  return {
    name,
    enabled: !!get('alertRuleEnabled')?.checked,
    scope,
    scope_id: scopeID,
    target_role: get('alertTargetRole')?.value || 'all',
    require_alertable: !!get('alertRequireAlertable')?.checked,
    metric,
    operator: get('alertOperator')?.value || 'gte',
    threshold,
    duration_seconds: duration,
    notify_channels: channels,
    notify_emails: notifyEmails,
    webhook_url: webhookURL,
    cooldown_seconds: cooldown,
  }
}

function bindAlertPageHandlers(container, rules, sites, devices, canEdit) {
  container.querySelector('#alertStatusFilter')?.addEventListener('change', e => {
    alertStatusFilter = e.target.value || 'active'
    renderAlertsPage(container)
  })

  container.querySelector('#refreshAlerts')?.addEventListener('click', () => renderAlertsPage(container))

  container.querySelectorAll('[data-alert-action]').forEach(btn => {
    btn.addEventListener('click', async () => {
      const id = parseInt(btn.dataset.alertId, 10)
      const action = btn.dataset.alertAction
      if (!Number.isFinite(id)) return
      btn.disabled = true
      try {
        if (action === 'ack') await api.acknowledgeAlert(id)
        if (action === 'resolve') await api.resolveAlert(id)
        showToast(action === 'ack' ? 'Alert acknowledged' : 'Alert resolved', 'success')
        await refreshAlertNavBadge()
        renderAlertsPage(container)
      } catch (e) {
        showToast('Alert action failed: ' + e.message, 'error')
        btn.disabled = false
      }
    })
  })

  if (!canEdit) return

  container.querySelector('#alertInstallRecommended')?.addEventListener('click', async () => {
    const missing = findMissingAlertPresets(rules)
    if (missing.length === 0) {
      showToast('Recommended alert rules are already installed', 'success')
      return
    }
    if (!confirm(`Install ${missing.length} recommended alert rule${missing.length === 1 ? '' : 's'}? They default to in-application alerts and can be edited afterward.`)) return
    const btn = container.querySelector('#alertInstallRecommended')
    btn.disabled = true
    try {
      for (const preset of missing) {
        await api.createAlertRule(normalizedAlertRule(preset.rule))
      }
      showToast(`Installed ${missing.length} recommended alert rule${missing.length === 1 ? '' : 's'}`, 'success')
      alertRuleEditingId = null
      alertRuleDraft = defaultAlertRule()
      await refreshAlertNavBadge()
      renderAlertsPage(container)
    } catch (e) {
      showToast('Install failed: ' + e.message, 'error')
      btn.disabled = false
    }
  })

  container.querySelectorAll('[data-alert-preset]').forEach(btn => {
    btn.addEventListener('click', () => {
      const preset = ALERT_PRESETS.find(p => p.key === btn.dataset.alertPreset)
      if (!preset) return
      alertRuleDraft = normalizedAlertRule(preset.rule)
      alertRuleEditingId = null
      renderAlertsPage(container)
    })
  })

  container.querySelectorAll('[data-alert-rule-edit]').forEach(btn => {
    btn.addEventListener('click', () => {
      const id = parseInt(btn.dataset.alertRuleEdit, 10)
      const rule = rules.find(r => r.id === id)
      if (!rule) return
      alertRuleDraft = normalizedAlertRule(rule)
      alertRuleEditingId = id
      renderAlertsPage(container)
    })
  })

  container.querySelectorAll('[data-alert-rule-copy]').forEach(btn => {
    btn.addEventListener('click', () => {
      const id = parseInt(btn.dataset.alertRuleCopy, 10)
      const rule = rules.find(r => r.id === id)
      if (!rule) return
      alertRuleDraft = normalizedAlertRule({ ...rule, id: undefined, name: `${rule.name || 'Alert rule'} copy` })
      alertRuleEditingId = null
      renderAlertsPage(container)
    })
  })

  container.querySelectorAll('[data-alert-rule-delete]').forEach(btn => {
    btn.addEventListener('click', async () => {
      const id = parseInt(btn.dataset.alertRuleDelete, 10)
      const rule = rules.find(r => r.id === id)
      if (!rule || !confirm(`Delete alert rule "${rule.name}"? Existing alert history remains.`)) return
      btn.disabled = true
      try {
        await api.deleteAlertRule(id)
        showToast('Alert rule deleted', 'success')
        if (alertRuleEditingId === id) {
          alertRuleEditingId = null
          alertRuleDraft = defaultAlertRule()
        }
        renderAlertsPage(container)
      } catch (e) {
        showToast('Delete failed: ' + e.message, 'error')
        btn.disabled = false
      }
    })
  })

  const scopeEl = container.querySelector('#alertScope')
  const updateScopeVisibility = () => {
    const scope = scopeEl?.value || 'all'
    container.querySelector('.alert-scope-site')?.classList.toggle('hidden', scope !== 'site')
    container.querySelector('.alert-scope-device')?.classList.toggle('hidden', scope !== 'device')
  }
  scopeEl?.addEventListener('change', updateScopeVisibility)
  updateScopeVisibility()

  const updateChannelVisibility = () => {
    const selected = new Set([...container.querySelectorAll('[data-alert-channel]:checked')].map(cb => cb.dataset.alertChannel))
    container.querySelector('.alert-email-row')?.classList.toggle('hidden', !selected.has('email'))
    container.querySelector('.alert-webhook-row')?.classList.toggle('hidden', !selected.has('webhook'))
  }
  container.querySelectorAll('[data-alert-channel]').forEach(cb => cb.addEventListener('change', updateChannelVisibility))
  updateChannelVisibility()

  container.querySelector('#alertMetric')?.addEventListener('change', e => {
    const meta = alertMetricMeta(e.target.value)
    const unit = container.querySelector('#alertThresholdUnit')
    if (unit) unit.textContent = meta.unit || ''
  })

  container.querySelector('#alertNewRule')?.addEventListener('click', () => {
    alertRuleDraft = defaultAlertRule()
    alertRuleEditingId = null
    renderAlertsPage(container)
  })

  container.querySelector('#alertCancelEdit')?.addEventListener('click', () => {
    alertRuleEditingId = null
    alertRuleDraft = defaultAlertRule()
    renderAlertsPage(container)
  })



  container.querySelector('#alertRuleForm')?.addEventListener('submit', async e => {
    e.preventDefault()
    try {
      const rule = readAlertRuleForm()
      if (alertRuleEditingId) {
        await api.updateAlertRule(alertRuleEditingId, rule)
        showToast('Alert rule updated', 'success')
      } else {
        await api.createAlertRule(rule)
        showToast('Alert rule created', 'success')
      }
      alertRuleEditingId = null
      alertRuleDraft = defaultAlertRule()
      await refreshAlertNavBadge()
      renderAlertsPage(container)
    } catch (err) {
      showToast('Save failed: ' + err.message, 'error')
    }
  })
}

async function renderAlertsPage(container) {
  const canEdit = canEditAlerts()
  if (!alertRuleDraft) alertRuleDraft = defaultAlertRule()

  container.innerHTML = `
    <div class="page alerts-page">
      <div class="alerts-header">
        <div>
          <h3>Alerts</h3>
          <p class="settings-note">Operator rules, active alarms, and notification delivery setup. The server evaluates rules continuously and records state here; email, webhook, and Zabbix delivery are optional.</p>
        </div>
        <button class="btn btn-secondary" id="refreshAlerts">Refresh</button>
      </div>
      <div id="alertsContent">Loading alerts...</div>
    </div>
  `

  const content = container.querySelector('#alertsContent')
  try {
    const [rules, alerts, sites] = await Promise.all([
      api.alertRules(),
      api.alerts(alertStatusFilter === 'all' ? '' : alertStatusFilter, 100),
      api.sites().catch(() => []),
    ])
    const devices = store.devices || []
    const activeCount = Array.isArray(alerts) && alertStatusFilter === 'active' ? alerts.length : null
    content.innerHTML = `
      <div class="alerts-layout">
        <div class="alerts-main">
          <div class="settings-section">
            <div class="alert-section-header">
              <h4>Active / Recent Alerts</h4>
              <div class="alert-inline-controls">
                <select id="alertStatusFilter" class="select-sm">
                  ${alertOption('active', 'Active', alertStatusFilter)}
                  ${alertOption('acknowledged', 'Acknowledged', alertStatusFilter)}
                  ${alertOption('resolved', 'Resolved', alertStatusFilter)}
                  ${alertOption('all', 'All recent', alertStatusFilter)}
                </select>
              </div>
            </div>
            ${renderActiveAlerts(alerts, canEdit)}
          </div>

          <div class="settings-section">
            <div class="alert-section-header">
              <h4>Alert Rules</h4>
              <span class="alert-count-note">${Array.isArray(rules) ? rules.length : 0} configured${activeCount !== null ? ` · ${activeCount} active alerts` : ''}</span>
            </div>
            ${renderAlertRuleTable(rules, canEdit)}
          </div>
        </div>

        <div class="alerts-sidebar">
          ${canEdit ? `
            <div class="settings-section">
              <div class="alert-section-header">
                <h4>Presets</h4>
                <button type="button" class="btn btn-sm btn-secondary" id="alertInstallRecommended">Install recommended rules</button>
              </div>
              <p class="settings-note">Pick a sane starting point, tune one rule before saving, or install the default host-down/signal/cpu/temp/capacity rules in one shot.</p>
              ${renderAlertPresetCards()}
            </div>
          ` : ''}

          <div class="settings-section">
            ${renderAlertRuleForm(alertRuleDraft, sites, devices, canEdit)}
          </div>
        </div>
      </div>
    `
    bindAlertPageHandlers(container, Array.isArray(rules) ? rules : [], Array.isArray(sites) ? sites : [], devices, canEdit)
    await refreshAlertNavBadge()
  } catch (e) {
    content.innerHTML = `<p class="error">Error loading alerts: ${escapeHTML(e.message)}</p>`
  }
}

// Render current page

function updateBodyPageClass(page) {
  const prefix = 'page-'
  // Remove any existing page-* classes (for page-scoped layout tweaks)
  Array.from(document.body.classList).forEach(cls => {
    if (cls.startsWith(prefix)) document.body.classList.remove(cls)
  })
  if (page) document.body.classList.add(prefix + page)
}

function renderCurrentPage() {
  updateBodyPageClass(store.currentPage)
  const main = document.getElementById('app')
  const page = store.currentPage
  
  // Clean up virtual table ONLY when leaving dashboard
  // Don't clean up if we're staying on dashboard (e.g. during poll refresh)
  if (!['dashboard', 'devices'].includes(page)) {
    cleanupVirtualTable()
  }
  
  // Show/hide sidebar based on page
  const sidebar = document.getElementById('sidebar')
  if (['topology'].includes(page)) {
    sidebar?.classList.add('collapsed')
  } else {
    sidebar?.classList.remove('collapsed')
  }
  
  // Remove drilldown full-screen mode unless on drilldown page
  if (page !== 'drilldown') {
    document.body.classList.remove('drilldown-active')
  }
  
  // Hide detail panel unless on dashboard/devices/associations/drilldown with selection
  const detailPanel = document.getElementById('detailPanel')
  if (!['dashboard', 'devices', 'associations', 'drilldown'].includes(page) || !store.selectedDevice) {
    detailPanel?.classList.add('hidden')
  }
  
  switch (page) {
    case 'dashboard':
    case 'devices':
      renderDevices(main)
      break
    case 'map':
      renderMapPage(main)
      break
    case 'associations':
      // If already on associations page, just refresh content
      if (main.querySelector('.page-associations')) {
        renderAssociationsContent()
      } else {
        renderAssociationsPage(main)
      }
      break
    case 'config':
      renderConfigPage(main)
      break
    case 'scheduler':
      renderSchedulerPage(main)
      break
    case 'reports':
      renderReportsPage(main)
      break
    case 'alerts':
      renderAlertsPage(main)
      break
    case 'drilldown':
      renderDrilldownPage(main)
      break
    case 'firmware':
      renderFirmwarePage(main)
      break
    case 'logs':
      renderLogsPage(main)
      break
    case 'settings':
      renderSettingsPage(main)
      break
    default:
      renderDevices(main)
  }
}

// Firmware page
async function renderFirmwarePage(container) {
  const canEdit = store.user?.roles?.includes('editor') || store.user?.roles?.includes('administrator')
  
  container.innerHTML = `
    <div class="page firmware-page">
      <div class="firmware-header">
        <h3>Firmware Files</h3>
        ${canEdit ? `
          <div class="firmware-upload">
            <input type="file" id="firmwareFile" accept=".bin" multiple style="display:none" />
            <button class="btn btn-primary" id="uploadBtn">Upload Firmware</button>
          </div>
        ` : ''}
      </div>
      <div class="firmware-list" id="firmwareList">Loading...</div>
    </div>
  `
  
  // Upload button handler
  if (canEdit) {
    document.getElementById('uploadBtn')?.addEventListener('click', () => {
      document.getElementById('firmwareFile')?.click()
    })
    
    document.getElementById('firmwareFile')?.addEventListener('change', async (e) => {
      const files = Array.from(e.target.files)
      if (!files.length) return
      
      const btn = document.getElementById('uploadBtn')
      btn.disabled = true
      
      let success = 0, failed = 0
      for (let i = 0; i < files.length; i++) {
        btn.textContent = `Uploading ${i + 1}/${files.length}...`
        try {
          await api.uploadFirmware(files[i])
          success++
        } catch (err) {
          failed++
          showToast(`Failed: ${files[i].name}: ${err.message}`, 'error')
        }
      }
      
      if (success > 0) {
        showToast(`Uploaded ${success} file${success > 1 ? 's' : ''}`, 'success')
      }
      
      btn.disabled = false
      btn.textContent = 'Upload Firmware'
      e.target.value = ''
      renderFirmwarePage(container)
    })
  }
  
  try {
    const firmware = await api.firmwareList()
    const list = document.getElementById('firmwareList')
    
    if (!firmware || firmware.length === 0) {
      list.innerHTML = '<p class="empty-state">No firmware files found. Add .bin files to the firmware directory.</p>'
      return
    }
    
    // Sort by platform, flavor, version descending
    firmware.sort((a, b) => {
      const p = (a.platform || '').localeCompare(b.platform || '')
      if (p !== 0) return p
      const f = (a.flavor || '').localeCompare(b.flavor || '')
      if (f !== 0) return f
      return (b.version || '').localeCompare(a.version || '')
    })
    
    list.innerHTML = `
      <table class="firmware-table">
        <thead>
          <tr>
            <th>Filename</th>
            <th>Platform</th>
            <th>Flavor</th>
            <th>Version</th>
            <th>Size</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          ${firmware.map(fw => `
            <tr>
              <td class="fw-name">${escapeHTML(fw.relative_path || fw.name)}</td>
              <td class="fw-platform">${escapeHTML((fw.platform || '-').toUpperCase())}</td>
              <td class="fw-flavor">${escapeHTML(fw.flavor || '-')}</td>
              <td class="fw-version">${escapeHTML(fw.version || '-')}</td>
              <td class="fw-size">${formatSize(fw.size)}</td>
              <td class="fw-actions">
                <a class="btn btn-sm btn-download" href="/api/wavecontrol/firmware/${encodeURIComponent(fw.id || fw.name)}/download" download title="Download">⬇</a>
                ${canEdit ? `
                  <button class="btn btn-sm btn-danger-subtle" data-wc-action="delete-firmware" data-reference="${escapeAttr(fw.id || fw.relative_path || fw.name)}" data-display-name="${escapeAttr(fw.relative_path || fw.name)}" title="Delete">🗑</button>
                ` : ''}
              </td>
            </tr>
          `).join('')}
        </tbody>
      </table>
    `
  } catch (e) {
    document.getElementById('firmwareList').innerHTML = `<p class="error">Error: ${escapeHTML(e.message)}</p>`
  }
}

// Delete firmware helper (global for onclick)
window.deleteFirmware = async function(reference, displayName = reference) {
  if (!confirm(`Delete firmware "${displayName}"?`)) return
  
  try {
    await api.deleteFirmware(reference)
    showToast(`Deleted ${displayName}`, 'success')
    // Refresh page
    const main = document.getElementById('mainContent')
    if (main) renderFirmwarePage(main)
  } catch (err) {
    showToast(`Delete failed: ${err.message}`, 'error')
  }
}

// Settings page
async function renderSettingsPage(container) {
  const isAdmin = store.user?.roles?.includes('administrator')
  
  if (!isAdmin) {
    container.innerHTML = `<div class="page"><p>Admin access required for settings.</p></div>`
    return
  }
  
  container.innerHTML = `
    <div class="page settings-page">
      <h3>Settings</h3>
      <div id="settingsContent">Loading...</div>
    </div>
  `
  
  try {
    const settings = await api.settings()
    const content = document.getElementById('settingsContent')
    
    content.innerHTML = `
      <div class="settings-layout">
        <div class="settings-main">
          <div class="settings-section">
            <h4>AP Credentials</h4>
            <p class="settings-note">Used for adding APs and polling. Tried in order when connecting.</p>
            <div class="credentials-grid">
              <div class="form-group">
                <label>Username 1</label>
                <input type="text" id="settAPUser1" value="${settings.ap_cred1_user || ''}" />
              </div>
              <div class="form-group">
                <label>Password 1</label>
                <input type="password" id="settAPPass1" value="${settings.ap_cred1_pass || ''}" autocomplete="new-password" />
              </div>
              <div class="form-group">
                <label>Username 2</label>
                <input type="text" id="settAPUser2" value="${settings.ap_cred2_user || ''}" />
              </div>
              <div class="form-group">
                <label>Password 2</label>
                <input type="password" id="settAPPass2" value="${settings.ap_cred2_pass || ''}" autocomplete="new-password" />
              </div>
              <div class="form-group">
                <label>Username 3</label>
                <input type="text" id="settAPUser3" value="${settings.ap_cred3_user || ''}" />
              </div>
              <div class="form-group">
                <label>Password 3</label>
                <input type="password" id="settAPPass3" value="${settings.ap_cred3_pass || ''}" autocomplete="new-password" />
              </div>
            </div>
          </div>
          
          <div class="settings-section">
            <h4>STA Credentials</h4>
            <p class="settings-note">Used when connecting to STAs. Tried first when polling STAs from AP peer list.</p>
            <div class="credentials-grid">
              <div class="form-group">
                <label>Username 1</label>
                <input type="text" id="settSTAUser1" value="${settings.sta_cred1_user || ''}" />
              </div>
              <div class="form-group">
                <label>Password 1</label>
                <input type="password" id="settSTAPass1" value="${settings.sta_cred1_pass || ''}" autocomplete="new-password" />
              </div>
              <div class="form-group">
                <label>Username 2</label>
                <input type="text" id="settSTAUser2" value="${settings.sta_cred2_user || ''}" />
              </div>
              <div class="form-group">
                <label>Password 2</label>
                <input type="password" id="settSTAPass2" value="${settings.sta_cred2_pass || ''}" autocomplete="new-password" />
              </div>
              <div class="form-group">
                <label>Username 3</label>
                <input type="text" id="settSTAUser3" value="${settings.sta_cred3_user || ''}" />
              </div>
              <div class="form-group">
                <label>Password 3</label>
                <input type="password" id="settSTAPass3" value="${settings.sta_cred3_pass || ''}" autocomplete="new-password" />
              </div>
            </div>
          </div>
          
          <div class="settings-section">
            <h4>Paths</h4>
            <p class="settings-note">Relative to working directory (user home or chroot)</p>
            <div class="form-row">
              <div class="form-group">
                <label>Firmware Directory</label>
                <input type="text" id="settFirmwarePath" value="${settings.firmware_path || 'firmware'}" />
              </div>
              <div class="form-group">
                <label>Backup Directory</label>
                <input type="text" id="settBackupDir" value="${settings.backup_dir || 'backups'}" />
              </div>
            </div>
          </div>
          
          <div class="settings-section">
            <h4>Server</h4>
            <div class="form-group">
              <label>Listen Address <small>(requires restart)</small></label>
              <input type="text" id="settListenAddr" value="${settings.listen_addr || '127.0.0.1:8080'}" />
            </div>
          </div>
          
          <div class="settings-section">
            <h4>Zabbix Integration</h4>
            <div class="form-check">
              <input type="checkbox" id="settZabbixEnabled" ${settings.zabbix_enabled === 'true' ? 'checked' : ''} />
              <label for="settZabbixEnabled">Enable Zabbix Agent Bridge</label>
            </div>
            <div class="form-group">
              <label>Zabbix Listen Address</label>
              <input type="text" id="settZabbixListen" value="${settings.zabbix_listen || '127.0.0.1:10050'}" />
            </div>
            <div class="form-group">
              <label>Allowed Hosts <small>(comma-separated IPs/CIDRs, empty = allow all)</small></label>
              <input type="text" id="settZabbixAllowedHosts" value="${settings.zabbix_allowed_hosts || ''}" placeholder="10.0.0.5,192.168.1.0/24" />
            </div>
          </div>

          <div class="settings-section">
            <h4>Wave</h4>
            <p class="settings-note">Feature flags for Wave/MLO parsing and station discovery</p>
            <div class="form-check">
              <input type="checkbox" id="settWaveMloMultiRadio" ${settings.wave_mlo_multi_radio === 'true' ? 'checked' : ''} />
              <label for="settWaveMloMultiRadio">Enable Wave MLO multi-radio mapping</label>
            </div>
            <p class="settings-note">Infers band from frequency/AFC. Shows MLO6 6 GHz correctly and exposes a second 5 GHz radio (MLO5) as “5 GHz #2”.</p>
            <div class="form-check">
              <input type="checkbox" id="settWavePeerFallback" ${settings.wave_peer_fallback === 'true' ? 'checked' : ''} />
              <label for="settWavePeerFallback">Enable Wave peer endpoint fallback</label>
            </div>
            <p class="settings-note">If /api/v1.0/statistics has empty peers, query /api/v1.0/wireless/peers as a fallback.</p>
          </div>
          
          <div class="settings-actions">
            <button class="btn btn-primary" id="saveSettings">Save Settings</button>
            <span id="settingsSaved" class="save-indicator hidden">[ok] Saved</span>
          </div>
        </div>
        
        <div class="settings-sidebar">
          <div class="settings-section">
            <h4>User Management</h4>
            <div id="userManagement">Loading...</div>
          </div>
          
          <div class="settings-section">
            <h4>Poller Configuration</h4>
            <p class="settings-note">Device polling interval and worker thread settings</p>
            <div id="pollerSettings">Loading...</div>
          </div>
          
          <div class="settings-section">
            <h4>Bulk Operations</h4>
            <p class="settings-note">Concurrency and retry settings for bulk upgrades, backups, and reboots</p>
            <div id="bulkOpsSettings">Loading...</div>
          </div>
          
          <div class="settings-section">
            <h4>TLS Certificates</h4>
            <p class="settings-note">Certificate pinning for secure device communication</p>
            <div id="tlsSettings">Loading...</div>
          </div>
          
          <div class="settings-section">
            <h4>Management IP Prefixes</h4>
            <p class="settings-note">Only learn device IPs that match these CIDR prefixes. Filters out unreliable airMAX IPs. Leave empty to learn all IPs.</p>
            <div class="form-group">
              <label>Allowed Prefixes <small>(one CIDR per line)</small></label>
              <textarea id="settMgmtPrefixes" rows="3" class="prefix-list" placeholder="172.24.0.0/16&#10;10.0.0.0/8">${parsePrefixesForDisplay(settings.management_prefixes)}</textarea>
            </div>
          </div>

          <div class="settings-section">
            <h4>Quality Thresholds</h4>
            <p class="settings-note">Controls when WaveControl flags <b>High channel interference</b> in Quality → Issues Queue. Values are percentages (0-100) based on Wave airtime utilization.</p>
            <div class="form-grid">
              <div class="form-group">
                <label>Interference warning (%)</label>
                <input id="settInterferenceWarnPct" type="number" min="0" max="100" step="0.1" value="${settings.interference_warning_pct || DEFAULT_INTERFERENCE_THRESHOLDS.warning}" />
              </div>
              <div class="form-group">
                <label>Interference critical (%)</label>
                <input id="settInterferenceCritPct" type="number" min="0" max="100" step="0.1" value="${settings.interference_critical_pct || DEFAULT_INTERFERENCE_THRESHOLDS.critical}" />
              </div>
            </div>
          </div>

          <div class="settings-section">
            <h4>Report Thresholds</h4>
            <p class="settings-note">Thresholds used when generating engineering reports.</p>
            <div class="form-grid">
              <div class="form-group">
                <label>Chain imbalance threshold (dB)</label>
                <input id="settChainThresholdDb" type="number" min="1" max="40" step="1" value="${settings.chain_imbalance_threshold_db || 5}" />
              </div>
              <div class="form-group">
                <label>RX level mismatch threshold (dB)</label>
                <input id="settRxMismatchThresholdDb" type="number" min="1" max="40" step="1" value="${settings.rx_mismatch_threshold_db || 8}" />
              </div>
            </div>
          </div>
          
          <div class="settings-section">
            <h4>Sites & Regions</h4>
            <p class="settings-note">Organize devices by geographic location. Regions group sites, sites contain devices.</p>
            <div id="sitesRegionsManager">Loading...</div>
          </div>
        </div>
      </div>
    `
    
    // Load dynamic config sections
    loadPollerSettings()
    loadBulkOpsSettings()
    loadTLSSettings()
    loadUserManagement()
    loadSitesRegionsManager()
    
    document.getElementById('saveSettings')?.addEventListener('click', async () => {
      const btn = document.getElementById('saveSettings')
      btn.disabled = true
      btn.textContent = 'Saving...'
      
      try {
	        // Validate quality thresholds
	        const warnEl = document.getElementById('settInterferenceWarnPct')
	        const critEl = document.getElementById('settInterferenceCritPct')
	        let warn = parseFloat(warnEl?.value)
	        let crit = parseFloat(critEl?.value)
	        if (!Number.isFinite(warn)) warn = DEFAULT_INTERFERENCE_THRESHOLDS.warning
	        if (!Number.isFinite(crit)) crit = DEFAULT_INTERFERENCE_THRESHOLDS.critical
	        // Clamp to 0-100
	        warn = Math.max(0, Math.min(100, warn))
	        crit = Math.max(0, Math.min(100, crit))
	        if (warn > crit) {
	          showToast('Interference warning threshold must be ≤ critical threshold', 'error')
	          return
	        }
	        // Normalize inputs
	        if (warnEl) warnEl.value = String(warn)
	        if (critEl) critEl.value = String(crit)

        const chainThresholdEl = document.getElementById('settChainThresholdDb')
        const rxMismatchThresholdEl = document.getElementById('settRxMismatchThresholdDb')
        let chainThreshold = parseInt(chainThresholdEl?.value, 10)
        let rxMismatchThreshold = parseInt(rxMismatchThresholdEl?.value, 10)
        if (!Number.isFinite(chainThreshold)) chainThreshold = 5
        if (!Number.isFinite(rxMismatchThreshold)) rxMismatchThreshold = 8
        chainThreshold = Math.max(1, Math.min(40, chainThreshold))
        rxMismatchThreshold = Math.max(1, Math.min(40, rxMismatchThreshold))
        if (chainThresholdEl) chainThresholdEl.value = String(chainThreshold)
        if (rxMismatchThresholdEl) rxMismatchThresholdEl.value = String(rxMismatchThreshold)

        const updates = {
          // AP credentials (3 pairs)
          ap_cred1_user: document.getElementById('settAPUser1').value,
          ap_cred1_pass: document.getElementById('settAPPass1').value,
          ap_cred2_user: document.getElementById('settAPUser2').value,
          ap_cred2_pass: document.getElementById('settAPPass2').value,
          ap_cred3_user: document.getElementById('settAPUser3').value,
          ap_cred3_pass: document.getElementById('settAPPass3').value,
          // STA credentials (3 pairs)
          sta_cred1_user: document.getElementById('settSTAUser1').value,
          sta_cred1_pass: document.getElementById('settSTAPass1').value,
          sta_cred2_user: document.getElementById('settSTAUser2').value,
          sta_cred2_pass: document.getElementById('settSTAPass2').value,
          sta_cred3_user: document.getElementById('settSTAUser3').value,
          sta_cred3_pass: document.getElementById('settSTAPass3').value,
          // Other settings
          firmware_path: document.getElementById('settFirmwarePath').value,
          backup_dir: document.getElementById('settBackupDir').value,
          listen_addr: document.getElementById('settListenAddr').value,
          zabbix_enabled: document.getElementById('settZabbixEnabled').checked ? 'true' : 'false',
          zabbix_listen: document.getElementById('settZabbixListen').value,
          zabbix_allowed_hosts: document.getElementById('settZabbixAllowedHosts').value,
          // Wave feature flags
          wave_mlo_multi_radio: document.getElementById('settWaveMloMultiRadio').checked ? 'true' : 'false',
          wave_peer_fallback: document.getElementById('settWavePeerFallback').checked ? 'true' : 'false',
          management_prefixes: prefixesToJSON(document.getElementById('settMgmtPrefixes').value),
	          // Quality thresholds (percent)
	          interference_warning_pct: String(warn),
	          interference_critical_pct: String(crit),
          chain_imbalance_threshold_db: String(chainThreshold),
          rx_mismatch_threshold_db: String(rxMismatchThreshold),
        }
        
        const settingResult = await api.updateSettings(updates)
        
	        // Update client-side cached settings so other pages use new values without a reload
	        const prev = (store.getState && store.getState().settings) ? store.getState().settings : {}
	        store.set({ settings: { ...prev, ...updates } })

	        const saved = document.getElementById('settingsSaved')
        saved.classList.remove('hidden')
        setTimeout(() => saved.classList.add('hidden'), 2000)
        
        const restartKeys = settingResult?.restart_required || []
        if (restartKeys.length) {
          showToast(`Settings saved; restart waveControl to apply: ${restartKeys.join(', ')}`, 'warning')
        } else {
          showToast('Settings saved', 'success')
        }
      } catch (e) {
        showToast('Failed: ' + e.message, 'error')
      } finally {
        btn.disabled = false
        btn.textContent = 'Save Settings'
      }
    })
  } catch (e) {
    document.getElementById('settingsContent').innerHTML = `<p class="error">Error: ${escapeHTML(e.message)}</p>`
  }
}

// Load poller settings section
async function loadPollerSettings() {
  const container = document.getElementById('pollerSettings')
  if (!container) return
  
  try {
    const config = await api.getPollerConfig()
    
    container.innerHTML = `
      <div class="form-row">
        <div class="form-group">
          <label>Poll Interval <small>(10-300 sec)</small></label>
          <input type="number" id="pollerInterval" value="${config.poll_interval || 30}" min="10" max="300" />
        </div>
        <div class="form-group">
          <label>APs per Worker <small>(5-100)</small></label>
          <input type="number" id="pollerApsPerWorker" value="${config.aps_per_worker || 30}" min="5" max="100" />
        </div>
        <div class="form-group">
          <label>Worker Threads <small>(read-only)</small></label>
          <input type="number" id="pollerWorkerCount" value="${config.worker_count || 50}" disabled />
        </div>
      </div>
      <div class="settings-actions">
        <button class="btn btn-secondary" id="savePollerConfig">Save Poller Settings</button>
        <span id="pollerSaved" class="save-indicator hidden">Saved</span>
      </div>
    `
    
    document.getElementById('savePollerConfig')?.addEventListener('click', async () => {
      const btn = document.getElementById('savePollerConfig')
      btn.disabled = true
      btn.textContent = 'Saving...'
      
      try {
        await api.updatePollerConfig({
          poll_interval: parseInt(document.getElementById('pollerInterval').value),
          aps_per_worker: parseInt(document.getElementById('pollerApsPerWorker').value)
        })
        
        const saved = document.getElementById('pollerSaved')
        saved.classList.remove('hidden')
        setTimeout(() => saved.classList.add('hidden'), 2000)
        
        showToast('Poller settings saved', 'success')
      } catch (e) {
        showToast('Failed: ' + e.message, 'error')
      } finally {
        btn.disabled = false
        btn.textContent = 'Save Poller Settings'
      }
    })
  } catch (e) {
    container.innerHTML = `<p class="error">Error loading poller config: ${escapeHTML(e.message)}</p>`
  }
}

// Load bulk operations settings section
async function loadBulkOpsSettings() {
  const container = document.getElementById('bulkOpsSettings')
  if (!container) return
  
  try {
    const config = await api.getBulkOpsConfig()
    
    container.innerHTML = `
      <div class="form-row">
        <div class="form-group">
          <label>Global Concurrent <small>(1-100)</small></label>
          <input type="number" id="bulkMaxGlobal" value="${config.max_global_concurrent || 10}" min="1" max="100" />
        </div>
        <div class="form-group">
          <label>Per Job <small>(1-50)</small></label>
          <input type="number" id="bulkMaxPerJob" value="${config.max_per_job || 5}" min="1" max="50" />
        </div>
        <div class="form-group">
          <label>Per AP Fanout <small>(1-20)</small></label>
          <input type="number" id="bulkMaxPerAP" value="${config.max_per_ap || 3}" min="1" max="20" />
        </div>
      </div>
      <div class="form-row">
        <div class="form-group">
          <label>Max Retries <small>(0-10)</small></label>
          <input type="number" id="bulkMaxRetries" value="${config.max_retries || 3}" min="0" max="10" />
        </div>
        <div class="form-group">
          <label>Initial Backoff <small>(seconds)</small></label>
          <input type="number" id="bulkInitBackoff" value="${(config.initial_backoff || 2000000000) / 1000000000}" min="1" max="30" />
        </div>
        <div class="form-group">
          <label>Max Backoff <small>(seconds)</small></label>
          <input type="number" id="bulkMaxBackoff" value="${(config.max_backoff || 60000000000) / 1000000000}" min="10" max="300" />
        </div>
      </div>
      <div class="settings-actions">
        <button class="btn btn-secondary" id="saveBulkOps">Save Bulk Ops Settings</button>
        <span id="bulkOpsSaved" class="save-indicator hidden">Saved</span>
      </div>
    `
    
    document.getElementById('saveBulkOps')?.addEventListener('click', async () => {
      const btn = document.getElementById('saveBulkOps')
      btn.disabled = true
      btn.textContent = 'Saving...'
      
      try {
        await api.updateBulkOpsConfig({
          max_global_concurrent: parseInt(document.getElementById('bulkMaxGlobal').value),
          max_per_job: parseInt(document.getElementById('bulkMaxPerJob').value),
          max_per_ap: parseInt(document.getElementById('bulkMaxPerAP').value),
          max_retries: parseInt(document.getElementById('bulkMaxRetries').value),
          initial_backoff: parseInt(document.getElementById('bulkInitBackoff').value) * 1000000000,
          max_backoff: parseInt(document.getElementById('bulkMaxBackoff').value) * 1000000000,
          backoff_multiplier: 2.0
        })
        
        const saved = document.getElementById('bulkOpsSaved')
        saved.classList.remove('hidden')
        setTimeout(() => saved.classList.add('hidden'), 2000)
        
        showToast('Bulk ops settings saved', 'success')
      } catch (e) {
        showToast('Failed: ' + e.message, 'error')
      } finally {
        btn.disabled = false
        btn.textContent = 'Save Bulk Ops Settings'
      }
    })
  } catch (e) {
    container.innerHTML = `<p class="error">Error loading bulk ops config: ${escapeHTML(e.message)}</p>`
  }
}

// Load TLS certificate management section
async function loadTLSSettings() {
  const container = document.getElementById('tlsSettings')
  if (!container) return
  
  try {
    // Load TLS mode and cert stats in parallel
    const [modeRes, statsRes] = await Promise.all([
      api.get('/tls/mode'),
      api.get('/tls/certs/stats')
    ])
    
    const mode = modeRes.mode || 'insecure'
    const stats = statsRes

    // Defensive numeric coercion (API returns JSON numbers, but protect against stringy values)
    const verifiedCount = Number(stats.verified) || 0
    const pendingCount = Number(stats.pending) || 0
    const changedCount = Number(stats.changed) || 0
    const noCertCount = Number(stats.no_cert) || 0
    const totalPendingCount = pendingCount + changedCount
    
    container.innerHTML = `
      <div class="form-row">
        <div class="form-group" style="flex: 2">
          <label>TLS Verification Mode</label>
          <select id="tlsMode" class="select-full">
            <option value="insecure" ${mode === 'insecure' ? 'selected' : ''}>Insecure (Skip verification - legacy)</option>
            <option value="tofu" ${mode === 'tofu' ? 'selected' : ''}>TOFU (Trust-on-first-use, auto-pin)</option>
            <option value="strict" ${mode === 'strict' ? 'selected' : ''}>Strict (Require pinned & verified)</option>
          </select>
          <p class="settings-note">TOFU auto-pins certificates on first connection. Strict mode requires admin verification.</p>
        </div>
      </div>
      
      <div class="cert-stats">
        <div class="stat-row">
          <span class="stat-label">Pinned Certificates:</span>
          <span class="stat-value">${stats.total || 0}</span>
        </div>
        <div class="stat-row">
          <span class="stat-label">Verified:</span>
          <span class="stat-value stat-good">${verifiedCount}</span>
        </div>
        <div class="stat-row">
          <span class="stat-label">Pending Verification:</span>
          <span class="stat-value ${pendingCount > 0 ? 'stat-warn' : ''}">${pendingCount}</span>
        </div>
        <div class="stat-row">
          <span class="stat-label">Changed (need re-verify):</span>
          <span class="stat-value ${changedCount > 0 ? 'stat-error' : ''}">${changedCount}</span>
        </div>
        <div class="stat-row">
          <span class="stat-label">Devices without Cert:</span>
          <span class="stat-value">${noCertCount}</span>
        </div>
      </div>
      
      <div class="settings-actions" style="margin-top: 1rem; display: flex; gap: 0.5rem; flex-wrap: wrap;">
        <button class="btn btn-secondary" id="saveTLSMode">Save Mode</button>
        <button class="btn btn-secondary" id="bulkLearnCerts">
          Learn Missing Certs (${noCertCount})
        </button>
        <button class="btn btn-secondary" id="bulkVerifyCerts">
          Verify All Pending (${totalPendingCount})
        </button>
        <button class="btn btn-secondary" id="viewPendingCerts">
          Review Pending (${totalPendingCount})
        </button>
        <button class="btn btn-secondary" id="viewAllCerts">View All Certs...</button>
      </div>
    `
    
    // Save mode button
    document.getElementById('saveTLSMode')?.addEventListener('click', async () => {
      const btn = document.getElementById('saveTLSMode')
      const mode = document.getElementById('tlsMode').value
      btn.disabled = true
      btn.textContent = 'Saving...'
      
      try {
        await api.patch('/tls/mode', { mode })
        showToast('TLS mode updated', 'success')
      } catch (e) {
        showToast('Failed: ' + e.message, 'error')
      } finally {
        btn.disabled = false
        btn.textContent = 'Save Mode'
      }
    })
    
    // Bulk learn certs
    document.getElementById('bulkLearnCerts')?.addEventListener('click', async () => {
      const btn = document.getElementById('bulkLearnCerts')
      if (noCertCount === 0) {
        showToast('No missing certificates to learn.', 'info')
        return
      }
      btn.disabled = true
      btn.textContent = 'Learning...'
      
      try {
		// Backend expects verify_immediately (bool).
		const result = await api.post('/tls/certs/learn', { verify_immediately: true })
        showToast(`Learned ${result.learned} certs, ${result.failed} failed`, result.failed > 0 ? 'warning' : 'success')
        loadTLSSettings() // Refresh stats
      } catch (e) {
        showToast('Failed: ' + e.message, 'error')
      } finally {
        btn.disabled = false
        btn.textContent = `Learn Missing Certs (${noCertCount})`
      }
    })
    
    // Bulk verify certs
    document.getElementById('bulkVerifyCerts')?.addEventListener('click', async () => {
      const btn = document.getElementById('bulkVerifyCerts')
      if (totalPendingCount === 0) {
        showToast('No pending certificates to verify.', 'info')
        return
      }
      btn.disabled = true
      btn.textContent = 'Verifying...'
      
      try {
        const result = await api.post('/tls/certs/verify-all', {})
        showToast(`Verified ${result.verified} certificates`, 'success')
        loadTLSSettings() // Refresh stats
      } catch (e) {
        showToast('Failed: ' + e.message, 'error')
      } finally {
        btn.disabled = false
        btn.textContent = `Verify All Pending (${totalPendingCount})`
      }
    })
    
    // View pending certs
    document.getElementById('viewPendingCerts')?.addEventListener('click', () => {
      showCertModal('pending')
    })
    
    // View all certs
    document.getElementById('viewAllCerts')?.addEventListener('click', () => {
      showCertModal('all')
    })
    
  } catch (e) {
    container.innerHTML = `<p class="error">Error loading TLS settings: ${escapeHTML(e.message)}</p>`
  }
}

function closeCertModal() {
  // Defensive: remove ALL instances (previous UI allowed duplicates).
  document.querySelectorAll('#certModal').forEach(el => el.remove())
}

// Show certificate management modal
async function showCertModal(type) {
  // Never stack duplicate modals ("View All Certs" used to create multiple windows).
  closeCertModal()

  const endpoint = type === 'pending' ? '/tls/certs/pending' : '/tls/certs'
  const title = type === 'pending' ? 'Pending Certificates' : 'All Device Certificates'

  // Render immediately so the user gets feedback even if the API is slow.
  const shell = `
    <div class="modal" id="certModal">
      <div class="modal-content modal-large">
        <div class="modal-header">
          <h3 id="certModalTitle">${escapeHTML(title)}</h3>
          <button class="modal-close close-modal" aria-label="Close">&times;</button>
        </div>
        <div class="modal-body">
          <div id="certModalContent">
            <div class="loading">Loading certificates...</div>
          </div>
        </div>
        <div class="modal-footer">
          <button class="btn btn-secondary close-modal">Close</button>
        </div>
      </div>
    </div>
  `

  document.body.insertAdjacentHTML('beforeend', shell)

  const modal = document.getElementById('certModal')
  modal.querySelectorAll('.close-modal').forEach(btn => btn.addEventListener('click', closeCertModal))
  modal.addEventListener('click', (e) => {
    // Click-outside-to-close
    if (e.target === modal) closeCertModal()
  })

  const contentEl = document.getElementById('certModalContent')

  try {
    const result = await api.get(endpoint)
    const certs = result.certs || []

    if (!contentEl) return
    if (certs.length === 0) {
      contentEl.innerHTML = '<p>No certificates found.</p>'
      return
    }

    contentEl.innerHTML = `
      <table class="data-table">
        <thead>
          <tr>
            <th>Device</th>
            <th>Fingerprint</th>
            <th>Subject</th>
            <th>Expires</th>
            <th>Status</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          ${certs.map(c => {
            const status = c.changed_at ? 'changed' : (c.verified ? 'verified' : 'pending')
            const statusClass = status === 'verified' ? 'stat-good' : (status === 'changed' ? 'stat-error' : 'stat-warn')
            const expiry = c.not_after ? new Date(c.not_after).toLocaleDateString() : '-'
            const expired = c.not_after && new Date(c.not_after) < new Date()

            const fp = (c.fingerprint || '')
            const fpShort = fp.length > 16 ? fp.substring(0, 16) + '...' : fp

            const subj = (c.subject || '')
            const subjShort = subj.length > 40 ? subj.substring(0, 40) + '...' : subj

            return `
              <tr data-device-id="${escapeAttr(c.device_id || '')}">
                <td>
                  <strong>${escapeHTML(c.hostname || c.ip || 'Unknown')}</strong>
                  ${c.ip ? `<br><small>${escapeHTML(c.ip)}</small>` : ''}
                </td>
                <td><code title="${escapeAttr(fp)}">${escapeHTML(fpShort)}</code></td>
                <td title="${escapeAttr(subj)}">${escapeHTML(subjShort)}</td>
                <td class="${expired ? 'stat-error' : ''}">${escapeHTML(expiry)}</td>
                <td>
                  <span class="${statusClass}">${escapeHTML(status)}</span>
                  ${c.previous_fingerprint ? `<br><small>was: ${escapeHTML(c.previous_fingerprint.substring(0, 8))}...</small>` : ''}
                </td>
                <td>
                  ${(!c.verified || c.changed_at) ? `<button class="btn btn-sm btn-verify-cert" data-id="${escapeAttr(c.device_id || '')}">Verify</button>` : ''}
                  <button class="btn btn-sm btn-danger btn-unpin-cert" data-id="${escapeAttr(c.device_id || '')}">Unpin</button>
                </td>
              </tr>
            `
          }).join('')}
        </tbody>
      </table>
    `

    // Wire up verify buttons
    contentEl.querySelectorAll('.btn-verify-cert').forEach(btn => {
      btn.addEventListener('click', async () => {
        const deviceId = btn.dataset.id
        btn.disabled = true
        btn.textContent = '...'
        try {
          await api.post(`/devices/${deviceId}/cert/verify`, {})
          const statusSpan = btn.closest('tr')?.querySelector('td:nth-child(5) span')
          if (statusSpan) {
            statusSpan.textContent = 'verified'
            statusSpan.className = 'stat-good'
          }
          btn.remove()
          showToast('Certificate verified', 'success')
          loadTLSSettings?.()
        } catch (e) {
          showToast('Failed: ' + e.message, 'error')
          btn.disabled = false
          btn.textContent = 'Verify'
        }
      })
    })

    // Wire up unpin buttons
    contentEl.querySelectorAll('.btn-unpin-cert').forEach(btn => {
      btn.addEventListener('click', async () => {
        const deviceId = btn.dataset.id
        if (!confirm('Unpin this certificate? The device will need to be re-verified.')) return
        btn.disabled = true
        btn.textContent = '...'
        try {
          await api.delete(`/devices/${deviceId}/cert`)
          btn.closest('tr')?.remove()
          showToast('Certificate unpinned', 'success')
          loadTLSSettings?.()
        } catch (e) {
          showToast('Failed: ' + e.message, 'error')
          btn.disabled = false
          btn.textContent = 'Unpin'
        }
      })
    })

  } catch (e) {
    if (contentEl) {
      contentEl.innerHTML = `<p class="error">Failed to load certificates: ${escapeHTML(e.message)}</p>`
    }
    showToast('Failed to load certificates: ' + e.message, 'error')
  }
}

// Load user management section in settings
async function loadUserManagement() {
  const container = document.getElementById('userManagement')
  if (!container) return
  
  try {
    const users = await api.users()
    const roles = await api.roles()
    
    container.innerHTML = `
      <div class="user-add-form">
        <input type="text" id="newUsername" placeholder="Username" class="input-sm" />
        <input type="password" id="newPassword" placeholder="Password" class="input-sm" />
        <select id="newUserRole" class="select-sm">
          ${roles.map(r => `<option value="${escapeAttr(r.name)}" ${r.name === 'reader' ? 'selected' : ''}>${escapeHTML(r.name)}</option>`).join('')}
        </select>
        <button class="btn btn-primary btn-sm" id="addUserBtn">Add User</button>
      </div>
      <table class="user-table">
        <thead>
          <tr>
            <th>USERNAME</th>
            <th>ROLES</th>
            <th>STATUS</th>
            <th>ACTIONS</th>
          </tr>
        </thead>
        <tbody>
          ${users.map(u => `
            <tr data-id="${u.id}">
              <td>${escapeHTML(u.username)}</td>
              <td>${escapeHTML((u.roles || []).join(', ') || '-')}</td>
              <td>${u.status === 1 ? 'active' : 'disabled'}</td>
              <td class="user-actions">
                <select class="select-sm user-role-select" data-id="${u.id}">
                  ${roles.map(r => `<option value="${escapeAttr(r.name)}" ${(u.roles || []).includes(r.name) ? 'selected' : ''}>${escapeHTML(r.name)}</option>`).join('')}
                </select>
                <button class="btn btn-sm btn-toggle-user" data-id="${u.id}" data-status="${u.status}">${u.status === 1 ? 'disable' : 'enable'}</button>
                <button class="btn btn-sm btn-danger btn-delete-user" data-id="${u.id}">del</button>
              </td>
            </tr>
          `).join('')}
        </tbody>
      </table>
    `
    
    // Add user handler
    document.getElementById('addUserBtn')?.addEventListener('click', async () => {
      const username = document.getElementById('newUsername').value.trim()
      const password = document.getElementById('newPassword').value
      const role = document.getElementById('newUserRole').value
      
      if (!username || !password) {
        showToast('Username and password required', 'error')
        return
      }
      
      try {
        await api.createUser(username, password, [role])
        showToast('User created', 'success')
        loadUserManagement()
      } catch (e) {
        showToast('Failed: ' + e.message, 'error')
      }
    })
    
    // Role change handler
    container.querySelectorAll('.user-role-select').forEach(select => {
      select.addEventListener('change', async () => {
        const id = parseInt(select.dataset.id)
        const role = select.value
        
        try {
          await api.updateUser(id, { roles: [role] })
          showToast('Role updated', 'success')
        } catch (e) {
          showToast('Failed: ' + e.message, 'error')
          loadUserManagement()
        }
      })
    })
    
    // Toggle status handler
    container.querySelectorAll('.btn-toggle-user').forEach(btn => {
      btn.addEventListener('click', async () => {
        const id = parseInt(btn.dataset.id)
        const currentStatus = parseInt(btn.dataset.status)
        const newStatus = currentStatus === 1 ? 0 : 1
        
        try {
          await api.updateUser(id, { status: newStatus })
          showToast(`User ${newStatus === 1 ? 'enabled' : 'disabled'}`, 'success')
          loadUserManagement()
        } catch (e) {
          showToast('Failed: ' + e.message, 'error')
        }
      })
    })
    
    // Delete handler
    container.querySelectorAll('.btn-delete-user').forEach(btn => {
      btn.addEventListener('click', async () => {
        const id = parseInt(btn.dataset.id)
        if (!confirm('Delete this user?')) return
        
        try {
          await api.deleteUser(id)
          showToast('User deleted', 'success')
          loadUserManagement()
        } catch (e) {
          showToast('Failed: ' + e.message, 'error')
        }
      })
    })
  } catch (e) {
    container.innerHTML = `<p class="error">Error loading users: ${escapeHTML(e.message)}</p>`
  }
}

// Sites & Regions management
async function loadSitesRegionsManager() {
  const container = document.getElementById('sitesRegionsManager')
  if (!container) return
  
  try {
    const [regions, sites] = await Promise.all([
      api.regions(),
      api.sites()
    ])
    
    container.innerHTML = `
      <div class="sites-regions-manager">
        <div class="sr-tabs">
          <button class="sr-tab active" data-tab="regions">Regions</button>
          <button class="sr-tab" data-tab="sites">Sites</button>
          <button class="sr-tab" data-tab="assign">Assign Devices</button>
        </div>
        
        <div class="sr-content" id="sr-regions">
          <div class="sr-add-form">
            <input type="text" id="newRegionName" placeholder="Region name" class="input-sm" />
            <select id="newRegionParent" class="select-sm">
              <option value="">No parent</option>
              ${(regions || []).map(r => `<option value="${r.id}">${escapeHTML(r.name)}</option>`).join('')}
            </select>
            <button class="btn btn-sm btn-primary" id="addRegionBtn">Add Region</button>
          </div>
          <table class="sr-table">
            <thead><tr><th>Name</th><th>Parent</th><th>Sites</th><th></th></tr></thead>
            <tbody>
              ${(regions || []).length === 0 ? '<tr><td colspan="4" class="text-muted">No regions defined</td></tr>' : 
                (regions || []).map(r => `
                  <tr data-id="${r.id}">
                    <td><input type="text" class="inline-edit region-name" value="${escapeAttr(r.name)}" /></td>
                    <td>
                      <select class="inline-edit region-parent">
                        <option value="">None</option>
                        ${(regions || []).filter(rr => rr.id !== r.id).map(rr => 
                          `<option value="${rr.id}" ${r.parent_id === rr.id ? 'selected' : ''}>${escapeHTML(rr.name)}</option>`
                        ).join('')}
                      </select>
                    </td>
                    <td>${r.site_count || 0}</td>
                    <td><button class="btn btn-xs btn-danger delete-region" data-id="${r.id}">×</button></td>
                  </tr>
                `).join('')}
            </tbody>
          </table>
        </div>
        
        <div class="sr-content hidden" id="sr-sites">
          <div class="sr-add-form">
            <input type="text" id="newSiteName" placeholder="Site name" class="input-sm" />
            <select id="newSiteRegion" class="select-sm">
              <option value="">No region</option>
              ${(regions || []).map(r => `<option value="${r.id}">${escapeHTML(r.name)}</option>`).join('')}
            </select>
            <input type="text" id="newSiteAddress" placeholder="Address (optional)" class="input-sm" />
            <input type="number" step="0.000001" id="newSiteLat" placeholder="Lat" class="input-sm" style="width: 120px;" />
            <input type="number" step="0.000001" id="newSiteLon" placeholder="Lon" class="input-sm" style="width: 120px;" />
            <button class="btn btn-sm btn-primary" id="addSiteBtn">Add Site</button>
          </div>
          <table class="sr-table">
            <thead><tr><th>Name</th><th>Region</th><th>Address</th><th>Lat</th><th>Lon</th><th>Tower H (m)</th><th>Devices</th><th></th></tr></thead>
            <tbody>
              ${(sites || []).length === 0 ? '<tr><td colspan="8" class="text-muted">No sites defined</td></tr>' : 
                (sites || []).map(s => `
                  <tr data-id="${s.id}">
                    <td><input type="text" class="inline-edit site-name" value="${escapeAttr(s.name)}" /></td>
                    <td>
                      <select class="inline-edit site-region">
                        <option value="">None</option>
                        ${(regions || []).map(r => 
                          `<option value="${r.id}" ${s.region_id === r.id ? 'selected' : ''}>${escapeHTML(r.name)}</option>`
                        ).join('')}
                      </select>
                    </td>
                    <td><input type="text" class="inline-edit site-address" value="${escapeAttr(s.address || '')}" /></td>
                    <td><input type="number" step="0.000001" class="inline-edit site-lat" value="${escapeAttr(s.gps_lat ?? '')}" style="width: 120px;" /></td>
                    <td><input type="number" step="0.000001" class="inline-edit site-lon" value="${escapeAttr(s.gps_lon ?? '')}" style="width: 120px;" /></td>
                    <td><input type="number" step="0.1" class="inline-edit site-tower" value="${escapeAttr(s.tower_h_m ?? '')}" style="width: 110px;" /></td>
                    <td>${s.device_count || 0}</td>
                    <td><button class="btn btn-xs btn-danger delete-site" data-id="${s.id}">×</button></td>
                  </tr>
                `).join('')}
            </tbody>
          </table>
        </div>
        
        <div class="sr-content hidden" id="sr-assign">
          <p class="settings-note">Select devices and assign them to a site</p>
          <div class="sr-assign-controls">
            <select id="assignSiteSelect" class="select-sm">
              <option value="">Select site...</option>
              <option value="null">-- Unassign from site --</option>
              ${(sites || []).map(s => `<option value="${s.id}">${escapeHTML(s.name)}${s.region_name ? ' (' + escapeHTML(s.region_name) + ')' : ''}</option>`).join('')}
            </select>
            <button class="btn btn-sm btn-primary" id="assignDevicesBtn" disabled>Assign Selected</button>
            <span class="assign-count">0 selected</span>
          </div>
          <div class="sr-device-list" id="srDeviceList">
            Loading devices...
          </div>
        </div>
      </div>
    `
    
    // Tab switching
    container.querySelectorAll('.sr-tab').forEach(tab => {
      tab.addEventListener('click', () => {
        container.querySelectorAll('.sr-tab').forEach(t => t.classList.remove('active'))
        tab.classList.add('active')
        container.querySelectorAll('.sr-content').forEach(c => c.classList.add('hidden'))
        container.querySelector(`#sr-${tab.dataset.tab}`).classList.remove('hidden')
        
        if (tab.dataset.tab === 'assign') {
          loadDeviceAssignList()
        }
      })
    })
    
    // Add region
    document.getElementById('addRegionBtn')?.addEventListener('click', async () => {
      const name = document.getElementById('newRegionName').value.trim()
      if (!name) return showToast('Region name required', 'error')
      const parentId = document.getElementById('newRegionParent').value || null
      try {
        await api.createRegion({ name, parent_id: parentId ? parseInt(parentId) : null })
        showToast('Region created', 'success')
        loadSitesRegionsManager()
      } catch (e) {
        showToast('Failed: ' + e.message, 'error')
      }
    })
    
    // Add site
    document.getElementById('addSiteBtn')?.addEventListener('click', async () => {
      const name = document.getElementById('newSiteName').value.trim()
      if (!name) return showToast('Site name required', 'error')
      const regionId = document.getElementById('newSiteRegion').value || null
      const address = document.getElementById('newSiteAddress').value.trim()
      const lat = parseNullableFloat(document.getElementById('newSiteLat')?.value)
      const lon = parseNullableFloat(document.getElementById('newSiteLon')?.value)
      try {
        const payload = {
          name,
          region_id: regionId ? parseInt(regionId) : null,
          // CreateSite expects a string (not null). Empty string is fine; the server stores NULL.
          address: address,
        }
        if (lat !== null) payload.gps_lat = lat
        if (lon !== null) payload.gps_lon = lon

        await api.createSite(payload)
        showToast('Site created', 'success')
        loadSitesRegionsManager()
      } catch (e) {
        showToast('Failed: ' + e.message, 'error')
      }
    })
    
    // Inline edit regions
    container.querySelectorAll('.region-name, .region-parent').forEach(input => {
      input.addEventListener('change', async () => {
        const row = input.closest('tr')
        const id = row.dataset.id
        const name = row.querySelector('.region-name').value.trim()
        const parentId = row.querySelector('.region-parent').value || null
        try {
          await api.updateRegion(id, { name, parent_id: parentId ? parseInt(parentId) : null })
          showToast('Region updated', 'success')
        } catch (e) {
          showToast('Failed: ' + e.message, 'error')
          loadSitesRegionsManager()
        }
      })
    })
    
    // Inline edit sites
    container.querySelectorAll('.site-name, .site-region, .site-address, .site-lat, .site-lon, .site-tower').forEach(input => {
      input.addEventListener('change', async () => {
        const row = input.closest('tr')
        const id = row.dataset.id
        const name = row.querySelector('.site-name').value.trim()
        const regionId = row.querySelector('.site-region').value || null
        const address = row.querySelector('.site-address').value.trim()
        const gpsLat = parseNullableFloat(row.querySelector('.site-lat')?.value)
        const gpsLon = parseNullableFloat(row.querySelector('.site-lon')?.value)
        const towerHM = parseNullableFloat(row.querySelector('.site-tower')?.value)
        try {
          await api.updateSite(id, {
            name,
            region_id: regionId ? parseInt(regionId) : null,
            address: address || null,
            gps_lat: gpsLat,
            gps_lon: gpsLon,
            tower_h_m: towerHM,
          })
          showToast('Site updated', 'success')
        } catch (e) {
          showToast('Failed: ' + e.message, 'error')
          loadSitesRegionsManager()
        }
      })
    })
    
    // Delete region
    container.querySelectorAll('.delete-region').forEach(btn => {
      btn.addEventListener('click', async () => {
        if (!confirm('Delete this region? Sites will be unassigned.')) return
        try {
          await api.deleteRegion(btn.dataset.id)
          showToast('Region deleted', 'success')
          loadSitesRegionsManager()
        } catch (e) {
          showToast('Failed: ' + e.message, 'error')
        }
      })
    })
    
    // Delete site
    container.querySelectorAll('.delete-site').forEach(btn => {
      btn.addEventListener('click', async () => {
        if (!confirm('Delete this site? Devices will be unassigned.')) return
        try {
          await api.deleteSite(btn.dataset.id)
          showToast('Site deleted', 'success')
          loadSitesRegionsManager()
        } catch (e) {
          showToast('Failed: ' + e.message, 'error')
        }
      })
    })
    
  } catch (e) {
    container.innerHTML = `<p class="error">Error loading sites/regions: ${escapeHTML(e.message)}</p>`
  }
}

// Load device list for site assignment
async function loadDeviceAssignList() {
  const container = document.getElementById('srDeviceList')
  if (!container) return
  
  const devices = store.devices || []
  const sites = await api.sites()
  const siteMap = {}
  for (const s of (sites || [])) {
    siteMap[s.id] = s.name
  }
  
  // Only show APs and unparented STAs
  const assignable = devices.filter(d => !d.parent_id)
  
  container.innerHTML = `
    <div class="sr-device-filter">
      <input type="text" id="srDeviceFilter" placeholder="Filter devices..." class="input-sm" />
      <label><input type="checkbox" id="srShowAll" /> Show STAs too</label>
    </div>
    <div class="sr-device-table-wrap">
      <table class="sr-table sr-device-table">
        <thead><tr><th><input type="checkbox" id="srSelectAll" /></th><th>Device</th><th>IP</th><th>Current Site</th></tr></thead>
        <tbody>
          ${assignable.map(d => `
            <tr data-id="${d.id}">
              <td><input type="checkbox" class="sr-device-cb" data-id="${d.id}" /></td>
              <td>${escapeHTML(d.hostname || d.product || 'Unknown')}</td>
              <td>${escapeHTML(d.ip_address || '-')}</td>
              <td>${d.site_id ? escapeHTML(siteMap[d.site_id] || 'Unknown') : '<span class="text-muted">None</span>'}</td>
            </tr>
          `).join('')}
        </tbody>
      </table>
    </div>
  `
  
  // Filter
  document.getElementById('srDeviceFilter')?.addEventListener('input', (e) => {
    const filter = e.target.value.toLowerCase()
    container.querySelectorAll('tbody tr').forEach(row => {
      const text = row.textContent.toLowerCase()
      row.style.display = text.includes(filter) ? '' : 'none'
    })
  })
  
  // Select all
  document.getElementById('srSelectAll')?.addEventListener('change', (e) => {
    container.querySelectorAll('.sr-device-cb').forEach(cb => {
      if (cb.closest('tr').style.display !== 'none') {
        cb.checked = e.target.checked
      }
    })
    updateAssignCount()
  })
  
  // Individual selection
  container.querySelectorAll('.sr-device-cb').forEach(cb => {
    cb.addEventListener('change', updateAssignCount)
  })
  
  // Assign button
  document.getElementById('assignDevicesBtn')?.addEventListener('click', async () => {
    const siteId = document.getElementById('assignSiteSelect').value
    if (!siteId) return showToast('Select a site first', 'error')
    
    const selected = [...container.querySelectorAll('.sr-device-cb:checked')].map(cb => parseInt(cb.dataset.id))
    if (selected.length === 0) return showToast('No devices selected', 'error')
    
    try {
      const siteVal = siteId === 'null' ? null : parseInt(siteId)
      await api.bulkAssignSite(selected, siteVal)
      showToast(`${selected.length} device(s) assigned`, 'success')
      await refreshDevices()
      loadDeviceAssignList()
    } catch (e) {
      showToast('Failed: ' + e.message, 'error')
    }
  })
  
  // Site select enables button
  document.getElementById('assignSiteSelect')?.addEventListener('change', updateAssignCount)
}

function updateAssignCount() {
  const selected = document.querySelectorAll('#srDeviceList .sr-device-cb:checked').length
  const countEl = document.querySelector('.assign-count')
  const btn = document.getElementById('assignDevicesBtn')
  if (countEl) countEl.textContent = `${selected} selected`
  if (btn) btn.disabled = selected === 0 || !document.getElementById('assignSiteSelect')?.value
}

// Scheduler page - hierarchical device picker and job management
async function renderSchedulerPage(container) {
  container.innerHTML = `
    <div class="page scheduler-page">
      <h3>Scheduler</h3>
      <div class="tabs scheduler-tabs">
        <button class="tab active" data-tab="jobs">Jobs</button>
        <button class="tab" data-tab="maintenance">Maintenance Windows</button>
        <button class="tab" data-tab="settings">Settings</button>
      </div>
      
      <div class="tab-content" id="tab-jobs">
        <div class="scheduler-layout">
          <div class="scheduler-picker">
            <h4>Select Targets</h4>
            <div class="picker-search">
              <input type="text" id="pickerSearch" placeholder="Search..." />
            </div>
            <div class="picker-tree" id="pickerTree">Loading...</div>
            <div class="picker-selection">
              <span id="selectionCount">0 selected</span>
              <button class="btn btn-sm" id="clearSelection">Clear</button>
            </div>
          </div>
          <div class="scheduler-actions">
            <h4>Schedule Job</h4>
            <div class="form-group">
              <label>Job Type</label>
              <select id="jobType">
                <option value="upgrade">Firmware Upgrade</option>
                <option value="reboot">Reboot</option>
              </select>
            </div>
            <div class="form-group" id="firmwareSelectGroup">
              <label>Target Version</label>
              <select id="firmwareSelect">
                <option value="">Select version (auto-matches device type)</option>
              </select>
              <small class="form-hint">System automatically selects correct firmware file for each device type</small>
            </div>
            <div class="form-group">
              <label>Schedule</label>
              <select id="scheduleType">
                <option value="now">Run Now</option>
                <option value="later">Schedule for Later</option>
                <option value="recurring">Recurring</option>
              </select>
            </div>
            <div class="form-group hidden" id="scheduleTimeGroup">
              <label>Date/Time</label>
              <input type="datetime-local" id="scheduleTime" />
            </div>
            <div class="form-group hidden" id="repeatGroup">
              <label>Repeat</label>
              <select id="repeatCron">
                <option value="@daily">Daily</option>
                <option value="@weekly">Weekly</option>
                <option value="12h">Every 12 hours</option>
                <option value="24h">Every 24 hours</option>
                <option value="168h">Every week</option>
              </select>
            </div>
            <div class="form-check" id="fanoutGroup">
              <input type="checkbox" id="jobFanout" />
              <label for="jobFanout">Fanout (include connected STAs when upgrading APs)</label>
            </div>
            <div class="form-check">
              <input type="checkbox" id="jobForce" />
              <label for="jobForce">Force (upgrade even if same version)</label>
            </div>
            <button class="btn btn-primary" id="createJob">Create Job</button>
          </div>
          <div class="scheduler-jobs">
            <div class="jobs-header">
              <h4>Scheduled Jobs</h4>
              <div class="jobs-toolbar">
                <div class="jobs-filter">
                  <label for="jobsStatusFilter">Status</label>
                  <select id="jobsStatusFilter">
                    <option value="all">All</option>
                    <option value="active">Active</option>
                    <option value="pending">Pending</option>
                    <option value="blocked">Blocked</option>
                    <option value="running">Running</option>
                    <option value="done">Done</option>
                    <option value="completed">Completed</option>
                    <option value="failed">Failed</option>
                    <option value="cancelled">Cancelled</option>
                  </select>
                </div>
                <button class="btn btn-secondary btn-sm" id="refreshJobs">Refresh</button>
              </div>
            </div>
            <div id="jobsList">Loading...</div>
          </div>
        </div>
      </div>
      
      <div class="tab-content hidden" id="tab-maintenance">
        <div class="maintenance-header">
          <h4>Maintenance Windows</h4>
          <button class="btn btn-primary" id="addMaintenanceWindow">Add Window</button>
        </div>
        <p class="help-text">Jobs will only run during active maintenance windows for their target region/site.</p>
        <div id="maintenanceList">Loading...</div>
      </div>
      
      <div class="tab-content hidden" id="tab-settings">
        <h4>Scheduler Settings</h4>
        <div class="settings-form" id="schedulerSettings">Loading...</div>
      </div>
    </div>
  `
  
  // Tab switching
  container.querySelectorAll('.scheduler-tabs .tab').forEach(tab => {
    tab.addEventListener('click', () => {
      container.querySelectorAll('.scheduler-tabs .tab').forEach(t => t.classList.remove('active'))
      container.querySelectorAll('.tab-content').forEach(c => c.classList.add('hidden'))
      tab.classList.add('active')
      document.getElementById('tab-' + tab.dataset.tab)?.classList.remove('hidden')
      
      // Load tab content
      if (tab.dataset.tab === 'maintenance') loadMaintenanceWindows()
      if (tab.dataset.tab === 'settings') loadSchedulerSettings()
    })
  })
  
  // Load hierarchical tree data
  await loadPickerTree()
  
  // Load firmware list
  await loadFirmwareList()
  
  // Load existing jobs
  await loadScheduledJobs()

  // Jobs filter + refresh
  document.getElementById('jobsStatusFilter')?.addEventListener('change', () => {
    // Re-render from cached jobs without refetch
    loadScheduledJobs(window.__scheduledJobsAll || [])
  })
  document.getElementById('refreshJobs')?.addEventListener('click', () => loadScheduledJobs())
  
  // Event handlers
  document.getElementById('pickerSearch')?.addEventListener('input', filterPickerTree)
  document.getElementById('clearSelection')?.addEventListener('click', clearPickerSelection)
  document.getElementById('scheduleType')?.addEventListener('change', (e) => {
    const isLater = e.target.value === 'later' || e.target.value === 'recurring'
    const isRecurring = e.target.value === 'recurring'
    document.getElementById('scheduleTimeGroup')?.classList.toggle('hidden', !isLater)
    document.getElementById('repeatGroup')?.classList.toggle('hidden', !isRecurring)
  })
  document.getElementById('jobType')?.addEventListener('change', (e) => {
    document.getElementById('firmwareSelectGroup').classList.toggle('hidden', e.target.value !== 'upgrade')
  })
  document.getElementById('createJob')?.addEventListener('click', createScheduledJob)
  document.getElementById('addMaintenanceWindow')?.addEventListener('click', () => showMaintenanceWindowModal())
}

// Picker tree state
let pickerSelection = new Set()

async function loadPickerTree() {
  const container = document.getElementById('pickerTree')
  if (!container) return
  
  try {
    // Get regions, sites, and devices
    const [regions, sites] = await Promise.all([
      api.regions(),
      api.sites()
    ])
    const devices = store.devices
    
    // Build hierarchy: Region > Site > AP > STA
    let html = ''
    
    // Regions
    for (const region of (regions || [])) {
      const regionSites = (sites || []).filter(s => s.region_id === region.id)
      const hasDevices = regionSites.some(s => devices.some(d => d.site_id === s.id))
      
      html += `
        <div class="picker-region" data-region="${region.id}">
          <div class="picker-row">
            <input type="checkbox" class="picker-cb region-cb" data-type="region" data-id="${region.id}" />
            <span class="picker-toggle">></span>
            <span class="picker-label">[D] ${escapeHTML(region.name)}</span>
          </div>
          <div class="picker-children hidden">
      `
      
      // Sites in region
      for (const site of regionSites) {
        const siteDevices = devices.filter(d => d.site_id === site.id && !d.parent_id)
        html += `
          <div class="picker-site" data-site="${site.id}">
            <div class="picker-row">
              <input type="checkbox" class="picker-cb site-cb" data-type="site" data-id="${site.id}" />
              <span class="picker-toggle">></span>
              <span class="picker-label">[S] ${escapeHTML(site.name)}</span>
              <span class="picker-count">${siteDevices.length}</span>
            </div>
            <div class="picker-children hidden">
              ${renderPickerDevices(siteDevices, devices)}
            </div>
          </div>
        `
      }
      
      html += `</div></div>`
    }
    
    // Unassigned sites (no region)
    const unassignedSites = (sites || []).filter(s => !s.region_id)
    if (unassignedSites.length > 0) {
      html += `<div class="picker-section"><strong>Unassigned Sites</strong></div>`
      for (const site of unassignedSites) {
        const siteDevices = devices.filter(d => d.site_id === site.id && !d.parent_id)
        html += `
          <div class="picker-site" data-site="${site.id}">
            <div class="picker-row">
              <input type="checkbox" class="picker-cb site-cb" data-type="site" data-id="${site.id}" />
              <span class="picker-toggle">></span>
              <span class="picker-label">[S] ${escapeHTML(site.name)}</span>
              <span class="picker-count">${siteDevices.length}</span>
            </div>
            <div class="picker-children hidden">
              ${renderPickerDevices(siteDevices, devices)}
            </div>
          </div>
        `
      }
    }
    
    // Unassigned devices (no site)
    const unassignedAPs = devices.filter(d => !d.site_id && !d.parent_id)
    if (unassignedAPs.length > 0) {
      html += `<div class="picker-section"><strong>Unassigned Devices</strong></div>`
      html += renderPickerDevices(unassignedAPs, devices)
    }
    
    container.innerHTML = html || '<p class="muted">No devices found</p>'
    
    // Attach event handlers
    container.querySelectorAll('.picker-toggle').forEach(toggle => {
      toggle.addEventListener('click', (e) => {
        const row = e.target.closest('.picker-row')
        const children = row.nextElementSibling
        if (children) {
          children.classList.toggle('hidden')
          e.target.textContent = children.classList.contains('hidden') ? '>' : 'v'
        }
      })
    })
    
    container.querySelectorAll('.picker-cb').forEach(cb => {
      cb.addEventListener('change', handlePickerSelection)
    })
    
  } catch (e) {
    container.innerHTML = `<p class="error">Error: ${escapeHTML(e.message)}</p>`
  }
}

function renderPickerDevices(aps, allDevices) {
  let html = ''
  for (const ap of aps) {
    const stas = allDevices.filter(d => d.parent_id === ap.id)
    const name = escapeHTML(ap.hostname || ap.ip_address || ap.mac)
    html += `
      <div class="picker-ap" data-ap="${ap.id}">
        <div class="picker-row">
          <input type="checkbox" class="picker-cb ap-cb" data-type="ap" data-id="${ap.id}" />
          ${stas.length > 0 ? '<span class="picker-toggle">></span>' : '<span class="picker-spacer"></span>'}
          <span class="picker-badge ap">AP</span>
          <span class="picker-label">${name}</span>
          ${stas.length > 0 ? `<span class="picker-count">${stas.length} STAs</span>` : ''}
        </div>
    `
    if (stas.length > 0) {
      html += `<div class="picker-children hidden">`
      for (const sta of stas) {
        const staName = escapeHTML(sta.hostname || sta.ip_address || sta.mac)
        html += `
          <div class="picker-sta" data-sta="${sta.id}">
            <div class="picker-row">
              <input type="checkbox" class="picker-cb sta-cb" data-type="sta" data-id="${sta.id}" />
              <span class="picker-spacer"></span>
              <span class="picker-badge sta">STA</span>
              <span class="picker-label">${staName}</span>
            </div>
          </div>
        `
      }
      html += `</div>`
    }
    html += `</div>`
  }
  return html
}

function handlePickerSelection(e) {
  const cb = e.target
  const type = cb.dataset.type
  const id = parseInt(cb.dataset.id)
  
  if (cb.checked) {
    // Select this and all children
    if (type === 'region') {
      // Select all sites in region, and all APs/STAs in those sites
      const regionEl = cb.closest('.picker-region')
      regionEl.querySelectorAll('.picker-cb').forEach(child => {
        child.checked = true
        pickerSelection.add(`${child.dataset.type}:${child.dataset.id}`)
      })
    } else if (type === 'site') {
      const siteEl = cb.closest('.picker-site')
      siteEl.querySelectorAll('.picker-cb').forEach(child => {
        child.checked = true
        pickerSelection.add(`${child.dataset.type}:${child.dataset.id}`)
      })
    } else if (type === 'ap') {
      const apEl = cb.closest('.picker-ap')
      apEl.querySelectorAll('.picker-cb').forEach(child => {
        child.checked = true
        pickerSelection.add(`${child.dataset.type}:${child.dataset.id}`)
      })
    } else {
      pickerSelection.add(`${type}:${id}`)
    }
  } else {
    // Deselect this and all children
    if (type === 'region') {
      const regionEl = cb.closest('.picker-region')
      regionEl.querySelectorAll('.picker-cb').forEach(child => {
        child.checked = false
        pickerSelection.delete(`${child.dataset.type}:${child.dataset.id}`)
      })
    } else if (type === 'site') {
      const siteEl = cb.closest('.picker-site')
      siteEl.querySelectorAll('.picker-cb').forEach(child => {
        child.checked = false
        pickerSelection.delete(`${child.dataset.type}:${child.dataset.id}`)
      })
    } else if (type === 'ap') {
      const apEl = cb.closest('.picker-ap')
      apEl.querySelectorAll('.picker-cb').forEach(child => {
        child.checked = false
        pickerSelection.delete(`${child.dataset.type}:${child.dataset.id}`)
      })
    } else {
      pickerSelection.delete(`${type}:${id}`)
    }
  }
  
  updateSelectionCount()
}

function updateSelectionCount() {
  const count = pickerSelection.size
  const el = document.getElementById('selectionCount')
  if (el) el.textContent = `${count} selected`
}

function clearPickerSelection() {
  pickerSelection.clear()
  document.querySelectorAll('.picker-cb').forEach(cb => cb.checked = false)
  updateSelectionCount()
}

function filterPickerTree() {
  const query = document.getElementById('pickerSearch')?.value.toLowerCase() || ''
  document.querySelectorAll('.picker-row').forEach(row => {
    const label = row.querySelector('.picker-label')?.textContent.toLowerCase() || ''
    const parent = row.closest('.picker-region, .picker-site, .picker-ap, .picker-sta')
    if (parent) {
      parent.style.display = label.includes(query) ? '' : 'none'
    }
  })
}

async function loadFirmwareList() {
  const select = document.getElementById('firmwareSelect')
  if (!select) return
  
  try {
    const versions = await api.firmwareVersions()
    select.innerHTML = '<option value="">Select version...</option>'
    for (const v of (versions || [])) {
      const flavorList = v.flavors?.join(', ') || ''
      const label = `${v.version} (${v.platform || 'unknown'})`
      select.innerHTML += `<option value="${escapeAttr(v.version)}" title="Available for: ${escapeAttr(flavorList)}">${escapeHTML(label)}</option>`
    }
  } catch (e) {
    console.error('Firmware versions error:', e)
    select.innerHTML = '<option value="">Error loading firmware</option>'
  }
}

async function createScheduledJob() {
  const jobType = document.getElementById('jobType')?.value
  const firmware = document.getElementById('firmwareSelect')?.value
  const scheduleType = document.getElementById('scheduleType')?.value
  const scheduleTime = document.getElementById('scheduleTime')?.value
  
  // Get selected device IDs (only APs and STAs, not regions/sites)
  const deviceIds = []
  pickerSelection.forEach(key => {
    const [type, id] = key.split(':')
    if (type === 'ap' || type === 'sta') {
      deviceIds.push(parseInt(id))
    }
  })
  
  if (deviceIds.length === 0) {
    showToast('Select at least one device', 'error')
    return
  }
  
  if (jobType === 'upgrade' && !firmware) {
    showToast('Select firmware version for upgrade', 'error')
    return
  }
  
  try {
    const scheduledAt = scheduleType === 'now' ? new Date().toISOString() : new Date(scheduleTime).toISOString()
    // Send firmware_version - backend resolves correct file per device flavor
    const params = jobType === 'upgrade' ? { firmware_version: firmware } : {}
    
    await api.createJob(jobType, deviceIds, params, scheduledAt, '')
    
    showToast(`Job created for ${deviceIds.length} devices`, 'success')
    clearPickerSelection()
    await loadScheduledJobs()
  } catch (e) {
    showToast('Failed: ' + e.message, 'error')
  }
}

// Logs page
async function renderLogsPage(container) {
  container.innerHTML = `
    <div class="page">
      <table class="logs-table">
        <thead>
          <tr>
            <th>Time</th>
            <th>Device</th>
            <th>Action</th>
            <th>User</th>
          </tr>
        </thead>
        <tbody id="logsBody">
          <tr><td colspan="4">Loading...</td></tr>
        </tbody>
      </table>
    </div>
  `
  
  try {
    const logs = await api.logs()
    renderLogs(document.getElementById('logsBody'), logs)
  } catch (e) {
    document.getElementById('logsBody').innerHTML = `<tr><td colspan="4">Error: ${escapeHTML(e.message)}</td></tr>`
  }
}

// Format file size
function formatSize(bytes) {
  if (bytes < 1024) return bytes + ' B'
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB'
  return (bytes / (1024 * 1024)).toFixed(1) + ' MB'
}

// ===== MAP PAGE =====
let mapInstance = null
let mapLinksLayer = null  // Layer group for links - can be toggled without reinit
let mapMarkersLayer = null
let mapFilterMode = 'all' // 'all', 'aps', 'stas'

function updateMapMarkerSizing() {
  if (!mapInstance) return
  const container = document.getElementById('mapContainer')
  if (!container) return

  const zoom = mapInstance.getZoom?.() ?? 0

  // Scale dots down as you zoom out, but never too small to see.
  // The current UI is tuned for typical Leaflet zoom ranges.
  const size = Math.max(6, Math.min(16, (2 * zoom) - 4))
  const border = size >= 12 ? 2 : 1

  container.style.setProperty('--map-dot-size', `${size}px`)
  container.style.setProperty('--map-dot-border', `${border}px`)
}

function renderMapPage(container) {
  container.innerHTML = `
    <div class="page page-map">
      <div class="map-toolbar">
        <div class="btn-group map-filter-group">
          <button class="btn btn-secondary active" id="mapShowAll">Show All</button>
          <button class="btn btn-secondary" id="mapShowAPs">Show APs</button>
          <button class="btn btn-secondary" id="mapShowSTAs">Show STAs</button>
        </div>
        <label class="form-check-inline">
          <input type="checkbox" id="mapShowLinks" checked /> Show Links
        </label>
        <button class="btn btn-secondary" id="mapCenterAll">Center All</button>
        <div class="map-toolbar-spacer"></div>
        <button class="btn btn-secondary" id="mapExportKMZ">Export KMZ</button>
      </div>
      <div id="mapContainer" class="map-container"></div>
    </div>
  `
  
  // Set up filter button handlers
  document.getElementById('mapShowAll')?.addEventListener('click', () => setMapFilter('all'))
  document.getElementById('mapShowAPs')?.addEventListener('click', () => setMapFilter('aps'))
  document.getElementById('mapShowSTAs')?.addEventListener('click', () => setMapFilter('stas'))
  
  // Initialize map
  setTimeout(() => initMap(), 100)
}

function setMapFilter(mode) {
  mapFilterMode = mode
  // Update button states
  document.querySelectorAll('.map-filter-group .btn').forEach(btn => btn.classList.remove('active'))
  if (mode === 'all') document.getElementById('mapShowAll')?.classList.add('active')
  else if (mode === 'aps') document.getElementById('mapShowAPs')?.classList.add('active')
  else if (mode === 'stas') document.getElementById('mapShowSTAs')?.classList.add('active')
  // Reinit map with new filter
  initMap()
}

function initMap() {
  const container = document.getElementById('mapContainer')
  if (!container) {
    console.error('Map: container not found')
    return
  }
  if (!window.L) {
    console.error('Map: Leaflet not loaded')
    container.innerHTML = '<p style="padding:1rem;color:var(--text-2)">Leaflet not loaded. Check network connection.</p>'
    return
  }
  
  // Destroy existing map
  if (mapInstance) {
    mapInstance.remove()
    mapInstance = null
    mapLinksLayer = null
    mapMarkersLayer = null
  }
  
  // Get filter from store
  const filter = store.treeFilter || ''
  
  // Filter devices based on tree filter, status filters, and AP/STA mode
  const filteredDevices = store.devices.filter(d => {
    // Apply AP/STA filter mode
    const isAP = !d.parent_id
    if (mapFilterMode === 'aps' && !isAP) return false
    if (mapFilterMode === 'stas' && isAP) return false
    
    // Apply status filter
    const status = store.getStatus(d)
    if (status === 'online' && !store.filters.online) return false
    if (status === 'offline' && !store.filters.offline) return false
    if (status === 'unknown' && !store.filters.unknown) return false
    
    // Apply search filter
    if (filter) {
      const matchesDevice = (d.hostname || '').toLowerCase().includes(filter) ||
                            (d.ip_address || '').includes(filter) ||
                            (d.site_name || '').toLowerCase().includes(filter)
      // Also include if parent AP matches (show AP and all its STAs)
      if (d.parent_id) {
        const parent = store.devices.find(p => p.id === d.parent_id)
        if (parent) {
          const parentMatches = (parent.hostname || '').toLowerCase().includes(filter) ||
                                (parent.ip_address || '').includes(filter)
          if (parentMatches) return true
        }
      }
      // Include APs if any of their STAs match
      if (!d.parent_id) {
        const stas = store.devices.filter(s => s.parent_id === d.id)
        const staMatches = stas.some(s => 
          (s.hostname || '').toLowerCase().includes(filter) ||
          (s.ip_address || '').includes(filter)
        )
        if (staMatches) return true
      }
      return matchesDevice
    }
    
    return true
  })
  
  console.log('Map: filtered devices =', filteredDevices.length, 'of', store.devices.length)
  
  if (filteredDevices.length === 0) {
    container.innerHTML = `
      <div style="display:flex;align-items:center;justify-content:center;height:100%;color:var(--text-2);flex-direction:column;gap:1rem">
        <p>${filter ? 'No devices match filter.' : 'No devices loaded yet.'}</p>
        <p style="font-size:0.85rem">${filter ? 'Try a different search term.' : 'Devices will appear once polled.'}</p>
      </div>
    `
    return
  }
  
  // Store filtered devices for export
  window._mapFilteredDevices = filteredDevices
  
  // Create map centered on first device with GPS or default
  const devicesWithGPS = filteredDevices.filter(d => d.gps_lat && d.gps_lon)
  console.log('Map: devices with GPS =', devicesWithGPS.length)
  
  // Show message if no devices have GPS
  if (devicesWithGPS.length === 0) {
    container.innerHTML = `
      <div style="display:flex;align-items:center;justify-content:center;height:100%;color:var(--text-2);flex-direction:column;gap:1rem">
        <p>No ${filter ? 'matching ' : ''}devices with GPS coordinates.</p>
        <p style="font-size:0.85rem">Configure GPS coordinates on your devices to see them on the map.</p>
      </div>
    `
    return
  }
  
  const center = [devicesWithGPS[0].gps_lat, devicesWithGPS[0].gps_lon]
  
  try {
    mapInstance = L.map('mapContainer').setView(center, 10)

    // Keep marker dots scaled relative to zoom level (smaller when zoomed out).
    mapInstance.on('zoom', updateMapMarkerSizing)
    mapInstance.on('zoomend', updateMapMarkerSizing)
    updateMapMarkerSizing()
    
    // Dark tile layer
    L.tileLayer('https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png', {
      attribution: '&copy; OpenStreetMap, &copy; CARTO',
      maxZoom: 19
    }).addTo(mapInstance)
    
    // Create layer groups
    mapMarkersLayer = L.layerGroup().addTo(mapInstance)
    mapLinksLayer = L.layerGroup().addTo(mapInstance)
    
    // Add device markers (only filtered devices)
    const markers = []
    const apMarkers = {}
    
    // Build set of filtered device IDs for quick lookup
    const filteredIds = new Set(filteredDevices.map(d => d.id))
    
    filteredDevices.forEach(device => {
      if (!device.gps_lat || !device.gps_lon) return
      
      const isAP = !device.parent_id
      const isOnline = store.getStatus(device) === 'online'
      
      const color = isOnline ? (isAP ? '#00c853' : '#2196f3') : '#f44336'
      const icon = L.divIcon({
        className: 'map-marker',
        html: `<div class="marker-dot" style="background:${color}"></div>`,
        iconSize: [16, 16],
        iconAnchor: [8, 8]
      })
      
      const productInfo = device.product || device.model || ''
      const marker = L.marker([device.gps_lat, device.gps_lon], { icon })
        .addTo(mapMarkersLayer)
        .bindPopup(`
          <strong>${escapeHTML(device.hostname || device.ip_address)}</strong><br>
          ${productInfo ? escapeHTML(productInfo) + '<br>' : ''}IP: ${escapeHTML(device.ip_address)}<br>
          Signal: ${getDeviceSignal(device) || 'N/A'} dBm
        `)
      
      markers.push(marker)
      if (isAP) apMarkers[device.id] = marker
    })
    
    console.log('Map: markers added =', markers.length)
    
    // Draw links between APs and STAs (into links layer) - only for filtered devices
    filteredDevices.forEach(device => {
      if (!device.parent_id || !device.gps_lat || !device.gps_lon) return
      
      // Only draw link if parent is also in filtered set
      if (!filteredIds.has(device.parent_id)) return
      
      const parentMarker = apMarkers[device.parent_id]
      if (!parentMarker) return
      
      const parentLatLng = parentMarker.getLatLng()
      const signal = getDeviceSignal(device) || -70
      const band = getDeviceBand(device)
      // Prefer server-computed quality (Go > JS)
      const serverQuality = device.radio_60ghz?.signal_quality || device.radio_5ghz?.signal_quality || device.radio_ltu?.signal_quality
      const quality = serverQuality || getSignalQuality(signal, band)
      const color = quality === 'excellent' || quality === 'good' ? '#00c853' : quality === 'fair' ? '#ffc107' : '#f44336'
      
      L.polyline(
        [[parentLatLng.lat, parentLatLng.lng], [device.gps_lat, device.gps_lon]],
        { color, weight: 2, opacity: 0.7 }
      ).addTo(mapLinksLayer)
    })
    
    // Apply initial show/hide state for links
    const showLinksCheckbox = document.getElementById('mapShowLinks')
    if (showLinksCheckbox && !showLinksCheckbox.checked) {
      mapInstance.removeLayer(mapLinksLayer)
    }
    
    // Center all button
    document.getElementById('mapCenterAll')?.addEventListener('click', () => {
      if (markers.length > 0) {
        const group = L.featureGroup(markers)
        mapInstance.fitBounds(group.getBounds().pad(0.1))
      }
    })
    
    // Toggle links - just show/hide the layer, don't reinit
    document.getElementById('mapShowLinks')?.addEventListener('change', (e) => {
      if (!mapInstance || !mapLinksLayer) return
      if (e.target.checked) {
        mapLinksLayer.addTo(mapInstance)
      } else {
        mapInstance.removeLayer(mapLinksLayer)
      }
    })
    
    // Force resize after a moment
    setTimeout(() => {
      if (mapInstance) mapInstance.invalidateSize()
    }, 200)
    
    // KMZ Export button handlers
    document.getElementById('mapExportKMZ')?.addEventListener('click', () => exportMapKMZ())
    
  } catch (e) {
    console.error('Map error:', e)
    container.innerHTML = `<p style="padding:1rem;color:var(--red)">Map error: ${escapeHTML(e.message)}</p>`
  }
}

// Generate KML content for devices
function generateKML(devices, title) {
  const placemarks = devices.map(device => {
    const name = device.hostname || device.ip_address || 'Unknown'
    const isAP = !device.parent_id
    const isOnline = store.getStatus(device) === 'online'
    const signal = getDeviceSignal(device)
    
    // Build description with device details
    const descParts = [
      `Type: ${isAP ? 'AP' : 'STA'}`,
      `Status: ${isOnline ? 'Online' : 'Offline'}`,
      `IP: ${device.ip_address || 'N/A'}`,
      `MAC: ${device.mac || 'N/A'}`,
      `Product: ${device.product || device.model || 'N/A'}`
    ]
    if (signal) descParts.push(`Signal: ${signal} dBm`)
    if (device.site_name) descParts.push(`Site: ${device.site_name}`)
    if (device.distance) descParts.push(`Distance: ${(device.distance/1000).toFixed(2)} km`)
    
    const description = descParts.join('\n')
    
    // Icon style based on device type and status
    const iconColor = isOnline ? (isAP ? 'ff00c853' : 'ff2196f3') : 'fff44336'  // AABBGGRR format
    const iconScale = isAP ? 1.2 : 0.9
    
    return `
    <Placemark>
      <name>${escapeXML(name)}</name>
      <description>${escapeXML(description)}</description>
      <Style>
        <IconStyle>
          <color>${iconColor}</color>
          <scale>${iconScale}</scale>
          <Icon>
            <href>http://maps.google.com/mapfiles/kml/shapes/placemark_circle.png</href>
          </Icon>
        </IconStyle>
        <LabelStyle>
          <scale>0.8</scale>
        </LabelStyle>
      </Style>
      <Point>
        <coordinates>${device.gps_lon},${device.gps_lat},0</coordinates>
      </Point>
    </Placemark>`
  }).join('\n')
  
  return `<?xml version="1.0" encoding="UTF-8"?>
<kml xmlns="http://www.opengis.net/kml/2.2">
  <Document>
    <name>${escapeXML(title)}</name>
    <description>Exported from waveControl</description>
    ${placemarks}
  </Document>
</kml>`
}

// Export map data as KMZ file - exports whatever is currently displayed
async function exportMapKMZ() {
  // Use the currently filtered devices from the map
  const filteredDevices = window._mapFilteredDevices || store.devices
  const devicesWithGPS = filteredDevices.filter(d => d.gps_lat && d.gps_lon)
  
  if (devicesWithGPS.length === 0) {
    alert('No devices with GPS coordinates to export')
    return
  }
  
  // Generate title based on current filter mode
  let title = 'waveControl'
  if (mapFilterMode === 'aps') title += ' - APs'
  else if (mapFilterMode === 'stas') title += ' - STAs'
  if (store.treeFilter) title += ` (${store.treeFilter})`
  
  const kmlContent = generateKML(devicesWithGPS, title)
  
  // Check if JSZip is available
  if (typeof window.JSZip === 'undefined') {
    // Fall back to KML download
    console.warn('JSZip not available, downloading as KML')
    downloadFile(kmlContent, 'wavecontrol-map.kml', 'application/vnd.google-earth.kml+xml')
    return
  }
  
  // Create KMZ (zip containing doc.kml)
  try {
    const zip = new window.JSZip()
    zip.file('doc.kml', kmlContent)
    
    const blob = await zip.generateAsync({ type: 'blob', compression: 'DEFLATE' })
    const filename = 'wavecontrol-map.kmz'
    
    // Download the file
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = filename
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
    URL.revokeObjectURL(url)
    
    console.log(`Exported ${devicesWithGPS.length} devices to ${filename}`)
  } catch (e) {
    console.error('KMZ export error:', e)
    alert('Error exporting KMZ: ' + e.message)
  }
}

// Helper to download a file
function downloadFile(content, filename, mimeType) {
  const blob = new Blob([content], { type: mimeType })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(url)
}

// Escape XML special characters
function escapeXML(str) {
  if (!str) return ''
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&apos;')
}

// ===== QUALITY PAGE =====
function renderAssociationsPage(container) {
  // Get unique sites for filter
  const sites = [...new Set(store.devices.map(d => d.site_name).filter(Boolean))].sort()
  const siteOptions = sites.length > 0 
    ? `<option value="">All Sites (${sites.length})</option>` + sites.map(s => `<option value="${escapeAttr(s)}">${escapeHTML(s)}</option>`).join('')
    : '<option value="">All Sites</option>'
  
  // Preserve tab state (legacy: "aps" -> "signal")
  let currentTab = window.assocCurrentTab || 'signal'
  if (currentTab === 'aps') currentTab = 'signal'
  window.assocCurrentTab = currentTab
  
  container.innerHTML = `
    <div class="page page-associations">
      <div class="assoc-header">
        <div class="assoc-tabs">
          <button class="assoc-tab ${currentTab === 'signal' ? 'active' : ''}" data-tab="signal">Signal Levels</button>
          <button class="assoc-tab ${currentTab === 'modulation' ? 'active' : ''}" data-tab="modulation">Modulation Rates</button>
          <button class="assoc-tab ${currentTab === 'issues' ? 'active' : ''}" data-tab="issues">Issues Queue</button>
          <div class="assoc-tabs-spacer"></div>
          <button class="assoc-tab ${currentTab === 'mismatches' ? 'active' : ''}" data-tab="mismatches">Mismatches</button>
        </div>
        <div class="assoc-toolbar">
          <select id="assocSite" class="form-control form-control-sm">
            ${siteOptions}
          </select>
          <input type="text" id="assocSearch" class="form-control form-control-sm" placeholder="Search..." />
          <select id="assocSort" class="form-control form-control-sm">
            <option value="poor_pct">Sort: Worst First</option>
            <option value="poor_count">Sort: Most Poor</option>
            <option value="clients">Sort: Most Clients</option>
            <option value="name">Sort: Name</option>
            <option value="health">Sort: Health</option>
          </select>
        </div>
      </div>
      <div class="assoc-main">
        <div class="assoc-content">
          <div id="assocSummary" class="assoc-summary"></div>
          <div id="assocTabContent"></div>
        </div>
      </div>
    </div>
  `
  
  // State - preserve existing state if already initialized
  if (!window.assocExpandedAPs) window.assocExpandedAPs = new Set()
  if (!window.assocCurrentTab) window.assocCurrentTab = 'signal'
  if (!window.mismatchExpandedGroups) window.mismatchExpandedGroups = new Set()
  // Don't reset selectedDevice here
  
  renderAssociationsContent()
  
  // Tab switching
  container.querySelectorAll('.assoc-tab').forEach(tab => {
    tab.addEventListener('click', () => {
      container.querySelectorAll('.assoc-tab').forEach(t => t.classList.remove('active'))
      tab.classList.add('active')
      window.assocCurrentTab = tab.dataset.tab
      renderAssociationsContent()
    })
  })
  
  // Filters
  document.getElementById('assocSite')?.addEventListener('change', () => renderAssociationsContent())
  document.getElementById('assocSearch')?.addEventListener('input', () => renderAssociationsContent())
  document.getElementById('assocSort')?.addEventListener('change', () => renderAssociationsContent())
}

// Helper to get best signal value from any device type
function getDeviceSignal(device) {
  // Check flattened signal fields first, then radio objects
  // signal_level is set by API for all STAs from parent AP's peer list
  return device.signal_60ghz || device.signal_5ghz || device.signal_ltu || device.signal_airmax || 
         device.signal_level ||
         device.radio_60ghz?.signal || device.radio_5ghz?.signal || device.radio_ltu?.signal ||
         device.signal || 0
}

function getSTAQualityBuckets(stas) {
  const buckets = { good: [], fair: [], poor: [], offline: [] }
  
  for (const sta of stas) {
    const status = store.getStatus(sta)
    const signal = getDeviceSignal(sta)
    
    // Treat as offline if:
    // 1. Status is not online, OR
    // 2. Signal is 0, null, or -100 (no reading / unreachable)
    if (status !== 'online' || !signal || signal <= -100) {
      buckets.offline.push(sta)
    } else {
      const band = getDeviceBand(sta)
      const quality = getSignalQuality(signal, band)
      if (quality === 'good') {
        buckets.good.push(sta)
      } else if (quality === 'fair') {
        buckets.fair.push(sta)
      } else {
        buckets.poor.push(sta)
      }
    }
  }
  
  return buckets
}

// ----- Directional RF quality helpers (CINR / EVM / SNR / LinkScore) -----

function getStaLinkRadio(sta) {
  if (!sta) return null;
  // Prefer the connected (active) link radio
  if (sta.radio_60ghz && sta.radio_60ghz.connected) return sta.radio_60ghz;
  if (sta.radio_ltu && sta.radio_ltu.connected) return sta.radio_ltu;
  if (sta.radio_5ghz && sta.radio_5ghz.connected) return sta.radio_5ghz;

  // Fallback: pick the first radio that exists (helps with partial / offline data)
  return sta.radio_60ghz || sta.radio_ltu || sta.radio_5ghz || null;
}

function classifyDirectionalMetric(metricType, value) {
  if (value === undefined || value === null || !Number.isFinite(value)) {
    return { quality: 'unknown', sort: Infinity };
  }

  // Thresholds are intentionally conservative and can be tuned per platform.
  switch (metricType) {
    case 'cinr': {
      if (value >= 25) return { quality: 'good', sort: -value };
      if (value >= 15) return { quality: 'warning', sort: -value };
      return { quality: 'poor', sort: -value };
    }
    case 'snr': {
      if (value >= 25) return { quality: 'good', sort: -value };
      if (value >= 15) return { quality: 'warning', sort: -value };
      return { quality: 'poor', sort: -value };
    }
    case 'link_score': {
      if (value >= 80) return { quality: 'good', sort: -value };
      if (value >= 60) return { quality: 'warning', sort: -value };
      return { quality: 'poor', sort: -value };
    }
    case 'evm': {
      // AirMAX EVM is reported as "higher is better".
      if (value >= 28) return { quality: 'good', sort: -value };
      if (value >= 22) return { quality: 'warning', sort: -value };
      return { quality: 'poor', sort: -value };
    }
    default:
      return { quality: 'unknown', sort: Infinity };
  }
}

function mergeDirectionalQualities(...qualities) {
  // Worst-of: poor > warning > good > unknown
  if (qualities.includes('poor')) return 'poor';
  if (qualities.includes('warning')) return 'warning';
  if (qualities.includes('good')) return 'good';
  return 'unknown';
}

function getStaDirectionalRFInfo(sta) {
  const radio = getStaLinkRadio(sta);
  if (!radio) {
    return null;
  }

  // Downlink: AP -> STA (STA RX)
  // Uplink:   STA -> AP (AP RX)
  //
  // Preference order:
  //   - CINR (if present)
  //   - EVM  (if present)
  //   - SNR  (Wave estimation)
  //   - LinkScore (Wave / LTU)

  const dlCandidates = [];
  const ulCandidates = [];

  if (radio.cinr && Number.isFinite(radio.cinr.dl)) dlCandidates.push({ type: 'cinr', value: radio.cinr.dl, label: `CINR ${radio.cinr.dl} dB` });
  if (radio.cinr && Number.isFinite(radio.cinr.ul)) ulCandidates.push({ type: 'cinr', value: radio.cinr.ul, label: `CINR ${radio.cinr.ul} dB` });

  if (radio.evm && Number.isFinite(radio.evm.dl)) dlCandidates.push({ type: 'evm', value: radio.evm.dl, label: `EVM ${radio.evm.dl.toFixed(1)} dB` });
  if (radio.evm && Number.isFinite(radio.evm.ul)) ulCandidates.push({ type: 'evm', value: radio.evm.ul, label: `EVM ${radio.evm.ul.toFixed(1)} dB` });

  if (radio.remote_snr !== undefined && radio.remote_snr !== null && Number.isFinite(radio.remote_snr)) dlCandidates.push({ type: 'snr', value: radio.remote_snr, label: `SNR ${radio.remote_snr} dB` });
  if (radio.snr !== undefined && radio.snr !== null && Number.isFinite(radio.snr)) ulCandidates.push({ type: 'snr', value: radio.snr, label: `SNR ${radio.snr} dB` });

  if (sta.link_score && Number.isFinite(sta.link_score.dl)) dlCandidates.push({ type: 'link_score', value: sta.link_score.dl, label: `Score ${sta.link_score.dl}%` });
  if (sta.link_score && Number.isFinite(sta.link_score.ul)) ulCandidates.push({ type: 'link_score', value: sta.link_score.ul, label: `Score ${sta.link_score.ul}%` });

  const dlQualities = dlCandidates.map(c => classifyDirectionalMetric(c.type, c.value).quality);
  const ulQualities = ulCandidates.map(c => classifyDirectionalMetric(c.type, c.value).quality);

  const dlQuality = mergeDirectionalQualities(...dlQualities);
  const ulQuality = mergeDirectionalQualities(...ulQualities);

  // Pick a primary display candidate (prefer CINR, then EVM, then SNR, then LinkScore)
  const prefOrder = { cinr: 1, evm: 2, snr: 3, link_score: 4 };
  const pickBest = (arr) => arr.slice().sort((a, b) => (prefOrder[a.type] || 99) - (prefOrder[b.type] || 99))[0] || null;

  const dlPrimary = pickBest(dlCandidates);
  const ulPrimary = pickBest(ulCandidates);

  return {
    radio,
    dl: { quality: dlQuality, primary: dlPrimary, candidates: dlCandidates },
    ul: { quality: ulQuality, primary: ulPrimary, candidates: ulCandidates }
  };
}

function computeAPData(aps) {
  return aps.map(ap => {
    const stas = store.getSTAs(ap.id)
    const buckets = getSTAQualityBuckets(stas)
  const onlineCount = stas.length - buckets.offline.length
    const signals = [...buckets.good, ...buckets.fair, ...buckets.poor]
      .map(s => getDeviceSignal(s))
      .filter(s => s && s > -100)  // Only include valid signal readings
    const worstSignal = signals.length > 0 ? Math.min(...signals) : 0
    const avgSignal = signals.length > 0 ? Math.round(signals.reduce((a,b) => a+b, 0) / signals.length) : 0
    
    // Health score: weighted by quality buckets
    const healthScore = stas.length === 0 ? 100 :
      Math.round(((buckets.good.length * 100) + (buckets.fair.length * 60) + (buckets.poor.length * 20)) / 
      Math.max(1, onlineCount))
    
    // Poor percentage
    const poorPct = onlineCount > 0 ? Math.round((buckets.poor.length / onlineCount) * 100) : 0
    
    return { ap, stas, buckets, worstSignal, avgSignal, healthScore, onlineCount, poorPct }
  })
}



// ============================================
// Modulation Quality Classification (Good / Fair / Poor)
// ============================================
//
// A wireless network performs best when most clients are running in the top
// 3-4 modulation rates supported by the radio/config. We use best-effort
// heuristics per platform. Where available, we prefer per-radio MCS (tx/rx)
// and the device-reported "ideal" MCS index.
//
// IMPORTANT: These are heuristics designed for triage/visibility (not RF design).
const MODULATION_PROFILES = {
  // Wave 60GHz / AF60 / Wave MLO (802.11ad/ay style MCS tables)
  wave60: { kind: 'mcs', max: 12, goodMin: 9, fairMin: 6, criticalMax: 4 },

  // LTU (vendor-specific, but MCS tables behave similarly)
  ltu: { kind: 'mcs', max: 12, goodMin: 9, fairMin: 6, criticalMax: 4 },

  // airMAX (rate-based fallback when MCS isn't available)
  airmax_m: { kind: 'mbps', max: 15, good: 130, fair: 65, critical: 40 },
  airmax_ac: { kind: 'mbps', max: 9, good: 200, fair: 120, critical: 80 },

  // airFiber (often reports modulation in "x" style, else use Mbps fallback)
  airfiber: { kind: 'mixed', goodX: 8, fairX: 6, criticalX: 4, goodMbps: 500, fairMbps: 200, criticalMbps: 100 },

  // Generic fallback
  generic: { kind: 'mbps', good: 200, fair: 100, critical: 60 }
}

function minNonZero(a, b) {
  const aa = Number(a) || 0
  const bb = Number(b) || 0
  if (aa > 0 && bb > 0) return Math.min(aa, bb)
  return aa > 0 ? aa : bb > 0 ? bb : 0
}

function pickModulationProfile(device) {
  const key = getModulationProfileKey(device)
  return MODULATION_PROFILES[key] || MODULATION_PROFILES.generic
}

// Best-effort detection of a device's modulation profile.
function getModulationProfileKey(device) {
  const platform = (device.platform || '').toLowerCase()
  const modelText = `${device.product || ''} ${device.model || ''} ${device.hostname || ''}`.toLowerCase()

  // 60GHz/Wave-style (including AF60-like devices) uses an MCS table
  if (device.radio_60ghz?.mcs || device.signal_60ghz || platform.includes('wave') || modelText.includes('wave')) {
    return 'wave60'
  }

  // LTU
  if (device.radio_ltu?.mcs || platform === 'ltu' || modelText.includes('ltu')) {
    return 'ltu'
  }

  // airFiber families
  if (platform.includes('airfiber') || modelText.includes('airfiber')) {
    return 'airfiber'
  }

  // airMAX
  if (platform.includes('airmax') || modelText.includes('airmax')) {
    // Heuristic: treat as AC if model/product contains an "AC" token
    // Examples: NanoBeam 5AC, PowerBeam 5AC, Rocket 5AC, LiteBeam 5AC, etc.
    const isAC = /(^|\W)(5?ac|ac\b|ac\s*gen2)(\W|$)/i.test(modelText)
    return isAC ? 'airmax_ac' : 'airmax_m'
  }

  return 'generic'
}

function pickRadioWithMCS(device) {
  // Prefer 60GHz (Wave) then LTU then 5GHz
  if (device.radio_60ghz?.mcs) return { radio: device.radio_60ghz, band: '60ghz' }
  if (device.radio_ltu?.mcs) return { radio: device.radio_ltu, band: '5ghz' }
  if (device.radio_5ghz?.mcs) return { radio: device.radio_5ghz, band: '5ghz' }
  return null
}

// Extract modulation information (best effort).
// Returns null when we can't extract any usable modulation metric.
function getDeviceModulationInfo(device) {
  const profileKey = getModulationProfileKey(device)
  const profile = MODULATION_PROFILES[profileKey] || MODULATION_PROFILES.generic

  // Prefer MCS from per-radio stats (if available)
  const picked = pickRadioWithMCS(device)
  if (picked && picked.radio && picked.radio.mcs) {
    const mcs = picked.radio.mcs
    const txIdx = mcs.tx_idx || 0
    const rxIdx = mcs.rx_idx || 0
    const txLabel = mcs.tx_label || (txIdx ? `MCS ${txIdx}` : '')
    const rxLabel = mcs.rx_label || (rxIdx ? `MCS ${rxIdx}` : '')

    // Some products report modulation as "8x" labels.
    const xMatchTx = (txLabel || '').match(/(\d+)\s*x/i)
    const xMatchRx = (rxLabel || '').match(/(\d+)\s*x/i)
    if (xMatchTx || xMatchRx) {
      const txX = xMatchTx ? parseInt(xMatchTx[1], 10) : 0
      const rxX = xMatchRx ? parseInt(xMatchRx[1], 10) : 0
      const value = minNonZero(txX, rxX)
      if (value > 0) {
        return {
          profileKey,
          unit: 'x',
          value,
          tx: txX,
          rx: rxX,
          ideal: 0,
          short: `${value}x`,
          long: `Tx ${txX || '?'}x / Rx ${rxX || '?'}x`
        }
      }
    }

    const value = minNonZero(txIdx, rxIdx)
    if (value > 0) {
      // Prefer device-reported ideal MCS when present; else use a profile max.
      let ideal = minNonZero(mcs.tx_idx_ideal, mcs.rx_idx_ideal)
      if (!ideal) ideal = profile.max || 0

      const short = (txIdx || rxIdx) ? `MCS ${txIdx || '?'} / ${rxIdx || '?'}` : [txLabel, rxLabel].filter(Boolean).join(' / ')
      const long = (txLabel || rxLabel) ? `Tx ${txLabel || '?'} / Rx ${rxLabel || '?'}` : short

      return {
        profileKey,
        unit: 'mcs',
        value,
        tx: txIdx,
        rx: rxIdx,
        ideal,
        short,
        long
      }
    }
  }

  // Fallback: use per-peer Tx/Rx rates (Mbps). This is a proxy for modulation.
  const txMbps = device.tx_rate ? (Number(device.tx_rate) / 1e6) : 0
  const rxMbps = device.rx_rate ? (Number(device.rx_rate) / 1e6) : 0
  const value = minNonZero(txMbps, rxMbps)
  if (value > 0) {
    return {
      profileKey,
      unit: 'mbps',
      value,
      tx: txMbps,
      rx: rxMbps,
      ideal: 0,
      short: `${Math.round(value)} Mbps`,
      long: `Tx ${Math.round(txMbps || 0)} / Rx ${Math.round(rxMbps || 0)} Mbps`
    }
  }

  return null
}

function classifyModulationInfo(info) {
  if (!info || !info.value) return { quality: '', critical: false }

  const profile = MODULATION_PROFILES[info.profileKey] || MODULATION_PROFILES.generic

  // MCS (Wave/LTU)
  if (info.unit === 'mcs') {
    const ideal = info.ideal || profile.max || 0
    if (ideal > 0) {
      const diff = ideal - info.value
      // Good = within top ~3-4 MCS of ideal
      if (diff <= 3) return { quality: 'good', critical: false }
      // Fair = next ~3-4
      if (diff <= 6) return { quality: 'fair', critical: false }
      return { quality: 'poor', critical: diff >= 9 || info.value <= (profile.criticalMax || 0) }
    }

    if (info.value >= (profile.goodMin || 0)) return { quality: 'good', critical: false }
    if (info.value >= (profile.fairMin || 0)) return { quality: 'fair', critical: false }
    return { quality: 'poor', critical: info.value <= (profile.criticalMax || 0) }
  }

  // airFiber (x) / other "x" modulation
  if (info.unit === 'x') {
    const good = profile.goodX || 8
    const fair = profile.fairX || 6
    const critical = profile.criticalX || 4
    if (info.value >= good) return { quality: 'good', critical: false }
    if (info.value >= fair) return { quality: 'fair', critical: false }
    return { quality: 'poor', critical: info.value <= critical }
  }

  // Mbps proxy
  const mbps = info.value
  let good = profile.good
  let fair = profile.fair
  let critical = profile.critical

  // airFiber Mbps fallback
  if (profile.kind === 'mixed') {
    good = profile.goodMbps
    fair = profile.fairMbps
    critical = profile.criticalMbps
  }

  if (mbps >= good) return { quality: 'good', critical: false }
  if (mbps >= fair) return { quality: 'fair', critical: false }
  return { quality: 'poor', critical: mbps <= critical }
}

function getSTAModulationBuckets(stas) {
  const buckets = { good: [], fair: [], poor: [], offline: [], unknown: [] }

  for (const sta of stas) {
    const status = store.getStatus(sta)
    if (status !== 'online') {
      buckets.offline.push(sta)
      continue
    }
    const info = getDeviceModulationInfo(sta)
    if (!info || !info.value) {
      buckets.unknown.push(sta)
      continue
    }

    const { quality } = classifyModulationInfo(info)
    if (quality === 'good') buckets.good.push(sta)
    else if (quality === 'fair') buckets.fair.push(sta)
    else buckets.poor.push(sta)
  }

  return buckets
}

function formatModulationAvg(value, unit) {
  if (!value) return ''
  if (unit === 'mcs') return `MCS ${Math.round(value)}`
  if (unit === 'x') return `${Math.round(value)}x`
  if (unit === 'mbps') return `${Math.round(value)} Mbps`
  return String(Math.round(value))
}

function computeAPModulationData(aps) {
  return aps.map(ap => {
    const stas = store.getSTAs(ap.id)
    const buckets = getSTAModulationBuckets(stas)
    const onlineCount = buckets.good.length + buckets.fair.length + buckets.poor.length

    // Gather values (use worst direction = min)
    let unit = ''
    let values = []
    let worstInfo = null

    for (const sta of [...buckets.good, ...buckets.fair, ...buckets.poor]) {
      const info = getDeviceModulationInfo(sta)
      if (!info || !info.value) continue
      if (!unit) unit = info.unit
      values.push(info.value)
      if (!worstInfo || info.value < worstInfo.value) worstInfo = info
    }

    const worstValue = values.length > 0 ? Math.min(...values) : 0
    const avgValue = values.length > 0 ? (values.reduce((a, b) => a + b, 0) / values.length) : 0

    // Health score: weighted by buckets (same idea as signal)
    const healthScore = stas.length === 0 ? 100 :
      Math.round(((buckets.good.length * 100) + (buckets.fair.length * 60) + (buckets.poor.length * 20)) /
      Math.max(1, onlineCount))

    const poorPct = onlineCount > 0 ? Math.round((buckets.poor.length / onlineCount) * 100) : 0

    const worstDisplay = worstInfo ? worstInfo.short : ''
    const avgDisplay = avgValue > 0 ? formatModulationAvg(avgValue, unit) : ''

    return {
      ap,
      stas,
      buckets,
      worstValue,
      avgValue: avgValue ? Math.round(avgValue) : 0,
      worstDisplay,
      avgDisplay,
      unit,
      healthScore,
      onlineCount,
      poorPct
    }
  })
}

// ============================================
// Mismatches (Data Quality)
// ============================================

function computeMismatches(devices, filterText = '') {
  const filter = (filterText || '').toLowerCase()

  const all = Array.isArray(devices) ? devices : []
  const deviceById = new Map()
  for (const d of all) {
    if (d && d.id != null) deviceById.set(d.id, d)
  }
  const filteredDevices = []

  const macGroups = new Map()
  const ipGroups = new Map()

	for (const d of all) {
    if (!d) continue

    const name = (d.name || d.hostname || '').toLowerCase()
    const ipLower = (d.ip || '').toLowerCase()
    const macLower = (d.mac || '').toLowerCase()
    const ssid = d.ssid || ''
    const ssidLower = (ssid || '').toLowerCase()

    if (filter) {
      if (!name.includes(filter) && !ipLower.includes(filter) && !macLower.includes(filter) && !ssidLower.includes(filter)) {
        continue
      }
    }

		filteredDevices.push(d)

    // Duplicate MACs
    if (macLower) {
      let g = macGroups.get(macLower)
      if (!g) {
        g = { mac: macLower, devices: [], ips: new Set(), ssids: new Set() }
        macGroups.set(macLower, g)
      }
      g.devices.push(d)
      if (d.ip) g.ips.add(d.ip)
      if (ssid) g.ssids.add(ssid)
    }

    // Duplicate IPs
    if (d.ip) {
      const key = d.ip
      let g = ipGroups.get(key)
      if (!g) {
        g = { ip: key, devices: [], macs: new Set(), ssids: new Set() }
        ipGroups.set(key, g)
      }
      g.devices.push(d)
      if (macLower) g.macs.add(macLower)
      if (ssid) g.ssids.add(ssid)
    }
  }

  const duplicateMacGroups = [...macGroups.values()]
    .filter(g => g.devices.length > 1)
    .map(g => ({ ...g, ips: [...g.ips], ssids: [...g.ssids] }))
    .sort((a, b) => b.devices.length - a.devices.length || a.mac.localeCompare(b.mac))

  const duplicateIPGroups = [...ipGroups.values()]
    .filter(g => g.devices.length > 1)
    .map(g => ({ ...g, macs: [...g.macs], ssids: [...g.ssids] }))
    .sort((a, b) => b.devices.length - a.devices.length || a.ip.localeCompare(b.ip))

	// Likely loops: STAs parented by other STAs
	const staParentLoops = []
	for (const d of filteredDevices) {
		if (!d) continue
		const role = String(d.role || '').toLowerCase()
		if (role !== 'sta') continue
		const pid = d.parent_id
		if (!pid) continue
		const parent = deviceById.get(pid)
		if (!parent) continue
		const parentRole = String(parent.role || '').toLowerCase()
		if (parentRole === 'sta') {
			staParentLoops.push({ sta: d, parent })
		}
	}

	return { duplicateMacGroups, duplicateIPGroups, staParentLoops }
}

function renderMismatchesTab(container) {
  const devices = store.getState().devices || []
  const filter = store.getState().mismatchesFilter || ''

	const { duplicateMacGroups, duplicateIPGroups, staParentLoops } = computeMismatches(devices, filter)

	const hasAny = duplicateMacGroups.length || duplicateIPGroups.length || staParentLoops.length

  container.innerHTML = `
    <div class="quality-panel">
      <div class="quality-panel-header">
        <div class="quality-panel-title">Mismatches</div>
        <div class="quality-panel-controls">
          <input id="mismatchesFilter" class="input" placeholder="Filter..." value="${escapeHTML(filter)}" />
        </div>
      </div>

      ${!hasAny ? `
        <div class="empty-state">
          <div class="empty-title">No mismatches detected</div>
          <div class="empty-subtitle">Duplicate MAC / IP checks run across all known devices.</div>
        </div>
      ` : ''}

      <div class="mismatch-grid">
        <div class="mismatch-card">
          <div class="mismatch-card-header">
            <div class="mismatch-card-title">Duplicate MAC Addresses</div>
            <div class="mismatch-card-stat">${duplicateMacGroups.length}</div>
          </div>
          <div class="mismatch-card-body">
            ${duplicateMacGroups.length ? `
              <table class="table table-compact">
                <thead>
                  <tr>
                    <th></th>
                    <th class="col-ip">MAC Address</th>
                    <th class="col-site">Rows</th>
                    <th>IPs</th>
                  </tr>
                </thead>
                <tbody>
                  ${duplicateMacGroups.map(renderMismatchMacGroupRow).join('')}
                </tbody>
              </table>
            ` : `<div class="mismatch-empty">No duplicate MACs.</div>`}
          </div>
        </div>

        <div class="mismatch-card">
          <div class="mismatch-card-header">
            <div class="mismatch-card-title">IP Collisions</div>
            <div class="mismatch-card-stat">${duplicateIPGroups.length}</div>
          </div>
          <div class="mismatch-card-body">
            ${duplicateIPGroups.length ? `
              <table class="table table-compact">
                <thead>
                  <tr>
                    <th></th>
                    <th class="col-ip">IP Address</th>
                    <th class="col-site">Rows</th>
                    <th>SSIDs</th>
                    <th>MACs</th>
                  </tr>
                </thead>
                <tbody>
                  ${duplicateIPGroups.map(renderMismatchIPGroupRow).join('')}
                </tbody>
              </table>
            ` : `<div class="mismatch-empty">No IP collisions.</div>`}
          </div>
        </div>

		<div class="mismatch-card">
			<div class="mismatch-card-header">
				<div class="mismatch-card-title">STA Parent Loops</div>
				<div class="mismatch-card-stat">${staParentLoops.length}</div>
			</div>
			<div class="mismatch-card-body">
				${staParentLoops.length ? `
					<table class="table table-compact">
						<thead>
							<tr>
								<th>STA</th>
								<th class="col-ip">STA IP</th>
								<th>Parent</th>
								<th class="col-ip">Parent IP</th>
							</tr>
						</thead>
						<tbody>
							${staParentLoops.map(({ sta, parent }) => {
								const staLabel = `#${sta.id} ${sta.name || sta.hostname || sta.ip || sta.mac || ''}`.trim()
								const parentLabel = `#${parent.id} ${parent.name || parent.hostname || parent.ip || parent.mac || ''}`.trim()
								return `
								<tr>
									<td><button type="button" class="detail-link detail-link-button" data-wc-action="select-device" data-device-id="${sta.id}">${escapeHTML(staLabel)}</button></td>
									<td class="col-ip">${escapeHTML(sta.ip || '')}</td>
									<td><button type="button" class="detail-link detail-link-button" data-wc-action="select-device" data-device-id="${parent.id}">${escapeHTML(parentLabel)}</button></td>
									<td class="col-ip">${escapeHTML(parent.ip || '')}</td>
								</tr>
								`
							}).join('')}
						</tbody>
					</table>
				` : `<div class="mismatch-empty">No STA parent loops.</div>`}
			</div>
		</div>
      </div>
    </div>
  `

  const filterInput = document.getElementById('mismatchesFilter')
  filterInput.addEventListener('input', (e) => {
    store.setState({ mismatchesFilter: e.target.value })
    renderCurrentQualityTab()
  })

  // Toggle expandable groups
  document.querySelectorAll('.mismatch-toggle').forEach(btn => {
    btn.addEventListener('click', () => {
      const type = btn.dataset.type
      const key = btn.dataset.key
      const expanded = store.getState().mismatchExpanded || {}
      const next = { ...expanded }
      const k = `${type}:${key}`
      next[k] = !next[k]
      store.setState({ mismatchExpanded: next })
      renderCurrentQualityTab()
    })
  })

  // "Delete" actions (for convenience)
  document.querySelectorAll('.mismatch-delete').forEach(btn => {
    btn.addEventListener('click', async () => {
      const id = btn.dataset.id
      if (!id) return
      if (!confirm('Remove this device from WaveControl?')) return
      try {
        await api.deleteDevice(id)
        await refreshDevices()
        renderCurrentQualityTab()
      } catch (e) {
        alert('Failed to delete device: ' + e.message)
      }
    })
  })
}

function renderMismatchMacGroupRow(group) {
  const expanded = store.getState().mismatchExpanded || {}
  const k = `mac:${group.mac}`
  const isOpen = !!expanded[k]

  const ipList = group.ips.length ? group.ips.join(', ') : '—'

  return `
    <tr>
      <td class="col-expand">
        <button class="mismatch-toggle" data-type="mac" data-key="${escapeHTML(group.mac)}">${isOpen ? '▾' : '▸'}</button>
      </td>
      <td class="mono">${escapeHTML(group.mac)}</td>
      <td class="mono">${group.devices.length}</td>
      <td class="mono">${escapeHTML(ipList)}</td>
    </tr>
    ${isOpen ? `
      <tr class="mismatch-expanded">
        <td colspan="4">
          <table class="table table-compact nested">
            <thead>
              <tr>
                <th class="col-id">ID</th>
                <th>Name</th>
                <th class="col-ip">IP</th>
                <th class="col-site">SSID</th>
                <th class="col-mac">MAC</th>
                <th class="col-site">Platform</th>
                <th class="col-action">Action</th>
              </tr>
            </thead>
            <tbody>
              ${group.devices.map(d => renderMismatchDeviceRow(d, true)).join('')}
            </tbody>
          </table>
        </td>
      </tr>
    ` : ''}
  `
}

function renderMismatchIPGroupRow(group) {
  const expanded = store.getState().mismatchExpanded || {}
  const k = `ip:${group.ip}`
  const isOpen = !!expanded[k]

  const macList = group.macs.length ? group.macs.join(', ') : '—'
  const ssidList = (group.ssids || []).filter(Boolean)
  const ssidDisplay = ssidList.length ? ssidList.join(', ') : '—'

  return `
    <tr>
      <td class="col-expand">
        <button class="mismatch-toggle" data-type="ip" data-key="${escapeHTML(group.ip)}">${isOpen ? '▾' : '▸'}</button>
      </td>
      <td class="mono">${escapeHTML(group.ip)}</td>
      <td class="mono">${group.devices.length}</td>
      <td>${escapeHTML(ssidDisplay)}</td>
      <td class="mono">${escapeHTML(macList)}</td>
    </tr>
    ${isOpen ? `
      <tr class="mismatch-expanded">
        <td colspan="5">
          <table class="table table-compact nested">
            <thead>
              <tr>
                <th class="col-id">ID</th>
                <th>Name</th>
                <th class="col-ip">IP</th>
                <th class="col-site">SSID</th>
                <th class="col-mac">MAC</th>
                <th class="col-site">Platform</th>
                <th class="col-action">Action</th>
              </tr>
            </thead>
            <tbody>
              ${group.devices.map(d => renderMismatchDeviceRow(d, true)).join('')}
            </tbody>
          </table>
        </td>
      </tr>
    ` : ''}
  `
}

function renderMismatchDeviceRow(d, asTableRowOnly = false) {
  const name = d.name || d.hostname || ''
  const ip = d.ip || '—'
  const mac = d.mac || '—'
  const platform = d.platform || d.Platform || '—'
  const ssid = d.ssid || '—'

  // Only used inside nested tables
  return `
    <tr>
      <td class="mono">${escapeHTML(String(d.id || ''))}</td>
      <td>${escapeHTML(name)}</td>
      <td class="mono">${escapeHTML(ip)}</td>
      <td>${escapeHTML(ssid)}</td>
      <td class="mono">${escapeHTML(mac)}</td>
      <td>${escapeHTML(platform)}</td>
      <td><button class="btn btn-xs btn-danger mismatch-delete" data-id="${escapeHTML(String(d.id || ''))}">Delete</button></td>
    </tr>
  `
}

// ============================================
// Modulation Table Rendering
// ============================================

function renderModulationTable(container, apData) {
  if (apData.length === 0) {
    container.innerHTML = `<div class="assoc-empty"><p>No APs match your criteria.</p></div>`
    return
  }

  container.innerHTML = `
    <table class="assoc-table">
      <thead>
        <tr>
          <th class="col-expand"></th>
          <th class="col-status"></th>
          <th class="col-name">AP Name</th>
          <th class="col-site">Site</th>
          <th class="col-clients">Clients</th>
          <th class="col-distribution">Distribution</th>
          <th class="col-poor">Poor</th>
          <th class="col-poor-pct">Poor %</th>
          <th class="col-signal">Worst</th>
          <th class="col-signal">Avg</th>
          <th class="col-health">Health</th>
        </tr>
      </thead>
      <tbody>
        ${apData.map(data => renderModulationAPRow(data)).join('')}
      </tbody>
    </table>
  `

  // Expand/collapse
  container.querySelectorAll('.assoc-row').forEach(row => {
    row.addEventListener('click', e => {
      if (e.target.closest('.assoc-expand-content')) return
      if (e.target.closest('.assoc-ap-name')) return
      if (e.target.closest('.assoc-view-all')) return
      if (e.target.closest('button')) return

      const id = parseInt(row.dataset.id)
      if (window.assocExpandedAPs.has(id)) window.assocExpandedAPs.delete(id)
      else window.assocExpandedAPs.add(id)
      renderAssociationsContent()
    })
  })

  // Client row click -> details
  container.querySelectorAll('.assoc-client-row').forEach(row => {
    row.addEventListener('click', e => {
      e.stopPropagation()
      const id = parseInt(row.dataset.id)
      showAssocDeviceDetails(id)
    })
  })

  // View all
  container.querySelectorAll('.assoc-view-all').forEach(btn => {
    btn.addEventListener('click', e => {
      e.stopPropagation()
      const apId = parseInt(btn.dataset.ap)
      showAllClientsPanel(apId, btn.dataset.mode || 'modulation')
    })
  })

  // AP name click -> details
  container.querySelectorAll('.assoc-ap-name').forEach(name => {
    name.addEventListener('click', e => {
      e.stopPropagation()
      const id = parseInt(name.closest('.assoc-row').dataset.id)
      showAssocDeviceDetails(id)
    })
  })
}

function renderModulationAPRow(data) {
  const { ap, stas, buckets, worstDisplay, avgDisplay, healthScore, onlineCount, poorPct } = data
  const status = store.getStatus(ap)
  const isExpanded = window.assocExpandedAPs?.has(ap.id)

  const total = buckets.good.length + buckets.fair.length + buckets.poor.length + buckets.unknown.length + buckets.offline.length
  const distBar = total > 0 ? `
    <div class="dist-bar">
      ${buckets.good.length > 0 ? `<div class="dist-seg good" style="width:${(buckets.good.length/total)*100}%" title="${buckets.good.length} good"></div>` : ''}
      ${buckets.fair.length > 0 ? `<div class="dist-seg fair" style="width:${(buckets.fair.length/total)*100}%" title="${buckets.fair.length} fair"></div>` : ''}
      ${buckets.poor.length > 0 ? `<div class="dist-seg poor" style="width:${(buckets.poor.length/total)*100}%" title="${buckets.poor.length} poor"></div>` : ''}
      ${buckets.unknown.length > 0 ? `<div class="dist-seg unknown" style="width:${(buckets.unknown.length/total)*100}%" title="${buckets.unknown.length} no data"></div>` : ''}
      ${buckets.offline.length > 0 ? `<div class="dist-seg offline" style="width:${(buckets.offline.length/total)*100}%" title="${buckets.offline.length} offline"></div>` : ''}
    </div>
  ` : '<span class="text-muted">—</span>'

  const healthClass = healthScore >= 80 ? 'good' : healthScore >= 50 ? 'fair' : 'poor'
  const worstClass = buckets.poor.length > 0 ? 'text-danger' : (buckets.fair.length > 0 ? 'text-warning' : '')

  let expandedContent = ''
  if (isExpanded) {
    const worstClients = [...buckets.poor, ...buckets.fair, ...buckets.good]
      .map(sta => {
        const info = getDeviceModulationInfo(sta)
        return { sta, info, value: info?.value || 0 }
      })
      .sort((a, b) => {
        // Sort by quality bucket first, then value
        const qa = a.info ? classifyModulationInfo(a.info).quality : ''
        const qb = b.info ? classifyModulationInfo(b.info).quality : ''
        const order = { poor: 0, fair: 1, good: 2 }
        const oa = order[qa] ?? 3
        const ob = order[qb] ?? 3
        if (oa != ob) return oa - ob
        return (a.value || 0) - (b.value || 0)
      })
      .slice(0, 10)

    expandedContent = `
      <tr class="assoc-expand-row">
        <td colspan="11">
          <div class="assoc-expand-content">
            <div class="assoc-expand-header">
              <span>Worst ${Math.min(10, worstClients.length)} clients</span>
              ${stas.length > 10 ? `<button class="btn btn-sm assoc-view-all" data-ap="${ap.id}" data-mode="modulation">View all ${stas.length} clients</button>` : ''}
            </div>
            ${worstClients.length > 0 ? `
              <table class="assoc-clients-table">
                <thead>
                  <tr>
                    <th></th>
                    <th>Name</th>
                    <th>IP</th>
                    <th>Modulation</th>
                    <th>Distance</th>
                    <th>Product</th>
                  </tr>
                </thead>
                <tbody>
                  ${worstClients.map(({ sta, info }) => {
                    const staStatus = store.getStatus(sta)
                    const q = info ? classifyModulationInfo(info).quality : ''
                    const cls = q === 'poor' ? 'poor' : q === 'fair' ? 'fair' : q === 'good' ? 'good' : ''
                    return `
                      <tr class="assoc-client-row" data-id="${sta.id}">
                        <td><span class="status-dot ${staStatus}"></span></td>
                        <td>${escapeHTML(sta.hostname || sta.mac || 'Unknown')}</td>
                        <td class="mono">${escapeHTML(sta.ip_address || '—')}</td>
                        <td class="${cls}">${info?.short || '—'}</td>
                        <td>${sta.distance ? `${(sta.distance/1000).toFixed(2)} km` : '—'}</td>
                        <td>${escapeHTML(sta.product || sta.model || '—')}</td>
                      </tr>
                    `
                  }).join('')}
                </tbody>
              </table>
            ` : '<p class="text-muted">No clients</p>'}
          </div>
        </td>
      </tr>
    `
  }

  return `
    <tr class="assoc-row ${isExpanded ? 'expanded' : ''}" data-id="${ap.id}">
      <td class="col-expand">${stas.length > 0 ? (isExpanded ? '▼' : '▶') : ''}</td>
      <td class="col-status"><span class="status-dot ${status}"></span></td>
      <td class="col-name"><span class="assoc-ap-name">${escapeHTML(ap.hostname || ap.ip_address || 'Unknown')}</span></td>
      <td class="col-site">${escapeHTML(ap.site_name || '—')}</td>
      <td class="col-clients">${stas.length}</td>
      <td class="col-distribution">${distBar}</td>
      <td class="col-poor ${buckets.poor.length > 0 ? 'text-danger' : ''}">${buckets.poor.length || '—'}</td>
      <td class="col-poor-pct ${poorPct > 20 ? 'text-danger' : poorPct > 10 ? 'text-warning' : ''}">${poorPct > 0 ? `${poorPct}%` : '—'}</td>
      <td class="col-signal ${worstClass}">${worstDisplay || '—'}</td>
      <td class="col-signal">${avgDisplay || '—'}</td>
      <td class="col-health"><span class="health-badge ${healthClass}">${healthScore}%</span></td>
    </tr>
    ${expandedContent}
  `
}
function renderAssociationsContent() {
  const tabContent = document.getElementById('assocTabContent')
  const summaryEl = document.getElementById('assocSummary')
  if (!tabContent) return

  // Normalize legacy tab key
  if (window.assocCurrentTab === 'aps') window.assocCurrentTab = 'signal'
  const currentTab = window.assocCurrentTab || 'signal'

  const siteSel = document.getElementById('assocSite')
  const searchEl = document.getElementById('assocSearch')
  const sortSel = document.getElementById('assocSort')

  // Mismatches is a data-quality view (not site-scoped), so hide irrelevant controls.
  if (currentTab === 'mismatches') {
    if (siteSel) siteSel.style.display = 'none'
    if (sortSel) sortSel.style.display = 'none'
  } else {
    if (siteSel) siteSel.style.display = ''
    if (sortSel) sortSel.style.display = ''
  }

  const siteFilter = siteSel?.value || ''
  const filter = (searchEl?.value || '').toLowerCase()
  const sortBy = sortSel?.value || 'poor_pct'

  // Mismatches tab
  if (currentTab === 'mismatches') {
    const mismatchData = computeMismatches(store.devices, filter)

    if (summaryEl) {
      summaryEl.innerHTML = `
        <div class="assoc-summary-stats">
          <div class="assoc-stat ${mismatchData.duplicateMacGroups.length > 0 ? 'warning' : ''}">
            <span class="assoc-stat-value">${mismatchData.duplicateMacGroups.length}</span>
            <span class="assoc-stat-label">Duplicate MAC groups</span>
          </div>
          <div class="assoc-stat ${mismatchData.duplicateIPGroups.length > 0 ? 'danger' : ''}">
            <span class="assoc-stat-value">${mismatchData.duplicateIPGroups.length}</span>
            <span class="assoc-stat-label">IP collisions</span>
          </div>
        </div>
        <div class="assoc-legend">
          <span class="legend-item">This view is for cleaning data inconsistencies (duplicates / IP reuse).</span>
        </div>
      `
    }

    renderMismatchesTab(tabContent, mismatchData)
    return
  }

  // Filter APs
  let aps = store.aps.filter(d => {
    if (siteFilter && d.site_name !== siteFilter) return false
    if (filter) {
      const matchesAP = (d.hostname || '').toLowerCase().includes(filter) ||
                        (d.ip_address || '').includes(filter)
      const stas = store.getSTAs(d.id)
      const matchesSTA = stas.some(s =>
        (s.hostname || '').toLowerCase().includes(filter) ||
        (s.ip_address || '').includes(filter)
      )
      return matchesAP || matchesSTA
    }
    return true
  })

  // Compute both datasets (Issues Queue needs both)
  const apDataSignal = computeAPData(aps)
  const apDataMod = computeAPModulationData(aps)

  // Pick active dataset for table view
  const activeData = currentTab === 'modulation' ? apDataMod : apDataSignal

  // Sort (only applies to table views)
  if (currentTab !== 'issues') {
    switch (sortBy) {
      case 'poor_pct':
        activeData.sort((a, b) => b.poorPct - a.poorPct || b.buckets.poor.length - a.buckets.poor.length)
        break
      case 'poor_count':
        activeData.sort((a, b) => b.buckets.poor.length - a.buckets.poor.length)
        break
      case 'clients':
        activeData.sort((a, b) => b.stas.length - a.stas.length)
        break
      case 'health':
        activeData.sort((a, b) => a.healthScore - b.healthScore)
        break
      default:
        activeData.sort((a, b) => (a.ap.hostname || '').localeCompare(b.ap.hostname || ''))
    }
  }

  // Summary stats
  const totalAPs = activeData.length
  const totalSTAs = activeData.reduce((sum, d) => sum + d.stas.length, 0)
  const onlineAPs = activeData.filter(d => store.getStatus(d.ap) === 'online').length
  const totalOnlineSTAs = activeData.reduce((sum, d) => sum + d.onlineCount, 0)
  const totalPoor = activeData.reduce((sum, d) => sum + d.buckets.poor.length, 0)
  const totalUnknown = activeData.reduce((sum, d) => sum + (d.buckets.unknown?.length || 0), 0)
  const totalOffline = activeData.reduce((sum, d) => sum + d.buckets.offline.length, 0)

  if (summaryEl) {
    if (currentTab === 'issues') {
      // Issues summary: show both signal + modulation counts
      const totalPoorSignal = apDataSignal.reduce((sum, d) => sum + d.buckets.poor.length, 0)
      const totalPoorMod = apDataMod.reduce((sum, d) => sum + d.buckets.poor.length, 0)
      const totalOffline2 = apDataSignal.reduce((sum, d) => sum + d.buckets.offline.length, 0)

      summaryEl.innerHTML = `
        <div class="assoc-summary-stats">
          <div class="assoc-stat">
            <span class="assoc-stat-value">${aps.length}</span>
            <span class="assoc-stat-label">APs</span>
          </div>
          <div class="assoc-stat">
            <span class="assoc-stat-value">${onlineAPs}</span>
            <span class="assoc-stat-label">Online</span>
          </div>
          <div class="assoc-stat ${totalPoorSignal > 0 ? 'danger' : ''}">
            <span class="assoc-stat-value">${totalPoorSignal}</span>
            <span class="assoc-stat-label">Poor Signal</span>
          </div>
          <div class="assoc-stat ${totalPoorMod > 0 ? 'danger' : ''}">
            <span class="assoc-stat-value">${totalPoorMod}</span>
            <span class="assoc-stat-label">Poor Modulation</span>
          </div>
          <div class="assoc-stat ${totalOffline2 > 0 ? 'warning' : ''}">
            <span class="assoc-stat-value">${totalOffline2}</span>
            <span class="assoc-stat-label">Offline</span>
          </div>
        </div>
        <div class="assoc-legend">
          <span class="legend-item"><span class="legend-dot good"></span> Good</span>
          <span class="legend-item"><span class="legend-dot fair"></span> Fair</span>
          <span class="legend-item"><span class="legend-dot poor"></span> Poor</span>
          <span class="legend-item"><span class="legend-dot offline"></span> Offline</span>
          <span class="legend-item legend-thresholds">Signal thresholds: 5GHz ${getThresholdLabel('5ghz').good}/${getThresholdLabel('5ghz').fair}/${getThresholdLabel('5ghz').poor}; 60GHz ${getThresholdLabel('60ghz').good}/${getThresholdLabel('60ghz').fair}/${getThresholdLabel('60ghz').poor}</span>
        </div>
      `
    } else if (currentTab === 'modulation') {
      summaryEl.innerHTML = `
        <div class="assoc-summary-stats">
          <div class="assoc-stat">
            <span class="assoc-stat-value">${totalAPs}</span>
            <span class="assoc-stat-label">APs</span>
          </div>
          <div class="assoc-stat">
            <span class="assoc-stat-value">${onlineAPs}</span>
            <span class="assoc-stat-label">Online</span>
          </div>
          <div class="assoc-stat">
            <span class="assoc-stat-value">${totalSTAs}</span>
            <span class="assoc-stat-label">Clients</span>
          </div>
          <div class="assoc-stat ${totalPoor > 0 ? 'danger' : ''}">
            <span class="assoc-stat-value">${totalPoor}</span>
            <span class="assoc-stat-label">Poor Modulation</span>
          </div>
          <div class="assoc-stat ${totalUnknown > 0 ? 'warning' : ''}">
            <span class="assoc-stat-value">${totalUnknown}</span>
            <span class="assoc-stat-label">No Data</span>
          </div>
          <div class="assoc-stat ${totalOffline > 0 ? 'warning' : ''}">
            <span class="assoc-stat-value">${totalOffline}</span>
            <span class="assoc-stat-label">Offline</span>
          </div>
        </div>
        <div class="assoc-legend">
          <span class="legend-item"><span class="legend-dot good"></span> Good</span>
          <span class="legend-item"><span class="legend-dot fair"></span> Fair</span>
          <span class="legend-item"><span class="legend-dot poor"></span> Poor</span>
          <span class="legend-item"><span class="legend-dot unknown"></span> No Data</span>
          <span class="legend-item"><span class="legend-dot offline"></span> Offline</span>
          <span class="legend-item legend-thresholds">Wave/LTU: Good=within 3 MCS of ideal; Fair=within 6. airMAX M: ≥130/65/<65 Mbps. airMAX AC: ≥200/120/<120 Mbps.</span>
        </div>
      `
    } else {
      // Signal Levels
      summaryEl.innerHTML = `
        <div class="assoc-summary-stats">
          <div class="assoc-stat">
            <span class="assoc-stat-value">${totalAPs}</span>
            <span class="assoc-stat-label">APs</span>
          </div>
          <div class="assoc-stat">
            <span class="assoc-stat-value">${onlineAPs}</span>
            <span class="assoc-stat-label">Online</span>
          </div>
          <div class="assoc-stat">
            <span class="assoc-stat-value">${totalSTAs}</span>
            <span class="assoc-stat-label">Clients</span>
          </div>
          <div class="assoc-stat ${totalPoor > 0 ? 'danger' : ''}">
            <span class="assoc-stat-value">${totalPoor}</span>
            <span class="assoc-stat-label">Poor Signal</span>
          </div>
          <div class="assoc-stat ${totalOffline > 0 ? 'warning' : ''}">
            <span class="assoc-stat-value">${totalOffline}</span>
            <span class="assoc-stat-label">Offline</span>
          </div>
        </div>
        <div class="assoc-legend">
          <span class="legend-item"><span class="legend-dot good"></span> Good</span>
          <span class="legend-item"><span class="legend-dot fair"></span> Fair</span>
          <span class="legend-item"><span class="legend-dot poor"></span> Poor</span>
          <span class="legend-item"><span class="legend-dot offline"></span> Offline</span>
          <span class="legend-item legend-thresholds">5GHz: ${getThresholdLabel('5ghz').good}/${getThresholdLabel('5ghz').fair}/${getThresholdLabel('5ghz').poor}</span>
          <span class="legend-item legend-thresholds">60GHz: ${getThresholdLabel('60ghz').good}/${getThresholdLabel('60ghz').fair}/${getThresholdLabel('60ghz').poor}</span>
        </div>
      `
    }
  }

  if (currentTab === 'issues') {
    renderIssuesQueue(tabContent, apDataSignal, apDataMod)
  } else if (currentTab === 'modulation') {
    renderModulationTable(tabContent, activeData)
  } else {
    renderAPTable(tabContent, activeData)
  }
}
function renderAPTable(container, apData) {
  if (apData.length === 0) {
    container.innerHTML = `<div class="assoc-empty"><p>No APs match your criteria.</p></div>`
    return
  }
  
  container.innerHTML = `
    <table class="assoc-table">
      <thead>
        <tr>
          <th class="col-expand"></th>
          <th class="col-status"></th>
          <th class="col-name">AP Name</th>
          <th class="col-site">Site</th>
          <th class="col-clients">Clients</th>
          <th class="col-distribution">Distribution</th>
          <th class="col-poor">Poor</th>
          <th class="col-poor-pct">Poor %</th>
          <th class="col-signal">Worst</th>
          <th class="col-signal">Avg</th>
          <th class="col-health">Health</th>
        </tr>
      </thead>
      <tbody>
        ${apData.map(data => renderAPRow(data)).join('')}
      </tbody>
    </table>
  `
  
  // Row click -> expand (but not on clickable elements)
  container.querySelectorAll('.assoc-row').forEach(row => {
    row.addEventListener('click', e => {
      // Skip if clicking on interactive elements
      if (e.target.closest('.assoc-expand-content')) return
      if (e.target.closest('.assoc-ap-name')) return
      if (e.target.closest('.assoc-view-all')) return
      if (e.target.closest('button')) return
      
      const id = parseInt(row.dataset.id)
      if (window.assocExpandedAPs.has(id)) {
        window.assocExpandedAPs.delete(id)
      } else {
        window.assocExpandedAPs.add(id)
      }
      renderAssociationsContent()
    })
  })
  
  // Client row click -> show details
  container.querySelectorAll('.assoc-client-row').forEach(row => {
    row.addEventListener('click', e => {
      e.stopPropagation()
      const id = parseInt(row.dataset.id)
      showAssocDeviceDetails(id)
    })
  })
  
  // View all clients
  container.querySelectorAll('.assoc-view-all').forEach(btn => {
    btn.addEventListener('click', e => {
      e.stopPropagation()
      const apId = parseInt(btn.dataset.ap)
      showAllClientsPanel(apId, btn.dataset.mode || 'signal')
    })
  })
  
  // AP name click -> show AP details
  container.querySelectorAll('.assoc-ap-name').forEach(name => {
    name.addEventListener('click', e => {
      e.stopPropagation()
      const id = parseInt(name.closest('.assoc-row').dataset.id)
      showAssocDeviceDetails(id)
    })
  })
}

function renderAPRow(data) {
  const { ap, stas, buckets, worstSignal, avgSignal, healthScore, onlineCount, poorPct } = data
  const status = store.getStatus(ap)
  const isExpanded = window.assocExpandedAPs?.has(ap.id)
  
  // Determine band from AP (STAs inherit the same band)
  const band = getDeviceBand(ap)
  const worstQuality = getSignalQuality(worstSignal, band)
  const worstSignalClass = worstQuality === 'poor' ? 'text-danger' : worstQuality === 'fair' ? 'text-warning' : ''
  
  // Distribution bar
  const total = buckets.good.length + buckets.fair.length + buckets.poor.length + buckets.offline.length
  const distBar = total > 0 ? `
    <div class="dist-bar">
      ${buckets.good.length > 0 ? `<div class="dist-seg good" style="width:${(buckets.good.length/total)*100}%" title="${buckets.good.length} good"></div>` : ''}
      ${buckets.fair.length > 0 ? `<div class="dist-seg fair" style="width:${(buckets.fair.length/total)*100}%" title="${buckets.fair.length} fair"></div>` : ''}
      ${buckets.poor.length > 0 ? `<div class="dist-seg poor" style="width:${(buckets.poor.length/total)*100}%" title="${buckets.poor.length} poor"></div>` : ''}
      ${buckets.offline.length > 0 ? `<div class="dist-seg offline" style="width:${(buckets.offline.length/total)*100}%" title="${buckets.offline.length} offline"></div>` : ''}
    </div>
  ` : '<span class="text-muted">—</span>'
  
  // Health class
  const healthClass = healthScore >= 80 ? 'good' : healthScore >= 50 ? 'fair' : 'poor'
  
  // Expanded content: worst 10 clients
  let expandedContent = ''
  if (isExpanded) {
    const worstClients = [...buckets.poor, ...buckets.fair, ...buckets.good]
      .sort((a, b) => {
        const sigA = getDeviceSignal(a) || -100
        const sigB = getDeviceSignal(b) || -100
        return sigA - sigB
      })
      .slice(0, 10)
    
    expandedContent = `
      <tr class="assoc-expand-row">
        <td colspan="11">
          <div class="assoc-expand-content">
            <div class="assoc-expand-header">
              <span>Worst ${Math.min(10, worstClients.length)} clients</span>
              ${stas.length > 10 ? `<button class="btn btn-sm assoc-view-all" data-ap="${ap.id}" data-mode="signal">View all ${stas.length} clients</button>` : ''}
            </div>
            ${worstClients.length > 0 ? `
              <table class="assoc-clients-table">
                <thead>
                  <tr>
                    <th></th>
                    <th>Name</th>
                    <th>IP</th>
                    <th>Signal</th>
                    <th>Distance</th>
                    <th>Product</th>
                  </tr>
                </thead>
                <tbody>
                  ${worstClients.map(sta => {
                    const staStatus = store.getStatus(sta)
                    const signal = getDeviceSignal(sta)
                    const band = getDeviceBand(sta)
                    const signalClass = getSignalQuality(signal, band)
                    return `
                      <tr class="assoc-client-row" data-id="${sta.id}">
                        <td><span class="status-dot ${staStatus}"></span></td>
                        <td>${escapeHTML(sta.hostname || sta.mac || 'Unknown')}</td>
                        <td class="mono">${escapeHTML(sta.ip_address || '—')}</td>
                        <td class="${signalClass}">${signal ? `${signal} dBm` : '—'}</td>
                        <td>${sta.distance ? `${(sta.distance/1000).toFixed(2)} km` : '—'}</td>
                        <td>${escapeHTML(sta.product || sta.model || '—')}</td>
                      </tr>
                    `
                  }).join('')}
                </tbody>
              </table>
            ` : '<p class="text-muted">No clients</p>'}
          </div>
        </td>
      </tr>
    `
  }
  
  return `
    <tr class="assoc-row ${isExpanded ? 'expanded' : ''}" data-id="${ap.id}">
      <td class="col-expand">${stas.length > 0 ? (isExpanded ? '▼' : '▶') : ''}</td>
      <td class="col-status"><span class="status-dot ${status}"></span></td>
      <td class="col-name"><span class="assoc-ap-name">${escapeHTML(ap.hostname || ap.ip_address || 'Unknown')}</span></td>
      <td class="col-site">${escapeHTML(ap.site_name || '—')}</td>
      <td class="col-clients">${stas.length}</td>
      <td class="col-distribution">${distBar}</td>
      <td class="col-poor ${buckets.poor.length > 0 ? 'text-danger' : ''}">${buckets.poor.length || '—'}</td>
      <td class="col-poor-pct ${poorPct > 20 ? 'text-danger' : poorPct > 10 ? 'text-warning' : ''}">${poorPct > 0 ? `${poorPct}%` : '—'}</td>
      <td class="col-signal ${worstSignalClass}">${worstSignal ? `${worstSignal}` : '—'}</td>
      <td class="col-signal">${avgSignal || '—'}</td>
      <td class="col-health"><span class="health-badge ${healthClass}">${healthScore}%</span></td>
    </tr>
    ${expandedContent}
  `
}

function renderIssuesQueue(container, apDataSignal = [], apDataMod = []) {
  const issues = []

  // Map modulation data by AP id for quick lookup
  const modByApId = new Map((apDataMod || []).map(d => [d.ap?.id, d]))

  // --------------------------------------------
  // AP-level issues (COMBINED: signal + modulation)
  // --------------------------------------------
  ;(apDataSignal || []).forEach(sd => {
    const ap = sd.ap
    const md = modByApId.get(ap?.id)

    const signalPoorPct = sd.poorPct || 0
    const signalPoorCount = sd.buckets?.poor?.length || 0
    const signalOnline = sd.onlineCount || 0

    const modPoorPct = md?.poorPct || 0
    const modPoorCount = md?.buckets?.poor?.length || 0
    const modOnline = md?.onlineCount || 0

    // Prefer offline count from signal view (based on device status + missing signal)
    const offlineCount = sd.buckets?.offline?.length || 0

    const hasSignalProblem = signalPoorPct > 20 && signalPoorCount > 0
    const hasModProblem = modPoorPct > 20 && modPoorCount > 0
    const hasOfflineProblem = offlineCount > 5

    if (!hasSignalProblem && !hasModProblem && !hasOfflineProblem) return

    const reasons = []
    if (signalPoorCount > 0) {
      reasons.push(`${signalPoorPct}% poor signal (${signalPoorCount}/${Math.max(1, signalOnline)})`)
    }
    if (modPoorCount > 0) {
      reasons.push(`${modPoorPct}% poor modulation (${modPoorCount}/${Math.max(1, modOnline)})`)
    }
    if (offlineCount > 0) {
      reasons.push(`${offlineCount} offline`)
    }

    let severity = 'info'
    if (signalPoorPct > 50 || modPoorPct > 50) severity = 'critical'
    else if (signalPoorPct > 20 || modPoorPct > 20 || offlineCount > 20) severity = 'warning'

    issues.push({
      type: 'ap_quality',
      severity,
      title: `${ap.hostname || ap.ip_address}: ${reasons.join('; ')}`,
      description: `${sd.stas?.length || 0} total clients` + (ap.site_name ? ` • ${ap.site_name}` : ''),
      device: ap,
      // Larger = worse for sorting
      metric: Math.max(signalPoorPct, modPoorPct, offlineCount)
    })
  })

// --------------------------------------------
// High INTERFERENCE on AP radios (Wave channel utilization)
// --------------------------------------------
;(apDataSignal || []).forEach(sd => {
  const ap = sd.ap;
  if (!ap) return;

	  const intInfo = getMaxInterference(ap);
	  const thresholds = getInterferenceThresholds();
	  if (!intInfo || intInfo.value < thresholds.warning) return;

	  const severity =
	    intInfo.value >= thresholds.critical ? 'critical' : 'warning';
  const bandLabel = intInfo.band ? ` (${intInfo.band})` : '';
  const stateNote =
    intInfo.linkState && intInfo.linkState !== 'connected'
      ? ` • ${intInfo.linkState}`
      : '';
  const siteNote = ap.site_name ? ` • ${ap.site_name}` : '';

  issues.push({
    type: 'ap_interference',
    severity,
    title: `${ap.hostname || ap.ip_address}: ${intInfo.value.toFixed(2)}% interference${bandLabel}`,
    description: `Interference airtime: ${intInfo.value.toFixed(2)}%${stateNote}${siteNote}`,
    device: ap,
    metric: intInfo.value
  });
});

  // --------------------------------------------
  // Directional RF issues (CINR / EVM / SNR / LinkScore)
  //
  // Goal: help distinguish AP-wide RX issues vs AP-wide TX issues, and also
  // highlight individual clients with poor RX/TX when it's not AP-wide.
  ;(apDataSignal || []).forEach(sd => {
    const ap = sd.ap
    const stas = Array.isArray(sd.stas) ? sd.stas : []

    // Consider connected clients only, and only when we have directional metrics
    const rfClients = stas
      .filter(sta => sta.connected)
      .map(sta => ({ sta, rf: getStaDirectionalRFInfo(sta) }))
      .filter(x => x.rf && x.rf.available)

    if (rfClients.length === 0) {
      return
    }

    const dlPoor = rfClients.filter(x => x.rf.dlQuality === 'poor')
    const ulPoor = rfClients.filter(x => x.rf.ulQuality === 'poor')

    const total = rfClients.length
    const dlPoorRatio = dlPoor.length / total
    const ulPoorRatio = ulPoor.length / total

    // AP-wide thresholds
    const minForAP = 3
    const txIssue = total >= minForAP && dlPoor.length >= minForAP && dlPoorRatio >= 0.5
    const rxIssue = total >= minForAP && ulPoor.length >= minForAP && ulPoorRatio >= 0.5

    if (txIssue || rxIssue) {
      const worstRatio = Math.max(dlPoorRatio, ulPoorRatio)
      const severity = worstRatio >= 0.75 ? 'critical' : 'warning'
      const directionLabel = txIssue && rxIssue ? 'TX + RX' : txIssue ? 'TX' : 'RX'

      issues.push({
        type: txIssue && rxIssue ? 'ap_directional_txrx' : txIssue ? 'ap_directional_tx' : 'ap_directional_rx',
        severity,
        title: `${ap.hostname || ap.ip_address}: suspected ${directionLabel} impairment`,
        description: `Directional quality: DL poor ${dlPoor.length}/${total}, UL poor ${ulPoor.length}/${total}`,
        device: ap,
        // Larger = worse for sorting
        metric: worstRatio * 100,
      })
    }

    // Client-specific issues when it doesn't look AP-wide
    if (!txIssue) {
      const badRx = rfClients
        .filter(x => x.rf.dlQuality === 'poor' && x.rf.ulQuality !== 'poor')
        .sort((a, b) => (a.rf.dlMetric?.value ?? 999) - (b.rf.dlMetric?.value ?? 999))

      badRx.slice(0, 5).forEach(x => {
        issues.push({
          type: 'client_bad_rx',
          severity: 'warning',
          title: `${x.sta.hostname || x.sta.ip_address}: poor DL (${x.rf.dlMetric?.label || x.rf.dlQuality})`,
          description: `Connected to ${ap.hostname || ap.ip_address} • UL ${x.rf.ulMetric?.label || x.rf.ulQuality}`,
          device: x.sta,
          // Larger = worse for sorting (lower metric value => closer to 0)
          metric: -Number(x.rf.dlMetric?.value || 0),
        })
      })
    }

    if (!rxIssue) {
      const badTx = rfClients
        .filter(x => x.rf.ulQuality === 'poor' && x.rf.dlQuality !== 'poor')
        .sort((a, b) => (a.rf.ulMetric?.value ?? 999) - (b.rf.ulMetric?.value ?? 999))

      badTx.slice(0, 5).forEach(x => {
        issues.push({
          type: 'client_bad_tx',
          severity: 'warning',
          title: `${x.sta.hostname || x.sta.ip_address}: poor UL (${x.rf.ulMetric?.label || x.rf.ulQuality})`,
          description: `Connected to ${ap.hostname || ap.ip_address} • DL ${x.rf.dlMetric?.label || x.rf.dlQuality}`,
          device: x.sta,
          metric: -Number(x.rf.ulMetric?.value || 0),
        })
      })
    }
  })

  // Critical SIGNAL clients (very low signal)
  // --------------------------------------------
  const criticalSignalClients = []
  ;(apDataSignal || []).forEach(sd => {
    const ap = sd.ap
    ;(sd.buckets?.poor || []).forEach(sta => {
      const signal = getDeviceSignal(sta)
      if (!signal || signal <= -100) return

      const band = getDeviceBand(sta)
      const poorThreshold = SIGNAL_THRESHOLDS[band]?.fair || -70
      const criticalThreshold = poorThreshold - 10
      if (signal < criticalThreshold) {
        criticalSignalClients.push({ sta, ap, signal, criticalThreshold })
      }
    })
  })

  criticalSignalClients.sort((a, b) => a.signal - b.signal)
  criticalSignalClients.slice(0, 20).forEach(({ sta, ap, signal, criticalThreshold }) => {
    issues.push({
      type: 'critical_signal',
      severity: signal < (criticalThreshold - 5) ? 'critical' : 'warning',
      title: `${sta.hostname || sta.ip_address}: ${signal} dBm`,
      description: `Connected to ${ap.hostname || ap.ip_address}`,
      device: sta,
      metric: Math.abs(signal)
    })
  })

  // --------------------------------------------
  // Critical MODULATION clients (very low MCS / very low rate)
  // --------------------------------------------
  const criticalModClients = []
  ;(apDataMod || []).forEach(md => {
    const ap = md.ap
    ;(md.buckets?.poor || []).forEach(sta => {
      const info = getDeviceModulationInfo(sta)
      if (!info || !info.value) return
      const classified = classifyModulationInfo(info)
      if (!classified?.critical) return
      criticalModClients.push({ sta, ap, info })
    })
  })

  // Sort worst-first: for MCS/X/Mbps lower is worse
  criticalModClients.sort((a, b) => (a.info.value || 0) - (b.info.value || 0))
  criticalModClients.slice(0, 20).forEach(({ sta, ap, info }) => {
    issues.push({
      type: 'critical_modulation',
      severity: 'critical',
      title: `${sta.hostname || sta.ip_address}: ${info.short || 'Poor modulation'}`,
      description: `Connected to ${ap.hostname || ap.ip_address}`,
      device: sta,
      // Lower modulation values are worse; use an inverted metric so that
      // "larger = worse" for queue sorting.
      metric: typeof info.value === 'number' ? -info.value : 0
    })
  })

  // Sort by severity, then metric
  const severityOrder = { critical: 0, warning: 1, info: 2 }
  issues.sort((a, b) => {
    const sev = severityOrder[a.severity] - severityOrder[b.severity]
    if (sev !== 0) return sev
    return (b.metric || 0) - (a.metric || 0)
  })

  if (issues.length === 0) {
    container.innerHTML = `
      <div class="assoc-empty">
        <div class="assoc-empty-icon">✓</div>
        <p>No issues detected</p>
        <p class="text-muted">No issues were detected.</p>
      </div>
    `
    return
  }

  container.innerHTML = `
    <div class="issues-queue">
      <div class="issues-header">
        <span class="issues-count">${issues.length} issues</span>
        <div class="issues-filters">
          <label class="form-check-inline"><input type="checkbox" id="showCritical" checked> Critical</label>
          <label class="form-check-inline"><input type="checkbox" id="showWarning" checked> Warning</label>
          <label class="form-check-inline"><input type="checkbox" id="showInfo" checked> Info</label>
        </div>
      </div>
      <div class="issues-list">
        ${issues.map((issue, i) => `
          <div class="issue-item ${issue.severity}" data-id="${issue.device.id}" data-index="${i}">
            <div class="issue-severity ${issue.severity}">
              ${issue.severity === 'critical' ? '!' : issue.severity === 'warning' ? '⚠' : 'ℹ'}
            </div>
            <div class="issue-content">
              <div class="issue-title">${escapeHTML(issue.title)}</div>
              <div class="issue-desc">${escapeHTML(issue.description)}</div>
            </div>
            <div class="issue-type">${issue.type.replace(/_/g, ' ')}</div>
          </div>
        `).join('')}
      </div>
    </div>
  `

  // Click handlers for issue items
  container.querySelectorAll('.issue-item').forEach(item => {
    item.addEventListener('click', e => {
      e.stopPropagation()
      const id = parseInt(item.dataset.id)
      showAssocDeviceDetails(id)
    })
  })

  // Filter checkbox handlers
  const filterCheckboxes = container.querySelectorAll('.issues-filters input[type="checkbox"]')
  filterCheckboxes.forEach(cb => {
    cb.addEventListener('click', e => e.stopPropagation())
    cb.addEventListener('change', () => {
      const showCritical = document.getElementById('showCritical')?.checked !== false
      const showWarning = document.getElementById('showWarning')?.checked !== false
      const showInfo = document.getElementById('showInfo')?.checked !== false

      // Filter and show/hide issue items
      container.querySelectorAll('.issue-item').forEach(item => {
        const isCritical = item.classList.contains('critical')
        const isWarning = item.classList.contains('warning')
        const isInfo = item.classList.contains('info')

        const visible = (isCritical && showCritical) ||
                       (isWarning && showWarning) ||
                       (isInfo && showInfo)
        item.style.display = visible ? '' : 'none'
      })

      // Update count
      const visibleCount = container.querySelectorAll('.issue-item:not([style*="display: none"])').length
      const countEl = container.querySelector('.issues-count')
      if (countEl) countEl.textContent = `${visibleCount} issues`
    })
  })
}


function showAssocDeviceDetails(deviceId) {
  const device = store.devices.find(d => d.id === deviceId)
  if (!device) return
  
  // Just set the selectedDevice - store.on() will handle showing the panel
  store.set({ selectedDevice: deviceId })
}

function showAllClientsPanel(apId, mode = 'signal') {
  const ap = store.devices.find(d => d.id === apId)
  if (!ap) return

  const stas = store.getSTAs(apId)
  const panel = document.getElementById('detailPanel')
  if (!panel) return

  const isMod = mode === 'modulation'

  // Sort by selected metric (worst first)
  const sorted = [...stas].sort((a, b) => {
    if (isMod) {
      const infoA = getDeviceModulationInfo(a)
      const infoB = getDeviceModulationInfo(b)
      const valA = infoA?.value || 0
      const valB = infoB?.value || 0
      // Lower modulation value = worse
      if (valA && valB) return valA - valB
      if (valA && !valB) return -1
      if (!valA && valB) return 1
      return 0
    }

    const sigA = getDeviceSignal(a) || -100
    const sigB = getDeviceSignal(b) || -100
    // More negative signal = worse
    return sigA - sigB
  })

  const titleMetric = isMod ? 'Modulation Rates' : 'Signal Levels'

  panel.innerHTML = `
    <div class="detail-header">
      <div class="detail-title">
        <h3>${escapeHTML(ap.hostname || ap.ip_address)} - All Clients (${escapeHTML(titleMetric)})</h3>
      </div>
      <button class="btn btn-icon detail-close" id="detailClose">&times;</button>
    </div>
    <div class="detail-body">
      <div class="detail-section">
        <h4>${stas.length} clients</h4>
        <div class="assoc-all-list">
          ${sorted.map(sta => {
            const staStatus = store.getStatus(sta)

            if (isMod) {
              const info = getDeviceModulationInfo(sta)
              const classified = classifyModulationInfo(info)
              const q = classified?.quality || ''
              const metricClass = staStatus !== 'online' || !info?.value ? 'offline' : q
              const label = info?.short || '—'
              return `
                <div class="assoc-all-row" data-id="${sta.id}">
                  <span class="status-dot ${staStatus}"></span>
                  <span class="assoc-all-name">${escapeHTML(sta.hostname || sta.ip_address || sta.mac || 'Unknown')}</span>
                  <span class="assoc-all-signal ${metricClass}">${escapeHTML(label)}</span>
                </div>
              `
            }

            const signal = getDeviceSignal(sta)
            const band = getDeviceBand(sta)
            const signalClass = getSignalQuality(signal, band)
            return `
              <div class="assoc-all-row" data-id="${sta.id}">
                <span class="status-dot ${staStatus}"></span>
                <span class="assoc-all-name">${escapeHTML(sta.hostname || sta.ip_address || sta.mac || 'Unknown')}</span>
                <span class="assoc-all-signal ${signalClass}">${signal ? `${signal} dBm` : '—'}</span>
              </div>
            `
          }).join('')}
        </div>
      </div>
    </div>
  `

  panel.classList.remove('hidden')

  // Close button
  document.getElementById('detailClose')?.addEventListener('click', () => {
    panel.classList.add('hidden')
    store.set({ selectedDevice: null })
  })

  // Click to show individual details
  panel.querySelectorAll('.assoc-all-row').forEach(row => {
    row.addEventListener('click', () => {
      const id = parseInt(row.dataset.id)
      showAssocDeviceDetails(id)
    })
  })
}
function formatUptime(seconds) {
  if (!seconds) return ''
  const d = Math.floor(seconds / 86400)
  const h = Math.floor((seconds % 86400) / 3600)
  if (d > 0) return `${d}d ${h}h`
  const m = Math.floor((seconds % 3600) / 60)
  return `${h}h ${m}m`
}

// ===== CONFIG PAGE =====

// Antenna model presets (degrees)
// These defaults are used for integrated-antenna products. Beamwidth can be overridden per device.
const ANTENNA_MODELS = [
  { id: '', name: 'Unknown / External Antenna', beamwidth_h_deg: null, beamwidth_v_deg: null, electrical_downtilt_deg: 0 },

  // Wave 60GHz family (integrated antennas)
  { id: 'wave-ap', name: 'Wave AP (30° x 3°)', beamwidth_h_deg: 30, beamwidth_v_deg: 3, electrical_downtilt_deg: 0 },
  { id: 'wave-ap-gen2', name: 'Wave AP Gen2 (90° x 7°)', beamwidth_h_deg: 90, beamwidth_v_deg: 7, electrical_downtilt_deg: 0 },
  { id: 'wave-ap-micro', name: 'Wave AP Micro (90° x 7°)', beamwidth_h_deg: 90, beamwidth_v_deg: 7, electrical_downtilt_deg: 0 },
  { id: 'wave-pro', name: 'Wave Pro (1.3° x 1.3°)', beamwidth_h_deg: 1.3, beamwidth_v_deg: 1.3, electrical_downtilt_deg: 0 },
  { id: 'wave-nano', name: 'Wave Nano (1.4° x 1.4°)', beamwidth_h_deg: 1.4, beamwidth_v_deg: 1.4, electrical_downtilt_deg: 0 },
  { id: 'wave-pico', name: 'Wave Pico (6° x 6°)', beamwidth_h_deg: 6, beamwidth_v_deg: 6, electrical_downtilt_deg: 0 },
  { id: 'wave-lr', name: 'Wave Long-Range (1° x 1°)', beamwidth_h_deg: 1, beamwidth_v_deg: 1, electrical_downtilt_deg: 0 },

  // airFiber 60 family (integrated antennas)
  { id: 'af60-lr', name: 'airFiber 60 LR (1.6° x 1.6°)', beamwidth_h_deg: 1.6, beamwidth_v_deg: 1.6, electrical_downtilt_deg: 0 },
  { id: 'af60-hd', name: 'airFiber 60 HD (3° x 3°)', beamwidth_h_deg: 3, beamwidth_v_deg: 3, electrical_downtilt_deg: 0 },
  { id: 'af60-xg', name: 'airFiber 60 XG (5° x 5°)', beamwidth_h_deg: 5, beamwidth_v_deg: 5, electrical_downtilt_deg: 0 },
  { id: 'af60-xr', name: 'airFiber 60 XR (1.4° x 1.4°)', beamwidth_h_deg: 1.4, beamwidth_v_deg: 1.4, electrical_downtilt_deg: 0 },

  // Common integrated PtMP/PtP radios
  { id: 'ltu-lite', name: 'LTU Lite (45° x 45°)', beamwidth_h_deg: 45, beamwidth_v_deg: 45, electrical_downtilt_deg: 0 },
  { id: 'ltu-lr', name: 'LTU Long-Range (14° x 14°)', beamwidth_h_deg: 14, beamwidth_v_deg: 14, electrical_downtilt_deg: 0 },
  { id: 'lbe-5ac-xr', name: 'LiteBeam 5AC XR (5.5° x 5.5°)', beamwidth_h_deg: 5.5, beamwidth_v_deg: 5.5, electrical_downtilt_deg: 0 },

  // airMAX PowerBeam integrated dishes
  // NOTE: These are approximate 3 dB beamwidths based on dish diameter at ~5 GHz.
  { id: 'pbe-5ac-400', name: 'PowerBeam 5AC 400 (10° x 10°)', beamwidth_h_deg: 10, beamwidth_v_deg: 10, electrical_downtilt_deg: 0 },
  { id: 'pbe-5ac-500', name: 'PowerBeam 5AC 500 (8° x 8°)', beamwidth_h_deg: 8, beamwidth_v_deg: 8, electrical_downtilt_deg: 0 },

  // airMAX / airPrism sector antennas
  // NOTE: These are intended for RF modeling defaults (coverage + elevation beamwidth).
  // Values here follow Ubiquiti's published coverage and vertical beamwidth numbers.
  { id: 'am-9m13', name: 'AM-9M13 (900 MHz, 13 dBi)', beamwidth_h_deg: 120, beamwidth_v_deg: 15, electrical_downtilt_deg: 0 },
  { id: 'am-2g15-120', name: 'AM-2G15-120 (2.4 GHz, 15 dBi)', beamwidth_h_deg: 120, beamwidth_v_deg: 9, electrical_downtilt_deg: 4 },
  { id: 'am-2g16-90', name: 'AM-2G16-90 (2.4 GHz, 16 dBi)', beamwidth_h_deg: 90, beamwidth_v_deg: 9, electrical_downtilt_deg: 4 },
  { id: 'am-3g18-120', name: 'AM-3G18-120 (3 GHz, 18 dBi)', beamwidth_h_deg: 120, beamwidth_v_deg: 6, electrical_downtilt_deg: 3 },
  { id: 'am-5g16-120', name: 'AM-5G16-120 (5 GHz, 16 dBi)', beamwidth_h_deg: 120, beamwidth_v_deg: 8, electrical_downtilt_deg: 4 },
  { id: 'am-5g17-90', name: 'AM-5G17-90 (5 GHz, 17 dBi)', beamwidth_h_deg: 90, beamwidth_v_deg: 8, electrical_downtilt_deg: 4 },
  { id: 'am-5g19-120', name: 'AM-5G19-120 (5 GHz, 19 dBi)', beamwidth_h_deg: 120, beamwidth_v_deg: 4, electrical_downtilt_deg: 2 },
  { id: 'am-5g20-90', name: 'AM-5G20-90 (5 GHz, 20 dBi)', beamwidth_h_deg: 90, beamwidth_v_deg: 4, electrical_downtilt_deg: 2 },
  { id: 'am-5ac21-60', name: 'AM-5AC21-60 (5 GHz, 21 dBi)', beamwidth_h_deg: 60, beamwidth_v_deg: 4, electrical_downtilt_deg: 2 },
  { id: 'am-5ac22-45', name: 'AM-5AC22-45 (5 GHz, 22 dBi)', beamwidth_h_deg: 45, beamwidth_v_deg: 4, electrical_downtilt_deg: 2 },
  // NOTE: Ubiquiti lists this as "3 x 30°". For modeling, we treat it as ~90° H x 30° V.
  { id: 'ap-5ac-90-hd', name: 'AP-5AC-90-HD (5 GHz, 22 dBi)', beamwidth_h_deg: 90, beamwidth_v_deg: 30, electrical_downtilt_deg: 2 },

  // Titanium Sector Antennas (adjustable beamwidth)
  { id: 'am-v2g-ti-60', name: 'AM-V2G-Ti 60° (17 dBi)', beamwidth_h_deg: 60, beamwidth_v_deg: 4, electrical_downtilt_deg: 4 },
  { id: 'am-v2g-ti-90', name: 'AM-V2G-Ti 90° (16 dBi)', beamwidth_h_deg: 90, beamwidth_v_deg: 4, electrical_downtilt_deg: 4 },
  { id: 'am-v2g-ti-120', name: 'AM-V2G-Ti 120° (15 dBi)', beamwidth_h_deg: 120, beamwidth_v_deg: 4, electrical_downtilt_deg: 4 },
  { id: 'am-v5g-ti-60', name: 'AM-V5G-Ti 60° (21 dBi)', beamwidth_h_deg: 60, beamwidth_v_deg: 4, electrical_downtilt_deg: 2 },
  { id: 'am-v5g-ti-90', name: 'AM-V5G-Ti 90° (20 dBi)', beamwidth_h_deg: 90, beamwidth_v_deg: 4, electrical_downtilt_deg: 2 },
  { id: 'am-v5g-ti-120', name: 'AM-V5G-Ti 120° (19 dBi)', beamwidth_h_deg: 120, beamwidth_v_deg: 4, electrical_downtilt_deg: 2 },
  { id: 'am-m-v5g-ti-60', name: 'AM-M-V5G-Ti 60° (17 dBi)', beamwidth_h_deg: 60, beamwidth_v_deg: 8, electrical_downtilt_deg: 3 },
  { id: 'am-m-v5g-ti-90', name: 'AM-M-V5G-Ti 90° (16 dBi)', beamwidth_h_deg: 90, beamwidth_v_deg: 8, electrical_downtilt_deg: 3 },
  { id: 'am-m-v5g-ti-120', name: 'AM-M-V5G-Ti 120° (15 dBi)', beamwidth_h_deg: 120, beamwidth_v_deg: 8, electrical_downtilt_deg: 3 },
]

const ANTENNA_MODEL_MAP = new Map(ANTENNA_MODELS.map(m => [m.id, m]))

function guessAntennaModelId(device) {
  // Prefer explicit device field (DB) if present
  if (device && device.antenna_model) return String(device.antenna_model)

  const s = `${device?.model || ''} ${device?.product || ''}`.toLowerCase()

  // Wave AP Micro often appears as "WAVE-AP-MICRO" (model) or "Wave AP Micro" (product).
  if (s.includes('wave-ap-micro') || s.includes('wave ap micro')) return 'wave-ap-micro'

  if (s.includes('wave-ap-gen2')) return 'wave-ap-gen2'
  if (s.includes('wave-ap')) return 'wave-ap'
  if (s.includes('wave-pro')) return 'wave-pro'
  if (s.includes('wave-nano')) return 'wave-nano'
  if (s.includes('wave-pico')) return 'wave-pico'
  if (s.includes('wave-lr')) return 'wave-lr'

  if (s.includes('af60-lr') || s.includes('airfiber 60 long-range')) return 'af60-lr'
  if (s.includes('af60-hd') || s.includes('airfiber 60 hd')) return 'af60-hd'
  if (s.includes('af60-xg') || s.includes('airfiber 60 xg')) return 'af60-xg'
  if (s.includes('af60-xr') || s.includes('airfiber 60 xr') || s.includes('xtreme range')) return 'af60-xr'

  if (s.includes('ltu-lite')) return 'ltu-lite'
  if (s.includes('ltu-lr') || s.includes('ltu lr') || s.includes('ltu long-range') || s.includes('ltu long range')) return 'ltu-lr'
  if (s.includes('lbe-5ac-xr')) return 'lbe-5ac-xr'

  // PowerBeam AC dishes (approximate beamwidth defaults)
  if (s.includes('pbe-5ac-400') || s.includes('powerbeam 5ac 400')) return 'pbe-5ac-400'
  if (s.includes('pbe-5ac-500') || s.includes('pbe-5ac-50') || s.includes('powerbeam 5ac 500') || s.includes('powerbeam 5ac 50')) return 'pbe-5ac-500'

  // Fallback: Wave platform APs are generally integrated antennas.
  if (device?.platform === 'wave' && (device?.role || '').toLowerCase() === 'ap') return 'wave-ap'

  return ''
}

function parseNullableFloat(val) {
  const t = String(val || '').trim()
  if (t === '') return null
  const n = parseFloat(t)
  return Number.isFinite(n) ? n : null
}

function parseNullableInt(val) {
  const t = String(val || '').trim()
  if (t === '') return null
  const n = parseInt(t, 10)
  return Number.isFinite(n) ? n : null
}

// Derive a default planning "tech" code from a center frequency (MHz).
//
// This is only a convenience fallback for export when the operator has not
// explicitly configured a tech code. Codes are intentionally coarse.
function deriveTechFromFrequencyMHz(freqMHz) {
  const f = Number(freqMHz)
  if (!Number.isFinite(f) || f <= 0) return null
  if (f < 1000) return 69   // 900 MHz class
  if (f >= 2300 && f < 2700) return 70
  if (f >= 3300 && f < 4000) return 71
  if (f >= 4900 && f < 6200) return 72
  return null
}
function renderConfigPage(container) {
  container.innerHTML = `
    <div class="page page-config">
      <div class="config-sections">
        <section class="config-section">
          <h3>Config Backups</h3>
          <p>Backup and restore device configurations.</p>
          <div class="config-actions">
            <button class="btn btn-primary" id="btnBackupAll">Backup All Devices</button>
            <button class="btn btn-secondary" id="btnViewBackups">View Backups</button>
          </div>
          <div id="backupsList" class="backups-list"></div>
        </section>
        
        <section class="config-section">
          <h3>Batch Configuration</h3>
          <p>Push configuration changes to multiple devices at once.</p>
          <button class="btn btn-primary" id="btnBatchConfig">Open Batch Config</button>
        </section>
        
        <section class="config-section">
          <h3>Device Templates</h3>
          <p>Create configuration templates for quick deployment.</p>
          <div class="templates-list">
            <p class="text-muted">No templates configured.</p>
          </div>
        </section>

        <section class="config-section">
          <h3>AP Antenna Parameters</h3>
          <p>Set antenna azimuth, downtilt, and beamwidth for RF modeling (optional).</p>
          <button class="btn btn-primary" id="btnAntennaConfig">Manage Antennas</button>
        </section>

        <section class="config-section">
          <h3>Sector CSV Export</h3>
          <p>Export site + sector planning fields as CSV for external modeling tools.</p>
          <button class="btn btn-secondary" id="btnExportSectorsCSV">Export CSV</button>
        </section>
      </div>
    </div>
  `
  
  document.getElementById('btnBackupAll')?.addEventListener('click', async () => {
    const deviceIds = store.devices.filter(d => !d.parent_id).map(d => d.id)
    if (deviceIds.length === 0) {
      showToast('No devices to backup', 'error')
      return
    }
    
    try {
      await startTrackedJob('backup', deviceIds, { include_config: true })
      updateJobBadge()
    } catch (e) {
      showToast('Failed to start backup: ' + e.message, 'error')
    }
  })
  
  document.getElementById('btnBatchConfig')?.addEventListener('click', () => {
    showBatchConfigModal()
  })
  
  document.getElementById('btnViewBackups')?.addEventListener('click', () => {
    loadBackupsList()
  })

  document.getElementById('btnAntennaConfig')?.addEventListener('click', () => {
    showAntennaConfigModal()
  })

  document.getElementById('btnExportSectorsCSV')?.addEventListener('click', () => {
    showExportSectorsModal()
  })
}


function showAntennaConfigModal() {
  const modal = document.getElementById('antennaConfigModal')
  const body = document.getElementById('antennaConfigBody')
  if (!modal || !body) return

  // Keep the modal stable: do not rebuild the DOM on every device update.
  // We'll patch rows in place and preserve scroll position.
  let unsubscribe = null
  let updateTimer = null
  let filterTimer = null
  let docPointerHandler = null

  const close = () => {
    modal.classList.add('hidden')
    if (docPointerHandler) {
      document.removeEventListener('pointerdown', docPointerHandler, true)
      docPointerHandler = null
    }
    if (unsubscribe) {
      unsubscribe()
      unsubscribe = null
    }
    if (updateTimer) {
      clearTimeout(updateTimer)
      updateTimer = null
    }
    if (filterTimer) {
      clearTimeout(filterTimer)
      filterTimer = null
    }
  }
  const closeBtn = document.getElementById('closeAntennaConfig')
  const closeFooter = document.getElementById('closeAntennaConfigFooter')
  if (closeBtn) closeBtn.onclick = close
  if (closeFooter) closeFooter.onclick = close

  // Treat "AP" as "no parent" to match the rest of the UI.
  const aps = (store.devices || []).filter(d => !d.parent_id)

  // Stable ordering: site, then name, then IP.
  aps.sort((a, b) => {
    const sa = String(a.site_name || '').toLowerCase()
    const sb = String(b.site_name || '').toLowerCase()
    if (sa < sb) return -1
    if (sa > sb) return 1
    const na = String(a.hostname || '').toLowerCase()
    const nb = String(b.hostname || '').toLowerCase()
    if (na < nb) return -1
    if (na > nb) return 1
    return String(a.ip_address || '').localeCompare(String(b.ip_address || ''))
  })

  body.innerHTML = `
    <div class="antenna-toolbar">
      <input type="text" id="antennaFilterInput" class="search-input" placeholder="Search APs (name, IP, MAC)..." />
      <div class="text-muted antenna-count" id="antennaFilterCount">
        Showing ${aps.length} AP${aps.length === 1 ? '' : 's'}
      </div>
      <div class="text-muted antenna-autosave-note">Autosaves when you leave a row</div>
    </div>

    <div class="antenna-table-wrap">
      <table class="backups-table antenna-table antenna-table-compact">
        <colgroup>
          <col class="ant-col-ap">
          <col class="ant-col-ip">
          <col class="ant-col-mac">
          <col class="ant-col-model">
          <col class="ant-col-small">
          <col class="ant-col-med">
          <col class="ant-col-med">
          <col class="ant-col-override">
          <col class="ant-col-small">
          <col class="ant-col-small">
          <col class="ant-col-small">
          <col class="ant-col-xsmall">
          <col class="ant-col-xsmall">
          <col class="ant-col-xsmall">
          <col class="ant-col-small">
          <col class="ant-col-biz">
        </colgroup>
        <thead>
          <tr>
            <th>AP</th>
            <th>IP</th>
            <th>MAC</th>
            <th>Model Preset</th>
            <th>Az</th>
            <th>Mech DT</th>
            <th>Elec DT</th>
            <th>Ovr</th>
            <th>Beam H</th>
            <th>Beam V</th>
            <th>Radius</th>
            <th>Tech</th>
            <th>Down</th>
            <th>Up</th>
            <th>Latency</th>
            <th>Biz/Res</th>
          </tr>
        </thead>
        <tbody id="antennaTableBody"></tbody>
      </table>
    </div>

    <p class="text-muted antenna-help-copy">
      By default, integrated antennas use the preset <b>beamwidth</b> and <b>electrical downtilt</b>.
      Check <b>Ovr</b> to customize those values. Mechanical downtilt is always user-entered.
      Edited fields turn blue until the row is successfully saved.
    </p>
  `

  const tbody = document.getElementById('antennaTableBody')
  const countEl = document.getElementById('antennaFilterCount')
  if (!tbody) return

  const tableWrap = body.querySelector('.antenna-table-wrap')
  const savedStateById = new Map()
  const rowById = new Map()

  function sortKeyForDevice(d) {
    const s = String(d.site_name || '').toLowerCase()
    const n = String(d.hostname || '').toLowerCase()
    const ip = String(d.ip_address || '')
    return `${s}\u0000${n}\u0000${ip}`
  }

  function snapshotFromDevice(device) {
    const guessedModelId = guessAntennaModelId(device)
    const modelId = (device.antenna_model || guessedModelId || '')
    const preset = ANTENNA_MODEL_MAP.get(modelId)
    const hasDefaults = preset && preset.beamwidth_h_deg !== null && preset.beamwidth_v_deg !== null
    const overrideChecked = device.antenna_override === true || !hasDefaults
    const presetEDT = (preset && typeof preset.electrical_downtilt_deg === 'number') ? preset.electrical_downtilt_deg : 0

    return {
      antenna_model: modelId,
      antenna_override: overrideChecked,
      antenna_azimuth_deg: device.antenna_azimuth_deg ?? '',
      antenna_downtilt_deg: device.antenna_downtilt_deg ?? '',
      antenna_electrical_downtilt_deg: overrideChecked ? (device.antenna_electrical_downtilt_deg ?? presetEDT) : presetEDT,
      antenna_beamwidth_h_deg: overrideChecked ? (device.antenna_beamwidth_h_deg ?? (hasDefaults ? preset.beamwidth_h_deg : '')) : (hasDefaults ? preset.beamwidth_h_deg : ''),
      antenna_beamwidth_v_deg: overrideChecked ? (device.antenna_beamwidth_v_deg ?? (hasDefaults ? preset.beamwidth_v_deg : '')) : (hasDefaults ? preset.beamwidth_v_deg : ''),
      radius_m: device.radius_m ?? '',
      tech: device.tech ?? '',
      down_mbps: device.down_mbps ?? '',
      up_mbps: device.up_mbps ?? '',
      latency_ms: device.latency_ms ?? '',
      bizres: (String(device.bizres ?? 'X').trim().toUpperCase() || 'X'),
    }
  }

  function valueKey(v) {
    if (v === undefined || v === null || v === '') return ''
    if (typeof v === 'boolean') return v ? '1' : '0'
    return String(v)
  }

  function collectRowState(id) {
    return {
      antenna_model: document.getElementById(`ant-model-${id}`)?.value || '',
      antenna_override: document.getElementById(`ant-ovr-${id}`)?.checked || false,
      antenna_azimuth_deg: document.getElementById(`ant-az-${id}`)?.value ?? '',
      antenna_downtilt_deg: document.getElementById(`ant-dt-${id}`)?.value ?? '',
      antenna_electrical_downtilt_deg: document.getElementById(`ant-edt-${id}`)?.value ?? '',
      antenna_beamwidth_h_deg: document.getElementById(`ant-bwh-${id}`)?.value ?? '',
      antenna_beamwidth_v_deg: document.getElementById(`ant-bwv-${id}`)?.value ?? '',
      radius_m: document.getElementById(`ant-rad-${id}`)?.value ?? '',
      tech: document.getElementById(`ant-tech-${id}`)?.value ?? '',
      down_mbps: document.getElementById(`ant-down-${id}`)?.value ?? '',
      up_mbps: document.getElementById(`ant-up-${id}`)?.value ?? '',
      latency_ms: document.getElementById(`ant-lat-${id}`)?.value ?? '',
      bizres: (String(document.getElementById(`ant-biz-${id}`)?.value || 'X').trim().toUpperCase() || 'X'),
    }
  }

  function getRowFieldElements(id) {
    return {
      antenna_model: document.getElementById(`ant-model-${id}`),
      antenna_override: document.getElementById(`ant-ovr-${id}`),
      antenna_azimuth_deg: document.getElementById(`ant-az-${id}`),
      antenna_downtilt_deg: document.getElementById(`ant-dt-${id}`),
      antenna_electrical_downtilt_deg: document.getElementById(`ant-edt-${id}`),
      antenna_beamwidth_h_deg: document.getElementById(`ant-bwh-${id}`),
      antenna_beamwidth_v_deg: document.getElementById(`ant-bwv-${id}`),
      radius_m: document.getElementById(`ant-rad-${id}`),
      tech: document.getElementById(`ant-tech-${id}`),
      down_mbps: document.getElementById(`ant-down-${id}`),
      up_mbps: document.getElementById(`ant-up-${id}`),
      latency_ms: document.getElementById(`ant-lat-${id}`),
      bizres: document.getElementById(`ant-biz-${id}`),
    }
  }

  function updateDirtyState(tr) {
    if (!tr) return false
    const id = parseInt(tr.dataset.id)
    if (!Number.isFinite(id)) return false
    const saved = savedStateById.get(id) || {}
    const current = collectRowState(id)
    const elements = getRowFieldElements(id)
    let dirty = false
    for (const [key, el] of Object.entries(elements)) {
      if (!el) continue
      const changed = valueKey(current[key]) !== valueKey(saved[key])
      el.classList.toggle('antenna-field-dirty', changed)
      if (changed) dirty = true
    }
    tr.dataset.dirty = dirty ? '1' : ''
    tr.classList.toggle('antenna-row-dirty', dirty)
    return dirty
  }

  function buildSavePayload(id) {
    const deviceId = parseInt(id)
    const device = store.getDeviceById(deviceId) || (store.devices || []).find(d => d.id === deviceId)
    if (!device) return null

    const model = document.getElementById(`ant-model-${deviceId}`)?.value || ''
    const override = document.getElementById(`ant-ovr-${deviceId}`)?.checked || false
    const az = parseNullableFloat(document.getElementById(`ant-az-${deviceId}`)?.value)
    const dt = parseNullableFloat(document.getElementById(`ant-dt-${deviceId}`)?.value)

    const preset = ANTENNA_MODEL_MAP.get(model)
    const presetEDT = (preset && typeof preset.electrical_downtilt_deg === 'number') ? preset.electrical_downtilt_deg : 0

    let edt = parseNullableFloat(document.getElementById(`ant-edt-${deviceId}`)?.value)
    if (!override) {
      edt = presetEDT
    } else if (edt === null) {
      edt = presetEDT
    }

    let bwH = parseNullableFloat(document.getElementById(`ant-bwh-${deviceId}`)?.value)
    let bwV = parseNullableFloat(document.getElementById(`ant-bwv-${deviceId}`)?.value)

    const radius = parseNullableFloat(document.getElementById(`ant-rad-${deviceId}`)?.value)
    const tech = parseNullableInt(document.getElementById(`ant-tech-${deviceId}`)?.value)
    const down = parseNullableFloat(document.getElementById(`ant-down-${deviceId}`)?.value)
    const up = parseNullableFloat(document.getElementById(`ant-up-${deviceId}`)?.value)
    const latency = parseNullableFloat(document.getElementById(`ant-lat-${deviceId}`)?.value)
    const bizresRaw = String(document.getElementById(`ant-biz-${deviceId}`)?.value || 'X').trim().toUpperCase()
    const bizres = (bizresRaw === 'B' || bizresRaw === 'R' || bizresRaw === 'X') ? bizresRaw : 'X'

    if (!override) {
      if (preset && preset.beamwidth_h_deg !== null && preset.beamwidth_v_deg !== null) {
        bwH = preset.beamwidth_h_deg
        bwV = preset.beamwidth_v_deg
      } else {
        bwH = null
        bwV = null
      }
    } else {
      if (bwH === null && preset && preset.beamwidth_h_deg !== null) bwH = preset.beamwidth_h_deg
      if (bwV === null && preset && preset.beamwidth_v_deg !== null) bwV = preset.beamwidth_v_deg
    }

    return {
      antenna_model: model,
      antenna_override: override,
      antenna_azimuth_deg: az,
      antenna_downtilt_deg: dt,
      antenna_electrical_downtilt_deg: edt,
      antenna_beamwidth_h_deg: bwH,
      antenna_beamwidth_v_deg: bwV,
      radius_m: radius,
      tech: tech,
      down_mbps: down,
      up_mbps: up,
      latency_ms: latency,
      bizres: bizres,
    }
  }

  async function saveRowById(deviceId, { showErrorToast = true } = {}) {
    const id = parseInt(deviceId)
    if (!Number.isFinite(id)) return false
    const tr = rowById.get(id)
    if (!tr || tr.dataset.saving === '1') return false

    const isDirty = updateDirtyState(tr)
    if (!isDirty) return false

    const device = store.getDeviceById(id) || (store.devices || []).find(d => d.id === id)
    if (!device) return false

    const payload = buildSavePayload(id)
    if (!payload) return false

    tr.dataset.saving = '1'
    tr.classList.add('antenna-row-saving')

    try {
      const resp = await api.updateDeviceAntenna(id, payload)
      Object.assign(device, resp)
      savedStateById.set(id, snapshotFromDevice(device))
      delete tr.dataset.dirty
      tr.classList.remove('antenna-row-dirty')
      updateRowFromDevice(tr, device, { force: true })
      store.set({ devices: store.devices })
      return true
    } catch (err) {
      if (showErrorToast) {
        showToast('Failed to save antenna parameters: ' + err.message, 'error')
      }
      updateDirtyState(tr)
      return false
    } finally {
      delete tr.dataset.saving
      tr.classList.remove('antenna-row-saving')
    }
  }

  function buildRow(device) {
    const id = device.id
    const name = escapeHTML(device.hostname || `Device ${id}`)
    const ip = escapeHTML(device.ip_address || '')
    const mac = escapeHTML((device.mac || '').toLowerCase())

    const guessedModelId = guessAntennaModelId(device)
    const modelId = (device.antenna_model || guessedModelId || '')
    const preset = ANTENNA_MODEL_MAP.get(modelId)

    const hasDefaults = preset && preset.beamwidth_h_deg !== null && preset.beamwidth_v_deg !== null
    const overrideChecked = device.antenna_override === true || !hasDefaults

    const az = (device.antenna_azimuth_deg ?? '')
    const dt = (device.antenna_downtilt_deg ?? '')
    const radius = (device.radius_m ?? '')
    const tech = (device.tech ?? '')
    const techPH = deriveTechFromFrequencyMHz(device.frequency)
    const down = (device.down_mbps ?? '')
    const up = (device.up_mbps ?? '')
    const latency = (device.latency_ms ?? '')
    const bizres = (String(device.bizres ?? 'X').trim().toUpperCase() || 'X')
    const presetEDT = (preset && typeof preset.electrical_downtilt_deg === 'number') ? preset.electrical_downtilt_deg : 0
    const edt = overrideChecked ? (device.antenna_electrical_downtilt_deg ?? presetEDT) : presetEDT
    const bwH = overrideChecked ? (device.antenna_beamwidth_h_deg ?? (hasDefaults ? preset.beamwidth_h_deg : '')) : (hasDefaults ? preset.beamwidth_h_deg : '')
    const bwV = overrideChecked ? (device.antenna_beamwidth_v_deg ?? (hasDefaults ? preset.beamwidth_v_deg : '')) : (hasDefaults ? preset.beamwidth_v_deg : '')

    const options = ANTENNA_MODELS.map(m => {
      const sel = m.id === modelId ? 'selected' : ''
      return `<option value="${escapeAttr(m.id)}" ${sel}>${escapeHTML(m.name)}</option>`
    }).join('')

    const searchBlob = escapeAttr(`${device.hostname || ''} ${device.ip_address || ''} ${device.mac || ''} ${device.model || ''} ${device.product || ''} ${device.site_name || ''}`.toLowerCase())
    const sortKey = escapeAttr(sortKeyForDevice(device))

    return `
      <tr data-id="${id}" data-search="${searchBlob}" data-sort="${sortKey}">
        <td><span class="antenna-cell-text">${name}</span></td>
        <td><span class="antenna-cell-text">${ip}</span></td>
        <td><span class="antenna-cell-text">${mac}</span></td>
        <td>
          <select id="ant-model-${id}" class="select-sm ant-field ant-model ant-w-model">${options}</select>
        </td>
        <td><input type="number" step="0.1" id="ant-az-${id}" value="${escapeAttr(az)}" class="input-sm ant-field ant-w-xs" /></td>
        <td><input type="number" step="0.1" id="ant-dt-${id}" value="${escapeAttr(dt)}" class="input-sm ant-field ant-w-sm" /></td>
        <td><input type="number" step="0.1" id="ant-edt-${id}" value="${escapeAttr(edt)}" class="input-sm ant-field ant-edt ant-w-sm" ${overrideChecked ? '' : 'disabled'} /></td>
        <td class="ant-ovr-cell">
          <input type="checkbox" id="ant-ovr-${id}" class="ant-field ant-ovr" ${overrideChecked ? 'checked' : ''} />
        </td>
        <td><input type="number" step="0.1" id="ant-bwh-${id}" value="${escapeAttr(bwH)}" class="input-sm ant-field ant-bwh ant-w-xs" ${overrideChecked ? '' : 'disabled'} /></td>
        <td><input type="number" step="0.1" id="ant-bwv-${id}" value="${escapeAttr(bwV)}" class="input-sm ant-field ant-bwv ant-w-xs" ${overrideChecked ? '' : 'disabled'} /></td>
        <td><input type="number" step="1" id="ant-rad-${id}" value="${escapeAttr(radius)}" class="input-sm ant-field ant-w-xs" /></td>
        <td><input type="number" step="1" id="ant-tech-${id}" value="${escapeAttr(tech)}" placeholder="${escapeAttr(techPH ?? '')}" class="input-sm ant-field ant-w-xxs" /></td>
        <td><input type="number" step="0.1" id="ant-down-${id}" value="${escapeAttr(down)}" class="input-sm ant-field ant-w-xxs" /></td>
        <td><input type="number" step="0.1" id="ant-up-${id}" value="${escapeAttr(up)}" class="input-sm ant-field ant-w-xxs" /></td>
        <td><input type="number" step="0.1" id="ant-lat-${id}" value="${escapeAttr(latency)}" class="input-sm ant-field ant-w-xs" /></td>
        <td>
          <select id="ant-biz-${id}" class="select-sm ant-field ant-w-biz">
            <option value="X" ${bizres === 'X' ? 'selected' : ''}>Both</option>
            <option value="B" ${bizres === 'B' ? 'selected' : ''}>Business</option>
            <option value="R" ${bizres === 'R' ? 'selected' : ''}>Residential</option>
          </select>
        </td>
      </tr>
    `
  }

  tbody.innerHTML = aps.map(buildRow).join('')
  const rowListInit = Array.from(tbody.querySelectorAll('tr'))
  let rowList = rowListInit
  for (const tr of rowList) {
    const id = parseInt(tr.dataset.id)
    if (!Number.isFinite(id)) continue
    rowById.set(id, tr)
    const device = aps.find(d => d.id === id)
    if (device) savedStateById.set(id, snapshotFromDevice(device))
  }

  function setCount(visible, total) {
    if (!countEl) return
    if (visible === total) {
      countEl.textContent = `Showing ${total} AP${total === 1 ? '' : 's'}`
    } else {
      countEl.textContent = `Showing ${visible} of ${total} (filtered)`
    }
  }

  let currentFilter = ''
  function applyFilter(filterText) {
    const f = String(filterText || '').trim().toLowerCase()
    currentFilter = f
    let visible = 0
    for (const tr of rowList) {
      const blob = tr.dataset.search || ''
      const match = !f || blob.includes(f)
      tr.style.display = match ? '' : 'none'
      if (match) visible++
    }
    setCount(visible, rowList.length)
  }

  function applyPresetIfLockedForRow(tr) {
    const id = parseInt(tr.dataset.id)
    if (!Number.isFinite(id)) return

    const modelSel = document.getElementById(`ant-model-${id}`)
    const overrideEl = document.getElementById(`ant-ovr-${id}`)
    const edtEl = document.getElementById(`ant-edt-${id}`)
    const bwhEl = document.getElementById(`ant-bwh-${id}`)
    const bwvEl = document.getElementById(`ant-bwv-${id}`)
    if (!modelSel || !overrideEl || !edtEl || !bwhEl || !bwvEl) return

    const model = modelSel.value || ''
    const preset = ANTENNA_MODEL_MAP.get(model)
    const hasDefaults = preset && preset.beamwidth_h_deg !== null && preset.beamwidth_v_deg !== null

    if (!hasDefaults && !overrideEl.checked) {
      overrideEl.checked = true
    }

    const override = overrideEl.checked
    const presetEDT = (preset && typeof preset.electrical_downtilt_deg === 'number') ? preset.electrical_downtilt_deg : 0

    if (!override) {
      edtEl.value = String(presetEDT)
      edtEl.disabled = true
      if (hasDefaults) {
        bwhEl.value = String(preset.beamwidth_h_deg)
        bwvEl.value = String(preset.beamwidth_v_deg)
      } else {
        bwhEl.value = ''
        bwvEl.value = ''
      }
      bwhEl.disabled = true
      bwvEl.disabled = true
      return
    }

    edtEl.disabled = false
    bwhEl.disabled = false
    bwvEl.disabled = false

    if (String(edtEl.value || '').trim() === '') edtEl.value = String(presetEDT)
    if (hasDefaults) {
      if (String(bwhEl.value || '').trim() === '') bwhEl.value = String(preset.beamwidth_h_deg)
      if (String(bwvEl.value || '').trim() === '') bwvEl.value = String(preset.beamwidth_v_deg)
    }
  }

  tbody.querySelectorAll('tr').forEach(tr => {
    applyPresetIfLockedForRow(tr)
    updateDirtyState(tr)
  })

  tbody.addEventListener('input', (e) => {
    const tr = e.target.closest('tr')
    if (!tr) return
    updateDirtyState(tr)
  })

  tbody.addEventListener('change', (e) => {
    const tr = e.target.closest('tr')
    if (!tr) return
    if (e.target.classList.contains('ant-model') || e.target.classList.contains('ant-ovr')) {
      applyPresetIfLockedForRow(tr)
    }
    updateDirtyState(tr)
  })

  tbody.addEventListener('focusout', (e) => {
    const tr = e.target.closest('tr')
    if (!tr) return
    const next = e.relatedTarget
    if (next && tr.contains(next)) return
    const id = parseInt(tr.dataset.id)
    if (!Number.isFinite(id)) return
    setTimeout(() => {
      if (modal.classList.contains('hidden')) return
      const active = document.activeElement
      if (active && tr.contains(active)) return
      saveRowById(id, { showErrorToast: true })
    }, 0)
  })

  docPointerHandler = (e) => {
    if (modal.classList.contains('hidden')) return
    for (const [id, tr] of rowById.entries()) {
      if (tr.dataset.dirty !== '1') continue
      if (tr.contains(e.target)) continue
      saveRowById(id, { showErrorToast: true })
    }
  }
  document.addEventListener('pointerdown', docPointerHandler, true)

  function updateRowFromDevice(tr, d, { force = false } = {}) {
    if (!tr || !d) return
    const id = d.id

    tr.dataset.search = `${(d.hostname || '')} ${(d.ip_address || '')} ${(d.mac || '')} ${(d.model || '')} ${(d.product || '')} ${(d.site_name || '')}`.toLowerCase()
    tr.dataset.sort = sortKeyForDevice(d)

    const tds = tr.children
    if (tds && tds.length >= 3) {
      const name = d.hostname || `Device ${id}`
      const nameNode = tds[0].querySelector('.antenna-cell-text') || tds[0]
      if (nameNode.textContent !== name) nameNode.textContent = name
      const ip = d.ip_address || ''
      const ipNode = tds[1].querySelector('.antenna-cell-text') || tds[1]
      if (ipNode.textContent !== ip) ipNode.textContent = ip
      const mac = String(d.mac || '').toLowerCase()
      const macNode = tds[2].querySelector('.antenna-cell-text') || tds[2]
      if (macNode.textContent !== mac) macNode.textContent = mac
    }

    if (!force && tr.dataset.dirty === '1') return

    const guessedModelId = guessAntennaModelId(d)
    const modelId = (d.antenna_model || guessedModelId || '')
    const preset = ANTENNA_MODEL_MAP.get(modelId)
    const hasDefaults = preset && preset.beamwidth_h_deg !== null && preset.beamwidth_v_deg !== null
    const overrideChecked = d.antenna_override === true || !hasDefaults

    const modelSel = document.getElementById(`ant-model-${id}`)
    const overrideEl = document.getElementById(`ant-ovr-${id}`)
    const needPresetUpdate = (modelSel && modelSel.value !== modelId) || (overrideEl && overrideEl.checked !== overrideChecked)

    if (modelSel && modelSel.value !== modelId) modelSel.value = modelId
    if (overrideEl && overrideEl.checked !== overrideChecked) overrideEl.checked = overrideChecked

    const azEl = document.getElementById(`ant-az-${id}`)
    const dtEl = document.getElementById(`ant-dt-${id}`)
    const radiusEl = document.getElementById(`ant-rad-${id}`)
    const techEl = document.getElementById(`ant-tech-${id}`)
    const downEl = document.getElementById(`ant-down-${id}`)
    const upEl = document.getElementById(`ant-up-${id}`)
    const latEl = document.getElementById(`ant-lat-${id}`)
    const bizEl = document.getElementById(`ant-biz-${id}`)

    const toStr = (v) => (v === null || v === undefined) ? '' : String(v)
    if (azEl && azEl.value !== toStr(d.antenna_azimuth_deg ?? '')) azEl.value = toStr(d.antenna_azimuth_deg ?? '')
    if (dtEl && dtEl.value !== toStr(d.antenna_downtilt_deg ?? '')) dtEl.value = toStr(d.antenna_downtilt_deg ?? '')
    if (radiusEl && radiusEl.value !== toStr(d.radius_m ?? '')) radiusEl.value = toStr(d.radius_m ?? '')
    if (techEl && techEl.value !== toStr(d.tech ?? '')) techEl.value = toStr(d.tech ?? '')
    if (downEl && downEl.value !== toStr(d.down_mbps ?? '')) downEl.value = toStr(d.down_mbps ?? '')
    if (upEl && upEl.value !== toStr(d.up_mbps ?? '')) upEl.value = toStr(d.up_mbps ?? '')
    if (latEl && latEl.value !== toStr(d.latency_ms ?? '')) latEl.value = toStr(d.latency_ms ?? '')
    if (bizEl) {
      const bizres = (String(d.bizres ?? 'X').trim().toUpperCase() || 'X')
      if (bizEl.value !== bizres) bizEl.value = bizres
    }

    const presetEDT = (preset && typeof preset.electrical_downtilt_deg === 'number') ? preset.electrical_downtilt_deg : 0
    const edtEl = document.getElementById(`ant-edt-${id}`)
    const bwhEl = document.getElementById(`ant-bwh-${id}`)
    const bwvEl = document.getElementById(`ant-bwv-${id}`)

    if (edtEl) {
      const edt = overrideChecked ? (d.antenna_electrical_downtilt_deg ?? presetEDT) : presetEDT
      if (edtEl.value !== toStr(edt)) edtEl.value = toStr(edt)
    }

    if (bwhEl && bwvEl) {
      let bwH = overrideChecked ? (d.antenna_beamwidth_h_deg ?? (hasDefaults ? preset.beamwidth_h_deg : '')) : (hasDefaults ? preset.beamwidth_h_deg : '')
      let bwV = overrideChecked ? (d.antenna_beamwidth_v_deg ?? (hasDefaults ? preset.beamwidth_v_deg : '')) : (hasDefaults ? preset.beamwidth_v_deg : '')
      if (bwhEl.value !== toStr(bwH ?? '')) bwhEl.value = toStr(bwH ?? '')
      if (bwvEl.value !== toStr(bwV ?? '')) bwvEl.value = toStr(bwV ?? '')
    }

    if (needPresetUpdate) applyPresetIfLockedForRow(tr)

    savedStateById.set(id, snapshotFromDevice(d))
    updateDirtyState(tr)
  }

  function insertRowSorted(newTr) {
    const newKey = newTr.dataset.sort || ''
    let inserted = false
    for (const child of Array.from(tbody.children)) {
      const k = child.dataset.sort || ''
      if (k > newKey) {
        tbody.insertBefore(newTr, child)
        inserted = true
        break
      }
    }
    if (!inserted) tbody.appendChild(newTr)
  }

  function updateFromStore() {
    if (modal.classList.contains('hidden')) return

    const scrollTop = tableWrap ? tableWrap.scrollTop : 0
    const atBottom = tableWrap ? (tableWrap.scrollTop + tableWrap.clientHeight >= tableWrap.scrollHeight - 2) : false

    const apsNow = (store.devices || []).filter(d => !d.parent_id)
    apsNow.sort((a, b) => {
      const sa = String(a.site_name || '').toLowerCase()
      const sb = String(b.site_name || '').toLowerCase()
      if (sa < sb) return -1
      if (sa > sb) return 1
      const na = String(a.hostname || '').toLowerCase()
      const nb = String(b.hostname || '').toLowerCase()
      if (na < nb) return -1
      if (na > nb) return 1
      return String(a.ip_address || '').localeCompare(String(b.ip_address || ''))
    })

    const byIdNow = new Map()
    for (const d of apsNow) {
      if (d && d.id !== undefined && d.id !== null) byIdNow.set(d.id, d)
    }

    for (const [id, tr] of Array.from(rowById.entries())) {
      if (!byIdNow.has(id)) {
        tr.remove()
        rowById.delete(id)
        savedStateById.delete(id)
      }
    }

    for (const d of apsNow) {
      const id = d.id
      if (!Number.isFinite(id)) continue
      if (rowById.has(id)) continue

      const tmp = document.createElement('tbody')
      tmp.innerHTML = buildRow(d).trim()
      const newTr = tmp.firstElementChild
      if (!newTr) continue
      rowById.set(id, newTr)
      savedStateById.set(id, snapshotFromDevice(d))
      insertRowSorted(newTr)
      applyPresetIfLockedForRow(newTr)
      updateDirtyState(newTr)
    }

    for (const [id, tr] of rowById.entries()) {
      const d = byIdNow.get(id)
      if (!d) continue
      updateRowFromDevice(tr, d)
    }

    rowList = Array.from(tbody.querySelectorAll('tr'))
    applyFilter(currentFilter)

    if (tableWrap) {
      if (atBottom) tableWrap.scrollTop = tableWrap.scrollHeight
      else tableWrap.scrollTop = scrollTop
    }
  }

  const filterInput = document.getElementById('antennaFilterInput')
  if (filterInput) {
    filterInput.addEventListener('input', (e) => {
      const v = e.target.value
      if (filterTimer) clearTimeout(filterTimer)
      filterTimer = setTimeout(() => applyFilter(v), 60)
    })
  }

  function scheduleUpdate() {
    if (updateTimer) return
    updateTimer = setTimeout(() => {
      updateTimer = null
      updateFromStore()
    }, 250)
  }

  applyFilter('')

  unsubscribe = store.subscribe((st, oldSt) => {
    if (modal.classList.contains('hidden')) return
    if (st.devicesVersion === oldSt.devicesVersion) return
    scheduleUpdate()
  })
  modal.classList.remove('hidden')
}


function csvEscape(v) {
  if (v === null || v === undefined) return ''
  const s = String(v)
  if (s === '') return ''
  if (/[",\n\r]/.test(s)) {
    return '"' + s.replace(/"/g, '""') + '"'
  }
  return s
}

async function showExportSectorsModal() {
  const modal = document.getElementById('exportSectorsModal')
  const textarea = document.getElementById('exportSectorsTextarea')
  const preview = document.getElementById('exportSectorsPreview')
  const tabPreview = document.getElementById('exportTabPreview')
  const tabRaw = document.getElementById('exportTabRaw')
  const closeBtn = document.getElementById('closeExportSectors')
  const closeFooter = document.getElementById('closeExportSectorsFooter')
  const downloadBtn = document.getElementById('downloadExportSectors')
  const copyBtn = document.getElementById('copyExportSectors')

  if (!modal || !textarea) {
    showToast('Export modal not found in DOM', 'error')
    return
  }

  const setView = (view) => {
    const isPreview = view === 'preview'
    if (tabPreview) tabPreview.classList.toggle('active', isPreview)
    if (tabRaw) tabRaw.classList.toggle('active', !isPreview)
    if (preview) preview.classList.toggle('hidden', !isPreview)
    if (textarea) textarea.classList.toggle('hidden', isPreview)
  }

  const close = () => modal.classList.add('hidden')
  if (closeBtn) closeBtn.onclick = close
  if (closeFooter) closeFooter.onclick = close

  // Default: show the nice preview.
  setView('preview')
  if (tabPreview) tabPreview.onclick = () => setView('preview')
  if (tabRaw) tabRaw.onclick = () => setView('raw')

  modal.classList.remove('hidden')
  if (preview) preview.innerHTML = '<div class="text-muted" style="padding:0.75rem;">Generating CSV...</div>'
  textarea.value = 'Generating CSV...'

  let sites = []
  try {
    sites = await api.sites()
  } catch (e) {
    console.warn('Failed to load sites for export:', e)
    sites = []
  }
  const siteById = new Map((sites || []).map(s => [s.id, s]))

  // Treat "AP" as "no parent" to match the rest of the UI.
  const aps = (store.devices || []).filter(d => !d.parent_id)

  const header = ['site_id','sector_id','site_lat','site_lon','azimuth','beam','radius_m','tech','down','up','latency','tower_h_m','bizres']
  const rows = []

  // Stable, readable ordering: site name, then SSID/hostname, then IP
  aps.sort((a,b) => {
    const sa = (siteById.get(a.site_id)?.name || a.site_name || '').toLowerCase()
    const sb = (siteById.get(b.site_id)?.name || b.site_name || '').toLowerCase()
    if (sa < sb) return -1
    if (sa > sb) return 1
    const ea = (a.ssid || a.hostname || '').toLowerCase()
    const eb = (b.ssid || b.hostname || '').toLowerCase()
    if (ea < eb) return -1
    if (ea > eb) return 1
    const ia = (a.ip_address || '')
    const ib = (b.ip_address || '')
    return ia.localeCompare(ib)
  })

  for (const ap of aps) {
    const site = siteById.get(ap.site_id) || null

    const siteId = (site?.name || ap.site_name || '')
    const sectorId = (ap.ssid || ap.hostname || ap.mac || '')

    // Prefer site coordinates (only if both lat/lon are present); otherwise fall back to device GPS (less accurate).
    let lat = ''
    let lon = ''
    if (site && site.gps_lat !== null && site.gps_lat !== undefined && site.gps_lon !== null && site.gps_lon !== undefined) {
      lat = site.gps_lat
      lon = site.gps_lon
    } else if (ap.gps_lat !== null && ap.gps_lat !== undefined && ap.gps_lon !== null && ap.gps_lon !== undefined) {
      lat = ap.gps_lat
      lon = ap.gps_lon
    }

    const towerHM = (site?.tower_h_m ?? '')

    // Rule: don't export APs that have no azimuth set. "0" (and any numeric) is valid; blank is not.
    const azRaw = ap.antenna_azimuth_deg
    const azPopulated = azRaw !== null && azRaw !== undefined && String(azRaw).trim() !== ''
    if (!azPopulated) {
      continue
    }
    const az = azRaw

    // Effective horizontal beamwidth comes from the selected preset unless override is enabled.
    let beamVal = ap.antenna_beamwidth_h_deg
    const preset = ANTENNA_MODELS.find(m => m.id === ap.antenna_model)
    if (!ap.antenna_override) {
      beamVal = preset?.beamwidth_h_deg ?? beamVal
    } else {
      // If override is enabled but no explicit value was set, fall back to the preset.
      if (beamVal === null || beamVal === undefined || beamVal === '') {
        beamVal = preset?.beamwidth_h_deg ?? beamVal
      }
    }
    const beam = (beamVal ?? '')
    const radius = (ap.radius_m ?? '')

    let tech = (ap.tech ?? '')
    if (tech === '' || tech === null || tech === undefined) {
      const derived = deriveTechFromFrequencyMHz(ap.frequency)
      if (derived !== null) tech = derived
    }

    const down = (ap.down_mbps ?? '')
    const up = (ap.up_mbps ?? '')
    const latency = (ap.latency_ms ?? '')
    const bizres = (String(ap.bizres ?? 'X').trim().toUpperCase() || 'X')

    rows.push([siteId, sectorId, lat, lon, az, beam, radius, tech, down, up, latency, towerHM, bizres])
  }

  const lines = [header.join(',')]
  for (const row of rows) {
    lines.push(row.map(csvEscape).join(','))
  }
  const csv = lines.join('\n')
  textarea.value = csv

  // Render "nice" preview table
  if (preview) {
    const maxRows = 2000
    const displayRows = rows.slice(0, maxRows)
    const truncated = rows.length > maxRows
    preview.innerHTML = `
      <div class="export-preview-wrap">
        <table class="export-preview-table">
          <thead>
            <tr>${header.map(h => `<th>${escapeHTML(h)}</th>`).join('')}</tr>
          </thead>
          <tbody>
            ${displayRows.map(r => `<tr>${r.map(v => `<td>${escapeHTML(v === null || v === undefined ? '' : v)}</td>`).join('')}</tr>`).join('')}
          </tbody>
        </table>
        ${truncated ? `<div class="text-muted" style="padding:0.5rem 0.75rem;">Preview limited to ${maxRows} rows (of ${rows.length}). Use Raw CSV for full output.</div>` : ''}
      </div>
    `
  }

  if (downloadBtn) {
    downloadBtn.onclick = () => {
      downloadFile(csv, 'wavecontrol-sectors.csv', 'text/csv')
    }
  }

  if (copyBtn) {
    copyBtn.onclick = async () => {
      try {
        if (navigator.clipboard?.writeText) {
          await navigator.clipboard.writeText(csv)
          showToast('Copied CSV to clipboard', 'success')
        } else {
          // Fallback
          setView('raw')
          textarea.focus()
          textarea.select()
          document.execCommand('copy')
          showToast('Copied CSV to clipboard', 'success')
        }
      } catch (e) {
        showToast('Copy failed: ' + e.message, 'error')
      }
    }
  }
}


async function loadBackupsList() {
  const container = document.getElementById('backupsList')
  if (!container) return
  
  container.innerHTML = '<p>Loading...</p>'
  
  try {
    // Fetch all backups in one call
    const backups = await api.listAllConfigs()
    
    if (!backups || backups.length === 0) {
      container.innerHTML = '<p class="text-muted">No backups found.</p>'
      return
    }
    
    // Already sorted by server, but ensure date order
    backups.sort((a, b) => new Date(b.created_at) - new Date(a.created_at))
    
    container.innerHTML = `
      <div class="backups-search">
        <input type="text" id="backupSearch" placeholder="Search backups..." class="search-input" />
      </div>
      <table class="backups-table">
        <thead>
          <tr>
            <th>Device</th>
            <th>Date</th>
            <th>Size</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody id="backupsBody">
          ${backups.map(b => `
            <tr data-search="${escapeAttr((b.device?.hostname || '') + ' ' + (b.device?.ip || ''))}">
              <td>${escapeHTML(b.device?.hostname || b.device?.ip || 'Unknown')}</td>
              <td>${new Date(b.created_at).toLocaleString()}</td>
              <td>${formatSize(b.size || 0)}</td>
              <td>
                <button class="btn btn-sm" data-wc-action="download-config" data-path="${escapeAttr(b.path)}">Download</button>
                ${b.device?.id ? `<button class="btn btn-sm" data-wc-action="restore-config" data-device-id="${b.device.id}" data-path="${escapeAttr(b.path)}">Restore</button>` : ''}
              </td>
            </tr>
          `).join('')}
        </tbody>
      </table>
    `
    
    // Search handler
    document.getElementById('backupSearch')?.addEventListener('input', (e) => {
      const query = e.target.value.toLowerCase()
      document.querySelectorAll('#backupsBody tr').forEach(row => {
        const searchText = row.dataset.search?.toLowerCase() || ''
        row.style.display = searchText.includes(query) ? '' : 'none'
      })
    })
  } catch (e) {
    container.innerHTML = `<p class="text-muted">Error loading backups: ${escapeHTML(e.message)}</p>`
  }
}

// Download config helper
window.downloadConfig = function(path) {
  const url = `/api/wavecontrol/configs/download?path=${encodeURIComponent(path)}`
  fetch(url, { credentials: 'same-origin', cache: 'no-store' })
  .then(resp => {
    if (!resp.ok) throw new Error('Download failed')
    return resp.blob()
  })
  .then(blob => {
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = path.split('/').pop() || 'config.cfg'
    a.click()
    URL.revokeObjectURL(a.href)
  })
  .catch(e => showToast('Download failed: ' + e.message, 'error'))
}

// Backup device config
window.backupDeviceConfig = async function(deviceId) {
  try {
    showToast('Backing up config...', 'info')
    const result = await api.backupConfig(deviceId)
    showToast(`Backup saved: ${result.path}`, 'success')
    // Refresh backups list if visible
    window.showDeviceBackups(deviceId)
  } catch (e) {
    showToast('Backup failed: ' + e.message, 'error')
  }
}

// Show device backups in detail panel
window.showDeviceBackups = async function(deviceId) {
  const container = document.getElementById(`deviceBackupsList-${deviceId}`)
  if (!container) return
  
  container.innerHTML = '<p class="text-muted">Loading...</p>'
  
  try {
    const configs = await api.listConfigs(deviceId)
    if (!configs || configs.length === 0) {
      container.innerHTML = '<p class="text-muted">No backups yet.</p>'
      return
    }
    
    container.innerHTML = `
      <div class="device-backups">
        ${configs.slice(0, 5).map(c => `
          <div class="backup-item">
            <span class="backup-name">${escapeHTML(c.name)}</span>
            <span class="backup-date">${new Date(c.created_at).toLocaleDateString()}</span>
            <span class="backup-size">${formatSize(c.size || 0)}</span>
            <div class="backup-actions">
              <button class="btn btn-xs" data-wc-action="download-config" data-path="${escapeAttr(c.path)}">↓</button>
              <button class="btn btn-xs" data-wc-action="restore-config" data-device-id="${deviceId}" data-path="${escapeAttr(c.path)}">Restore</button>
            </div>
          </div>
        `).join('')}
        ${configs.length > 5 ? `<p class="text-muted">${configs.length - 5} more backups...</p>` : ''}
      </div>
    `
  } catch (e) {
    container.innerHTML = `<p class="text-muted">Error: ${escapeHTML(e.message)}</p>`
  }
}

// Restore config helper
window.restoreConfig = async function(deviceId, path) {
  if (!confirm('Restore this configuration? The device will reboot.')) return
  
  try {
    showToast('Restoring config...', 'info')
    await api.restoreConfig(deviceId, path)
    showToast('Config restored, device rebooting', 'success')
  } catch (e) {
    showToast('Restore failed: ' + e.message, 'error')
  }
}

function showBatchConfigModal() {
  const modal = document.getElementById('batchConfigModal')
  if (!modal) return
  
  // Populate device list
  const select = modal.querySelector('#batchDevices')
  if (select) {
    select.innerHTML = ''
    store.devices.forEach(d => {
      const opt = document.createElement('option')
      opt.value = d.id
      opt.textContent = `${d.hostname || d.ip_address} (${d.product || 'Unknown'})`
      select.appendChild(opt)
    })
  }
  
  modal.classList.remove('hidden')
}

// Batch config submit
document.getElementById('confirmBatchConfig')?.addEventListener('click', async () => {
  const modal = document.getElementById('batchConfigModal')
  const select = modal?.querySelector('#batchDevices')
  const deviceIds = Array.from(select?.selectedOptions || []).map(o => parseInt(o.value))
  
  if (deviceIds.length === 0) {
    showToast('Select at least one device', 'error')
    return
  }
  
  const changes = {}
  if (document.getElementById('cfgSSID')?.checked) {
    changes.ssid = document.getElementById('cfgSSIDValue')?.value
  }
  if (document.getElementById('cfgChannel')?.checked) {
    changes.channel = parseInt(document.getElementById('cfgChannelValue')?.value)
  }
  if (document.getElementById('cfgPower')?.checked) {
    changes.tx_power = parseInt(document.getElementById('cfgPowerValue')?.value)
  }
  if (document.getElementById('cfgPassword')?.checked) {
    changes.password = document.getElementById('cfgPasswordValue')?.value
  }
  
  if (Object.keys(changes).length === 0) {
    showToast('Select at least one configuration option', 'error')
    return
  }
  
  showToast(`Applying config to ${deviceIds.length} devices...`, 'info')
  
  try {
    const result = await api.batchConfig(deviceIds, changes)
    const success = result.results?.filter(r => r.status === 'success').length || 0
    showToast(`Config applied: ${success}/${deviceIds.length} success`, success > 0 ? 'success' : 'error')
    modal?.classList.add('hidden')
  } catch (e) {
    showToast('Config failed: ' + e.message, 'error')
  }
})

// ===== REPORTS PAGE =====
// Drilldown page state
let drilldownSort = { col: null, dir: null }
let drilldownFilter = ''

function renderDrilldownPage(container) {
  // Enable full-screen drilldown mode
  document.body.classList.add('drilldown-active')
  
  // Reset local filter when entering page
  drilldownFilter = ''
  
  container.innerHTML = `
    <div class="page page-drilldown">
      <div class="drilldown-header">
        <div class="drilldown-controls">
          <select id="drilldownListSelect" class="form-select">
            <option value="default">APs</option>
          </select>
          <button class="btn btn-sm" id="btnManageDrilldownLists">Manage Lists</button>
        </div>
        <div class="drilldown-search">
          <input type="text" id="drilldownSearch" placeholder="Filter..." value="${escapeAttr(drilldownFilter)}" />
        </div>
      </div>
      <div class="drilldown-table-wrap">
        <table class="drilldown-table" id="drilldownTable">
          <thead>
            <tr>
              <th data-col="host" class="sortable ${drilldownSort.col === 'host' ? 'sorted ' + drilldownSort.dir : ''}">Host</th>
              <th data-col="radio_name" class="sortable ${drilldownSort.col === 'radio_name' ? 'sorted ' + drilldownSort.dir : ''}">Radio Name</th>
              <th data-col="radio_type" class="sortable ${drilldownSort.col === 'radio_type' ? 'sorted ' + drilldownSort.dir : ''}">Radio Type</th>
              <th data-col="ssid" class="sortable ${drilldownSort.col === 'ssid' ? 'sorted ' + drilldownSort.dir : ''}">SSID</th>
              <th data-col="user_count" class="sortable ${drilldownSort.col === 'user_count' ? 'sorted ' + drilldownSort.dir : ''}">Users</th>
              <th data-col="ch_width" class="sortable ${drilldownSort.col === 'ch_width' ? 'sorted ' + drilldownSort.dir : ''}">Width</th>
              <th data-col="mode" class="sortable ${drilldownSort.col === 'mode' ? 'sorted ' + drilldownSort.dir : ''}">Mode</th>
              <th data-col="frame" class="sortable ${drilldownSort.col === 'frame' ? 'sorted ' + drilldownSort.dir : ''}">Frame</th>
              <th data-col="sync" class="sortable ${drilldownSort.col === 'sync' ? 'sorted ' + drilldownSort.dir : ''}">Sync</th>
              <th data-col="features" class="sortable ${drilldownSort.col === 'features' ? 'sorted ' + drilldownSort.dir : ''}">Features</th>
              <th data-col="freq" class="sortable ${drilldownSort.col === 'freq' ? 'sorted ' + drilldownSort.dir : ''}">Freq</th>
              <th data-col="freq2" class="sortable ${drilldownSort.col === 'freq2' ? 'sorted ' + drilldownSort.dir : ''}">Radio2</th>
              <th data-col="firmware" class="sortable ${drilldownSort.col === 'firmware' ? 'sorted ' + drilldownSort.dir : ''}">Firmware</th>
            </tr>
          </thead>
          <tbody id="drilldownBody"></tbody>
        </table>
      </div>
      <div class="drilldown-footer" id="drilldownFooter"></div>
    </div>
  `
  
  // Wire up local filter - independent from main search
  const localSearchInput = document.getElementById('drilldownSearch')
  localSearchInput?.addEventListener('input', (e) => {
    drilldownFilter = e.target.value
    renderDrilldownBody()
  })
  
  // Wire up sorting (click cycles: asc -> desc -> none)
  document.querySelectorAll('#drilldownTable th.sortable').forEach(th => {
    th.addEventListener('click', () => {
      const col = th.dataset.col
      if (drilldownSort.col === col) {
        if (drilldownSort.dir === 'asc') {
          drilldownSort.dir = 'desc'
        } else if (drilldownSort.dir === 'desc') {
          // Clear sort completely
          drilldownSort.col = null
          drilldownSort.dir = null
        } else {
          // Was null, start fresh
          drilldownSort.dir = 'asc'
        }
      } else {
        drilldownSort.col = col
        drilldownSort.dir = 'asc'
      }
      // Update header classes
      document.querySelectorAll('#drilldownTable th.sortable').forEach(h => {
        h.classList.remove('sorted', 'asc', 'desc')
        if (drilldownSort.col && h.dataset.col === drilldownSort.col) {
          h.classList.add('sorted', drilldownSort.dir)
        }
      })
      renderDrilldownBody()
    })
  })
  
  // Load custom drilldown lists
  loadDrilldownLists()
  
  // List selector change
  document.getElementById('drilldownListSelect')?.addEventListener('change', (e) => {
    store.selectedDrilldownList = e.target.value
    renderDrilldownBody()
  })
  
  // Manage lists button
  document.getElementById('btnManageDrilldownLists')?.addEventListener('click', showDrilldownListsModal)
  
  renderDrilldownBody()
}

async function loadDrilldownLists() {
  try {
    const lists = await api.listDrilldownLists()
    store.drilldownLists = lists || []
    
    // Update dropdown
    const select = document.getElementById('drilldownListSelect')
    if (select) {
      // Keep default option, add custom lists
      const currentValue = select.value
      select.innerHTML = '<option value="default">APs</option>'
      lists.forEach(list => {
        const opt = document.createElement('option')
        opt.value = list.id
        opt.textContent = `${list.name} (${list.host_count} hosts)`
        select.appendChild(opt)
      })
      // Restore selection
      if (currentValue && select.querySelector(`option[value="${currentValue}"]`)) {
        select.value = currentValue
      }
    }
  } catch (e) {
    console.error('Failed to load drilldown lists:', e)
  }
}

function showDrilldownListsModal() {
  let modal = document.getElementById('drilldownListsModal')
  if (!modal) {
    modal = document.createElement('div')
    modal.id = 'drilldownListsModal'
    modal.className = 'modal hidden'
    document.body.appendChild(modal)
    
    // Close on backdrop click
    modal.addEventListener('click', (e) => {
      if (e.target === modal) {
        modal.classList.add('hidden')
      }
    })
  }
  
  modal.innerHTML = `
    <div class="modal-content modal-lg">
      <div class="modal-header">
        <h4>Manage Drilldown Lists</h4>
        <button class="modal-close" data-wc-action="close-modal" data-modal-id="drilldownListsModal">&times;</button>
      </div>
      <div class="modal-body">
        <div class="drilldown-lists-section">
          <h5>Lists</h5>
          <div id="drilldownListsContainer">Loading...</div>
          <div class="form-row" style="margin-top: 1rem; gap: 0.5rem;">
            <input type="text" id="newListName" placeholder="New list name..." class="form-input" style="flex: 2;" />
            <input type="number" id="newListInterval" placeholder="Interval (s)" value="30" min="30" class="form-input" style="width: 100px;" />
            <button class="btn btn-primary" data-wc-action="create-drilldown-list">Create</button>
          </div>
        </div>
        <div id="drilldownHostsSection" style="display: none; margin-top: 1.5rem;">
          <h5>Hosts in <span id="currentListName"></span></h5>
          <div id="drilldownHostsContainer"></div>
          <div class="form-row" style="margin-top: 1rem;">
            <input type="text" id="newHostIP" placeholder="Host IP address..." class="form-input" />
            <button class="btn btn-primary" data-wc-action="add-drilldown-host">Add Host</button>
          </div>
        </div>
      </div>
    </div>
  `
  modal.classList.remove('hidden')
  renderDrilldownListsInModal()
}

window.closeDrilldownListsModal = function() {
  const modal = document.getElementById('drilldownListsModal')
  if (modal) modal.classList.add('hidden')
}

async function renderDrilldownListsInModal() {
  const container = document.getElementById('drilldownListsContainer')
  if (!container) return
  
  try {
    const lists = await api.listDrilldownLists()
    store.drilldownLists = lists || []
    
    // Count APs
    const apCount = store.devices.filter(d => !d.parent_id).length
    
    container.innerHTML = `
      <table class="table-compact">
        <thead>
          <tr><th>Name</th><th>Hosts</th><th>Interval</th><th>Enabled</th><th>Actions</th></tr>
        </thead>
        <tbody>
          <tr class="row-default">
            <td><strong>APs</strong></td>
            <td>${apCount}</td>
            <td>30s</td>
            <td><input type="checkbox" checked disabled /></td>
            <td><span class="text-muted">Default</span></td>
          </tr>
          ${lists.map(l => `
            <tr>
              <td>${escapeHTML(l.name)}</td>
              <td>${l.host_count}</td>
              <td>${l.poll_interval}s</td>
              <td>
                <input type="checkbox" ${l.enabled ? 'checked' : ''}
                       data-wc-change="toggle-drilldown-list" data-list-id="${l.id}" />
              </td>
              <td>
                <button class="btn btn-xs" data-wc-action="edit-drilldown-list" data-list-id="${l.id}" data-list-name="${escapeAttr(l.name)}">Edit</button>
                <button class="btn btn-xs btn-danger" data-wc-action="delete-drilldown-list" data-list-id="${l.id}">Delete</button>
              </td>
            </tr>
          `).join('')}
        </tbody>
      </table>
    `
  } catch (e) {
    container.innerHTML = `<p class="text-error">Error: ${escapeHTML(e.message)}</p>`
  }
}

window.toggleDrilldownList = async function(id, enabled) {
  try {
    await api.updateDrilldownList(id, { enabled })
    loadDrilldownLists()
  } catch (e) {
    showToast('Failed to update list: ' + e.message, 'error')
    renderDrilldownListsInModal()
  }
}

window.createDrilldownList = async function() {
  const nameInput = document.getElementById('newListName')
  const intervalInput = document.getElementById('newListInterval')
  const name = nameInput?.value?.trim()
  let interval = parseInt(intervalInput?.value) || 30
  
  if (!name) {
    showToast('Enter a list name', 'error')
    return
  }
  
  // Enforce minimum 30 second interval
  if (interval < 30) interval = 30
  
  try {
    await api.createDrilldownList(name, '', interval)
    nameInput.value = ''
    intervalInput.value = '30'
    showToast('List created', 'success')
    renderDrilldownListsInModal()
    loadDrilldownLists()
  } catch (e) {
    showToast('Failed to create list: ' + e.message, 'error')
  }
}

window.deleteDrilldownList = async function(id) {
  if (!confirm('Delete this drilldown list?')) return
  
  try {
    await api.deleteDrilldownList(id)
    showToast('List deleted', 'success')
    renderDrilldownListsInModal()
    loadDrilldownLists()
  } catch (e) {
    showToast('Failed to delete list: ' + e.message, 'error')
  }
}

window.editDrilldownList = async function(listId, listName) {
  store.editingDrilldownListId = listId
  
  const section = document.getElementById('drilldownHostsSection')
  const nameSpan = document.getElementById('currentListName')
  const container = document.getElementById('drilldownHostsContainer')
  
  if (section) section.style.display = 'block'
  if (nameSpan) nameSpan.textContent = listName
  if (container) container.innerHTML = 'Loading...'
  
  try {
    const hosts = await api.getDrilldownHosts(listId)
    
    if (hosts.length === 0) {
      container.innerHTML = '<p class="text-muted">No hosts in this list.</p>'
      return
    }
    
    container.innerHTML = `
      <table class="table-compact">
        <thead>
          <tr><th>Host</th><th>Device</th><th>Last Poll</th><th>Actions</th></tr>
        </thead>
        <tbody>
          ${hosts.map(h => `
            <tr>
              <td>${escapeHTML(h.host)}</td>
              <td>${escapeHTML(h.hostname || h.model || '-')}</td>
              <td>${h.last_poll ? new Date(h.last_poll).toLocaleString() : '-'}</td>
              <td>
                <button class="btn btn-xs btn-danger" data-wc-action="remove-drilldown-host" data-list-id="${listId}" data-host-id="${h.id}">Remove</button>
              </td>
            </tr>
          `).join('')}
        </tbody>
      </table>
    `
  } catch (e) {
    container.innerHTML = `<p class="text-error">Error: ${escapeHTML(e.message)}</p>`
  }
}

window.addDrilldownHost = async function() {
  const listId = store.editingDrilldownListId
  const hostInput = document.getElementById('newHostIP')
  const host = hostInput?.value?.trim()
  
  if (!listId || !host) {
    showToast('Enter a host IP address', 'error')
    return
  }
  
  try {
    await api.addDrilldownHost(listId, host)
    hostInput.value = ''
    showToast('Host added', 'success')
    // Refresh the hosts list
    const listName = document.getElementById('currentListName')?.textContent || ''
    editDrilldownList(listId, listName)
    loadDrilldownLists()
  } catch (e) {
    showToast('Failed to add host: ' + e.message, 'error')
  }
}

window.removeDrilldownHost = async function(listId, hostId) {
  try {
    await api.removeDrilldownHost(listId, hostId)
    showToast('Host removed', 'success')
    const listName = document.getElementById('currentListName')?.textContent || ''
    editDrilldownList(listId, listName)
    loadDrilldownLists()
  } catch (e) {
    showToast('Failed to remove host: ' + e.message, 'error')
  }
}

function getDrilldownData(device) {
  // Extract data from device for drilldown table
  const radio5 = device.radio_5ghz || {}
  const radio60 = device.radio_60ghz || {}
  const radioLtu = device.radio_ltu || {}
  const radio6 = device.radio_6ghz || {}
  const config = device.config || {}
  
  // Primary frequency (prefer 60GHz, then LTU, then 5GHz, then 6GHz, then DB)
  const primaryRadio = radio60.frequency
    ? radio60
    : (radioLtu.frequency
      ? radioLtu
      : (radio5.frequency
        ? radio5
        : (radio6.frequency
          ? radio6
          : null)))

  let freq = primaryRadio ? primaryRadio.frequency : (device.frequency || '')
  let freq2 = ''

  // Second radio frequency (Wave dual-radio scenarios)
  let secondaryRadio = null
  if (primaryRadio === radio60 || primaryRadio === radioLtu) {
    secondaryRadio = radio5.frequency ? radio5 : (radio6.frequency ? radio6 : null)
  } else if (primaryRadio === radio5) {
    secondaryRadio = radio6.frequency ? radio6 : null
  } else if (primaryRadio === radio6) {
    secondaryRadio = radio5.frequency ? radio5 : null
  }

  if (secondaryRadio && secondaryRadio.frequency) {
    freq2 = secondaryRadio.frequency + '/' + (secondaryRadio.channel_width || 20)
  }

  // Channel width - prefer primary radio, fall back to DB
  let chWidth = (primaryRadio ? primaryRadio.channel_width : '') || device.channel_width || ''

  // Mode from config or construct from role
  let mode = ''
  let netMode = ''
  if (config.mode) {
    mode = config.mode
    netMode = config.net_mode || ''
  } else {
    const isAP = !device.parent_id
    mode = device.role || (isAP ? 'ap' : 'sta')
    // Heuristic for net mode
    const peerCount = device.peer_count || 0
    if (isAP && peerCount <= 1 && (device.product?.includes('LR') || device.product?.includes('Long'))) {
      netMode = 'ptp'
    } else {
      netMode = 'ptmp'
    }
  }
  
  // Frame mode (fixed/flex with params)
  let frame = ''
  if (config.frame_mode) {
    frame = config.frame_mode
    if (config.frame_mode === 'fixed' && config.frame_len && config.dl_ratio) {
      frame = `${config.frame_len}/${config.dl_ratio}`
    }
  } else if (radioLtu.frame_length && radioLtu.dl_ratio) {
    frame = `${radioLtu.frame_length}/${radioLtu.dl_ratio}`
  }
  
  // GPS sync
  let sync = false
  if (config.gps_sync !== undefined) {
    sync = config.gps_sync
  } else {
    // Check live radio stats (gpsSyncState == 2 means synced)
    const gpsSyncState = radio60.gps_sync_state || radioLtu.gps_sync_state || radio5.gps_sync_state || radio6.gps_sync_state
    sync = gpsSyncState === 2
  }
  
  // Other features (waveai, auto freq, 11n)
  const features = []
  if (config.wave_ai) features.push('waveai')
  if (config.auto_freq_60) features.push('auto60')
  if (config.auto_freq_6) features.push('auto6')
  if (config.auto_freq_5) features.push('auto5')
  if (config.compat_11n) features.push('11n')
  
  // Radio type - use model directly (e.g. "Wave AP", "LTU-Rocket", "Rocket 5AC")
  const radioType = device.model || device.product || ''
  
  // SSID - prefer config.ssid (Wave), then device.ssid (DB/AirMAX)
  const ssid = config.ssid || device.ssid || ''
  
  return {
    host: device.ip_address || '',
    radio_name: device.hostname || '',
    radio_type: radioType,
    ssid: ssid,
    user_count: device.peer_count || 0,
    ch_width: chWidth,
    mode: mode + (netMode ? '-' + netMode : ''),
    frame: frame,
    sync: sync,
    features: features,
    freq: freq,
    freq2: freq2,
    firmware: device.firmware || ''
  }
}

function renderDrilldownBody() {
  const tbody = document.getElementById('drilldownBody')
  const footer = document.getElementById('drilldownFooter')
  if (!tbody) return
  
  // Get APs only by default (no parent_id)
  let rows = store.devices
    .filter(d => !d.parent_id)  // APs only
    .map(d => ({
      device: d,
      data: getDrilldownData(d)
    }))
  
  // Filter
  if (drilldownFilter) {
    const f = drilldownFilter.toLowerCase()
    rows = rows.filter(r => {
      // Search across all fields including features array and pill text
      const d = r.data
      const syncText = d.sync ? 'sync' : ''
      const searchStr = [d.host, d.radio_name, d.radio_type, d.ssid, d.mode, d.frame, 
                        d.freq, d.freq2, d.firmware, syncText, ...d.features].join(' ').toLowerCase()
      return searchStr.includes(f)
    })
  }
  
  // Sort (only if a column is selected)
  if (drilldownSort.col) {
    const col = drilldownSort.col
    const dir = drilldownSort.dir === 'asc' ? 1 : -1
    rows.sort((a, b) => {
      let va = a.data[col]
      let vb = b.data[col]
      
      // Boolean columns
      if (col === 'sync') {
        return ((va ? 1 : 0) - (vb ? 1 : 0)) * dir
      }
      
      // Array columns (features)
      if (col === 'features') {
        va = va.length
        vb = vb.length
        return (va - vb) * dir
      }
      
      // Numeric columns
      if (['user_count', 'ch_width', 'freq'].includes(col)) {
        va = parseFloat(va) || 0
        vb = parseFloat(vb) || 0
        return (va - vb) * dir
      }
      
      // String columns
      va = String(va || '').toLowerCase()
      vb = String(vb || '').toLowerCase()
    return va.localeCompare(vb) * dir
    })
  }
  
  // Helper to render a pill badge with data-text for filtering
  const pill = (text, color) => text ? `<span class="pill pill-${color}" data-text="${escapeAttr(text)}">${escapeHTML(text.toLowerCase())}</span>` : ''
  
  // Render
  tbody.innerHTML = rows.map(r => {
    const d = r.data
    
    // Mode pill (ap-ptmp, ap-ptp, sta-ptmp, sta-ptp)
    let modePill = ''
    if (d.mode) {
      const m = d.mode.toLowerCase()
      if (m === 'ap-ptmp') modePill = pill(d.mode, 'cyan')
      else if (m === 'ap-ptp') modePill = pill(d.mode, 'teal')
      else if (m === 'sta-ptmp') modePill = pill(d.mode, 'yellow')
      else if (m === 'sta-ptp') modePill = pill(d.mode, 'amber')
      else modePill = pill(d.mode, 'gray')
    }
    
    // Frame pill (fixed/flex with params) - different colors for variations
    let framePill = ''
    if (d.frame) {
      if (d.frame.includes('/')) {
        // Parse ratio to pick color shade
        const parts = d.frame.split('/')
        const ratio = parseInt(parts[1]) || 50
        let color = 'blue'
        if (ratio >= 75) color = 'indigo'
        else if (ratio >= 60) color = 'blue'
        else if (ratio <= 40) color = 'sky'
        framePill = pill('FIXED ' + d.frame, color)
      } else if (d.frame === 'fixed') {
        framePill = pill('FIXED', 'blue')
      } else if (d.frame === 'flex') {
        framePill = pill('FLEX', 'purple')
      }
    }
    
    // Sync pill
    const syncPill = d.sync ? pill('SYNC', 'green') : ''
    
    // Feature pills
    const featurePills = d.features.map(f => {
      if (f === 'waveai') return pill('WAVEAI', 'cyan')
      if (f === 'auto60') return pill('AUTO60', 'orange')
      if (f === 'auto6') return pill('AUTO6', 'orange')
      if (f === 'auto5') return pill('AUTO5', 'orange')
      if (f === '11n') return pill('11N', 'gray')
      return pill(f, 'gray')
    }).join(' ')
    
    return `
      <tr data-id="${r.device.id}">
        <td>${escapeHTML(d.host)}</td>
        <td>${escapeHTML(d.radio_name)}</td>
        <td>${escapeHTML(d.radio_type)}</td>
        <td>${escapeHTML(d.ssid)}</td>
        <td>${d.user_count}</td>
        <td>${d.ch_width}</td>
        <td>${modePill}</td>
        <td>${framePill}</td>
        <td>${syncPill}</td>
        <td>${featurePills}</td>
        <td>${d.freq}</td>
        <td>${escapeHTML(d.freq2)}</td>
        <td class="col-firmware">${escapeHTML(d.firmware)}</td>
      </tr>
    `
  }).join('')
  
  // Footer with count
  const total = store.devices.length
  footer.textContent = drilldownFilter 
    ? `Showing ${rows.length} of ${total} entries (filtered)`
    : `Showing ${rows.length} entries`
  
  // Click to select device and show detail panel
  tbody.querySelectorAll('tr').forEach(tr => {
    tr.addEventListener('click', () => {
      const id = parseInt(tr.dataset.id)
      const device = store.devices.find(d => d.id === id)
      store.set({ selectedDevice: id })
      
      // Highlight selected row
      tbody.querySelectorAll('tr').forEach(r => r.classList.remove('selected'))
      tr.classList.add('selected')
      
      // Show detail panel
      const detailPanel = document.getElementById('detailPanel')
      if (device && detailPanel) {
        renderDeviceDetail(detailPanel, device)
        detailPanel.classList.remove('hidden')
      }
    })
  })
}

function renderReportsPage(container) {
  container.innerHTML = `
    <div class="page page-reports">
      <div class="reports-header">
        <h3>Network Reports</h3>
        <button class="btn btn-primary" id="btnGenerateReport">Generate Report</button>
      </div>
      
      <div class="report-types">
        <div class="report-type" data-type="health">
          <h4>Network Health</h4>
          <p>Overall network status, device uptime, and signal quality summary.</p>
        </div>
        <div class="report-type" data-type="inventory">
          <h4>Device Inventory</h4>
          <p>Complete list of all devices with firmware versions and configuration.</p>
        </div>
        <div class="report-type" data-type="performance">
          <h4>Performance Summary</h4>
          <p>Throughput, capacity utilization, and link quality metrics.</p>
        </div>
        <div class="report-type" data-type="chain">
          <h4>Chain Imbalance</h4>
          <p>Devices and links with per-chain RSSI spread greater than 5 dB, marked as AP side, STA side, or both.</p>
        </div>
        <div class="report-type" data-type="rx_mismatch">
          <h4>RX Level Mismatch</h4>
          <p>Links where AP RX and STA RX for the same link differ by more than 8 dB.</p>
        </div>
      </div>
      
      <div class="reports-section">
        <h4>Recent Reports</h4>
        <div id="reportsList" class="reports-list">
          <p class="text-muted">No reports generated yet.</p>
        </div>
      </div>
      
      <div class="report-viewer" id="reportViewer" style="display:none">
        <div class="report-viewer-header">
          <h4 id="reportViewerTitle">Report</h4>
          <button class="btn btn-sm" id="closeReportViewer">Close</button>
        </div>
        <div class="report-content" id="reportContent"></div>
      </div>
    </div>
  `
  
  // Report type selection
  document.querySelectorAll('.report-type').forEach(el => {
    el.addEventListener('click', () => {
      document.querySelectorAll('.report-type').forEach(e => e.classList.remove('selected'))
      el.classList.add('selected')
    })
  })
  
  document.getElementById('btnGenerateReport')?.addEventListener('click', async () => {
    const selected = document.querySelector('.report-type.selected')
    const type = selected?.dataset.type || 'health'
    
    showToast('Generating report...', 'info')
    
    try {
      const result = await api.generateReport(type)
      showToast('Report generated', 'success')
      loadReportsList()
    } catch (e) {
      showToast('Failed: ' + e.message, 'error')
    }
  })
  
  document.getElementById('closeReportViewer')?.addEventListener('click', () => {
    document.getElementById('reportViewer').style.display = 'none'
  })
  
  loadReportsList()
}

async function loadReportsList() {
  const container = document.getElementById('reportsList')
  if (!container) return
  
  try {
    const reports = await api.listReports()
    
    if (!reports || reports.length === 0) {
      container.innerHTML = '<p class="text-muted">No reports generated yet.</p>'
      return
    }
    
    container.innerHTML = `
      <div class="reports-compare-bar" style="display:none">
        <span class="compare-info">Select 2 reports of the same type to compare</span>
        <button class="btn btn-sm btn-compare-reports" disabled>Compare Selected</button>
      </div>
      <table class="reports-table">
        <thead>
          <tr>
            <th><input type="checkbox" id="compareToggle" title="Enable comparison mode"></th>
            <th>Type</th>
            <th>Generated</th>
            <th>Devices</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          ${reports.map(r => `
            <tr>
              <td><input type="checkbox" class="compare-checkbox" data-id="${r.id}" data-type="${r.type}" style="display:none"></td>
              <td>${r.type}</td>
              <td>${new Date(r.created_at).toLocaleString()}</td>
              <td>${r.device_count || 0}</td>
              <td class="report-actions">
                <button class="btn btn-sm btn-view-report" data-id="${r.id}" data-type="${r.type}" title="View Report">View</button>
                <a class="btn btn-sm btn-json-dl" href="/api/wavecontrol/reports/${r.id}/download" download title="Download JSON">JSON</a>
                ${r.type === 'inventory' || r.type === 'performance' || r.type === 'chain' || r.type === 'rx_mismatch' ? `
                  <a class="btn btn-sm btn-csv-dl" href="/api/wavecontrol/reports/${r.id}/download?format=csv" download title="Download CSV">CSV</a>
                ` : ''}
                <button class="btn btn-sm btn-delete-report btn-danger-subtle" data-id="${r.id}" title="Delete Report">🗑</button>
              </td>
            </tr>
          `).join('')}
        </tbody>
      </table>
    `
    
    // Compare mode toggle
    const compareToggle = container.querySelector('#compareToggle')
    const compareBar = container.querySelector('.reports-compare-bar')
    const compareBtn = container.querySelector('.btn-compare-reports')
    const checkboxes = container.querySelectorAll('.compare-checkbox')
    
    compareToggle.addEventListener('change', () => {
      const enabled = compareToggle.checked
      compareBar.style.display = enabled ? 'flex' : 'none'
      checkboxes.forEach(cb => {
        cb.style.display = enabled ? 'inline' : 'none'
        cb.checked = false
      })
      compareBtn.disabled = true
    })
    
    // Checkbox change - validate selection
    checkboxes.forEach(cb => {
      cb.addEventListener('change', () => {
        const selected = Array.from(checkboxes).filter(c => c.checked)
        
        if (selected.length === 2) {
          // Check if same type
          const types = selected.map(c => c.dataset.type)
          if (types[0] === types[1]) {
            compareBtn.disabled = false
            container.querySelector('.compare-info').textContent = `Compare ${types[0]} reports`
          } else {
            compareBtn.disabled = true
            container.querySelector('.compare-info').textContent = 'Reports must be the same type'
          }
        } else {
          compareBtn.disabled = true
          container.querySelector('.compare-info').textContent = `Select 2 reports of the same type (${selected.length}/2)`
        }
        
        // Limit to 2 selections
        if (selected.length >= 2) {
          checkboxes.forEach(c => {
            if (!c.checked) c.disabled = true
          })
        } else {
          checkboxes.forEach(c => c.disabled = false)
        }
      })
    })
    
    // Compare button click
    compareBtn.addEventListener('click', async () => {
      const selected = Array.from(checkboxes).filter(c => c.checked)
      if (selected.length !== 2) return
      
      const id1 = parseInt(selected[0].dataset.id)
      const id2 = parseInt(selected[1].dataset.id)
      const type = selected[0].dataset.type
      
      try {
        showToast('Comparing reports...', 'info')
        const comparison = await api.compareReports(id1, id2)
        displayReportComparison(comparison, type)
      } catch (e) {
        showToast('Comparison failed: ' + e.message, 'error')
      }
    })
    
    // View report handlers
    container.querySelectorAll('.btn-view-report').forEach(btn => {
      btn.addEventListener('click', async () => {
        const reportId = btn.dataset.id
        const reportType = btn.dataset.type
        try {
          const report = await api.getReport(reportId)
          displayReport(report, reportType)
        } catch (e) {
          showToast('Failed to load report: ' + e.message, 'error')
        }
      })
    })
    
    // Delete report handlers
    container.querySelectorAll('.btn-delete-report').forEach(btn => {
      btn.addEventListener('click', async () => {
        const reportId = btn.dataset.id
        if (!confirm('Delete this report? This cannot be undone.')) return
        
        try {
          await api.deleteReport(reportId)
          showToast('Report deleted', 'success')
          loadReportsList()
        } catch (e) {
          showToast('Failed to delete: ' + e.message, 'error')
        }
      })
    })
  } catch (e) {
    container.innerHTML = `<p class="error">Error: ${escapeHTML(e.message)}</p>`
  }
}

// Display report comparison
function displayReportComparison(comparison, type) {
  const viewer = document.getElementById('reportViewer')
  const title = document.getElementById('reportViewerTitle')
  const content = document.getElementById('reportContent')
  
  title.textContent = `${type.charAt(0).toUpperCase() + type.slice(1)} Report Comparison`
  
  const time1 = new Date(comparison.report_1_time).toLocaleString()
  const time2 = new Date(comparison.report_2_time).toLocaleString()
  
  let html = `
    <div class="report-header-strip">
      <div class="report-scope">
        <strong>Report #${comparison.report_1_id}</strong>: ${time1}
      </div>
      <div class="report-scope">
        <strong>Report #${comparison.report_2_id}</strong>: ${time2}
      </div>
    </div>
    <div class="comparison-table-container">
  `
  
  // Render comparison tables based on type
  if (comparison.summary) {
    html += `
      <h5>Summary Changes</h5>
      <table class="report-table report-table-sm">
        <thead><tr><th>Metric</th><th>Report #${comparison.report_1_id}</th><th>Report #${comparison.report_2_id}</th><th>Change</th></tr></thead>
        <tbody>
          ${Object.entries(comparison.summary).map(([key, val]) => {
            const delta = val.delta || 0
            const deltaClass = delta > 0 ? 'text-warning' : delta < 0 ? 'text-success' : ''
            const deltaStr = delta > 0 ? `+${formatCompareValue(delta, key)}` : formatCompareValue(delta, key)
            return `
              <tr>
                <td>${formatMetricName(key)}</td>
                <td>${formatCompareValue(val.report_1, key)}</td>
                <td>${formatCompareValue(val.report_2, key)}</td>
                <td class="${deltaClass}">${deltaStr}</td>
              </tr>
            `
          }).join('')}
        </tbody>
      </table>
    `
  }
  
  if (comparison.link_quality) {
    html += `
      <h5>Link Quality Changes</h5>
      <table class="report-table report-table-sm">
        <thead><tr><th>Quality</th><th>Report #${comparison.report_1_id}</th><th>Report #${comparison.report_2_id}</th><th>Change</th></tr></thead>
        <tbody>
          ${Object.entries(comparison.link_quality).map(([key, val]) => {
            const delta = val.delta || 0
            const isGood = key === 'good'
            const deltaClass = isGood ? (delta > 0 ? 'text-success' : delta < 0 ? 'text-danger' : '') 
                                      : (delta > 0 ? 'text-danger' : delta < 0 ? 'text-success' : '')
            const deltaStr = delta > 0 ? `+${delta}` : `${delta}`
            return `
              <tr>
                <td>${key.charAt(0).toUpperCase() + key.slice(1)}</td>
                <td>${val.report_1}</td>
                <td>${val.report_2}</td>
                <td class="${deltaClass}">${deltaStr}</td>
              </tr>
            `
          }).join('')}
        </tbody>
      </table>
    `
  }
  
  if (comparison.signal_quality) {
    html += `
      <h5>Signal Quality Changes</h5>
      <table class="report-table report-table-sm">
        <thead><tr><th>Quality</th><th>Report #${comparison.report_1_id}</th><th>Report #${comparison.report_2_id}</th><th>Change</th></tr></thead>
        <tbody>
          ${Object.entries(comparison.signal_quality).map(([key, val]) => {
            const delta = val.delta || 0
            const isGood = key === 'good'
            const deltaClass = isGood ? (delta > 0 ? 'text-success' : delta < 0 ? 'text-danger' : '') 
                                      : (delta > 0 ? 'text-danger' : delta < 0 ? 'text-success' : '')
            const deltaStr = delta > 0 ? `+${delta}` : `${delta}`
            return `
              <tr>
                <td>${key.charAt(0).toUpperCase() + key.slice(1)}</td>
                <td>${val.report_1}</td>
                <td>${val.report_2}</td>
                <td class="${deltaClass}">${deltaStr}</td>
              </tr>
            `
          }).join('')}
        </tbody>
      </table>
    `
  }
  
  if (comparison.system_health) {
    html += `
      <h5>System Health Changes</h5>
      <table class="report-table report-table-sm">
        <thead><tr><th>Metric</th><th>Report #${comparison.report_1_id}</th><th>Report #${comparison.report_2_id}</th><th>Change</th></tr></thead>
        <tbody>
          ${Object.entries(comparison.system_health).map(([key, val]) => {
            const delta = val.delta || 0
            const deltaClass = delta > 0 ? 'text-danger' : delta < 0 ? 'text-success' : ''
            const deltaStr = delta > 0 ? `+${delta}` : `${delta}`
            return `
              <tr>
                <td>${formatMetricName(key)}</td>
                <td>${val.report_1}</td>
                <td>${val.report_2}</td>
                <td class="${deltaClass}">${deltaStr}</td>
              </tr>
            `
          }).join('')}
        </tbody>
      </table>
    `
  }
  
  html += '</div>'
  
  content.innerHTML = html
  initSortableReportTables(content)
  viewer.style.display = 'block'
}

function formatMetricName(key) {
  return key.replace(/_/g, ' ').replace(/\b\w/g, l => l.toUpperCase())
}

function formatCompareValue(val, key) {
  if (key.includes('rate')) {
    return formatRate(val)
  }
  if (key === 'uptime') {
    return val.toFixed(1) + '%'
  }
  return val
}



function parseSortableIP(value) {
  const text = String(value || '').trim()
  if (!text) return null
  const m = text.match(/^(\d{1,3})(?:\.(\d{1,3}))(?:\.(\d{1,3}))(?:\.(\d{1,3}))$/)
  if (!m) return null
  const octets = m.slice(1).map(v => Number(v))
  if (octets.some(v => Number.isNaN(v) || v < 0 || v > 255)) return null
  return octets.reduce((acc, v) => acc * 256 + v, 0)
}

function compareSortableValues(a, b, type) {
  if (type === 'number') {
    const an = Number(a)
    const bn = Number(b)
    const aBad = Number.isNaN(an)
    const bBad = Number.isNaN(bn)
    if (aBad && bBad) return 0
    if (aBad) return 1
    if (bBad) return -1
    return an - bn
  }
  if (type === 'ip') {
    const ai = parseSortableIP(a)
    const bi = parseSortableIP(b)
    if (ai != null && bi != null) return ai - bi
    if (ai != null) return -1
    if (bi != null) return 1
  }
  return String(a || '').localeCompare(String(b || ''), undefined, { numeric: true, sensitivity: 'base' })
}

function initSortableReportTables(root) {
  if (!root) return
  root.querySelectorAll('table.report-sortable').forEach(table => {
    const headers = Array.from(table.querySelectorAll('thead th[data-sort-type]'))
    const tbody = table.querySelector('tbody')
    if (!tbody || !headers.length) return
    headers.forEach((th, index) => {
      if (th.dataset.sortInit === '1') return
      th.dataset.sortInit = '1'
      th.classList.add('report-sort-header')
      th.tabIndex = 0
      const activate = () => {
        const prevIndex = Number(table.dataset.sortIndex || '-1')
        const prevDir = table.dataset.sortDir || 'desc'
        const nextDir = prevIndex === index && prevDir === 'asc' ? 'desc' : 'asc'
        table.dataset.sortIndex = String(index)
        table.dataset.sortDir = nextDir
        headers.forEach((h, i) => {
          h.dataset.sortDir = i === index ? nextDir : ''
        })
        const rows = Array.from(tbody.querySelectorAll('tr'))
        rows.sort((rowA, rowB) => {
          const cellA = rowA.children[index]
          const cellB = rowB.children[index]
          const valA = cellA?.dataset.sortValue ?? cellA?.textContent?.trim() ?? ''
          const valB = cellB?.dataset.sortValue ?? cellB?.textContent?.trim() ?? ''
          const cmp = compareSortableValues(valA, valB, th.dataset.sortType || 'text')
          return nextDir === 'asc' ? cmp : -cmp
        })
        rows.forEach(row => tbody.appendChild(row))
      }
      th.addEventListener('click', activate)
      th.addEventListener('keydown', (ev) => {
        if (ev.key === 'Enter' || ev.key === ' ') {
          ev.preventDefault()
          activate()
        }
      })
    })
  })
}

function displayReport(report, type) {
  const viewer = document.getElementById('reportViewer')
  const title = document.getElementById('reportViewerTitle')
  const content = document.getElementById('reportContent')
  
  const reportTitleMap = {
    health: 'Health Report',
    inventory: 'Inventory Report',
    performance: 'Performance Report',
    chain: 'Chain Imbalance Report',
    rx_mismatch: 'RX Level Mismatch Report'
  }
  title.textContent = reportTitleMap[type] || `${type.charAt(0).toUpperCase() + type.slice(1)} Report`
  
  // Format report content based on type
  let html = ''
  if (report.data) {
    const data = typeof report.data === 'string' ? JSON.parse(report.data) : report.data
    
    if (type === 'health') {
      html = formatHealthReport(data)
    } else if (type === 'inventory') {
      html = formatInventoryReport(data)
    } else if (type === 'performance') {
      html = formatPerformanceReport(data)
    } else if (type === 'chain') {
      html = formatChainReport(data)
    } else if (type === 'rx_mismatch') {
      html = formatRxMismatchReport(data)
    } else {
      html = `<pre>${JSON.stringify(data, null, 2)}</pre>`
    }
  } else {
    html = '<p class="text-muted">No report data available.</p>'
  }
  
  content.innerHTML = html
  initSortableReportTables(content)
  viewer.style.display = 'block'
  
  // Wire up report tab switching
  content.querySelectorAll('.report-tab').forEach(tab => {
    tab.addEventListener('click', () => {
      // Update active tab
      content.querySelectorAll('.report-tab').forEach(t => t.classList.remove('active'))
      tab.classList.add('active')
      
      // Show/hide content
      const tabName = tab.dataset.tab
      content.querySelectorAll('.report-tab-content').forEach(c => {
        c.classList.toggle('hidden', c.id !== `tab-${tabName}`)
      })
    })
  })
  
  // Load throughput chart for performance reports
  if (type === 'performance') {
    loadThroughputChart()
  }
}

// Load and render throughput history chart
async function loadThroughputChart() {
  const container = document.getElementById('throughputChartContainer')
  if (!container) return
  
  try {
    const data = await api.getThroughputHistory()
    const samples = data.samples || []
    
    if (samples.length < 2) {
      container.innerHTML = '<div class="chart-empty">Not enough data yet. Chart populates over time.</div>'
      return
    }
    
    // Render chart without a third-party runtime dependency
    renderThroughputChart(container, samples)
  } catch (e) {
    container.innerHTML = `<div class="chart-error">Failed to load chart: ${escapeHTML(e.message)}</div>`
  }
}

// Render throughput history without a third-party chart runtime.
function renderThroughputChart(container, samples) {
  const ns = 'http://www.w3.org/2000/svg'
  const createSVG = (name, attrs = {}, text = '') => {
    const node = document.createElementNS(ns, name)
    for (const [key, value] of Object.entries(attrs)) {
      node.setAttribute(key, String(value))
    }
    if (text) node.textContent = text
    return node
  }

  const data = samples
    .map(sample => ({
      time: new Date(sample.timestamp),
      tx: Number.isFinite(Number(sample.tx_rate)) ? Math.max(0, Number(sample.tx_rate)) : 0,
      rx: Number.isFinite(Number(sample.rx_rate)) ? Math.max(0, Number(sample.rx_rate)) : 0
    }))
    .filter(sample => Number.isFinite(sample.time.getTime()))
    .sort((a, b) => a.time - b.time)

  if (data.length < 2) {
    container.innerHTML = '<div class="chart-empty">Not enough valid data to draw the chart.</div>'
    return
  }

  const margin = { top: 20, right: 80, bottom: 30, left: 60 }
  const outerWidth = Math.max(container.clientWidth || 720, 360)
  const outerHeight = 200
  const width = Math.max(outerWidth - margin.left - margin.right, 180)
  const height = outerHeight - margin.top - margin.bottom
  const startTime = data[0].time.getTime()
  const endTime = data[data.length - 1].time.getTime()
  const timeSpan = Math.max(endTime - startTime, 1)
  const maxRate = Math.max(1, ...data.flatMap(sample => [sample.tx, sample.rx])) * 1.1
  const x = time => ((time.getTime() - startTime) / timeSpan) * width
  const y = rate => height - (rate / maxRate) * height

  container.replaceChildren()
  const svg = createSVG('svg', {
    width: outerWidth,
    height: outerHeight,
    viewBox: `0 0 ${outerWidth} ${outerHeight}`,
    role: 'img',
    'aria-label': 'Transmit and receive throughput history'
  })
  const plot = createSVG('g', { transform: `translate(${margin.left},${margin.top})` })
  svg.appendChild(plot)
  container.appendChild(svg)

  const axis = createSVG('g', { class: 'chart-axis' })
  axis.appendChild(createSVG('line', { x1: 0, y1: height, x2: width, y2: height }))
  axis.appendChild(createSVG('line', { x1: 0, y1: 0, x2: 0, y2: height }))

  const xTicks = Math.min(6, data.length)
  for (let index = 0; index < xTicks; index += 1) {
    const fraction = xTicks === 1 ? 0 : index / (xTicks - 1)
    const tickX = fraction * width
    const tickTime = new Date(startTime + fraction * timeSpan)
    axis.appendChild(createSVG('line', { x1: tickX, y1: height, x2: tickX, y2: height + 5 }))
    axis.appendChild(createSVG('text', {
      x: tickX,
      y: height + 18,
      'text-anchor': index === 0 ? 'start' : index === xTicks - 1 ? 'end' : 'middle'
    }, tickTime.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })))
  }

  const yTicks = 5
  for (let index = 0; index <= yTicks; index += 1) {
    const fraction = index / yTicks
    const tickY = height - fraction * height
    const rate = fraction * maxRate
    axis.appendChild(createSVG('line', { x1: -5, y1: tickY, x2: 0, y2: tickY }))
    axis.appendChild(createSVG('text', {
      x: -9,
      y: tickY + 4,
      'text-anchor': 'end'
    }, formatRateShort(rate)))
  }
  plot.appendChild(axis)

  const linePath = key => data
    .map((sample, index) => `${index === 0 ? 'M' : 'L'}${x(sample.time).toFixed(2)},${y(sample[key]).toFixed(2)}`)
    .join(' ')

  plot.appendChild(createSVG('path', {
    d: linePath('tx'),
    fill: 'none',
    stroke: 'var(--cyan)',
    'stroke-width': 2,
    'vector-effect': 'non-scaling-stroke'
  }))
  plot.appendChild(createSVG('path', {
    d: linePath('rx'),
    fill: 'none',
    stroke: 'var(--green)',
    'stroke-width': 2,
    'vector-effect': 'non-scaling-stroke'
  }))

  const legend = createSVG('g', { transform: `translate(${Math.max(width - 60, 0)},0)` })
  const addLegendEntry = (offset, label, stroke) => {
    legend.appendChild(createSVG('line', { x1: 0, x2: 20, y1: offset, y2: offset, stroke, 'stroke-width': 2 }))
    legend.appendChild(createSVG('text', { x: 25, y: offset + 4, fill: 'var(--text-2)', 'font-size': 12 }, label))
  }
  addLegendEntry(0, 'TX', 'var(--cyan)')
  addLegendEntry(15, 'RX', 'var(--green)')
  plot.appendChild(legend)
}

function formatRateShort(bps) {
  if (!bps) return '0'
  if (bps >= 1e9) return (bps / 1e9).toFixed(1) + 'G'
  if (bps >= 1e6) return (bps / 1e6).toFixed(0) + 'M'
  if (bps >= 1e3) return (bps / 1e3).toFixed(0) + 'K'
  return bps + ''
}

function formatHealthReport(data) {
  // Data structure: { summary, link_quality, system_health, stability, firmware_distribution, top_offenders, generated_at }
  const summary = data.summary || data
  const online = summary.online || 0
  const offline = summary.offline || 0
  const total = summary.total || (online + offline)
  const uptime = summary.uptime || (total > 0 ? (online / total * 100) : 0)
  const apCount = summary.ap_count || 0
  const staCount = summary.sta_count || 0
  const apOnline = summary.ap_online || 0
  const staOnline = summary.sta_online || 0
  
  const linkQuality = data.link_quality || {}
  const goodSignal = linkQuality.good || 0
  const fairSignal = linkQuality.fair || 0
  const poorSignal = linkQuality.poor || 0
  const totalSignal = goodSignal + fairSignal + poorSignal
  const poorPct = totalSignal > 0 ? Math.round(poorSignal / totalSignal * 100) : 0
  
  const systemHealth = data.system_health || {}
  const highCPU = systemHealth.high_cpu || 0
  const highMem = systemHealth.high_mem || 0
  const highTemp = systemHealth.high_temp || 0
  
  const stability = data.stability || {}
  const flaps1h = stability.flaps_1h || 0
  const flaps24h = stability.flaps_24h || 0
  const reboots1h = stability.reboots_1h || 0
  const reboots24h = stability.reboots_24h || 0
  
  const topOffenders = data.top_offenders || {}
  const offlineDevices = topOffenders.offline || []
  const poorSignalDevices = topOffenders.poor_signal || []
  const highCPUDevices = topOffenders.high_cpu || []
  const flappingDevices = topOffenders.flapping || []
  const rebootingDevices = topOffenders.rebooting || []
  
  const fwDist = data.firmware_distribution || {}
  const fwVersions = Object.entries(fwDist).sort((a, b) => b[1] - a[1]).slice(0, 5)
  
  // Report header strip
  const headerHtml = `
    <div class="report-header-strip">
      <div class="report-scope">
        <strong>Scope:</strong> All Devices
        <span class="report-breakdown">APs: ${apCount} | STAs: ${staCount}</span>
      </div>
      <div class="report-coverage">
        <strong>Data Coverage:</strong> ${online}/${total} devices online (${uptime.toFixed(1)}%)
      </div>
      <div class="report-time">
        <strong>Generated:</strong> ${data.generated_at ? new Date(data.generated_at).toLocaleString() : 'Unknown'}
      </div>
    </div>
  `
  
  // 6 Scorecards (added Stability)
  const scorecardsHtml = `
    <div class="report-scorecards">
      <div class="report-scorecard">
        <h5>Availability</h5>
        <div class="scorecard-stats">
          <div class="scorecard-stat">
            <span class="stat-value online">${online}</span>
            <span class="stat-label">Online</span>
          </div>
          <div class="scorecard-stat">
            <span class="stat-value ${offline > 0 ? 'offline' : ''}">${offline}</span>
            <span class="stat-label">Offline</span>
          </div>
          <div class="scorecard-stat">
            <span class="stat-value">${uptime.toFixed(1)}%</span>
            <span class="stat-label">Uptime</span>
          </div>
        </div>
      </div>
      
      <div class="report-scorecard">
        <h5>Link Quality (STAs)</h5>
        <div class="scorecard-stats">
          <div class="scorecard-stat">
            <span class="stat-value good">${goodSignal}</span>
            <span class="stat-label">Good</span>
          </div>
          <div class="scorecard-stat">
            <span class="stat-value fair">${fairSignal}</span>
            <span class="stat-label">Fair</span>
          </div>
          <div class="scorecard-stat">
            <span class="stat-value ${poorSignal > 0 ? 'poor' : ''}">${poorSignal}</span>
            <span class="stat-label">Poor</span>
          </div>
        </div>
        ${totalSignal > 0 ? `<div class="scorecard-footer">${poorPct}% of STAs have poor signal</div>` : ''}
      </div>
      
      <div class="report-scorecard">
        <h5>Stability</h5>
        <div class="scorecard-stats">
          <div class="scorecard-stat">
            <span class="stat-value ${flaps1h > 0 ? 'warning' : ''}">${flaps1h}</span>
            <span class="stat-label">Flaps (1h)</span>
          </div>
          <div class="scorecard-stat">
            <span class="stat-value">${flaps24h}</span>
            <span class="stat-label">Flaps (24h)</span>
          </div>
          <div class="scorecard-stat">
            <span class="stat-value ${reboots1h > 0 ? 'warning' : ''}">${reboots1h}</span>
            <span class="stat-label">Reboots (1h)</span>
          </div>
        </div>
      </div>
      
      <div class="report-scorecard">
        <h5>System Health</h5>
        <div class="scorecard-stats">
          <div class="scorecard-stat">
            <span class="stat-value ${highCPU > 0 ? 'warning' : ''}">${highCPU}</span>
            <span class="stat-label">High CPU</span>
          </div>
          <div class="scorecard-stat">
            <span class="stat-value ${highMem > 0 ? 'warning' : ''}">${highMem}</span>
            <span class="stat-label">High Memory</span>
          </div>
          <div class="scorecard-stat">
            <span class="stat-value ${highTemp > 0 ? 'warning' : ''}">${highTemp}</span>
            <span class="stat-label">High Temp</span>
          </div>
        </div>
      </div>
      
      <div class="report-scorecard">
        <h5>AP/STA Breakdown</h5>
        <div class="scorecard-stats">
          <div class="scorecard-stat">
            <span class="stat-value">${apOnline}/${apCount}</span>
            <span class="stat-label">APs Online</span>
          </div>
          <div class="scorecard-stat">
            <span class="stat-value">${staOnline}/${staCount}</span>
            <span class="stat-label">STAs Online</span>
          </div>
        </div>
      </div>
      
      <div class="report-scorecard">
        <h5>Firmware Versions</h5>
        <div class="scorecard-list">
          ${fwVersions.length > 0 ? fwVersions.map(([fw, count]) => `
            <div class="scorecard-list-item">
              <span>${escapeHTML(fw || 'Unknown')}</span>
              <span>${count}</span>
            </div>
          `).join('') : '<span class="text-muted">No data</span>'}
        </div>
      </div>
    </div>
  `
  
  // Top Offenders tables
  let offendersHtml = '<div class="report-offenders">'
  
  if (offlineDevices.length > 0) {
    offendersHtml += `
      <div class="report-offender-section">
        <h5>Offline Devices (${offlineDevices.length})</h5>
        <table class="report-table report-table-sm">
          <thead><tr><th>Device</th><th>Site</th><th>Last Seen</th></tr></thead>
          <tbody>
            ${offlineDevices.slice(0, 10).map(d => `
              <tr>
                <td>${escapeHTML(d.hostname || d.ip || '-')}</td>
                <td>${escapeHTML(d.site || '-')}</td>
                <td>${d.last_seen ? new Date(d.last_seen).toLocaleString() : '-'}</td>
              </tr>
            `).join('')}
          </tbody>
        </table>
      </div>
    `
  }
  
  if (flappingDevices.length > 0) {
    offendersHtml += `
      <div class="report-offender-section">
        <h5>Flapping Devices (${flappingDevices.length})</h5>
        <table class="report-table report-table-sm">
          <thead><tr><th>Device</th><th>Flaps (1h)</th><th>Flaps (24h)</th></tr></thead>
          <tbody>
            ${flappingDevices.slice(0, 10).map(d => `
              <tr>
                <td>${escapeHTML(d.hostname || d.ip || '-')}</td>
                <td class="text-warning">${d.flaps_1h}</td>
                <td>${d.flaps_24h}</td>
              </tr>
            `).join('')}
          </tbody>
        </table>
      </div>
    `
  }
  
  if (rebootingDevices.length > 0) {
    offendersHtml += `
      <div class="report-offender-section">
        <h5>Rebooting Devices (${rebootingDevices.length})</h5>
        <table class="report-table report-table-sm">
          <thead><tr><th>Device</th><th>Reboots (1h)</th><th>Reboots (24h)</th></tr></thead>
          <tbody>
            ${rebootingDevices.slice(0, 10).map(d => `
              <tr>
                <td>${escapeHTML(d.hostname || d.ip || '-')}</td>
                <td class="text-warning">${d.reboots_1h}</td>
                <td>${d.reboots_24h}</td>
              </tr>
            `).join('')}
          </tbody>
        </table>
      </div>
    `
  }
  
  if (poorSignalDevices.length > 0) {
    offendersHtml += `
      <div class="report-offender-section">
        <h5>Worst Signal STAs (${poorSignalDevices.length})</h5>
        <table class="report-table report-table-sm">
          <thead><tr><th>Device</th><th>Site</th><th>Signal</th></tr></thead>
          <tbody>
            ${poorSignalDevices.slice(0, 10).map(d => `
              <tr>
                <td>${escapeHTML(d.hostname || d.ip || '-')}</td>
                <td>${escapeHTML(d.site || '-')}</td>
                <td class="text-danger">${d.signal} dBm</td>
              </tr>
            `).join('')}
          </tbody>
        </table>
      </div>
    `
  }
  
  if (highCPUDevices.length > 0) {
    offendersHtml += `
      <div class="report-offender-section">
        <h5>High CPU Devices (${highCPUDevices.length})</h5>
        <table class="report-table report-table-sm">
          <thead><tr><th>Device</th><th>Site</th><th>CPU</th></tr></thead>
          <tbody>
            ${highCPUDevices.slice(0, 10).map(d => `
              <tr>
                <td>${escapeHTML(d.hostname || d.ip || '-')}</td>
                <td>${escapeHTML(d.site || '-')}</td>
                <td class="text-warning">${d.cpu}%</td>
              </tr>
            `).join('')}
          </tbody>
        </table>
      </div>
    `
  }
  
  offendersHtml += '</div>'
  
  return headerHtml + scorecardsHtml + offendersHtml
}

function formatInventoryReport(data) {
  if (!data.devices) return '<p>No device data</p>'
  return `
    <table class="report-table">
      <thead>
        <tr>
          <th>Device</th>
          <th>IP</th>
          <th>MAC</th>
          <th>Model</th>
          <th>Flavor</th>
          <th>Firmware</th>
          <th>Type</th>
          <th>Parent</th>
          <th>Status</th>
        </tr>
      </thead>
      <tbody>
        ${data.devices.map(d => `
          <tr>
            <td>${escapeHTML(d.hostname || d.ip || '-')}</td>
            <td>${escapeHTML(d.ip || '-')}</td>
            <td>${escapeHTML(d.mac || '-')}</td>
            <td>${escapeHTML(d.product || '-')}</td>
            <td>${escapeHTML(d.flavor || '-')}</td>
            <td>${escapeHTML(d.firmware || '-')}</td>
            <td>${d.is_sta ? 'STA' : 'AP'}</td>
            <td>${escapeHTML(d.parent_hostname || d.parent_ip || '-')}</td>
            <td><span class="status-dot ${d.status === 'online' ? 'online' : 'offline'}"></span> ${escapeHTML(d.status || '-')}</td>
          </tr>
        `).join('')}
      </tbody>
    </table>
  `
}

function formatPerformanceReport(data) {
  const summary = data.summary || data
  const txRate = summary.total_tx_rate || 0
  const rxRate = summary.total_rx_rate || 0
  const apTx = summary.ap_tx_rate || 0
  const apRx = summary.ap_rx_rate || 0
  const staTx = summary.sta_tx_rate || 0
  const staRx = summary.sta_rx_rate || 0
  const avgSignal = summary.avg_signal || 0
  const deviceCount = summary.device_count || 0
  const apCount = summary.ap_count || 0
  const staCount = summary.sta_count || 0
  
  const signalQuality = data.signal_quality || {}
  const goodSignal = signalQuality.good || 0
  const fairSignal = signalQuality.fair || 0
  const poorSignal = signalQuality.poor || 0
  const totalSignal = goodSignal + fairSignal + poorSignal
  const poorPct = totalSignal > 0 ? Math.round(poorSignal / totalSignal * 100) : 0
  
  // Get device lists - support both old and new format
  const apDevices = data.ap_devices || []
  const staDevices = data.sta_devices || []
  const capacityRisk = data.capacity_risk || []
  const oldDevices = data.devices || []
  
  // Report header strip
  const headerHtml = `
    <div class="report-header-strip">
      <div class="report-scope">
        <strong>Scope:</strong> All Devices
        <span class="report-breakdown">APs: ${apCount} | STAs: ${staCount}</span>
      </div>
      <div class="report-coverage">
        <strong>Data Coverage:</strong> ${deviceCount} devices with metrics
      </div>
      <div class="report-time">
        <strong>Generated:</strong> ${data.generated_at ? new Date(data.generated_at).toLocaleString() : 'Unknown'}
      </div>
    </div>
  `
  
  // No network-wide summary - stats are per-platform only since Wave reports throughput while LTU/airMAX report capacity
  const summaryHtml = ''
  
  // Signal quality bar (still makes sense network-wide)
  let signalBarHtml = ''
  if (totalSignal > 0) {
    signalBarHtml = `
      <div class="report-signal-bar">
        <div class="signal-bar-label">STA Signal Distribution:</div>
        <div class="signal-bar">
          <div class="signal-seg good" style="width:${(goodSignal/totalSignal)*100}%" title="${goodSignal} good"></div>
          <div class="signal-seg fair" style="width:${(fairSignal/totalSignal)*100}%" title="${fairSignal} fair"></div>
          <div class="signal-seg poor" style="width:${(poorSignal/totalSignal)*100}%" title="${poorSignal} poor"></div>
        </div>
        <div class="signal-bar-legend">
          <span class="legend-item"><span class="legend-dot good"></span> Good: ${goodSignal}</span>
          <span class="legend-item"><span class="legend-dot fair"></span> Fair: ${fairSignal}</span>
          <span class="legend-item"><span class="legend-dot poor"></span> Poor: ${poorSignal} (${poorPct}%)</span>
        </div>
      </div>
    `
  }
  
  // Platform breakdown table with per-platform stats
  const platformBreakdown = data.platform_breakdown || {}
  const platformOrder = ['wave60', 'wavemlo', 'ltu', 'airmaxac', 'airmaxm', 'af11', 'af24', 'af2x', 'af5x', 'other']
  let platformHtml = ''
  const platforms = platformOrder.filter(p => platformBreakdown[p])
  if (platforms.length > 0) {
    platformHtml = `
      <div class="report-platform-breakdown">
        <h5>Performance by Platform</h5>
        <p class="report-note" style="font-size: 0.85em; color: var(--text-2); margin: 0.5em 0 1em 0; font-style: italic;">Wave reports actual throughput. LTU/airMAX report link capacity.</p>
        <table class="platform-table">
          <thead>
            <tr>
              <th>Platform</th>
              <th>APs</th>
              <th>STAs</th>
              <th>Total TX</th>
              <th>Total RX</th>
              <th>Avg Signal</th>
              <th>Poor STAs</th>
              <th>Signal Distribution</th>
            </tr>
          </thead>
          <tbody>
            ${platforms.map(p => {
              const pb = platformBreakdown[p]
              const total = (pb.good || 0) + (pb.fair || 0) + (pb.poor || 0)
              const goodPct = total > 0 ? Math.round((pb.good || 0) / total * 100) : 0
              const fairPct = total > 0 ? Math.round((pb.fair || 0) / total * 100) : 0
              const poorPct = total > 0 ? Math.round((pb.poor || 0) / total * 100) : 0
              const metricLabel = pb.metric_type === 'throughput' ? '' : ' (cap)'
              return `
                <tr>
                  <td><strong>${pb.name || p}</strong></td>
                  <td>${pb.ap_count || 0}</td>
                  <td>${pb.sta_count || 0}</td>
                  <td>${formatRate(pb.tx_rate || 0)}${metricLabel}</td>
                  <td>${formatRate(pb.rx_rate || 0)}${metricLabel}</td>
                  <td>${pb.avg_signal ? pb.avg_signal + ' dBm' : '-'}</td>
                  <td class="${(pb.poor || 0) > 0 ? 'warning' : ''}">${pb.poor || 0}</td>
                  <td>
                    ${total > 0 ? `
                      <div class="mini-signal-bar">
                        <div class="signal-seg good" style="width:${goodPct}%" title="${pb.good || 0} good"></div>
                        <div class="signal-seg fair" style="width:${fairPct}%" title="${pb.fair || 0} fair"></div>
                        <div class="signal-seg poor" style="width:${poorPct}%" title="${pb.poor || 0} poor"></div>
                      </div>
                    ` : '<span class="text-muted">-</span>'}
                  </td>
                </tr>
              `
            }).join('')}
          </tbody>
        </table>
      </div>
    `
  }
  
  // Throughput chart removed - would mix Wave throughput with LTU/airMAX capacity
  const throughputChartHtml = ''
  
  // Build tabs content
  let tabsHtml = ''
  
  // If we have new-format data with separate AP/STA lists
  if (apDevices.length > 0 || staDevices.length > 0) {
    // Sort APs by throughput
    const topAPs = [...apDevices]
      .sort((a, b) => ((b.tx_rate || 0) + (b.rx_rate || 0)) - ((a.tx_rate || 0) + (a.rx_rate || 0)))
      .slice(0, 50)
    
    // Sort STAs by throughput
    const topSTAs = [...staDevices]
      .sort((a, b) => ((b.tx_rate || 0) + (b.rx_rate || 0)) - ((a.tx_rate || 0) + (a.rx_rate || 0)))
      .slice(0, 50)
    
    // Worst signal STAs
    const worstSTAs = [...staDevices]
      .filter(d => d.signal && d.signal < 0 && d.signal > -100)
      .sort((a, b) => (a.signal || 0) - (b.signal || 0))
      .slice(0, 50)
    
    tabsHtml = `
      <div class="report-tabs">
        <button class="report-tab active" data-tab="apPerf">AP Performance (${topAPs.length})</button>
        <button class="report-tab" data-tab="staPerf">STA Performance (${topSTAs.length})</button>
        <button class="report-tab" data-tab="worstSignal">Worst Signal (${worstSTAs.length})</button>
        ${capacityRisk.length > 0 ? `<button class="report-tab" data-tab="capacityRisk">Capacity Risk (${capacityRisk.length})</button>` : ''}
      </div>
      
      <div class="report-tab-content" id="tab-apPerf">
        <p class="report-tab-desc">Top APs by link capacity (Wave: throughput, LTU/airMAX: capacity)</p>
        <table class="report-table">
          <thead>
            <tr>
              <th>AP</th>
              <th>Site</th>
              <th>Clients</th>
              <th>Poor %</th>
              <th>TX Rate</th>
              <th>RX Rate</th>
              <th>CPU</th>
            </tr>
          </thead>
          <tbody>
            ${topAPs.map(d => `
              <tr>
                <td>${escapeHTML(d.hostname || d.ip || '-')}</td>
                <td>${escapeHTML(d.site || '-')}</td>
                <td>${d.client_count || 0}</td>
                <td class="${(d.poor_pct || 0) > 20 ? 'text-danger' : ''}">${d.poor_pct || 0}%</td>
                <td>${d.tx_rate ? formatRate(d.tx_rate) : '-'}</td>
                <td>${d.rx_rate ? formatRate(d.rx_rate) : '-'}</td>
                <td>${d.cpu ? d.cpu + '%' : '-'}</td>
              </tr>
            `).join('')}
          </tbody>
        </table>
      </div>
      
      <div class="report-tab-content hidden" id="tab-staPerf">
        <p class="report-tab-desc">Top STAs by link rate</p>
        <table class="report-table">
          <thead>
            <tr>
              <th>STA</th>
              <th>Parent AP</th>
              <th>Site</th>
              <th>Signal</th>
              <th>TX Rate</th>
              <th>RX Rate</th>
            </tr>
          </thead>
          <tbody>
            ${topSTAs.map(d => {
              const quality = getSignalQuality(d.signal, d.band || '5ghz')
              const signalClass = quality === 'poor' ? 'text-danger' : quality === 'fair' ? 'text-warning' : ''
              return `
              <tr>
                <td>${escapeHTML(d.hostname || d.ip || '-')}</td>
                <td>${escapeHTML(d.parent_hostname || '-')}</td>
                <td>${escapeHTML(d.site || '-')}</td>
                <td class="${signalClass}">${d.signal && d.signal > -100 ? d.signal + ' dBm' : '-'}</td>
                <td>${d.tx_rate ? formatRate(d.tx_rate) : '-'}</td>
                <td>${d.rx_rate ? formatRate(d.rx_rate) : '-'}</td>
              </tr>
            `}).join('')}
          </tbody>
        </table>
      </div>
      
      <div class="report-tab-content hidden" id="tab-worstSignal">
        <p class="report-tab-desc">STAs with weakest signal</p>
        <table class="report-table">
          <thead>
            <tr>
              <th>STA</th>
              <th>Parent AP</th>
              <th>Site</th>
              <th>Signal</th>
              <th>TX Rate</th>
              <th>RX Rate</th>
            </tr>
          </thead>
          <tbody>
            ${worstSTAs.map(d => {
              const quality = getSignalQuality(d.signal, d.band || '5ghz')
              const signalClass = quality === 'poor' ? 'text-danger' : quality === 'fair' ? 'text-warning' : ''
              return `
              <tr>
                <td>${escapeHTML(d.hostname || d.ip || '-')}</td>
                <td>${escapeHTML(d.parent_hostname || '-')}</td>
                <td>${escapeHTML(d.site || '-')}</td>
                <td class="${signalClass}">${d.signal ? d.signal + ' dBm' : '-'}</td>
                <td>${d.tx_rate ? formatRate(d.tx_rate) : '-'}</td>
                <td>${d.rx_rate ? formatRate(d.rx_rate) : '-'}</td>
              </tr>
            `}).join('')}
          </tbody>
        </table>
      </div>
      
      ${capacityRisk.length > 0 ? `
        <div class="report-tab-content hidden" id="tab-capacityRisk">
          <p class="report-tab-desc">APs with &gt;20% poor signal clients - may need attention</p>
          <table class="report-table">
            <thead>
              <tr>
                <th>AP</th>
                <th>Site</th>
                <th>Clients</th>
                <th>Poor Clients</th>
                <th>Poor %</th>
                <th>TX Rate</th>
              </tr>
            </thead>
            <tbody>
              ${capacityRisk.map(d => `
                <tr>
                  <td>${escapeHTML(d.hostname || d.ip || '-')}</td>
                  <td>${escapeHTML(d.site || '-')}</td>
                  <td>${d.client_count || 0}</td>
                  <td class="text-danger">${d.poor_clients || 0}</td>
                  <td class="text-danger">${d.poor_pct || 0}%</td>
                  <td>${d.tx_rate ? formatRate(d.tx_rate) : '-'}</td>
                </tr>
              `).join('')}
            </tbody>
          </table>
        </div>
      ` : ''}
    `
  } else if (oldDevices.length > 0) {
    // Fall back to old format (for existing reports)
    const topTalkers = [...oldDevices]
      .filter(d => d.tx_rate || d.rx_rate)
      .sort((a, b) => ((b.tx_rate || 0) + (b.rx_rate || 0)) - ((a.tx_rate || 0) + (a.rx_rate || 0)))
      .slice(0, 50)
    
    const worstSignal = [...oldDevices]
      .filter(d => d.signal && d.signal < 0 && d.signal > -100)
      .sort((a, b) => (a.signal || 0) - (b.signal || 0))
      .slice(0, 50)
    
    tabsHtml = `
      <div class="report-tabs">
        <button class="report-tab active" data-tab="topTalkers">Top 50 Talkers</button>
        <button class="report-tab" data-tab="worstSignal">Worst 50 Signal</button>
      </div>
      
      <div class="report-tab-content" id="tab-topTalkers">
        <p class="report-tab-desc">${topTalkers.length} devices with highest link rate</p>
        <table class="report-table">
          <thead>
            <tr>
              <th>Device</th>
              <th>IP</th>
              <th>Type</th>
              <th>TX Rate</th>
              <th>RX Rate</th>
              <th>Signal</th>
            </tr>
          </thead>
          <tbody>
            ${topTalkers.map(d => `
              <tr>
                <td>${escapeHTML(d.hostname || d.ip || '-')}</td>
                <td>${escapeHTML(d.ip || '-')}</td>
                <td>${d.is_sta ? 'STA' : 'AP'}</td>
                <td>${d.tx_rate ? formatRate(d.tx_rate) : '-'}</td>
                <td>${d.rx_rate ? formatRate(d.rx_rate) : '-'}</td>
                <td class="${getSignalQuality(d.signal, '5ghz') === 'poor' ? 'text-danger' : ''}">${d.signal && d.signal > -100 ? d.signal + ' dBm' : '-'}</td>
              </tr>
            `).join('')}
          </tbody>
        </table>
      </div>
      
      <div class="report-tab-content hidden" id="tab-worstSignal">
        <p class="report-tab-desc">${worstSignal.length} devices with weakest signal</p>
        <table class="report-table">
          <thead>
            <tr>
              <th>Device</th>
              <th>IP</th>
              <th>Type</th>
              <th>Signal</th>
              <th>TX Rate</th>
              <th>RX Rate</th>
            </tr>
          </thead>
          <tbody>
            ${worstSignal.map(d => {
              const quality = getSignalQuality(d.signal, '5ghz')
              const signalClass = quality === 'poor' ? 'text-danger' : quality === 'fair' ? 'text-warning' : ''
              return `
              <tr>
                <td>${escapeHTML(d.hostname || d.ip || '-')}</td>
                <td>${escapeHTML(d.ip || '-')}</td>
                <td>${d.is_sta ? 'STA' : 'AP'}</td>
                <td class="${signalClass}">${d.signal ? d.signal + ' dBm' : '-'}</td>
                <td>${d.tx_rate ? formatRate(d.tx_rate) : '-'}</td>
                <td>${d.rx_rate ? formatRate(d.rx_rate) : '-'}</td>
              </tr>
            `}).join('')}
          </tbody>
        </table>
      </div>
    `
  }
  
  return headerHtml + summaryHtml + signalBarHtml + platformHtml + throughputChartHtml + tabsHtml
}





function formatChainReport(data) {
  const summary = data.summary || {}
  const issues = Array.isArray(data.issues) ? data.issues : []
  const threshold = summary.threshold_db || 5
  const totalIssues = summary.total_issues || issues.length || 0
  const deviceIssues = summary.device_issues || 0
  const peerIssues = summary.peer_issues || 0
  const bothIssues = summary.both_issues || 0
  const apOnlyIssues = summary.ap_only_issues || 0
  const staOnlyIssues = summary.sta_only_issues || 0

  const scorecards = `
    <div class="report-scorecards">
      <div class="report-scorecard">
        <h5>Threshold</h5>
        <div class="scorecard-stats"><div class="scorecard-stat"><span class="stat-value">${threshold} dB</span><span class="stat-label">Spread trigger</span></div></div>
      </div>
      <div class="report-scorecard">
        <h5>Total Findings</h5>
        <div class="scorecard-stats"><div class="scorecard-stat"><span class="stat-value ${totalIssues > 0 ? 'warning' : ''}">${totalIssues}</span><span class="stat-label">Rows</span></div></div>
      </div>
      <div class="report-scorecard">
        <h5>Device Radios</h5>
        <div class="scorecard-stats"><div class="scorecard-stat"><span class="stat-value">${deviceIssues}</span><span class="stat-label">Imbalanced</span></div></div>
      </div>
      <div class="report-scorecard">
        <h5>Peer Sides</h5>
        <div class="scorecard-stats">
          <div class="scorecard-stat"><span class="stat-value">${bothIssues}</span><span class="stat-label">Both</span></div>
          <div class="scorecard-stat"><span class="stat-value">${apOnlyIssues}</span><span class="stat-label">AP only</span></div>
          <div class="scorecard-stat"><span class="stat-value">${staOnlyIssues}</span><span class="stat-label">STA only</span></div>
        </div>
      </div>
    </div>
  `

  const table = `
    <table class="report-table report-sortable">
      <thead>
        <tr>
          <th data-sort-type="text">Scope</th>
          <th data-sort-type="text">Band</th>
          <th data-sort-type="text">Device / Link</th>
          <th data-sort-type="ip">Affected IP</th>
          <th data-sort-type="text">Parent AP</th>
          <th data-sort-type="text">Site</th>
          <th data-sort-type="text">Side</th>
          <th data-sort-type="number">AP Spread</th>
          <th data-sort-type="number">STA Spread</th>
          <th data-sort-type="text">AP Chains</th>
          <th data-sort-type="text">STA Chains</th>
        </tr>
      </thead>
      <tbody>
        ${issues.map(issue => {
          const apChains = Array.isArray(issue.ap_chains) ? issue.ap_chains.join(' / ') : '-'
          const staChains = Array.isArray(issue.sta_chains) ? issue.sta_chains.join(' / ') : '-'
          const deviceLabel = escapeHTML(issue.hostname || issue.ip || '-')
          const affectedIp = issue.affected_ip || issue.ip || issue.parent_ip || '-'
          const parentLabel = issue.parent_hostname || issue.parent_ip ? escapeHTML(issue.parent_hostname || issue.parent_ip) : '—'
          const side = issue.mismatch_side || issue.scope || '-'
          const sidePretty = side === 'ap_only' ? 'AP only' : side === 'sta_only' ? 'STA only' : side === 'both' ? 'Both' : side
          const apSpread = issue.ap_spread_db || 0
          const staSpread = issue.sta_spread_db || 0
          return `
            <tr>
              <td>${escapeHTML(issue.scope || '-')}</td>
              <td>${escapeHTML(issue.band || '-')}</td>
              <td>${deviceLabel}</td>
              <td data-sort-value="${escapeHTML(affectedIp)}">${escapeHTML(affectedIp)}</td>
              <td>${parentLabel}</td>
              <td>${escapeHTML(issue.site || '-')}</td>
              <td>${escapeHTML(sidePretty)}</td>
              <td class="${apSpread > threshold ? 'text-warning' : ''}" data-sort-value="${apSpread}">${apSpread} dB</td>
              <td class="${staSpread > threshold ? 'text-warning' : ''}" data-sort-value="${staSpread}">${staSpread} dB</td>
              <td>${escapeHTML(apChains)}</td>
              <td>${escapeHTML(staChains)}</td>
            </tr>
          `
        }).join('')}
      </tbody>
    </table>
  `

  const generatedAt = data.generated_at ? new Date(data.generated_at).toLocaleString() : 'Unknown'
  return `
    <div class="report-header-strip">
      <div class="report-scope"><strong>Threshold:</strong> ${threshold} dB</div>
      <div class="report-coverage"><strong>Peer findings:</strong> ${peerIssues}</div>
      <div class="report-time"><strong>Generated:</strong> ${generatedAt}</div>
    </div>
    ${scorecards}
    ${issues.length ? table : '<p class="text-muted">No chain imbalances above threshold.</p>'}
  `
}



function formatRxMismatchReport(data) {
  const summary = data.summary || {}
  const issues = Array.isArray(data.issues) ? data.issues : []
  const threshold = summary.threshold_db || 8
  const totalIssues = summary.total_issues || issues.length || 0
  const apStronger = summary.ap_stronger_issues || 0
  const staStronger = summary.sta_stronger_issues || 0

  const scorecards = `
    <div class="report-scorecards">
      <div class="report-scorecard">
        <h5>Threshold</h5>
        <div class="scorecard-stats"><div class="scorecard-stat"><span class="stat-value">${threshold} dB</span><span class="stat-label">Delta trigger</span></div></div>
      </div>
      <div class="report-scorecard">
        <h5>Total Links</h5>
        <div class="scorecard-stats"><div class="scorecard-stat"><span class="stat-value ${totalIssues > 0 ? 'warning' : ''}">${totalIssues}</span><span class="stat-label">Mismatched</span></div></div>
      </div>
      <div class="report-scorecard">
        <h5>AP RX Stronger</h5>
        <div class="scorecard-stats"><div class="scorecard-stat"><span class="stat-value">${apStronger}</span><span class="stat-label">Links</span></div></div>
      </div>
      <div class="report-scorecard">
        <h5>STA RX Stronger</h5>
        <div class="scorecard-stats"><div class="scorecard-stat"><span class="stat-value">${staStronger}</span><span class="stat-label">Links</span></div></div>
      </div>
    </div>
  `

  const table = `
    <table class="report-table report-sortable">
      <thead>
        <tr>
          <th data-sort-type="text">Band</th>
          <th data-sort-type="text">AP</th>
          <th data-sort-type="text">STA</th>
          <th data-sort-type="ip">Affected IP</th>
          <th data-sort-type="text">Site</th>
          <th data-sort-type="number">AP RX</th>
          <th data-sort-type="number">STA RX</th>
          <th data-sort-type="number">Delta</th>
          <th data-sort-type="text">Stronger Side</th>
        </tr>
      </thead>
      <tbody>
        ${issues.map(issue => {
          const affectedIp = issue.affected_ip || issue.sta_ip || issue.ap_ip || '-'
          const apRx = issue.ap_rx || 0
          const staRx = issue.sta_rx || 0
          const delta = issue.delta_db || 0
          const stronger = issue.stronger_side === 'ap_rx' ? 'AP RX' : issue.stronger_side === 'sta_rx' ? 'STA RX' : '-'
          return `
            <tr>
              <td>${escapeHTML(issue.band || '-')}</td>
              <td>${escapeHTML(issue.ap_hostname || issue.ap_ip || '-')}</td>
              <td>${escapeHTML(issue.sta_hostname || issue.sta_ip || '-')}</td>
              <td data-sort-value="${escapeHTML(affectedIp)}">${escapeHTML(affectedIp)}</td>
              <td>${escapeHTML(issue.site || '-')}</td>
              <td data-sort-value="${apRx}">${apRx} dBm</td>
              <td data-sort-value="${staRx}">${staRx} dBm</td>
              <td class="text-warning" data-sort-value="${delta}">${delta} dB</td>
              <td>${stronger}</td>
            </tr>
          `
        }).join('')}
      </tbody>
    </table>
  `

  const generatedAt = data.generated_at ? new Date(data.generated_at).toLocaleString() : 'Unknown'
  return `
    <div class="report-header-strip">
      <div class="report-scope"><strong>Threshold:</strong> ${threshold} dB</div>
      <div class="report-coverage"><strong>Findings:</strong> ${totalIssues}</div>
      <div class="report-time"><strong>Generated:</strong> ${generatedAt}</div>
    </div>
    ${scorecards}
    ${issues.length ? table : '<p class="text-muted">No AP/STA RX mismatches above threshold.</p>'}
  `
}


function formatRate(bps) {
  if (!bps) return '-'
  if (bps >= 1e9) return (bps / 1e9).toFixed(1) + ' Gbps'
  if (bps >= 1e6) return (bps / 1e6).toFixed(1) + ' Mbps'
  if (bps >= 1e3) return (bps / 1e3).toFixed(1) + ' Kbps'
  return bps + ' bps'
}

// Store listener for re-renders
store.on(() => {
  updateCounts()

  // Global warning popup (AP threshold warnings)
  try {
    updateWarningsPanel(computeActiveWarnings())
  } catch (e) {
    console.warn('warning panel update failed', e)
  }
  
  const badge = document.getElementById('userBadge')
  if (badge) {
    badge.textContent = store.user ? store.user.username : 'not signed in'
  }
  
  document.querySelectorAll('.header-tabs a').forEach(a => {
    a.classList.toggle('active', a.dataset.page === store.currentPage)
  })
  
  // Update detail panel
  const detailPanel = document.getElementById('detailPanel')
  const wasHidden = detailPanel?.classList.contains('hidden')
  
  if (store.selectedDevice && ['dashboard', 'devices', 'associations', 'drilldown'].includes(store.currentPage)) {
    const device = store.devices.find(d => d.id === store.selectedDevice)
    if (device) {
      renderDeviceDetail(detailPanel, device)
      detailPanel?.classList.remove('hidden')
      // Trigger resize so virtual table recalculates
      if (wasHidden) {
        setTimeout(() => window.dispatchEvent(new Event('resize')), 50)
      }
    } else {
      detailPanel?.classList.add('hidden')
      if (!wasHidden) {
        setTimeout(() => window.dispatchEvent(new Event('resize')), 50)
      }
    }
  } else if (!['associations', 'drilldown'].includes(store.currentPage)) {
    // Don't auto-hide on associations/drilldown - let manual close handle it
    detailPanel?.classList.add('hidden')
    if (!wasHidden) {
      setTimeout(() => window.dispatchEvent(new Event('resize')), 50)
    }
  }
})

// Nav link handlers
document.querySelectorAll('.header-tabs a[data-page]').forEach(link => {
  link.addEventListener('click', e => {
    e.preventDefault()
    const page = link.dataset.page
    store.set({ currentPage: page, selectedDevice: null })
    renderCurrentPage()
  })
})

// Sidebar link handlers (logs, users)
document.querySelectorAll('.sidebar-link[data-page]').forEach(link => {
  link.addEventListener('click', e => {
    e.preventDefault()
    const page = link.dataset.page
    store.set({ currentPage: page, selectedDevice: null })
    renderCurrentPage()
  })
})

// Logout
document.getElementById('logoutBtn')?.addEventListener('click', async () => {
  ws.disconnect()
  
  try {
    await api.logout()
  } catch (e) {
    // Clear local state even if the server is temporarily unreachable.
  }
  forceClientLogout('Signed out')
})

// Status filter toggles
function syncStatusBadgeClasses() {
  ;['filterOnline', 'filterOffline', 'filterUnknown'].forEach(id => {
    const input = document.getElementById(id)
    if (!input) return
    const label = input.closest('.status-badge')
    if (!label) return
    const checked = !!input.checked
    label.classList.toggle('active', checked)
    label.classList.toggle('inactive', !checked)
  })
}

// Ensure the status filter inputs reflect the *actual* applied filter state.
// Some browsers restore checkbox state on reload; our filter defaults are in JS.
function applyStatusFiltersToUI() {
  const mapping = {
    filterOnline: 'online',
    filterOffline: 'offline',
    filterUnknown: 'unknown'
  }

  Object.entries(mapping).forEach(([inputId, key]) => {
    const input = document.getElementById(inputId)
    if (!input) return
    input.checked = !!store.filters?.[key]
  })

  syncStatusBadgeClasses()
}

;['filterOnline', 'filterOffline', 'filterUnknown'].forEach(id => {
  const el = document.getElementById(id)
  if (el) {
    el.addEventListener('change', e => {
      const status = id.replace('filter', '').toLowerCase()
      const filters = { ...store.filters, [status]: e.target.checked }
      store.set({ filters })

      // Keep the header badges visually in sync with the applied filters.
      applyStatusFiltersToUI()
      // Preserve scroll position during filter change
      const virtualScroll = document.querySelector('.virtual-scroll-container')
      const tableWrapper = document.querySelector('.device-table-wrapper')
      const scrollTop = virtualScroll?.scrollTop || tableWrapper?.scrollTop || 0
      renderCurrentPage()
      renderTree()
      const newVirtualScroll = document.querySelector('.virtual-scroll-container')
      const newWrapper = document.querySelector('.device-table-wrapper')
      if (newVirtualScroll && scrollTop > 0) {
        newVirtualScroll.scrollTop = scrollTop
      } else if (newWrapper && scrollTop > 0) {
        newWrapper.scrollTop = scrollTop
      }
    })
  }
})

// Initialize badge highlight state on initial load
applyStatusFiltersToUI()

// Header search with in-view highlighting and navigation
const searchInput = document.getElementById('searchInput')
const searchInfo = document.getElementById('searchInfo')
let searchDebounce = null
let searchMatches = []  // Array of matched element IDs
let currentMatchIndex = -1

function performSearch() {
  const query = (searchInput?.value || '').trim().toLowerCase()
  searchMatches = []
  currentMatchIndex = -1
  
  // Clear all highlights
  document.querySelectorAll('.search-highlight').forEach(el => el.classList.remove('search-highlight'))
  document.querySelectorAll('.search-current').forEach(el => el.classList.remove('search-current'))
  
  if (!query) {
    if (searchInfo) searchInfo.textContent = ''
    return
  }
  
  // Find matches based on current page
  const page = store.currentPage
  
  if (page === 'dashboard' || page === 'devices') {
    // Search ALL devices in store, not just DOM
    store.devices.forEach(d => {
      const hostname = (d.hostname || '').toLowerCase()
      const ip = (d.ip_address || '').toLowerCase()
      const mac = (d.mac || '').toLowerCase()
      const product = (d.product || d.model || '').toLowerCase()
      
      if (hostname.includes(query) || ip.includes(query) || mac.includes(query) || product.includes(query)) {
        searchMatches.push({ type: 'device', id: d.id, device: d })
      }
    })
  } else if (page === 'topology') {
    // Topology view - search all devices, will highlight cards/chips
    store.devices.forEach(d => {
      const hostname = (d.hostname || '').toLowerCase()
      const ip = (d.ip_address || '').toLowerCase()
      
      if (hostname.includes(query) || ip.includes(query)) {
        searchMatches.push({ type: 'device', id: d.id, device: d, isSTA: !!d.parent_id })
      }
    })
  } else if (page === 'map') {
    // Map view - match devices for map focus
    store.devices.forEach(d => {
      const hostname = (d.hostname || '').toLowerCase()
      const ip = (d.ip_address || '').toLowerCase()
      if (hostname.includes(query) || ip.includes(query)) {
        searchMatches.push({ type: 'device', id: d.id, device: d })
      }
    })
  } else if (page === 'drilldown') {
    // Drilldown view - search ONLY the currently displayed drilldown table rows
    // (not the full device inventory).
    const tbody = document.getElementById('drilldownBody')
    const rows = tbody ? Array.from(tbody.querySelectorAll('tr[data-id]')) : []
    rows.forEach(row => {
      const text = (row.innerText || row.textContent || '').toLowerCase()
      if (!text.includes(query)) return
      const id = parseInt(row.dataset.id || '', 10)
      if (Number.isNaN(id)) return
      const device = store.getDeviceById(id) || store.devices.find(d => d && d.id === id)
      if (device) searchMatches.push({ type: 'device', id: device.id, device })
    })
  }
  
  // Update search info
  updateSearchInfo()
  
  // If we have matches, highlight the first one
  if (searchMatches.length > 0) {
    currentMatchIndex = 0
    highlightCurrentMatch()
  }
}

function updateSearchInfo() {
  if (!searchInfo) return
  
  if (searchMatches.length === 0) {
    const query = (searchInput?.value || '').trim()
    searchInfo.textContent = query ? 'No matches' : ''
    searchInfo.className = 'search-info' + (query ? ' no-matches' : '')
  } else {
    searchInfo.textContent = `${currentMatchIndex + 1} of ${searchMatches.length}`
    searchInfo.className = 'search-info has-matches'
  }
}

function highlightCurrentMatch() {
  // Remove current highlight from all
  document.querySelectorAll('.search-current').forEach(el => el.classList.remove('search-current'))
  document.querySelectorAll('.search-highlight').forEach(el => el.classList.remove('search-highlight'))
  
  if (currentMatchIndex < 0 || currentMatchIndex >= searchMatches.length) return
  
  const match = searchMatches[currentMatchIndex]
  const page = store.currentPage
  
  if (match?.type === 'device') {
    const deviceId = match.id
    const device = match.device
    
    if (page === 'dashboard' || page === 'devices') {
      // Expand parent if this is a STA
      if (device.parent_id) {
        store.treeExpanded[device.parent_id] = true
        // Re-render tree to show expanded state
        renderTree()
      }
      
      // Find and highlight the tree node
      const treeNode = document.querySelector(`.tree-node[data-id="${deviceId}"]`)
      if (treeNode) {
        const content = treeNode.querySelector('.tree-node-content')
        content?.classList.add('search-highlight')
        content?.classList.add('search-current')
        treeNode.scrollIntoView({ behavior: 'smooth', block: 'center' })
      }
      
      // Scroll to and highlight device in the table (works with virtual table too)
      // Also select the device to show in detail panel
      store.set({ selectedDevice: deviceId })
      setTimeout(() => scrollToDevice(deviceId), 100)
    } else if (page === 'topology') {
      // Find the card or STA chip
      if (device.parent_id) {
        // It's a STA - find the chip
        const chip = document.querySelector(`.topo-sta-chip[data-id="${deviceId}"]`)
        if (chip) {
          chip.classList.add('search-highlight')
          chip.classList.add('search-current')
          chip.scrollIntoView({ behavior: 'smooth', block: 'center' })
        } else {
          // Chip might be in a card that's not showing offline - highlight parent card
          const parentCard = document.querySelector(`.topo-card[data-id="${device.parent_id}"]`)
          if (parentCard) {
            parentCard.classList.add('search-highlight')
            parentCard.classList.add('search-current')
            parentCard.scrollIntoView({ behavior: 'smooth', block: 'center' })
          }
        }
      } else {
        // It's an AP - find the card
        const card = document.querySelector(`.topo-card[data-id="${deviceId}"]`)
        if (card) {
          card.classList.add('search-highlight')
          card.classList.add('search-current')
          card.scrollIntoView({ behavior: 'smooth', block: 'center' })
        }
      }
    } else if (page === 'map') {
      // Could pan map to marker - for now just show in info
      // TODO: integrate with Leaflet map panning
    } else if (page === 'drilldown') {
      // Find and highlight the drilldown row
      const row = document.querySelector(`#drilldownBody tr[data-id="${deviceId}"]`)
      if (row) {
        row.classList.add('search-highlight')
        row.classList.add('search-current')
        row.scrollIntoView({ behavior: 'smooth', block: 'center' })
        
        // Also select the device
        store.set({ selectedDevice: deviceId })
        
        // Show detail panel
        const detailPanel = document.getElementById('detailPanel')
        if (device && detailPanel) {
          renderDeviceDetail(detailPanel, device)
          detailPanel.classList.remove('hidden')
        }
      }
    }
  } else if (match instanceof Element) {
    // Legacy DOM element handling
    if (match.classList.contains('tree-node')) {
      match.querySelector('.tree-node-content')?.classList.add('search-current')
    } else {
      match.classList.add('search-current')
    }
    match.scrollIntoView({ behavior: 'smooth', block: 'center' })
  }
  
  updateSearchInfo()
}

function searchNext() {
  if (searchMatches.length === 0) return
  currentMatchIndex = (currentMatchIndex + 1) % searchMatches.length
  highlightCurrentMatch()
}

function searchPrev() {
  if (searchMatches.length === 0) return
  currentMatchIndex = (currentMatchIndex - 1 + searchMatches.length) % searchMatches.length
  highlightCurrentMatch()
}

searchInput?.addEventListener('input', () => {
  clearTimeout(searchDebounce)
  searchDebounce = setTimeout(performSearch, 150)
})

searchInput?.addEventListener('keydown', e => {
  if (e.key === 'Escape') {
    searchInput.value = ''
    performSearch()
  } else if (e.key === 'Enter' || e.key === 'ArrowDown') {
    e.preventDefault()
    searchNext()
  } else if (e.key === 'ArrowUp') {
    e.preventDefault()
    searchPrev()
  }
})

// Search navigation buttons
document.getElementById('searchNext')?.addEventListener('click', searchNext)
document.getElementById('searchPrev')?.addEventListener('click', searchPrev)

// Re-run search when page changes
store.subscribe((newState, oldState) => {
  if (newState.currentPage !== oldState.currentPage) {
    // Small delay to let the page render
    setTimeout(performSearch, 50)
  }
})

// Tree search
const treeSearch = document.getElementById('treeSearch')
let treeSearchDebounce = null
treeSearch?.addEventListener('input', () => {
  clearTimeout(treeSearchDebounce)
  treeSearchDebounce = setTimeout(() => {
    const filter = treeSearch.value.trim().toLowerCase()
    store.set({ treeFilter: filter })
    renderTree(filter)
    // Re-render map if on map page
    if (store.currentPage === 'map') {
      initMap()
    }
  }, 200)
})

// Add device modal
const addDeviceModal = document.getElementById('addDeviceModal')
document.getElementById('btnAddDevice')?.addEventListener('click', () => {
  addDeviceModal?.classList.remove('hidden')
  document.getElementById('addDeviceIP')?.focus()
})

document.getElementById('closeAddDevice')?.addEventListener('click', () => addDeviceModal?.classList.add('hidden'))
document.getElementById('cancelAddDevice')?.addEventListener('click', () => addDeviceModal?.classList.add('hidden'))

// Job events modal close
document.getElementById('closeJobEvents')?.addEventListener('click', () => {
  document.getElementById('jobEventsModal')?.classList.add('hidden')
})

// Enter key submits add device form
document.getElementById('addDevicePass')?.addEventListener('keydown', (e) => {
  if (e.key === 'Enter') {
    e.preventDefault()
    document.getElementById('confirmAddDevice')?.click()
  }
})

document.getElementById('confirmAddDevice')?.addEventListener('click', async () => {
  const ip = document.getElementById('addDeviceIP')?.value.trim()
  const username = document.getElementById('addDeviceUser')?.value.trim()
  const password = document.getElementById('addDevicePass')?.value
  
  if (!ip) {
    showToast('IP address required', 'error')
    return
  }
  
  const btn = document.getElementById('confirmAddDevice')
  try {
    btn.disabled = true
    btn.textContent = 'Adding...'
    
    await api.addDevice(ip, username, password)
    const devices = await api.devices()
    store.set({ devices: Array.isArray(devices) ? devices : [] })
    
    addDeviceModal?.classList.add('hidden')
    showToast('Device added', 'success')
    renderTree()
    renderCurrentPage()
    
    document.getElementById('addDeviceIP').value = ''
    document.getElementById('addDeviceUser').value = ''
    document.getElementById('addDevicePass').value = ''
  } catch (e) {
    showToast('Failed: ' + e.message, 'error')
  } finally {
    btn.disabled = false
    btn.textContent = 'Add Device'
  }
})

// Track mousedown target to prevent drag-out-close
let addDeviceMouseDownTarget = null
addDeviceModal?.addEventListener('mousedown', e => {
  addDeviceMouseDownTarget = e.target
})
addDeviceModal?.addEventListener('click', e => {
  if (e.target === addDeviceModal && addDeviceMouseDownTarget === addDeviceModal) {
    addDeviceModal.classList.add('hidden')
  }
  addDeviceMouseDownTarget = null
})

// Bulk add modal
const bulkAddModal = document.getElementById('bulkAddModal')
document.getElementById('btnBulkAdd')?.addEventListener('click', () => {
  bulkAddModal?.classList.remove('hidden')
  document.getElementById('bulkAddIPs')?.focus()
  document.getElementById('bulkAddResults')?.classList.add('hidden')
})

document.getElementById('closeBulkAdd')?.addEventListener('click', () => bulkAddModal?.classList.add('hidden'))
document.getElementById('cancelBulkAdd')?.addEventListener('click', () => bulkAddModal?.classList.add('hidden'))

document.getElementById('confirmBulkAdd')?.addEventListener('click', async () => {
  const ipsText = document.getElementById('bulkAddIPs')?.value || ''
  const username = document.getElementById('bulkAddUser')?.value.trim()
  const password = document.getElementById('bulkAddPass')?.value
  
  // Parse IPs (comma or newline separated)
  const ips = ipsText.split(/[\n,]/).map(s => s.trim()).filter(s => s)
  
  if (ips.length === 0) {
    showToast('Enter at least one IP', 'error')
    return
  }
  
  const btn = document.getElementById('confirmBulkAdd')
  const results = document.getElementById('bulkAddResults')
  const ipsTextarea = document.getElementById('bulkAddIPs')
  
  try {
    btn.disabled = true
    btn.textContent = `Adding ${ips.length} devices...`
    results.innerHTML = '<p>Starting...</p>'
    results.classList.remove('hidden')
    
    // Start the bulk add job
    const response = await api.bulkAddDevices(ips, username, password)
    
    if (!response.job_id) {
      throw new Error('No job ID returned')
    }
    
    // Track the job - the job panel will show progress via WebSocket
    trackJob(response.job_id, 'bulk_add', [])
    
    // Set up result tracking for this specific job
    const jobResults = []
    let completed = 0
    
    // Listen for job events via existing WebSocket handler
    const handleBulkAddResult = (msg) => {
      if (msg.type === 'job_event' && msg.data?.job_id === response.job_id) {
        if (msg.data.data) {
          jobResults.push(msg.data.data)
          // Update results display
          const icon = msg.data.data.status === 'success' ? '[ok]' : '[x]'
          const cls = msg.data.data.status === 'success' ? 'success' : 'error'
          const text = msg.data.data.message || msg.data.data.hostname || msg.data.data.status
          results.innerHTML += `<div class="bulk-result ${cls}">${icon} ${escapeHTML(msg.data.data.ip)}: ${escapeHTML(text)}</div>`
        }
      }
      
      if (msg.type === 'job_progress' && msg.data?.job_id === response.job_id) {
        completed = msg.data.completed_steps || 0
        btn.textContent = `Adding... ${completed}/${ips.length}`
      }
      
      if (msg.type === 'job_update' && msg.data?.job_id === response.job_id && 
          (msg.data.status === 'completed' || msg.data.status === 'failed')) {
        // Job complete - finalize UI
        ws.off(handleBulkAddResult)
        
        const allResults = msg.data.results || jobResults
        const failures = allResults.filter(r => r.status !== 'success')
        const successes = allResults.filter(r => r.status === 'success')
        
        // Update textarea: keep only failed IPs
        if (failures.length > 0 && successes.length > 0) {
          ipsTextarea.value = failures.map(f => f.ip).join('\n')
          results.innerHTML += `<div class="bulk-info"><small>${successes.length} successful IPs removed. ${failures.length} failed - update credentials and retry.</small></div>`
        } else if (failures.length === 0) {
          ipsTextarea.value = ''
        }
        
        // Refresh device list
        api.devices().then(devices => {
          store.set({ devices: Array.isArray(devices) ? devices : [] })
          renderTree()
        })
        
        // Show toast
        if (successes.length === ips.length) {
          showToast(`Added ${successes.length}/${ips.length} devices`, 'success')
          setTimeout(() => bulkAddModal?.classList.add('hidden'), 1500)
        } else if (successes.length > 0) {
          showToast(`Added ${successes.length}/${ips.length} devices - ${failures.length} failed`, 'warning')
        } else {
          showToast(`All ${ips.length} devices failed to add`, 'error')
        }
        
        btn.disabled = false
        btn.textContent = 'Add All'
      }
    }
    
    // Subscribe to WebSocket updates
    const unsubscribe = ws.on(handleBulkAddResult)
    // Store unsubscribe for cleanup
    handleBulkAddResult.unsubscribe = unsubscribe
    
    // Clear initial "Starting..." message
    results.innerHTML = ''
    
  } catch (e) {
    results.innerHTML = `<p class="error">Error: ${escapeHTML(e.message)}</p>`
    showToast('Bulk add failed', 'error')
    btn.disabled = false
    btn.textContent = 'Add All'
  }
})

// Track mousedown target to prevent drag-out-close
let bulkAddMouseDownTarget = null
bulkAddModal?.addEventListener('mousedown', e => {
  bulkAddMouseDownTarget = e.target
})
bulkAddModal?.addEventListener('click', e => {
  // Only close if both mousedown AND click were on the backdrop
  if (e.target === bulkAddModal && bulkAddMouseDownTarget === bulkAddModal) {
    bulkAddModal.classList.add('hidden')
  }
  bulkAddMouseDownTarget = null
})

// Auth retry modal (for failed STA credentials during fanout)
const authRetryModal = document.getElementById('authRetryModal')
let authRetryDeviceIds = []

document.getElementById('closeAuthRetry')?.addEventListener('click', () => authRetryModal?.classList.add('hidden'))
document.getElementById('cancelAuthRetry')?.addEventListener('click', () => authRetryModal?.classList.add('hidden'))

let authRetryMouseDownTarget = null
authRetryModal?.addEventListener('mousedown', e => {
  authRetryMouseDownTarget = e.target
})
authRetryModal?.addEventListener('click', e => {
  if (e.target === authRetryModal && authRetryMouseDownTarget === authRetryModal) {
    authRetryModal.classList.add('hidden')
  }
  authRetryMouseDownTarget = null
})

// Show auth retry modal for devices that failed authentication
window.showAuthRetryModal = function(failedDevices) {
  if (!failedDevices || failedDevices.length === 0) return
  
  authRetryDeviceIds = failedDevices.map(d => d.device_id)
  
  // Show list of failed devices
  const deviceList = document.getElementById('authRetryDevices')
  deviceList.innerHTML = failedDevices.map(d => 
    `<div class="auth-retry-device">${escapeHTML(d.hostname || d.device_ip || 'Device ' + d.device_id)}</div>`
  ).join('')
  
  document.getElementById('authRetryInfo').textContent = 
    `${failedDevices.length} device(s) failed authentication. Enter credentials to retry:`
  
  // Clear previous results and inputs
  document.getElementById('authRetryResults')?.classList.add('hidden')
  document.getElementById('authRetryUser').value = ''
  document.getElementById('authRetryPass').value = ''
  
  authRetryModal?.classList.remove('hidden')
  document.getElementById('authRetryUser')?.focus()
}

document.getElementById('confirmAuthRetry')?.addEventListener('click', async () => {
  if (authRetryDeviceIds.length === 0) return
  
  const username = document.getElementById('authRetryUser')?.value.trim()
  const password = document.getElementById('authRetryPass')?.value
  const force = document.getElementById('authRetryForce')?.checked
  
  if (!username) {
    showToast('Username required', 'error')
    return
  }
  
  const btn = document.getElementById('confirmAuthRetry')
  const results = document.getElementById('authRetryResults')
  
  try {
    btn.disabled = true
    btn.textContent = `Retrying ${authRetryDeviceIds.length} devices...`
    results.innerHTML = '<p>Processing...</p>'
    results.classList.remove('hidden')
    
    const response = await api.retryUpgradeWithCredentials(authRetryDeviceIds, username, password, force)
    
    // Show results
    const failures = response.results.filter(r => r.status !== 'success' && r.status !== 'skipped')
    const successes = response.results.filter(r => r.status === 'success')
    const authFailures = response.results.filter(r => r.status === 'auth_failed')
    
    let html = response.results.map(r => {
      const icon = r.status === 'success' ? '[ok]' : r.status === 'skipped' ? '[-]' : '[x]'
      const cls = r.status === 'success' ? 'success' : r.status === 'skipped' ? 'warning' : 'error'
      return `<div class="bulk-result ${cls}">${icon} ${escapeHTML(r.hostname || r.device_ip || 'Device ' + r.device_id)}: ${escapeHTML(r.message || r.status)}</div>`
    }).join('')
    
    results.innerHTML = html
    
    // If still have auth failures, update the list
    if (authFailures.length > 0) {
      authRetryDeviceIds = authFailures.map(d => d.device_id)
      showToast(`${successes.length} succeeded, ${authFailures.length} still failing auth`, 'warning')
    } else if (successes.length > 0) {
      showToast(`Retry completed: ${successes.length} succeeded`, 'success')
      setTimeout(() => authRetryModal?.classList.add('hidden'), 2000)
    } else {
      showToast('Retry failed - check credentials', 'error')
    }
    
  } catch (e) {
    results.innerHTML = `<p class="error">Error: ${escapeHTML(e.message)}</p>`
    showToast('Retry failed', 'error')
  } finally {
    btn.disabled = false
    btn.textContent = 'Retry with Credentials'
  }
})

// Upgrade modal
const upgradeModal = document.getElementById('upgradeModal')
let upgradeDeviceId = null
let upgradeDeviceParentId = null

document.getElementById('closeUpgrade')?.addEventListener('click', () => upgradeModal?.classList.add('hidden'))
document.getElementById('cancelUpgrade')?.addEventListener('click', () => upgradeModal?.classList.add('hidden'))

window.refreshDevice = async function(deviceId) {
  try {
    await api.refreshDevice(deviceId)
    showToast('Device refreshed', 'success')
  } catch (e) {
    showToast('Refresh failed: ' + e.message, 'error')
  }
}

window.rebootDevice = async function(deviceId) {
  const device = store.devices.find(d => d.id === deviceId)
  if (!device) return

  const name = device.hostname || device.ip_address || device.mac || 'device'
  const platform = String(device.platform || device.flavor || 'unknown').toUpperCase()
  const ok = window.confirm(`Reboot ${name}?

This sends the platform-specific reboot command directly to the remote radio.
Platform/flavor: ${platform}`)
  if (!ok) return

  try {
    const resp = await api.rebootDevice(deviceId)
    const apiUsed = resp?.api ? ` via ${resp.api}` : ''
    showToast(`Reboot initiated${apiUsed}`, 'success')

    const newDevices = store.devices.map(d => {
      if (d.id !== deviceId) return d
      return {
        ...d,
        status: 'unknown',
        db_status: 'unknown',
        status_reason: 'reboot_requested',
        db_status_reason: 'reboot_requested',
        online: false
      }
    })
    store.set({ devices: newDevices })
  } catch (e) {
    showToast('Reboot failed: ' + e.message, 'error')
  }
}

window.learnReplacementMAC = async function(deviceId) {
  const device = store.devices.find(d => d.id === deviceId)
  if (!device) return

  try {
    let mismatch = device.identity_mismatch
    try {
      mismatch = await api.deviceIdentityMismatch(deviceId)
    } catch (e) {
      showToast('No captured replacement MAC yet. Refresh the device first.', 'warning')
      await api.refreshDevice(deviceId)
      return
    }

    const observed = Array.isArray(mismatch?.observed_macs) ? mismatch.observed_macs : []
    if (observed.length === 0) {
      showToast('No observed replacement MAC recorded. Refresh the device first.', 'warning')
      return
    }

    let selectedMac = observed[0]
    if (observed.length > 1) {
      const entered = window.prompt(`Observed replacement MACs:\n${observed.join('\n')}\n\nEnter the MAC to learn:`, observed[0])
      if (!entered) return
      selectedMac = entered.trim()
    }

    const expected = mismatch.expected_mac || device.mac || 'unknown'
    const observedIP = mismatch.observed_ip || device.ip_address || 'unknown'
    const role = String(device.role || (device.parent_id ? 'sta' : 'ap')).toLowerCase()
    const isAP = role === 'ap'
    const roleLabel = isAP ? 'AP' : 'STA'
    const reason = isAP ? 'ap_replaced' : 'sta_replaced'
    const actionCopy = isAP
      ? 'This will replace the AP inventory MAC, update child station parent references, clear mac_mismatch, and audit the change.'
      : 'This will replace the station inventory MAC, keep the AP association unchanged, clear mac_mismatch, and audit the change.'
    const ok = window.confirm(`Learn replacement MAC for ${roleLabel} ${device.hostname || device.ip_address || 'device'}?

Expected/current MAC: ${expected}
Observed at ${observedIP}: ${selectedMac}

${actionCopy}`)
    if (!ok) return

    const resp = await api.learnDeviceMAC(deviceId, selectedMac, reason)

    const newDevices = store.devices.map(d => {
      if (d.id !== deviceId) return d
      return {
        ...d,
        mac: resp.new_mac || selectedMac,
        status: 'unknown',
        db_status: 'unknown',
        status_reason: '',
        db_status_reason: '',
        identity_mismatch: null,
        online: false
      }
    })
    store.set({ devices: newDevices })
    showToast(`Replacement MAC learned: ${resp.old_mac} → ${resp.new_mac}`, 'success')
    await refreshDevices()
    store.set({ selectedDevice: deviceId })
  } catch (e) {
    showToast('Learn replacement MAC failed: ' + e.message, 'error')
  }
}


window.updateDeviceAlerting = async function(deviceId, patch) {
  const device = store.devices.find(d => d.id === deviceId)
  if (!device) return
  try {
    const resp = await api.updateDeviceAlerting(deviceId, patch)
    const updated = resp?.device || { id: deviceId, ...patch }
    Object.assign(device, updated)
    store.set({ devices: store.devices })
    updateDetailPanel(device)
    await refreshAlertNavBadge()
    const msg = Object.prototype.hasOwnProperty.call(patch, 'alertable')
      ? (patch.alertable ? 'Device marked alertable' : 'Device marked not alertable')
      : (patch.clear_silence ? 'Alert silence cleared' : 'Device alert silence updated')
    showToast(msg, 'success')
  } catch (e) {
    showToast('Alerting update failed: ' + e.message, 'error')
    await refreshDevices()
  }
}

window.editDeviceAlertNotes = async function(deviceId) {
  const device = store.devices.find(d => d.id === deviceId)
  if (!device) return
  const notes = window.prompt('Alert notes for this device:', device.alert_notes || '')
  if (notes === null) return
  await window.updateDeviceAlerting(deviceId, { alert_notes: notes })
}

window.selectDevice = function(deviceId) {
  const device = store.devices.find(d => d.id === deviceId)
  if (!device) return
  
  // If it's a STA and we're not on the right page, switch to dashboard
  if (!['dashboard', 'devices', 'associations'].includes(store.currentPage)) {
    store.set({ currentPage: 'dashboard' })
  }
  
  // Expand parent tree node if this is a STA
  if (device.parent_id) {
    store.treeExpanded[device.parent_id] = true
  }
  
  // Select the device
  store.set({ selectedDevice: deviceId })
  
  // Scroll to the device row in the table
  setTimeout(() => {
    const row = document.querySelector(`tr[data-id="${deviceId}"], .tree-node[data-id="${deviceId}"]`)
    if (row) {
      row.scrollIntoView({ behavior: 'smooth', block: 'center' })
    }
  }, 100)
}

window.showUpgradeModal = async function(deviceId) {
  const device = store.devices.find(d => d.id === deviceId)
  if (!device) return
  
  upgradeDeviceId = deviceId
  upgradeDeviceParentId = device.parent_id
  
  // Show/hide fanout option for APs
  const fanoutOption = document.getElementById('upgradeFanoutOption')
  if (fanoutOption) {
    fanoutOption.style.display = device.parent_id ? 'none' : 'block'
    document.getElementById('upgradeFanout').checked = false
  }
  
  // Show device info
  document.getElementById('upgradeInfo').innerHTML = `
    <div><strong>${escapeHTML(device.hostname || device.ip_address)}</strong></div>
    <div>Current: ${escapeHTML(device.firmware_version || device.firmware || 'Unknown')}</div>
    <div>Flavor: ${escapeHTML(device.flavor || 'Unknown')}</div>
  `
  
  // Load firmware versions (grouped by version, auto-selects correct file per device flavor)
  const select = document.getElementById('upgradeSelect')
  select.innerHTML = '<option>Loading...</option>'
  
  try {
    const versions = await api.firmwareVersions()
    const deviceFlavor = (device.flavor || '').toLowerCase()
    const devicePlatform = (device.platform || '').toLowerCase()
    
    // Filter to versions that have firmware for this device's flavor or platform
    const matching = versions.filter(v => {
      // Check if this version has the device's flavor
      const hasFlavor = v.flavors?.some(f => f.toLowerCase() === deviceFlavor)
      // Or matching platform (for mixed equipment upgrades)
      const hasPlatform = v.platform?.toLowerCase() === devicePlatform
      return hasFlavor || (devicePlatform && hasPlatform)
    })
    
    if (matching.length === 0) {
      select.innerHTML = `<option value="">No firmware for ${escapeHTML(device.flavor || 'unknown flavor')}</option>`
    } else {
      select.innerHTML = matching.map(v => {
        const flavorList = v.flavors?.join(', ') || v.platform || ''
        return `<option value="${escapeAttr(v.version)}" title="Available for: ${escapeAttr(flavorList)}">${escapeHTML(v.version)} (${escapeHTML(v.platform || 'unknown')})</option>`
      }).join('')
      
      // Hide warning - version-based selection handles equipment differences automatically
      const warningEl = document.getElementById('upgradeWarning')
      if (warningEl) {
        warningEl.classList.add('hidden')
      }
    }
  } catch (e) {
    console.error('Firmware versions error:', e)
    select.innerHTML = '<option value="">Error loading firmware</option>'
  }
  
  document.getElementById('upgradeProgress').classList.add('hidden')
  upgradeModal?.classList.remove('hidden')
}

document.getElementById('confirmUpgrade')?.addEventListener('click', async () => {
  if (!upgradeDeviceId) return
  
  const firmware = document.getElementById('upgradeSelect')?.value
  const force = document.getElementById('upgradeForce')?.checked
  const fanout = document.getElementById('upgradeFanout')?.checked && !upgradeDeviceParentId
  
  const btn = document.getElementById('confirmUpgrade')
  
  try {
    btn.disabled = true
    btn.textContent = 'Starting...'
    
    // Use async job system with firmware version - backend resolves correct file per device
    const jobType = fanout ? 'fanout_upgrade' : 'upgrade'
    await startTrackedJob(jobType, [upgradeDeviceId], { 
      firmware_version: firmware, 
      force 
    })
    
    // Close modal - progress shown in job panel
    upgradeModal?.classList.add('hidden')
    updateJobBadge()
    
  } catch (e) {
    showToast('Failed to start upgrade: ' + e.message, 'error')
  } finally {
    btn.disabled = false
    btn.textContent = 'Upgrade'
  }
})

let upgradeMouseDownTarget = null
upgradeModal?.addEventListener('mousedown', e => {
  upgradeMouseDownTarget = e.target
})
upgradeModal?.addEventListener('click', e => {
  if (e.target === upgradeModal && upgradeMouseDownTarget === upgradeModal) {
    upgradeModal.classList.add('hidden')
  }
  upgradeMouseDownTarget = null
})

// Detail panel close - use closest() to handle clicks on button or its children
document.getElementById('detailPanel')?.addEventListener('click', e => {
  if (e.target.closest('.panel-close') || e.target.closest('.detail-close')) {
    e.stopPropagation()
    store.set({ selectedDevice: null })
    document.getElementById('detailPanel')?.classList.add('hidden')
    // Trigger resize so virtual table recalculates
    setTimeout(() => window.dispatchEvent(new Event('resize')), 50)
  }
})

// Network health check
let networkOk = true
async function checkNetwork() {
  const banner = document.getElementById('networkBanner')
  const result = await api.ping()
  
  // Auth failure is handled by api.ping() which will reload the page
  if (result.status === 401) return
  
  if (result.ok) {
    if (!networkOk) {
      networkOk = true
      banner?.classList.add('hidden')
      if (store.user) {
        const devices = await api.devices()
        store.set({ devices: Array.isArray(devices) ? devices : [] })
        renderTree()
        renderCurrentPage()
      }
    }
  } else {
    // Show appropriate error message
    let msg = 'Server not responding'
    if (result.network) {
      msg = 'Network connection unavailable'
    } else if (result.status === 429) {
      msg = 'Rate limit exceeded - too many requests'
    } else if (result.status === 503) {
      msg = 'Server unavailable - service may be restarting'
    } else if (result.error) {
      msg = result.error
    } else if (result.status) {
      msg = `Server error (${result.status})`
    }
    
    if (banner) {
      banner.textContent = msg
      banner.classList.remove('hidden')
    }
    networkOk = false
  }
}

// Poll for updates (fallback when WebSocket is stale or disconnected).
// IMPORTANT: This must not reset virtual scroll or steal focus on large installs.
async function pollDevices() {
  if (!store.user || !networkOk) return

  // If WebSocket is connected and we have seen realtime updates recently,
  // skip polling entirely to avoid disruptive full refreshes.
  const wsOpen = ws?.socket && ws.socket.readyState === WebSocket.OPEN
  const forceFullReconcile = !lastFullReconcileAt || (Date.now() - lastFullReconcileAt) >= FORCE_FULL_RECONCILE_MS
  if (!forceFullReconcile && wsOpen && lastRealtimeUpdateAt && (Date.now() - lastRealtimeUpdateAt) < 45000) {
    return
  }

  try {
    const devices = await api.devices()
    if (!Array.isArray(devices)) return

    const current = store.devices || []

    // First load - do a normal render.
    if (current.length === 0) {
      store.set({ devices })
      renderTree()
      if (['dashboard', 'devices'].includes(store.currentPage)) {
        renderCurrentPage()
      }
      updateCounts()
      lastFullReconcileAt = Date.now()
      scheduleAssociationsRefresh('pollDevices:firstload')
      return
    }

    // Merge by id to preserve object identity (prevents virtual scroll reset).
    const currentById = new Map(current.map(d => [d.id, d]))
    const seen = new Set()
    let added = 0
    let hierarchyChanged = false
    let needsReindex = false

    // For non-virtual table mode we may need to update specific rows.
    const rowPatches = []

    for (const d of devices) {
      if (!d || d.id === undefined || d.id === null) continue
      const c = currentById.get(d.id)
      if (!c) {
        added++
        continue
      }
      seen.add(d.id)

      // Detect hierarchy changes (tree).
      if ((c.parent_id || null) !== (d.parent_id || null) ||
          (c.hostname || '') !== (d.hostname || '') ||
          (c.site_name || '') !== (d.site_name || '')) {
        hierarchyChanged = true
      }

      // Build a minimal patch.
      // IMPORTANT: do NOT treat uptime-only drift as a reason to rebuild the whole list.
      const patch = {}
      const fields = [
        'online','status','status_reason','db_status','db_status_reason','last_error',
        'peer_count','temperature','cpu','ram',
        'signal_60ghz','capacity_60ghz','signal_5ghz','capacity_5ghz','signal_per_chain',
        'signal_ltu','capacity_ltu',
        'firmware','firmware_version',
        'ip_address','mac','product','model','ssid',
        'distance','tx_rate','rx_rate','link_score'
      ]
      for (const f of fields) {
        if (d[f] !== undefined && c[f] !== d[f]) {
          patch[f] = d[f]
          if (f === 'ip_address' || f === 'mac') needsReindex = true
        }
      }

      // Include nested objects when provided (detail panel correctness).
      if (d.radio_60ghz && c.radio_60ghz !== d.radio_60ghz) patch.radio_60ghz = d.radio_60ghz
      if (d.radio_5ghz && c.radio_5ghz !== d.radio_5ghz) patch.radio_5ghz = d.radio_5ghz
      if (d.radio_ltu && c.radio_ltu !== d.radio_ltu) patch.radio_ltu = d.radio_ltu
      if (d.config && c.config !== d.config) patch.config = d.config

      if (Object.keys(patch).length > 0) {
        Object.assign(c, patch)
        if (shouldUseVirtualTable()) {
          wsBatcher.add(c.id, patch)
        } else {
          rowPatches.push({ id: c.id, ip: c.ip_address, patch })
        }

        if (store.selectedDevice && c.id === store.selectedDevice) {
          updateDetailPanel(c)
        }
      }
    }

    const removed = current.length - seen.size

    // If the list membership changed, reconcile once (preserving existing objects where possible).
    if (added > 0 || removed > 0 || current.length !== devices.length) {
      const newList = []
      for (const d of devices) {
        if (!d || d.id === undefined || d.id === null) continue
        const c = currentById.get(d.id)
        newList.push(c || d)
      }

      // Preserve scroll + focused input during the one-time rebuild.
      const rerender = ['dashboard','devices'].includes(store.currentPage)
      let scrollTop = 0
      let activeId = null
      let selStart = null
      let selEnd = null
      if (rerender) {
        const virtualScroll = document.querySelector('.virtual-scroll-container')
        const tableWrapper = document.querySelector('.device-table-wrapper')
        scrollTop = virtualScroll?.scrollTop || tableWrapper?.scrollTop || 0
        const active = document.activeElement
        if (active && (active.tagName === 'INPUT' || active.tagName === 'TEXTAREA')) {
          activeId = active.id || null
          try { selStart = active.selectionStart; selEnd = active.selectionEnd } catch {}
        }
      }

      store.set({ devices: newList })
      if (hierarchyChanged || added > 0 || removed > 0) renderTree()
      if (rerender) {
        renderCurrentPage()
        const newVirtualScroll = document.querySelector('.virtual-scroll-container')
        const newWrapper = document.querySelector('.device-table-wrapper')
        if (newVirtualScroll && scrollTop > 0) newVirtualScroll.scrollTop = scrollTop
        else if (newWrapper && scrollTop > 0) newWrapper.scrollTop = scrollTop
        if (activeId) {
          const el = document.getElementById(activeId)
          if (el) {
            el.focus()
            if (selStart !== null && selEnd !== null) {
              try { el.setSelectionRange(selStart, selEnd) } catch {}
            }
          }
        }
      }
      updateCounts()
      lastFullReconcileAt = Date.now()
      scheduleAssociationsRefresh('pollDevices:reconcile')
      return
    }

    if (needsReindex) store.reindexDevices()

    if (!shouldUseVirtualTable() && rowPatches.length > 0) {
      rowPatches.forEach(rp => updateDeviceRow(rp.id, rp.ip, rp.patch))
    }

    // Tree only needs updating when hierarchy changes (rare).
    if (hierarchyChanged) renderTree()

    updateCounts()
    lastFullReconcileAt = Date.now()
    if (rowPatches.length > 0 || hierarchyChanged || forceFullReconcile) {
      scheduleAssociationsRefresh('pollDevices:patch')
    }
  } catch (e) {
    // Ignore poll errors
  }
}

// Start polling
checkNetwork()
setInterval(checkNetwork, 5000)
setInterval(pollDevices, 30000)
setInterval(runSyncWatchdog, AUTH_SYNC_CHECK_MS)

// ===== Context Menu =====
const contextMenu = document.getElementById('contextMenu')
let contextDeviceId = null
let contextTargetRow = null

function clearContextTarget() {
  if (contextTargetRow) {
    contextTargetRow.classList.remove('context-menu-target')
    contextTargetRow = null
  }
}

document.addEventListener('contextmenu', e => {
  const row = e.target.closest('tr[data-id]')
  if (!row || !contextMenu) return
  
  // Don't show context menu on drilldown page
  if (store.currentPage === 'drilldown') return
  
  e.preventDefault()
  
  // Clear previous target
  clearContextTarget()
  
  // Highlight the target row
  contextTargetRow = row
  row.classList.add('context-menu-target')
  
  contextDeviceId = parseInt(row.dataset.id)
  
  const device = store.devices.find(d => d.id === contextDeviceId)
  if (!device) return
  
  // Show/hide fanout option for APs only
  const fanoutItem = contextMenu.querySelector('[data-action="fanout"]')
  if (fanoutItem) {
    fanoutItem.style.display = device.parent_id ? 'none' : 'block'
  }

  // Show/hide ultra debug option for devices that have an IP (directly queried devices and
  // any devices we might directly target for actions like firmware upgrade).
  const ultraItem = contextMenu.querySelector('[data-action="ultra-debug"]')
  if (ultraItem) {
    const hasIP = !!device.ip_address
    ultraItem.style.display = hasIP ? 'block' : 'none'
    ultraItem.textContent = device.ultra_debug ? 'Disable Ultra Debug' : 'Enable Ultra Debug'
  }
  
  // Use clientX/Y for fixed positioning (viewport-relative)
  // Also check bounds so menu doesn't go off-screen
  let x = e.clientX
  let y = e.clientY
  
  // Temporarily show to measure
  contextMenu.style.visibility = 'hidden'
  contextMenu.classList.remove('hidden')
  const menuRect = contextMenu.getBoundingClientRect()
  
  // Adjust if menu would go off right edge
  if (x + menuRect.width > window.innerWidth) {
    x = window.innerWidth - menuRect.width - 5
  }
  // Adjust if menu would go off bottom edge
  if (y + menuRect.height > window.innerHeight) {
    y = window.innerHeight - menuRect.height - 5
  }
  
  contextMenu.style.left = x + 'px'
  contextMenu.style.top = y + 'px'
  contextMenu.style.visibility = 'visible'
})

document.addEventListener('click', () => {
  contextMenu?.classList.add('hidden')
  clearContextTarget()
})

document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && contextMenu && !contextMenu.classList.contains('hidden')) {
    contextMenu.classList.add('hidden')
    clearContextTarget()
  }
})

contextMenu?.querySelectorAll('.context-item').forEach(item => {
  item.addEventListener('click', async () => {
    if (!contextDeviceId) return
    
    const action = item.dataset.action
    const device = store.devices.find(d => d.id === contextDeviceId)
    if (!device) return
    
    switch (action) {
      case 'refresh':
        try {
          await api.refreshDevice(contextDeviceId)
          showToast('Refreshing...', 'info')
        } catch (e) {
          showToast('Refresh failed', 'error')
        }
        break
        
      case 'upgrade':
        if (window.showUpgradeModal) window.showUpgradeModal(contextDeviceId)
        break
        
      case 'fanout':
        if (window.showUpgradeModal) {
          window.showUpgradeModal(contextDeviceId)
          setTimeout(() => {
            const fanoutCb = document.getElementById('upgradeFanout')
            if (fanoutCb) fanoutCb.checked = true
          }, 100)
        }
        break
        
      case 'copy-ip':
        if (device.ip_address) {
          navigator.clipboard.writeText(device.ip_address)
          showToast('IP copied', 'success')
        }
        break
        
      case 'copy-mac':
        if (device.mac) {
          navigator.clipboard.writeText(device.mac)
          showToast('MAC copied', 'success')
        }
        break
        
      case 'copy-firmware':
        const fw = device.firmware_version || device.firmware || ''
        if (fw) {
          navigator.clipboard.writeText(fw)
          showToast('Firmware copied', 'success')
        } else {
          showToast('No firmware info', 'error')
        }
        break
        
      case 'backup':
        try {
          await startTrackedJob('backup', [contextDeviceId], {})
          showToast('Backup started', 'success')
          updateJobBadge()
        } catch (e) {
          showToast('Backup failed: ' + e.message, 'error')
        }
        break
        
      case 'restore':
        showToast('Select a backup file from the Config page', 'info')
        store.set({ currentPage: 'config' })
        renderCurrentPage()
        break
        
      case 'reboot':
        await window.rebootDevice(contextDeviceId)
        break
        
      case 'open-ui':
        if (device.ip_address) {
			window.open(`/api/wavecontrol/open-ui?device_id=${encodeURIComponent(device.id)}`, '_blank')
        }
        break

      case 'ultra-debug':
      // Ultra Debug requires an IP address so we can bind capture to a host
      if (!device.ip_address) {
        showToast('This device does not have an IP address to enable Ultra Debug.', 'error')
        break
      }
      const enable = !device.ultra_debug
      try {
        // Enable/disable for device ID (if present) and the host IP
        if (contextDeviceId) {
          await api.setUltraDebugDevice(contextDeviceId, enable)
        }
        await api.setUltraDebugHost(device.ip_address, enable)
        await refreshUltraDebugBadge()
        showToast(enable ? 'Ultra Debug enabled' : 'Ultra Debug disabled', 'success')
      } catch (err) {
        showToast('Failed to toggle Ultra Debug: ' + (err.message || err), 'error')
      }
      break
      case 'delete':
        if (confirm(`Delete ${device.hostname || device.ip_address}?`)) {
          try {
            await api.deleteDevice(contextDeviceId)
            const devices = await api.devices()
            store.set({ devices, selectedDevice: null })
            renderTree()
            renderCurrentPage()
            showToast('Device deleted', 'success')
          } catch (e) {
            showToast('Delete failed', 'error')
          }
        }
        break
    }
    
    contextMenu?.classList.add('hidden')
    clearContextTarget()
  })
})

// ===== Bulk Actions =====
const bulkToolbar = document.getElementById('bulkToolbar')

function getSelectedDeviceIds() {
  const checkboxes = document.querySelectorAll('.device-table tbody input[type="checkbox"]:checked')
  return Array.from(checkboxes).map(cb => parseInt(cb.dataset.id)).filter(id => !isNaN(id))
}

function updateBulkToolbar() {
  const selected = getSelectedDeviceIds()
  const count = selected.length
  
  if (count > 0) {
    bulkToolbar?.classList.remove('hidden')
    const countEl = document.getElementById('bulkSelectedCount')
    if (countEl) countEl.textContent = count
  } else {
    bulkToolbar?.classList.add('hidden')
  }
}

// Listen for checkbox changes
document.addEventListener('change', e => {
  if (e.target.matches('.device-table input[type="checkbox"]')) {
    updateBulkToolbar()
  }
})

document.getElementById('bulkCancel')?.addEventListener('click', () => {
  document.querySelectorAll('.device-table input[type="checkbox"]').forEach(cb => {
    cb.checked = false
  })
  bulkToolbar?.classList.add('hidden')
})

document.getElementById('bulkRefresh')?.addEventListener('click', async () => {
  const ids = getSelectedDeviceIds()
  if (ids.length === 0) return
  
  showToast(`Refreshing ${ids.length} devices...`, 'info')
  
  for (const id of ids) {
    try {
      await api.refreshDevice(id)
    } catch (e) {
      // Continue on error
    }
  }
  
  setTimeout(async () => {
    const devices = await api.devices()
    store.set({ devices })
    renderTree()
    renderCurrentPage()
    showToast('Refresh complete', 'success')
  }, 2000)
})

document.getElementById('bulkUpgrade')?.addEventListener('click', async () => {
  const ids = getSelectedDeviceIds()
  if (ids.length === 0) return
  
  // Get firmware list
  let firmware
  try {
    firmware = await api.firmwareList()
  } catch (e) {
    showToast('Failed to load firmware list', 'error')
    return
  }
  
  if (!firmware || firmware.length === 0) {
    showToast('No firmware files available', 'error')
    return
  }
  
  // For bulk, use auto-selection (empty firmware = auto-select by flavor)
  const force = confirm('Force upgrade even if same version?')
  
  try {
    // Use async job system for bulk upgrades
    await startTrackedJob('bulk_upgrade', ids, { force })
    updateJobBadge()
  } catch (e) {
    showToast('Failed to start bulk upgrade: ' + e.message, 'error')
  }
})

document.getElementById('bulkBackup')?.addEventListener('click', async () => {
  const ids = getSelectedDeviceIds()
  if (ids.length === 0) return
  
  try {
    await startTrackedJob('backup', ids, { include_config: true })
    updateJobBadge()
  } catch (e) {
    showToast('Failed to start backup: ' + e.message, 'error')
  }
})


document.getElementById('bulkMarkAlertable')?.addEventListener('click', async () => {
  const ids = getSelectedDeviceIds()
  if (ids.length === 0) return
  try {
    await api.bulkUpdateDeviceAlerting(ids, { alertable: true, clear_silence: true })
    const idSet = new Set(ids)
    store.devices.forEach(d => {
      if (idSet.has(d.id)) {
        d.alertable = true
        d.alert_silenced_until = null
      }
    })
    store.set({ devices: store.devices })
    renderCurrentPage()
    showToast(`Marked ${ids.length} device${ids.length === 1 ? '' : 's'} alertable`, 'success')
  } catch (e) {
    showToast('Bulk alerting update failed: ' + e.message, 'error')
  }
})

document.getElementById('bulkMuteAlerts')?.addEventListener('click', async () => {
  const ids = getSelectedDeviceIds()
  if (ids.length === 0) return
  const choice = window.prompt('Mute selected devices for how many hours? Use 0 to mark not alertable permanently.', '24')
  if (choice === null) return
  const hours = Number(choice)
  if (!Number.isFinite(hours) || hours < 0) {
    showToast('Enter a valid hour count', 'error')
    return
  }
  const patch = hours === 0 ? { alertable: false } : { silence_seconds: Math.round(hours * 3600) }
  try {
    const resp = await api.bulkUpdateDeviceAlerting(ids, patch)
    const resultDevices = (resp?.results || []).filter(r => r.ok && r.device).map(r => r.device)
    const byID = new Map(resultDevices.map(d => [d.id, d]))
    store.devices.forEach(d => {
      const upd = byID.get(d.id)
      if (upd) Object.assign(d, upd)
    })
    if (hours === 0) {
      const idSet = new Set(ids)
      store.devices.forEach(d => { if (idSet.has(d.id)) d.alertable = false })
    }
    store.set({ devices: store.devices })
    await refreshAlertNavBadge()
    renderCurrentPage()
    showToast(hours === 0 ? `Marked ${ids.length} device${ids.length === 1 ? '' : 's'} not alertable` : `Muted ${ids.length} device${ids.length === 1 ? '' : 's'} for ${hours}h`, 'success')
  } catch (e) {
    showToast('Bulk alerting update failed: ' + e.message, 'error')
  }
})

document.getElementById('bulkDelete')?.addEventListener('click', async () => {
  const ids = getSelectedDeviceIds()
  if (ids.length === 0) return
  
  if (!confirm(`Delete ${ids.length} devices? This cannot be undone.`)) return
  
  let deleted = 0
  for (const id of ids) {
    try {
      await api.deleteDevice(id)
      deleted++
    } catch (e) {
      // Continue on error
    }
  }
  
  const devices = await api.devices()
  store.set({ devices, selectedDevice: null })
  renderTree()
  renderCurrentPage()
  bulkToolbar?.classList.add('hidden')
  showToast(`Deleted ${deleted} devices`, 'success')
})

// ===== Scheduled Jobs =====
async function loadScheduledJobs(jobsOverride = null) {
  const container = document.getElementById('jobsList') || document.getElementById('scheduledJobs')
  if (!container) return

  try {
    const allJobs = Array.isArray(jobsOverride) ? jobsOverride : await api.jobs()

    // Cache jobs for details/edit (avoid refetch when filtering)
    window.__scheduledJobsAll = allJobs || []
    window.__scheduledJobsById = new Map((window.__scheduledJobsAll || []).map(j => [j.id, j]))

    if (!allJobs || allJobs.length === 0) {
      container.innerHTML = '<p class="empty-state">No scheduled jobs</p>'
      return
    }

    const statusFilter = String(document.getElementById('jobsStatusFilter')?.value || 'all').toLowerCase()
    const normStatus = (job) => String(job?.status || '').toLowerCase()
    const filterSet = (vals) => new Set(vals.map(v => String(v).toLowerCase()))
    const activeSet = filterSet(['pending', 'blocked', 'running'])
    const doneSet = filterSet(['completed', 'failed', 'cancelled'])

    let jobs = allJobs
    if (statusFilter === 'active') {
      jobs = allJobs.filter(j => activeSet.has(normStatus(j)))
    } else if (statusFilter === 'done') {
      jobs = allJobs.filter(j => doneSet.has(normStatus(j)))
    } else if (statusFilter !== 'all') {
      jobs = allJobs.filter(j => normStatus(j) === statusFilter)
    }

    if (!jobs || jobs.length === 0) {
      container.innerHTML = '<p class="empty-state">No jobs match this filter</p>'
      return
    }

    const isCancellable = (job) => ['pending', 'blocked', 'running'].includes(String(job.status || ''))
    const isEditable = (job) => ['pending', 'blocked'].includes(String(job.status || ''))

    container.innerHTML = `
      <table class="jobs-table">
        <thead>
          <tr>
            <th>ID</th>
            <th>Type</th>
            <th>Devices</th>
            <th>Scheduled</th>
            <th>Status</th>
            <th>Progress</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          ${jobs.map(job => `
            <tr class="${escapeAttr(job.status)}" data-job-id="${job.id}">
              <td>#${job.id}</td>
              <td>${escapeHTML(job.job_type)}</td>
              <td>${job.device_ids?.length || 0} devices</td>
              <td>
                <div class="job-sched-cell">
                  <span class="job-sched-time">${formatJobTime(job.scheduled_at, job.next_run)}</span>
                  ${job.repeat_cron ? '<span class="badge">Recurring</span>' : ''}
                  ${isEditable(job) ? `<a href="#" class="job-edit-inline" data-id="${job.id}" title="Edit schedule">Edit</a>` : ''}
                </div>
              </td>
              <td><span class="job-status ${escapeAttr(job.status)}">${escapeHTML(job.status)}</span></td>
              <td>
                ${job.status === 'running' ? `
                  <div class="progress-bar">
                    <div class="progress-fill" style="width: ${job.progress || 0}%"></div>
                    <span class="progress-text">${job.completed_devices || 0}/${job.total_devices || 0}</span>
                  </div>
                ` : (job.status === 'completed' ? '[ok]' : (job.status === 'failed' ? '[x]' : '-'))}
              </td>
              <td>
                <div class="cell-actions jobs-actions">
                  <button class="btn btn-secondary btn-sm btn-job-details" data-id="${job.id}" title="View details">Details</button>
                  ${isEditable(job) ? `<button class="btn btn-secondary btn-sm btn-edit-job" data-id="${job.id}" title="Edit schedule">Edit</button>` : ''}
                  ${isCancellable(job) ? `<button class="btn btn-danger-subtle btn-sm btn-cancel-job" data-id="${job.id}" title="Cancel job">Cancel</button>` : ''}
                </div>
              </td>
            </tr>
            ${job.error_message ? `<tr class="error-row"><td colspan="7" class="job-error">${escapeHTML(job.error_message)}</td></tr>` : ''}
          `).join('')}
        </tbody>
      </table>
    `

    // Row click opens details (but ignore clicks on buttons/links)
    container.querySelectorAll('tr[data-job-id]').forEach(row => {
      row.addEventListener('click', (e) => {
        if (e.target && (e.target.closest('button') || e.target.closest('a'))) return
        const jobId = parseInt(row.dataset.jobId)
        const job = window.__scheduledJobsById?.get(jobId)
        if (job) showJobDetailsModal(job)
      })
    })

    container.querySelectorAll('.btn-job-details').forEach(btn => {
      btn.addEventListener('click', () => {
        const jobId = parseInt(btn.dataset.id)
        const job = window.__scheduledJobsById?.get(jobId)
        if (job) showJobDetailsModal(job)
      })
    })

    container.querySelectorAll('.btn-edit-job').forEach(btn => {
      btn.addEventListener('click', () => {
        const jobId = parseInt(btn.dataset.id)
        const job = window.__scheduledJobsById?.get(jobId)
        if (job) showJobEditModal(job)
      })
    })

    container.querySelectorAll('.job-edit-inline').forEach(a => {
      a.addEventListener('click', (e) => {
        e.preventDefault()
        const jobId = parseInt(a.dataset.id)
        const job = window.__scheduledJobsById?.get(jobId)
        if (job) showJobEditModal(job)
      })
    })

    container.querySelectorAll('.btn-cancel-job').forEach(btn => {
      btn.addEventListener('click', async () => {
        const jobId = parseInt(btn.dataset.id)
        const job = window.__scheduledJobsById?.get(jobId)
        const status = String(job?.status || '')
        const msg = status === 'running'
          ? 'Cancel this running job? It will stop as soon as possible.'
          : 'Cancel this scheduled job?'
        if (!confirm(msg)) return

        try {
          await api.cancelJob(jobId)
          showToast('Job cancelled', 'success')
          loadScheduledJobs()
        } catch (e) {
          showToast('Failed: ' + e.message, 'error')
        }
      })
    })
  } catch (e) {
    container.innerHTML = `<p class="error">Error: ${escapeHTML(e.message)}</p>`
  }
}

function formatJobTime(scheduledAt, nextRun) {
  const time = nextRun || scheduledAt
  if (!time) return '-'
  const d = new Date(time)
  return d.toLocaleString()
}

function showJobDetailsModal(job) {
  if (!job) return

  let modal = document.getElementById('jobDetailsModal')
  if (!modal) {
    modal = document.createElement('div')
    modal.id = 'jobDetailsModal'
    modal.className = 'modal hidden'
    modal.innerHTML = `
      <div class="modal-content modal-wide">
        <div class="modal-header">
          <h3>Scheduled Job Details</h3>
          <button class="modal-close" aria-label="Close">&times;</button>
        </div>
        <div class="modal-body" id="jobDetailsBody"></div>
        <div class="modal-footer" id="jobDetailsFooter">
          <button class="btn btn-secondary" id="jobDetailsClose">Close</button>
        </div>
      </div>
    `
    document.body.appendChild(modal)

    // Close handlers
    modal.querySelector('.modal-close')?.addEventListener('click', () => modal.classList.add('hidden'))
    modal.querySelector('#jobDetailsClose')?.addEventListener('click', () => modal.classList.add('hidden'))
    modal.addEventListener('click', (e) => {
      if (e.target === modal) modal.classList.add('hidden')
    })
  }

  const isCancellable = ['pending', 'blocked', 'running'].includes(String(job.status || ''))
  const isEditable = ['pending', 'blocked'].includes(String(job.status || ''))

  // Pretty params
  let paramsText = ''
  try {
    const p = job.parameters ?? {}
    paramsText = JSON.stringify(p, null, 2)
  } catch {
    paramsText = String(job.parameters || '')
  }

  // Device list
  const ids = Array.isArray(job.device_ids) ? job.device_ids : []
  const deviceLines = ids.map(id => {
    const d = store.getDeviceById(id)
    const name = d?.hostname || d?.ip_address || d?.mac || `Device ${id}`
    const extra = []
    if (d?.ip_address) extra.push(d.ip_address)
    if (d?.product) extra.push(d.product)
    if (d?.site_name) extra.push(d.site_name)
    return `#${id} - ${name}${extra.length ? ' (' + extra.join(' · ') + ')' : ''}`
  })

  const body = modal.querySelector('#jobDetailsBody')
  if (body) {
    body.innerHTML = `
      <div class="job-details-grid">
        <div><strong>ID:</strong> #${escapeHTML(String(job.id))}</div>
        <div><strong>Type:</strong> ${escapeHTML(String(job.job_type || ''))}</div>
        <div><strong>Status:</strong> <span class="job-status ${escapeAttr(job.status)}">${escapeHTML(String(job.status || ''))}</span></div>
        <div><strong>Scheduled:</strong> ${escapeHTML(formatJobTime(job.scheduled_at, job.next_run))}</div>
        <div><strong>Repeat:</strong> ${job.repeat_cron ? escapeHTML(String(job.repeat_cron)) : '-'}</div>
        <div><strong>Created By:</strong> ${escapeHTML(String(job.created_by ?? '-'))}</div>
        <div><strong>Created At:</strong> ${job.created_at ? escapeHTML(new Date(job.created_at).toLocaleString()) : '-'}</div>
      </div>

      ${job.status === 'running' ? `
        <div class="job-details-progress">
          <div class="progress-bar">
            <div class="progress-fill" style="width: ${job.progress || 0}%"></div>
            <span class="progress-text">${job.completed_devices || 0}/${job.total_devices || 0}</span>
          </div>
        </div>
      ` : ''}

      ${job.error_message ? `<div class="job-details-error">${escapeHTML(String(job.error_message))}</div>` : ''}

      <h4>Targets</h4>
      <div class="job-details-targets">
        ${deviceLines.length ? `<pre class="job-details-pre">${escapeHTML(deviceLines.join('\n'))}</pre>` : '<p class="muted">No devices</p>'}
      </div>

      <h4>Parameters</h4>
      <pre class="job-details-pre">${escapeHTML(paramsText)}</pre>
    `
  }

  const footer = modal.querySelector('#jobDetailsFooter')
  if (footer) {
    footer.innerHTML = `
      <button class="btn btn-secondary" id="jobDetailsClose">Close</button>
      ${isEditable ? `<button class="btn btn-secondary" id="jobDetailsEdit">Edit</button>` : ''}
      ${isCancellable ? `<button class="btn btn-danger-subtle" id="jobDetailsCancel">Cancel Job</button>` : ''}
    `
    footer.querySelector('#jobDetailsEdit')?.addEventListener('click', () => {
      modal.classList.add('hidden')
      showJobEditModal(job)
    })

    footer.querySelector('#jobDetailsClose')?.addEventListener('click', () => modal.classList.add('hidden'))
    footer.querySelector('#jobDetailsCancel')?.addEventListener('click', async () => {
      const status = String(job.status || '')
      const msg = status === 'running'
        ? 'Cancel this running job? It will stop as soon as possible.'
        : 'Cancel this scheduled job?'
      if (!confirm(msg)) return
      try {
        await api.cancelJob(job.id)
        showToast('Job cancelled', 'success')
        modal.classList.add('hidden')
        loadScheduledJobs()
      } catch (e) {
        showToast('Failed: ' + e.message, 'error')
      }
    })
  }

  modal.classList.remove('hidden')
}

function toDatetimeLocalValue(d) {
  if (!d) return ''
  const date = (d instanceof Date) ? d : new Date(d)
  if (isNaN(date.getTime())) return ''
  const pad = (n) => String(n).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}

function showJobEditModal(job) {
  if (!job) return

  let modal = document.getElementById('jobEditModal')
  if (!modal) {
    modal = document.createElement('div')
    modal.id = 'jobEditModal'
    modal.className = 'modal hidden'
    modal.innerHTML = `
      <div class="modal-content modal-wide">
        <div class="modal-header">
          <h3>Edit Scheduled Job</h3>
          <button class="modal-close" aria-label="Close">&times;</button>
        </div>
        <div class="modal-body" id="jobEditBody"></div>
        <div class="modal-footer" id="jobEditFooter"></div>
      </div>
    `
    document.body.appendChild(modal)
    modal.querySelector('.modal-close')?.addEventListener('click', () => modal.classList.add('hidden'))
    modal.addEventListener('click', (e) => {
      if (e.target === modal) modal.classList.add('hidden')
    })
  }

  const nextTime = job.next_run || job.scheduled_at
  const currentRepeat = job.repeat_cron || ''
  const common = ['', '@hourly', '@daily', '@weekly', '12h', '24h', '168h']
  const isCommon = common.includes(currentRepeat)

  const body = modal.querySelector('#jobEditBody')
  if (body) {
    body.innerHTML = `
      <div class="form-group">
        <label>Job</label>
        <div class="muted">#${escapeHTML(String(job.id))} • ${escapeHTML(String(job.job_type || ''))} • ${escapeHTML(String(job.status || ''))}</div>
      </div>
      <div class="form-group">
        <label>Next Run Time</label>
        <input type="datetime-local" id="jobEditTime" />
        <small class="form-hint">This sets the next time the job will run.</small>
      </div>
      <div class="form-group">
        <label>Repeat Schedule</label>
        <select id="jobEditRepeat">
          <option value="">One-time (no repeat)</option>
          <option value="@hourly">Hourly</option>
          <option value="@daily">Daily</option>
          <option value="@weekly">Weekly</option>
          <option value="12h">Every 12 hours</option>
          <option value="24h">Every 24 hours</option>
          <option value="168h">Every week</option>
          <option value="custom">Custom…</option>
        </select>
        <input type="text" id="jobEditRepeatCustom" class="hidden" placeholder="e.g. 6h, 30m" />
        <small class="form-hint">Use @hourly/@daily/@weekly or a Go duration like 12h, 30m.</small>
      </div>
    `

    const timeEl = body.querySelector('#jobEditTime')
    if (timeEl) timeEl.value = toDatetimeLocalValue(nextTime)

    const sel = body.querySelector('#jobEditRepeat')
    const custom = body.querySelector('#jobEditRepeatCustom')
    if (sel && custom) {
      if (currentRepeat && !isCommon) {
        sel.value = 'custom'
        custom.value = currentRepeat
        custom.classList.remove('hidden')
      } else {
        sel.value = currentRepeat
        custom.classList.add('hidden')
        custom.value = ''
      }

      sel.addEventListener('change', () => {
        if (sel.value === 'custom') {
          custom.classList.remove('hidden')
          custom.focus()
        } else {
          custom.classList.add('hidden')
        }
      })
    }
  }

  const footer = modal.querySelector('#jobEditFooter')
  if (footer) {
    footer.innerHTML = `
      <button class="btn btn-secondary" id="jobEditCancel">Cancel</button>
      <button class="btn btn-primary" id="jobEditSave">Save</button>
    `

    footer.querySelector('#jobEditCancel')?.addEventListener('click', () => modal.classList.add('hidden'))
    footer.querySelector('#jobEditSave')?.addEventListener('click', async () => {
      const timeVal = modal.querySelector('#jobEditTime')?.value
      if (!timeVal) {
        showToast('Select a next run time', 'error')
        return
      }

      const repeatSel = modal.querySelector('#jobEditRepeat')?.value || ''
      const repeatCustom = modal.querySelector('#jobEditRepeatCustom')?.value || ''
      const repeat = (repeatSel === 'custom') ? repeatCustom.trim() : repeatSel

      try {
        const iso = new Date(timeVal).toISOString()
        await api.updateJob(job.id, { scheduled_at: iso, repeat_cron: repeat })
        showToast('Job updated', 'success')
        modal.classList.add('hidden')
        await loadScheduledJobs()
      } catch (e) {
        showToast('Failed: ' + e.message, 'error')
      }
    })
  }

  modal.classList.remove('hidden')
}

function showScheduleJobModal() {
  // Create modal if it doesn't exist
  let modal = document.getElementById('scheduleJobModal')
  if (!modal) {
    modal = document.createElement('div')
    modal.id = 'scheduleJobModal'
    modal.className = 'modal'
    modal.innerHTML = `
      <div class="modal-content">
        <div class="modal-header">
          <h3>Schedule Job</h3>
          <button class="modal-close" data-wc-action="close-modal" data-modal-id="scheduleJobModal">&times;</button>
        </div>
        <div class="modal-body">
          <div class="form-group">
            <label>Job Type</label>
            <select id="schedJobType">
              <option value="upgrade">Firmware Upgrade</option>
              <option value="reboot">Reboot</option>
            </select>
          </div>
          <div class="form-group">
            <label>Devices</label>
            <select id="schedDevices" multiple size="8"></select>
            <small>Hold Ctrl/Cmd to select multiple</small>
          </div>
          <div class="form-group">
            <label>Schedule Time</label>
            <input type="datetime-local" id="schedTime" />
          </div>
          <div class="form-group">
            <label>Repeat</label>
            <select id="schedRepeat">
              <option value="">One-time</option>
              <option value="@hourly">Hourly</option>
              <option value="@daily">Daily</option>
              <option value="@weekly">Weekly</option>
            </select>
          </div>
          <div id="schedUpgradeOptions">
            <div class="form-check">
              <input type="checkbox" id="schedForce" />
              <label for="schedForce">Force upgrade</label>
            </div>
            <div class="form-check">
              <input type="checkbox" id="schedFanout" />
              <label for="schedFanout">Fanout (include connected STAs)</label>
            </div>
          </div>
        </div>
        <div class="modal-footer">
          <button class="btn btn-secondary" data-wc-action="close-modal" data-modal-id="scheduleJobModal">Cancel</button>
          <button class="btn btn-primary" id="confirmScheduleJob">Schedule</button>
        </div>
      </div>
    `
    document.body.appendChild(modal)
    
    // Populate device list
    const select = modal.querySelector('#schedDevices')
    store.devices.forEach(d => {
      const opt = document.createElement('option')
      opt.value = d.id
      opt.textContent = `${d.hostname || d.ip_address} (${d.product || 'Unknown'})`
      select.appendChild(opt)
    })
    
    // Set default time to now + 1 hour
    const now = new Date()
    now.setHours(now.getHours() + 1)
    now.setMinutes(0)
    modal.querySelector('#schedTime').value = now.toISOString().slice(0, 16)
    
    // Toggle upgrade options
    modal.querySelector('#schedJobType').addEventListener('change', e => {
      modal.querySelector('#schedUpgradeOptions').style.display = 
        e.target.value === 'upgrade' ? 'block' : 'none'
    })
    
    // Handle submit
    modal.querySelector('#confirmScheduleJob').addEventListener('click', async () => {
      const jobType = modal.querySelector('#schedJobType').value
      const deviceSelect = modal.querySelector('#schedDevices')
      const deviceIds = Array.from(deviceSelect.selectedOptions).map(o => parseInt(o.value))
      const schedTime = modal.querySelector('#schedTime').value
      const repeat = modal.querySelector('#schedRepeat').value
      const force = modal.querySelector('#schedForce').checked
      const fanout = modal.querySelector('#schedFanout').checked
      
      if (deviceIds.length === 0) {
        showToast('Select at least one device', 'error')
        return
      }
      
      const params = jobType === 'upgrade' ? { force, fanout } : {}
      
      try {
        await api.createJob(jobType, deviceIds, params, new Date(schedTime).toISOString(), repeat)
        showToast('Job scheduled', 'success')
        modal.classList.add('hidden')
        loadScheduledJobs()
      } catch (e) {
        showToast('Failed: ' + e.message, 'error')
      }
    })
  }
  
  modal.classList.remove('hidden')
}

// ===== Maintenance Windows =====
async function loadMaintenanceWindows() {
  const container = document.getElementById('maintenanceList')
  if (!container) return
  
  try {
    const windows = await api.get('/maintenance-windows')
    
    if (!windows || windows.length === 0) {
      container.innerHTML = '<p class="empty-state">No maintenance windows configured. Jobs will run anytime.</p>'
      return
    }
    
    const dayNames = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']
    
    container.innerHTML = `
      <table class="data-table">
        <thead>
          <tr>
            <th>Name</th>
            <th>Scope</th>
            <th>Days</th>
            <th>Time</th>
            <th>Job Types</th>
            <th>Status</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          ${windows.map(mw => `
            <tr>
              <td>${escapeHTML(mw.name)}</td>
              <td>${escapeHTML(mw.scope)}${mw.region_id ? ' (Region ' + mw.region_id + ')' : ''}${mw.site_id ? ' (Site ' + mw.site_id + ')' : ''}</td>
              <td>${mw.day_of_week?.length ? mw.day_of_week.map(d => dayNames[d]).join(', ') : 'Any'}</td>
              <td>${escapeHTML(mw.start_time)} - ${escapeHTML(mw.end_time)} ${escapeHTML(mw.timezone)}</td>
              <td>${mw.allow_jobs?.join(', ') || 'All'}</td>
              <td><span class="badge ${mw.enabled ? 'success' : 'muted'}">${mw.enabled ? 'Active' : 'Disabled'}</span></td>
              <td>
                <button class="btn btn-sm btn-edit-mw" data-id="${mw.id}">Edit</button>
                <button class="btn btn-sm btn-danger-subtle btn-delete-mw" data-id="${mw.id}" title="Delete">🗑</button>
              </td>
            </tr>
          `).join('')}
        </tbody>
      </table>
    `
    
    // Delete handlers
    container.querySelectorAll('.btn-delete-mw').forEach(btn => {
      btn.addEventListener('click', async () => {
        if (!confirm('Delete this maintenance window?')) return
        try {
          await api.delete('/maintenance-windows/' + btn.dataset.id)
          showToast('Maintenance window deleted', 'success')
          loadMaintenanceWindows()
        } catch (e) {
          showToast('Failed: ' + e.message, 'error')
        }
      })
    })
    
  } catch (e) {
    container.innerHTML = `<p class="error">Error: ${escapeHTML(e.message)}</p>`
  }
}

function showMaintenanceWindowModal(editData = null) {
  let modal = document.getElementById('maintenanceModal')
  if (!modal) {
    modal = document.createElement('div')
    modal.id = 'maintenanceModal'
    modal.className = 'modal'
    modal.innerHTML = `
      <div class="modal-content">
        <div class="modal-header">
          <h3>${editData ? 'Edit' : 'Add'} Maintenance Window</h3>
          <button class="modal-close" data-wc-action="close-modal" data-modal-id="maintenanceModal">&times;</button>
        </div>
        <div class="modal-body">
          <div class="form-group">
            <label>Name</label>
            <input type="text" id="mwName" placeholder="e.g., Nightly Maintenance" />
          </div>
          <div class="form-group">
            <label>Scope</label>
            <select id="mwScope">
              <option value="global">Global (all devices)</option>
              <option value="region">Region</option>
              <option value="site">Site</option>
            </select>
          </div>
          <div class="form-group hidden" id="mwRegionGroup">
            <label>Region</label>
            <select id="mwRegion"></select>
          </div>
          <div class="form-group hidden" id="mwSiteGroup">
            <label>Site</label>
            <select id="mwSite"></select>
          </div>
          <div class="form-group">
            <label>Days of Week</label>
            <div class="day-picker" id="mwDays">
              <label><input type="checkbox" value="0" /> Sun</label>
              <label><input type="checkbox" value="1" /> Mon</label>
              <label><input type="checkbox" value="2" /> Tue</label>
              <label><input type="checkbox" value="3" /> Wed</label>
              <label><input type="checkbox" value="4" /> Thu</label>
              <label><input type="checkbox" value="5" /> Fri</label>
              <label><input type="checkbox" value="6" /> Sat</label>
            </div>
            <small>Leave unchecked for all days</small>
          </div>
          <div class="form-row">
            <div class="form-group">
              <label>Start Time</label>
              <input type="time" id="mwStartTime" value="02:00" />
            </div>
            <div class="form-group">
              <label>End Time</label>
              <input type="time" id="mwEndTime" value="06:00" />
            </div>
            <div class="form-group">
              <label>Timezone</label>
              <select id="mwTimezone">
                <option value="UTC">UTC</option>
                <option value="America/New_York">Eastern</option>
                <option value="America/Chicago">Central</option>
                <option value="America/Denver">Mountain</option>
                <option value="America/Los_Angeles">Pacific</option>
              </select>
            </div>
          </div>
          <div class="form-group">
            <label>Allowed Job Types</label>
            <div class="job-type-picker">
              <label><input type="checkbox" id="mwAllowUpgrade" checked /> Upgrade</label>
              <label><input type="checkbox" id="mwAllowReboot" checked /> Reboot</label>
            </div>
          </div>
          <div class="form-check">
            <input type="checkbox" id="mwEnabled" checked />
            <label for="mwEnabled">Enabled</label>
          </div>
        </div>
        <div class="modal-footer">
          <button class="btn btn-secondary" data-wc-action="close-modal" data-modal-id="maintenanceModal">Cancel</button>
          <button class="btn btn-primary" id="saveMaintenance">Save</button>
        </div>
      </div>
    `
    document.body.appendChild(modal)
    
    // Scope change handler
    document.getElementById('mwScope').addEventListener('change', (e) => {
      document.getElementById('mwRegionGroup').classList.toggle('hidden', e.target.value !== 'region')
      document.getElementById('mwSiteGroup').classList.toggle('hidden', e.target.value !== 'site')
    })
    
    // Save handler
    document.getElementById('saveMaintenance').addEventListener('click', async () => {
      const days = []
      document.querySelectorAll('#mwDays input:checked').forEach(cb => days.push(parseInt(cb.value)))
      
      const allowJobs = []
      if (document.getElementById('mwAllowUpgrade').checked) allowJobs.push('upgrade')
      if (document.getElementById('mwAllowReboot').checked) allowJobs.push('reboot')
      
      const data = {
        name: document.getElementById('mwName').value,
        scope: document.getElementById('mwScope').value,
        region_id: document.getElementById('mwScope').value === 'region' ? parseInt(document.getElementById('mwRegion').value) : null,
        site_id: document.getElementById('mwScope').value === 'site' ? parseInt(document.getElementById('mwSite').value) : null,
        day_of_week: days,
        start_time: document.getElementById('mwStartTime').value,
        end_time: document.getElementById('mwEndTime').value,
        timezone: document.getElementById('mwTimezone').value,
        allow_jobs: allowJobs,
        enabled: document.getElementById('mwEnabled').checked
      }
      
      try {
        await api.post('/maintenance-windows', data)
        showToast('Maintenance window saved', 'success')
        modal.classList.add('hidden')
        loadMaintenanceWindows()
      } catch (e) {
        showToast('Failed: ' + e.message, 'error')
      }
    })
  }
  
  modal.classList.remove('hidden')
}

// ===== Scheduler Settings =====
async function loadSchedulerSettings() {
  const container = document.getElementById('schedulerSettings')
  if (!container) return
  
  try {
    const settings = await api.get('/scheduler/settings')
    
    container.innerHTML = `
      <div class="form-group">
        <label>Max Concurrent Jobs</label>
        <input type="number" id="maxConcurrent" min="1" max="50" value="${settings.max_concurrent_jobs || 5}" />
        <small>Maximum number of jobs that can run simultaneously (1-50)</small>
      </div>
      <div class="form-group">
        <label>Check Interval (seconds)</label>
        <input type="number" id="checkInterval" min="5" max="300" value="${settings.check_interval_sec || 10}" />
        <small>How often to check for due jobs (5-300 seconds)</small>
      </div>
      <div class="form-check">
        <input type="checkbox" id="respectMaintenance" ${settings.respect_maintenance ? 'checked' : ''} />
        <label for="respectMaintenance">Respect Maintenance Windows</label>
        <small>When enabled, jobs only run during configured maintenance windows</small>
      </div>
      <button class="btn btn-primary" id="saveSchedulerSettings">Save Settings</button>
    `
    
    document.getElementById('saveSchedulerSettings').addEventListener('click', async () => {
      const data = {
        max_concurrent: parseInt(document.getElementById('maxConcurrent').value),
        check_interval_sec: parseInt(document.getElementById('checkInterval').value),
        respect_maintenance: document.getElementById('respectMaintenance').checked
      }
      
      try {
        await api.patch('/scheduler/settings', data)
        showToast('Scheduler settings saved', 'success')
      } catch (e) {
        showToast('Failed: ' + e.message, 'error')
      }
    })
    
  } catch (e) {
    container.innerHTML = `<p class="error">Error: ${escapeHTML(e.message)}</p>`
  }
}

// Refresh alert badge periodically; alerts are server-authoritative and may fire between websocket UI updates.
setInterval(() => {
  if (store.user) refreshAlertNavBadge()
}, 60000)

// Initialize
init()
