// Simple reactive store for waveControl

const listeners = []

// Indexes for O(1) updates on large datasets (avoid O(n) array scans per WebSocket message)
let deviceByIp = new Map()   // ip_address -> device object (fallback only)
let deviceById = new Map()   // id -> device object (preferred)
let deviceByMac = new Map()  // mac (lowercase) -> device object (preferred when id missing)

let childrenByParentId = new Map() // parent_id -> [STA devices]
let childIndexDirty = true

function rebuildDeviceIndexes(devices) {
  deviceByIp = new Map()
  deviceById = new Map()
  deviceByMac = new Map()
  childrenByParentId = new Map()
  childIndexDirty = false

  if (!Array.isArray(devices)) return

  for (const d of devices) {
    if (d && d.ip_address) deviceByIp.set(d.ip_address, d)
    if (d && d.id !== undefined && d.id !== null) deviceById.set(d.id, d)
    if (d && d.mac) deviceByMac.set(String(d.mac).toLowerCase(), d)

    if (d && d.parent_id) {
      const pid = d.parent_id
      let kids = childrenByParentId.get(pid)
      if (!kids) {
        kids = []
        childrenByParentId.set(pid, kids)
      }
      kids.push(d)
    }
  }
}

function rebuildChildIndex(devices) {
  childrenByParentId = new Map()
  childIndexDirty = false
  if (!Array.isArray(devices)) return
  for (const d of devices) {
    // Managed devices (added via Add IP / Bulk) should appear at the root level,
    // even if they have a parent_id (i.e. they are STAs).
    if (d && d.parent_id && !d.managed) {
      const pid = d.parent_id
      let kids = childrenByParentId.get(pid)
      if (!kids) {
        kids = []
        childrenByParentId.set(pid, kids)
      }
      kids.push(d)
    }
  }
}


// Default visible columns

const DASHBOARD_SCOPE_STORAGE_KEY = 'wavecontrol.dashboardExclusions.v1'

function defaultDashboardExclusions() {
  return {
    types: {
      airmax: false,
      airfiber: false,
      ltu: false,
      wave60: false,
      waveMlo5: false,
      waveMlo6: false
    },
    bands: {
      band2: false,
      band3: false,
      band5: false,
      band6: false,
      band11: false,
      band16: false,
      band24: false,
      band60: false
    }
  }
}

function loadDashboardExclusions() {
  try {
    const raw = localStorage.getItem(DASHBOARD_SCOPE_STORAGE_KEY)
    if (!raw) return defaultDashboardExclusions()
    const parsed = JSON.parse(raw)
    const defaults = defaultDashboardExclusions()
    return {
      types: { ...defaults.types, ...(parsed?.types || {}) },
      bands: { ...defaults.bands, ...(parsed?.bands || {}) }
    }
  } catch (_) {
    return defaultDashboardExclusions()
  }
}

function saveDashboardExclusions(exclusions) {
  try {
    localStorage.setItem(DASHBOARD_SCOPE_STORAGE_KEY, JSON.stringify(exclusions))
  } catch (_) {}
}

function getBandKeysFromFrequency(freqMHz) {
  const bands = []
  const f = Number(freqMHz || 0)
  if (!f) return bands
  if (f >= 57000) bands.push('band60')
  else if (f >= 5925) bands.push('band6')
  else if (f >= 23000) bands.push('band24')
  else if (f >= 15000) bands.push('band16')
  else if (f >= 10500) bands.push('band11')
  else if (f >= 4900) bands.push('band5')
  else if (f >= 2900) bands.push('band3')
  else if (f >= 1800) bands.push('band2')
  return bands
}

function detectDeviceType(device) {
  const product = String(device?.product || '').toLowerCase()
  const model = String(device?.model || '').toLowerCase()
  const platform = String(device?.platform || '').toLowerCase()
  const flavor = String(device?.flavor || '').toLowerCase()
  const text = [product, model, platform, flavor].join(' ')

  if (text.includes('mlo6')) return 'waveMlo6'
  if (text.includes('mlo5')) return 'waveMlo5'
  if (text.includes('airfiber') || /af(2|3|5|11|24|24hd|5u|5x|5xhd)/.test(text) || text.includes('11fx')) return 'airfiber'
  if (text.includes('ltu')) return 'ltu'
  if (text.includes('wave')) return 'wave60'
  if (text.includes('airmax') || /(rocket|powerbeam|nanobeam|litebeam|prism|nanostation|airgrid|loco)/.test(text) || flavor === 'xc' || flavor === 'wa') return 'airmax'
  return ''
}

function getDeviceBands(device) {
  const set = new Set()
  const addRadioBand = (radio) => {
    if (!radio) return
    const override = String(radio.display_band_override || '').toLowerCase()
    if (override.includes('5')) set.add('band5')
    for (const key of getBandKeysFromFrequency(radio.frequency)) set.add(key)
  }
  addRadioBand(device?.radio_60ghz)
  addRadioBand(device?.radio_6ghz)
  addRadioBand(device?.radio_5ghz)
  addRadioBand(device?.radio_ltu)
  const product = String(device?.product || '').toLowerCase()
  const model = String(device?.model || '').toLowerCase()
  const text = `${product} ${model}`
  if (set.size === 0) {
    if (/24|24hd|airfiber 24/.test(text)) set.add('band24')
    if (/16|airfiber 16/.test(text)) set.add('band16')
    if (/11|11fx|airfiber 11/.test(text)) set.add('band11')
    if (/3|3ghz|airfiber 3/.test(text)) set.add('band3')
    if (/2|2\.4|2ghz|900/.test(text)) set.add('band2')
    if (/5ac|5x|5u|5ghz|5 g|rocket|powerbeam|nanobeam|litebeam|mlo5/.test(text)) set.add('band5')
    if (/wave|60ghz|60 g/.test(text) && !text.includes('mlo')) set.add('band60')
    if (/mlo6/.test(text)) set.add('band6')
  }
  return set
}

function hasActiveDashboardExclusions(exclusions) {
  const ex = exclusions || defaultDashboardExclusions()
  return Object.values(ex.types || {}).some(Boolean) || Object.values(ex.bands || {}).some(Boolean)
}

function shouldApplyDashboardExclusions() {
  return ['dashboard', 'devices'].includes(state.currentPage) && hasActiveDashboardExclusions(state.dashboardExclusions)
}

function isExcludedFromDashboard(device, exclusions = state.dashboardExclusions) {
  if (!device) return false
  const ex = exclusions || defaultDashboardExclusions()
  const typeKey = detectDeviceType(device)
  if (typeKey && ex.types?.[typeKey]) return true
  const bands = getDeviceBands(device)
  for (const band of bands) {
    if (ex.bands?.[band]) return true
  }
  return false
}

function getDashboardScopedDevices(devices = state.devices) {
  const list = Array.isArray(devices) ? devices : []
  if (!shouldApplyDashboardExclusions()) return list
  return list.filter(d => !isExcludedFromDashboard(d))
}


const defaultColumns = {
  status: true,
  name: true,
  ip: true,
  mac: false,  // Hidden by default, but searchable
  product: true,
  site: true,
  signal60: true,   // AP 60GHz
  sta60: false,     // STA 60GHz - what STA receives from AP
  signal5: true,      // AP 5GHz (MRC) - what AP receives from STA
  signal5c0: false,   // AP 5GHz Chain 0
  signal5c1: false,   // AP 5GHz Chain 1
  sta5: false,        // STA 5GHz (MRC) - what STA receives from AP
  sta5c0: false,      // STA 5GHz Chain 0
  sta5c1: false,      // STA 5GHz Chain 1
  dir: false,         // Directional RF diagnosis (DL/UL)
  health: false,      // Signal quality bars
  distance: true,
  capacity: true,
  firmware: true
}

let state = {
  user: null,
  settings: {},
  devices: [],
  devicesVersion: 0, // Increments when devices array changes
  selectedDevice: null,
  currentPage: 'dashboard',
  filters: {
    online: true,
    offline: true,
    unknown: true
  },
  searchQuery: '',
  treeFilter: '', // Filter for tree and map
  mismatchesFilter: '',
  mismatchExpanded: {},
  treeExpanded: {}, // { deviceId: true/false }
  sortColumn: null, // null = hierarchical/tree view
  sortDirection: 'asc',
  groupBy: null, // null, 'site', 'region'
  columns: { ...defaultColumns }, // visible columns
  dashboardExclusions: loadDashboardExclusions(),
  
  // Drilldown lists
  drilldownLists: [],
  selectedDrilldownList: 'default',
  editingDrilldownListId: null
}

function getStatus(device) {
  // Prefer computed/live status (server may mark STA offline when not associated)
  if (device.status) return device.status

  // Fallback to DB status when we don't have a computed status
  const dbStatus = device.db_status

  if (device.online === true) return 'online'
  if (device.online === false) {
    // If DB explicitly says unknown, keep unknown; otherwise treat as offline.
    if (dbStatus === 'unknown') return 'unknown'
    return 'offline'
  }

  return dbStatus || 'unknown'
}

function getStatusReason(device) {
  // Prefer computed status_reason, then DB status reason, then last_error.
  return device.status_reason || device.db_status_reason || device.last_error || '';
}

export const store = {
  get user() { return state.user },
  get devices() { return state.devices },
  get devicesVersion() { return state.devicesVersion },
  get selectedDevice() { return state.selectedDevice },
  get currentPage() { return state.currentPage },
  get filters() { return state.filters },
  get searchQuery() { return state.searchQuery },
  get treeExpanded() { return state.treeExpanded },
  get sortColumn() { return state.sortColumn },
  get sortDirection() { return state.sortDirection },
  get groupBy() { return state.groupBy },
  get columns() { return state.columns },
  get drilldownLists() { return state.drilldownLists },
  get selectedDrilldownList() { return state.selectedDrilldownList },
  get editingDrilldownListId() { return state.editingDrilldownListId },
  
  // Setters for drilldown (for direct assignment)
  set drilldownLists(val) { state.drilldownLists = val },
  set selectedDrilldownList(val) { state.selectedDrilldownList = val },
  set editingDrilldownListId(val) { state.editingDrilldownListId = val },
  
  getStatus,
  getStatusReason,

  // Fast lookup helpers (used by WebSocket handlers)
  getDeviceByIp(ip) { return deviceByIp.get(ip) },
  getDeviceById(id) { return deviceById.get(id) },
  getDeviceByMac(mac) { return deviceByMac.get(String(mac || '').toLowerCase()) },
  reindexDevices() { rebuildDeviceIndexes(state.devices) },
  invalidateChildIndex() { childIndexDirty = true },
  
  set(updates) {
    const next = { ...state, ...updates }
    // Increment version when devices array changes
    if (updates.devices !== undefined) {
      next.devicesVersion = state.devicesVersion + 1
      rebuildDeviceIndexes(next.devices)
    }
    state = next
    if (updates.dashboardExclusions !== undefined) {
      saveDashboardExclusions(state.dashboardExclusions)
    }
    listeners.forEach(fn => fn(state))
  },
  
  toggleColumn(col) {
    const columns = { ...state.columns }
    columns[col] = !columns[col]
    state = { ...state, columns }
    listeners.forEach(fn => fn(state))
  },
  
  on(fn) {
    listeners.push(fn)
    return () => {
      const i = listeners.indexOf(fn)
      if (i >= 0) listeners.splice(i, 1)
    }
  },
  
  // Alias for on() - subscribe with oldState tracking
  subscribe(fn) {
    let oldState = { ...state }
    return this.on((newState) => {
      fn(newState, oldState)
      oldState = { ...newState }
    })
  },
  
  
  // Backwards-compatible helpers used by some UI code
  getState() {
    return state;
  },
  
  setState(partial) {
    this.set(partial);
  },

setDashboardExclusion(group, key, value) {
  const dashboardExclusions = {
    ...state.dashboardExclusions,
    [group]: {
      ...(state.dashboardExclusions?.[group] || {}),
      [key]: !!value
    }
  }
  this.set({ dashboardExclusions })
},

resetDashboardExclusions() {
  this.set({ dashboardExclusions: defaultDashboardExclusions() })
},

get dashboardExclusionCount() {
  const ex = state.dashboardExclusions || defaultDashboardExclusions()
  return [...Object.values(ex.types || {}), ...Object.values(ex.bands || {})].filter(Boolean).length
},

isExcludedFromDashboard(device) {
  return isExcludedFromDashboard(device)
},

get dashboardScopedDevices() {
  return getDashboardScopedDevices(state.devices)
},
  
  // Helper: Get top-level devices for the dashboard tree.
  //
  // Normally, APs are top-level (parent_id is null) and STAs are nested.
  // If a device is explicitly managed (added via Add IP / Add Bulk),
  // we keep it top-level even if it has a parent_id.
  get aps() {
    return getDashboardScopedDevices(state.devices).filter(d => !d.parent_id || d.managed)
  },
  
  // Helper: Get STAs for an AP
  getSTAs(apId) {
    if (childIndexDirty) rebuildChildIndex(state.devices)
    const kids = childrenByParentId.get(apId) || []
    return shouldApplyDashboardExclusions() ? kids.filter(d => !isExcludedFromDashboard(d)) : kids
  },
  
  // Helper: Get filtered devices based on status filters
  get filteredDevices() {
    const scoped = getDashboardScopedDevices(state.devices)
    return scoped.filter(d => {
      const status = getStatus(d)
      if (status === 'online' && !state.filters.online) return false
      if (status === 'offline' && !state.filters.offline) return false
      if (status === 'unknown' && !state.filters.unknown) return false
      return true
    })
  },
  
  // Helper: Count devices by status
  get counts() {
    const counts = { online: 0, offline: 0, unknown: 0 }
    const devices = getDashboardScopedDevices(state.devices)
    devices.forEach(d => {
      const status = getStatus(d)
      if (status === 'online') counts.online++
      else if (status === 'offline') counts.offline++
      else counts.unknown++
    })
    return counts
  },
  
  // Toggle tree node expansion
  toggleExpanded(deviceId) {
    const expanded = { ...state.treeExpanded }
    expanded[deviceId] = expanded[deviceId] === false ? true : false
    state = { ...state, treeExpanded: expanded }
    listeners.forEach(fn => fn(state))
  },
  
  setExpanded(deviceId, value) {
    const expanded = { ...state.treeExpanded }
    expanded[deviceId] = value
    state = { ...state, treeExpanded: expanded }
    listeners.forEach(fn => fn(state))
  }
}