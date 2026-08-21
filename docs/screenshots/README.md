# README screenshots

The PNGs in this directory are deterministic sample views for the project README. They use WaveControl's production stylesheet with fictional rural Washington tower sites, fictional customer-station labels, placeholder metrics, and RFC 5737 documentation-only IP addresses; they contain no real customer data or credentials.

Regenerate all seven images from the repository root with:

```sh
python3 verification_ui_reports/generate_readme_screenshots.py
```

The generator requires Python 3 and Playwright for Python. It uses `WAVECONTROL_CHROMIUM` when set, otherwise `/usr/bin/chromium` when present, and finally Playwright's managed Chromium. The map preview is an offline, deterministic fictional rural Washington topology, so regeneration never depends on a map-tile service. Every map line carries an explicit AP and STA identity, and the generator rejects AP-to-AP, STA-to-STA, inferred site-to-site, missing-parent, duplicate-parent, or unlinked-STA examples. It also fails when a fixture introduces page-level horizontal overflow, produces an implausibly small image, restores any removed dashboard pill (`NO ALERTS` or a connected-peer count in the table or tree), paints dashboard signal cells with severity-colored backgrounds instead of coloring only the signal values, or drops representative tower/customer content.
