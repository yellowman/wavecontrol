# Work Plan: Wave MLO (MLO6 + MLO5) multi-radio + peer/station mapping

This plan breaks Wave MLO support into **small phases** so each phase can be completed and validated quickly, producing a buildable zip at each step.

## Implementation status
- ✅ Phase 1: Split the Wave poller into distinct phases (delivered as `wavecontrol_fix_v87_phase1.zip`)
- ✅ Phase 2: Parse MLO multi-radio correctly (band inference) (delivered as `wavecontrol_fix_v88_phase2.zip`)
- ✅ Phase 3: Fix peer mapping to bands (delivered as `wavecontrol_fix_v89_phase3.zip`)
- ✅ Phase 4: UI: show both radios + correct labels (delivered as `wavecontrol_fix_v90_phase4.zip`)
- ✅ Phase 5: Surface valuable Wave data we already fetch (AirView + AFC) (delivered as `wavecontrol_fix_v91_phase5.zip`)
- ✅ Phase 6: Regression tests + performance (delivered as `wavecontrol_fix_v92_phase6.zip`)
- ✅ Phase 7: Peer discovery Plan B (probe alternate peer endpoints + optional fallback) (delivered as `wavecontrol_fix_v93_phase7.zip`)



- ✅ Phase 8: UI/labeling polish across panels (delivered as `wavecontrol_fix_v94_phase8.zip`)
- ✅ Phase 9: Peer parsing & station list endpoint (Wave) (delivered as `wavecontrol_fix_v95_phase9.zip`)
- ✅ Phase 10a: Role loop prevention + verify queue (part A) (delivered as `wavecontrol_fix_v96_phase10a.zip`)
- ✅ Phase 10b: Role loop prevention + verify queue (part B) (delivered as `wavecontrol_fix_v97_phase10b.zip`)
- ✅ Phase 10c: Role loop prevention + verify queue (part C) (delivered as `wavecontrol_fix_v98_phase10c.zip`)
- ✅ Phase 11: Connected-peers fallback hardening (delivered as `wavecontrol_fix_v99_phase11.zip`)
- ✅ Phase 12: MLO6 second radio display + schema stability (delivered as `wavecontrol_fix_v100_phase12.zip`)
- ✅ Phase 13: AirView utilization summary (min/median/max) + full bins table (delivered as `wavecontrol_fix_v101_phase13.zip`)
- ✅ Phase 14: AFC status + allowed channels (6 GHz) in the host panel (delivered as `wavecontrol_fix_v102_phase14.zip`)
- ✅ Phase 15: Standardize scheme resolution (http/https) across Wave + TLS tooling (delivered as `wavecontrol_fix_v103_phase15.zip`)

## Goal
1. Correctly display **both radios** on Wave MLO devices:
   - **MLO6**: one 5 GHz (5.1–5.8) + one 6 GHz (6.0–7.1) radio
   - **MLO5**: two 5 GHz radios
2. Fix **connected station/peer** parsing so WaveControl can show stations/clients accurately.
3. Surface valuable Wave-specific fields we already receive (AFC, AirView utilization, etc.).

## Inputs / Fixtures
Keep these JSON files as reproducible fixtures in `docs/fixtures/` (or `internal/testdata/`):
- `wave_mlo6_ultra.json` – Ultra debug export for a known MLO6 AP.
- `wave_mlo5_ultra.json` – Ultra debug export for MLO5.
- `wave_lr_ultra.json` – A legacy Wave-LR / Wave-Pro sample where `main` may be 60 GHz.

Each fixture should include (at minimum):
- `/api/v1.0/statistics`
- `/api/v1.0/device`
- `/api/v1.0/system/airos/configuration`

## High-level Design Decisions
### Radio “band” detection MUST be data-driven
Do **not** map `id == main/backup` to band.
Instead infer band using:
- Frequency:
  - `tx >= 57000` => 60 GHz
  - `tx >= 5925` (or 5945 depending on API) => 6 GHz
  - else => 5 GHz
- Presence of `afc` => 6 GHz (useful when `tx` is null/auto)
- Channel width hint:
  - very large widths (~1000+ MHz) => 60 GHz

### Two 5 GHz radios (MLO5)
WaveControl currently has one 5 GHz slot. Until we introduce a true “multi-radio list” model, the compatibility approach is:
- First detected 5 GHz radio => `Radio5GHz`
- Second detected 5 GHz radio => store into the existing “6 GHz” slot **but label it clearly in UI as "5 GHz #2"**.

This avoids dropping data while minimizing schema changes.

---

## Implementation progress

This repo has been updated incrementally in small “phase zips” to keep tool/runtime bounded.

- ✅ Phase 1 — Band inference (backend): `wavecontrol_fix_v87_phase1.zip`
- ✅ Phase 2 — Peer mapping fix (backend): `wavecontrol_fix_v88_phase2.zip`
- ✅ Phase 3 — UI render for 2nd radio + label fixes: `wavecontrol_fix_v89_phase3.zip`
- ✅ Phase 4 — Stations/clients mapping fixes: `wavecontrol_fix_v90_phase4.zip`
- ✅ Phase 5 — Surface extra Wave stats (AFC/AirView helpers): `wavecontrol_fix_v91_phase5.zip`
- ✅ Phase 6 — Data model consistency + minor fixes: `wavecontrol_fix_v92_phase6.zip`
- ✅ Phase 7 — Misc. cleanup + polish: `wavecontrol_fix_v93_phase7.zip`
- ✅ Phase 8 — UI/labeling polish across panels: `wavecontrol_fix_v94_phase8.zip`
- ✅ Phase 9 — Expose `wireless.radios[]` list + show in host panel: `wavecontrol_fix_v95_phase9.zip`

Remaining phases in this document are still planned.



---

# Phase 0 — Baseline + Repro
**Objective:** Reproduce today’s failure and identify the exact mapping assumptions.

### Changes
- Add fixture files (see above).
- Add a short developer note in `docs/` describing how to run parsing against fixtures.

### Validation
- Confirm current code maps `main/backup` incorrectly for MLO.
- Confirm peers are missing/misassigned when band mapping is wrong.

### Deliverable
- Zip with fixtures and baseline notes.

---

# Phase 1 — Implement band inference (backend)
**Objective:** Implement a single canonical function to map Wave radios into WaveControl slots.

### Changes
- Add helper:
  - `inferWaveBand(radioStats, radioCfg) -> enum {GHz5, GHz6, GHz60, Unknown}`
- Update Wave poller parsing to:
  - iterate `wireless.radios[]`
  - infer band
  - assign into internal stats model

### Validation
- Run parser against `wave_mlo6_ultra.json`:
  - 5 GHz radio populates 5 GHz UI block
  - 6 GHz radio populates 6 GHz UI block
- Run parser against `wave_lr_ultra.json`:
  - 60 GHz mapping remains correct

### Deliverable
- Zip that compiles and shows correct band mapping for MLO6 and legacy Wave.

---

# Phase 2 — Support MLO5 (two 5 GHz radios)
**Objective:** Ensure neither 5 GHz radio is dropped.

### Changes
- When two radios infer as 5 GHz:
  - first => `Radio5GHz`
  - second => `Radio6GHz` slot but set `DisplayBandOverride = "5 GHz #2"` (or similar UI label).

### Validation
- Run parser against `wave_mlo5_ultra.json`:
  - both radios appear
  - second is labeled correctly

### Deliverable
- Zip that compiles and renders two 5 GHz radios.

---

# Phase 3 — Fix peer/station mapping (backend)
**Objective:** Fix “WaveControl can’t figure out stations connected to the AP.”

**Status:** Implemented in `wavecontrol_fix_v89_phase3.zip`.

### Changes
- Use **the same band inference** for peer data:
  - If peer entries include radio id (`main/backup`), map that id to the inferred band based on the radios list.
  - Avoid hardcoding `main/backup`.
- Store peers into the band-correct structures.

**Implementation notes**
- `internal/poller/wave_parse.go`
  - Build a per-device `radioSlotByID` map while parsing `wireless.radios`.
  - Pass that mapping into `parsePeer(...)`.
  - In `parsePeer`, map `local` and `remote` radio entries into the correct per-band peer structures (5/6/60/LTU) using `radioSlotByID`.

### Validation
- With a fixture that has connected peers:
  - peers show under correct band
  - connected peer counts match device reality

### Deliverable
- Zip that compiles and shows stations correctly.

---

# Phase 4 — UI: display the second radio cleanly
**Objective:** Present multi-radio data without confusing labels.

### Changes
- Host panel:
  - If both 5 and 6 are present, show both sections.
  - If second 5 GHz is stored in the 6 GHz slot, label it "5 GHz #2".
- Dashboard columns (optional, if available):
  - keep existing Wave 60/5 columns stable
  - do not break layout for non-MLO devices

### Validation
- No layout regressions on dashboard
- Host panel shows correct sections

### Deliverable
- Zip that compiles and looks correct.

---

# Phase 5 — Surface valuable Wave data we already fetch

**Status:** ✅ Implemented (AirView + AFC)

- Parses Wave `/api/v1.0/statistics` `airview.utilization` into `wireless.airview_utilization`.
- Parses per-radio `afc` status/expiry/regulatory into `radio.afc` (6 GHz).
- Host panel now shows an **AirView Utilization** section, and 6 GHz radio cards show **AFC** status + expiry + allowed widths/channels summary.


**Objective:** Use existing payload fields to improve operations.

### Candidate fields (from the provided payload)
- AirView utilization (`airview.utilization[]`)
  - render a small sparkline / summary (min/avg/max utilization)
- AFC status + expiry + channel allowances (`wireless.radios[].afc`)
  - show AFC status label, expiry time
  - optionally a collapsible table of channelWidth -> allowed centers -> maxEirp
- GPS fix quality (`device.gps.dop`, `sats`, `fix`)
- Interface pps/rate/bytes (already present under `interfaces[].statistics`)

### Validation
- New fields display only when present
- No UI spam (collapse detail tables)

### Deliverable
- Zip with “Wave extras” and clean UI.

---

# Phase 6 — Regression tests + performance

**Status:** ✅ Implemented (tests + AirView cap)

**Objective:** Lock correctness and keep 5000+ devices fast.

### Changes
- Added unit tests for Wave band inference (`inferWaveBand`):
  - AFC ⇒ 6 GHz
  - frequency ≥ 57 GHz ⇒ 60 GHz
  - channel width ≥ 1000 MHz ⇒ 60 GHz
  - frequency ≥ 5945 MHz ⇒ 6 GHz
  - otherwise ⇒ 5 GHz (when frequency present)
- Added unit tests for multi-radio parsing (`parseStats`):
  - MLO6: `main`→5 GHz, `backup`→6 GHz (AFC captured)
  - MLO5: two 5 GHz radios, with the 2nd mapped into the “6 GHz slot” and labeled `5ghz#2`
- Added a sanity cap to AirView utilization parsing:
  - `maxAirViewUtilizationPoints = 512`
  - prevents pathological payload sizes from bloating memory on large fleets.

### Validation
- `go test ./...`
- `go build ./cmd/server`

### Deliverable
- Release-ready zip that includes tests and the AirView cap.


# Phase 7 — Peer discovery “Plan B” (only if peers are still missing)
**Objective:** If an AP *does* have connected stations but `wireless.peers` stays empty, add a low-risk fallback that discovers peers without guessing.

### Why this phase exists
- The provided MLO6 snapshot shows `wireless.peers: []`.
- That can be **legitimate** (no clients / link disconnected).
- But you reported a broader issue: *WaveControl can’t figure out stations connected to the AP.*
  - If that happens even when the AP clearly has stations, we need to confirm whether:
    - we’re parsing the peers list incorrectly, **or**
    - the peers list is exposed in a different endpoint for some Wave/MLO firmware.

### Changes (investigation-first)
1. **Collect a “known-good” Ultra Debug**
   - Pick a Wave/MLO AP with at least 1 connected station.
   - Capture an ultra debug poll at the same time the UI shows stations.
   - Confirm whether `GET /api/v1.0/statistics` includes non-empty `wireless.peers`.

2. **If peers are still missing in `/statistics`, identify the correct endpoint**
   - Use one of:
     - browser dev-tools on the radio UI while opening the “Stations/Peers” section, or
     - extend ultra debug to optionally hit “candidate endpoints” (below) and store their HTTP status + body size.

3. **Add a guarded fallback request**
   - Only run when:
     - device is AP mode, **and**
     - `/statistics` returns `wireless.peers: []`, **and**
     - a feature flag is enabled (config default: off until proven).
   - Candidate endpoints to probe (examples — do not commit to these until verified on a real device):
     - `/api/v1.0/statistics/wireless/peers`
     - `/api/v1.0/wireless/peers`
     - `/api/v1.0/peers`
   - Once the real endpoint is confirmed, remove the others.

### Validation
- On a device that truly has 0 peers, UI remains “no clients”.
- On a device with peers, WaveControl consistently shows the correct client count.
- Poll time impact: minimal (fallback should be rare and guarded).

### Deliverable
- Zip that adds fallback peer discovery behind a feature flag + ultra debug evidence.

✅ Implemented
- Added setting `wave_peer_fallback` (default: false) and plumbed it into the poller.
- When an AP reports `wireless.peers: []`:
  - If `wave_peer_fallback` is enabled, WaveControl will query a small set of candidate peer endpoints and use the first non-empty peer list.
  - Else (fallback disabled), if Ultra Debug is enabled for the device, WaveControl will probe the same candidate endpoints so the Ultra Debug capture includes their HTTP responses for diagnosis.
- Candidate endpoints currently probed (until verified on real device firmware):
  - `/api/v1.0/wireless/peers`
  - `/api/v1.0/statistics/wireless/peers`
  - `/api/v1.0/peers`

---

# Phase 8 — Make band mapping consistent across *all* Wave UI surfaces
**Objective:** Ensure the same radio/band inference is used everywhere we show Wave data (host panel, dashboard, quality pages, drilldown).

### Why
Even if the host panel is fixed, other pages can still appear “wrong” if they:
- still assume `main=60` / `backup=5`, or
- treat the “second 5 GHz” radio as 6 GHz without labeling.

### Changes
- Centralize Wave radio identification in one helper (single source of truth):
  - input: `wireless.radios[]` (statistics) + optional `wireless.interfaces[]` (configuration)
  - output: `map[id]Band` + stable ordering + “displayLabelOverride”
- Ensure all Wave rendering uses this mapping:
  - Host panel radio cards
  - Dashboard “radio” quick summary (if any)
  - Quality pages that depend on Wave radio stats

### Validation
- No contradictory UI labels across pages
- MLO6 always shows one 5 GHz radio + one 6 GHz radio
- MLO5 always shows two 5 GHz radios with correct labels

### Deliverable
- Zip that eliminates “split-brain” radio mapping.

---

# Phase 9 — Optional: upgrade Wave radios from fixed slots → dynamic list
**Objective:** Remove the long-term hack of storing “second 5 GHz” into the 6 GHz slot, by modeling Wave radios as a list.

### Changes
- Add a new internal struct (example):
  - `[]WaveRadioStats` with fields `{id, band, frequency, width, outputPower, afc, dfs, linkState, ...}`
- Keep existing fields for backward compatibility during transition:
  - `Radio5GHz`, `Radio6GHz`, `Radio60GHz` can be derived from the list
- Update API payloads and UI to iterate over the list when present
- Add DB migration only if we persist these per-radio details (prefer avoiding schema migrations if possible)

### Validation
- Existing Wave devices still display correctly
- MLO5/MLO6 render as 2 radios without any slot-reuse hacks

### Deliverable
- “Refactor zip” gated behind an internal flag (optional rollout).

---

# Phase 10a — Rollout telemetry counters
**Objective:** Provide fast visibility into Wave parsing behavior (band inference + peer discovery) without adding DB fields or UI complexity.

### Changes
- Add **in-process counters** (reset on restart):
  - Radio band inference totals: `5ghz`, `6ghz`, `60ghz`, `unknown`
  - Peer discovery source totals (AP-mode): `stats_peers`, `fallback`, `none`
  - Peer fallback attempted total
- Add a protected API endpoint:
  - `GET /api/wavecontrol/debug/wave-parse-counters` → returns a JSON snapshot with `started_at` + counter values

### Validation
- Hitting the endpoint shows non-zero counters on active systems.
- No noticeable performance impact (atomic increments only).

### Deliverable
- Zip with telemetry counters + debug endpoint.

---

# Phase 10b — Feature flags & safe defaults
**Objective:** Allow quick rollback if new parsing logic causes regressions.

### Changes
- Add feature flags (Settings table + UI):
  - `wave_mlo_multi_radio` — Enable Wave MLO multi-radio mapping (band inference + MLO5 second 5 GHz slot)
  - `wave_peer_fallback` — Enable Wave peer endpoint fallback (`/statistics` peers empty → query `/wireless/peers`)
- Default conservative (both off) until verified in the field.

### Validation
- Toggling either flag via Settings takes effect without restart (poller reloads settings periodically).

---

# Phase 10c — Ultra debug parse report (implemented in wavecontrol_fix_v98_phase10c.zip)
**Objective:** Make field debugging easy by embedding “why we decided this” into ultra-debug bundles.

### Changes
- Add a `wave_parse_report` block per poll that includes:
  - Per-radio band inference inputs (tx freq, AFC present, width) + result
  - Peer discovery path taken (stats vs fallback endpoint), including endpoint used
  - Any ambiguous/unknown reasons

### Validation
- Ultra debug bundles contain a clear one-page explanation for MLO radios.

### Implementation notes (v98)
- `internal/poller/wave_parse_report.go`: new helper that generates the plain-text report.
- `internal/poller/wave_poll.go`: captures peer fallback details (attempted/endpoint/count) and records a `wave_parse_report` entry into Ultra Debug (device + host scope) when enabled.

---

# Phase 11 — Display Wave MLO network/system details in the host panel
**Objective:** Surface key network/system values that already exist in Wave’s APIs (VLAN, IPs, gateway, DNS, port link), so operators can troubleshoot quickly without opening device UI.

### Why
From the MLO6 payload we already retrieve:

- `GET /api/v1.0/system` → hostname, domain, DNS, default gateway, management VLAN, management addresses (v4/v6)
- `GET /api/v1.0/system/airos/configuration` → bridge/router mode, MTU, static management IPv4, physical port config
- `GET /api/v1.0/statistics` → per-interface `plugged`, negotiated `currentSpeed`, counters and error rates

WaveControl currently surfaces only a subset (and different models expose different subsets). This phase makes the host panel consistent and “debug useful” for Wave MLO.

### Changes
Backend (Wave parser / stats model):
- Extend Wave stats extraction to persist the following (all optional / best-effort):
  - **Management VLAN:** `system.management.vlanID`
  - **Management addresses:** `system.management.addresses[]` (capture the “best” IPv4 + first non-link-local IPv6)
  - **Gateway & DNS:** `system.defaultGateway[]`, `system.dnsServers[]`
  - **Domain:** `system.domainName`
  - **Mode/MTU:** `configuration.network.mode`, `configuration.network.mtu`
  - **Data IPv4 CIDR/gateway:** `configuration.network.interfaces.data.ipv4.cidr` + `.defaultGateway`
  - **Port config:** `configuration.network.physicalPorts[]` (id, enabled, speed)
  - **Port link:** join with `statistics.interfaces[]` by `name`/`id` to capture `plugged`, `currentSpeed` (or `speed`) and show “link up/down”

UI (host panel):
- Add a **Network** section:
  - Management VLAN
  - Management IP(s) (v4 + v6)
  - Gateway
  - DNS
  - Domain
  - Mode + MTU
- Add a **Ports** mini-table (or stacked list) showing:
  - Port ID (eth0/eth1)
  - Config speed (auto/forced)
  - Link (plugged yes/no)
  - Negotiated speed (if available)

### Validation
- MLO6 sample should show:
  - VLAN 9
  - Management v4 CIDR `172.27.192.4/24`
  - DNS `199.68.200.2`, `199.68.207.2`
  - Gateway `172.27.192.1`
  - eth1 plugged, negotiated `1000-full` (per sample)
- Missing fields on older Wave/Wave-LR should not break rendering.

### Deliverable
- Zip with host-panel Network/Ports sections + JSON mapping notes.

---

# Phase 12 — Show per-interface throughput, errors, and packets for Wave MLO
**Objective:** Expose the interface counters/rates already in `/api/v1.0/statistics` so operators can quickly differentiate RF issues from backhaul/L2 issues.

### Why
The MLO payload includes per-interface:
- `rate` (bytes/sec-ish), `pps`, `rxRate`, `txRate`
- drops + errors (`rxErrors`, `txErrors`, `dropped`)

WaveControl currently shows limited interface info and may hide errors that are operationally important.

### Changes
- Backend:
  - Persist interface stats in a stable structure keyed by **interface id** (`lan/0`, `lan/1`, `main`, `backup`) and surface friendly names (`eth0`, `eth1`, `wifi0`, `wifi1`).
  - Normalize rates into bps for display (if the API returns bytes/sec, multiply by 8; if uncertain, label as “rate” and add tooltip).
- UI:
  - Enhance the existing **Interfaces** section (host panel) to include:
    - TX/RX rate
    - PPS
    - RX/TX errors
    - Dropped
  - Highlight non-zero error counters.

### Validation
- Sample should show `lan/1` has non-zero `pps` and `rate`.
- Verify no layout blow-ups on devices with many interfaces.

### Deliverable
- Zip with interface stats visible + tooltips on units.

---



**Implementation notes (v100):**
- Interface list is now sorted by ID/name before storing, so UI ordering is stable across polls.
- Host “Interfaces” section now shows Rate, PPS, Packets, Errors, Dropped, and Bytes.
- Non-zero error/dropped counters are highlighted.
- Rate line includes a tooltip clarifying units are as reported by the device (bps).

**Status:** Implemented in `wavecontrol_fix_v100_phase12.zip`

# Phase 13 — AirView utilization: summarize now, chart later
**Objective:** Start using `airview.utilization[]` to provide actionable congestion context in WaveControl.

### Why
The payload provides `airview.utilization` (frequency → usage%) spanning both 5 GHz and 6 GHz ranges. Even a simple summary (min/median/max) is valuable; a chart is even better.

### Changes
- Backend:
  - Store AirView utilization samples in Wave stats (bounded):
    - Keep at most N points per poll (or downsample to e.g. 5 MHz buckets)
    - Persist only a short window if needed (or store last snapshot only)
  - Compute and store summary per band:
    - 5 GHz: points with `freq < 5925`
    - 6 GHz: points with `freq >= 5925`
    - per-band: min/median/max usage
- UI:
  - Host panel: show “AirView usage (5 GHz / 6 GHz)” as a compact line:
    - `min/med/max` or `med/max`
  - Quality page (optional next step): add a small chart/heat strip.

### Validation
- Verify MLO6 shows both band summaries.
- Ensure AirView absence on non-Wave devices doesn’t create empty widgets.

### Deliverable
- Zip with AirView summary in host panel.

---


**Implementation notes (v101):**
- Host panel now shows AirView utilization summaries per band (5/6/60 GHz): min/median/max usage, plus peak (max) usage @ frequency.
- Summary is computed in the UI from `wireless.airview_utilization[]` (no backend schema change).
- Added two collapsible details views:
  - **Top channels** (highest utilization)
  - **All bins** (full snapshot sorted by frequency)

**Status:** Implemented in `wavecontrol_fix_v101_phase13.zip`

---

# Phase 14 — AFC status + allowed channels (6 GHz) in the host panel
**Objective:** Make AFC state visible and actionable for 6 GHz radios (especially MLO6 `backup`).

### Why
6 GHz operation depends on AFC state. The payload provides:
- `wireless.radios[].afc.status`, `label`, `expiry`
- `regulatory[]` listing allowed channels and max EIRP per channel width

This is extremely valuable for “why won’t it link on 6 GHz?” troubleshooting.

### Changes
- Backend:
  - Parse `afc` block when present and store:
    - status/type/label
    - expiry timestamp
    - per-width allowed channel centers + maxEirp (bounded; may store summaries if too large)
    - freqRanges (start/end)
- UI:
  - In the 6 GHz radio card (or whatever band the AFC-enabled radio resolves to), show:
    - AFC label + status
    - Expiry (human readable)
    - “Allowed channels” summary per width (count + min/max center + maxEIRP range)
  - Optional “details” expander to show full channel list when needed.

### Validation
- MLO6 `backup` radio shows AFC “Ready”, expiry, and allowed ranges.
- Ensure we don’t blow up the UI with hundreds of channels by default.

### Deliverable
- Zip with AFC status in host panel (summary first, full list behind expander).

---

# Phase 15 — Standardize scheme resolution (http/https) across Wave + TLS tooling
**Objective:** Remove the current “mixed hardcoded http/https” behavior by creating one canonical scheme resolver used everywhere (pollers, firmware upgrade, TLS pinning, UI links).

### Why
WaveControl today contains multiple ad-hoc URL builders (some default `http`, some `https`, some flip based on platform). This causes:
- cert learning/verification to miss devices
- UI links to open the wrong scheme
- inconsistent behavior between pollers and firmware subsystem

### Changes
- Introduce a shared helper:
  - `internal/netutil` (or similar): `ResolveScheme(host, platformHint) (scheme string, confidence string)`
  - Cache per-host scheme decision with TTL; persist last-known scheme in DB if helpful
  - Probe order:
    1. If DB has scheme for host, use it
    2. If TLS pin exists, prefer https
    3. Try https (HEAD or GET /device or /login), fall back to http if https fails
    4. Record result + reason
- Replace all ad-hoc URL concatenation with `BuildDeviceBaseURL(host)`.
- Ensure the TLS “Learn Missing Certs” path uses the same resolver + records failures.

### Validation
- On a mixed estate, “Learn Missing Certs” should attempt https first but fall back to http and mark those devices as “no TLS”.
- Host panel “Open UI” uses the resolved scheme.

### Deliverable
- Zip with scheme resolver wired into Wave poller + TLS tooling + UI links.

---

# Phase 16 — Make TLS certificate stats & modals fully live
**Objective:** Ensure TLS stats and related modals (Learn Missing / Verify Pending / Review Pending / View All) always reflect the real DB state and update immediately after actions.

### Changes
- Backend:
  - Add a dedicated endpoint returning TLS stats counts derived from DB (not cached UI state).
  - After any cert-learning or verification job completes, broadcast:
    - `tls_stats_update` event (counts + timestamps)
    - optionally `tls_certs_update` (if the cert list changed)
- UI:
  - Settings page subscribes to `tls_stats_update` and refreshes counters.
  - Ensure only **one** instance of each TLS modal can exist (debounce “View All Certs”).
  - After Learn Missing completes, auto-refresh the “View All” list if open.

### Validation
- Reproduce: click Learn Missing → count changes without page reload.
- Reopen settings after login: counts correct.

### Deliverable
- Zip with live TLS stats + modal singleton behavior.

---

# Phase 17 — Hierarchy loop guard + role re-verify workflow (AP/STA)
**Objective:** Prevent the “STA parented by STA” loop from persisting and make recovery automatic.

### Changes
- DB / backend:
  - On ingest: if `device.role == sta` and `parent.role == sta`, clear `parent_id` and mark device as “needs role verify”.
  - Add a “role verify” job that:
    - contacts the device directly (using scheme resolver)
    - reads platform-specific evidence (Wave: `configuration.wireless.interfaces[].apMode`; LTU: config/status role; airMAX: status role)
    - if verified AP, promote and rebuild peer relationships
- UI:
  - If a loop is detected, show a warning badge in host panel and a one-click “Re-verify role” action.

### Validation
- Force the known loop scenario and confirm it self-heals after verify.

### Deliverable
- Zip with loop auto-clear + verify job + UI warning/action.

---

# Phase 18 — Wave MLO fixtures + golden parser tests
**Objective:** Lock in correct behavior for MLO5/MLO6 (multi-radio parsing + display) so we don’t regress when Wave firmware/API evolves.

**Tool-time slice:** one short edit session to add fixtures/tests + one `go test ./...` run.

### What to add
- Add **redacted** ultra-debug JSON fixtures (strip creds/tokens) for:
  - Wave MLO6 (5 GHz + 6 GHz)
  - Wave MLO5 (5 GHz + 5 GHz)
  - (optional) Wave LR/Pro (60 GHz + 5 GHz) to ensure we don’t break legacy behavior.
- Add parser unit tests that assert:
  - radio band inference is correct even when `id == "main"/"backup"` is misleading
  - 6 GHz is detected via `tx >= ~5925` **or** presence of `afc`
  - 60 GHz is detected via very-high frequency or very-large channel widths
  - two 5 GHz radios are both retained (5 GHz #1 + 5 GHz #2)

### Acceptance criteria
- `go test ./...` passes.
- Tests fail if we accidentally revert to “main=60, backup=5” hardcoding.

### Deliverable
- Fixture JSONs + tests committed.

---

# Phase 19 — Radio inventory model + UI rendering for 2+ radios
**Objective:** Make the host panel (and any Wave device pages) render **all radios**, not a fixed set of “one-per-band” slots.

**Tool-time slice:** one session for backend model + one session for UI wiring.

### Backend changes
- Represent radios as a **list/map keyed by radio id** (e.g. `main`, `backup`, future ids) with fields:
  - `id`, `band` ("5", "6", "60"), `frequency_tx`, `channel_width_tx/rx`, `link_state`, `output_power`, `dfs`, `afc` (optional)
- Keep backward-compatible derived fields (if needed) but treat them as *views* of the list.

### UI changes
- Host info pane:
  - Render a **Radio Cards** section (one card per radio).
  - Each card shows:
    - Band + id label (e.g. “5 GHz (main)” / “6 GHz (backup)”)
    - Tx freq + channel width + power
    - Link state (connected/disconnected)
    - DFS status
  - If two 5 GHz radios exist (MLO5), show “5 GHz #1” and “5 GHz #2” explicitly.

### Acceptance criteria
- MLO6 shows two radios (5 + 6) consistently.
- MLO5 shows two radios (5 + 5) consistently.
- Legacy Wave LR/Pro still shows 60 + 5 correctly.

### Deliverable
- Backend model + UI rendering merged.

---

# Phase 20 — 6 GHz AFC details: status + expiry + allowed channels
**Objective:** Expose AFC visibility for 6 GHz links so operators can debug “why can’t I use channel X?” and validate regulatory data.

**Tool-time slice:** one backend session to parse/store AFC + one UI session for display.

### What to parse/store
From `wireless.radios[].afc`:
- `label`, `status`, `type`, `reason`, `detail`
- `expiry`
- Regulatory channel sets:
  - group by `channelWidth`
  - list `{center, maxEirp}`
  - list `freqRanges`

### UI display
- In the 6 GHz radio card:
  - AFC status (Ready / Pending / Failed)
  - Expiry (absolute + relative)
  - “View Allowed Channels” expandable table:
    - width → centers → maxEIRP

### Important performance note
- AFC regulatory arrays can be large. Consider storing:
  - current width’s channels only, or
  - a summarized representation (counts + ranges) in the DB, while keeping the full payload only in ultra-debug snapshots.

### Deliverable
- AFC shown for 6 GHz radios with sane size/perf.

---

# Phase 21 — AirView utilization: parse, store, and visualize
**Objective:** Use `airview.utilization[]` as actionable spectrum congestion data.

**Tool-time slice:** one backend session to store/downsample + one UI session to visualize.

### Backend
- Parse `airview.utilization[]` → list of `{frequency, usage}`
- Downsample/normalize:
  - keep only bins in the device’s supported ranges (from capabilities freqRanges)
  - optionally keep top-N “worst” bins + a coarse histogram

### UI
- Add a small “Spectrum Utilization” mini-panel:
  - quick summary: worst 3 frequencies + usage %
  - optional simple chart (bar/line) when expanded

### Deliverable
- Utilization visible and stable (doesn’t bloat DB).

---

# Phase 22 — Peer/station visibility: correct mapping + better UX when empty
**Objective:** Fix “WaveControl can’t figure out stations connected to the AP” for Wave MLO and avoid misleading “broken” output when peers are truly empty.

**Tool-time slice:** one session for parser correctness + one session for UX/retention.

### Parser correctness
- Ensure peers are mapped using the **same radio band inference** used for radios.
- Never attach peers to a band solely by `id == main/backup`.

### UX improvements
- If the device is AP mode and peers list is empty:
  - display “No stations connected at poll time” (not “unknown” / “broken”).
- Add a “Last seen peers” cache (short TTL, e.g. 10–30 min):
  - if peers disappear temporarily (brief disconnect), keep last snapshot but timestamp it clearly.

### Optional endpoint discovery
- If Wave firmware exposes a dedicated peer endpoint (not shown in this snapshot), implement a probe list:
  - try candidate endpoints and log 404s once per firmware version.
  - only enable the additional call when AP mode is true.

### Deliverable
- Correct peer mapping + non-misleading empty-state behavior.

---

# Phase 23 — AP/STA role inference for Wave MLO based on configuration
**Objective:** Make role detection robust (especially in MLO devices) without relying on brittle heuristics.

**Tool-time slice:** one backend session.

### Approach
- Use `/system/airos/configuration` → `wireless.interfaces[].apMode`:
  - If **any enabled interface** has `apMode == true` → role = AP
  - Else role = STA
- Store per-radio AP/STA mode in the radio card (so mixed modes are visible if they ever occur).

### Deliverable
- Role stays correct even if peers are empty or link is disconnected.

---

# Phase 24 — Storage + safety hardening for large Wave payload fields
**Objective:** Prevent DB bloat and avoid leaking secrets while still keeping useful diagnostics.

**Tool-time slice:** one backend session.

### DB size controls
- AirView utilization: store a compact representation (downsample / limit)
- AFC channels: store summary (counts + ranges) unless explicitly requested
- Keep full raw payload only in:
  - ultra-debug ring buffer (short retention)
  - on-demand “snapshot download” (explicit operator action)

### Secret redaction controls
- Ensure snapshots redact:
  - login creds
  - auth tokens
  - any “passphrase” / WPA fields

### Deliverable
- Safe + bounded storage behavior for Wave MLO telemetry.

# Phase 25 — Surface device health metrics (CPU/RAM/uptime/storage)
**Objective:** show the “non-wireless” device health data that already exists in the Wave payload so operators can distinguish RF issues from device/transport issues.

**Tool-time slice:** one UI pass (host panel rendering) + one backend pass (ensure fields are stored in stats model).

### What to display (from `/api/v1.0/statistics -> device`)
- **Uptime** (seconds → human)
- **CPU usage** (per-core / per-identifier) – for now show max + list in a collapsible section.
- **RAM** (usage %, free/total bytes)
- **Storage** list (name, type, used/size) + highlight partitions at >80% used.
- **GPS live fix health** should remain Phase 27 (we’ll keep this phase focused on general device health).

### Why this matters
- Firmware upgrade failures and “offline” flaps can be caused by CPU/memory pressure.
- Storage exhaustion is a common silent failure mode (logs/overlays).

### Deliverable
- Host panel shows a **Device Health** block for Wave-family devices.
- Values update via WS without requiring reload.


# Phase 26 — Surface interface/transport telemetry (eth/wifi counters)
**Objective:** use the existing `interfaces[]` statistics to quickly identify LAN issues vs RF issues.

**Tool-time slice:** small backend parse/normalize + UI table.

### What to display
- For each interface (`eth0`, `eth1`, `wifi0`, `wifi1`):
  - `status.plugged`, `status.speed`, `status.mtu` (plus description)
  - `statistics.rxRate/txRate` + `rxPPS/txPPS`
  - `rxErrors/txErrors`, `rxDropped/txDropped`

### Notes
- The sample shows `eth1` has rate/pps while others are zero. That’s valuable in itself.
- Don’t over-interpret rate units unless confirmed; label as **“reported rate”** initially.

### Deliverable
- New **Interfaces** block in host panel.
- Optional: a small red badge when errors/drops increase since last poll.


# Phase 27 — Surface live GPS fix quality + time sync indicators
**Objective:** display GPS fix quality from stats (not just configured location) so we can trust coordinates and spot GPS outages.

**Tool-time slice:** backend store + UI additions.

### What to display (from `/api/v1.0/statistics -> device.gps`)
- `fix` (true/false)
- `sats` (satellites)
- `dop` (dilution of precision)
- `dim` (2D/3D)
- `alt`, `lat`, `lon`
- `timeSyncEnabled` and `lastSync` (if meaningful)

### Deliverable
- Host panel shows **GPS: Fix (Y/N), Sats, DOP** and live coordinates.
- Map page can optionally prefer **live GPS coords** when fix is valid (with a clear tooltip).


# Phase 28 — Surface per-radio RF state details (DFS, CAC, power)
**Objective:** display RF state that is already present in `wireless.radios[]` but currently hidden.

**Tool-time slice:** small UI block per radio.

### What to display
- DFS:
  - `dfs.enabled`
  - `dfs.cacRemaining` / `dfs.cacDuration` (when present)
- Power:
  - `outputPower.autoPower`
  - `outputPower.conducted` and `outputPower.eirp`
- Channel width (tx/rx) + puncturing bitmap (where present)

### Deliverable
- Each radio card shows DFS + power summary.
- If CAC is running, show **“CAC in progress: Xs remaining”**.


# Phase 29 — AirView utilization: aggregate + visualize
**Objective:** convert the raw `airview.utilization[]` list into something operators can use quickly.

**Tool-time slice:** backend summarization + minimal UI graph/list.

### Backend summarization
- Compute:
  - **Top N congested frequencies** (highest usage)
  - **Average usage** across the list
  - **Current channel’s neighborhood** (±20/40 MHz window) average, if we know the active frequency.

### UI
- In host panel, show:
  - “Most congested: 5300 MHz (63%)” style list.
- In Quality (or a new “Spectrum” view), show a simple chart/table.

### Deliverable
- Operators can see congestion patterns at a glance without opening raw debug.


# Phase 30 — AFC deep-dive view (optional but very useful on 6 GHz)
**Objective:** expose the regulatory constraints the radio is operating under so channel planning is possible.

**Tool-time slice:** UI collapsible panel; keep backend changes minimal.

### What to display
- `afc.status`, `afc.label`, `afc.expiry`
- For each `regulatory[]` entry:
  - channelWidth
  - allowed channels (center + maxEirp)
  - freqRanges

### Deliverable
- Host panel “AFC” section per 6 GHz radio with a “Show channels” expander.


# Phase 31 — Normalize Wave AP/station mapping using SSID + band
**Objective:** reduce cases where Wave stations can’t be attached to the right AP due to ambiguous naming.

**Tool-time slice:** backend linking logic + UI validation.

### Approach
- When `wireless.peers[]` provides SSID/BSSID fields (future snapshots), link peers by:
  - SSID match
  - band match
  - frequency proximity (control/tx)

### Deliverable
- “Stations” table correctly groups peers under the right radio on MLO devices.


# Phase 32 — Operator tooling: export “sanitized snapshot” from UI
**Objective:** make it easy to collect future MLO snapshots without leaking creds/tokens.

**Tool-time slice:** UI button + small server endpoint.

### What it does
- Button in host panel: **“Download sanitized snapshot”**
- Server produces a JSON containing:
  - the relevant Wave endpoints’ responses
  - redacted headers (tokens)
  - no usernames/passwords

### Deliverable
- One-click artifact for debugging issues like “second radio missing” or “stations empty”.

---

# Phase 33 — Config vs runtime radio summary (operator-friendly)
**Objective:** show what the radio is *configured* to do versus what it is *currently doing* (and make “disconnected” states intelligible).

**Tool-time slice:** backend mapping (config ↔ stats) + UI block.

### Data sources
- Config: `/api/v1.0/system/airos/configuration -> wireless.interfaces[]`
- Runtime: `/api/v1.0/statistics -> wireless.radios[]`

### Mapping rule
- Prefer matching by `id` (`main`, `backup`).
- If ids ever diverge, fall back to band inference (frequency/AFC) and nearest-band match.

### UI output per radio
- **Mode**: apMode (AP/STA), ptpMode, linkDirection
- **SSID**
- **Configured**: band + control/tx freq (auto/manual), channelWidth tx (and puncturing auto/enabled)
- **Runtime**: current tx freq, rx/tx channelWidth, linkState

### Deliverable
- A “Radio Summary” section where each radio shows Config vs Runtime in two columns.


# Phase 34 — Capabilities viewer + radio capability hints
**Objective:** expose `GET /api/v1.0/device` capability data in a friendly way so we can debug why certain settings/telemetry differ by model.

**Tool-time slice:** mostly UI + light backend storage.

### What to display
- Device capabilities flags (supportMultiLinkOperation, supportPtpAp, supportPtpSta, etc.)
- Supported services/tools (handy for troubleshooting)
- Per-radio capability blocks:
  - freqRanges
  - supported channel widths
  - ieeeModes
  - txpower min/max
  - mcsRates label table (for interpreting modulations)

### Deliverable
- Host panel “Capabilities” expander (collapsed by default).


# Phase 35 — Wave poller: capability-gated endpoint strategy
**Objective:** reduce wasted calls, reduce error logs, and improve speed by calling only what a device supports.

**Tool-time slice:** one backend pass.

### Strategy
1. Always fetch `/api/v1.0/device` (identification + capabilities).
2. Only fetch `/api/v1.0/system/airos/configuration` if management config is supported.
3. Only fetch AirView utilization if device reports support for site survey / analytics (or if endpoint is known stable).

### Deliverable
- Cleaner logs + faster polling, especially in mixed fleets.


# Phase 36 — Multi-Link Operation (MLO) health indicators
**Objective:** give operators simple MLO-specific “is this behaving?” indicators.

**Tool-time slice:** UI + light parsing.

### Candidates from stats payload
- `wireless.serviceTime`
- `wireless.serviceUptime`

### Presentation
- Show service uptime as “MLO service up: 2h 36m”.
- If serviceTime is meaningful, label it as “Service time (reported): …” until we confirm semantics.

### Deliverable
- A small “MLO service” line in the wireless summary on MLO devices.


# Phase 37 — Radio ↔ interface correlation (wifi0/wifi1 ↔ main/backup)
**Objective:** ensure operators can reconcile “wifi0” counters with the correct radio in multi-radio devices.

**Tool-time slice:** mapping logic + UI label.

### Suggested correlation heuristics
- Match by order: first wireless interface in `interfaces[]` likely maps to `wireless.radios[0]`.
- Use `interfaces[].status.description` (“5.2 GHz WiFi interface”) as a band hint.

### Deliverable
- Radio cards show “Interface: wifi0” / “Interface: wifi1” when confidently mapped.


# Phase 38 — Wave station discovery expansion (endpoint probing)
**Objective:** if `wireless.peers[]` is frequently empty even when stations exist, add optional probing of additional endpoints (with safe fallbacks).

**Tool-time slice:** backend probe list + ultra-debug logging.

### Approach
- Add a small list of candidate endpoints (opt-in; do not spam):
  - e.g. `/api/v1.0/wireless/peers`, `/api/v1.0/peers`, `/api/v1.0/wireless/stations`
- On 404/405, record “unsupported” and back off.
- On success, normalize payload into the same peer model.

### Deliverable
- Increased station visibility on Wave families where peers aren’t present in `/statistics`.


# Phase 39 — Storage/performance safeguards for large Wave payload fields
**Objective:** prevent DB bloat and WS overload (AirView utilization + AFC channel tables can be large).

**Tool-time slice:** backend storage changes.

### Safeguards
- Persist summarized forms by default:
  - AirView: top N + average; optionally keep full list in memory only.
  - AFC: counts + expiry + a small sample; full list behind a “fetch on demand” action.
- Add size caps before broadcasting over WS.

### Deliverable
- Stable WS + DB size even with many MLO devices.


# Phase 40 — Validation pass in a mixed fleet (Wave LR/Pro + MLO5/6)
**Objective:** ensure the new logic does not break older Wave devices that historically used “main=60GHz / backup=5GHz”.

**Tool-time slice:** operational validation + small bugfix loop.

### Checklist
- Wave LR/Pro still shows 60 GHz and 5 GHz correctly.
- MLO6 shows 5 GHz + 6 GHz.
- MLO5 shows 5 GHz + “5 GHz #2”.
- Stations (when present) attach to the right radio.
- Host panel does not get slow or exceed WS limits.

### Deliverable
- A short validation note + any minor fixes bundled into the final zip.


# Phase 41 — Native mobile push clients
**Objective:** make Android/iOS clients efficient full-time alert receivers without relying on background WebSockets.

**Implemented in this bundle:**
- Added server-authoritative mobile push architecture: Alert Manager -> durable notification_outbox -> FCM/APNs.
- Added encrypted mobile token registry (`mobile_devices`) and per-user mobile preferences.
- Added mobile REST endpoints for token registration, preferences, bootstrap/reconcile, and test push.
- Added `mobile` as an alert notify channel for active and resolved alerts.
- Added direct FCM HTTP v1 and APNs token-auth providers using existing settings storage.
- Documented Android/Kotlin and iOS/SwiftUI client runtime expectations.

**Future client work:**
- Android Kotlin/Compose client with FCM, Room cache, DataStore settings, and foreground-only WebSocket.
- iOS SwiftUI client with APNs/FCM bridge, Keychain auth, local cache, and foreground-only WebSocket.
- Optional visible Android NOC foreground-service mode for charging/kiosk/field monitoring.
- Alert grouping by site/outage blast radius once site-level correlation is defined.


# Phase 42 — Explicit replacement AP/STA MAC adoption
**Objective:** resolve `mac_mismatch` safely when an AP or directly managed STA is physically swapped without corrupting inventory identity.

**Implemented in this bundle:**
- Added persisted `device_identity_mismatches` records with expected MAC, observed MAC candidates, observed IP, source, timestamp, and last error.
- Added `GET /api/wavecontrol/devices/{id}/identity-mismatch` for the device detail pane.
- Added `POST /api/wavecontrol/devices/{id}/learn-mac` requiring editor/admin permission.
- Learn flow verifies the device is still in `mac_mismatch`, the observed IP still matches, the requested MAC was actually observed, and the MAC is not already another inventory row.
- Learn flow updates the device MAC, rewrites child `parent_mac` references only for AP replacements, keeps STA AP association unchanged, clears mismatch state, removes stale in-memory stats, writes changelog, broadcasts WebSocket state, and queues a refresh.
- Added AP/STA detail-panel warning and **Learn replacement MAC** button.
- Added migration `016_device_identity_mismatches.sql` plus idempotent startup schema guard.

**Future work:**
- Add richer conflict UI for `observed_mac_already_exists` with “open existing device” and inventory merge actions.
- Add a small replacement-history view under the device changelog.


# Phase 43 — Managed STA replacement MAC adoption
**Objective:** make the explicit replacement-MAC workflow usable for directly managed STA radios as well as AP radios.

**Implemented in this bundle:**
- Removed the AP-only UI gate so `status_reason=mac_mismatch` shows **Learn replacement MAC** on AP and directly managed STA detail panes.
- Added role-aware confirmation text: AP adoption updates child station parent references; STA adoption keeps the AP association unchanged.
- Added role-aware reasons (`ap_replaced` / `sta_replaced`) from the browser.
- Guarded backend child `parent_mac` rewrites so they run only for AP replacements.
- Returned `child_parent_refs_updated` in the learn response for audit/debug visibility.

**Future work:**
- Add a separate discovered-only STA merge/adopt workflow for stations that appear only from AP peer lists and do not have a directly polled management IP.

# Phase 44 — AirMAX chain-report chain sanitization
**Objective:** prevent AirOS placeholder entries from being reported as real chain RSSI values in Chain Imbalance reports without discarding valid weak chains.

**Implemented in this bundle:**
- Added AirMAX chain sanitization for station and remote `chainrssi` arrays.
- Preserved normal positive AirMAX RSSI conversion (`RSSI - 95`) and legitimate negative dBm chain values.
- Dropped explicit zero placeholder entries only.
- Mirrored AirMAX STA-side remote chain values into the 5 GHz peer-radio structure so the chain report sees the same AP/STA link context as the UI.
- Added defensive chain-report sanitization before spread calculation, so stale/legacy in-memory zero placeholders cannot produce phantom findings.
- Added unit coverage for normal chain conversion, trailing zero placeholders, and legitimate negative dBm chain values including values near the reported noise floor.

**Future work:**
- Add raw-report fixture tests for edge-case AirOS chain arrays from field captures.

# Phase 45 — Host-pane direct platform-aware reboot
**Objective:** make the host info pane Reboot button send the correct remote-radio reboot command immediately instead of relying on ambiguous scheduled job behavior.

**Implemented in this bundle:**
- Added `POST /api/wavecontrol/devices/{id}/reboot` for direct editor/admin reboot actions.
- Added a **Reboot** button to the device detail / host info pane.
- Updated the existing context-menu reboot action to use the same direct endpoint.
- Added platform-aware reboot dispatch in the firmware service:
  - Wave, Wave MLO, and AirFiber 60 use REST `POST /api/v1.0/system/reboot`.
  - LTU / AirFiber 5XHD use REST `POST /api/v1.0/system/reboot`, with airMAX CGI `/reboot.cgi` fallback for hybrid builds.
  - airMAX AC/M and legacy AirFiber use CGI `POST /reboot.cgi` with `reboot=yes`.
- Updated scheduled/async reboot jobs to use the same device-ID/platform-aware reboot path.
- Hardened airMAX reboot handling so non-2xx `/reboot.cgi` responses are surfaced instead of silently treated as success.
- Added reboot platform inference tests for Wave/LTU/airMAX/AirFiber firmware flavors.

**Future work:**
- Add per-user confirmation preferences for destructive actions such as reboot/upgrade/restore.
- Optionally add a “reboot reason” field for changelog/audit workflows.


# Phase 46 — Preserve weak negative AirMAX chain readings
**Objective:** correct the chain-report over-sanitization that treated noise-floor-adjacent negative chain values as placeholders. Field captures can contain valid weak chain readings such as `-89 dBm`.

**Implemented in this bundle:**
- Removed the noise-floor-adjacent drop rule from AirMAX station and remote chain parsing.
- Removed the same drop rule from the Chain Imbalance report sanitizer.
- Kept the valid zero-placeholder drop rule for AirOS arrays like `[36, 34, 0]`.
- Updated unit tests so negative chain values equal to or near `noisefloor` are preserved as real dBm readings.
- Updated JSON flow/spec documentation to state that `noisefloor` must not be used to discard negative chain values.

**Future work:**
- Add a raw fixture for the 9029 La Donna Stafford capture if the full report/export path needs regression coverage beyond parser-level tests.

## v142 alert rules operator UI

- Added top-level Alerts navigation entry with active-alert count badge.
- Added active/recent alert list with status filter and acknowledge/resolve actions.
- Added Alert Rules table with edit/copy/delete controls.
- Added operator-facing rule form for all/site/device scoping, metric/operator/threshold, persistence delay, cooldown, and channels.
- Added presets for Host down, weak 5 GHz, weak 60 GHz, weak LTU, high CPU, high temperature, low 60 GHz capacity, peer count dropped, and low link score.
- Added mobile test push action from the Alerts page.
- Updated alert-rule creation so explicit `enabled: false` is preserved; omitted enabled still defaults to true for old clients.

Validation:
- `node --check web/js/api.js web/js/app.js web/js/components.js`
- Go parser over all Go files.
- `GOPROXY=off go test ./internal/airmax ./internal/firmware ./internal/stats ./internal/push`

Known limitation:
- Full dependency-backed `go test ./...` still requires module cache or network access for external dependencies.

- Added one-click recommended alert rule installer on the Alerts page; duplicate-safe and defaults to mobile push.

## v144 alert target policy

- Added device-level alert policy (`alertable`, temporary silence, notes).
- Added rule-level target filters (`target_role=all|ap|sta`, `require_alertable`).
- Alert evaluation now gates by rule scope, target role, device alertability, and silence window before metric evaluation.
- Added device pane alerting controls plus bulk mark-alertable / mute-alerts actions.
- Added alert-rule editor target controls and updated presets to use AP/STA roles.
