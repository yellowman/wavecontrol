# AirMax Web UI API Spec (Dissected from JavaScript Handlers)

This document is derived from the client-side handlers found in:

- `airmax.main.js`
- `airmax.vendors.js`

It reflects **what the UI code actually calls** (fully observed), plus a list of endpoints that are **enumerated in UI settings** but **not invoked** in these two bundles (so request/response details are unknown from this evidence alone).

---

## Table of contents

- [1. Cross-cutting behavior the UI assumes](#1-cross-cutting-behavior-the-ui-assumes)
- [2. Endpoints with complete observed request specs](#2-endpoints-with-complete-observed-request-specs)
  - [Authentication](#authentication)
  - [Public info and localization](#public-info-and-localization)
  - [Device status and regulatory domain](#device-status-and-regulatory-domain)
  - [Configuration read/write/apply/discard](#configuration-readwriteapplydiscard)
  - [Password and credentials management](#password-and-credentials-management)
  - [System actions](#system-actions)
  - [Discovery and station management](#discovery-and-station-management)
- [3. Endpoints enumerated in UI settings but not invoked](#3-endpoints-enumerated-in-ui-settings-but-not-invoked)
- [4. One extra endpoint used by the UI but not in settings](#4-one-extra-endpoint-used-by-the-ui-but-not-in-settings)
- [5. External / non-device URLs referenced](#5-external--non-device-urls-referenced)

---

## 1) Cross-cutting behavior the UI assumes

### Base URL / origin

- Requests are **same-origin** (device host serving the UI).
- Some URLs are stored without a leading slash (e.g. `api/auth`), but when the UI is served from `/` or `/index.html` these resolve to `/api/auth`.

### Encoding / content types

- All observed `POST`s are jQuery `$.ajax({ data: ... })` style -> `application/x-www-form-urlencoded; charset=UTF-8` body encoding (typical jQuery behavior).
- Responses are typically expected as:
  - `dataType: "json"` for most actions
  - `dataType: "text"` for a few endpoints (config read, active channel list)

### CSRF model (important)

- Login responses provide a CSRF token in a response header:
  - **Response header:** `X-CSRF-ID: <token>`
- The UI installs a global `$.ajaxSetup` hook that automatically adds:
  - **Request header:** `X-CSRF-ID: <token-from-session>`
  - for all same-origin requests (`!crossDomain`)

### Auth/session error handling

If the UI believes you are logged in and any request returns **HTTP 403**, it:

- clears the session
- reloads the page

### Cookie requirement during login

After login, the UI checks for a cookie matching `/\bok=1\b/` in `document.cookie`.

- If it doesn't see it, it treats login as failed and shows a "cookies must be enabled" style error.
- This implies the server sets or requires an `ok=1` cookie during/after auth.

### "Standalone/demo mode" URL rewriting

There's a URL rewrite helper `u(url)` that does this when `window.sa` is truthy:

- Most URLs get rewritten to `resources<path-without-extension>.json`
- Logout gets rewritten to `index.html`

This is for a demo/offline mode and doesn't change the real API, but it hints the UI expects many endpoints to be JSON-ish in normal mode.

---

## 2) Endpoints with complete observed request specs

These are endpoints where the bundle includes the actual request code (method + parameters), not just a URL constant.

### Authentication

#### `POST api/auth` (login)

- **Method:** `POST`
- **URL:** `api/auth` (resolves to `/api/auth` in normal deployment)
- **Body (form-urlencoded):**
  - `username: string` (login form `maxlength=64`)
  - `password: string` (login form `maxlength=64`)
- **Response:** `application/json`
  - On failure, the UI expects an `error` field:
    - `error?: string`
  - On success, it stores these response fields into session if present:
    - `readOnlyUser?: boolean | number`
    - `fullVersion?: string`
    - `ccode?: number`
    - `rd?: any` (regdomain data blob; structure not visible in these bundles)
    - `rd60?: any` (same idea for 60 GHz)
    - `boardinfo?: string` (looks like plain-text-ish board info stored in session)
- **Response headers (required by UI):**
  - `X-CSRF-ID: string` (must be present or UI rejects login)
- **Side conditions:**
  - UI requires cookies to work (expects `ok=1` cookie to appear).

#### `POST api/auth-ticket` (login via ticket)

- **Method:** `POST`
- **URL:** `api/auth-ticket` (resolves to `/api/auth-ticket`)
- **How invoked:** if URL hash matches `#ticketid=...`
- **Body (form-urlencoded):**
  - `ticketid: string`
- **Response:** same handling as `api/auth` (expects CSRF header, JSON body with same session fields)

#### `POST /logout.cgi`

- **Method:** `POST`
- **URL:** `/logout.cgi`
- **Body:** none observed
- **Response:** ignored (UI clears session & reloads regardless)

---

### Public info + localization

#### `GET /api/info/public`

- **Method:** `GET` (jQuery default when `type` not set)
- **Query parameters:**
  - `include_langs: boolean` (UI passes `true`)
  - `lang: string | undefined` (UI passes `UBNT.Utils.Session.get("new_ui_lang")`)
- **Response:** `application/json`
- **Observed response fields used by UI:**
  - `ui_lang: string`
  - `ui_langs: Array<{ lang: string, code: string }>` (used to populate language dropdown)
  - `ui_translations: { strings?: Record<string,string> }`
  - `product_name: string` (shown on login/setup pages)
  - `uservice_active: boolean` (toggles a UI icon)
  - `default_password: boolean` (if true, UI prefills username/password from defaults)
  - `setup_data: {`
    - `rd_countries: Array<{ name: string, code: number }>` (country list)
    - `license_agreement: string`
    - `...: any`
    - `}`

There are likely more fields, but these are the ones referenced in handlers/templates in these bundles.

---

### Device status + regulatory domain

#### `GET /status.cgi`

- **Method:** `GET` (`$.getJSON`)
- **URL:** `/status.cgi`
- **Params:** none observed
- **Response:** `application/json`
- **Schema:** not directly visible here (the status payload is fed into a `Status` model class that lives in other lazy-loaded chunks not included in what you uploaded).
- **Note:** the UI treats the first resolved value as the actual JSON (`e[0]` pattern via `$.when` + jqXHR tuple semantics).

#### `GET /api/regdomain`

- **Method:** `GET` (`$.getJSON`)
- **URL:** `/api/regdomain`
- **Params:** none observed
- **Response:** `application/json`
- **Observed response fields:**
  - `data: any`
  - `data60: any`
- **Usage:** called when the configured `radio.countrycode` differs from cached session `ccode`.

#### `POST /api/regdomain/activate-unii`

- **Method:** `POST`
- **URL:** `/api/regdomain/activate-unii`
- **Body (form-urlencoded):**
  - `name: string`
  - `key: string` (UI normalizes it as: remove `-` and `.trim()`)
- **Response:** `application/json` (UI requests `dataType:"json"`)
- **Purpose:** "Unlock UNII" / regulatory activation.

---

### Configuration read/write/apply/discard

#### `GET /getcfg.cgi`

- **Method:** `GET`
- **URL:** `/getcfg.cgi`
- **Response type:** `text` (UI requests `"text"`)
- **Response body:** plain text configuration that the UI parses into a config store.
- **Observed response headers:**
  - `X-Cfg-Modified: "1" | other` -> UI maps to `is_backup: boolean`
  - `X-Cfg-First-Setup: "1" | other` -> UI maps to `from_first_setup: boolean`
- **UI return object shape (internal):**
  - `cfg_store: <parsed from text>`
  - `is_backup: boolean`
  - `from_first_setup: boolean`

#### `POST /writecfg.cgi`

- **Method:** `POST`
- **URL:** `/writecfg.cgi`
- **Body (form-urlencoded):**
  - `cfgData: string` (serialized full config text)
  - `testmode?: "yes"` (added when caller requests test mode)
  - `nosave?: "yes"` (added if UI is in "no save mode")
- **Response:** not explicitly typed in this bundle (no `dataType` specified in the call site)

#### `POST /apply.cgi`

- **Method:** `POST`
- **URL:** `/apply.cgi`
- **Body (form-urlencoded):**
  - `apply: "yes"`
- **Response:** JSON (`dataType:"json"`)

#### `POST /discard.cgi`

- **Method:** `POST`
- **URL:** `/discard.cgi`
- **Body (form-urlencoded):**
  - `d: number` (UI always sends `0`)
  - `testmode?: "yes"` (if caller requests)
- **Response:** JSON (`dataType:"json"`)

---

### Password / credentials management

#### `POST /pwd.cgi` (multi-action endpoint)

This endpoint is used in two different "modes" depending on fields.

**A) Password check**

- **Method:** `POST`
- **URL:** `/pwd.cgi`
- **Body:**
  - `check: "yes"`
  - `ro: 0 | 1`
  - `pwd: string`

**B) Change password (and possibly username)**

- **Method:** `POST`
- **URL:** `/pwd.cgi`
- **Body:**
  - `change: "yes"`
  - `ro: 0 | 1`
  - `pwd: string` (new password)
  - `oldPwd: string`
  - `newUsername: string`

**Response:**

- JSON (`dataType:"json"`)
- Exact schema not shown in these bundles (likely `{ success: boolean, error?: string }` pattern, but that's not directly referenced here).

---

### System actions

#### `POST /reboot.cgi`

- **Method:** `POST`
- **URL:** `/reboot.cgi`
- **Body:**
  - `reboot: "yes"`
- **Response:** JSON (`dataType:"json"`)

#### `POST /reset.cgi`

- **Method:** `POST`
- **URL:** `/reset.cgi`
- **Body:**
  - `reset: "yes"`
- **Response:** JSON (`dataType:"json"`)

#### `POST /api/provmode`

- **Method:** `POST`
- **URL:** `/api/provmode`
- **Body:**
  - `action: "start" | "stop"`
- **Response:** JSON (`dataType:"json"`)
- **UI naming:** "Management mode" start/stop.

---

### Discovery / station management

#### `POST /discovery.cgi`

- **Method:** `POST`
- **URL:** `/discovery.cgi`
- **Body (form-urlencoded):**
  - `discover: "y"`
  - `duration: number` (UI passes `e || this.options.duration`, default `1000`)
  - `filter_aircube: any` (UI passes `i`; used as boolean-ish flag)
  - `host?: string` (only if `this.options.targetIp` is set)
- **Timeout behavior:**
  - `timeout = duration + 5000` (ms)
- **Response:** JSON (`dataType:"json"`)
- **Observed response field:**
  - `devices: Array<any>` (UI resolves with `t.devices`)

#### `POST /stakick.cgi`

- **Method:** `POST`
- **URL:** `/stakick.cgi`
- **Body (form-urlencoded):**
  - `staif: string` (wireless interface name from config, via `getWlanInterface()`)
  - `staid: string` (station identifier, likely MAC)
- **Response:** JSON (`dataType:"json"`)

---

## 3) Endpoints enumerated in settings but not actually invoked in these two bundles

These URLs are present in the central `_settings={...}` block, but **no request code exists** in the two files you provided, so method/params/schemas are **not knowable** from this evidence alone.

I'm still listing them because they are clearly part of the UI's API surface.

### `/api/*` endpoints (unknown methods/params in provided code)

- `/api/cfg` (partial config write endpoint; unused here)
- `/api/dhcp-client/info`
- `/api/dhcp-client/renew`
- `/api/dhcp-client/release`
- `/api/dhcp-server/leases`
- `/api/firewall/user-rules`
- `/api/firewall/port-forward`
- `/api/info/help`
- `/api/logs`
- `/api/logs/clear`
- `/api/pppoe/info`
- `/api/pppoe/restart`
- `/api/service-config`
- `/api/traceroute`
- `/api/unms/enable`
- `/api/unms/disable`
- `/api/warnings`
- `/api/warnings/dismiss`

### `*.cgi` endpoints (unknown methods/params in provided code)

- `/airviewdata.cgi`
- `/amdata.cgi`
- `/arp.cgi`
- `/brmacs.cgi`
- `/ethtool.cgi`
- `/fwflash.cgi`
- `/ipscan.cgi`
- `/pingtest_action.cgi`
- `/poll.cgi`
- `/sptest_action.cgi`
- `/sroutes.cgi`
- `/survey.json.cgi`
- `/temperature.cgi`
- `/test_mode.cgi`
- `/crashlog.cgi`

### Device setup endpoint (partially inferable)

`api/device-setup` is present in settings, but no request call site is in these bundles.

However, the setup wizard collects a `results` object with these fields, which are strong candidates for what gets POSTed somewhere (likely to `api/device-setup`):

- `device_setup: "user" | "cfg" | "first"`
- `new_username: string` (from first-time username form)
- `new_password: string`
- `new_ccode: number` (country code selection)
- `new_lang: string` (language selection)
- `new_config: string` (uploaded config text)

These are **inferred from client-side data collection**, not from an observed request.

---

## 4) One extra endpoint used by the UI that is not in the settings block

### `GET /chanlist_active.cgi`

- **Method:** `GET`
- **URL:** `/chanlist_active.cgi`
- **Params:** none
- **Response type:** `text`
- **Response format (as parsed by UI):**
  - A space-separated list of frequencies, e.g. `"2412 2417 2422 ..."`
- **UI parses into:**
  - `Array<{ freq: number }>` where `freq = parseInt(token)`

---

## 5) External / non-device URLs referenced

These aren't device-local API endpoints, but they're referenced by constants/settings:

- `//airos-api.ubnt.com/crashreport/` (crash report upload target; not invoked in these bundles)
- `https://link.ubnt.com` (AirLink portal; not an API used by ajax here)
- `https://dev-link.ubnt.com` (dev AirLink portal; not invoked in these bundles)
- `http://192.168.1.20/` (default device URL constant)

