// =============================================================================
// VirtualTable - Renders only visible rows + buffer for large datasets
// =============================================================================
//
// ARCHITECTURE:
// ┌─────────────────────────────────────────────────────────────────┐
// │ .virtual-table-wrapper (flex container, height: 100%)          │
// │ ┌─────────────────────────────────────────────────────────────┐ │
// │ │ <table> HEADER (fixed, not scrolled)                        │ │
// │ │   <thead><tr><th>...</th></tr></thead>                      │ │
// │ └─────────────────────────────────────────────────────────────┘ │
// │ ┌─────────────────────────────────────────────────────────────┐ │
// │ │ .virtual-scroll-container (overflow-y: auto, flex: 1)       │ │
// │ │ ┌─────────────────────────────────────────────────────────┐ │ │
// │ │ │ .virtual-spacer (height = totalRows * rowHeight)        │ │ │
// │ │ │   Creates scrollbar for full data height                │ │ │
// │ │ │ ┌─────────────────────────────────────────────────────┐ │ │ │
// │ │ │ │ <table> BODY (position: absolute)                   │ │ │ │
// │ │ │ │   <tbody>                                           │ │ │ │
// │ │ │ │     <tr style="transform: translateY(Npx)">         │ │ │ │
// │ │ │ │     ... only buffered rows rendered                 │ │ │ │
// │ │ │ │   </tbody>                                          │ │ │ │
// │ │ │ └─────────────────────────────────────────────────────┘ │ │ │
// │ │ └─────────────────────────────────────────────────────────┘ │ │
// │ └─────────────────────────────────────────────────────────────┘ │
// └─────────────────────────────────────────────────────────────────┘
//
// KEY CONCEPTS:
// - rowHeight: Fixed height per row (default 40px)
// - bufferSize: Rows to render above/below viewport (default 100)
// - Viewport shows ~15-20 rows, buffer adds 200 more = ~220 DOM nodes
// - Full dataset stays in JS memory for instant search/filter/sort
//

export class VirtualTable {
  constructor(options) {
    // Required
    this.container = options.container
    this.renderRow = options.renderRow      // (item) => innerHTML string (no <tr>)
    this.renderHeader = options.renderHeader // () => innerHTML string for thead
    
    // Optional with defaults
    this.rowHeight = options.rowHeight || 40
    this.bufferSize = options.bufferSize || 100
    this.onRowClick = options.onRowClick    // (item, tr, event) => void
    this.getRowId = options.getRowId || (d => d.id)
    this.getRowIp = options.getRowIp || (d => d.ip_address)
    
    // Internal state
    this.data = []
    this.dataById = new Map()
    this.dataByIp = new Map()
    
    // Viewport tracking
    this.scrollTop = 0
    this.viewportHeight = 0
    this.bufferStart = 0
    this.bufferEnd = 0
    
    // DOM references (set in mount())
    this.wrapper = null
    this.headerTable = null
    this.scrollContainer = null
    this.spacer = null
    this.bodyTable = null
    this.tbody = null
    
    // Row cache: id -> { element, index }
    this.rowCache = new Map()
    
    // Batch update tracking
    this.pendingUpdates = new Map()
    this.updateTimer = null
    this.batchMs = 50
    
    // Scroll RAF tracking
    this._scrollRAF = null

    // ResizeObserver (tracks scroll container width changes that don't trigger window.resize,
    // e.g. opening/closing the detail panel).
    this._resizeObserver = null
    
    // Bind event handlers
    this._onScroll = this._onScroll.bind(this)
    this._onResize = this._onResize.bind(this)
    
    // Debug mode
    this.debug = false
  }
  
  // ===========================================================================
  // LIFECYCLE
  // ===========================================================================
  
  mount() {
    this._log('mount() called, container:', this.container)
    
    if (!this.container) {
      console.error('VirtualTable: No container provided')
      return this
    }
    
    // Build DOM structure
    this.container.innerHTML = `
      <div class="virtual-table-wrapper">
        <table class="device-table virtual-header-table">
          <thead></thead>
        </table>
        <div class="virtual-scroll-container">
          <div class="virtual-spacer"></div>
          <table class="device-table virtual-body-table">
            <tbody></tbody>
          </table>
        </div>
      </div>
    `
    
    // Cache DOM references
    this.wrapper = this.container.querySelector('.virtual-table-wrapper')
    this.headerTable = this.container.querySelector('.virtual-header-table')
    this.scrollContainer = this.container.querySelector('.virtual-scroll-container')
    this.spacer = this.container.querySelector('.virtual-spacer')
    this.bodyTable = this.container.querySelector('.virtual-body-table')
    this.tbody = this.container.querySelector('tbody')
    
    // Render header
    const thead = this.container.querySelector('thead')
    if (thead && this.renderHeader) {
      thead.innerHTML = this.renderHeader()
    }
    
    // Attach event listeners
    this.scrollContainer.addEventListener('scroll', this._onScroll, { passive: true })
    window.addEventListener('resize', this._onResize)

    // Keep the header width aligned with the scroll container content width.
    // Without this, the header can be wider than the body due to scrollbar width,
    // which makes columns appear to "shift".
    this._syncHeaderWidth()

    // Observe container width changes that won't trigger window.resize.
    this._installResizeObserver()
    
    // Calculate initial viewport
    // Use requestAnimationFrame to ensure container has dimensions
    requestAnimationFrame(() => {
      this._updateViewportHeight()
      this._syncHeaderWidth()
      this._log('Initial viewport height:', this.viewportHeight)
      if (this.data.length > 0) {
        this._renderViewport()
      }
    })
    
    return this
  }
  
  destroy() {
    this._log('destroy() called')
    
    if (this.scrollContainer) {
      this.scrollContainer.removeEventListener('scroll', this._onScroll)
    }
    window.removeEventListener('resize', this._onResize)

    if (this._resizeObserver) {
      try {
        this._resizeObserver.disconnect()
      } catch (e) {
        // ignore
      }
      this._resizeObserver = null
    }
    
    if (this.updateTimer) {
      clearTimeout(this.updateTimer)
    }
    
    if (this._scrollRAF) {
      cancelAnimationFrame(this._scrollRAF)
    }
    
    this.rowCache.clear()
    this.pendingUpdates.clear()
    
    if (this.container) {
      this.container.innerHTML = ''
    }
  }
  
  // ===========================================================================
  // DATA MANAGEMENT
  // ===========================================================================
  
  setData(data) {
    this._log('setData() called with', data.length, 'items')

    this.data = data || []
    this._rebuildIndexes()
    this._updateSpacerHeight()

    // Data length changes can cause the vertical scrollbar to appear/disappear.
    // That changes scrollContainer.clientWidth without changing its border-box size,
    // so ResizeObserver won't fire. Sync header width after layout settles.
    requestAnimationFrame(() => {
      this._syncHeaderWidth()
    })

    this._renderViewport(true)  // Force re-render when data changes
  }

  
  _rebuildIndexes() {
    this.dataById.clear()
    this.dataByIp.clear()
    
    for (const item of this.data) {
      const id = this.getRowId(item)
      const ip = this.getRowIp(item)
      if (id != null) this.dataById.set(id, item)
      if (ip) this.dataByIp.set(ip, item)
    }
  }
  
  // ===========================================================================
  // RENDERING
  // ===========================================================================
  
  _updateSpacerHeight() {
    if (!this.spacer) return
    
    const totalHeight = this.data.length * this.rowHeight
    this.spacer.style.height = `${totalHeight}px`
    this._log('Spacer height set to', totalHeight)
  }
  
  _updateViewportHeight() {
    if (!this.scrollContainer) return
    
    this.viewportHeight = this.scrollContainer.clientHeight
    this._log('Viewport height:', this.viewportHeight)
  }

  _syncHeaderWidth() {
    if (!this.headerTable || !this.scrollContainer) return

    // clientWidth excludes the vertical scrollbar, which is exactly the width
    // the body table is laid out against.
    const w = this.scrollContainer.clientWidth
    if (w && w > 0) {
      this.headerTable.style.width = `${w}px`
    } else {
      this.headerTable.style.width = '100%'
    }
  }

  _installResizeObserver() {
    if (!this.scrollContainer) return
    if (typeof ResizeObserver === 'undefined') return
    if (this._resizeObserver) return

    this._resizeObserver = new ResizeObserver(() => {
      // Only sync widths here; avoid heavy re-render work.
      this._syncHeaderWidth()
    })
    this._resizeObserver.observe(this.scrollContainer)
  }
  
  _calculateBufferRange() {
    const rowsInView = Math.ceil(this.viewportHeight / this.rowHeight)
    const scrollRow = Math.floor(this.scrollTop / this.rowHeight)
    
    const visibleStart = Math.max(0, scrollRow)
    const visibleEnd = Math.min(this.data.length, scrollRow + rowsInView + 1)
    
    this.bufferStart = Math.max(0, visibleStart - this.bufferSize)
    this.bufferEnd = Math.min(this.data.length, visibleEnd + this.bufferSize)
    
    this._log('Buffer range:', this.bufferStart, '-', this.bufferEnd, 
              '(visible:', visibleStart, '-', visibleEnd, ')')
  }
  
  _renderViewport(force = false) {
    if (!this.tbody) return
    
    const oldStart = this.bufferStart
    const oldEnd = this.bufferEnd
    
    this._calculateBufferRange()
    
    // Skip render if buffer range hasn't changed significantly
    // Small scroll within current buffer doesn't need DOM changes
    // BUT always re-render when force=true (data changed, e.g. sort)
    if (!force && oldStart === this.bufferStart && oldEnd === this.bufferEnd) {
      return
    }
    
    // When data order changed (sort), clear cache and re-render all
    if (force) {
      for (const [id, cached] of this.rowCache) {
        cached.element.remove()
      }
      this.rowCache.clear()
    } else {
      // Remove rows outside new buffer - iterate cache directly
      const toRemove = []
      for (const [id, cached] of this.rowCache) {
        const idx = cached.index
        if (idx < this.bufferStart || idx >= this.bufferEnd) {
          toRemove.push(id)
        }
      }
      for (const id of toRemove) {
        const cached = this.rowCache.get(id)
        cached.element.remove()
        this.rowCache.delete(id)
      }
    }
    
    // Add/update rows in buffer
    for (let i = this.bufferStart; i < this.bufferEnd; i++) {
      const item = this.data[i]
      if (!item) continue
      
      const id = this.getRowId(item)
      const yPos = i * this.rowHeight
      
      const cached = this.rowCache.get(id)
      
      if (cached) {
        // Update position if index changed
        if (cached.index !== i) {
          cached.element.style.transform = 'translateY(' + yPos + 'px)'
          cached.index = i
        }
      } else {
        // Create new row
        const tr = this._createRow(item, i)
        this.tbody.appendChild(tr)
        this.rowCache.set(id, { element: tr, index: i })
      }
    }
    
    this._log('Rendered', this.rowCache.size, 'rows')
  }
  
  _createRow(item, index) {
    const tr = document.createElement('tr')
    const id = this.getRowId(item)
    const ip = this.getRowIp(item)
    const yPos = index * this.rowHeight
    
    tr.dataset.id = id
    if (ip) tr.dataset.ip = ip
    
    // Set styles inline - faster than multiple style assignments
    tr.style.cssText = 'position:absolute;top:0;left:0;right:0;height:' + this.rowHeight + 'px;transform:translateY(' + yPos + 'px)'
    
    // Render content
    tr.innerHTML = this.renderRow(item)
    
    // Click handler
    if (this.onRowClick) {
      tr.addEventListener('click', (e) => {
        this.onRowClick(item, tr, e)
      })
    }
    
    return tr
  }
  
  // ===========================================================================
  // INCREMENTAL UPDATES (for WebSocket batching)
  // ===========================================================================
  
  updateByIp(ip, updates) {
    const item = this.dataByIp.get(ip)
    if (!item) return
    
    // Merge updates into data
    Object.assign(item, updates)
    
    // Queue DOM update
    this.pendingUpdates.set(this.getRowId(item), item)
    this._scheduleBatchUpdate()
  }
  
  updateById(id, updates) {
    const item = this.dataById.get(id)
    if (!item) return
    
    Object.assign(item, updates)
    this.pendingUpdates.set(id, item)
    this._scheduleBatchUpdate()
  }
  
  _scheduleBatchUpdate() {
    if (this.updateTimer) return
    
    this.updateTimer = setTimeout(() => {
      this._flushUpdates()
      this.updateTimer = null
    }, this.batchMs)
  }
  
  _flushUpdates() {
    for (const [id, item] of this.pendingUpdates) {
      const cached = this.rowCache.get(id)
      if (cached) {
        // Re-render row content
        cached.element.innerHTML = this.renderRow(item)
      }
    }
    this.pendingUpdates.clear()
  }
  
  // ===========================================================================
  // EVENT HANDLERS
  // ===========================================================================
  
  _onScroll() {
    // Use RAF to batch scroll handling - prevents multiple renders per frame
    if (this._scrollRAF) return
    this._scrollRAF = requestAnimationFrame(() => {
      this._scrollRAF = null
      this.scrollTop = this.scrollContainer.scrollTop
      this._renderViewport()
    })
  }
  
  _onResize() {
    this._updateViewportHeight()
    this._syncHeaderWidth()
    this._renderViewport()
  }
  
  // ===========================================================================
  // PUBLIC METHODS
  // ===========================================================================
  
  scrollToId(id) {
    const item = this.dataById.get(id)
    if (!item) return false
    
    const index = this.data.indexOf(item)
    if (index === -1) return false
    
    const targetY = index * this.rowHeight - (this.viewportHeight / 2)
    this.scrollContainer.scrollTop = Math.max(0, targetY)
    
    // Highlight after scroll
    setTimeout(() => {
      const cached = this.rowCache.get(id)
      if (cached) {
        cached.element.classList.add('highlighted')
        setTimeout(() => cached.element.classList.remove('highlighted'), 2000)
      }
    }, 100)
    
    return true
  }
  
  getScrollTop() {
    return this.scrollContainer?.scrollTop || 0
  }
  
  setScrollTop(value) {
    if (this.scrollContainer) {
      this.scrollContainer.scrollTop = value
    }
  }
  
  // ===========================================================================
  // DEBUG
  // ===========================================================================
  
  _log(...args) {
    if (this.debug) {
      console.log('[VirtualTable]', ...args)
    }
  }
}
