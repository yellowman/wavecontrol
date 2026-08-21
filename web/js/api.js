// API client for waveControl. Authentication is held exclusively in an
// HttpOnly, SameSite cookie set by the server; JavaScript never receives or
// stores a bearer token.

const BASE = '/api/wavecontrol'
const CSRF_HEADER = 'X-WaveControl-CSRF'

let sessionAuthenticated = false

function authError(message = 'Unauthorized') {
  const error = new Error(message)
  error.status = 401
  return error
}

function handleAuthFailure() {
  sessionAuthenticated = false
  window.dispatchEvent(new CustomEvent('wavecontrol-auth-failure'))
}

export const auth = {
  isAuthenticated() { return sessionAuthenticated },
  markAuthenticated() { sessionAuthenticated = true },
  clear() { sessionAuthenticated = false },
  // Expiration is enforced by the server because the JWT is intentionally
  // inaccessible to JavaScript. Periodic /me checks provide fail-closed sync.
  isExpired() { return false },
}


// Server/client sync tracking. Any successful authenticated HTTP request or
// websocket message counts as proof that this tab is still in sync with the server.
const syncListeners = []
let syncState = {
  lastHttpAt: 0,
  lastWsAt: 0,
  lastPongAt: 0,
  lastAuthOkAt: 0,
  lastAuthCheckAt: 0,
  wsStatus: 'disconnected',
  wsConnectedAt: 0,
  wsDisconnectedAt: 0,
  lastError: ''
}

function updateSyncState(patch = {}) {
  syncState = { ...syncState, ...patch }
  syncListeners.forEach(fn => {
    try { fn(syncState) } catch (_) {}
  })
}

export const sync = {
  getState() {
    return syncState
  },
  on(fn) {
    syncListeners.push(fn)
    return () => {
      const i = syncListeners.indexOf(fn)
      if (i >= 0) syncListeners.splice(i, 1)
    }
  },
  markHTTP() {
    const now = Date.now()
    updateSyncState({ lastHttpAt: now, lastAuthOkAt: now, lastError: '' })
  },
  markAuthCheck() {
    updateSyncState({ lastAuthCheckAt: Date.now() })
  },
  markAuthOK() {
    const now = Date.now()
    updateSyncState({ lastAuthOkAt: now, lastHttpAt: Math.max(syncState.lastHttpAt || 0, now), lastError: '' })
  },
  markWSOpen() {
    updateSyncState({ wsStatus: 'connected', wsConnectedAt: Date.now(), lastError: '' })
  },
  markWSClose(reason = '') {
    updateSyncState({ wsStatus: 'disconnected', wsDisconnectedAt: Date.now(), lastError: reason || syncState.lastError || '' })
  },
  markWSError(reason = '') {
    updateSyncState({ wsStatus: 'error', lastError: reason || syncState.lastError || '' })
  },
  markWSMessage() {
    updateSyncState({ lastWsAt: Date.now(), lastError: '' })
  },
  markPong() {
    const now = Date.now()
    updateSyncState({ lastPongAt: now, lastWsAt: now, lastError: '' })
  }
}

async function request(method, path, body = null, options = {}) {
  const upperMethod = String(method || 'GET').toUpperCase()
  const headers = { 'Accept': 'application/json' }
  if (body !== null) headers['Content-Type'] = 'application/json'
  if (!['GET', 'HEAD', 'OPTIONS'].includes(upperMethod)) headers[CSRF_HEADER] = '1'

  const opts = {
    method: upperMethod,
    headers,
    credentials: 'same-origin',
    cache: 'no-store',
  }
  if (body !== null) opts.body = JSON.stringify(body)

  const resp = await fetch(BASE + path, opts)
  if (resp.status === 401) {
    auth.clear()
    if (options.handleAuthFailure !== false) handleAuthFailure()
    throw authError()
  }
  if (!resp.ok) {
    const text = await resp.text()
    const error = new Error(text || resp.statusText)
    error.status = resp.status
    throw error
  }

  const data = await resp.json()
  auth.markAuthenticated()
  sync.markHTTP()
  return data
}

async function download(path) {
  const resp = await fetch(BASE + path, {
    method: 'GET',
    credentials: 'same-origin',
    cache: 'no-store',
  })
  if (resp.status === 401) {
    handleAuthFailure()
    throw authError()
  }
  if (!resp.ok) {
    const text = await resp.text()
    const error = new Error(text || resp.statusText)
    error.status = resp.status
    throw error
  }
  auth.markAuthenticated()
  sync.markHTTP()
  return resp.blob()
}

export const api = {
  // Auth
  async login(username, password) {
    const resp = await fetch(BASE + '/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', [CSRF_HEADER]: '1' },
      credentials: 'same-origin',
      cache: 'no-store',
      body: JSON.stringify({ username, password })
    })
    if (!resp.ok) {
      const text = await resp.text().catch(() => '')
      const error = new Error(text || 'Invalid credentials')
      error.status = resp.status
      throw error
    }
    const data = await resp.json()
    auth.markAuthenticated()
    sync.markAuthOK()
    return data
  },

  async logout() {
    try {
      await request('POST', '/auth/logout', {}, { handleAuthFailure: false })
    } finally {
      auth.clear()
    }
  },
  
  async me(options = {}) {
    return request('GET', '/me', null, { handleAuthFailure: options.handleAuthFailure !== false })
  },
  
  // Devices
  async devices() {
    return request('GET', '/devices')
  },
  
  async device(id) {
    return request('GET', `/devices/${id}`)
  },
  
  async addDevice(ip, username, password, siteId = null) {
    return request('POST', '/devices', { ip, username, password, site_id: siteId })
  },
  
  async bulkAddDevices(ips, username, password, siteId = null) {
    return request('POST', '/devices/bulk-add', { ips, username, password, site_id: siteId })
  },
  
  async deleteDevice(id) {
    return request('DELETE', `/devices/${id}`)
  },
  
  async refreshDevice(id) {
    return request('POST', `/devices/${id}/refresh`)
  },

  async rebootDevice(id) {
    return request('POST', `/devices/${id}/reboot`)
  },

  async deviceIdentityMismatch(id) {
    return request('GET', `/devices/${id}/identity-mismatch`)
  },

  async learnDeviceMAC(id, newMac, reason = 'device_replaced') {
    return request('POST', `/devices/${id}/learn-mac`, { new_mac: newMac, reason })
  },

  async updateDeviceAntenna(deviceId, data) {
    return request('PATCH', `/devices/${deviceId}/antenna`, data)
  },

  // Ultra Debug (per-device / per-host)
  async setUltraDebugDevice(deviceId, enabled) {
    return request('POST', `/devices/${deviceId}/ultra-debug`, { enabled: !!enabled })
  },

  async setUltraDebugHost(host, enabled) {
    return request('POST', `/ultra-debug/host`, { host: String(host || ''), enabled: !!enabled })
  },

  async ultraDebugList() {
    return request('GET', `/ultra-debug`)
  },

  async ultraDebugDevice(deviceId, tail = 500) {
    return request('GET', `/ultra-debug/${deviceId}?tail=${encodeURIComponent(tail)}`)
  },

  async ultraDebugHost(host, tail = 500) {
    return request('GET', `/ultra-debug/host/${encodeURIComponent(host)}?tail=${encodeURIComponent(tail)}`)
  },

  async clearUltraDebugDevice(deviceId) {
    return request('POST', `/ultra-debug/${deviceId}/clear`, {})
  },

  async clearUltraDebugHost(host) {
    return request('POST', `/ultra-debug/host/${encodeURIComponent(host)}/clear`, {})
  },


  async downloadUltraDebugDevice(deviceId) {
    return download(`/ultra-debug/${deviceId}/download`)
  },

  async downloadUltraDebugHost(host) {
    return download(`/ultra-debug/host/${encodeURIComponent(host)}/download`)
  },
  
  // Stats (real-time from memory)
  async stats() {
    return request('GET', '/stats')
  },
  
  async deviceStats(ip) {
    return request('GET', `/stats/${encodeURIComponent(ip)}`)
  },
  
  async getThroughputHistory() {
    return request('GET', '/stats/throughput-history')
  },
  
  async getStabilityStats() {
    return request('GET', '/stats/stability')
  },
  
  // Firmware
  async firmwareList() {
    return request('GET', '/firmware')
  },
  
  async firmwareVersions() {
    return request('GET', '/firmware/versions')
  },
  
  async uploadFirmware(file) {
    const formData = new FormData()
    formData.append('firmware', file)
    const resp = await fetch(BASE + '/firmware', {
      method: 'POST',
      headers: { [CSRF_HEADER]: '1' },
      credentials: 'same-origin',
      cache: 'no-store',
      body: formData
    })
    if (resp.status === 401) {
      handleAuthFailure()
      throw authError()
    }
    if (!resp.ok) {
      const text = await resp.text()
      const error = new Error(text || resp.statusText)
      error.status = resp.status
      throw error
    }
    auth.markAuthenticated()
    sync.markHTTP()
    return resp.json()
  },
  
  async deleteFirmware(reference) {
    return request('DELETE', `/firmware/${encodeURIComponent(reference)}`)
  },
  
  async upgradeDevice(deviceId, firmwareFile, force = false) {
    return request('POST', `/devices/${deviceId}/upgrade`, { firmware: firmwareFile, force })
  },
  
  async upgradeFanout(deviceId, force = false) {
    return request('POST', `/devices/${deviceId}/upgrade-fanout`, { force })
  },
  
  async bulkUpgrade(deviceIds, firmware, force = false) {
    return request('POST', '/devices/bulk-upgrade', { device_ids: deviceIds, firmware, force })
  },
  
  async retryUpgradeWithCredentials(deviceIds, username, password, force = false) {
    return request('POST', '/devices/retry-upgrade', { device_ids: deviceIds, username, password, force })
  },
  
  // Search
  async search(q) {
    return request('GET', `/search?q=${encodeURIComponent(q)}`)
  },
  
  // Logs
  async logs(limit = 50) {
    return request('GET', `/logs?limit=${limit}`)
  },
  
  // Users (admin)
  async users() {
    return request('GET', '/users')
  },
  
  async createUser(username, password, roles) {
    return request('POST', '/users', { username, password, roles })
  },
  
  async updateUser(id, data) {
    return request('PATCH', `/users/${id}`, data)
  },
  
  async deleteUser(id) {
    return request('DELETE', `/users/${id}`)
  },
  
  async roles() {
    return request('GET', '/roles')
  },
  
  // Settings
  async settings() {
    return request('GET', '/settings')
  },
  
  async updateSetting(key, value) {
    return request('PATCH', `/settings/${encodeURIComponent(key)}`, { value })
  },

  async updateSettings(settings) {
    return request('PATCH', '/settings', { settings })
  },

  // Alerts and alert rules
  async alertRules() {
    return request('GET', '/alerts/rules')
  },

  async createAlertRule(rule) {
    return request('POST', '/alerts/rules', rule)
  },

  async updateAlertRule(id, rule) {
    return request('PATCH', `/alerts/rules/${id}`, rule)
  },

  async deleteAlertRule(id) {
    return request('DELETE', `/alerts/rules/${id}`)
  },

  async alerts(status = '', limit = 100) {
    let url = `/alerts?limit=${encodeURIComponent(limit)}`
    if (status) url += `&status=${encodeURIComponent(status)}`
    return request('GET', url)
  },

  async acknowledgeAlert(id) {
    return request('POST', `/alerts/${id}/acknowledge`)
  },

  async resolveAlert(id) {
    return request('POST', `/alerts/${id}/resolve`)
  },


  async updateDeviceAlerting(id, patch) {
    return request('PATCH', `/devices/${id}/alerting`, patch)
  },

  async bulkUpdateDeviceAlerting(deviceIds, patch) {
    return request('POST', '/devices/bulk-alerting', { device_ids: deviceIds, ...patch })
  },

  // Sites
  async sites() {
    return request('GET', '/sites')
  },

  async createSite(data) {
    return request('POST', '/sites', data)
  },

  async updateSite(id, data) {
    return request('PATCH', `/sites/${id}`, data)
  },

  async deleteSite(id) {
    return request('DELETE', `/sites/${id}`)
  },

  // Regions
  async regions() {
    return request('GET', '/regions')
  },

  async createRegion(data) {
    return request('POST', '/regions', data)
  },

  async updateRegion(id, data) {
    return request('PATCH', `/regions/${id}`, data)
  },

  async deleteRegion(id) {
    return request('DELETE', `/regions/${id}`)
  },
  
  // Bulk assign site to devices
  async bulkAssignSite(deviceIds, siteId) {
    return request('POST', '/devices/bulk-assign-site', { device_ids: deviceIds, site_id: siteId })
  },
  
  // Ping (health check)
  async ping() {
    try {
      const resp = await fetch(BASE + '/ping', { cache: 'no-store' })
      if (resp.status === 401) {
        handleAuthFailure()
        return { ok: false, status: 401, error: 'Unauthorized' }
      }
      if (!resp.ok) {
        const text = await resp.text().catch(() => '')
        return { ok: false, status: resp.status, error: text || resp.statusText }
      }
      return { ok: true, ...(await resp.json()) }
    } catch (e) {
      // Network error - no response at all
      return { ok: false, network: true, error: e.message }
    }
  },
  
  // Scheduled Jobs
  async jobs(status = '', limit = 50) {
    let url = `/jobs?limit=${limit}`
    if (status) url += `&status=${status}`
    return request('GET', url)
  },
  
  async createJob(jobType, deviceIds, params = {}, scheduledAt = null, repeatCron = '') {
    return request('POST', '/jobs', {
      job_type: jobType,
      device_ids: deviceIds,
      parameters: params,
      scheduled_at: scheduledAt || new Date().toISOString(),
      repeat_cron: repeatCron
    })
  },
  
  async cancelJob(jobId) {
    return request('DELETE', `/jobs/${jobId}`)
  },

  async updateJob(jobId, fields = {}) {
    return request('PATCH', `/jobs/${jobId}`, fields)
  },
  
  // Config Backup
  async backupConfig(deviceId) {
    return request('POST', `/devices/${deviceId}/backup`)
  },
  
  async restoreConfig(deviceId, path) {
    return request('POST', `/devices/${deviceId}/restore`, { path })
  },
  
  async listConfigs(deviceId) {
    return request('GET', `/devices/${deviceId}/configs`)
  },
  
  async listAllConfigs() {
    return request('GET', '/configs')
  },
  
  async bulkBackup(deviceIds) {
    return request('POST', '/devices/bulk-backup', { device_ids: deviceIds })
  },
  
  // Batch Config
  async batchConfig(deviceIds, changes) {
    return request('POST', '/devices/batch-config', { device_ids: deviceIds, changes })
  },
  
  // Reports
  async generateReport(type, options = {}) {
    return request('POST', '/reports/generate', { type, ...options })
  },
  
  async listReports(options = {}) {
    const params = new URLSearchParams()
    const limit = Number.parseInt(options.limit, 10)
    if (Number.isSafeInteger(limit) && limit > 0) params.set('limit', String(Math.min(limit, 200)))
    if (options.type) params.set('type', String(options.type))
    const query = params.toString()
    return request('GET', `/reports${query ? `?${query}` : ''}`)
  },
  
  async getReport(reportId) {
    return request('GET', `/reports/${reportId}`)
  },
  
  async deleteReport(reportId) {
    return request('DELETE', `/reports/${reportId}`)
  },
  
  async compareReports(reportId1, reportId2) {
    return request('POST', '/reports/compare', {
      report_id_1: reportId1,
      report_id_2: reportId2
    })
  },
  
  // Async Job Runs
  async startJob(jobType, deviceIds, params = {}) {
    return request('POST', '/job-runs', {
      job_type: jobType,
      device_ids: deviceIds,
      parameters: params
    })
  },
  
  async getJobRun(jobId) {
    return request('GET', `/job-runs/${jobId}`)
  },
  
  async getJobRunEvents(jobId, limit = 100) {
    return request('GET', `/job-runs/${jobId}/events?limit=${limit}`)
  },
  
  async listJobRuns(status = '', limit = 50) {
    let url = `/job-runs?limit=${limit}`
    if (status) url += `&status=${status}`
    return request('GET', url)
  },
  
  async cancelJobRun(jobId) {
    return request('DELETE', `/job-runs/${jobId}`)
  },
  
  // Bulk Operations Config
  async getBulkOpsConfig() {
    return request('GET', '/bulk-ops/config')
  },
  
  async updateBulkOpsConfig(config) {
    return request('PATCH', '/bulk-ops/config', config)
  },
  
  async getBulkOpsStats() {
    return request('GET', '/bulk-ops/stats')
  },
  
  // Poller Config
  async getPollerConfig() {
    return request('GET', '/poller/config')
  },
  
  async updatePollerConfig(config) {
    return request('PATCH', '/poller/config', config)
  },
  
  // Drilldown Lists
  async listDrilldownLists() {
    return request('GET', '/drilldown-lists')
  },
  
  async createDrilldownList(name, description, pollInterval) {
    return request('POST', '/drilldown-lists', { name, description, poll_interval: pollInterval })
  },
  
  async updateDrilldownList(id, data) {
    return request('PATCH', `/drilldown-lists/${id}`, data)
  },
  
  async deleteDrilldownList(id) {
    return request('DELETE', `/drilldown-lists/${id}`)
  },
  
  async getDrilldownHosts(listId) {
    return request('GET', `/drilldown-lists/${listId}/hosts`)
  },
  
  async addDrilldownHost(listId, host) {
    return request('POST', `/drilldown-lists/${listId}/hosts`, { host })
  },
  
  async removeDrilldownHost(listId, hostId) {
    return request('DELETE', `/drilldown-lists/${listId}/hosts/${hostId}`)
  },

  // Generic REST methods for endpoints without dedicated methods
  async get(path) {
    return request('GET', path)
  },
  
  async post(path, data) {
    return request('POST', path, data)
  },
  
  async put(path, data) {
    return request('PUT', path, data)
  },
  
  async patch(path, data) {
    return request('PATCH', path, data)
  },
  
  async delete(path) {
    return request('DELETE', path)
  }
}

// WebSocket connection for real-time updates
export const ws = {
  socket: null,
  listeners: [],
  reconnectAttempts: 0,
  maxReconnectAttempts: 10,
  
  connect() {
    if (!auth.isAuthenticated()) return
    
    // Reset reconnect counter on new connection attempt
    this.reconnectAttempts = 0
    
    const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const url = `${protocol}//${location.host}${BASE}/ws`
    
    try {
      this.socket = new WebSocket(url)
      
      this.socket.onopen = () => {
        console.log('WebSocket connected')
        this.reconnectAttempts = 0
        sync.markWSOpen()
      }
      
      this.socket.onmessage = (event) => {
        try {
          // Handle multiple messages (batched)
          const messages = event.data.split('\n').filter(Boolean)
          messages.forEach(msgStr => {
            const msg = JSON.parse(msgStr)
            if (msg?.type === 'pong') sync.markPong()
            else sync.markWSMessage()
            this.notify(msg)
          })
        } catch (e) {
          console.error('WebSocket parse error:', e)
        }
      }
      
      this.socket.onclose = (event) => {
        console.log('WebSocket disconnected')
        sync.markWSClose(event?.reason || '')
        this.socket = null
        this.scheduleReconnect()
      }
      
      this.socket.onerror = (err) => {
        console.error('WebSocket error:', err)
        sync.markWSError(err?.message || 'websocket error')
      }
    } catch (e) {
      console.error('WebSocket connection failed:', e)
      this.scheduleReconnect()
    }
  },
  
  disconnect() {
    // Clear any pending reconnect
    this.reconnectAttempts = this.maxReconnectAttempts
    if (this.socket) {
      // Remove onclose handler to prevent reconnect scheduling
      this.socket.onclose = null
      this.socket.close()
      this.socket = null
      sync.markWSClose('client disconnect')
    }
  },
  
  scheduleReconnect() {
    if (this.reconnectAttempts >= this.maxReconnectAttempts) {
      console.log('Max reconnect attempts reached')
      return
    }
    
    const delay = Math.min(1000 * Math.pow(2, this.reconnectAttempts), 30000)
    this.reconnectAttempts++
    
    setTimeout(() => {
      if (auth.isAuthenticated()) {
        this.connect()
      }
    }, delay)
  },
  
  send(data) {
    if (this.socket && this.socket.readyState === WebSocket.OPEN) {
      this.socket.send(JSON.stringify(data))
    }
  },
  
  // Subscribe to messages
  on(callback) {
    this.listeners.push(callback)
    return () => {
      const i = this.listeners.indexOf(callback)
      if (i >= 0) this.listeners.splice(i, 1)
    }
  },
  
  // Unsubscribe from messages
  off(callback) {
    const i = this.listeners.indexOf(callback)
    if (i >= 0) this.listeners.splice(i, 1)
  },
  
  notify(msg) {
    this.listeners.forEach(fn => fn(msg))
  },
  
  // Ping to keep connection alive (only starts once)
  pingStarted: false,
  startPing() {
    if (this.pingStarted) return
    this.pingStarted = true
    setInterval(() => {
      this.send({ type: 'ping' })
    }, 25000)
  }
}