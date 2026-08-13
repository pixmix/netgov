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
- *claim* — **same-address arbitration** (see below): `<address> <dev:prio[:ssid,…]>…`,
  e.g. `192.168.222.153 enp114s0:100 wlo1:50:CNNet`. Leave empty for none.
- **↧ snapshot current** — fill v4/v6/rules and tick the active AP from your *current*
  live config, so "save what I have now as a profile" is one click.

Per row: **activate** (switch to it now), **edit** (load into the builder), **×**
(delete). The badge by the title shows `ARMED · mode` or `disarmed`. A row carrying a
claim is marked with the address it can move (`⇄192.168.222.153`) — this is the one
pattern property that can **move an address between adapters**, so it is visible at a
glance rather than only on edit.

CLI: `netgov pat-set <name> <prio> [--require a,b] [--ssid S --ssid-iface <uplink>]
[--ap <name,…>] [--v4 …] [--v6 …] [--snapshot] [--floor]`, plus
`pat-apply | pat-del | eval [--apply] | arm [--dry] | disarm`.

---

## Same-address arbitration (claims)

For a host that must keep **one identity on either medium** — a DHCP reservation held
for *both* its MACs, so it is the same address on cable or Wi-Fi. `dnsmasq` warns that
this "will only work reliably if only one of the hardware addresses is active at any
time and there is no way for dnsmasq to enforce this". netgov is that enforcer: **one
address, N adapters, exactly one holder**, chosen by priority.

The claim lives **on a pattern**, because address identity belongs to a site rather than
to a box — you may be somewhere with none of your usual routers. It is **inert unless
that pattern is active**.

Set it in the **pattern builder's `claim` field**, in the same grammar as the CLI so the
two cannot drift into dialects:

```
192.168.222.153 enp114s0:100 wlo1:50:CNNet
   address        wired,      Wi-Fi, only when associated to CNNet
                  priority100  priority 50
```

Highest-priority **eligible** adapter wins; eligibility is **carrier + association + the
gateway answering ARP on that interface** (needs `arping`; absent, that last test fails open).
Carrier alone is not a health signal — a cable can negotiate 1000/full and still lose most
frames, and an arbiter trusting carrier would hand the address to it.
Listing SSIDs on a Wi-Fi claimant matters: an adapter associated to a *different* network
is not a path to that address. (Pointing one at an upstream WAN SSID would let the arbiter
move the address somewhere it can never be reached.)

**Two different arm switches, and `arm` is not the one you want here:**

| switch | arms | how |
|---|---|---|
| **Arm** button / `netgov arm` | the **pattern** failover loop | button, or `arm`/`disarm` |
| `/etc/netgov-claim.armed` | the **address arbiter** | create/remove the file as root |

Arming the loop does **not** arm arbitration. Both boot disarmed, and until the flag
exists the arbiter only reports what it *would* do (`netgov claim` is always safe to run).

**Safety:** claim-before-release — it only ever *moves* an address, and if no claimant is
eligible the current holder keeps it, so a box is never left with no address. Hand-over
sends a gratuitous ARP, because a failover the segment cannot see is not a failover.

> ⚠️ **Gating DHCP is necessary but not sufficient.** A standby holding no lease still
> answers ARP for the address (Linux `arp_ignore=0`) and still competes to be the reply
> path. On a host with two NICs on one subnet, also set `arp_ignore=1`/`arp_announce=2`
> with per-interface source routing, or keep the standby off the segment — then verify
> from the *segment* that exactly one MAC answers.

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
