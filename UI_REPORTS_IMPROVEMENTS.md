# waveControl UI and Reports Improvements

## Scope

This update preserves the completed v145 security and scope remediation while rebuilding the report workflow and standardizing popup dialogs.

## Modal and dialog changes

- Added a shared modal controller with focus trap, focus restoration, Escape handling, backdrop policy, body-scroll locking, and responsive sizing.
- Replaced every loaded JavaScript use of browser-native `alert()`, `confirm()`, and `prompt()` with application dialogs.
- Standardized headers, subtitles, sections, callouts, forms, choice cards, validation, loading/empty/error states, and footers.
- Rebuilt dynamically generated dialogs for certificate management, drilldown lists, scheduled jobs, job details, maintenance windows, and destructive operational actions.
- Corrected maintenance-window editing: Edit now loads the selected record, displays region/site choices, validates the schedule, and performs PATCH instead of creating a duplicate.
- Added responsive full-screen report viewing and improved dark/light select, textarea, time-input, and disabled-control styling.

## Report engine v2

- Introduced schema-versioned snapshots with explicit scope, timestamp, inventory count, and metric coverage.
- Uses PostgreSQL inventory as the authority and joins live measurements by normalized MAC.
- Supports Network Health, Device Inventory, Performance Summary, Chain Imbalance, and RX Level Mismatch.
- Captures throughput history inside performance reports so reopened reports remain historically accurate.
- Adds AP/STA, platform, firmware, site, signal-quality, coverage, capacity-risk, stability, and offender summaries as appropriate.
- Ranks operational and RF findings deterministically by severity.
- Adds complete report-specific CSV output, including AP and STA performance rows and header-only empty diagnostics.
- Adds bounded report history filtering, creator metadata, robust not-found handling, and same-type comparisons for all report types.

## Report interface

- Report-type selection cards with clear purpose and generation permissions.
- Search and filter saved history by type, creator, date, or ID.
- Full-screen viewer with captured metadata, metric cards, sortable tables, report tabs, print, JSON, and CSV.
- Same-type before/after comparison with newer-minus-older deltas and metric-aware coloring.
- Explicit legacy warning when an old performance report lacks captured throughput history.

## Verification

See `RELEASE_VERIFICATION.txt` and `verification_ui_reports/` for syntax, static security, route authorization, visual-layout, offline compile/test/vet/race, and archive-integrity results. The real dependency test is retained separately because this isolated environment could not resolve `proxy.golang.org`.
