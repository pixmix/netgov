# Operating netgov — a custodian's reference

For whoever has to touch a **live host**. The [UI guide](UI.md) and [CLI reference](CLI.md) tell you
what the features do; this tells you what a command will do **to a box you cannot afford to lose**.

> **On a host you cannot afford to lose, never run a netgov verb to find out what it does.
> Read the table, or ask.**
>
> Since 2.7, `--help` / `-h` / `help` anywhere in the arguments prints and exits without acting, for
> every verb — so interrogating an unfamiliar one is always safe. Before 2.7 it was not:
> `netgov web --help` **started the dashboard**, and `netgov web install` on a host with passwordless
> sudo escalated silently and left root-owned state behind. If you are on an older build, treat every
> verb as guilty until this table says otherwise.

---

## Verb table

**MUTATES** — writes `state.json` (config) or changes the live system (routing, addresses, units).
**ROOT** — needs privilege. On a host with **passwordless sudo, "needs root" and "escalates without
asking you" are the same sentence**, which is how a stray `install` reconfigured a production server.

| verb | mutates | root | what it actually does |
|---|---|---|---|
| `status` (or no args) | — | — | live view: uplinks, rules, defaults, APs, bridges |
| `--version` / `version` | — | — | declared version, build target, source commit |
| `--help` / `-h` / `help` | — | — | prints usage; **never acts** (2.7+) |
| `plan` | — | — | prints the `ip` plan it *would* execute |
| `eval` | — | — | which pattern it *would* pick |
| `pat-list` | — | — | patterns + satisfiability |
| `claim` / `claim status` | — | — | the active pattern's claim + who holds the address |
| `claim eval` | — | — | the arbitration decision it *would* make |
| `uplink list`, `ap list`, `rule list` | — | — | listings |
| `init` | **state** | — | seeds uplinks from detected interfaces. Defines only — applies nothing |
| `uplink define` / `del` | **state** | — | |
| `rule add` / `del` | **state** | — | |
| `default set` / `clear` | **state** | — | stored; takes effect on the next `apply` |
| `pat-set` / `pat-del` | **state** | — | |
| `claim set` / `claim clear` | **state** | — | declares arbitration; does not perform it |
| `ap save` / `del` | **state** | — | defines an AP; does **not** switch it on |
| `apply` / `refresh` | **system** | **yes** | realises the config: installs `ip rule`s and tables |
| `reset` | **system** | **yes** | removes every netgov rule → NM baseline |
| `link up` / `down` / `reapply` | **system** | **yes** | |
| `ap on` / `off` | **system** | **yes** | brings a radio up/down |
| `pat-apply` | **state+system** | **yes** | activates a pattern: routing, APs, **and its claim** |
| `eval --apply` | **state+system** | **yes** | picks a pattern and activates it |
| `arm` / `disarm` | **state+system** | **yes** | enables/disables `netgov-roled.service` — the **pattern loop** |
| `claim arm` / `claim disarm` | **system** | **yes** | creates/removes `/etc/netgov-claim.armed` — the **arbiter** |
| `claim apply` | **system** | **yes** | moves the address now (refuses unless armed) |
| `web` | — | — | serves the dashboard in the foreground. Does not return |
| `web install` / `install` | **system** | **yes** | writes the NM dispatcher hook + systemd units |

**Two verbs whose names under-describe them:**

- **`web`** does not return — it serves until killed. Fine interactively, a hang in a script.
- **`install`** is the heaviest verb here. It writes to `/etc/NetworkManager/dispatcher.d/` and
  installs units. It is also **required after upgrading the binary** (see *Enforcement* below).

---

## What "armed" means — two switches, and one of them is not the one you want

| switch | arms | how | survives reboot |
|---|---|---|---|
| `netgov arm` | the **pattern failover loop** (`netgov-roled.service`) | `arm` / `disarm`, or the dashboard | yes (unit is enabled) |
| `/etc/netgov-claim.armed` | the **address arbiter** | `claim arm` / `claim disarm`, or the dashboard (2.6+) | yes (file on disk) |

**Arming the loop does not arm arbitration.** They are independent, and the operator has already
been caught by this once: he armed the loop and reasonably believed the claim was live.

### Enforcement — what actually makes an armed arbiter act

| build | arbitration runs when | consequence |
|---|---|---|
| ≤ 2.6 | **only when a pattern is activated** | on a box with one always-satisfiable pattern, **never** — armed but not enforcing |
| 2.7+ | **on link up/down**, via the NM dispatcher hook | the carrier event, which is the one that changes eligibility |

Pattern selection and claimant eligibility change on **different events**. A cable dying does not
change which pattern is satisfiable, so pre-2.7 the arbiter was never consulted on the one event it
exists for.

> ⚠️ **Upgrading the binary is not enough.** The dispatcher hook on disk dates from its last
> install, so a host that takes 2.7+ **without re-running `netgov install`** stays *armed but
> unenforcing* — worse than before, because now everyone believes it is fixed.
>
> ```
> grep -c claim /etc/NetworkManager/dispatcher.d/90-netgov     # 0 = old hook
> ```

### The gateway probe

Eligibility measures the **loss fraction** to the gateway out of each claimant's own interface:
10 ARP probes, **rejected above 10% loss**, reported as the fraction —
`enp114s0: 10/10 replies to 192.168.222.1, 0% loss`.

Needs **`arping`** (`iputils-arping`). Without it the check **fails open** and eligibility falls
back to carrier + association.

> ⚠️ **Do not "harden" this by raising the probe count under an any-reply rule.** Until 2.9 the test
> was `arping -c 2` and passed if *any* reply arrived — a `1 − loss^N` chance of accepting a bad leg,
> so **more probes made a wrong pass MORE likely**: N=2 → 42%, N=5 → 75%, N=10 → 94% on a 76%-loss
> leg. The rule is what matters, not the sample size.
>
> **This fault does not look slow, it looks perfect intermittently.** Measured survivors on a leg
> losing 76% of frames ran 0.31–0.83 ms — better than many healthy links. Low latency on the
> survivors is characteristic of the fault, not evidence against it, which is why only a ratio
> separates them.

Cost: ~10 s of wall time (arping's `-i` takes whole seconds), claimants probed **concurrently** so
it is the slowest leg, not the sum. That is the price of not moving an address onto a dead cable.

---

## Files, ownership, escalation

| path | owner | written by |
|---|---|---|
| `~/.config/netgov/state.json` | **the user** | any state-mutating verb |
| `/etc/netgov-claim.armed` | root | `claim arm` / `claim disarm` |
| `/etc/NetworkManager/dispatcher.d/90-netgov` | root | `install` |
| `/etc/systemd/system/netgov-roled.service` | root | `install` |
| `/var/log/netgov-dispatch.log` | root | the hook |

> ⚠️ **The trap: `state.json` is user-owned but root-writable.** Run a state-mutating verb as root
> (directly, or via passwordless sudo) and the file becomes **root-owned**, after which the user's
> next `pat-set` fails with permission denied. The roled loop uses `saveStateKeepOwner` for exactly
> this reason; the plain CLI path does not yet. **Prefer running netgov as the owning user** and let
> it escalate for the verbs that need it. After any root invocation:
>
> ```
> ls -l ~/.config/netgov/state.json
> ```

Privileged verbs re-exec through `sudo -A` with `SUDO_ASKPASS`, so on a desktop you get a dialog
naming the command. **On a headless box with passwordless sudo there is no dialog and no prompt** —
the escalation is silent. That is the environment where this table matters most.

---

## Worked example: a pure-arbitration host

The minimal correct deployment — a **server** that wants one stable address across two NICs and **no
egress policy at all**. No uplinks, no rules, no `init`.

```sh
# 1. Declare the floor EXPLICITLY. Do not let it be auto-created: the default floor carries
#    v6=block, which is IPv6 leak-protection for a travelling laptop and wrong for a server.
netgov pat-set floor 0 --floor --v4 direct --v6 direct

# 2. Declare the claim. `direct` normalises to "no default pinned", so this changes NO routing.
netgov claim set floor 192.168.222.186 eno1:100 wlp0s20f3:50:CNNet

# 3. Verify before arming — read-only, safe.
netgov claim

# 4. Arm the ARBITER (not `netgov arm`, which is the pattern loop).
netgov claim arm

# 5. Make the arm flag mean something on this host.
netgov install
```

**Why the floor and not a new pattern:** the floor is always satisfiable, so it is always the active
pattern on a box with no others — and a claim is **inert unless its pattern is active**. Putting the
claim on a conditional pattern (e.g. one triggered by an SSID) means arbitration stops exactly when
that condition fails, which is usually the moment you need it.

**Why SSIDs go on the claimant, not the pattern:** `wlp0s20f3:50:CNNet` says *this adapter counts
only while associated to CNNet*. That keeps the arbitration itself unconditional. Never list an
upstream/WAN SSID — an adapter on a different network is not a path to that address, and the arbiter
would move the address somewhere it cannot be reached.

Verify routing really did not change:

```sh
ip route; ip -6 route; ip rule        # identical before and after steps 1–2
```

### Gating DHCP is necessary but not sufficient

A standby holding no lease still answers ARP for the address (Linux `arp_ignore=0`: any interface
answers for any local address) and still competes to be the reply path. On a host with two NICs on
one subnet, also set `arp_ignore=1` + `arp_announce=2` with per-interface source routing, or keep
the standby off the segment — then verify **from the segment** that exactly one MAC answers.

---

## Running two dashboards at once

`.153` serves its own on `127.0.0.1:8474`. A peer's is reached by SSH-forwarding to a **different
local port**:

```sh
ssh -N -L 8475:127.0.0.1:8474 ms-rosy
```

> ⚠️ **The pages look identical.** Nothing on either says which host it is. Browser form state can
> carry between tabs, and a claim belonging to one box has been observed sitting in the other's
> editor, one *save* from being written to the wrong host. Keep them in **separate windows**, check
> the port in the address bar before saving, and prefer the CLI over the dashboard when you have
> both open.

The header badge shows the **build** (`netgov/2.7`), not the host — useful for confirming a deploy
landed, useless for telling the two apart.

---

## If something looks wrong

| symptom | first thing to check |
|---|---|
| dashboard shows stale/absent data | hard-refresh. Pre-`b4977ef` builds served the page with no cache validators |
| `pat-set` → permission denied | `ls -l ~/.config/netgov/state.json` — root-owned from a root run |
| claim declared but nothing happens | is its pattern **active**? `netgov claim` says so explicitly |
| armed but no failover on a carrier change | old dispatcher hook — re-run `netgov install` |
| a claimant never becomes eligible | is `arping` installed? is the Wi-Fi claimant on a listed SSID? |
| IPv6 broke after an apply | `default_v6: block`, often from an auto-created floor. `netgov default clear` |
