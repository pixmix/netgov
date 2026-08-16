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

### Reading `apply`'s output (2.29)

`netgov: applied (14 cmds, 0 errors)` is the summary, but **the count is not the check** —
`apply` also verifies the *effect* and prints this when it fails:

```
⚠ uplink cable (v4): table 100 has NO default, but rule `from 192.168.222.153 table 100`
  is installed — traffic from 192.168.222.153 falls through to main and still works, so
  nothing looks broken, while every pin routed through this uplink is NOT in effect.
```

That is the state to know about, because it is invisible from the outside: `apply` flushes each
uplink's table before rebuilding it and installs the source rule regardless, so a rebuild that
fails leaves a live rule pointing at an empty table. Connectivity is unaffected — traffic falls
through to `main` — and only the *policy* is silently gone. Re-run `netgov apply` once the link
has settled.

`ip route get <dst> from <src>` is the independent check: it should name the uplink's table.

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

`manage-metrics on` derives `ipv4.route-metric` from **claim priority and the
arbiter's live verdict**, so the adapter your claim says is preferred *and that
currently works* is the one the host speaks from. Without it, an arbiter can hold an
address on one adapter while every packet leaves on another — true, and not what the
verdict is read as.

**Since 2.33 the verdict outranks the priority.** A claimant the arbiter judges
ineligible on **2 consecutive evaluations** is ranked below every eligible leg,
whatever its declared priority — otherwise a rejected leg keeps the preferred metric
and the box advertises an address it cannot source from (measured on ms-rosy,
2026-08-16: `.186` held by a healthy adapter while traffic left via one losing
70–100 % of frames). One good verdict restores the original ranking immediately:
slow to punish, instant to forgive.

The demotion is **runtime state on `/run`** (cleared by a reboot, expires after 15
minutes) and is deliberately *not* a saved baseline — absent, stale or unreadable all
mean "no demotion", i.e. the pre-2.33 behaviour. The metric is recomputed from the
current verdict rather than restored from a record, so there is nothing to go stale.

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

### IPv6 source rules cover the PREFIX, not one address (2.32)

Each uplink gets a source-return rule so replies leave the way they came. On **v4** that pins the
single address — there, the address is the host's identity. On **v6** it pins every global
**prefix** on the device, because privacy extensions (RFC 4941) put a stable address and a rotating
temporary address on the same `/64`, and RFC 6724 tells applications to prefer the *temporary* one.

A rule pinned to one v6 address matches whichever the tool saw first and silently misses the other;
traffic matching no source rule falls into the `v6=block` blackhole at priority 29000. Before 2.32
that showed up as a dashboard badge reading **"no internet" on a working IPv6 uplink** — and the
same miss applied to real traffic, not just the probe.

`netgov status` reports the **stable** v6 address rather than the temporary one, so the value does
not change under you every few hours.

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
| `⚠ N DISTINCT HOSTS answer for <gw>` | a real address conflict on the gateway — every reading through it is unreliable until resolved |

**Duplicate replies are not a duplicate responder (2.31).** If the probe sees more replies than it
sent, netgov reads the MAC of each one and tells you which case it is:

- **`+N duplicate DELIVERY from <mac>`** — one host, frames arriving twice. On Wi-Fi this is an
  ordinary retransmission after a lost ACK, so it tracks link quality and channel contention
  (`iw dev <ap> station dump` → `tx failed`), not addressing. Nothing to fix on the router.
- **`⚠ N DISTINCT HOSTS answer`** — two machines really do claim the gateway address. This is the
  condition the arbiter exists for, and it is checked independently of the counts: two responders
  while sent and received happen to balance is still a conflict.
- **`responder MACs unreadable`** — the question is left open rather than answered with the
  scarier of the two.

A correctly-configured identity-MAC host prints **no `⚠` at all**.

### Recovering a failover by hand (2.30)

> ⚠️ **Stop the timer first.** `netgov-claim-watch.timer` re-applies the claim every 60 s, so a
> manual `nmcli` fix to a `cloned-mac-address` netgov manages **is reverted within the second**:
> ```
> systemctl stop netgov-claim-watch.timer     # then recover; start it again after
> ```
> Since 2.30 netgov logs `NOTE: overriding …` whenever it overwrites an external change, naming
> this command — but the safest order is still to stop it before you touch anything.

**What a healthy identity move looks like**, and the two lines that are *not* failures:

| line | meaning |
|---|---|
| `staged: … (no carrier on X)` | the MAC is written and applies when the link returns — the ordinary cable-pull case |
| `applied (activation incomplete)` | the MAC landed; `connection up` timed out because that MAC has **no DHCP reservation**, which is expected for a parked leg |
| `COOLDOWN OVERRIDDEN` | a previous attempt failed and left the address held by nobody, so the backoff is being ignored — correct, and the only time it happens |
| `ABORT: … is NOT on the intended MAC` | a real stop: the MAC could not be verified, so continuing risks two adapters on one MAC |

**A parked leg holds a MAC the router has no reservation for**, so it takes a **pool address**, not
its usual one. Two consequences, both measured on the 2026-08-15 ms-rosy failover:

- The parked leg asks for its old address, is offered a pool address instead, and accepts —
  `DHCPDISCOVER .186 / DHCPOFFER .247 / DHCPACK .247`. That is the mechanism working: the
  reservation stays bound to the identity MAC alone.
- **Activation can outlast `nmcli connection up`.** DHCP completed **107 seconds** after
  netgov's `connection up` had already returned exit 4. Since 2.30 that is handled by measuring the
  MAC rather than trusting the exit code — it is why `applied (activation incomplete)` exists.
- Each park leaves a **pool lease under a MAC no interface wears** for the lease duration (12 h
  here). Harmless with a large pool; worth knowing if yours is small.

⚠️ **The genuine precondition is that the server has a usable pool at all.** With no pool a parked
leg gets no address, reads `UNCONFIGURED, not lossy`, and cannot win the claim back. Verify this
before arming identity-MAC on a box you cannot reach — but note it is a *precondition*, not a
common failure: on a `/24` with a 150-address pool it will not be your problem.

## Links

```
netgov link up|down|reapply <iface>
```
