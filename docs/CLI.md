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

## Links

```
netgov link up|down|reapply <iface>
```
