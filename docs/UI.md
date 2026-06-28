# netgov — Dashboard (UI) guide

The dashboard is a single localhost page served by `netgov web` (or the
`netgov-web` user service): **http://127.0.0.1:8474**. It mirrors the CLI — every
control maps to a `netgov …` command. Status auto-refreshes every ~15 s (paused
while you're typing in a field).

> **Safety model.** netgov only ever *adds* `ip rule`s in the priority band
> 8000–29999 and routes in tables 100–199. It never edits your main routing table
> or your NetworkManager profiles. **Restore** (or `netgov reset`) removes exactly
> that and your OS/NM baseline reappears. Nothing here is destructive.

---

## Header

- **APPLY ▸** — realise the current configuration (`netgov apply`). A `sudo` dialog
  appears (it edits routing). Run this after you change uplinks/rules/default by hand.
- **↻ refresh** — reload live status now.
- The subtitle shows the current overall default (`v4=… v6=…`) and, when armed,
  `ARMED(mode)`.

---

## Uplinks

A named egress path bound to a network device. The underlying connection
(addresses, Wi-Fi credentials, …) is configured in **NetworkManager / your OS
settings** — netgov only references the device and shows live status:

- **IPv4 / IPv6** columns: `up`, source address, gateway, and an `internet ✓ / no
  internet` probe.
- **↺** restores a link to its NM profile (`nmcli device reapply`); **×** deletes
  the uplink from netgov (not the connection).
- **define**: add an uplink — *name*, *iface*, optional *gateway* (only needed for a
  never-default LAN link). CLI: `netgov uplink define <name> --dev <iface> [--gw <ip>]`.

`netgov init` auto-discovers interfaces and seeds uplinks for you.

---

## Access points (named library)

APs are a **named library**. **Saving _defines_ an AP — it does not switch it on.**
This is deliberate: you can define an AP on the *same radio that's currently your
Wi-Fi uplink* (e.g. the internal Wi-Fi) **without** dropping that uplink — it only
goes live when you turn it on or a pattern activates it.

Builder fields: *name*, *radio* (Wi-Fi interface), *SSID*, *passphrase* (≥8),
*band*. **save** defines/updates it.

Per row:
- **on / off** — bring this AP up / down standalone. Turning one on **shadows that
  radio's uplink** (the radio is busy serving the AP). Only **one AP per radio** can
  be on at a time.
- **edit** — load it back into the builder (leave passphrase blank to keep it).
- **×** — delete the definition.

CLI: `netgov ap save <name> --dev <iface> --ssid <s> --psk <p> [--band bg|a]
[--channel N]`, then `netgov ap on|off|del <name>`.

---

## Destination rules — domain → uplink

Pin traffic **to a domain** out a chosen uplink (or `block` it). The domain is
resolved to its current IPs. Useful for a *lifeline pin* — keep one service on a
stable path regardless of the overall default, e.g. `api.example.com → wifi`.

Add: *domain*, *via* (uplink or `block`), *family* (`both`/`4`/`6`). CLI:
`netgov rule add --domain <d> --via <uplink|block> [--fam 4|6|both]`.

## Source rules — containers / VMs / subnet → uplink

Pin traffic **by where it comes from** — a detected container/VM bridge, a custom
CIDR, or an interface name (`iif`). E.g. send a Docker subnet out the cable while
the host goes elsewhere. CLI: `netgov rule add --from <CIDR|iface> --via <uplink|block>`.

RFC1918 / link-local always stays on the main table (your LANs keep working).

---

## Overall default — unpinned traffic

The per-family default for everything not matched by a rule: an **uplink**,
**block** (blackhole — leak-protect), or **(none)** = direct/main table. IPv4 and
IPv6 are independent; IPv6 defaults to `block` when the host has no global IPv6.
CLI: `netgov default set --v4 <uplink|block> [--v6 …]`.

---

## Patterns — roled

A **pattern** is a named, prioritised snapshot of egress policy (v4/v6 default +
rules) plus an optional **trigger** and **AP set**. Patterns let you switch whole
network personas by hand, or **arm** a background loop that picks the best one
automatically.

**Automation buttons:**
- **Arm** — start the root failover loop. It keeps the active pattern's internet
  healthy and, on a *sustained* outage (debounced), re-selects the highest-priority
  pattern whose requirements are met and whose default validates internet — else an
  always-reachable **floor** (auto-added). Applies dialog-free as root.
- **Dry-run** — same evaluation, but only *logs* what it would do
  (`journalctl -u netgov-roled -f`). Applies nothing.
- **Disarm** — stop the loop. **Boots disarmed.**
- **↻ eval now** — evaluate + apply the best pattern immediately.

**Building a pattern:**
- *name*, *prio* (higher wins).
- *v4 / v6* — the overall default this pattern sets (`direct` / `block` / an uplink).
- *require up* — uplinks that must be **up** for this pattern to be eligible.
- *SSID* + *on* — a **trigger**: the pattern is eligible only when this SSID (or any
  of a comma-list) is **in range** on the chosen Wi-Fi uplink (a cached scan — it
  never forces a disruptive scan on a connected STA).
- *AP on* — the AP(s) (by name) this pattern brings up. On activation netgov
  **swaps** to exactly these: it brings them up and takes any other netgov AP down.
  An AP on a radio this pattern uses as its Wi-Fi uplink is **masked** (you can't run
  AP and STA on one radio).
- *rules* — one per line: `selector uplink [fam]`, where selector is a `domain` or
  `from:CIDR`. e.g. `api.example.com wifi` or `from:172.18.0.0/16 cable`.
- **↧ snapshot current** — fill v4/v6/rules and tick the active AP from your *current*
  live config, so "save what I have now as a profile" is one click.

Per row: **activate** (switch to it now), **edit** (load into the builder), **×**
(delete). The badge by the title shows `ARMED · mode` or `disarmed`.

CLI: `netgov pat-set <name> <prio> [--require a,b] [--ssid S --ssid-iface <uplink>]
[--ap <name,…>] [--v4 …] [--v6 …] [--snapshot] [--floor]`, plus
`pat-apply | pat-del | eval [--apply] | arm [--dry] | disarm`.

---

## Restore

**⟲ Restore to NetworkManager** (`netgov reset`) flushes **all** netgov rules and
tables — your OS/NM baseline reappears intact. netgov never edited NM itself, so
there's nothing else to undo. (Disarm first if armed.)

## Log

Shows the result of the last action (apply output, AP messages, eval results, …).

---

## Typical first run

```sh
netgov init                                  # discover interfaces -> uplinks
# (configure Wi-Fi/connections in NetworkManager as usual)
netgov default set --v4 cable --v6 block     # everything out the cable, no v6 leak
netgov rule add --domain api.example.com --via wifi   # keep one service direct
netgov apply
netgov install                               # optional: web service + failover unit
```
