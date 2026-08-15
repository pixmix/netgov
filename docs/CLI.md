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

### Who decides the path (2.21+)

Two NetworkManager profile properties decide which adapter actually carries
traffic. netgov can own them; **both are opt-in, and `reset` restores whatever
they were before netgov first took them over.**

```
netgov uplink default-route <name> yes|no|auto
netgov uplink manage-metrics on|off
```

`default-route` maps to `ipv4.never-default` — whether this uplink may carry the
default route **at all**. A `never-default` profile silently overrules
`netgov default set --v4 <that uplink>`, which is why the property is here rather
than left invisible.

| value | meaning |
|---|---|
| `yes` | may carry the default route (`ipv4.never-default no`) |
| `no` | may **not** — e.g. a cable to a router you do not want capturing your egress |
| `auto` | **netgov does not hold the property**; NetworkManager keeps whatever it has. The default. |

`manage-metrics on` derives `ipv4.route-metric` from **claim priority**, so the
adapter your claim says is preferred is also the one the host speaks from. Without
it, an arbiter can hold an address on one adapter while every packet leaves on
another — true, and not what the verdict is read as.

**Nothing is written until `netgov apply`.** Run `netgov plan` first: the NM half is
printed separately, with the reason beside each change. `netgov claim status` reports
whether **netgov or NetworkManager** currently governs the path — the version tells
you the capability is installed, not that it governs anything.

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
identity whether it is on cable or Wi-Fi. The claim is a property **of a pattern**, because
address identity belongs to a site, not to a box: it is inert unless that pattern is the
active one.

```
netgov claim                                  # show the claim on the active pattern + who holds it
netgov claim set <pattern> <address> [identity=<mac>] <dev:prio[:ssid,ssid]>…
netgov claim clear <pattern>
```

### Two mechanisms — declare `identity=` and you get the better one (2.27+)

|  | **identity-MAC** (`identity=…`) | **lease arbitration** (no `identity=`) |
|---|---|---|
| router side | one reservation **per adapter**, each to its own MAC | **one** reservation listing **all** the host's MACs |
| failover moves | the **MAC** (NM `cloned-mac-address`) | the **lease** (release / renew) |
| standby holds | its **own** address, always | nothing, or a pool consolation address |
| adapter ever downed | **no** | sometimes |

```
netgov claim set LH 192.168.222.153 identity=48:21:0b:6e:06:85 enp114s0:100 wlo1:50:CNNet
```

Prefer identity-MAC. `dnsmasq` documents that a multi-MAC reservation "will only work
reliably if only one of the hardware addresses is active at any time and there is no way
for dnsmasq to enforce this" — but a *host* enforces "only one of my NICs wears this MAC"
trivially. Moving the identity also removes two faults the lease mechanism cannot avoid:
the standby is never offered an address another adapter already has (so its own RFC 5227
conflict detection cannot see a sibling answer and `DHCPDECLINE` the reservation into a
10-minute bench), and no adapter is ever taken down.

A loser whose *permanent* MAC is the identity **parks** on the same address with the
locally-administered bit set (`48:21:… → 4a:21:…`), so no two adapters ever wear one MAC.
If the permanent MAC cannot be read (`ethtool -P`), netgov **refuses to plan** rather than
guess — a swap it cannot undo is worse than no swap.

Lease arbitration remains for claims with no `identity=`, and its rules below still apply
to those.

Highest-priority **eligible** adapter wins. Eligibility is **carrier + association +
gateway-answers-ARP-on-that-interface** (never NetworkManager's connectivity verdict — that
is an input downstream of the arbiter's own output; an ARP exchange is a measurement taken on
the interface itself). A Wi-Fi claimant may list the SSIDs on which it is eligible: an adapter
associated to some *other* network is not a path to that address and is skipped.

**Two separate arm switches — do not confuse them:**

| switch | what it arms | how |
|---|---|---|
| `netgov arm` | the **pattern** failover loop (`netgov-roled.service`) | `arm` / `disarm` |
| `/etc/netgov-claim.armed` | the **address arbiter** | `netgov claim arm` / `claim disarm` |

`netgov arm` does **not** arm arbitration, and the arbiter file does not start the loop.
Both boot **disarmed**. Until the flag exists the arbiter only ever reports what it *would*
do — `netgov claim` is safe to run at any time.

**What enforces it (2.7+):** the NetworkManager dispatcher hook runs `claim apply` on link
**up/down** — the carrier event is the one that changes claimant eligibility. Before 2.7
arbitration ran only when a *pattern* was activated, which is the wrong trigger: a cable dying
does not change which pattern is satisfiable, so an armed arbiter was never consulted on the
event it exists for.

> ⚠️ **Upgrading the binary is not enough — re-run `netgov install`** on each host. The hook on
> disk dates from whenever it was last installed, so a host with an older hook stays armed but
> unenforcing. Check: `grep -c claim /etc/NetworkManager/dispatcher.d/90-netgov`.

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

> ⚠️ **Lease arbitration only — gating DHCP is necessary but not sufficient.** A standby
> that holds no lease still answers ARP for the address (Linux `arp_ignore=0`: any interface
> answers for any local address) and still competes to be the reply path. On a host with two
> NICs on one subnet, also set `arp_ignore=1` + `arp_announce=2` with per-interface source
> routing, or keep the standby off the segment. Verify from the SEGMENT that exactly one MAC
> answers. **Under identity-MAC this does not arise**: the standby is on its own address and
> its own MAC, so there is no second answer for the guarded address to begin with.

### What `netgov claim` warns about (2.28)

Under **lease arbitration**, a standby holding any address on the guarded subnet is itself
the finding — it is on the segment.

Under **identity-MAC** that is the design, and saying so on every healthy box would be a
warning nobody reads. So the report is silent on it and speaks on the states that break the
mechanism instead:

| line | meaning |
|---|---|
| `⚠ SPLIT-BRAIN` | two of this host's legs carry the guarded address at once (a fault under **both** mechanisms) |
| `⚠ MAC COLLISION` | two adapters wear one MAC — exactly what parked MACs exist to prevent |
| `⚠ identity MAC … worn by NO claimant` | the router reserves the address to a MAC not present here, so no leg can take it |
| `⚠ … neither its permanent MAC nor its parked MAC` | a foreign clone or a half-applied swap; failover from it is not predictable |
| `⚠ HOLDS is not PATH` | the holder is correct and the host talks on the *other* adapter — chosen by route metric, not by the arbiter |

A correctly-configured identity-MAC host prints **no `⚠` at all**.

## Links

```
netgov link up|down|reapply <iface>
```
