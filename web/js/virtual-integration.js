// =============================================================================
// Virtual Table Integration for WaveControl
// =============================================================================
//
// This module provides:
// 1. shouldUseVirtualTable() - Check if dataset is large enough
// 2. wsBatcher - Batches WebSocket updates to reduce DOM operations
// 3. getSortedFilteredDevices() - Gets filtered/sorted data for virtual table
// 4. Re-exports VirtualTable class
//

import { VirtualTable } from './virtual-table.js?v=6'
import { store } from './store.js?v=15'

// Re-export for components.js
export { VirtualTable }

// =============================================================================
// CONFIGURATION
// =============================================================================

// Switch to virtual table when device count exceeds this
export const VIRTUAL_THRESHOLD = 500

// =============================================================================
// THRESHOLD CHECK
// =============================================================================

export function shouldUseVirtualTable() {
  const count = store.devices?.length || 0
  return count > VIRTUAL_THRESHOLD
}

// =============================================================================
// WEBSOCKET UPDATE BATCHER
// =============================================================================
// 
// Instead of updating DOM for each of 5000 WebSocket messages:
// 1. Collect updates in pending map
// 2. After batchMs, flush all updates in single pass
// 3. Dramatically reduces browser reflows
//

let virtualTableRef = null  // Set by components.js when virtual table is active

export function setVirtualTableRef(vt) {
  virtualTableRef = vt
}

// Scroll to and highlight a device by ID in the virtual table
export function scrollToDeviceById(id) {
  if (virtualTableRef && typeof virtualTableRef.scrollToId === 'function') {
    return virtualTableRef.scrollToId(id)  // Returns true if found and scrolled
  }
  return false
}

const wsBatcher = {
  // Use device id as the primary key to avoid conflating devices when IPs are reused.
  // (IP is only a fallback identifier when no MAC/ID exists.)
  pending: new Map(),   // id -> updates object
  timer: null,
  batchMs: 100,         // Collect updates for 100ms then flush

  // Counts updates (online/offline/unknown) can be expensive on large datasets
  // because they require scanning the full in-memory list. Throttle them.
  countsDirty: false,
  countsTimer: null,
  lastCountsAt: 0,
  countsMinIntervalMs: 1000,
  
  add(id, updates) {
    if (id === undefined || id === null) return
    const existing = this.pending.get(id) || {}
    this.pending.set(id, { ...existing, ...updates })

    // Mark counts dirty only when status-affecting fields change.
    if (updates && (Object.prototype.hasOwnProperty.call(updates, 'online') ||
        Object.prototype.hasOwnProperty.call(updates, 'status') ||
        Object.prototype.hasOwnProperty.call(updates, 'db_status'))) {
      this.countsDirty = true
    }
    this._schedule()
  },
  
  _schedule() {
    if (this.timer) return
    this.timer = setTimeout(() => {
      this._flush()
      this.timer = null
    }, this.batchMs)
  },
  
  _flush() {
    if (this.pending.size === 0) return
    
    // Apply updates to virtual table
    if (virtualTableRef) {
      for (const [id, updates] of this.pending) {
        virtualTableRef.updateById(id, updates)
      }
    }
    
    this.pending.clear()

    // Trigger counts update (throttled)
    if (this.countsDirty && typeof updateCountsCallback === 'function') {
      const now = Date.now()
      const elapsed = now - (this.lastCountsAt || 0)

      const doUpdate = () => {
        this.lastCountsAt = Date.now()
        this.countsDirty = false
        this.countsTimer = null
        try { updateCountsCallback() } catch (e) {}
      }

      if (elapsed >= this.countsMinIntervalMs) {
        doUpdate()
      } else if (!this.countsTimer) {
        // Ensure counts update eventually even if updates stop.
        this.countsTimer = setTimeout(doUpdate, this.countsMinIntervalMs - elapsed)
      }
    }
  }
}

export { wsBatcher }

// Callback for updating status counts
let updateCountsCallback = null
export function setUpdateCountsCallback(fn) {
  updateCountsCallback = fn
}

// Allow other modules (e.g. components.js) to trigger a counts refresh
// without importing app.js. This calls the callback registered by app.js.
export function triggerUpdateCounts() {
  if (typeof updateCountsCallback === 'function') {
    try { updateCountsCallback() } catch (e) {}
  }
}

// =============================================================================
// DATA HELPERS
// =============================================================================

/**
 * Get filtered and sorted device list for virtual table.
 * This runs entirely client-side on in-memory data.
 * Optimized to minimize array allocations.
 */
export function getSortedFilteredDevices() {
  const devices = store.filteredDevices || []
  const query = (store.searchQuery || '').toLowerCase()
  
  // 1. Apply search filter - single pass
  let filtered
  if (query) {
    filtered = []
    for (let i = 0; i < devices.length; i++) {
      const d = devices[i]
      if ((d.hostname && d.hostname.toLowerCase().indexOf(query) !== -1) ||
          (d.ip_address && d.ip_address.toLowerCase().indexOf(query) !== -1) ||
          (d.product && d.product.toLowerCase().indexOf(query) !== -1) ||
          (d.mac && d.mac.toLowerCase().indexOf(query) !== -1) ||
          (d.flavor && d.flavor.toLowerCase().indexOf(query) !== -1)) {
        filtered.push(d)
      }
    }
  } else {
    filtered = devices
  }
  
  // 2. Check if we're doing column sorting (flat) or hierarchical view
  const sortCol = store.sortColumn
  const sortDir = store.sortDirection || 'asc'
  
  // Helper functions for getting displayed signal values (must match components.js)
  const getSignal5GHz = (d) => {
    if (d.radio_5ghz?.signal_combined) return d.radio_5ghz.signal_combined
    if (d.radio_ltu?.signal_combined) return d.radio_ltu.signal_combined
    return d.signal_5ghz || d.signal_ltu || d.signal_airmax || 
           d.radio_5ghz?.signal || d.radio_ltu?.signal || 0
  }
  
  const getSignal5GHzChains = (d) => {
    if (d.radio_5ghz?.signal_per_chain?.length > 0) return d.radio_5ghz.signal_per_chain
    if (d.radio_ltu?.signal_per_chain?.length > 0) return d.radio_ltu.signal_per_chain
    if (d.signal_per_chain?.length > 0) return d.signal_per_chain
    const sig = d.signal_5ghz || d.signal_ltu || d.signal_airmax || 0
    return sig ? [sig] : []
  }
  
  const getSignal5GHzChain = (d, idx) => getSignal5GHzChains(d)[idx] || 0
  
  const getSTASignal5GHz = (d) => {
    if (d.radio_5ghz?.remote_signal_combined) return d.radio_5ghz.remote_signal_combined
    if (d.radio_ltu?.remote_signal_combined) return d.radio_ltu.remote_signal_combined
    if (d.remote_signal_combined) return d.remote_signal_combined
    return d.radio_5ghz?.remote_signal || d.radio_ltu?.remote_signal || d.remote_signal || 0
  }
  
  const getSTASignal5GHzChains = (d) => {
    if (d.radio_5ghz?.remote_signal_per_chain?.length > 0) return d.radio_5ghz.remote_signal_per_chain
    if (d.radio_ltu?.remote_signal_per_chain?.length > 0) return d.radio_ltu.remote_signal_per_chain
    if (d.remote_signal_per_chain?.length > 0) return d.remote_signal_per_chain
    const sig = d.radio_5ghz?.remote_signal || d.radio_ltu?.remote_signal || d.remote_signal || 0
    return sig ? [sig] : []
  }
  
  const getSTASignal5GHzChain = (d, idx) => getSTASignal5GHzChains(d)[idx] || 0
  
  const getSTASignal60GHz = (d) => d.radio_60ghz?.remote_signal || 0
  
  // If sorting by column, do flat sort (skip hierarchical grouping)
  if (sortCol) {
    filtered.sort((a, b) => {
      let av, bv
      
      // Sort by what's displayed in the column - must match components.js exactly
      switch (sortCol) {
        case 'hostname': 
          av = a.hostname || ''; bv = b.hostname || ''; break
        case 'ip': 
          av = a.ip_address || ''; bv = b.ip_address || ''; break
        case 'mac': 
          av = a.mac || ''; bv = b.mac || ''; break
        case 'product': 
          av = a.product || a.model || ''; bv = b.product || b.model || ''; break
        case 'site': 
          av = a.site_name || ''; bv = b.site_name || ''; break
        case 'signal_60ghz': 
          av = a.signal_60ghz || -999; bv = b.signal_60ghz || -999; break
        case 'sta_60ghz':
          av = getSTASignal60GHz(a) || -999; bv = getSTASignal60GHz(b) || -999; break
        case 'signal_5ghz':
          av = getSignal5GHz(a) || -999; bv = getSignal5GHz(b) || -999; break
        case 'signal_5ghz_c0':
          av = getSignal5GHzChain(a, 0) || -999; bv = getSignal5GHzChain(b, 0) || -999; break
        case 'signal_5ghz_c1':
          av = getSignal5GHzChain(a, 1) || -999; bv = getSignal5GHzChain(b, 1) || -999; break
        case 'sta_5ghz':
          av = getSTASignal5GHz(a) || -999; bv = getSTASignal5GHz(b) || -999; break
        case 'sta_5ghz_c0':
          av = getSTASignal5GHzChain(a, 0) || -999; bv = getSTASignal5GHzChain(b, 0) || -999; break
        case 'sta_5ghz_c1':
          av = getSTASignal5GHzChain(a, 1) || -999; bv = getSTASignal5GHzChain(b, 1) || -999; break
        case 'distance': 
          av = a.distance || 0; bv = b.distance || 0; break
        case 'capacity': 
          av = a.capacity_60ghz || a.capacity_ltu || a.capacity_5ghz || 0
          bv = b.capacity_60ghz || b.capacity_ltu || b.capacity_5ghz || 0
          break
        case 'firmware': 
          av = a.firmware_version || a.firmware || ''
          bv = b.firmware_version || b.firmware || ''
          break
        default: 
          av = a.hostname || ''; bv = b.hostname || ''
      }
      
      if (typeof av === 'string') {
        return sortDir === 'asc' ? av.localeCompare(bv) : bv.localeCompare(av)
      }
      return sortDir === 'asc' ? av - bv : bv - av
    })
    return filtered
  }
  
  // No column sort - use hierarchical grouping (APs first, then their STAs)
  // Optimized for large datasets: O(N) grouping using a children map (avoids nested loops)
  const apIds = new Set()
  const grouped = []
  const childrenByParent = new Map() // parent_id -> [devices]

  for (let i = 0; i < filtered.length; i++) {
    const d = filtered[i]
    // Managed devices (added via Add IP / Bulk) should appear at the root level,
    // even if they have a parent_id (i.e. they are STAs).
    if (!d.parent_id || d.managed) {
      apIds.add(d.id)
    } else {
      const pid = d.parent_id
      let arr = childrenByParent.get(pid)
      if (!arr) {
        arr = []
        childrenByParent.set(pid, arr)
      }
      arr.push(d)
    }
  }

  // Add APs with their STAs
  for (let i = 0; i < filtered.length; i++) {
    const d = filtered[i]
    if (!d.parent_id || d.managed) {
      grouped.push(d)
      const kids = childrenByParent.get(d.id)
      if (kids && kids.length) {
        // Maintain insertion order (AP, then its STAs)
        for (let k = 0; k < kids.length; k++) {
          grouped.push(kids[k])
        }
      }
    }
  }

  // Add orphan STAs (parent AP not in filtered list)
  for (let i = 0; i < filtered.length; i++) {
    const d = filtered[i]
    if (d.parent_id && !d.managed && !apIds.has(d.parent_id)) {
      grouped.push(d)
    }
  }
  
  return grouped
}
