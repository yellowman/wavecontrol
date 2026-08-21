# Alerters: sending alerts to sysmon-web without being a sysmond

An alerter is a daemon with something to say and no fleet of hosts
behind it - a backup job, a UPS script, a RAID monitor, a cron
watchdog. It connects to the same TLS listener the monitoring boxes
use, proves itself with the same kind of minted token, and then sends
alerts. Those alerts ride the exact push pipeline a sysmond's host
transitions ride, with the same priorities: CRITICAL is loud on the
phones, WARNING and OK are quiet.

An alerter is never part of the fleet. It has no config to manage, no
hosts to poll, no generations to roll out. The Fleet page lists it in
its own "Alerters" section; the config editor and the map never see it.

## Getting a token

Same as a monitoring box: **Admin -> Monitoring boxes -> Add a box**.
Mint a token under the name the alerter will use (letters, digits,
`-`, `_`; max 64 chars). The name is the alerter's identity - it
appears in notifications and on the Fleet page - so name the thing,
not the machine: `backupd`, not `server3`.

Revoking the token on the same page cuts the alerter off at its next
connection attempt.

## Connecting

- **Transport**: TLS to sysmon-web's agent port (default `1347`, the
  `-agent-listen` flag). TLS is required, not negotiable - the first
  line carries a bearer token.
- **Server certificate**: sysmon-web generates a self-signed
  certificate on first start (or uses `-agent-cert`/`-agent-key`).
  Verify against that certificate - the same `aggregator-ca.pem` a
  monitoring box pins. Skipping verification hands your token to
  whoever answers the port first.
- The connection is long-lived. Stay connected and send alerts as they
  happen; reconnect with backoff when the link drops. The server never
  polls an alerter, so a silent alerter costs nothing.

## Protocol

Text lines, terminated by `\n` (a trailing `\r` is tolerated). Every
reply is one line starting `333 ` (success) or `444 ` (refusal).

### Handshake (first line, within 20 seconds of connecting)

    ALERTER <name> <token> [application name...]

- `333 welcome` - authenticated; send alerts from here on.
- `444 rejected` - bad name/token pair, or the token is revoked. The
  socket closes; back off before retrying.

Everything after the token is what the application calls itself -
free text up to 128 characters, e.g. `Bacula 15.0 nightly backups`.
It shows on the Fleet page and in alerts. Optional but worth sending:
the token name identifies, this describes.

The greeting verb is what separates an alerter from a monitoring box:
a sysmond says `HELLO` and gets polled, an alerter says `ALERTER` and
does the talking.

### Sending an alert

    ALERT <CRITICAL|WARNING|OK> <object> <text...>

- `<object>` names the thing the alert is about (same character rules
  as the alerter name). One alerter can alert about many objects.
- `<text>` is free-form to end of line, up to 512 characters
  (anything longer is truncated, not refused). Optional - omitted, a
  plain "name reports object STATUS" is generated.
- Reply is `333 ok` once accepted, or `444 <reason>` for a malformed
  line. A `444` never closes the connection; fix the line and carry on.

Semantics, identical to a sysmond's transitions:

- **CRITICAL** delivers loud: sound, heads-up on Android, time-sensitive
  on iOS.
- **WARNING** and **OK** deliver quiet: they land in the notification
  shade without a sound. Send `OK` when the condition clears - it
  replaces the earlier alert on the phones rather than stacking a
  second notification, because `<alerter>:<object>` is the collapse
  key, exactly as host alerts collapse per host.
- Delivery honors the master push switch in the admin UI; alerts sent
  while push is disabled are acknowledged and dropped, and the server
  log says so.

### Keepalive and goodbye

    PING            ->  333 pong
    QUIT            ->  333 bye (server closes)

Send `PING` every minute or so if your network kills idle
connections; the server does not require it.

## Names, nicknames, and what an alert shows

Three names are in play, in order of what alerts display:

1. **Nickname** - optional, set by an admin on the Fleet page's
   Alerters card (the pencil next to "Shows as"). Wins when set.
2. **Application name** - what the alerter declared at handshake.
3. **Token name** - the identity; the fallback when nothing else is set.

The token name is what keys everything internally - collapse keys,
logs, the registry - so renaming a nickname never re-keys anything.

## What the web UI does with alerts

- Push notifications to every subscribed phone, with the priority
  routing above.
- The admin **Push Log** records each fan-out like any other.
- The **Fleet page** shows the alerter: connected or gone, what it
  shows as (nickname or application name), its address, how many
  alerts it has sent, and the last one.

Alerts are fire-and-forget by design: they are not stored as host
state, do not appear on the dashboard, and are not replayed to phones
that subscribe later. If a thing needs its state *tracked*, it wants
to be a monitored host on a sysmond, not an alerter.

## Example: shell

```sh
# One-shot alert via openssl s_client (BSD echo; adjust for your shell).
{
  echo 'ALERTER backupd tok-abc123... Bacula 15.0 nightly backups'
  sleep 1
  echo 'ALERT CRITICAL nightly-backup tape jam in drive 2'
  sleep 1
  echo 'QUIT'
} | openssl s_client -quiet -connect sysmon-web.example.net:1347 \
      -CAfile aggregator-ca.pem -verify_return_error
```

## Example: Python

```python
import socket, ssl, time

HOST, PORT = "sysmon-web.example.net", 1347
NAME, TOKEN = "backupd", "tok-abc123..."

ctx = ssl.create_default_context(cafile="aggregator-ca.pem")
ctx.check_hostname = False  # self-signed cert carries no hostname

def connect():
    raw = socket.create_connection((HOST, PORT), timeout=20)
    tls = ctx.wrap_socket(raw)
    f = tls.makefile("rw", newline="\n")
    f.write(f"ALERTER {NAME} {TOKEN} Bacula 15.0 nightly backups\n"); f.flush()
    if not f.readline().startswith("333"):
        raise RuntimeError("rejected")
    return f

def alert(f, status, obj, text):
    f.write(f"ALERT {status} {obj} {text}\n"); f.flush()
    return f.readline().startswith("333")

f = connect()
alert(f, "CRITICAL", "nightly-backup", "tape jam in drive 2")
# ... later, when it clears:
alert(f, "OK", "nightly-backup", "backup completed after operator fixed the jam")
```

Reconnect on any read/write error, with backoff (the monitoring boxes
use a few seconds, doubling to a minute; do the same). The token is a
credential - keep it out of argv and world-readable files.
