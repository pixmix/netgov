# netgov

**Host-level multi-homing / policy-routing switchboard for Linux.** A small,
dependency-free Go tool that steers traffic **per-destination-domain** and
**per-source** (containers, VMs, subnets, interfaces) to a chosen **uplink** —
independent of any router. It works whether you're on Ethernet, Wi-Fi, a USB
tether, or several at once, and it's safe by construction: it only *adds* policy
rules in a reserved band and tears them all down cleanly on `reset`.

Useful when a machine has more than one way to reach the internet and you want
explicit control over which traffic uses which path — e.g. keep one critical
service on a stable link while everything else goes out a different uplink, pin a
container's egress to a specific WAN, or blackhole a whole address family to
prevent leaks.

> Comes with a CLI and a localhost-only web dashboard that mirror each other.

---

## Model

- **Uplink** — a named egress path bound to a network device. Each uplink gets a
  private routing table and a source-based rule so traffic from its address uses
  its own gateway. The underlying connection (addresses, Wi-Fi credentials, etc.)
  is configured in **NetworkManager / your OS settings**; netgov only *references*
  the device.
- **Rule** — steers traffic to an uplink (or `block`s it). Exactly one of:
  - **destination** — a domain (resolved to current IPs), or
  - **source** — a CIDR or an interface name (`iif`), e.g. a Docker/libvirt bridge.
- **Overall default** — per address family (IPv4 and IPv6 are independent): an
  uplink name, `block` (blackhole), or *direct* (use the main table / OS default).
- **Access Point** — optionally turn a Wi-Fi interface into a shared AP
  (NetworkManager `ipv4.method shared`: DHCP + NAT); clients egress via the host
  default and inherit your rules.
- **Lifeline pattern** — a destination pinned to a stable uplink so it stays on a
  known-good path regardless of the overall default (just an ordinary rule, e.g.
  `netgov rule add --domain example.com --via wifi`).

RFC1918 / link-local traffic always stays on the main table (local LANs keep
working). IPv6 defaults to `block` when the host has no global IPv6 (leak-protect).

### Safe by design

netgov only writes `ip rule`s in the priority band **8000–29999** and routes in
tables **100–199**. It never edits the main table or your NetworkManager profiles.
`netgov reset` flushes exactly that owned band and the OS networking baseline
reappears — there is no persistent change to undo.

---

## Requirements

- Linux with `iproute2` (`ip`) and, for the AP feature and link toggles,
  **NetworkManager** (`nmcli`).
- Go (to build) — standard library only, no third-party modules.
- Root for `apply`/`reset` (it edits routing). The CLI re-execs itself via
  `sudo`; in a desktop session it uses an askpass helper. Set `SUDO_ASKPASS` to
  your own helper, or run the privileged verbs as root.

## Build & install

```sh
go build -o netgov .
install -m755 netgov ~/bin/netgov        # or /usr/local/bin

netgov init        # auto-discover devices -> seed uplinks
netgov install     # optional: localhost web service + NetworkManager re-apply hook
```

## CLI

```
netgov status                      # live view: uplinks, rules, default, APs
netgov plan                        # dry-run: print the ip-rule/route plan
netgov apply | refresh             # apply the plan (root; idempotent)
netgov reset                       # remove everything -> pure NetworkManager
netgov init                        # discover devices, seed uplinks

netgov uplink define <name> --dev <iface> [--gw <ip>]
netgov uplink del <name>

netgov rule add --domain <d> --via <uplink|block> [--fam 4|6|both]
netgov rule add --from <CIDR|iface> --via <uplink|block> [--fam 4|6|both]
netgov rule del (--domain <d> | --from <s>)

netgov default set --v4 <uplink|block> [--v6 <uplink|block>]
netgov default clear

netgov ap on <iface> --ssid <S> --psk <P> [--band bg|a] [--channel <n>]
netgov ap off <iface>
netgov ap list

netgov link up|down|reapply <iface>
netgov web                         # serve the dashboard (localhost)
```

### Example

```sh
# Two uplinks: built-in Wi-Fi and a wired link
netgov uplink define wifi  --dev wlp2s0
netgov uplink define cable --dev enp3s0

# Everything out the cable by default, but keep one service on Wi-Fi
netgov default set --v4 cable --v6 block
netgov rule add --domain example.com --via wifi

# Pin a Docker bridge's containers to the cable
netgov rule add --from 172.18.0.0/16 --via cable

netgov apply
```

## Web dashboard

`netgov web` (or the `netgov install` service) serves a localhost-only dashboard
that mirrors the CLI: cards for uplinks, destination/source rules, the per-family
default, and access points, with live status. Bind address is configurable; keep
it on loopback unless you intend otherwise.

## State

Configuration is a single JSON file at `~/.config/netgov/state.json`
(override with `NETGOV_STATE` or `--state`). It contains only your topology
(uplink names/devices, rules, defaults, AP SSIDs); apply derives all `ip` commands
from it.

## License

MIT — see [LICENSE](LICENSE).
