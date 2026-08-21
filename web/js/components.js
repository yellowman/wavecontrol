import { store } from './store.js?v=15'
import { api } from './api.js?v=27'
import { 
  VirtualTable, 
  shouldUseVirtualTable, 
  getSortedFilteredDevices,
  setVirtualTableRef,
  VIRTUAL_THRESHOLD,
  triggerUpdateCounts
} from './virtual-integration.js?v=11'


async function requestConfirmation(message, options = {}) {
  const dialog = window.waveControlConfirm
  if (typeof dialog !== 'function') {
    showToast('The confirmation dialog is not available. Reload WaveControl and try again.', 'error')
    return false
  }
  return Boolean(await dialog(message, options))
}

async function requestInput(options = {}) {
  const dialog = window.waveControlInput
  if (typeof dialog !== 'function') {
    showToast('The input dialog is not available. Reload WaveControl and try again.', 'error')
    return null
  }
  return dialog(options)
}

// Use threshold from virtual-integration.js
const VIRTUAL_TABLE_THRESHOLD = VIRTUAL_THRESHOLD

// Extract clean firmware version from full firmware string
// "AFLTU.ar934x.v2.4.0.hash123.date" -> "v2.4.0"
// "GMC.ipq5018.v4.1.0.deda4ab.251212" -> "v4.1.0"
// "XC.qca955x.v8.7.0" -> "v8.7.0"
// "v2.4.0.00017" -> "v2.4.0.00017" (preserve build numbers)
function extractFirmwareVersion(firmware) {
  if (!firmware) return ''
  
  // Find ".v" to locate version start
  const idx = firmware.indexOf('.v')
  if (idx > 0) {
    firmware = firmware.substring(idx + 1)
  } else if (!firmware.startsWith('v')) {
    // If it doesn't have ".v" or start with "v", return as-is
    // (might be a clean version already)
    return firmware
  }
  
  // Parse version parts
  if (firmware.startsWith('v')) {
    const parts = firmware.split('.')
    const result = []
    for (let i = 0; i < parts.length; i++) {
      const p = parts[i]
      if (i === 0) {
        result.push(p)
        continue
      }
      // Keep numeric parts, stop at hash/date
      if (p.length > 0 && p[0] >= '0' && p[0] <= '9') {
        // Check if it looks like a hash (6+ hex chars)
        if (p.length >= 6 && /^[0-9a-fA-F]+$/.test(p.substring(0, 6))) {
          break
        }
        result.push(p)
      } else {
        break
      }
    }
    if (result.length >= 2) {
      return result.join('.')
    }
  }
  
  return firmware
}

// Get display firmware version - extract clean version and strip 'v' prefix
function getDisplayFirmware(device) {
  let fw = ''
  // Try firmware field first, then firmware_version
  if (device.firmware) fw = device.firmware
  else if (device.firmware_version) fw = device.firmware_version
  else return '-'
  
  // Extract clean version (removes platform prefix, keeps build numbers)
  fw = extractFirmwareVersion(fw)
  
  // Strip leading 'v' for cleaner display
  if (fw.startsWith('v')) fw = fw.substring(1)
  return fw
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


const DASHBOARD_SCOPE_TYPE_OPTIONS = [
  { key: 'airmax', label: 'airMAX' },
  { key: 'airfiber', label: 'airFiber' },
  { key: 'ltu', label: 'LTU' },
  { key: 'wave60', label: 'Wave 60' },
  { key: 'waveMlo5', label: 'Wave MLO5' },
  { key: 'waveMlo6', label: 'Wave MLO6' }
]

const DASHBOARD_SCOPE_BAND_OPTIONS = [
  { key: 'band2', label: '2 GHz' },
  { key: 'band3', label: '3 GHz' },
  { key: 'band5', label: '5 GHz' },
  { key: 'band6', label: '6 GHz' },
  { key: 'band11', label: '11 GHz' },
  { key: 'band16', label: '16 GHz' },
  { key: 'band24', label: '24 GHz' },
  { key: 'band60', label: '60 GHz' }
]


function countActiveDashboardScope(exclusions = store.dashboardExclusions) {
  if (!exclusions) return 0
  return [...Object.values(exclusions.types || {}), ...Object.values(exclusions.bands || {})].filter(Boolean).length
}

function formatDashboardCount(value) {
  return Number(value || 0).toLocaleString()
}

function getDashboardScopeSummary(visibleCount, totalCount) {
  const visible = Math.max(0, Number(visibleCount || 0))
  const total = Math.max(0, Number(totalCount || 0))
  const hidden = Math.max(0, total - visible)
  if (hidden > 0) return `${formatDashboardCount(visible)} • ${formatDashboardCount(hidden)} / ${formatDashboardCount(total)}`
  return `${formatDashboardCount(total)}`
}

function buildDashboardScopeFooter(visibleCount, totalCount) {
  const visible = Math.max(0, Number(visibleCount || 0))
  const total = Math.max(0, Number(totalCount || 0))
  const hidden = Math.max(0, total - visible)
  const compact = getDashboardScopeSummary(visible, total)
  const title = hidden > 0
    ? `${formatDashboardCount(visible)} visible, ${formatDashboardCount(hidden)} hidden of ${formatDashboardCount(total)}`
    : `${formatDashboardCount(total)} total`
  return `<div class="dashboard-status-foot" title="${escapeAttr(title)}">${escapeHTML(compact)}</div>`
}


function buildDashboardScopeControl(visibleCount, totalCount) {
  const exclusions = store.dashboardExclusions || { types: {}, bands: {} }
  const activeCount = countActiveDashboardScope(exclusions)
  const renderOption = (group, opt) => {
    const active = !!exclusions[group]?.[opt.key]
    return `
      <button type="button" class="scope-chip-btn ${active ? 'active' : ''}" data-scope-group="${group}" data-scope-key="${opt.key}" aria-pressed="${active ? 'true' : 'false'}">
        ${escapeHTML(opt.label)}
      </button>
    `
  }
  const titleText = activeCount > 0 ? `Exclude from dashboard · ${activeCount} active` : 'Exclude from dashboard'
  return `
    <div class="dashboard-scope">
      <button class="btn btn-subtle toolbar-mini-btn js-toggle-dashboard-scope ${activeCount ? 'active' : ''}" title="Focus filters" aria-label="Focus filters" type="button">
        <span class="toolbar-mini-icon">◉</span>
      </button>
      <div class="dashboard-scope-dropdown hidden">
        <div class="scope-header">
          <div class="scope-title">${escapeHTML(titleText)}</div>
          <button class="scope-reset js-reset-dashboard-scope" type="button" title="Turn all exclusions off">Clear all</button>
        </div>
        <div class="scope-section">
          <div class="scope-section-title">Type</div>
          <div class="scope-chip-grid">
            ${DASHBOARD_SCOPE_TYPE_OPTIONS.map(opt => renderOption('types', opt)).join('')}
          </div>
        </div>
        <div class="scope-section">
          <div class="scope-section-title">Band</div>
          <div class="scope-chip-grid">
            ${DASHBOARD_SCOPE_BAND_OPTIONS.map(opt => renderOption('bands', opt)).join('')}
          </div>
        </div>
      </div>
    </div>
  `
}


function buildDashboardHeaderTools(visibleCount, totalCount, cols) {
  return `
    <div class="dashboard-header-tools">
      <div class="toolbar-mini-group header-mini-group">
        ${buildDashboardScopeControl(visibleCount, totalCount)}
        ${buildColumnMenu(cols)}
      </div>
    </div>
  `
}


function positionHeaderDropdown(trigger, dropdown) {
  if (!trigger || !dropdown) return
  // Make the menu viewport-positioned so it isn't clipped by sticky headers / table overflow.
  dropdown.style.position = 'fixed'
  dropdown.style.right = 'auto'
  dropdown.style.top = '0px'
  dropdown.style.left = '0px'
  dropdown.classList.remove('hidden')

  const rect = trigger.getBoundingClientRect()
  const menuWidth = dropdown.offsetWidth || parseInt(dropdown.dataset.prefWidth || '0', 10) || 280
  const menuHeight = dropdown.offsetHeight || 0
  let left = rect.right - menuWidth
  left = Math.max(8, Math.min(left, window.innerWidth - menuWidth - 8))
  let top = rect.bottom + 6
  if (menuHeight > 0 && top + menuHeight > window.innerHeight - 8) {
    top = Math.max(8, rect.top - menuHeight - 6)
  }
  dropdown.style.left = `${left}px`
  dropdown.style.top = `${top}px`
}

function resetHeaderDropdownPosition(dropdown) {
  if (!dropdown) return
  dropdown.style.position = ''
  dropdown.style.left = ''
  dropdown.style.top = ''
  dropdown.style.right = ''
}


function setupDashboardScopeHandlers(container) {
  if (!container || container.dataset.dashboardScopeBound === 'true') return
  container.dataset.dashboardScopeBound = 'true'

  const closeAllMenus = () => {
    container.querySelectorAll('.dashboard-scope-dropdown').forEach(el => {
      el.classList.add('hidden')
      resetHeaderDropdownPosition(el)
    })
    container.querySelectorAll('.column-dropdown').forEach(el => {
      el.classList.add('hidden')
      resetHeaderDropdownPosition(el)
    })
  }

  // Close menus when the dashboard/table scrolls or viewport changes
  const wrapper = container.querySelector('.device-table-wrapper')
  wrapper?.addEventListener('scroll', closeAllMenus, { passive: true })
  window.addEventListener('resize', closeAllMenus)

  container.addEventListener('click', (e) => {
    const scopeToggle = e.target.closest('.js-toggle-dashboard-scope')
    if (scopeToggle) {
      e.preventDefault()
      e.stopPropagation()
      const scope = scopeToggle.closest('.dashboard-scope')
      const dropdown = scope?.querySelector('.dashboard-scope-dropdown')
      if (!dropdown) return
      const willOpen = dropdown.classList.contains('hidden')
      closeAllMenus()
      if (willOpen) positionHeaderDropdown(scopeToggle, dropdown)
      return
    }

    const scopeReset = e.target.closest('.js-reset-dashboard-scope')
    if (scopeReset) {
      e.preventDefault()
      e.stopPropagation()
      store.resetDashboardExclusions()
      triggerUpdateCounts()
      renderTree(store.treeFilter || '')
      renderDevices(container)
      return
    }

    const scopeChip = e.target.closest('[data-scope-group][data-scope-key]')
    if (scopeChip) {
      e.preventDefault()
      e.stopPropagation()
      const group = scopeChip.dataset.scopeGroup
      const key = scopeChip.dataset.scopeKey
      const current = !!store.dashboardExclusions?.[group]?.[key]
      store.setDashboardExclusion(group, key, !current)
      triggerUpdateCounts()
      renderTree(store.treeFilter || '')
      renderDevices(container)
      // Re-open the scope menu after render so toggles feel responsive.
      const reopenedToggle = container.querySelector('.js-toggle-dashboard-scope')
      const reopenedDropdown = container.querySelector('.dashboard-scope-dropdown')
      if (reopenedToggle && reopenedDropdown) positionHeaderDropdown(reopenedToggle, reopenedDropdown)
      return
    }

    if (!e.target.closest('.dashboard-header-tools') && !e.target.closest('.dashboard-scope-dropdown') && !e.target.closest('.column-dropdown')) {
      closeAllMenus()
    }
  })
}

// Toast notifications
export function showToast(message, type = 'info') {
  const root = document.getElementById('toastRoot')
  if (!root) return
  
  const toast = document.createElement('div')
  toast.className = `toast ${type}`
  
  const text = document.createElement('span')
  text.className = 'toast-text'
  text.textContent = message
  toast.appendChild(text)
  
  // Error toasts get copy button and stay until user clicks elsewhere
  if (type === 'error') {
    const copyBtn = document.createElement('button')
    copyBtn.className = 'toast-btn'
    copyBtn.innerHTML = '[]'
    copyBtn.title = 'Copy to clipboard'
    copyBtn.addEventListener('click', (e) => {
      e.stopPropagation()
      navigator.clipboard.writeText(message)
      copyBtn.innerHTML = 'ok'
      setTimeout(() => copyBtn.innerHTML = '[]', 1500)
    })
    toast.appendChild(copyBtn)
    
    // Dismiss on any click outside the toast
    const dismissHandler = (e) => {
      if (!toast.contains(e.target)) {
        toast.remove()
        document.removeEventListener('click', dismissHandler)
      }
    }
    // Delay adding listener so the triggering click doesn't dismiss immediately
    setTimeout(() => document.addEventListener('click', dismissHandler), 100)
  } else {
    // Non-error toasts auto-dismiss
    setTimeout(() => toast.remove(), 4000)
  }
  
  root.appendChild(toast)
}

// Render sidebar tree
export function renderTree(filter = '') {
  const container = document.getElementById('deviceTree')
  if (!container) return
  
  const startTime = performance.now()
  const isLargeDataset = store.devices.length > VIRTUAL_TABLE_THRESHOLD
  
  const aps = store.aps.filter(d => {
    // Apply status filter
    const status = d.online ? 'online' : (d.db_status === 'offline' ? 'offline' : 'unknown')
    if (status === 'online' && !store.filters.online) return false
    if (status === 'offline' && !store.filters.offline) return false
    if (status === 'unknown' && !store.filters.unknown) return false
    
    // Apply search filter
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
  
  // Sort APs by hostname
  aps.sort((a, b) => (a.hostname || '').localeCompare(b.hostname || ''))
  
  // Helper to strip [AP]/[STA] prefix from hostname
  const stripRolePrefix = (name) => {
    if (!name) return name
    return name.replace(/^\[(AP|STA)\]\s*/i, '')
  }
  
  // For very large datasets, only render AP nodes initially (not children)
  // Children are rendered on-demand when expanded
  container.innerHTML = aps.map(ap => {
    const staCount = store.getSTAs(ap.id).length
    
    // PERF: Default to collapsed for large datasets (500+ devices)
    const shouldExpand = isLargeDataset 
      ? store.treeExpanded[ap.id] === true  // Must explicitly expand
      : store.treeExpanded[ap.id] !== false // Default expanded for small datasets
    const hasChildren = staCount > 0
    const apStatus = ap.online ? 'online' : (ap.db_status === 'offline' ? 'offline' : 'unknown')
    const apName = stripRolePrefix(ap.hostname) || ap.product || ap.ip_address || 'Unknown'
    
    // Only render children if expanded (lazy rendering for performance)
    let childrenHtml = ''
    if (hasChildren && shouldExpand) {
      const stas = store.getSTAs(ap.id).filter(s => {
        const status = s.online ? 'online' : (s.db_status === 'offline' ? 'offline' : 'unknown')
        if (status === 'online' && !store.filters.online) return false
        if (status === 'offline' && !store.filters.offline) return false
        if (status === 'unknown' && !store.filters.unknown) return false
        return true
      })
      childrenHtml = `
        <div class="tree-children">
          ${stas.map(sta => {
            const staStatus = sta.online ? 'online' : (sta.db_status === 'offline' ? 'offline' : 'unknown')
            const staName = stripRolePrefix(sta.hostname) || sta.product || sta.ip_address || 'Unknown STA'
            return `
            <div class="tree-node" data-id="${sta.id}" data-ip="${escapeAttr(sta.ip_address || '')}">
              <div class="tree-node-content ${store.selectedDevice === sta.id ? 'selected' : ''}" data-id="${sta.id}">
                <span class="tree-toggle empty">></span>
                <span class="tree-status ${staStatus}"></span>
                <span class="tree-label">${escapeHTML(staName)}</span>
              </div>
            </div>
          `}).join('')}
        </div>
      `
    } else if (hasChildren) {
      childrenHtml = `<div class="tree-children collapsed"></div>`
    }
    
    return `
      <div class="tree-node" data-id="${ap.id}" data-ip="${escapeAttr(ap.ip_address || '')}">
        <div class="tree-node-content ${store.selectedDevice === ap.id ? 'selected' : ''}" data-id="${ap.id}">
          <span class="tree-toggle ${shouldExpand ? 'expanded' : ''} ${!hasChildren ? 'empty' : ''}" data-id="${ap.id}">></span>
          <span class="tree-status ${apStatus}"></span>
          <span class="tree-label">${escapeHTML(apName)}</span>
        </div>
        ${childrenHtml}
      </div>
    `
  }).join('')
  
  const renderTime = performance.now() - startTime
  if (renderTime > 50) {
    console.log(`renderTree: ${aps.length} APs in ${renderTime.toFixed(1)}ms`)
  }
  
  // Add click handlers
  container.querySelectorAll('.tree-toggle:not(.empty)').forEach(toggle => {
    toggle.addEventListener('click', e => {
      e.stopPropagation()
      const id = parseInt(toggle.dataset.id)
      store.toggleExpanded(id)
      renderTree(filter)
    })
  })
  
  container.querySelectorAll('.tree-node-content').forEach(node => {
    node.addEventListener('click', () => {
      const id = parseInt(node.dataset.id)
      store.set({ selectedDevice: id })
      // Update detail panel
      const device = store.devices.find(d => d.id === id)
      const detailPanel = document.getElementById('detailPanel')
      if (device && detailPanel) {
        renderDeviceDetail(detailPanel, device)
        detailPanel.classList.remove('hidden')
      }
      renderTree(filter)
      scrollToDevice(id)
    })
  })
}

// Scroll to device in table
function scrollToDevice(id) {
  const row = document.querySelector(`tr[data-id="${id}"]`)
  if (row) {
    row.scrollIntoView({ behavior: 'smooth', block: 'center' })
    row.classList.add('highlighted')
    setTimeout(() => row.classList.remove('highlighted'), 2000)
  }
}

// Render device info pane (top panel when device selected)
// Render main device table
export function renderDevices(container) {
  const useVirtual = shouldUseVirtualTable()
  // Avoid noisy logging on large installs; the devices list can re-render
  // frequently (filters/search/page switches) and console logging can
  // noticeably slow down the UI.
  
  // Use virtual scrolling for large datasets (500+ devices)
  if (useVirtual) {
    renderDevicesVirtual(container)
    return
  }
  
  renderDevicesRegular(container)
}

// Regular (non-virtual) device rendering
function renderDevicesRegular(container) {
  const devices = store.filteredDevices
  const query = store.searchQuery.toLowerCase()
  
  // Get selected device
  const selectedDevice = store.selectedDevice 
    ? devices.find(d => d.id === store.selectedDevice) 
    : null
  
  // Filter by search
  const filtered = query ? devices.filter(d => 
    (d.hostname || '').toLowerCase().includes(query) ||
    (d.ip_address || '').toLowerCase().includes(query) ||
    (d.product || '').toLowerCase().includes(query) ||
    (d.mac || '').toLowerCase().includes(query) ||
    (d.flavor || '').toLowerCase().includes(query)
  ) : devices
  
  // Current sort state - null means hierarchical (tree) view
  const sortCol = store.sortColumn
  const sortDir = store.sortDirection || 'asc'
  
  let sorted
  
  // If sorting by column, do flat sort (skip hierarchical grouping)
  if (sortCol) {
    sorted = [...filtered].sort((a, b) => {
      let av, bv
      // Sort by what's displayed in the column (same value regardless of device type)
      switch (sortCol) {
        case 'hostname': av = a.hostname || ''; bv = b.hostname || ''; break
        case 'ip': av = a.ip_address || ''; bv = b.ip_address || ''; break
        case 'mac': av = a.mac || ''; bv = b.mac || ''; break
        case 'product': av = a.product || a.model || ''; bv = b.product || b.model || ''; break
        case 'signal_60ghz': av = a.signal_60ghz || -999; bv = b.signal_60ghz || -999; break
        case 'sta_60ghz': av = getSTASignal60GHz(a) || -999; bv = getSTASignal60GHz(b) || -999; break
        case 'signal_5ghz': av = getSignal5GHz(a) || -999; bv = getSignal5GHz(b) || -999; break
        case 'signal_5ghz_c0': av = getSignal5GHzChain(a, 0) || -999; bv = getSignal5GHzChain(b, 0) || -999; break
        case 'signal_5ghz_c1': av = getSignal5GHzChain(a, 1) || -999; bv = getSignal5GHzChain(b, 1) || -999; break
        case 'sta_5ghz': av = getSTASignal5GHz(a) || -999; bv = getSTASignal5GHz(b) || -999; break
        case 'sta_5ghz_c0': av = getSTASignal5GHzChain(a, 0) || -999; bv = getSTASignal5GHzChain(b, 0) || -999; break
        case 'sta_5ghz_c1': av = getSTASignal5GHzChain(a, 1) || -999; bv = getSTASignal5GHzChain(b, 1) || -999; break
        case 'distance': av = a.distance || 0; bv = b.distance || 0; break
        case 'capacity': av = a.capacity_60ghz || a.capacity_ltu || a.capacity_5ghz || 0; bv = b.capacity_60ghz || b.capacity_ltu || b.capacity_5ghz || 0; break
        case 'firmware': av = a.firmware_version || a.firmware || ''; bv = b.firmware_version || b.firmware || ''; break
        case 'site': av = a.site_name || ''; bv = b.site_name || ''; break
        default: av = a.hostname || ''; bv = b.hostname || ''
      }
      if (typeof av === 'string') {
        return sortDir === 'asc' ? av.localeCompare(bv) : bv.localeCompare(av)
      }
      return sortDir === 'asc' ? av - bv : bv - av
    })
  } else {
	    // No column sort - use hierarchical grouping (root devices first, then their STAs).
	    // Managed devices (added via Add IP/Bulk) are treated as root-level even if they have a parent_id.
	    const aps = filtered.filter(d => !d.parent_id || d.managed)
    const grouped = []
    
    aps.forEach(ap => {
      grouped.push(ap)
	      const stas = filtered.filter(d => d.parent_id === ap.id && !d.managed)
      stas.sort((a, b) => (a.hostname || '').localeCompare(b.hostname || ''))
      grouped.push(...stas)
    })
    
    // Add orphan STAs
	    const orphans = filtered.filter(d => d.parent_id && !d.managed && !aps.find(ap => ap.id === d.parent_id))
    grouped.push(...orphans)
    
    sorted = grouped
  }
  
  const sortIcon = (col) => sortCol === col ? (sortDir === 'asc' ? ' ▲' : ' ▼') : ''
  const cols = store.columns
  
  // Count visible columns for colspan
  let colSpan = 2 // checkbox + actions always visible
  if (cols.status) colSpan++
  if (cols.name) colSpan++
  if (cols.ip) colSpan++
  if (cols.mac) colSpan++
  if (cols.product) colSpan++
  if (cols.site) colSpan++
  if (cols.signal60) colSpan++
  if (cols.sta60) colSpan++
  if (cols.signal5) colSpan++
  if (cols.signal5c0) colSpan++
  if (cols.signal5c1) colSpan++
  if (cols.sta5) colSpan++
  if (cols.sta5c0) colSpan++
  if (cols.sta5c1) colSpan++
  if (cols.dir) colSpan++
  if (cols.health) colSpan++
  if (cols.distance) colSpan++
  if (cols.capacity) colSpan++
  if (cols.firmware) colSpan++
  
  // Column toggle menu
  const columnMenu = `
    <div class="column-menu" id="columnMenu">
      <button class="btn btn-subtle toolbar-mini-btn" id="toggleColumnMenu" title="Column menu" aria-label="Column menu"><span class="toolbar-mini-icon">☰</span></button>
      <div class="column-dropdown hidden" id="columnDropdown">
        <label><input type="checkbox" data-col="status" ${cols.status ? 'checked' : ''}/> Status</label>
        <label><input type="checkbox" data-col="name" ${cols.name ? 'checked' : ''}/> Name</label>
        <label><input type="checkbox" data-col="ip" ${cols.ip ? 'checked' : ''}/> IP Address</label>
        <label><input type="checkbox" data-col="mac" ${cols.mac ? 'checked' : ''}/> MAC Address</label>
        <label><input type="checkbox" data-col="product" ${cols.product ? 'checked' : ''}/> Product</label>
        <label><input type="checkbox" data-col="site" ${cols.site ? 'checked' : ''}/> Site</label>
        <label><input type="checkbox" data-col="signal60" ${cols.signal60 ? 'checked' : ''}/> AP 60GHz</label>
        <label><input type="checkbox" data-col="sta60" ${cols.sta60 ? 'checked' : ''}/> STA 60GHz</label>
        <label><input type="checkbox" data-col="signal5" ${cols.signal5 ? 'checked' : ''}/> AP 5GHz</label>
        <label><input type="checkbox" data-col="signal5c0" ${cols.signal5c0 ? 'checked' : ''}/> AP 5GHz C0</label>
        <label><input type="checkbox" data-col="signal5c1" ${cols.signal5c1 ? 'checked' : ''}/> AP 5GHz C1</label>
        <label><input type="checkbox" data-col="sta5" ${cols.sta5 ? 'checked' : ''}/> STA 5GHz</label>
        <label><input type="checkbox" data-col="sta5c0" ${cols.sta5c0 ? 'checked' : ''}/> STA 5GHz C0</label>
        <label><input type="checkbox" data-col="sta5c1" ${cols.sta5c1 ? 'checked' : ''}/> STA 5GHz C1</label>
        <label><input type="checkbox" data-col="dir" ${cols.dir ? 'checked' : ''}/> Dir Diag</label>
        <label><input type="checkbox" data-col="health" ${cols.health ? 'checked' : ''}/> Health Bars</label>
        <label><input type="checkbox" data-col="distance" ${cols.distance ? 'checked' : ''}/> Distance</label>
        <label><input type="checkbox" data-col="capacity" ${cols.capacity ? 'checked' : ''}/> Capacity</label>
        <label><input type="checkbox" data-col="firmware" ${cols.firmware ? 'checked' : ''}/> Firmware</label>
      </div>
    </div>
  `
  container.innerHTML = `
    <div class="devices-split-view">
      <div class="device-table-wrapper">
        <table class="device-table">
          <thead>
            <tr>
              <th class="cell-checkbox"></th>
              ${cols.status ? '<th class="cell-status"></th>' : ''}
              ${cols.name ? `<th class="cell-name sortable" data-sort="hostname">Name${sortIcon('hostname')}</th>` : ''}
              ${cols.ip ? `<th class="cell-ip sortable" data-sort="ip">IP${sortIcon('ip')}</th>` : ''}
              ${cols.mac ? `<th class="cell-mac sortable" data-sort="mac">MAC${sortIcon('mac')}</th>` : ''}
              ${cols.product ? `<th class="cell-product sortable" data-sort="product">Product${sortIcon('product')}</th>` : ''}
              ${cols.site ? `<th class="cell-site sortable" data-sort="site">Site${sortIcon('site')}</th>` : ''}
              ${cols.signal60 ? `<th class="cell-signal sortable" data-sort="signal_60ghz">AP 60G${sortIcon('signal_60ghz')}</th>` : ''}
              ${cols.sta60 ? `<th class="cell-signal sortable" data-sort="sta_60ghz">STA 60G${sortIcon('sta_60ghz')}</th>` : ''}
              ${cols.signal5 ? `<th class="cell-signal sortable" data-sort="signal_5ghz">AP 5G${sortIcon('signal_5ghz')}</th>` : ''}
              ${cols.signal5c0 ? `<th class="cell-signal sortable" data-sort="signal_5ghz_c0">AP C0${sortIcon('signal_5ghz_c0')}</th>` : ''}
              ${cols.signal5c1 ? `<th class="cell-signal sortable" data-sort="signal_5ghz_c1">AP C1${sortIcon('signal_5ghz_c1')}</th>` : ''}
              ${cols.sta5 ? `<th class="cell-signal sortable" data-sort="sta_5ghz">STA 5G${sortIcon('sta_5ghz')}</th>` : ''}
              ${cols.sta5c0 ? `<th class="cell-signal sortable" data-sort="sta_5ghz_c0">STA C0${sortIcon('sta_5ghz_c0')}</th>` : ''}
              ${cols.sta5c1 ? `<th class="cell-signal sortable" data-sort="sta_5ghz_c1">STA C1${sortIcon('sta_5ghz_c1')}</th>` : ''}
              ${cols.dir ? `<th class="cell-dir">Dir</th>` : ''}
              ${cols.health ? '<th class="cell-health">Health</th>' : ''}
              ${cols.distance ? `<th class="cell-distance sortable" data-sort="distance">Dist${sortIcon('distance')}</th>` : ''}
              ${cols.capacity ? `<th class="cell-capacity sortable" data-sort="capacity">Capacity${sortIcon('capacity')}</th>` : ''}
              ${cols.firmware ? `<th class="cell-firmware sortable" data-sort="firmware">Firmware${sortIcon('firmware')}</th>` : ''}
              <th class="cell-actions cell-header-tools">${buildDashboardHeaderTools(sorted.length, store.devices.length, cols)}</th>
            </tr>
          </thead>
          <tbody>
            ${sorted.map(d => renderDeviceRow(d, cols)).join('')}
            ${sorted.length === 0 ? `<tr><td colspan="${colSpan}" class="empty-state">No devices found</td></tr>` : ''}
          </tbody>
        </table>
        ${buildDashboardScopeFooter(sorted.length, store.devices.length)}
      </div>
    </div>
  `
  
  setupDashboardScopeHandlers(container)

  setupColumnMenuHandlers(container)

  // Sort header click handlers
  container.querySelectorAll('th.sortable').forEach(th => {
    th.addEventListener('click', (e) => {
      const col = th.dataset.sort
      if (store.sortColumn === col) {
        // Toggle direction, or clear sort to return to hierarchy
        if (store.sortDirection === 'desc') {
          store.set({ sortColumn: null, sortDirection: 'asc' })
        } else {
          store.set({ sortDirection: 'desc' })
        }
      } else {
        store.set({ sortColumn: col, sortDirection: 'asc' })
      }
      // Preserve scroll position during re-render
      const tableWrapper = container.querySelector('.device-table-wrapper')
      const scrollTop = tableWrapper?.scrollTop || 0
      renderDevices(container)
      const newWrapper = container.querySelector('.device-table-wrapper')
      if (newWrapper) newWrapper.scrollTop = scrollTop
    })
  })
  
  // Row click for detail - show info pane
  container.querySelectorAll('tbody tr[data-id]').forEach(row => {
    row.addEventListener('click', e => {
      if (e.target.tagName === 'BUTTON' || e.target.tagName === 'INPUT') return
      const id = parseInt(row.dataset.id)
      // Toggle selection - clicking same device again closes the pane
      if (store.selectedDevice === id) {
        store.set({ selectedDevice: null })
        document.getElementById('detailPanel')?.classList.add('hidden')
      } else {
        store.set({ selectedDevice: id })
        // Immediately update detail panel
        const device = store.devices.find(d => d.id === id)
        const detailPanel = document.getElementById('detailPanel')
        if (device && detailPanel) {
          renderDeviceDetail(detailPanel, device)
          detailPanel.classList.remove('hidden')
        }
      }
      // Preserve scroll position during re-render
      const tableWrapper = container.querySelector('.device-table-wrapper')
      const scrollTop = tableWrapper?.scrollTop || 0
      renderDevices(container)
      // Restore scroll position after render
      const newWrapper = container.querySelector('.device-table-wrapper')
      if (newWrapper) newWrapper.scrollTop = scrollTop
    })
  })
  
  // Row actions
  container.querySelectorAll('.btn-refresh').forEach(btn => {
    btn.addEventListener('click', async e => {
      e.stopPropagation()
      const id = parseInt(btn.dataset.id)
      try {
        btn.disabled = true
        await api.refreshDevice(id)
        showToast('Refreshing...', 'info')
        setTimeout(async () => {
          const devices = await api.devices()
          store.set({ devices })
          renderTree()
          renderDevices(container)
        }, 2000)
      } catch (e) {
        showToast('Refresh failed: ' + e.message, 'error')
        btn.disabled = false
      }
    })
  })
  
  container.querySelectorAll('.btn-upgrade').forEach(btn => {
    btn.addEventListener('click', e => {
      e.stopPropagation()
      const id = parseInt(btn.dataset.id)
      if (window.showUpgradeModal) window.showUpgradeModal(id)
    })
  })
  
  container.querySelectorAll('.btn-delete').forEach(btn => {
    btn.addEventListener('click', async e => {
      e.stopPropagation()
      const id = parseInt(btn.dataset.id)
      const confirmed = await requestConfirmation(
        'This device and its WaveControl inventory record will be removed. Discovered child devices may also disappear from the current view.',
        {
          title: 'Delete device?',
          eyebrow: 'Inventory change',
          confirmText: 'Delete device',
          tone: 'danger',
          calloutTitle: 'This action removes the selected device from WaveControl.'
        }
      )
      if (!confirmed) return
      
      try {
        await api.deleteDevice(id)
        const devices = await api.devices()
        store.set({ devices, selectedDevice: null })
        renderTree()
        renderDevices(container)
        showToast('Device deleted', 'success')
      } catch (e) {
        showToast('Delete failed: ' + e.message, 'error')
      }
    })
  })
}

// Get 5GHz signal (combined from chains) - works for Wave, airMAX, LTU
// Uses pre-computed signal_combined from Go when available
function getSignal5GHz(device) {
  // Prefer pre-computed combined signal from server (Go > JS)
  if (device.radio_5ghz?.signal_combined) return device.radio_5ghz.signal_combined
  if (device.radio_ltu?.signal_combined) return device.radio_ltu.signal_combined
  
  // Fallback to single signal value
  return device.signal_5ghz || device.signal_ltu || device.signal_airmax || 
         device.radio_5ghz?.signal || device.radio_ltu?.signal || 0
}

// Get specific chain signal
function getSignal5GHzChain(device, chainIdx) {
  const chains = getSignal5GHzChains(device)
  return chains[chainIdx] || 0
}

// Get all 5GHz chain values from device
function getSignal5GHzChains(device) {
  // Wave 5GHz backup
  if (device.radio_5ghz?.signal_per_chain?.length > 0) {
    return device.radio_5ghz.signal_per_chain
  }
  // LTU
  if (device.radio_ltu?.signal_per_chain?.length > 0) {
    return device.radio_ltu.signal_per_chain
  }
  // airMAX - use signal_per_chain from device level
  if (device.signal_per_chain?.length > 0) {
    return device.signal_per_chain
  }
  // Fallback to single signal value
  const sig = device.signal_5ghz || device.signal_ltu || device.signal_airmax || 0
  return sig ? [sig] : []
}

// Get STA 5GHz signal (remote - what STA receives from AP)
// Uses pre-computed remote_signal_combined from Go when available
function getSTASignal5GHz(device) {
  // Prefer pre-computed combined signal from server (Go > JS)
  if (device.radio_5ghz?.remote_signal_combined) return device.radio_5ghz.remote_signal_combined
  if (device.radio_ltu?.remote_signal_combined) return device.radio_ltu.remote_signal_combined
  if (device.remote_signal_combined) return device.remote_signal_combined
  
  // Fallback to single remote signal value
  return device.radio_5ghz?.remote_signal || device.radio_ltu?.remote_signal || device.remote_signal || 0
}

function getSTASignal5GHzChain(device, chainIdx) {
  const chains = getSTASignal5GHzChains(device)
  return chains[chainIdx] || 0
}

function getSTASignal5GHzChains(device) {
  // Wave 5GHz remote signal
  if (device.radio_5ghz?.remote_signal_per_chain?.length > 0) {
    return device.radio_5ghz.remote_signal_per_chain
  }
  // LTU remote signal
  if (device.radio_ltu?.remote_signal_per_chain?.length > 0) {
    return device.radio_ltu.remote_signal_per_chain
  }
  // airMAX - remote_signal_per_chain from device level
  if (device.remote_signal_per_chain?.length > 0) {
    return device.remote_signal_per_chain
  }
  // Fallback to single remote signal value
  const remoteSig = device.radio_5ghz?.remote_signal || 
                    device.radio_ltu?.remote_signal || 
                    device.remote_signal || 0
  return remoteSig ? [remoteSig] : []
}

// Get STA 60GHz signal (remote - what STA receives from AP)
function getSTASignal60GHz(device) {
  // Wave 60GHz remote signal
  if (device.radio_60ghz?.remote_signal) {
    return device.radio_60ghz.remote_signal
  }
  return 0
}

// ==========================================================================
// Virtual Table Renderer - for 500+ devices
// ==========================================================================

// Singleton virtual table instance
let virtualTableInstance = null

// Render devices using virtual scrolling
function renderDevicesVirtual(container) {
  try {
    const cols = store.columns
    const sortCol = store.sortColumn
    const sortDir = store.sortDirection || 'asc'
    
    // Column menu (same as regular render)
    const columnMenu = buildColumnMenu(cols)
    
    // Build or reuse virtual table
    if (!virtualTableInstance) {
      container.innerHTML = `
        <div class="devices-split-view">
          <div class="device-table-wrapper virtual-mode">
            <div id="virtualTableContainer"></div>
            ${buildDashboardScopeFooter(getSortedFilteredDevices().length, store.devices.length)}
          </div>
        </div>
      `
      
      const vtContainer = document.getElementById('virtualTableContainer')
      if (!vtContainer) {
        console.error('VirtualTable: container #virtualTableContainer not found')
        renderDevicesRegular(container)
        return
      }
      
      // Create new VirtualTable instance
      virtualTableInstance = new VirtualTable({
        container: vtContainer,
        rowHeight: 40,
        bufferSize: 100,
        renderRow: (d) => renderDeviceRowContent(d, cols),
        renderHeader: () => renderVirtualHeader(cols, sortCol, sortDir),
        onRowClick: handleVirtualRowClick,
        getRowId: d => d.id,
        getRowIp: d => d.ip_address
      })
      
      // Enable debug logging temporarily
      // virtualTableInstance.debug = true
      
      virtualTableInstance.mount()
      
      // Store reference for WebSocket batcher
      setVirtualTableRef(virtualTableInstance)
      
      // Set up column menu handlers
      setupColumnMenuHandlers(container)
    } else {
      // Update header if sort changed
      const thead = container.querySelector('thead')
      if (thead) {
        thead.innerHTML = renderVirtualHeader(cols, sortCol, sortDir)
      }
    }
    
    setupDashboardScopeHandlers(container)
    setupColumnMenuHandlers(container)

    // Get sorted/filtered data and update virtual table
    const data = getSortedFilteredDevices()
    virtualTableInstance.setData(data)
    const footer = container.querySelector('.dashboard-status-foot')
    if (footer) footer.outerHTML = buildDashboardScopeFooter(data.length, store.devices.length)
    
    // Set up sort click handlers on header
    container.querySelectorAll('th.sortable').forEach(th => {
      th.onclick = () => handleVirtualSort(th.dataset.sort, container)
    })
  } catch (err) {
    console.error('VirtualTable error:', err)
    virtualTableInstance = null
    setVirtualTableRef(null)
    renderDevicesRegular(container)
  }
}

// Build column menu HTML

function buildColumnMenu(cols) {
  return `
    <div class="column-menu">
      <button class="btn btn-subtle toolbar-mini-btn js-toggle-column-menu" title="Column menu" aria-label="Column menu" type="button"><span class="toolbar-mini-icon">☰</span></button>
      <div class="column-dropdown hidden">
        <label><input type="checkbox" data-col="status" ${cols.status ? 'checked' : ''}/> Status</label>
        <label><input type="checkbox" data-col="name" ${cols.name ? 'checked' : ''}/> Name</label>
        <label><input type="checkbox" data-col="ip" ${cols.ip ? 'checked' : ''}/> IP Address</label>
        <label><input type="checkbox" data-col="mac" ${cols.mac ? 'checked' : ''}/> MAC Address</label>
        <label><input type="checkbox" data-col="product" ${cols.product ? 'checked' : ''}/> Product</label>
        <label><input type="checkbox" data-col="site" ${cols.site ? 'checked' : ''}/> Site</label>
        <label><input type="checkbox" data-col="signal60" ${cols.signal60 ? 'checked' : ''}/> AP 60GHz</label>
        <label><input type="checkbox" data-col="sta60" ${cols.sta60 ? 'checked' : ''}/> STA 60GHz</label>
        <label><input type="checkbox" data-col="signal5" ${cols.signal5 ? 'checked' : ''}/> AP 5GHz</label>
        <label><input type="checkbox" data-col="signal5c0" ${cols.signal5c0 ? 'checked' : ''}/> AP 5GHz C0</label>
        <label><input type="checkbox" data-col="signal5c1" ${cols.signal5c1 ? 'checked' : ''}/> AP 5GHz C1</label>
        <label><input type="checkbox" data-col="sta5" ${cols.sta5 ? 'checked' : ''}/> STA 5GHz</label>
        <label><input type="checkbox" data-col="sta5c0" ${cols.sta5c0 ? 'checked' : ''}/> STA 5GHz C0</label>
        <label><input type="checkbox" data-col="sta5c1" ${cols.sta5c1 ? 'checked' : ''}/> STA 5GHz C1</label>
        <label><input type="checkbox" data-col="dir" ${cols.dir ? 'checked' : ''}/> Dir Diag</label>
        <label><input type="checkbox" data-col="health" ${cols.health ? 'checked' : ''}/> Health Bars</label>
        <label><input type="checkbox" data-col="distance" ${cols.distance ? 'checked' : ''}/> Distance</label>
        <label><input type="checkbox" data-col="capacity" ${cols.capacity ? 'checked' : ''}/> Capacity</label>
        <label><input type="checkbox" data-col="firmware" ${cols.firmware ? 'checked' : ''}/> Firmware</label>
      </div>
    </div>
  `
}

// Render header for virtual table
function renderVirtualHeader(cols, sortCol, sortDir) {
  const sortIcon = (col) => sortCol === col ? (sortDir === 'asc' ? ' ▲' : ' ▼') : ''
  
  return `
    <tr>
      <th class="cell-checkbox"></th>
      ${cols.status ? '<th class="cell-status"></th>' : ''}
      ${cols.name ? `<th class="cell-name sortable" data-sort="hostname">Name${sortIcon('hostname')}</th>` : ''}
      ${cols.ip ? `<th class="cell-ip sortable" data-sort="ip">IP${sortIcon('ip')}</th>` : ''}
      ${cols.mac ? `<th class="cell-mac sortable" data-sort="mac">MAC${sortIcon('mac')}</th>` : ''}
      ${cols.product ? `<th class="cell-product sortable" data-sort="product">Product${sortIcon('product')}</th>` : ''}
      ${cols.site ? `<th class="cell-site sortable" data-sort="site">Site${sortIcon('site')}</th>` : ''}
      ${cols.signal60 ? `<th class="cell-signal sortable" data-sort="signal_60ghz">AP 60G${sortIcon('signal_60ghz')}</th>` : ''}
      ${cols.sta60 ? `<th class="cell-signal sortable" data-sort="sta_60ghz">STA 60G${sortIcon('sta_60ghz')}</th>` : ''}
      ${cols.signal5 ? `<th class="cell-signal sortable" data-sort="signal_5ghz">AP 5G${sortIcon('signal_5ghz')}</th>` : ''}
      ${cols.signal5c0 ? `<th class="cell-signal sortable" data-sort="signal_5ghz_c0">AP C0${sortIcon('signal_5ghz_c0')}</th>` : ''}
      ${cols.signal5c1 ? `<th class="cell-signal sortable" data-sort="signal_5ghz_c1">AP C1${sortIcon('signal_5ghz_c1')}</th>` : ''}
      ${cols.sta5 ? `<th class="cell-signal sortable" data-sort="sta_5ghz">STA 5G${sortIcon('sta_5ghz')}</th>` : ''}
      ${cols.sta5c0 ? `<th class="cell-signal sortable" data-sort="sta_5ghz_c0">STA C0${sortIcon('sta_5ghz_c0')}</th>` : ''}
      ${cols.sta5c1 ? `<th class="cell-signal sortable" data-sort="sta_5ghz_c1">STA C1${sortIcon('sta_5ghz_c1')}</th>` : ''}
      ${cols.dir ? '<th class="cell-dir">Dir</th>' : ''}
      ${cols.health ? '<th class="cell-health">Health</th>' : ''}
      ${cols.distance ? `<th class="cell-distance sortable" data-sort="distance">Dist${sortIcon('distance')}</th>` : ''}
      ${cols.capacity ? `<th class="cell-capacity sortable" data-sort="capacity">Capacity${sortIcon('capacity')}</th>` : ''}
      ${cols.firmware ? `<th class="cell-firmware sortable" data-sort="firmware">Firmware${sortIcon('firmware')}</th>` : ''}
      <th class="cell-actions cell-header-tools">${buildDashboardHeaderTools(getSortedFilteredDevices().length, store.devices.length, cols)}</th>
    </tr>
  `
}

// Render row content (innerHTML without <tr> wrapper) for virtual table
function renderDeviceRowContent(device, cols) {
  const isSTA = !!device.parent_id && !device.managed
  const isOnline = device.online
  const status = isOnline ? 'online' : (device.db_status === 'offline' ? 'offline' : 'unknown')
  
  // Device name - prefer hostname, then product+IP combo, then just IP
  const deviceName = escapeHTML(device.hostname || (device.product ? `${device.product} (${device.ip_address})` : device.ip_address) || device.mac || 'Unknown')

  // APs are already visually top-level, so only directly managed stations
  // need role badges. Alertability is shown in the host pane rather than as a
  // persistent dashboard pill; temporary silences remain visible here.
  const inferredRole = String(device.role || (device.parent_id ? 'sta' : 'ap')).toLowerCase()
  const directBadge = (device.managed && inferredRole !== 'ap')
    ? `<span class="direct-badge" title="Directly managed (Add IP/Bulk)">DIRECT</span>`
    : ''
  const managedStaBadge = (device.managed && inferredRole === 'sta') ? `<span class="role-badge sta">STA</span>` : ''
  const alertSilenced = device.alert_silenced_until && new Date(device.alert_silenced_until).getTime() > Date.now()
  const alertSilencedBadge = alertSilenced ? `<span class="role-badge muted" title="Alerts temporarily silenced">SILENCED</span>` : ''

  // 60GHz signal - prefer server-computed quality (Go > JS)
  const signal60 = device.signal_60ghz || 0
  const signal60Display = signal60 ? `${signal60} dBm` : '-'
  const signal60Quality = device.radio_60ghz?.signal_quality
  const signal60Class = signal60 ? getSignalClassFromQuality(signal60Quality, signal60, '60ghz') : ''
  
  // STA 60GHz signal (remote)
  const sta60 = getSTASignal60GHz(device)
  const sta60Quality = device.radio_60ghz?.remote_signal_quality
  const colSta60 = cols.sta60 ? `<td class="cell-signal ${sta60 ? getSignalClassFromQuality(sta60Quality, sta60, '60ghz') : ''}">${sta60 ? `${sta60} dBm` : '-'}</td>` : ''
  
  // 5GHz signals - prefer server-computed quality
  const chains = getSignal5GHzChains(device)
  const signal5Combined = getSignal5GHz(device)
  const signal5Quality = device.radio_5ghz?.signal_quality || device.radio_ltu?.signal_quality
  const signal5C0 = chains[0] || 0
  const signal5C1 = chains[1] || 0
  
  const col5Combined = cols.signal5 ? `<td class="cell-signal cell-signal-5ghz ${signal5Combined ? getSignalClassFromQuality(signal5Quality, signal5Combined, '5ghz') : ''}">${signal5Combined ? `${signal5Combined} dBm` : '-'}</td>` : ''
  // Per-chain: no server quality, use JS (but these are rarely shown)
  const col5C0 = cols.signal5c0 ? `<td class="cell-signal ${signal5C0 ? getSignalClass5(signal5C0) : ''}">${signal5C0 || '-'}</td>` : ''
  const col5C1 = cols.signal5c1 ? `<td class="cell-signal ${signal5C1 ? getSignalClass5(signal5C1) : ''}">${signal5C1 || '-'}</td>` : ''
  
  // STA 5GHz (remote)
  const staChains = getSTASignal5GHzChains(device)
  const sta5Combined = getSTASignal5GHz(device)
  const sta5Quality = device.radio_5ghz?.remote_signal_quality || device.radio_ltu?.remote_signal_quality || device.remote_signal_quality
  const sta5C0 = staChains[0] || 0
  const sta5C1 = staChains[1] || 0
  
  const colSta5Combined = cols.sta5 ? `<td class="cell-signal ${sta5Combined ? getSignalClassFromQuality(sta5Quality, sta5Combined, '5ghz') : ''}">${sta5Combined ? `${sta5Combined} dBm` : '-'}</td>` : ''
  // Per-chain: no server quality, use JS
  const colSta5C0 = cols.sta5c0 ? `<td class="cell-signal ${sta5C0 ? getSignalClass5(sta5C0) : ''}">${sta5C0 || '-'}</td>` : ''
  const colSta5C1 = cols.sta5c1 ? `<td class="cell-signal ${sta5C1 ? getSignalClass5(sta5C1) : ''}">${sta5C1 || '-'}</td>` : ''

  // Directional diagnosis (DL/UL)
  const colDir = cols.dir ? `<td class="cell-dir">${renderDirectionalCell(device)}</td>` : ''
  
  // Health
  const healthSignal = signal60 || signal5Combined || 0
  const healthBand = signal60 ? '60ghz' : '5ghz'
  const healthCol = cols.health ? `<td class="cell-health">${getSignalBars(healthSignal, healthBand)}</td>` : ''
  
  // Other columns
  const siteName = escapeHTML(device.site_name || '-')
  const distanceStr = device.distance ? `${(device.distance / 1000).toFixed(2)} km` : '-'
  const capacity = device.capacity_60ghz || device.capacity_ltu || device.capacity_5ghz || 0
  const capacityStr = capacity ? `${(capacity / 1e6).toFixed(0)} Mbps` : '-'
  const firmware = escapeHTML(getDisplayFirmware(device))
  const escapedMAC = escapeHTML(device.mac || '-')
  const escapedProduct = escapeHTML(device.product || device.model || '-')
  
  return `
    <td class="cell-checkbox"><input type="checkbox" data-id="${device.id}" /></td>
    ${cols.status !== false ? `<td class="cell-status"><span class="status-dot ${status}" title="${escapeAttr(store.getStatusReason(device) || '')}"></span></td>` : ''}
    ${cols.name !== false ? `
      <td class="cell-name">
        ${isSTA ? '<span class="sta-indent">+-</span>' : ''}
        <span class="device-name">${deviceName}</span>${directBadge}${managedStaBadge}${alertSilencedBadge}
      </td>
    ` : ''}
    ${cols.ip !== false ? `<td class="cell-ip">${escapeHTML(device.ip_address || '-')}</td>` : ''}
    ${cols.mac ? `<td class="cell-mac">${escapedMAC}</td>` : ''}
    ${cols.product !== false ? `<td class="cell-product">${escapedProduct}</td>` : ''}
    ${cols.site !== false ? `<td class="cell-site">${siteName}</td>` : ''}
    ${cols.signal60 !== false ? `<td class="cell-signal cell-signal-60 ${signal60Class}">${signal60Display}</td>` : ''}
    ${colSta60}
    ${col5Combined}
    ${col5C0}
    ${col5C1}
    ${colSta5Combined}
    ${colSta5C0}
    ${colSta5C1}
    ${colDir}
    ${healthCol}
    ${cols.distance !== false ? `<td class="cell-distance">${distanceStr}</td>` : ''}
    ${cols.capacity !== false ? `<td class="cell-capacity">${capacityStr}</td>` : ''}
    ${cols.firmware !== false ? `<td class="cell-firmware">${firmware}</td>` : ''}
    <td class="cell-actions">
      <button class="btn btn-icon-xs btn-refresh" data-id="${device.id}" title="Refresh"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M23 4v6h-6M1 20v-6h6"/><path d="M3.51 9a9 9 0 0114.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0020.49 15"/></svg></button>
      <button class="btn btn-icon-xs btn-upgrade" data-id="${device.id}" title="Upgrade"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 01-2 2H5a2 2 0 01-2-2v-4M17 8l-5-5-5 5M12 3v12"/></svg></button>
      <button class="btn btn-icon-xs btn-danger btn-delete" data-id="${device.id}" title="Delete"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 6h18M19 6v14a2 2 0 01-2 2H7a2 2 0 01-2-2V6m3 0V4a2 2 0 012-2h4a2 2 0 012 2v2"/></svg></button>
    </td>
  `
}

// Handle row click in virtual table
function handleVirtualRowClick(device, tr, e) {
  // Clicks on SVG paths inside buttons have tagName of 'path'/'svg'. Use closest()
  // so action buttons work reliably.
  const btn = e.target.closest('button')
  if (btn) {
    if (btn.classList.contains('btn-refresh')) {
      handleRefreshClick(device.id)
    } else if (btn.classList.contains('btn-upgrade')) {
      if (window.showUpgradeModal) window.showUpgradeModal(device.id)
    } else if (btn.classList.contains('btn-delete')) {
      handleDeleteClick(device.id)
    }
    return
  }
  // Don't toggle selection when clicking the checkbox
  if (e.target.closest('input')) return
  
  // Toggle selection
  if (store.selectedDevice === device.id) {
    store.set({ selectedDevice: null })
    document.getElementById('detailPanel')?.classList.add('hidden')
  } else {
    store.set({ selectedDevice: device.id })
    const detailPanel = document.getElementById('detailPanel')
    if (detailPanel) {
      renderDeviceDetail(detailPanel, device)
      detailPanel.classList.remove('hidden')
    }
  }
}

// Handle context menu in virtual table
function handleVirtualRowContext(device, tr, e) {
  e.preventDefault()
  // TODO: Show context menu
}

// Handle sort click in virtual table
function handleVirtualSort(col, container) {
  if (store.sortColumn === col) {
    if (store.sortDirection === 'desc') {
      store.set({ sortColumn: null, sortDirection: 'asc' })
    } else {
      store.set({ sortDirection: 'desc' })
    }
  } else {
    store.set({ sortColumn: col, sortDirection: 'asc' })
  }
  
  // Re-render with new sort
  if (virtualTableInstance) {
    const data = getSortedFilteredDevices()
    virtualTableInstance.setData(data)
    const footer = container.querySelector('.dashboard-status-foot')
    if (footer) footer.outerHTML = buildDashboardScopeFooter(data.length, store.devices.length)
    
    // Update header
    const thead = container.querySelector('thead')
    if (thead) {
      thead.innerHTML = renderVirtualHeader(store.columns, store.sortColumn, store.sortDirection)
      container.querySelectorAll('th.sortable').forEach(th => {
        th.onclick = () => handleVirtualSort(th.dataset.sort, container)
      })
    }
  }
}


// Set up column menu handlers
function setupColumnMenuHandlers(container) {
  if (!container || container.dataset.columnMenuBound === 'true') return
  container.dataset.columnMenuBound = 'true'

  const closeAllColumnMenus = () => {
    container.querySelectorAll('.column-dropdown').forEach(el => {
      el.classList.add('hidden')
      resetHeaderDropdownPosition(el)
    })
  }

  const wrapper = container.querySelector('.device-table-wrapper')
  wrapper?.addEventListener('scroll', closeAllColumnMenus, { passive: true })
  window.addEventListener('resize', closeAllColumnMenus)

  container.addEventListener('click', (e) => {
    const toggle = e.target.closest('.js-toggle-column-menu')
    if (toggle) {
      e.preventDefault()
      e.stopPropagation()
      const menu = toggle.closest('.column-menu')
      const dropdown = menu?.querySelector('.column-dropdown')
      if (!dropdown) return
      const willOpen = dropdown.classList.contains('hidden')
      closeAllColumnMenus()
      if (willOpen) positionHeaderDropdown(toggle, dropdown)
      return
    }

    if (!e.target.closest('.column-menu') && !e.target.closest('.column-dropdown')) {
      closeAllColumnMenus()
    }
  })

  container.addEventListener('change', (e) => {
    const cb = e.target.closest('.column-dropdown input[data-col]')
    if (!cb) return
    e.stopPropagation()
    store.toggleColumn(cb.dataset.col)
    cleanupVirtualTableInstance()
    renderDevices(container)
    const reopenedToggle = container.querySelector('.js-toggle-column-menu')
    const reopened = container.querySelector('.column-dropdown')
    if (reopenedToggle && reopened) positionHeaderDropdown(reopenedToggle, reopened)
  })
}

// Cleanup virtual table instance
function cleanupVirtualTableInstance() {
  if (virtualTableInstance) {
    try {
      virtualTableInstance.destroy()
    } catch (e) {
      console.warn('Error destroying virtual table:', e)
    }
    virtualTableInstance = null
    setVirtualTableRef(null)
  }
}

// Handle refresh button click
async function handleRefreshClick(deviceId) {
  try {
    await api.refreshDevice(deviceId)
    showToast('Refreshing...', 'info')
  } catch (e) {
    showToast('Refresh failed: ' + e.message, 'error')
  }
}

// Handle delete button click  
async function handleDeleteClick(deviceId) {
  const confirmed = await requestConfirmation(
    'This device and its WaveControl inventory record will be removed. Discovered child devices may also disappear from the current view.',
    {
      title: 'Delete device?',
      eyebrow: 'Inventory change',
      confirmText: 'Delete device',
      tone: 'danger',
      calloutTitle: 'This action removes the selected device from WaveControl.'
    }
  )
  if (!confirmed) return
  
  try {
    await api.deleteDevice(deviceId)

    // IMPORTANT: Avoid re-fetching the entire /api/devices list after a delete.
    // On large installs (5k+), that can cause rate limiting and UI stalls.
    // Instead, remove the device locally and let the next poll refresh resync.
    const current = store.devices || []
    const newDevices = current.filter(d => d.id !== deviceId && d.parent_id !== deviceId)

    // If the selected device (or its parent) was removed, clear selection.
    const selected = store.selectedDevice
    const selectedStillExists = selected ? newDevices.some(d => d.id === selected) : true
    store.set({ devices: newDevices, selectedDevice: selectedStillExists ? selected : null })

    // Update tree + virtual table without tearing down the scroll container.
    try { renderTree() } catch (e) {}
    if (virtualTableInstance) {
      virtualTableInstance.setData(getSortedFilteredDevices())
    }

    // Hide detail panel if selection was cleared
    if (!selectedStillExists) {
      document.getElementById('detailPanel')?.classList.add('hidden')
    }

    // Update header counts (throttled)
    triggerUpdateCounts()
    showToast('Device deleted', 'success')
  } catch (e) {
    showToast('Delete failed: ' + e.message, 'error')
  }
}

// Destroy virtual table on page navigation
export function cleanupVirtualTable() {
  cleanupVirtualTableInstance()
}

// ==========================================================================
// End Virtual Table Renderer
// ==========================================================================

// Render a single device row
function renderDeviceRow(device, cols = {}) {
  const isSTA = !!device.parent_id && !device.managed
  const isOnline = device.online
  const status = isOnline ? 'online' : (device.db_status === 'offline' ? 'offline' : 'unknown')
  
  // Device name - prefer hostname, then product+IP combo, then just IP
  const deviceName = escapeHTML(device.hostname || (device.product ? `${device.product} (${device.ip_address})` : device.ip_address) || device.mac || 'Unknown')

  // APs are already visually top-level, so only directly managed stations
  // need role badges. Alertability is shown in the host pane rather than as a
  // persistent dashboard pill; temporary silences remain visible here.
  const inferredRole = String(device.role || (device.parent_id ? 'sta' : 'ap')).toLowerCase()
  const directBadge = (device.managed && inferredRole !== 'ap')
    ? `<span class="direct-badge" title="Directly managed (Add IP/Bulk)">DIRECT</span>`
    : ''
  const managedStaBadge = (device.managed && inferredRole === 'sta') ? `<span class="role-badge sta">STA</span>` : ''
  const alertSilenced = device.alert_silenced_until && new Date(device.alert_silenced_until).getTime() > Date.now()
  const alertSilencedBadge = alertSilenced ? `<span class="role-badge muted" title="Alerts temporarily silenced">SILENCED</span>` : ''
  
  // 60GHz signal (Wave devices) - prefer server-computed quality
  const signal60 = device.signal_60ghz || 0
  const signal60Display = signal60 ? `${signal60} dBm` : '-'
  const signal60Quality = device.radio_60ghz?.signal_quality
  const signal60Class = signal60 ? getSignalClassFromQuality(signal60Quality, signal60, '60ghz') : ''
  
  // STA 60GHz signal (remote - what STA receives from AP)
  const sta60 = getSTASignal60GHz(device)
  const sta60Quality = device.radio_60ghz?.remote_signal_quality
  const colSta60 = cols.sta60 ? (() => {
    const display = sta60 ? `${sta60} dBm` : '-'
    const cls = sta60 ? getSignalClassFromQuality(sta60Quality, sta60, '60ghz') : ''
    return `<td class="cell-signal cell-signal-sta60 ${cls}">${display}</td>`
  })() : ''
  
  // 5GHz signals (all platforms: Wave backup, airMAX, LTU) - prefer server-computed quality
  const chains = getSignal5GHzChains(device)
  const signal5Combined = getSignal5GHz(device)
  const signal5Quality = device.radio_5ghz?.signal_quality || device.radio_ltu?.signal_quality
  const signal5C0 = chains[0] || 0
  const signal5C1 = chains[1] || 0
  
  // 5GHz combined column
  const col5Combined = cols.signal5 ? (() => {
    const display = signal5Combined ? `${signal5Combined} dBm` : '-'
    const cls = signal5Combined ? getSignalClassFromQuality(signal5Quality, signal5Combined, '5ghz') : ''
    return `<td class="cell-signal cell-signal-5ghz ${cls}">${display}</td>`
  })() : ''
  
  // 5GHz C0 column (no server quality for per-chain)
  const col5C0 = cols.signal5c0 ? (() => {
    const display = signal5C0 ? `${signal5C0}` : '-'
    const cls = signal5C0 ? getSignalClass5(signal5C0) : ''
    return `<td class="cell-signal cell-signal-c0 ${cls}">${display}</td>`
  })() : ''
  
  // 5GHz C1 column (no server quality for per-chain)
  const col5C1 = cols.signal5c1 ? (() => {
    const display = signal5C1 ? `${signal5C1}` : '-'
    const cls = signal5C1 ? getSignalClass5(signal5C1) : ''
    return `<td class="cell-signal cell-signal-c1 ${cls}">${display}</td>`
  })() : ''
  
  // STA 5GHz signals (remote - what STA receives from AP)
  const staChains = getSTASignal5GHzChains(device)
  const sta5Combined = getSTASignal5GHz(device)
  const sta5Quality = device.radio_5ghz?.remote_signal_quality || device.radio_ltu?.remote_signal_quality || device.remote_signal_quality
  const sta5C0 = staChains[0] || 0
  const sta5C1 = staChains[1] || 0
  
  // STA 5GHz combined column
  const colSta5Combined = cols.sta5 ? (() => {
    const display = sta5Combined ? `${sta5Combined} dBm` : '-'
    const cls = sta5Combined ? getSignalClassFromQuality(sta5Quality, sta5Combined, '5ghz') : ''
    return `<td class="cell-signal cell-signal-sta5 ${cls}">${display}</td>`
  })() : ''
  
  // STA 5GHz C0 column (no server quality for per-chain)
  const colSta5C0 = cols.sta5c0 ? (() => {
    const display = sta5C0 ? `${sta5C0}` : '-'
    const cls = sta5C0 ? getSignalClass5(sta5C0) : ''
    return `<td class="cell-signal cell-signal-sta-c0 ${cls}">${display}</td>`
  })() : ''
  
  // STA 5GHz C1 column (no server quality for per-chain)
  const colSta5C1 = cols.sta5c1 ? (() => {
    const display = sta5C1 ? `${sta5C1}` : '-'
    const cls = sta5C1 ? getSignalClass5(sta5C1) : ''
    return `<td class="cell-signal cell-signal-sta-c1 ${cls}">${display}</td>`
  })() : ''

  // Directional diagnosis (DL/UL) - computed client-side from CINR/SNR/EVM if available
  const colDir = cols.dir ? `<td class="cell-dir">${renderDirectionalCell(device)}</td>` : ''
  
  // Health column - signal bars for primary signal
  const healthCol = cols.health ? (() => {
    const primarySignal = signal60 || signal5Combined || 0
    const band = signal60 ? '60ghz' : '5ghz'
    return `<td class="cell-health">${getSignalBars(primarySignal, band)}</td>`
  })() : ''
  
  // Site - escape for XSS protection
  const siteName = escapeHTML(device.site_name || '-')
  
  // Distance
  const distanceStr = device.distance ? `${(device.distance / 1000).toFixed(2)} km` : '-'
  
  // Capacity  
  const capacity = device.capacity_60ghz || device.capacity_ltu || device.capacity_5ghz || 0
  const capacityStr = capacity ? `${(capacity / 1e6).toFixed(0)} Mbps` : '-'
  
  // Firmware - use helper to extract clean version
  const firmware = escapeHTML(getDisplayFirmware(device))
  
  // Escape all device-controlled attribute values
  const escapedIP = escapeAttr(device.ip_address || '')
  const escapedMAC = escapeHTML(device.mac || '-')
  const escapedProduct = escapeHTML(device.product || device.model || '-')
  
  return `
    <tr data-id="${device.id}" data-ip="${escapedIP}" class="${isSTA ? 'sta-row' : ''} ${device.managed ? 'managed-row' : ''} ${device.alertable === false ? 'not-alertable-row' : ''} ${store.selectedDevice === device.id ? 'selected' : ''}">
      <td class="cell-checkbox"><input type="checkbox" data-id="${device.id}" /></td>
      ${cols.status !== false ? `<td class="cell-status"><span class="status-dot ${status}" title="${escapeAttr(store.getStatusReason(device) || '')}"></span></td>` : ''}
      ${cols.name !== false ? `
        <td class="cell-name">
          ${isSTA ? '<span class="sta-indent">+-</span>' : ''}
          <span class="device-name">${deviceName}</span>${directBadge}${managedStaBadge}${alertSilencedBadge}
        </td>
      ` : ''}
      ${cols.ip !== false ? `<td class="cell-ip">${escapeHTML(device.ip_address || '-')}</td>` : ''}
      ${cols.mac ? `<td class="cell-mac">${escapedMAC}</td>` : ''}
      ${cols.product !== false ? `<td class="cell-product">${escapedProduct}</td>` : ''}
      ${cols.site !== false ? `<td class="cell-site">${siteName}</td>` : ''}
      ${cols.signal60 !== false ? `<td class="cell-signal cell-signal-60 ${signal60Class}">${signal60Display}</td>` : ''}
      ${colSta60}
      ${col5Combined}
      ${col5C0}
      ${col5C1}
      ${colSta5Combined}
      ${colSta5C0}
      ${colSta5C1}
      ${colDir}
      ${healthCol}
      ${cols.distance !== false ? `<td class="cell-distance">${distanceStr}</td>` : ''}
      ${cols.capacity !== false ? `<td class="cell-capacity">${capacityStr}</td>` : ''}
      ${cols.firmware !== false ? `<td class="cell-firmware">${firmware}</td>` : ''}
      <td class="cell-actions">
        <button class="btn btn-icon-xs btn-refresh" data-id="${device.id}" title="Refresh"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M23 4v6h-6M1 20v-6h6"/><path d="M3.51 9a9 9 0 0114.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0020.49 15"/></svg></button>
        <button class="btn btn-icon-xs btn-upgrade" data-id="${device.id}" title="Upgrade"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 01-2 2H5a2 2 0 01-2-2v-4M17 8l-5-5-5 5M12 3v12"/></svg></button>
        <button class="btn btn-icon-xs btn-danger btn-delete" data-id="${device.id}" title="Delete"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 6h18M19 6v14a2 2 0 01-2 2H7a2 2 0 01-2-2V6m3 0V4a2 2 0 012-2h4a2 2 0 012 2v2"/></svg></button>
      </td>
    </tr>
  `
}

// Signal thresholds - must match app.js SIGNAL_THRESHOLDS and Go store.go
const SIGNAL_THRESHOLDS = {
  '60ghz': { good: -55, fair: -65 },
  '5ghz': { good: -62, fair: -70 }
}

// Get signal CSS class from pre-computed quality (Go > JS)
// Falls back to JS computation if server quality not available
function getSignalClassFromQuality(quality, level, band) {
  // Prefer server-computed quality
  if (quality) {
    return `signal-${quality}`
  }
  // Fallback to JS computation
  return band === '60ghz' ? getSignalClass60(level) : getSignalClass5(level)
}

// Signal class for 60GHz
function getSignalClass60(level) {
  if (!level) return ''
  const t = SIGNAL_THRESHOLDS['60ghz']
  if (level >= t.good + 5) return 'signal-excellent'
  if (level > t.good) return 'signal-good'
  if (level > t.fair) return 'signal-fair'
  return 'signal-poor'
}

// Signal class for 5GHz/LTU/airMAX
function getSignalClass5(level) {
  if (!level) return ''
  const t = SIGNAL_THRESHOLDS['5ghz']
  if (level >= t.good + 5) return 'signal-excellent'
  if (level > t.good) return 'signal-good'
  if (level > t.fair) return 'signal-fair'
  return 'signal-poor'
}

// Generate signal bars HTML - optimized to avoid allocations in hot path
function getSignalBars(level, band = '5ghz') {
  if (!level) return '<div class="signal-bars"></div>'
  
  const t = SIGNAL_THRESHOLDS[band] || SIGNAL_THRESHOLDS['5ghz']
  // 5 bars: excellent (>good+5), very good (>good), good (>good-5), fair (>fair), poor
  let bars = 1
  if (level >= t.good + 5) bars = 5
  else if (level >= t.good) bars = 4
  else if (level >= t.good - 5) bars = 3
  else if (level >= t.fair) bars = 2
  
  const cls = bars >= 4 ? 'excellent' : bars >= 3 ? 'good' : bars >= 2 ? 'fair' : 'poor'
  
  // Build string without array allocation
  let barsHtml = ''
  for (let i = 1; i <= 5; i++) {
    barsHtml += '<div class="signal-bar' + (i <= bars ? ' active' : '') + '"></div>'
  }
  
  return '<div class="signal-bars ' + cls + '">' + barsHtml + '</div>'
}

// Directional RF thresholds (must match app.js DIR_THRESHOLDS)
// Values are intentionally conservative so we can differentiate UL vs DL issues without
// over-alerting.
// - CINR/SNR are dB and higher is better
// - Link score is a 0-100 quality score where higher is better
// - EVM is treated as "higher is better" (AirMAX exposes it that way)
const DIR_THRESHOLDS = {
  cinr: { good: 20, fair: 15, poor: 12 },
  snr:  { good: 25, fair: 18, poor: 12 },
  link_score: { good: 90, fair: 80, poor: 60 },
  evm:  { good: 17, fair: 13, poor: 9 }
}

function getWorseQuality(a, b) {
  const rank = { poor: 3, fair: 2, good: 1, unknown: 0 }
  return (rank[a] >= rank[b]) ? a : b
}

// Numeric rank for quality strings. Higher is worse.
function qualityRank(q) {
  const rank = { poor: 3, fair: 2, good: 1, unknown: 0 }
  const v = rank[q]
  return (v === undefined) ? 0 : v
}

function metricQuality(val, thresholds) {
  if (val === null || val === undefined || Number.isNaN(val)) return 'unknown'
  if (val < thresholds.poor) return 'poor'
  if (val < thresholds.fair) return 'fair'
  return 'good'
}

function getDirectionalRadio(device) {
  const r60 = device?.radio_60ghz || null
  const r6  = device?.radio_6ghz  || null
  const r5  = device?.radio_5ghz  || null
  const rltu = device?.radio_ltu  || null

  const radios = [r60, r6, r5, rltu].filter(Boolean)
  if (radios.length === 0) return null

  const connected = radios.find(r => r?.connected)
  return connected || radios[0]
}

function getStaDirectionalInfo(device) {
  const radio = getDirectionalRadio(device)
  if (!radio) return null

  const dlCinr = radio?.cinr?.dl
  const ulCinr = radio?.cinr?.ul

  // SNR fields may be absent depending on platform; when possible, compute an SNR estimate from
  // (signal - noise floor). Note: for Wave, noise floor is long-term average; still useful.
  let dlSNR = radio?.remote_snr
  let ulSNR = radio?.snr
  if ((dlSNR === null || dlSNR === undefined) && radio?.remote_signal !== null && radio?.remote_signal !== undefined && radio?.remote_noise_floor !== null && radio?.remote_noise_floor !== undefined) {
    const v = radio.remote_signal - radio.remote_noise_floor
    if (Number.isFinite(v)) dlSNR = v
  }
  if ((ulSNR === null || ulSNR === undefined) && radio?.signal !== null && radio?.signal !== undefined && radio?.noise_floor !== null && radio?.noise_floor !== undefined) {
    const v = radio.signal - radio.noise_floor
    if (Number.isFinite(v)) ulSNR = v
  }

  // Some platforms expose a per-radio link_score, while others expose it at the device level.
  const dlLinkScore = radio?.link_score?.dl ?? device?.link_score?.dl
  const ulLinkScore = radio?.link_score?.ul ?? device?.link_score?.ul

  const dlEVM = radio?.evm?.dl
  const ulEVM = radio?.evm?.ul

  // Prefer CINR if present; then SNR; then platform link score.
  const dlPrimary = (dlCinr !== null && dlCinr !== undefined) ? { type: 'cinr', val: dlCinr } :
                    (dlSNR  !== null && dlSNR  !== undefined) ? { type: 'snr',  val: dlSNR  } :
                    (dlLinkScore !== null && dlLinkScore !== undefined) ? { type: 'link_score', val: dlLinkScore } : null
  const ulPrimary = (ulCinr !== null && ulCinr !== undefined) ? { type: 'cinr', val: ulCinr } :
                    (ulSNR  !== null && ulSNR  !== undefined) ? { type: 'snr',  val: ulSNR  } :
                    (ulLinkScore !== null && ulLinkScore !== undefined) ? { type: 'link_score', val: ulLinkScore } : null

  let dlQuality = dlPrimary ? metricQuality(dlPrimary.val, DIR_THRESHOLDS[dlPrimary.type]) : 'unknown'
  let ulQuality = ulPrimary ? metricQuality(ulPrimary.val, DIR_THRESHOLDS[ulPrimary.type]) : 'unknown'

  // If EVM exists, use it as a secondary constraint (it can reveal TX chain issues even when
  // CINR/SNR look ok). We conservatively pick the worse bucket.
  if (dlEVM !== null && dlEVM !== undefined) {
    dlQuality = getWorseQuality(dlQuality, metricQuality(dlEVM, DIR_THRESHOLDS.evm))
  }
  if (ulEVM !== null && ulEVM !== undefined) {
    ulQuality = getWorseQuality(ulQuality, metricQuality(ulEVM, DIR_THRESHOLDS.evm))
  }

  return {
    dl: { quality: dlQuality, cinr: dlCinr, snr: dlSNR, link_score: dlLinkScore, evm: dlEVM },
    ul: { quality: ulQuality, cinr: ulCinr, snr: ulSNR, link_score: ulLinkScore, evm: ulEVM },
  }
}

function summarizeAPDirectional(device) {
  if (!device || device.parent_id) return null
  const stas = store.getSTAs(device.id) || []
  const infos = []
  for (const sta of stas) {
    const info = getStaDirectionalInfo(sta)
    if (info) infos.push({ sta, info })
  }
  if (infos.length === 0) return null

  const counts = {
    total: infos.length,
    dl: { good: 0, fair: 0, poor: 0, unknown: 0 },
    ul: { good: 0, fair: 0, poor: 0, unknown: 0 },
  }
  for (const { info } of infos) {
    counts.dl[info.dl.quality] = (counts.dl[info.dl.quality] || 0) + 1
    counts.ul[info.ul.quality] = (counts.ul[info.ul.quality] || 0) + 1
  }

  // Heuristic: flag AP-wide issues if a material fraction of STAs are degraded
  const frac = (n) => n / Math.max(1, counts.total)
  const apDL = (frac(counts.dl.poor) >= 0.3 || counts.dl.poor >= 2) ? 'poor' :
               (frac(counts.dl.fair) >= 0.3 || counts.dl.fair >= 2) ? 'fair' : 'good'
  const apUL = (frac(counts.ul.poor) >= 0.3 || counts.ul.poor >= 2) ? 'poor' :
               (frac(counts.ul.fair) >= 0.3 || counts.ul.fair >= 2) ? 'fair' : 'good'

  return { apDL, apUL, counts, infos }
}

function renderDirBadges(dlQuality, ulQuality, title, badgeTextDL = 'DL', badgeTextUL = 'UL') {
  const dl = dlQuality || 'unknown'
  const ul = ulQuality || 'unknown'
  return `<span class="dir-badges" title="${escapeAttr(title || '')}">
    <span class="health-badge ${dl}">${escapeHTML(badgeTextDL)}</span>
    <span class="health-badge ${ul}">${escapeHTML(badgeTextUL)}</span>
  </span>`
}

function renderDirBadgeSolo(quality, badgeText, title) {
  const q = quality || 'unknown'
  return `<span class="dir-badges" title="${escapeAttr(title || '')}">
    <span class="health-badge ${q}">${escapeHTML(badgeText || '')}</span>
  </span>`
}

export function renderDirectionalCell(device) {
  if (!device) return '-'

  // AP summary
  if (!device.parent_id) {
    const sum = summarizeAPDirectional(device)
    if (!sum) return '-'
    const c = sum.counts
    const title = `Directional summary across ${c.total} STA(s)\n` +
      `DL (AP→STA): ${c.dl.poor} poor, ${c.dl.fair} fair, ${c.dl.good} good\n` +
      `UL (STA→AP): ${c.ul.poor} poor, ${c.ul.fair} fair, ${c.ul.good} good`
    return renderDirBadges(sum.apDL, sum.apUL, title)
  }

  // STA link
  const info = getStaDirectionalInfo(device)
  if (!info) return '-'

  const fmt = (v) => (v === null || v === undefined || Number.isNaN(v)) ? '-' : `${v}`
  const title = `Downlink (AP→STA): CINR ${fmt(info.dl.cinr)} dB, SNR ${fmt(info.dl.snr)} dB, EVM ${fmt(info.dl.evm)} dB\n` +
                `Uplink (STA→AP): CINR ${fmt(info.ul.cinr)} dB, SNR ${fmt(info.ul.snr)} dB, EVM ${fmt(info.ul.evm)} dB`
  return renderDirBadges(info.dl.quality, info.ul.quality, title)
}

function renderDirectionalDetailSection(device) {
  if (!device || !device.directional) return ''

  // STA: show per-link UL/DL diagnosis
  if (device.role === 'sta') {
    const info = getStaDirectionalInfo(device)
    if (!info) return ''

    const hasVal = (v) => v !== null && v !== undefined && !Number.isNaN(v)
    const hasAnyMetric =
      hasVal(info.dl.cinr) || hasVal(info.dl.snr) || hasVal(info.dl.evm) ||
      hasVal(info.ul.cinr) || hasVal(info.ul.snr) || hasVal(info.ul.evm)

    const fmt = (v) => (v === null || v === undefined || Number.isNaN(v)) ? '-' : `${v}`

    if (!hasAnyMetric) {
      return `
        <div class="detail-section">
          <h4>Directional Diagnosis</h4>
          <div class="detail-note">
            Directional metrics unavailable (no CINR/SNR/EVM reported by this device yet).
          </div>
        </div>
      `
    }

    const note = ''

    return `
      <div class="detail-section">
        <h4>Directional Diagnosis</h4>
        <div class="detail-grid">
          <div class="detail-label">Downlink (AP→STA)</div>
          <div class="detail-value">${renderDirBadgeSolo(info.dl.quality, 'DL', 'Downlink quality')} <span class="text-small">CINR ${fmt(info.dl.cinr)} dB • SNR ${fmt(info.dl.snr)} dB • EVM ${fmt(info.dl.evm)} dB</span></div>

          <div class="detail-label">Uplink (STA→AP)</div>
          <div class="detail-value">${renderDirBadgeSolo(info.ul.quality, 'UL', 'Uplink quality')} <span class="text-small">CINR ${fmt(info.ul.cinr)} dB • SNR ${fmt(info.ul.snr)} dB • EVM ${fmt(info.ul.evm)} dB</span></div>
        </div>
        ${note}
      </div>
    `
  }

  // AP: aggregate per-station directional quality
  if (device.role === 'ap') {
    const sum = summarizeAPDirectional(device)
    if (!sum) return ''

    const c = sum.counts
    const dlNoData = c.dl.unknown || 0
    const ulNoData = c.ul.unknown || 0

    const dlIssues = (c.dl.poor > 0 || c.dl.fair > 0)
    const ulIssues = (c.ul.poor > 0 || c.ul.fair > 0)
    const anyIssues = dlIssues || ulIssues

    const anyData = (c.dl.good + c.dl.fair + c.dl.poor + c.ul.good + c.ul.fair + c.ul.poor) > 0

    const fmtCounts = (obj, noData) => {
      const base = `${obj.poor} poor • ${obj.fair} fair • ${obj.good} good`
      return noData ? `${base} • ${noData} unknown` : base
    }

    const worstMetric = (dir) => {
      if (dir.cinr !== null && dir.cinr !== undefined) return dir.cinr
      if (dir.snr !== null && dir.snr !== undefined) return dir.snr
      if (dir.link_score !== null && dir.link_score !== undefined) return dir.link_score
      if (dir.evm !== null && dir.evm !== undefined) return dir.evm
      return 999
    }

    const withInfo = sum.stations.filter(x => x.info)

    const worstDL = dlIssues ? withInfo
      .filter(x => ['poor', 'fair'].includes(x.info.dl.quality))
      .sort((a, b) => {
        const r = qualityRank(b.info.dl.quality) - qualityRank(a.info.dl.quality) // worse first
        if (r !== 0) return r
        return worstMetric(a.info.dl) - worstMetric(b.info.dl) // lower is worse
      })
      .slice(0, 10) : []

    const worstUL = ulIssues ? withInfo
      .filter(x => ['poor', 'fair'].includes(x.info.ul.quality))
      .sort((a, b) => {
        const r = qualityRank(b.info.ul.quality) - qualityRank(a.info.ul.quality) // worse first
        if (r !== 0) return r
        return worstMetric(a.info.ul) - worstMetric(b.info.ul) // lower is worse
      })
      .slice(0, 10) : []

    const renderStaList = (items, direction) => {
      if (!items.length) return ''
      const title = direction === 'dl' ? 'Worst Downlink stations' : 'Worst Uplink stations'
      const rows = items.map(x => {
        const sta = x.sta
        const info = getStaDirectionalInfo(sta)
        const badge = info ? (direction === 'dl'
          ? renderDirBadgeSolo(info.dl.quality, 'DL', 'Downlink quality')
          : renderDirBadgeSolo(info.ul.quality, 'UL', 'Uplink quality')) : ''
        const label = sta.name || sta.mac || sta.ip || 'STA'
        return `<li><a href="#?device=${encodeURIComponent(sta.id)}" title="${buildDirTooltip(sta)}">${escapeHtml(label)}</a> ${badge}</li>`
      }).join('')
      return `<div class="detail-subsection"><div class="detail-subtitle">${title}</div><ul class="detail-list">${rows}</ul></div>`
    }

    let note = ''
    if (!anyData && (dlNoData > 0 || ulNoData > 0)) {
      note = `
        <div class="detail-note">
          Directional metrics unavailable for connected stations (no CINR/SNR/EVM/link score reported yet).
        </div>`
    }

    return `
      <div class="detail-section">
        <h4>Directional Diagnosis</h4>
        <div class="detail-grid">
          <div class="detail-label">Stations analyzed</div>
          <div class="detail-value">${sum.total}</div>

          <div class="detail-label">Downlink (AP→STA)</div>
          <div class="detail-value">${renderDirBadgeSolo('unknown', 'DL', 'Downlink summary')} <span class="text-small">${fmtCounts(c.dl, dlNoData)}</span></div>

          <div class="detail-label">Uplink (STA→AP)</div>
          <div class="detail-value">${renderDirBadgeSolo('unknown', 'UL', 'Uplink summary')} <span class="text-small">${fmtCounts(c.ul, ulNoData)}</span></div>
        </div>
        ${renderStaList(worstDL, 'dl')}
        ${renderStaList(worstUL, 'ul')}
        ${note}
      </div>
    `
  }

  return ''
}

// Render device detail panel
export function renderDeviceDetail(panel, device) {
  if (!panel || !device) return
  
  const role = String(device.role || (!device.parent_id ? 'ap' : 'sta')).toLowerCase()
  const isAP = role === 'ap'
  const roleLabel = isAP ? 'AP' : 'STA'
  const status = store.getStatus(device)
  const statusText = status.charAt(0).toUpperCase() + status.slice(1)
  const statusReason = store.getStatusReason(device)
  const hasMacMismatch = statusReason === 'mac_mismatch'
  const identityMismatch = device.identity_mismatch || null
  const observedMacs = identityMismatch && Array.isArray(identityMismatch.observed_macs)
    ? identityMismatch.observed_macs
    : []
  const observedMacText = observedMacs.length ? observedMacs.join(', ') : 'Refresh to capture observed MAC'
  const mismatchCopy = isAP
    ? 'This usually means the AP was replaced or the IP now belongs to a different device. Learning the replacement MAC will also update child station parent references.'
    : 'This usually means the managed station radio was replaced or the IP now belongs to a different device. Learning the replacement MAC keeps the AP association unchanged.'
  
  // Get live stats if available
  const ls = device.live_stats || {}
  const net = device.network || ls.network || {}

  
  // GPS string
  const gpsStr = (device.gps_lat && device.gps_lon) 
    ? `${device.gps_lat.toFixed(6)}, ${device.gps_lon.toFixed(6)}` 
    : '-'
  const alertable = device.alertable !== false
  const alertSilencedUntil = device.alert_silenced_until ? new Date(device.alert_silenced_until) : null
  const alertSilencedActive = alertSilencedUntil && !Number.isNaN(alertSilencedUntil.getTime()) && alertSilencedUntil.getTime() > Date.now()
  const alertSilenceText = alertSilencedActive ? alertSilencedUntil.toLocaleString() : '-'
  
  panel.innerHTML = `
    <div class="detail-header">
      <div class="detail-title">
        <span class="status-dot ${status}" title="${escapeAttr(store.getStatusReason(device) || '')}"></span>
        <h3>${escapeHTML(device.hostname || (device.product ? `${device.product} (${device.ip_address})` : device.ip_address) || 'Device Details')}</h3>
        <span class="detail-role ${isAP ? 'ap' : 'sta'}">${roleLabel}</span>
      </div>
      <button class="panel-close">&times;</button>
    </div>
    <div class="detail-body">
      <div class="detail-quick-actions">
		<button class="btn btn-sm" data-wc-action="open-device-ui" data-device-id="${escapeAttr(device.id)}">Open UI</button>
        <button class="btn btn-sm" data-wc-action="refresh-device" data-device-id="${device.id}">Refresh</button>
        <button class="btn btn-sm btn-danger" data-wc-action="reboot-device" data-device-id="${device.id}">Reboot</button>
        ${hasMacMismatch ? `<button class="btn btn-sm btn-warning" data-wc-action="learn-replacement-mac" data-device-id="${device.id}">Learn replacement MAC</button>` : ''}
        <button class="btn btn-sm btn-primary" data-wc-action="show-upgrade-modal" data-device-id="${device.id}">Upgrade</button>
      </div>
      
      ${hasMacMismatch ? `
        <div class="detail-mismatch-panel">
          <div class="detail-mismatch-title">MAC mismatch detected</div>
          <div class="detail-mismatch-copy">The ${roleLabel} row expected <code>${escapeHTML(identityMismatch?.expected_mac || device.mac || '-')}</code> at <code>${escapeHTML(device.ip_address || '-')}</code>, but the device there reported <code>${escapeHTML(observedMacText)}</code>. ${escapeHTML(mismatchCopy)}</div>
          <div class="detail-mismatch-actions">
            <button class="btn btn-sm btn-warning" data-wc-action="learn-replacement-mac" data-device-id="${device.id}">Learn replacement MAC</button>
            <button class="btn btn-sm" data-wc-action="refresh-device" data-device-id="${device.id}">Refresh</button>
          </div>
        </div>
      ` : ''}

      <div class="detail-section">
        <h4>Identity</h4>
        <div class="detail-grid">
          <div class="detail-label">IP Address</div>
          <div class="detail-value">${escapeHTML(device.ip_address || '-')}</div>
          
          <div class="detail-label">MAC</div>
          <div class="detail-value">${escapeHTML(device.mac || '-')}</div>
          
          <div class="detail-label">Product</div>
          <div class="detail-value">${escapeHTML(device.product || device.model || '-')}</div>
          
          <div class="detail-label">Platform</div>
          <div class="detail-value">${escapeHTML(device.platform || '-')}</div>
          
          <div class="detail-label">Flavor</div>
          <div class="detail-value">${escapeHTML(device.flavor || '-')}</div>
          
          <div class="detail-label">Firmware</div>
          <div class="detail-value" title="${escapeAttr(device.firmware || '')}">${escapeHTML(getDisplayFirmware(device))}</div>

					${(device.managed && String(device.role || '').toLowerCase() !== 'ap') ? `
            <div class="detail-label">Direct</div>
            <div class="detail-value"><span class="direct-badge">DIRECT</span> (Add IP/Bulk)</div>
          ` : ''}
        </div>
      </div>
      
      <div class="detail-section">
        <h4>Status</h4>
        <div class="detail-grid">
          <div class="detail-label">Status</div>
          <div class="detail-value"><span class="status-dot ${status}" title="${escapeAttr(store.getStatusReason(device) || '')}"></span> ${statusText}</div>
          ${statusReason ? `
            <div class="detail-label">Reason</div>
            <div class="detail-value">${escapeHTML(statusReason)}</div>
          ` : ''}
          
          <div class="detail-label">Uptime</div>
          <div class="detail-value">${formatUptime(device.uptime || ls.uptime)}</div>
          
          ${device.last_seen ? `
            <div class="detail-label">Last Seen</div>
            <div class="detail-value">${new Date(device.last_seen).toLocaleString()}</div>
          ` : ''}
          
          ${device.power_time ? `
            <div class="detail-label">Power Time</div>
            <div class="detail-value">${formatUptime(device.power_time)}</div>
          ` : ''}
          
          ${device.service_uptime ? `
            <div class="detail-label">Service Uptime</div>
            <div class="detail-value">${formatUptime(device.service_uptime)}</div>
          ` : ''}
          
          ${isAP ? `
            <div class="detail-label">Connected Peers</div>
            <div class="detail-value">${device.peer_count || 0}</div>
          ` : ''}
          
          ${device.ssid ? `
            <div class="detail-label">SSID</div>
            <div class="detail-value">${escapeHTML(device.ssid)}</div>
          ` : ''}
          
          ${device.net_mode ? `
            <div class="detail-label">Network Mode</div>
            <div class="detail-value">${escapeHTML(device.net_mode)}</div>
          ` : ''}
          
          ${device.carrier_drop !== undefined ? `
            <div class="detail-label">Carrier Drop</div>
            <div class="detail-value ${device.carrier_drop ? 'text-warning' : ''}">${device.carrier_drop ? 'Yes' : 'No'}</div>
          ` : ''}
        </div>
      </div>

      <div class="detail-section">
        <h4>Alerting</h4>
        <div class="detail-grid">
          <div class="detail-label">Alertable</div>
          <div class="detail-value">
            <label class="form-check inline-check">
              <input type="checkbox" ${alertable ? 'checked' : ''} data-wc-change="toggle-alertable" data-device-id="${device.id}" />
              <span>${alertable ? 'Included in alert rules' : 'Excluded from alert rules'}</span>
            </label>
          </div>
          <div class="detail-label">Silenced until</div>
          <div class="detail-value">${escapeHTML(alertSilenceText)}</div>
          <div class="detail-label">Actions</div>
          <div class="detail-value detail-inline-actions">
            <button class="btn btn-sm btn-secondary" data-wc-action="silence-device" data-device-id="${device.id}" data-seconds="3600">Silence 1h</button>
            <button class="btn btn-sm btn-secondary" data-wc-action="silence-device" data-device-id="${device.id}" data-seconds="86400">Silence 24h</button>
            <button class="btn btn-sm btn-secondary" data-wc-action="clear-device-silence" data-device-id="${device.id}">Clear silence</button>
          </div>
          ${device.alert_notes ? `
            <div class="detail-label">Notes</div>
            <div class="detail-value">${escapeHTML(device.alert_notes)}</div>
          ` : ''}
          <div class="detail-label">Notes</div>
          <div class="detail-value"><button class="btn btn-sm btn-secondary" data-wc-action="edit-alert-notes" data-device-id="${device.id}">Edit notes</button></div>
        </div>
      </div>
      
      <div class="detail-section">
        <h4>Location</h4>
        <div class="detail-grid">
          <div class="detail-label">Site</div>
          <div class="detail-value">${escapeHTML(device.site_name || '-')}</div>
          
          <div class="detail-label">Region</div>
          <div class="detail-value">${escapeHTML(device.region_name || '-')}</div>
          
          <div class="detail-label">GPS</div>
          <div class="detail-value">${gpsStr}</div>
          
          ${device.distance ? `
            <div class="detail-label">Distance</div>
            <div class="detail-value">${(device.distance / 1000).toFixed(2)} km</div>
          ` : ''}
        </div>
      </div>
      
	      ${(device.parent_id || device.parent_mac) ? `
      <div class="detail-section">
        <h4>Hierarchy</h4>
        <div class="detail-grid">
          <div class="detail-label">Parent AP</div>
          <div class="detail-value">${(() => {
            if (device.parent_id) {
              const parent = store.devices.find(d => d.id === device.parent_id)
              if (parent) {
                const label = escapeHTML(parent.hostname || parent.ip_address || 'Unknown')
                const parentRole = String(parent.role || '').toLowerCase()
                const warn = parentRole === 'sta' ? ` <span class="detail-warning">⚠ parent is STA (possible loop)</span>` : ''
                return `<button type="button" class="detail-link detail-link-button" data-wc-action="select-device" data-device-id="${parent.id}">${label}</button>${warn}`
              }
            }
            return device.parent_mac ? `<span class="text-muted">${escapeHTML(device.parent_mac)} (offline)</span>` : '-'
          })()}</div>
          
          ${device.parent_mac ? `
            <div class="detail-label">Parent MAC</div>
            <div class="detail-value">${escapeHTML(device.parent_mac)}</div>
          ` : ''}
          
          ${device.ssid ? `
            <div class="detail-label">SSID</div>
            <div class="detail-value">${escapeHTML(device.ssid)}</div>
          ` : ''}
        </div>
      </div>
      ` : ''}
      
      ${device.orientation ? `
      <div class="detail-section">
        <h4>Orientation</h4>
        <div class="detail-grid">
          <div class="detail-label">Tilt</div>
          <div class="detail-value">${device.orientation.tilt?.toFixed(1) || 0}deg</div>
          <div class="detail-label">Roll</div>
          <div class="detail-value">${device.orientation.roll?.toFixed(1) || 0}deg</div>
          ${device.orientation.tilt24 !== undefined ? `
            <div class="detail-label">Tilt (24h avg)</div>
            <div class="detail-value">${device.orientation.tilt24?.toFixed(1) || 0}deg</div>
            <div class="detail-label">Roll (24h avg)</div>
            <div class="detail-value">${device.orientation.roll24?.toFixed(1) || 0}deg</div>
          ` : ''}
        </div>
      </div>
      ` : ''}
      
      ${device.cpu || device.ram || device.temperature ? `
      <div class="detail-section">
        <h4>Hardware</h4>
        <div class="detail-grid">
          ${device.temperature ? `
            <div class="detail-label">Temperature</div>
            <div class="detail-value">${device.temperature}degC</div>
          ` : ''}
          ${device.cpu && device.cpu.length > 0 ? `
            <div class="detail-label">CPU</div>
            <div class="detail-value">${device.cpu.map(c => `${c.usage}%`).join(' / ')}</div>
          ` : ''}
          ${device.ram && device.ram.total ? `
            <div class="detail-label">RAM</div>
            <div class="detail-value">${device.ram.usage || Math.round((1 - device.ram.free / device.ram.total) * 100)}% (${formatBytes(device.ram.free)} free)</div>
          ` : ''}
        </div>
      </div>
      ` : ''}
      
      ${net && (net.mode || net.data_ipv4_cidr || net.default_gateway || (Array.isArray(net.dns_servers) && net.dns_servers.length) || (net.mtu !== undefined && net.mtu !== null) || (net.mgmt_vlan !== undefined && net.mgmt_vlan !== null)) ? `
      <div class="detail-section">
        <h4>Network</h4>
        <div class="detail-grid">
          <div class="detail-label">Mode</div>
          <div class="detail-value">${escapeHTML(net.mode || '-')}</div>

          <div class="detail-label">MTU</div>
          <div class="detail-value">${(net.mtu !== undefined && net.mtu !== null) ? net.mtu : '-'}</div>

          <div class="detail-label">Mgmt VLAN</div>
          <div class="detail-value">${(net.mgmt_vlan !== undefined && net.mgmt_vlan !== null) ? net.mgmt_vlan : '-'}</div>

          <div class="detail-label">IPv4</div>
          <div class="detail-value">${escapeHTML(net.data_ipv4_cidr || '-')}</div>

          <div class="detail-label">Gateway</div>
          <div class="detail-value">${escapeHTML(net.default_gateway || '-')}</div>

          <div class="detail-label">DNS</div>
          <div class="detail-value">${Array.isArray(net.dns_servers) && net.dns_servers.length ? escapeHTML(net.dns_servers.join(', ')) : '-'}</div>
        </div>
      </div>
      ` : ''}

      <div class="detail-section">
        <h4>${device.platform === 'airmax' ? 'Capacity' : 'Throughput'}</h4>
        <div class="detail-grid">
          ${device.tx_rate ? `
            <div class="detail-label">${device.platform === 'airmax' ? 'DL Capacity' : 'TX Rate'}</div>
            <div class="detail-value">${formatRate(device.tx_rate)}</div>
          ` : ''}
          ${device.rx_rate ? `
            <div class="detail-label">${device.platform === 'airmax' ? 'UL Capacity' : 'RX Rate'}</div>
            <div class="detail-value">${formatRate(device.rx_rate)}</div>
          ` : ''}
        </div>
      </div>
      
      ${device.radio_60ghz ? `
      <div class="detail-section">
        <h4>${getRadioLabel(device.radio_60ghz, '60ghz', device.platform)}</h4>
        <div class="radio-perspectives">
          <div class="perspective">
            <h5>AP RX</h5>
            ${renderRadioSignalStats(device.radio_60ghz, getRadioBand(device.radio_60ghz, '60ghz'))}
          </div>
          ${device.radio_60ghz.remote_signal ? `
          <div class="perspective">
            <h5>STA RX</h5>
            ${renderRemoteSignalStats(device.radio_60ghz, getRadioBand(device.radio_60ghz, '60ghz'))}
          </div>
          ` : ''}
        </div>
      </div>
      ` : ''}
      
      ${device.radio_ltu ? `
      <div class="detail-section">
        <h4>LTU Radio</h4>
        <div class="radio-perspectives">
          <div class="perspective">
            <h5>${device.role === 'ap' ? 'Radio Info' : 'AP RX'}</h5>
            ${renderRadioSignalStats(device.radio_ltu, 'ltu')}
          </div>
          ${device.radio_ltu.remote_signal || device.radio_ltu.remote_signal_combined || device.remote_signal ? `
          <div class="perspective">
            <h5>STA RX</h5>
            ${device.radio_ltu.remote_signal || device.radio_ltu.remote_signal_combined ? renderRemoteSignalStats(device.radio_ltu, '5ghz') : renderLegacyRemoteSignal(device)}
          </div>
          ` : ''}
        </div>
      </div>
      ` : ''}
      
      ${device.platform === 'airmax' && device.remote_signal ? `
      <div class="detail-section">
        <h4>STA RX</h4>
        <div class="detail-grid">
          <div class="detail-label">Signal</div>
          <div class="detail-value">
            <div class="signal-display">
              ${getSignalBars(device.remote_signal, getRadioBandFromFreq(device.radio_5ghz?.frequency))}
              <span>${device.remote_signal} dBm</span>
            </div>
          </div>
          ${device.remote_noise_floor ? `
            <div class="detail-label">Noise Floor</div>
            <div class="detail-value">${device.remote_noise_floor} dBm</div>
          ` : ''}
          ${device.remote_signal_per_chain?.length ? `
            <div class="detail-label">Per-Chain</div>
            <div class="detail-value">${device.remote_signal_per_chain.join(' / ')} dBm</div>
          ` : ''}
        </div>
      </div>
      ` : ''}
      
      ${renderAirViewUtilizationSection(ls?.wireless?.airview_utilization)}
      ${renderRadioListSection(ls?.wireless?.radios, device.platform)}

      ${device.radio_5ghz ? `
      <div class="detail-section">
        <h4>${getRadioLabel(device.radio_5ghz, '5ghz', device.platform)}</h4>
        ${device.platform === 'airmax' ? renderRadioStats(device.radio_5ghz) : `
        <div class="radio-perspectives">
          <div class="perspective">
            <h5>AP RX</h5>
            ${renderRadioSignalStats(device.radio_5ghz, '5ghz')}
          </div>
          ${device.radio_5ghz.remote_signal ? `
          <div class="perspective">
            <h5>STA RX</h5>
            ${renderRemoteSignalStats(device.radio_5ghz, '5ghz')}
          </div>
          ` : ''}
        </div>
        `}
      </div>
      ` : ''}

      ${device.radio_6ghz ? `
      <div class="detail-section">
        <h4>${getRadioLabel(device.radio_6ghz, '6ghz', device.platform)}</h4>
        ${device.platform === 'airmax' ? renderRadioStats(device.radio_6ghz) : `
        <div class="radio-perspectives">
          <div class="perspective">
            <h5>AP RX</h5>
            ${renderRadioSignalStats(device.radio_6ghz, '6ghz')}
          </div>
          ${device.radio_6ghz.remote_signal ? `
          <div class="perspective">
            <h5>STA RX</h5>
            ${renderRemoteSignalStats(device.radio_6ghz, '6ghz')}
          </div>
          ` : ''}
        </div>
        `}
      </div>
      ` : ''}
      
      ${device.link_score ? `
      <div class="detail-section">
        <h4>Link Score</h4>
        <div class="detail-grid">
          <div class="detail-label">Downlink</div>
          <div class="detail-value">${device.link_score.dl || 0}%</div>
          <div class="detail-label">Uplink</div>
          <div class="detail-value">${device.link_score.ul || 0}%</div>
        </div>
      </div>
      ` : ''}

      ${renderDirectionalDetailSection(device)}
      
${device.interfaces && device.interfaces.length > 0 ? `
<div class="detail-section">
  <h4>Interfaces</h4>
  <div class="interfaces-list">
    ${device.interfaces.map(iface => {
      const name = iface.name || iface.id || ''
      const idSuffix = (iface.name && iface.id && iface.id !== iface.name) ? ` (${iface.id})` : ''
      const hasRate = iface.tx_rate != null || iface.rx_rate != null
      const hasPps = iface.tx_pps != null || iface.rx_pps != null
      const hasPackets = iface.tx_packets != null || iface.rx_packets != null
      const hasErrors = iface.tx_errors != null || iface.rx_errors != null
      const hasDropped = iface.tx_dropped != null || iface.rx_dropped != null
      const hasBytes = iface.tx_bytes != null || iface.rx_bytes != null

      const fmtPlain = (v) => {
        if (v == null) return '–'
        const n = Number(v)
        if (!Number.isFinite(n)) return escapeHTML(String(v))
        return n.toLocaleString()
      }

      const fmtHighlight = (v, danger = false) => {
        if (v == null) return '–'
        const n = Number(v)
        if (!Number.isFinite(n)) return escapeHTML(String(v))
        const s = n.toLocaleString()
        if (n > 0) return `<span class="${danger ? 'text-danger' : 'text-warning'}">${s}</span>`
        return s
      }

      return `
      <div class="interface-item">
        <span class="interface-name">${escapeHTML(name + idSuffix)}</span>
        <div class="interface-stats">
          <span class="interface-type">${escapeHTML(iface.type || 'unknown')}</span>
          ${iface.description ? `<span>${escapeHTML(iface.description)}</span>` : ''}
          <span>Link: ${typeof iface.plugged === 'boolean' ? (iface.plugged ? 'up' : 'down') : escapeHTML(iface.status || 'n/a')}</span>
          ${(iface.current_speed || iface.config_speed || iface.speed) ? `<span>Speed: ${escapeHTML(iface.current_speed || iface.config_speed || String(iface.speed))}</span>` : ''}
          ${iface.mtu ? `<span>MTU: ${escapeHTML(String(iface.mtu))}</span>` : ''}
          ${hasRate ? `<span>Rate: TX ${formatRate(iface.tx_rate)} / RX ${formatRate(iface.rx_rate)} <span class="muted" title="As reported by device (bps)">ⓘ</span></span>` : ''}
          ${hasPps ? `<span>PPS: TX ${fmtPlain(iface.tx_pps)} / RX ${fmtPlain(iface.rx_pps)}</span>` : ''}
          ${hasPackets ? `<span>Packets: TX ${fmtPlain(iface.tx_packets)} / RX ${fmtPlain(iface.rx_packets)}</span>` : ''}
          ${hasErrors ? `<span>Errors: TX ${fmtHighlight(iface.tx_errors, true)} / RX ${fmtHighlight(iface.rx_errors, true)}</span>` : ''}
          ${hasDropped ? `<span>Dropped: TX ${fmtHighlight(iface.tx_dropped)} / RX ${fmtHighlight(iface.rx_dropped)}</span>` : ''}
          ${hasBytes ? `<span>Bytes: TX ${formatBytes(iface.tx_bytes)} / RX ${formatBytes(iface.rx_bytes)}</span>` : ''}
        </div>
      </div>
      `
    }).join('')}
  </div>
</div>
` : ''}

      
      <div class="detail-section">
        <h4>Config Backups</h4>
        <div class="config-backup-actions">
          <button class="btn btn-sm" data-wc-action="backup-device-config" data-device-id="${device.id}">Backup Now</button>
          <button class="btn btn-sm" data-wc-action="show-device-backups" data-device-id="${device.id}">View Backups</button>
        </div>
        <div id="deviceBackupsList-${device.id}" class="device-backups-list"></div>
      </div>
    </div>
  `
}

function renderRadioStats(radio) {
  if (!radio) return '<p>No data</p>'
  
  // Use signal_combined (MRC computed by Go) when available, fallback to signal
  const signal = radio.signal_combined || radio.signal
  const hasSignal = signal && signal !== 0
  const signalDisplay = hasSignal ? `${signal} dBm` : '-'
  
  return `
    <div class="detail-grid">
      <div class="detail-label">Signal</div>
      <div class="detail-value">
        <div class="signal-display">
          ${hasSignal ? getSignalBars(signal, '5ghz') : ''}
          <span>${signalDisplay}</span>
        </div>
      </div>
      
      ${radio.connection_time ? `
        <div class="detail-label">Connection Time</div>
        <div class="detail-value">${formatUptime(radio.connection_time)}</div>
      ` : ''}
      
      ${radio.signal_per_chain ? `
        <div class="detail-label">Per-Chain</div>
        <div class="detail-value">${radio.signal_per_chain.join(' / ')} dBm</div>
      ` : ''}
      
      ${radio.signal_histogram ? `
        <div class="detail-label">Signal Range</div>
        <div class="detail-value">${radio.signal_histogram.min_signal} to ${radio.signal_histogram.max_signal} dBm</div>
      ` : ''}
      
      ${radio.cinr ? `
        <div class="detail-label">CINR</div>
        <div class="detail-value">DL: ${radio.cinr.dl} dB / UL: ${radio.cinr.ul} dB</div>
      ` : ''}

	  ${radio.evm ? `
		<div class="detail-label">EVM</div>
		<div class="detail-value">DL: ${Number.isFinite(radio.evm.dl) ? radio.evm.dl.toFixed(1) : '-'} dB / UL: ${Number.isFinite(radio.evm.ul) ? radio.evm.ul.toFixed(1) : '-'} dB</div>
	  ` : ''}
      
      ${radio.mcs ? `
        <div class="detail-label">MCS</div>
        <div class="detail-value">TX: ${radio.mcs.tx_label || radio.mcs.tx_idx} / RX: ${radio.mcs.rx_label || radio.mcs.rx_idx}</div>
      ` : ''}
      
      ${radio.airtime_dl !== undefined ? `
        <div class="detail-label">Airtime</div>
        <div class="detail-value">DL: ${radio.airtime_dl?.toFixed(1) || 0}% / UL: ${radio.airtime_ul?.toFixed(1) || 0}%</div>
      ` : ''}
      
      ${radio.capacity ? `
        <div class="detail-label">Capacity DL</div>
        <div class="detail-value">${(radio.capacity.dl / 1e6).toFixed(0)} Mbps${radio.capacity.dl_ideal ? ` (ideal: ${(radio.capacity.dl_ideal / 1e6).toFixed(0)})` : ''}</div>
        <div class="detail-label">Capacity UL</div>
        <div class="detail-value">${(radio.capacity.ul / 1e6).toFixed(0)} Mbps${radio.capacity.ul_ideal ? ` (ideal: ${(radio.capacity.ul_ideal / 1e6).toFixed(0)})` : ''}</div>
      ` : ''}
      
      ${radio.link_score ? `
        <div class="detail-label">Link Score</div>
        <div class="detail-value">DL: ${radio.link_score.dl}% / UL: ${radio.link_score.ul}%</div>
      ` : ''}
    </div>
  `
}

// Render just signal stats for AP RX perspective
function renderRadioSignalStats(radio, radioId = '') {
  if (!radio) return '<p>No data</p>'
  
  // Use signal_combined (MRC computed by Go) when available, fallback to signal
  const signal = radio.signal_combined || radio.signal
  const hasSignal = signal && signal !== 0
  const signalDisplay = hasSignal ? `${signal} dBm` : '-'
  const dataAttr = radioId ? `data-stat="signal-${radioId}"` : ''
  const band = getRadioBand(radio, radioId)
  
  // Calculate signal delta from expected (negative = worse than expected)
  const idealSignal = radio.ideal_signal
  const hasIdeal = idealSignal && idealSignal !== 0
  const signalDelta = hasSignal && hasIdeal ? signal - idealSignal : null
  const deltaClass = signalDelta !== null ? (signalDelta < -3 ? 'text-warning' : signalDelta > 3 ? 'text-success' : '') : ''
  


  // AFC (6 GHz)
  const afc = radio.afc || null
  const afcLabel = afc?.label ? String(afc.label).trim() : ''
  const afcStatus = afc?.status ? String(afc.status).trim() : ''
  const afcType = afc?.type ? String(afc.type).trim() : ''
  const afcExpiry = afc?.expiry_ms ? formatTime(afc.expiry_ms) : ''

  let afcAllowed = ''
  if (Array.isArray(afc?.regulatory) && afc.regulatory.length) {
    const parts = []
    for (const r of afc.regulatory) {
      const w = r.channel_width_mhz
      const chCount = Array.isArray(r.channels) ? r.channels.length : 0
      const rangeCount = Array.isArray(r.freq_ranges) ? r.freq_ranges.length : 0
      if (!w) continue
      if (chCount) parts.push(`${w}MHz: ${chCount} ch`)
      else if (rangeCount) parts.push(`${w}MHz: ${rangeCount} ranges`)
      else parts.push(`${w}MHz`)
      if (parts.length >= 6) break
    }
    afcAllowed = parts.join(', ')
  }

  let afcChannelsDetails = ''
  if (afc && Array.isArray(afc.regulatory) && afc.regulatory.some(r => Array.isArray(r.channels) && r.channels.length > 0)) {
    const blocks = afc.regulatory
      .filter(r => Array.isArray(r.channels) && r.channels.length > 0)
      .map(r => {
        const w = r.channel_width_mhz || 0
        const channels = Array.isArray(r.channels) ? r.channels : []
        const limit = 30
        const preview = channels
          .slice(0, limit)
          .map(c => {
            const center = c.center_mhz
            if (center === undefined || center === null) return ''
            const eirp = c.max_eirp_dbm
            if (eirp === undefined || eirp === null) return String(center)
            return `${center}@${Math.round(Number(eirp))}`
          })
          .filter(Boolean)
        const more = channels.length > limit ? ` … +${channels.length - limit} more` : ''
        const text = preview.join(', ') + more
        return `
          <div style="margin-top: 8px;">
            <div class="detail-label">${w} MHz (${channels.length})</div>
            <div class="detail-value" style="white-space: pre-wrap; word-break: break-word;">${escapeHTML(text)}</div>
          </div>
        `
      })
      .join('')

    if (blocks) {
      afcChannelsDetails = `
        <details style="margin-top: 6px;">
          <summary class="detail-label">Show AFC channels</summary>
          ${blocks}
        </details>
      `
  }
}

// Channel utilization / interference (Wave stats)
const util = radio.utilization || null;
const utilDL = util && Number.isFinite(util.dl) ? util.dl : null;
const utilUL = util && Number.isFinite(util.ul) ? util.ul : null;
const utilInterf =
  util && Number.isFinite(util.interference) ? util.interference : null;
const interfWarn =
  utilInterf !== null && utilInterf >= 10;

return `
    <div class="detail-grid compact">
      ${hasSignal || radio.signal_per_chain?.length ? `
        <div class="detail-label">Signal</div>
        <div class="detail-value">
          <div class="signal-display">
            ${hasSignal ? getSignalBars(signal, band) : ''}
            <span ${dataAttr}>${signalDisplay}</span>
          </div>
        </div>
      ` : ''}
      ${hasIdeal ? `
        <div class="detail-label">Expected</div>
        <div class="detail-value ${deltaClass}">${idealSignal} dBm${signalDelta !== null ? ` (${signalDelta >= 0 ? '+' : ''}${signalDelta})` : ''}</div>
      ` : ''}
      ${radio.signal_per_chain?.length ? `
        <div class="detail-label">Per-Chain</div>
        <div class="detail-value" data-stat="chains-${radioId}">${radio.signal_per_chain.join(' / ')} dBm</div>
      ` : ''}
      ${radio.noise_floor ? `
        <div class="detail-label">Noise Floor</div>
        <div class="detail-value">${radio.noise_floor} dBm</div>
      ` : ''}
      ${(radio.snr !== undefined && radio.snr !== null) ? `
        <div class="detail-label">Est. SNR</div>
        <div class="detail-value">${radio.snr} dB</div>
      ` : ''}
      ${radio.frequency ? `
        <div class="detail-label">Frequency</div>
        <div class="detail-value">${radio.frequency} MHz</div>
      ` : ''}
      ${radio.channel_width ? `
        <div class="detail-label">Channel Width</div>
        <div class="detail-value">${radio.channel_width} MHz</div>
      ` : ''}
      ${utilDL !== null ? `
        <div class="detail-label">Airtime DL</div>
        <div class="detail-value">${utilDL.toFixed(1)}%</div>
      ` : ''}
      ${utilUL !== null ? `
        <div class="detail-label">Airtime UL</div>
        <div class="detail-value">${utilUL.toFixed(1)}%</div>
      ` : ''}
      ${utilInterf !== null ? `
        <div class="detail-label">Interference</div>
        <div class="detail-value">${utilInterf.toFixed(1)}%${interfWarn ? ' <span class="detail-warning">High</span>' : ''}</div>
      ` : ''}
      ${radio.tx_power_eirp ? `
        <div class="detail-label">TX Power</div>
        <div class="detail-value">${radio.tx_power_eirp} dBm EIRP${radio.tx_power ? ` (${radio.tx_power} conducted)` : ''}</div>
      ` : ''}
      ${afc ? `
        <div class="detail-label">AFC</div>
        <div class="detail-value">${escapeHTML([afcLabel, afcStatus].filter(Boolean).join(' · ') || afcType || 'Enabled')}</div>
      ` : ''}
      ${afc && afcExpiry ? `
        <div class="detail-label">AFC Expiry</div>
        <div class="detail-value">${escapeHTML(afcExpiry)}</div>
      ` : ''}
      ${afc && afcAllowed ? `
        <div class="detail-label">AFC Allowed</div>
        <div class="detail-value">${escapeHTML(afcAllowed)}</div>
      ` : ''}

      ${radio.dl_ratio ? `
        <div class="detail-label">DL/UL Ratio</div>
        <div class="detail-value">${radio.dl_ratio}%</div>
      ` : ''}
      ${radio.rx_efficiency ? `
        <div class="detail-label">RX Efficiency</div>
        <div class="detail-value">${radio.rx_efficiency.toFixed(1)}%</div>
      ` : ''}
    </div>
    ${afcChannelsDetails}
  `
}



function renderAirViewUtilizationSection(util) {
  if (!Array.isArray(util) || util.length === 0) return ''

  // Normalize points (backward compatible with older key names)
  const points = util
    .map(p => ({
      f: Number(p.frequency_mhz ?? p.frequency ?? 0),
      u: Number(p.usage_pct ?? p.usage ?? 0)
    }))
    .filter(p => Number.isFinite(p.f) && p.f > 0 && Number.isFinite(p.u))

  if (!points.length) return ''

  // Band buckets by frequency (MHz)
  const pts5 = points.filter(p => p.f < 5925)
  const pts6 = points.filter(p => p.f >= 5925 && p.f < 57000)
  const pts60 = points.filter(p => p.f >= 57000)

  const summarize = (pts) => {
    if (!pts || pts.length === 0) return null

    const values = pts.map(p => p.u).filter(v => Number.isFinite(v))
    if (values.length === 0) return null

    values.sort((a, b) => a - b)
    const n = values.length
    const min = values[0]
    const max = values[n - 1]
    const mid = Math.floor(n / 2)
    const median = (n % 2 === 1) ? values[mid] : (values[mid - 1] + values[mid]) / 2
    const avg = values.reduce((s, v) => s + v, 0) / n

    // Peak channel (max utilization), tie-breaker = lowest frequency
    const peak = pts.reduce((best, p) => {
      if (!best) return { u: p.u, f: p.f }
      if (p.u > best.u) return { u: p.u, f: p.f }
      if (p.u === best.u && p.f < best.f) return { u: p.u, f: p.f }
      return best
    }, null)

    return { n, min, median, max, avg, peak }
  }

  const fmtPct = (v) => {
    if (v === null || v === undefined || !Number.isFinite(v)) return '—'
    if (Math.abs(v - Math.round(v)) < 1e-9) return String(Math.round(v))
    return v.toFixed(1)
  }

  const rows = []
  const addRow = (label, pts) => {
    const s = summarize(pts)
    if (!s) return
    const peakF = s.peak?.f ? `${Math.round(s.peak.f)} MHz` : '—'
    const peakU = fmtPct(s.peak?.u)

    rows.push({
      label,
      value: `med ${fmtPct(s.median)}% (min ${fmtPct(s.min)}%, max ${fmtPct(s.max)}%) · peak ${peakU}% @ ${peakF} · n ${s.n}`
    })
  }

  addRow('5 GHz', pts5)
  addRow('6 GHz', pts6)
  addRow('60 GHz', pts60)

  const summaryHtml = rows.map(r => `
    <div class="detail-label">${escapeHTML(r.label)}</div>
    <div class="detail-value">${escapeHTML(r.value)}</div>
  `).join('')

  const top = [...points].sort((a, b) => b.u - a.u).slice(0, 12)
  const topRows = top.map(p => `
    <tr>
      <td>${escapeHTML(String(p.f))}</td>
      <td>${escapeHTML(String(p.u))}%</td>
    </tr>
  `).join('')

  const bandLabel = (f) => {
    if (f >= 57000) return '60 GHz'
    if (f >= 5925) return '6 GHz'
    return '5 GHz'
  }

  const allSorted = [...points].sort((a, b) => a.f - b.f)
  const allRows = allSorted.map(p => `
    <tr>
      <td>${escapeHTML(bandLabel(p.f))}</td>
      <td>${escapeHTML(String(p.f))}</td>
      <td>${escapeHTML(String(p.u))}%</td>
    </tr>
  `).join('')

  const detailsTop = topRows ? `
    <details style="margin-top: 8px;">
      <summary>Top channels</summary>
      <table class="data-table">
        <thead>
          <tr><th>Frequency (MHz)</th><th>Utilization</th></tr>
        </thead>
        <tbody>
          ${topRows}
        </tbody>
      </table>
    </details>
  ` : ''

  const detailsAll = allRows ? `
    <details style="margin-top: 8px;">
      <summary>All bins</summary>
      <table class="data-table">
        <thead>
          <tr><th>Band</th><th>Frequency (MHz)</th><th>Utilization</th></tr>
        </thead>
        <tbody>
          ${allRows}
        </tbody>
      </table>
    </details>
  ` : ''

  return `
    <div class="detail-section">
      <h4>AirView Utilization</h4>
      <div class="detail-grid compact">
        ${summaryHtml}
      </div>
      ${detailsTop}
      ${detailsAll}
      <div class="detail-help" style="margin-top: 6px; font-size: 11px; opacity: 0.8;">
        Snapshot of channel utilization per frequency bin (AirView). Higher values generally mean more RF congestion.
      </div>
    </div>
  `
}



function renderRadioListSection(radios, platform = 'wave') {
  if (!Array.isArray(radios) || radios.length === 0) return ''

  const fmt = (v, suffix = '') => {
    if (v === null || v === undefined) return '—'
    if (typeof v === 'number' && !Number.isFinite(v)) return '—'
    if (v === 0) return '—'
    return `${escapeHTML(String(v))}${suffix}`
  }

  const rows = radios.map(r => {
    const label = r.display_band_override || r.band || 'Radio'
    const name = r.name || r.id || ''
    const title = name ? `${label} (${name})` : label

    const pow = (r.output_power_eirp || r.output_power) ? `${(r.output_power_eirp || r.output_power)} dBm` : '—'
    const dfs = r.dfs_enabled ? (r.dfs_cac_remaining ? `CAC ${r.dfs_cac_remaining}s` : 'on') : (r.dfs_enabled === false ? 'off' : '—')
    const afc = r.afc ? (r.afc.label || r.afc.status || 'AFC') : '—'

    return `
      <tr>
        <td>${escapeHTML(title)}</td>
        <td>${escapeHTML(String(r.id || '—'))}</td>
        <td>${escapeHTML(String(r.link_state || '—'))}</td>
        <td>${fmt(r.frequency_tx, ' MHz')}</td>
        <td>${fmt(r.channel_width_tx, ' MHz')}</td>
        <td>${escapeHTML(pow)}</td>
        <td>${escapeHTML(dfs)}</td>
        <td>${escapeHTML(afc)}</td>
      </tr>
    `
  }).join('')

  return `
    <details style="margin-top: 8px;">
      <summary>All radios</summary>
      <table class="data-table">
        <thead>
          <tr>
            <th>Radio</th>
            <th>ID</th>
            <th>Link</th>
            <th>TX Freq</th>
            <th>TX Width</th>
            <th>Power</th>
            <th>DFS</th>
            <th>AFC</th>
          </tr>
        </thead>
        <tbody>
          ${rows}
        </tbody>
      </table>
    </details>
  `
}


// Get radio band label from frequency or default
function getRadioBandFromFreq(freq) {
  if (!freq) return '5ghz'
  if (freq >= 57000) return '60ghz'
  if (freq >= 5925) return '6ghz'
  if (freq >= 5000) return '5ghz'
  if (freq >= 2400) return '2ghz'
  return '5ghz'
}

// Get radio band for signal thresholds
function getRadioBand(radio, defaultBand = '5ghz') {
  if (!radio) return defaultBand

  // Backend override for UI bucketing (ex: Wave MLO5 stores a 2nd 5GHz radio in the radio_6ghz slot).
  const override = radio.display_band_override || ''
  if (override) {
    const o = override.toLowerCase()
    if (o.includes('60')) return '60ghz'
    if (o.includes('6')) return '6ghz'
    if (o.includes('5')) return '5ghz'
    if (o.includes('2.4')) return '2ghz'
    if (o.includes('ltu')) return 'ltu'
  }
  // Check frequency first
  if (radio.frequency) {
    return getRadioBandFromFreq(radio.frequency)
  }
  // Fall back to default
  return defaultBand
}

// Get radio section label based on frequency and platform
function getRadioLabel(radio, defaultBand = '5ghz', platform = 'wave') {
  if (!radio) {
    switch (defaultBand) {
      case '60ghz':
        return '60GHz Radio'
      case '6ghz':
        return '6GHz Radio'
      case '5ghz':
        return '5GHz Radio'
      case '2ghz':
        return '2.4GHz Radio'
      case '900mhz':
      case '900':
        return '900MHz Radio'
      case 'ltu':
        return 'LTU Radio'
      default:
        return 'Radio'
    }
  }

  // Backend override (ex: Wave MLO5 stores a 2nd 5GHz radio in the radio_6ghz slot)
  const override = radio.display_band_override || ''
  if (override) return override

  // Use frequency if available
  if (radio.frequency) {
    const freq = radio.frequency
    if (freq >= 57000) return '60GHz Radio'
    if (freq >= 5925) return '6GHz Radio'
    if (freq >= 5000) return '5GHz Radio'
    if (freq >= 2400) return '2.4GHz Radio'
    if (freq >= 900) return '900MHz Radio'
  }

  // Use name if set
  if (radio.name && radio.name !== '5GHz') {
    return radio.name
  }

  // airMAX platform - be generic since we might not know the exact band
  if (platform === 'airmax') {
    return 'Radio'
  }

  // Default based on slot
  switch (defaultBand) {
    case '60ghz':
      return '60GHz Radio'
    case '6ghz':
      return '6GHz Radio'
    case '2ghz':
      return '2.4GHz Radio'
    case '900mhz':
    case '900':
      return '900MHz Radio'
    case 'ltu':
      return 'LTU Radio'
    default:
      return '5GHz Radio'
  }
}

// Render remote signal stats for STA RX perspective
function renderRemoteSignalStats(radio, band = '5ghz') {
  if (!radio || (!radio.remote_signal && !radio.remote_signal_combined)) return '<p>No data</p>'
  
  // Use remote_signal_combined (MRC computed by Go) when available
  const signal = radio.remote_signal_combined || radio.remote_signal
  
  // Calculate signal delta from expected (negative = worse than expected)
  const idealSignal = radio.remote_ideal_signal
  const hasIdeal = idealSignal && idealSignal !== 0
  const signalDelta = hasIdeal ? signal - idealSignal : null
  const deltaClass = signalDelta !== null ? (signalDelta < -3 ? 'text-warning' : signalDelta > 3 ? 'text-success' : '') : ''
  
  return `
    <div class="detail-grid compact">
      <div class="detail-label">Signal</div>
      <div class="detail-value">
        <div class="signal-display">
          ${getSignalBars(signal, band)}
          <span>${signal} dBm</span>
        </div>
      </div>
      ${hasIdeal ? `
        <div class="detail-label">Expected</div>
        <div class="detail-value ${deltaClass}">${idealSignal} dBm${signalDelta !== null ? ` (${signalDelta >= 0 ? '+' : ''}${signalDelta})` : ''}</div>
      ` : ''}
      ${radio.remote_signal_per_chain?.length ? `
        <div class="detail-label">Per-Chain</div>
        <div class="detail-value">${radio.remote_signal_per_chain.join(' / ')} dBm</div>
      ` : ''}
      ${radio.remote_noise_floor ? `
        <div class="detail-label">Noise Floor</div>
        <div class="detail-value">${radio.remote_noise_floor} dBm</div>
      ` : ''}
      ${(radio.remote_snr !== undefined && radio.remote_snr !== null) ? `
        <div class="detail-label">Est. SNR</div>
        <div class="detail-value">${radio.remote_snr} dB</div>
      ` : ''}
    </div>
  `
}

// Render legacy remote signal from device level (backward compatibility)
function renderLegacyRemoteSignal(device) {
  if (!device.remote_signal && !device.remote_signal_combined) return '<p>No data</p>'
  
  // Use remote_signal_combined (MRC computed by Go) when available
  const signal = device.remote_signal_combined || device.remote_signal
  
  // Determine band from device
  const band = device.signal_60ghz ? '60ghz' : '5ghz'
  
  return `
    <div class="detail-grid compact">
      <div class="detail-label">Signal</div>
      <div class="detail-value">
        <div class="signal-display">
          ${getSignalBars(signal, band)}
          <span>${signal} dBm</span>
        </div>
      </div>
      ${device.remote_signal_per_chain?.length ? `
        <div class="detail-label">Per-Chain</div>
        <div class="detail-value">${device.remote_signal_per_chain.join(' / ')} dBm</div>
      ` : ''}
      ${device.remote_noise_floor ? `
        <div class="detail-label">Noise Floor</div>
        <div class="detail-value">${device.remote_noise_floor} dBm</div>
      ` : ''}

	  ${(device.remote_noise_floor !== undefined && device.remote_noise_floor !== null && signal !== undefined && signal !== null) ? `
		<div class="detail-label">Est. SNR</div>
		<div class="detail-value">${signal - device.remote_noise_floor} dB</div>
	  ` : ''}
    </div>
  `
}

function formatUptime(seconds) {
  if (!seconds) return '-'
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const mins = Math.floor((seconds % 3600) / 60)
  
  if (days > 0) return `${days}d ${hours}h`
  if (hours > 0) return `${hours}h ${mins}m`
  return `${mins}m`
}

function formatBytes(bytes) {
  if (!bytes || bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`
}

function formatRate(bps) {
  if (!bps || bps === 0) return '0'
  if (bps >= 1e9) return `${(bps / 1e9).toFixed(1)} Gbps`
  if (bps >= 1e6) return `${(bps / 1e6).toFixed(1)} Mbps`
  if (bps >= 1e3) return `${(bps / 1e3).toFixed(1)} Kbps`
  return `${bps} bps`
}

// Render logs table body
export function renderLogs(container, logs) {
  if (!logs || logs.length === 0) {
    container.innerHTML = '<tr><td colspan="4" class="empty-state">No logs found</td></tr>'
    return
  }
  
  container.innerHTML = logs.map(log => `
    <tr>
      <td class="log-time">${formatTime(log.created_at)}</td>
      <td class="log-device">${escapeHTML(log.device_mac || '-')}</td>
      <td>${escapeHTML(log.action || log.change || '')}</td>
      <td>${escapeHTML(log.user || '-')}</td>
    </tr>
  `).join('')
}

// Render users management
export function renderUsers(container, users, roles) {
  const availableRoles = Array.isArray(roles) ? roles : []
  const defaultRole = availableRoles.some(role => role.name === 'viewer')
    ? 'viewer'
    : (availableRoles[0]?.name || '')

  container.innerHTML = `
    <div class="user-add-form">
      <div class="form-group">
        <label for="componentNewUsername">Username</label>
        <input type="text" id="componentNewUsername" class="input-sm" autocomplete="off" placeholder="operator.name" />
      </div>
      <div class="form-group">
        <label for="componentNewPassword">Temporary password</label>
        <input type="password" id="componentNewPassword" class="input-sm" autocomplete="new-password" placeholder="Enter a strong password" />
      </div>
      <div class="form-group">
        <label for="componentNewRole">Initial role</label>
        <select id="componentNewRole" class="select-sm">
          ${availableRoles.map(role => `<option value="${escapeAttr(role.name)}" ${role.name === defaultRole ? 'selected' : ''}>${escapeHTML(role.name)}</option>`).join('')}
        </select>
      </div>
      <button class="btn btn-primary" id="componentAddUser">Add user</button>
    </div>
    <div class="modal-table-wrap">
      <table class="logs-table user-table">
        <thead>
          <tr>
            <th>Username</th>
            <th>Role</th>
            <th>Status</th>
            <th class="table-actions-heading">Actions</th>
          </tr>
        </thead>
        <tbody>
          ${(users || []).map(user => `
            <tr data-id="${user.id}">
              <td><strong>${escapeHTML(user.username)}</strong></td>
              <td>
                <select class="select-sm component-user-role" data-id="${user.id}" aria-label="Role for ${escapeAttr(user.username)}">
                  ${availableRoles.map(role => `<option value="${escapeAttr(role.name)}" ${(user.roles || []).includes(role.name) ? 'selected' : ''}>${escapeHTML(role.name)}</option>`).join('')}
                </select>
              </td>
              <td><span class="status-pill ${user.status === 1 ? 'online' : 'offline'}">${user.status === 1 ? 'Active' : 'Disabled'}</span></td>
              <td class="table-actions">
                <button class="btn btn-secondary btn-sm component-reset-user" data-id="${user.id}" data-username="${escapeAttr(user.username)}">Set password</button>
                <button class="btn btn-secondary btn-sm component-toggle-user" data-id="${user.id}" data-status="${user.status}">${user.status === 1 ? 'Disable' : 'Enable'}</button>
                <button class="btn btn-danger btn-sm component-delete-user" data-id="${user.id}" data-username="${escapeAttr(user.username)}">Delete</button>
              </td>
            </tr>
          `).join('') || '<tr><td colspan="4"><div class="modal-empty-state"><strong>No user accounts found.</strong><span>Create an account above to grant WaveControl access.</span></div></td></tr>'}
        </tbody>
      </table>
    </div>
  `

  const addButton = container.querySelector('#componentAddUser')
  addButton?.addEventListener('click', async () => {
    const username = container.querySelector('#componentNewUsername')?.value.trim() || ''
    const password = container.querySelector('#componentNewPassword')?.value || ''
    const role = container.querySelector('#componentNewRole')?.value || defaultRole
    if (!username || !password || !role) {
      showToast('Username, password, and role are required.', 'error')
      return
    }
    addButton.disabled = true
    const originalText = addButton.textContent
    addButton.textContent = 'Creating…'
    try {
      await api.createUser(username, password, [role])
      showToast('User created', 'success')
      location.reload()
    } catch (error) {
      showToast('User creation failed: ' + error.message, 'error')
      addButton.disabled = false
      addButton.textContent = originalText
    }
  })

  container.querySelectorAll('.component-user-role').forEach(select => {
    select.addEventListener('change', async () => {
      const id = Number.parseInt(select.dataset.id, 10)
      select.disabled = true
      try {
        await api.updateUser(id, { roles: [select.value] })
        showToast('User role updated', 'success')
      } catch (error) {
        showToast('Role update failed: ' + error.message, 'error')
        location.reload()
      } finally {
        select.disabled = false
      }
    })
  })

  container.querySelectorAll('.component-toggle-user').forEach(button => {
    button.addEventListener('click', async () => {
      const id = Number.parseInt(button.dataset.id, 10)
      const currentStatus = Number.parseInt(button.dataset.status, 10)
      const newStatus = currentStatus === 1 ? 0 : 1
      button.disabled = true
      try {
        await api.updateUser(id, { status: newStatus })
        showToast(`User ${newStatus === 1 ? 'enabled' : 'disabled'}`, 'success')
        location.reload()
      } catch (error) {
        showToast('Status update failed: ' + error.message, 'error')
        button.disabled = false
      }
    })
  })

  container.querySelectorAll('.component-reset-user').forEach(button => {
    button.addEventListener('click', async () => {
      const username = button.dataset.username || 'user'
      const password = await requestInput({
        title: `Set password for ${username}`,
        eyebrow: 'Access control',
        message: 'The user’s existing sessions will be invalidated after the password is changed.',
        label: 'New password',
        inputType: 'password',
        required: true,
        placeholder: 'Enter a strong password',
        confirmText: 'Update password',
        helpText: 'Use a unique password. WaveControl does not display the saved value.',
        validate: value => value.length >= 8 || 'Use at least 8 characters.'
      })
      if (password === null) return
      button.disabled = true
      try {
        await api.updateUser(Number.parseInt(button.dataset.id, 10), { password })
        showToast('Password updated and prior sessions revoked', 'success')
      } catch (error) {
        showToast('Password update failed: ' + error.message, 'error')
      } finally {
        button.disabled = false
      }
    })
  })

  container.querySelectorAll('.component-delete-user').forEach(button => {
    button.addEventListener('click', async () => {
      const username = button.dataset.username || 'this user'
      const confirmed = await requestConfirmation(
        `${username} will lose WaveControl access and all active sessions will be invalidated.`,
        {
          title: 'Delete user account?',
          eyebrow: 'Access control',
          confirmText: 'Delete user',
          tone: 'danger',
          calloutTitle: 'This account and its role assignments will be removed.'
        }
      )
      if (!confirmed) return
      button.disabled = true
      try {
        await api.deleteUser(Number.parseInt(button.dataset.id, 10))
        showToast('User deleted', 'success')
        location.reload()
      } catch (error) {
        showToast('User deletion failed: ' + error.message, 'error')
        button.disabled = false
      }
    })
  })
}

// Format timestamp
function formatTime(timestamp) {
  if (!timestamp) return '-'
  const d = new Date(timestamp)
  return d.toLocaleString()
}

// === Warning Panel (AP threshold warnings) ===

// This panel is intentionally client-side only. It tracks active warnings
// (computed in app.js) and keeps a local history until the user clears it.

const WARN_PANEL_HISTORY_KEY = 'wavecontrol_warning_history_v1'
const WARN_PANEL_UI_KEY = 'wavecontrol_warning_panel_ui_v1'

const warningPanelState = {
  initialized: false,
  expanded: false,
  history: [], // { key, type, severity, device_id, name, ip, message, value, band, first_seen, last_seen, active, cleared_at }
  lastActiveKeys: new Set(),
}

function safeLoadJSON(key, fallback) {
  try {
    const raw = localStorage.getItem(key)
    if (!raw) return fallback
    const parsed = JSON.parse(raw)
    return parsed ?? fallback
  } catch (_) {
    return fallback
  }
}

function safeSaveJSON(key, value) {
  try {
    localStorage.setItem(key, JSON.stringify(value))
  } catch (_) {
    // ignore
  }
}

function initWarningPanelState() {
  if (warningPanelState.initialized) return
  warningPanelState.initialized = true

  // Restore persisted UI state + history
  const ui = safeLoadJSON(WARN_PANEL_UI_KEY, {})
  warningPanelState.expanded = !!ui.expanded
  const persisted = safeLoadJSON(WARN_PANEL_HISTORY_KEY, [])
  if (Array.isArray(persisted)) {
    // Basic sanity filtering
    warningPanelState.history = persisted
      .filter((w) => w && typeof w === 'object' && w.key && w.type && w.device_id)
      .slice(-500)
  }
}

function persistWarningPanel() {
  safeSaveJSON(WARN_PANEL_UI_KEY, { expanded: warningPanelState.expanded })
  // Cap history size so it can't grow unbounded.
  const capped = warningPanelState.history.slice(-500)
  safeSaveJSON(WARN_PANEL_HISTORY_KEY, capped)
}

function getWarningPanelElement() {
  initWarningPanelState()

  let el = document.getElementById('warningPanel')
  if (el) return el

  el = document.createElement('div')
  el.id = 'warningPanel'
  el.className = 'warning-panel-container hidden'

  el.innerHTML = `
    <div class="warning-panel-header" id="warningPanelHeader">
      <div class="warning-panel-title">
        <span class="warning-panel-title-text">Warnings</span>
        <span class="warning-panel-count" id="warningPanelCount"></span>
      </div>
      <div class="warning-panel-actions">
        <button class="btn btn-xs btn-secondary" id="warningPanelClear" title="Clear old warnings">Clear</button>
        <button class="btn btn-icon-xs btn-secondary" id="warningPanelToggle" title="Expand/collapse">
          <span id="warningPanelToggleIcon">▾</span>
        </button>
      </div>
    </div>
    <div class="warning-panel-body" id="warningPanelBody"></div>
  `.trim()

  document.body.appendChild(el)

  const header = el.querySelector('#warningPanelHeader')
  const toggleBtn = el.querySelector('#warningPanelToggle')
  const clearBtn = el.querySelector('#warningPanelClear')

  const toggleExpanded = () => {
    warningPanelState.expanded = !warningPanelState.expanded
    persistWarningPanel()
    renderWarningPanel()
  }

  header.addEventListener('click', (e) => {
    // Don't toggle if clicking an action button.
    if (e.target.closest('.warning-panel-actions')) return
    toggleExpanded()
  })
  toggleBtn.addEventListener('click', (e) => {
    e.preventDefault()
    e.stopPropagation()
    toggleExpanded()
  })

  clearBtn.addEventListener('click', (e) => {
    e.preventDefault()
    e.stopPropagation()
    // Clear *inactive* warnings only.
    warningPanelState.history = warningPanelState.history.filter((w) => w.active)
    persistWarningPanel()
    renderWarningPanel()
    showToast('Cleared old warnings', 'success')
  })

  // Initial render
  renderWarningPanel()
  return el
}

function formatWarningLabel(w) {
  if (!w) return ''
  const name = w.name || `Device ${w.device_id}`
  const ip = w.ip ? ` (${w.ip})` : ''
  return `${name}${ip}`
}

function formatWarningMeta(w) {
  const parts = []
  if (w.band) parts.push(String(w.band))
  if (typeof w.value === 'number') parts.push(`${Math.round(w.value)}%`)
  return parts.join(' · ')
}

function renderWarningRow(w) {
  const row = document.createElement('div')
  row.className = `warning-row severity-${w.severity || 'warning'}`
  row.dataset.deviceId = w.device_id
  row.dataset.warningKey = w.key

  const left = document.createElement('div')
  left.className = 'warning-row-left'

  const title = document.createElement('div')
  title.className = 'warning-row-title'
  title.textContent = formatWarningLabel(w)

  const msg = document.createElement('div')
  msg.className = 'warning-row-message'
  msg.textContent = w.message || w.type

  left.appendChild(title)
  left.appendChild(msg)

  const right = document.createElement('div')
  right.className = 'warning-row-right'

  const meta = document.createElement('div')
  meta.className = 'warning-row-meta'
  meta.textContent = formatWarningMeta(w)

  const ts = document.createElement('div')
  ts.className = 'warning-row-ts'
  if (w.active) {
    ts.textContent = `Last seen: ${formatTime(w.last_seen)}`
  } else {
    ts.textContent = `Cleared: ${formatTime(w.cleared_at || w.last_seen)}`
  }

  right.appendChild(meta)
  right.appendChild(ts)

  row.appendChild(left)
  row.appendChild(right)

  row.addEventListener('click', () => {
    const deviceId = Number(row.dataset.deviceId)
    if (!deviceId) return
    const device = store.getDevice(deviceId)
    if (!device) return
    const panel = document.getElementById('detailPanel')
    if (!panel) return
    panel.innerHTML = ''
    panel.appendChild(renderDeviceDetail(device))
    panel.classList.remove('hidden')
  })

  return row
}

function renderWarningSection(container, titleText, warnings) {
  const section = document.createElement('div')
  section.className = 'warning-section'

  const title = document.createElement('div')
  title.className = 'warning-section-title'
  title.textContent = `${titleText} (${warnings.length})`
  section.appendChild(title)

  const list = document.createElement('div')
  list.className = 'warning-section-list'

  warnings.forEach((w) => list.appendChild(renderWarningRow(w)))
  section.appendChild(list)
  container.appendChild(section)
}

function renderWarningPanel() {
  const el = document.getElementById('warningPanel')
  if (!el) return

  const active = warningPanelState.history.filter((w) => w.active)
  const old = warningPanelState.history.filter((w) => !w.active)
  const hasAny = active.length > 0 || old.length > 0

  // Show panel only if anything has ever triggered.
  el.classList.toggle('hidden', !hasAny)
  if (!hasAny) return

  // Highest severity styling
  const hasCritical = active.some((w) => w.severity === 'critical')
  el.classList.toggle('has-critical', hasCritical)

  const countEl = el.querySelector('#warningPanelCount')
  if (countEl) {
    const histSuffix = old.length > 0 ? ` · ${old.length} old` : ''
    countEl.textContent = `${active.length} active${histSuffix}`
  }

  const toggleIcon = el.querySelector('#warningPanelToggleIcon')
  if (toggleIcon) toggleIcon.textContent = warningPanelState.expanded ? '▴' : '▾'

  const clearBtn = el.querySelector('#warningPanelClear')
  if (clearBtn) clearBtn.disabled = old.length === 0

  const body = el.querySelector('#warningPanelBody')
  if (!body) return
  body.innerHTML = ''
  body.classList.toggle('hidden', !warningPanelState.expanded)
  el.classList.toggle('expanded', warningPanelState.expanded)

  if (!warningPanelState.expanded) return

  // Sort: active by severity then last_seen desc
  const severityRank = (s) => (s === 'critical' ? 0 : 1)
  active.sort((a, b) => {
    const sr = severityRank(a.severity) - severityRank(b.severity)
    if (sr !== 0) return sr
    return (b.last_seen || 0) - (a.last_seen || 0)
  })
  old.sort((a, b) => (b.cleared_at || b.last_seen || 0) - (a.cleared_at || a.last_seen || 0))

  if (active.length > 0) renderWarningSection(body, 'Active', active)
  if (old.length > 0) renderWarningSection(body, 'History', old)
}

// Public entrypoint: Update the warning panel with the current active warnings.
//
// activeWarnings items are expected to have:
// { type, severity, device_id, name, ip, message, value, band }
export function updateWarningsPanel(activeWarnings) {
  getWarningPanelElement()

  const now = Date.now()
  const byKey = new Map()
  for (const w of warningPanelState.history) {
    if (w && w.key) byKey.set(w.key, w)
  }

  const activeKeys = new Set()
  for (const w of activeWarnings || []) {
    if (!w || !w.type || !w.device_id) continue
    const key = `${w.type}:${w.device_id}`
    activeKeys.add(key)
    let rec = byKey.get(key)
    if (!rec) {
      rec = {
        key,
        type: w.type,
        severity: w.severity || 'warning',
        device_id: w.device_id,
        name: w.name,
        ip: w.ip,
        message: w.message,
        value: w.value,
        band: w.band,
        first_seen: now,
        last_seen: now,
        active: true,
        cleared_at: null,
      }
      warningPanelState.history.push(rec)
      byKey.set(key, rec)
    } else {
      rec.type = w.type
      rec.severity = w.severity || rec.severity
      rec.device_id = w.device_id
      rec.name = w.name
      rec.ip = w.ip
      rec.message = w.message
      rec.value = w.value
      rec.band = w.band
      rec.last_seen = now
      rec.active = true
      rec.cleared_at = null
    }
  }

  // Mark previously-active warnings as cleared if they're no longer active.
  for (const rec of warningPanelState.history) {
    if (rec && rec.active && !activeKeys.has(rec.key)) {
      rec.active = false
      rec.cleared_at = now
    }
  }

  // Cap history
  if (warningPanelState.history.length > 500) {
    warningPanelState.history = warningPanelState.history.slice(-500)
  }

  persistWarningPanel()
  renderWarningPanel()
}

// === Job Progress Panel ===

// Job panel state
const jobPanelState = {
  visible: false,
  minimized: false,
  jobs: new Map(), // jobId -> { status, progress, events, ... }
  currentEventsJobId: null, // Track which job's events modal is open
  // Local-only dismissal: keep a set of dismissed job IDs so if the backend
  // re-sends status/progress updates (reconnect, polling, etc.) we don't
  // resurrect jobs the user already dismissed.
  dismissedJobIds: new Set(),
}

// Job IDs can be integers (DB autoincrement) or UUID strings depending on the flow.
// DOM dataset values are always strings, so we normalize to string keys for the job map.
function normalizeJobId(jobId) {
  return String(jobId)
}

function isJobDismissed(jobId) {
  return jobPanelState.dismissedJobIds.has(normalizeJobId(jobId))
}

// Fetch a job from state using either string or numeric keys, and migrate numeric keys to string.
function getJobFromState(jobId) {
  const key = normalizeJobId(jobId)
  let job = jobPanelState.jobs.get(key)
  if (job) return { key, job }

  const num = Number(jobId)
  if (!Number.isNaN(num) && jobPanelState.jobs.has(num)) {
    job = jobPanelState.jobs.get(num)
    // Migrate to the normalized string key so future lookups work consistently.
    jobPanelState.jobs.delete(num)
    jobPanelState.jobs.set(key, job)
    return { key, job }
  }
  return { key, job: null }
}

function deleteJobFromState(jobId) {
  const key = normalizeJobId(jobId)
  const num = Number(jobId)
  // Mark as dismissed so we don't resurrect it if the backend re-sends updates.
  jobPanelState.dismissedJobIds.add(key)
  const deletedString = jobPanelState.jobs.delete(key)
  const deletedNumber = !Number.isNaN(num) ? jobPanelState.jobs.delete(num) : false
  return deletedString || deletedNumber
}

// Create/get the job panel container
function getJobPanel() {
  let panel = document.getElementById('jobPanel')
  if (!panel) {
    panel = document.createElement('div')
    panel.id = 'jobPanel'
    panel.className = 'job-panel hidden'
    panel.innerHTML = `
      <div class="job-panel-header">
        <span class="job-panel-title">Jobs</span>
        <div class="job-panel-controls">
          <button class="job-panel-btn" id="jobPanelMinimize" title="Minimize">-</button>
          <button class="job-panel-btn" id="jobPanelClose" title="Close">x</button>
        </div>
      </div>
      <div class="job-panel-body">
        <div class="job-list" id="jobList"></div>
      </div>
    `
    document.body.appendChild(panel)
    
    // Event handlers
    document.getElementById('jobPanelMinimize').addEventListener('click', () => {
      jobPanelState.minimized = !jobPanelState.minimized
      panel.classList.toggle('minimized', jobPanelState.minimized)
      document.getElementById('jobPanelMinimize').textContent = jobPanelState.minimized ? '+' : '-'
    })
    
    document.getElementById('jobPanelClose').addEventListener('click', () => {
      hideJobPanel()
    })

    // Delegate job list actions so frequent updates don't cause jarring re-binds
    const jobList = panel.querySelector('#jobList')
    if (jobList && !jobList.dataset.bound) {
      jobList.dataset.bound = '1'
      jobList.addEventListener('click', async (e) => {
        const cancelBtn = e.target.closest('.btn-cancel-job')
        if (cancelBtn) {
          const jobId = cancelBtn.dataset.jobId
          try {
            await api.cancelJobRun(jobId)
            showToast('Job cancelled', 'info')
            const { job } = getJobFromState(jobId)
            if (job) {
              job.status = 'cancelled'
              renderJobList()
            }
          } catch (err) {
            // If cancel failed, fetch latest status from server
            try {
              const jobData = await api.getJobRun(jobId)
              if (jobData) {
                updateJobStatus({
                  job_id: jobId,
                  job_type: jobData.job_type,
                  status: jobData.status,
                  progress: jobData.progress,
                  total_steps: jobData.total_steps,
                  completed_steps: jobData.completed_steps,
                  error_message: jobData.error_message
                })
              }
            } catch (_) {
              // ignore
            }
            showToast('Failed to cancel: ' + (err?.message || err), 'error')
          }
          return
        }

        const dismissBtn = e.target.closest('.btn-dismiss-job')
        if (dismissBtn) {
          const jobId = dismissBtn.dataset.jobId
          deleteJobFromState(jobId)
          renderJobList()
          if (jobPanelState.jobs.size === 0) {
            hideJobPanel()
          }
          return
        }

        const toggleBtn = e.target.closest('.btn-toggle-events')
        if (toggleBtn) {
          const jobId = toggleBtn.dataset.jobId
          showJobEventsModal(normalizeJobId(jobId))
          return
        }
      })
    }
  }
  return panel
}

export function showJobPanel() {
  const panel = getJobPanel()
  panel.classList.remove('hidden')
  jobPanelState.visible = true
  renderJobList()
}

export function hideJobPanel() {
  const panel = getJobPanel()
  panel.classList.add('hidden')
  jobPanelState.visible = false
}

export function toggleJobPanel() {
  if (jobPanelState.visible) {
    hideJobPanel()
  } else {
    showJobPanel()
  }
}

// Track a job in the panel
export function trackJob(jobId, jobType, deviceIds) {
  if (isJobDismissed(jobId)) {
    return
  }
  const { key, job: existing } = getJobFromState(jobId)

  // Don't overwrite if job was already created/updated by websocket message
  if (existing) {
    // Just update device_ids if missing
    if (!existing.device_ids) {
      existing.device_ids = deviceIds
    }
    showJobPanel()
    renderJobList()
    return
  }

  jobPanelState.jobs.set(key, {
    id: key,
    job_type: jobType,
    device_ids: deviceIds,
    status: 'pending',
    progress: 0,
    total_steps: deviceIds.length || 1,
    completed_steps: 0,
    events: [],
    created_at: new Date().toISOString()
  })
  showJobPanel()
  renderJobList()
}

// Update job from WebSocket message
export function updateJobProgress(data) {
  const { key: jobId, job: existingJob } = getJobFromState(data.job_id)
  if (isJobDismissed(jobId)) {
    return
  }
  let job = existingJob
  if (!job) {
    // Create job entry if we receive progress before job_update
    job = {
      id: jobId,
      job_type: 'unknown',
      status: 'running',
      progress: data.progress || 0,
      total_steps: data.total_steps || 1,
      completed_steps: data.completed_steps || 0,
      events: [],
      created_at: new Date().toISOString()
    }
    jobPanelState.jobs.set(jobId, job)
    showJobPanel()
  }
  job.progress = data.progress
  job.completed_steps = data.completed_steps
  job.total_steps = data.total_steps
  renderJobList()
}

export function updateJobStatus(data) {
  // Normalize job_id to string to handle both numeric (scheduled) and UUID (async) IDs
  const { key: jobId, job: existingJob } = getJobFromState(data.job_id)
  if (isJobDismissed(jobId)) {
    return
  }
  let job = existingJob
  const wasRunning = job && (job.status === 'running' || job.status === 'pending')
  if (!job) {
    // New job we weren't tracking - create entry
    job = {
      id: jobId,
      job_type: data.job_type || 'job', // Default to generic 'job' instead of undefined
      status: data.status,
      progress: data.progress || 0,
      total_steps: data.total_steps || 1,
      completed_steps: data.completed_steps || 0,
      target: data.target || '',
      events: [],
      created_at: new Date().toISOString()
    }
    jobPanelState.jobs.set(jobId, job)
    showJobPanel()
  }
  
  // Only update job_type if incoming data has a valid value
  if (data.job_type && data.job_type !== 'unknown') {
    job.job_type = data.job_type
  }
  
  // Update target info if provided
  if (data.target) {
    job.target = data.target
  }
  
  job.status = data.status
  job.progress = data.progress ?? job.progress
  if (data.total_steps !== undefined) {
    job.total_steps = data.total_steps
  }
  if (data.completed_steps !== undefined) {
    job.completed_steps = data.completed_steps
  }
  if (data.error_message) {
    job.error_message = data.error_message
  }

  // Some backend flows can emit a final status without a final progress/step update.
  // Make the UI resilient so "COMPLETED" doesn't show as 0%/0... and can be dismissed.
  if (job.status === 'completed') {
    if (job.total_steps && job.total_steps > 0) {
      job.completed_steps = job.total_steps
    }
    job.progress = 100
  } else if ((job.status === 'failed' || job.status === 'cancelled') && (data.progress === undefined || data.progress === null)) {
    if (job.total_steps && job.total_steps > 0) {
      const ratio = job.completed_steps / job.total_steps
      if (Number.isFinite(ratio)) {
        job.progress = Math.max(0, Math.min(100, Math.round(ratio * 100)))
      }
    }
  }
  renderJobList()
  
  // Check for auth failures when upgrade jobs complete
  const isUpgradeJob = job.job_type === 'fanout_upgrade' || job.job_type === 'bulk_upgrade' || job.job_type === 'upgrade'
  const justCompleted = wasRunning && (data.status === 'completed' || data.status === 'failed' || data.status === 'rebooting')
  if (isUpgradeJob && justCompleted) {
    checkForAuthFailures(jobId)
  }
  
  // Refresh events modal if open for this job and it just completed
  if (justCompleted && jobPanelState.currentEventsJobId === jobId) {
    showJobEventsModal(jobId)
  }
}

// Check job results for auth_failed devices and show retry modal
async function checkForAuthFailures(jobId) {
  try {
    const jobData = await api.getJobRun(jobId)
    if (!jobData || !jobData.result) return
    
    // Parse results - can be single result or array
    let results = jobData.result
    if (!Array.isArray(results)) {
      results = [results]
    }
    
    // Find devices that failed due to authentication
    const authFailures = results.filter(r => r && r.status === 'auth_failed')
    
    if (authFailures.length > 0) {
      console.log('Auth failures detected:', authFailures)
      // Show the auth retry modal if available
      if (typeof window.showAuthRetryModal === 'function') {
        window.showAuthRetryModal(authFailures)
      }
    }
  } catch (e) {
    console.error('Error checking for auth failures:', e)
  }
}

export function addJobEvent(data) {
  const { key: jobId, job: existingJob } = getJobFromState(data.job_id)
  if (isJobDismissed(jobId)) {
    return
  }
  let job = existingJob
  if (!job) {
    // Create job entry if we receive an event before job_update
    job = {
      id: jobId,
      job_type: 'unknown',
      status: 'running',
      progress: 0,
      total_steps: 1,
      completed_steps: 0,
      events: [],
      created_at: new Date().toISOString()
    }
    jobPanelState.jobs.set(jobId, job)
    showJobPanel()
  }
  const event = {
    event_type: data.event_type,
    device_id: data.device_id,
    message: data.message,
    timestamp: new Date().toISOString()
  }
  job.events.push(event)
  
  // Keep only last 100 events per job (increased from 50)
  if (job.events.length > 100) {
    job.events = job.events.slice(-100)
  }
  renderJobList()
  
  // If events modal is open for this job, append the new event
  if (jobPanelState.currentEventsJobId === jobId) {
    appendEventToModal(event, job)
  }
}

// Append a single event to the open events modal
function appendEventToModal(event, job) {
  const listContainer = document.getElementById('jobEventsList')
  if (!listContainer) return
  
  // Get job start time for relative offset
  const startTime = job.created_at ? new Date(job.created_at).getTime() : Date.now()
  const timestamp = event.timestamp ? new Date(event.timestamp).getTime() : Date.now()
  const relativeMs = timestamp - startTime
  const relativeStr = formatDuration(relativeMs)
  
  const isFailure = event.message && (
    event.message.toLowerCase().includes('failed') ||
    event.message.toLowerCase().includes('error') ||
    event.message.toLowerCase().includes('timeout') ||
    event.event_type === 'error'
  )
  
  const eventTypeFormatted = formatEventType(event.event_type)
  const eventTypeClass = getEventTypeClass(event.event_type)
  
  // Build device display from hostname/IP if available
  let deviceDisplay = ''
  if (event.device_hostname || event.device_ip) {
    const name = event.device_hostname || event.device_ip
    const ip = event.device_ip ? ` (${event.device_ip})` : ''
    deviceDisplay = `<span class="event-device">${escapeHTML(name)}${event.device_hostname && event.device_ip ? escapeHTML(ip) : ''}</span>`
  }
  
  const eventHtml = `
    <div class="event-row ${isFailure ? 'failure' : ''}">
      <span class="event-offset">+${relativeStr}</span>
      <span class="event-type-badge ${eventTypeClass}">${escapeHTML(eventTypeFormatted)}</span>
      <span class="event-message">${escapeHTML(event.message || '')}</span>
      ${deviceDisplay}
    </div>
  `
  
  // Remove "no events" placeholder if present
  const emptyPlaceholder = listContainer.querySelector('.job-events-empty')
  if (emptyPlaceholder) {
    emptyPlaceholder.remove()
  }
  
  // Append the new event
  listContainer.insertAdjacentHTML('beforeend', eventHtml)
  
  // Scroll to bottom to show latest
  listContainer.scrollTop = listContainer.scrollHeight
}

// Render the job list
function renderJobList() {
  const container = document.getElementById('jobList')
  if (!container) return

  // Preserve scroll position for smooth updates
  const prevScrollTop = container.scrollTop
  const wasAtBottom = Math.abs((container.scrollHeight - container.clientHeight) - container.scrollTop) < 6

  if (jobPanelState.jobs.size === 0) {
    container.innerHTML = '<div class="job-empty">No active jobs</div>'
    return
  }

  // Stable ordering: newest first by created_at (do NOT reorder by status)
  const jobs = Array.from(jobPanelState.jobs.values())
    .sort((a, b) => {
      const ta = a.created_at ? Date.parse(a.created_at) : 0
      const tb = b.created_at ? Date.parse(b.created_at) : 0
      return tb - ta
    })

  // Bound the DOM size: show all active jobs + up to N finished jobs (keeps updates snappy)
  const activeStatuses = new Set(['running', 'pending', 'rebooting'])
  const activeCount = jobs.reduce((n, j) => n + (activeStatuses.has(j.status) ? 1 : 0), 0)
  const MAX_TOTAL = 40
  const maxFinished = Math.max(0, MAX_TOTAL - activeCount)

  let finishedShown = 0
  const displayJobs = []
  for (const job of jobs) {
    if (activeStatuses.has(job.status)) {
      displayJobs.push(job)
    } else if (finishedShown < maxFinished) {
      displayJobs.push(job)
      finishedShown++
    }
  }

  const html = displayJobs.map(job => {
    // Count failures from actual events
    const failureCount = (job.events || []).filter(e => e.message && (
      e.message.toLowerCase().includes('failed') ||
      e.message.toLowerCase().includes('error') ||
      e.message.toLowerCase().includes('timeout') ||
      e.event_type === 'error'
    )).length
    const hasFailures = failureCount > 0
    const showExpandedEvents = hasFailures || job.status === 'failed'
    const events = job.events || []
    const eventsToShow = showExpandedEvents ? events.slice(-10) : events.slice(-5)

    return `
    <div class="job-item ${job.status} ${hasFailures ? 'has-failures' : ''}" data-job-id="${escapeAttr(job.id)}">
      <div class="job-header">
        <span class="job-type">${escapeHTML(formatJobType(job.job_type))}</span>
        ${job.target ? `<span class="job-target">${escapeHTML(job.target)}</span>` : ''}
        <span class="job-status ${job.status}">${escapeHTML(String(job.status || '').toUpperCase())}</span>
        ${hasFailures ? `<span class="job-failure-badge">${failureCount} Failed</span>` : ''}
      </div>
      <div class="job-progress-container">
        <div class="job-progress-bar ${hasFailures ? 'has-failures' : ''}">
          <div class="job-progress-fill" style="width: ${job.progress || 0}%"></div>
        </div>
        <span class="job-progress-text">${job.progress || 0}%</span>
      </div>
      ${job.completed_steps !== undefined ? `
        <div class="job-steps">
          ${job.completed_steps || 0} / ${job.total_steps || 0} devices
        </div>
      ` : ''}
      ${job.error_message ? `
        <div class="job-error">${escapeHTML(job.error_message)}</div>
      ` : ''}
      <div class="job-events ${(events.length > 0) ? '' : 'hidden'} ${showExpandedEvents ? 'expanded' : ''}">
        ${eventsToShow.map(e => {
          const isFailure = e.message && (
            e.message.toLowerCase().includes('failed') ||
            e.message.toLowerCase().includes('error') ||
            e.message.toLowerCase().includes('timeout') ||
            e.event_type === 'error'
          )
          return `
          <div class="job-event ${e.event_type} ${isFailure ? 'failure' : ''}">
            <span class="event-icon">${getEventIcon(e.event_type)}</span>
            <span class="event-message">${escapeHTML(e.message)}</span>
          </div>
        `}).join('')}
      </div>
      <div class="job-actions">
        ${(job.status === 'running' || job.status === 'pending') ? `
          <button class="btn btn-sm btn-danger btn-cancel-job" data-job-id="${escapeAttr(job.id)}">Cancel</button>
        ` : `
          <button class="btn btn-sm btn-secondary btn-dismiss-job" data-job-id="${escapeAttr(job.id)}">Dismiss</button>
        `}
        <button class="btn btn-sm btn-secondary btn-toggle-events" data-job-id="${escapeAttr(job.id)}" title="View all events">
          ${events.length} Event${events.length !== 1 ? 's' : ''} ${hasFailures ? `(${failureCount} failed)` : ''} ▸
        </button>
      </div>
    </div>
  `
  }).join('')

  // Update only if changed to avoid unnecessary DOM churn
  if (container.innerHTML !== html) {
    container.innerHTML = html
  }

  // Restore scroll position (or keep pinned to bottom)
  if (wasAtBottom) {
    container.scrollTop = container.scrollHeight
  } else {
    const maxTop = Math.max(0, container.scrollHeight - container.clientHeight)
    container.scrollTop = Math.min(prevScrollTop, maxTop)
  }

  // Dispatch event so app.js can update the badge
  window.dispatchEvent(new CustomEvent('jobsChanged'))
}

// Show job events in a modal
async function showJobEventsModal(jobId) {
  jobId = normalizeJobId(jobId)
  const modal = document.getElementById('jobEventsModal')
  const infoContainer = document.getElementById('jobEventsInfo')
  const listContainer = document.getElementById('jobEventsList')
  
  if (!modal || !infoContainer || !listContainer) return
  
  // Track which job's events we're viewing for live updates
  jobPanelState.currentEventsJobId = jobId
  
  // Get job from local state
  const { job } = getJobFromState(jobId)
  
  // Show loading state
  infoContainer.innerHTML = `
    <div class="info-item">
      <span class="info-label">Job ID</span>
      <span class="info-value">${escapeHTML(jobId)}</span>
    </div>
    <div class="info-item">
      <span class="info-label">Type</span>
      <span class="info-value">${escapeHTML(formatJobType(job?.job_type || 'unknown'))}</span>
    </div>
    <div class="info-item">
      <span class="info-label">Status</span>
      <span class="info-value">${escapeHTML(job?.status?.toUpperCase() || 'UNKNOWN')}</span>
    </div>
  `
  listContainer.innerHTML = '<div class="job-events-loading">Loading events...</div>'
  
  modal.classList.remove('hidden')
  
  // Fetch events from API
  let events = []
  try {
    const apiEvents = await api.getJobRunEvents(jobId, 500)
    if (apiEvents && Array.isArray(apiEvents)) {
      events = apiEvents
    }
  } catch (e) {
    console.log('Could not fetch events from API, using local:', e)
  }
  
  // Fall back to local events if API returned nothing
  if (events.length === 0 && job?.events) {
    events = job.events
  }
  
  // Count failures
  const failureCount = events.filter(e => e.message && (
    e.message.toLowerCase().includes('failed') ||
    e.message.toLowerCase().includes('error') ||
    e.message.toLowerCase().includes('timeout') ||
    e.event_type === 'error'
  )).length
  
  // Calculate job duration from events
  let startTime = null
  let endTime = null
  events.forEach(e => {
    const t = new Date(e.event_time || e.timestamp || e.created_at || 0).getTime()
    if (t > 0) {
      if (!startTime || t < startTime) startTime = t
      if (!endTime || t > endTime) endTime = t
    }
  })
  
  const isRunning = job?.status === 'running' || job?.status === 'pending'
  const duration = isRunning ? (Date.now() - startTime) : (endTime - startTime)
  const durationStr = startTime ? formatDuration(duration) : '-'
  
  // Update info with event count and duration
  infoContainer.innerHTML = `
    <div class="info-item">
      <span class="info-label">Job ID</span>
      <span class="info-value" style="font-size: 10px">${escapeHTML(jobId.substring(0, 8))}</span>
    </div>
    <div class="info-item">
      <span class="info-label">Type</span>
      <span class="info-value">${escapeHTML(formatJobType(job?.job_type || 'unknown'))}</span>
    </div>
    <div class="info-item">
      <span class="info-label">Status</span>
      <span class="info-value">${escapeHTML(job?.status?.toUpperCase() || 'UNKNOWN')}</span>
    </div>
    <div class="info-item">
      <span class="info-label">Duration</span>
      <span class="info-value ${isRunning ? 'job-duration-live' : ''}" data-start="${startTime || ''}">${durationStr}</span>
    </div>
    ${failureCount > 0 ? `
    <div class="info-item">
      <span class="info-label">Failures</span>
      <span class="info-value failure-count">${failureCount}</span>
    </div>
    ` : ''}
  `
  
  if (events.length === 0) {
    listContainer.innerHTML = '<div class="job-events-empty">No events recorded</div>'
    return
  }
  
  // Render events (oldest first for chronological view)
  const sortedEvents = [...events].sort((a, b) => {
    const ta = new Date(a.event_time || a.timestamp || a.created_at || 0).getTime()
    const tb = new Date(b.event_time || b.timestamp || b.created_at || 0).getTime()
    return ta - tb
  })
  
  listContainer.innerHTML = sortedEvents.map((e, idx) => {
    const time = e.event_time || e.timestamp || e.created_at
    const timestamp = time ? new Date(time).getTime() : 0
    
    // Show relative time from job start
    const relativeMs = startTime && timestamp ? timestamp - startTime : 0
    const relativeStr = formatDuration(relativeMs)
    
    // Build device display from hostname/IP if available
    let deviceDisplay = ''
    if (e.device_hostname || e.device_ip) {
      const name = e.device_hostname || e.device_ip
      const ip = e.device_ip ? ` (${e.device_ip})` : ''
      deviceDisplay = `<span class="event-device">${escapeHTML(name)}${e.device_hostname && e.device_ip ? escapeHTML(ip) : ''}</span>`
    }
    
    const isFailure = e.message && (
      e.message.toLowerCase().includes('failed') ||
      e.message.toLowerCase().includes('error') ||
      e.message.toLowerCase().includes('timeout') ||
      e.event_type === 'error'
    )
    
    const eventTypeFormatted = formatEventType(e.event_type)
    const eventTypeClass = getEventTypeClass(e.event_type)
    
    return `
      <div class="event-row ${isFailure ? 'failure' : ''}">
        <span class="event-offset">+${relativeStr}</span>
        <span class="event-type-badge ${eventTypeClass}">${escapeHTML(eventTypeFormatted)}</span>
        <span class="event-message">${escapeHTML(e.message || '')}</span>
        ${deviceDisplay}
      </div>
    `
  }).join('')
  
  // Start live duration timer if job is running
  startJobDurationTimer(infoContainer, startTime, isRunning)
}

// Format duration as m:ss or h:mm:ss
function formatDuration(ms) {
  if (!ms || ms < 0) return '0:00'
  const totalSeconds = Math.floor(ms / 1000)
  const hours = Math.floor(totalSeconds / 3600)
  const minutes = Math.floor((totalSeconds % 3600) / 60)
  const seconds = totalSeconds % 60
  
  if (hours > 0) {
    return `${hours}:${minutes.toString().padStart(2, '0')}:${seconds.toString().padStart(2, '0')}`
  }
  return `${minutes}:${seconds.toString().padStart(2, '0')}`
}

// Format event type for display
function formatEventType(type) {
  if (!type) return ''
  const names = {
    'started': 'Started',
    'progress': 'Progress',
    'step_complete': 'Step Done',
    'warning': 'Warning',
    'error': 'Error',
    'completed': 'Completed'
  }
  return names[type] || type.replace(/_/g, ' ').replace(/\b\w/g, c => c.toUpperCase())
}

// Get CSS class for event type
function getEventTypeClass(type) {
  const classes = {
    'started': 'type-started',
    'progress': 'type-progress',
    'step_complete': 'type-success',
    'warning': 'type-warning',
    'error': 'type-error',
    'completed': 'type-success'
  }
  return classes[type] || 'type-info'
}

// Live duration timer for running jobs
let jobDurationInterval = null
function startJobDurationTimer(container, startTime, isRunning) {
  // Clear any existing timer
  if (jobDurationInterval) {
    clearInterval(jobDurationInterval)
    jobDurationInterval = null
  }
  
  if (!isRunning || !startTime) return
  
  const durationEl = container.querySelector('.job-duration-live')
  if (!durationEl) return
  
  const updateDuration = () => {
    const elapsed = Date.now() - startTime
    durationEl.textContent = formatDuration(elapsed)
  }
  
  // Update every second
  jobDurationInterval = setInterval(updateDuration, 1000)
  
  // Clean up when modal closes
  const modal = document.getElementById('jobEventsModal')
  if (modal) {
    const observer = new MutationObserver((mutations) => {
      mutations.forEach((mutation) => {
        if (mutation.type === 'attributes' && mutation.attributeName === 'class') {
          if (modal.classList.contains('hidden')) {
            clearInterval(jobDurationInterval)
            jobDurationInterval = null
            jobPanelState.currentEventsJobId = null
            observer.disconnect()
          }
        }
      })
    })
    observer.observe(modal, { attributes: true })
  }
}

function formatJobType(type) {
  const names = {
    'upgrade': 'Upgrade',
    'bulk_upgrade': 'Bulk Upgrade',
    'fanout_upgrade': 'Fanout Upgrade',
    'backup': 'Backup',
    'reboot': 'Reboot',
    'refresh': 'Refresh',
    'job': 'Task',
    'unknown': 'Task'
  }
  return names[type] || (type ? type.charAt(0).toUpperCase() + type.slice(1).replace(/_/g, ' ') : 'Task')
}

function getEventIcon(eventType) {
  const icons = {
    'started': '>',
    'progress': 'o',
    'step_complete': 'ok',
    'warning': '!',
    'error': 'x',
    'completed': 'ok'
  }
  return icons[eventType] || '-'
}

// Start a job and track it
export async function startTrackedJob(jobType, deviceIds, params = {}) {
  console.log('startTrackedJob:', jobType, deviceIds, params)
  try {
    const result = await api.startJob(jobType, deviceIds, params)
    console.log('startTrackedJob result:', result)
    if (result.job_id) {
      trackJob(result.job_id, jobType, deviceIds)
      showToast(`Job started: ${formatJobType(jobType)}`, 'info')
      // Start polling as fallback in case websocket fails
      pollJobStatus(result.job_id)
      return result.job_id
    }
  } catch (e) {
    console.error('startTrackedJob error:', e)
    showToast('Failed to start job: ' + e.message, 'error')
    throw e
  }
}

// Poll job status as fallback if websocket doesn't update
async function pollJobStatus(jobId) {
  const maxPolls = 60  // Max 5 minutes (5s intervals)
  let polls = 0
  
  const poll = async () => {
    polls++
  const { job } = getJobFromState(jobId)
    
    // Stop polling if job finished or was removed
    if (!job || job.status === 'completed' || job.status === 'failed' || job.status === 'cancelled' || job.status === 'skipped' || job.status === 'rebooting') {
      return
    }
    
    // Stop after max polls
    if (polls >= maxPolls) {
      console.log('Job polling timeout:', jobId)
      return
    }
    
    try {
      const jobData = await api.getJobRun(jobId)
      if (jobData) {
        // Only update if status changed (websocket might have handled it)
        if (job.status !== jobData.status || job.progress !== jobData.progress) {
          console.log('Poll update job:', jobId, jobData.status, jobData.progress)
          updateJobStatus({
            job_id: jobId,
            job_type: jobData.job_type,
            status: jobData.status,
            progress: jobData.progress,
            total_steps: jobData.total_steps,
            completed_steps: jobData.completed_steps,
            error_message: jobData.error_message
          })
          // updateJobBadge triggered automatically via jobsChanged event
        }
        
        // Continue polling if still running
        if (jobData.status === 'running' || jobData.status === 'pending') {
          setTimeout(poll, 5000)
        }
      }
    } catch (e) {
      console.error('Poll job status error:', e)
      // Continue polling on error
      setTimeout(poll, 5000)
    }
  }
  
  // Start first poll after 2 seconds (give websocket a chance)
  setTimeout(poll, 2000)
}

// Get count of active jobs for badge display
export function getActiveJobCount() {
  let count = 0
  jobPanelState.jobs.forEach(job => {
    if (job.status === 'running' || job.status === 'pending') {
      count++
    }
  })
  return count
}
