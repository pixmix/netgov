# CLI Reference

The CLI and the [dashboard](UI.md) are equivalent. Privileged verbs
(`apply`, `reset`, `arm`, `disarm`, `pat-apply`) re-exec via `sudo -A` (honour
`SUDO_ASKPASS`); run them as root to skip the dialog.

State lives in `~/.config/netgov/state.json` (override with `NETGOV_STATE` or
`--state <path>`).

## Core

| Command | What it does |
|---|---|
| `netgov status` | live view: uplinks, rules, default, APs, bridges |
| `netgov plan` | dry-run: print the `ip` plan, execute nothing |
| `netgov apply` \| `refresh` | realise the config (root; idempotent) |
| `netgov reset` | remove all netgov rules → pure NetworkManager baseline |
| `netgov init` | auto-discover interfaces → seed uplinks |
| `netgov web [--addr 127.0.0.1:8474]` | serve the dashboard (localhost only) |
| `netgov install` | install the web service, NM re-apply hook, and failover unit |

## Uplinks

```
netgov uplink list
netgov uplink define <name> --dev <iface> [--gw <ip>]
netgov uplink del <name>
```

## Rules & default

```
netgov rule add --domain <d>          --via <uplink|block> [--fam 4|6|both]
netgov rule add --from <CIDR|iface>   --via <uplink|block> [--fam 4|6|both]
netgov rule del (--domain <d> | --from <s>)
netgov rule list

netgov default set --v4 <uplink|block> [--v6 <uplink|block>]
netgov default clear
```

RFC1918 / link-local always stays on the main table.

## Access points (named library)

Saving **defines** an AP; it does not switch on (so you can define an AP on a radio
that's currently a Wi-Fi uplink without dropping it). One AP per radio may be on.

```
netgov ap list
netgov ap save <name> --dev <iface> --ssid <s> --psk <p> [--band bg|a] [--channel N]
netgov ap on  <name>      # bring up (shadows that radio's uplink)
netgov ap off <name>
netgov ap del <name>
```

## Patterns & failover

```
netgov pat-list
netgov pat-set <name> <prio> [--require a,b]
                            [--ssid <S[,S2]> --ssid-iface <uplink>]   # SSID-in-range trigger
                            [--ap <name,…>]                            # APs to swap in
                            [--v4 <uplink|block|direct>] [--v6 …]
                            [--snapshot]                               # capture current egress
                            [--floor]
netgov pat-apply <name>     # switch to a pattern now (root)
netgov pat-del <name>
netgov eval [--apply]       # pick best satisfiable pattern (dry, or apply)
netgov arm [--dry]          # start the root failover loop (boots disarmed)
netgov disarm               # stop it
```

**Selection (when armed):** highest-priority pattern whose `require` uplinks are up
and whose SSID trigger (if any) is in range, then whose default validates internet
(poll up to ~45 s, debounced) — else the always-reachable `floor`. Watch it with
`journalctl -u netgov-roled -f`.

## Same-address arbitration (`claim`)

One address, several adapters, **exactly one holder** — for a host that must keep one
identity whether it is on cable or Wi-Fi (a static DHCP reservation reserved for BOTH
of its MACs). The claim is a property **of a pattern**, because address identity belongs
to a site, not to a box: it is inert unless that pattern is the active one.

```
netgov claim                                  # show the claim on the active pattern + who holds it
netgov claim set <pattern> <address> <dev:prio[:ssid,ssid]>…
netgov claim clear <pattern>
```

```
netgov claim set LH 192.168.222.153 enp114s0:100 wlo1:50:CNNet
```

Highest-priority **eligible** adapter wins. Eligibility is **carrier + association +
gateway-answers-ARP-on-that-interface** (never NetworkManager's connectivity verdict — that
is an input downstream of the arbiter's own output; an ARP exchange is a measurement taken on
the interface itself). A Wi-Fi claimant may list the SSIDs on which it is eligible: an adapter
associated to some *other* network is not a path to that address and is skipped.

**Two separate arm switches — do not confuse them:**

| switch | what it arms | how |
|---|---|---|
| `netgov arm` | the **pattern** failover loop (`netgov-roled.service`) | `arm` / `disarm` |
| `/etc/netgov-claim.armed` | the **address arbiter** | create/remove the file (root) |

`netgov arm` does **not** arm arbitration, and the arbiter file does not start the loop.
Both boot **disarmed**. Until the flag exists the arbiter only ever reports what it *would*
do — `netgov claim` is safe to run at any time.

**Safety rules it obeys:** claim-before-release (it only ever MOVES an address; if no
claimant is eligible the current holder KEEPS it, so the box is never left with none) and
a gratuitous ARP on hand-over, because a failover the segment cannot see is not a failover.

**Eligibility also requires the gateway to answer ARP on that interface** (needs `arping`;
absent, the probe fails open and eligibility is carrier+association only). Carrier is not a
health signal: a cable can negotiate 1000/full and still lose most frames, and an arbiter
trusting carrier would hand the address to it. Note the two different fail directions — a
gateway that does not answer fails **closed** (ineligible), while a missing probe fails
**open** (losing the ability to test a leg is not evidence against it).

**Claiming on a server that wants no egress policy?** Put the claim on the **floor**: it is
always satisfiable, so the arbitration is never inert. Declare the floor explicitly first —
an auto-created floor carries `v6=block`, which is leak-protection for a travelling laptop
and wrong for a server:

```
netgov pat-set floor 0 --floor --v4 direct --v6 direct
netgov claim set floor 192.168.222.186 eno1:100 wlp0s20f3:50:CNNet
```

`direct` normalises to "no default pinned", so declaring this changes no routing. No `init`
is needed — a claim references DEVICES, not uplinks.

> ⚠️ **Gating DHCP is necessary but not sufficient.** A standby that holds no lease still
> answers ARP for the address (Linux `arp_ignore=0`: any interface answers for any local
> address) and still competes to be the reply path. On a host with two NICs on one subnet,
> also set `arp_ignore=1` + `arp_announce=2` with per-interface source routing, or keep the
> standby off the segment. Verify from the SEGMENT that exactly one MAC answers.

## Links

```
netgov link up|down|reapply <iface>
```
