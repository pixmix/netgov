# Contributing to netgov

## Examples must be documentation values, never a real network

netgov is developed against a real LAN, and a routing tool's tests are made of addresses.
That combination makes it very easy to paste a live `ip -j addr` or `nmcli` capture into a
fixture and publish somebody's network with it. So the repository has one rule:

> **The tool is public. The network it was written on is not.**
> Where a fixture needs an address, a MAC, a hostname or an SSID, it uses a *documentation*
> value. If a real capture is the clearest way to describe a case, describe how to take the
> capture — do not commit the capture.

### The values to use

| Thing | Use | Notes |
|---|---|---|
| IPv4 LAN | `10.0.0.0/24` — gateway `10.0.0.1`, hosts `10.0.0.10`, `.11`, `.20`, `.21` | `.120+` for pool leases |
| IPv4 container subnets | `172.18.0.0/16`, `172.20.0.0/16` | source-rule examples |
| IPv6 global | `2001:db8::/32` (RFC 3849) — the `/64` in the tests is `2001:db8:0:1::/64` | |
| IPv6 ULA | `fd00:db8::/32` | |
| MAC | `00:00:5E:00:53:xx` (RFC 7042) | and `02:00:5E:00:53:xx` for its locally-administered twin, which the parked-MAC examples need |
| Hostnames | `host-a`, `host-b` | |
| SSIDs | `HomeAP`, `UplinkAP`, `VenueWiFi` | |

Public anycast addresses the tool actually pings (`1.1.1.1`, `2606:4700:4700::1111`) are a
well-known service rather than anyone's machine, and belong in the code as themselves.

### The check

`.githooks/pre-commit` (`netgov-publish-gate/1.0`) refuses a commit that carries anything of
those shapes outside the allowlist at the top of the file. Enable it once per clone:

```sh
git config core.hooksPath .githooks
```

It **fails closed**: if it cannot complete, it refuses. A guard that silently does not run is
worse than no guard, because it manufactures confidence.

Adding a genuinely new example means adding it to the allowlist — one visible line, in the
file that explains why the list exists. That is deliberate: the exception should be something
a reviewer sees, not something a pattern quietly permits.

### Why the IPv6 axis is not an afterthought

The rule above was written after a review that counted private addresses and reported the
repository "a map of a network nobody outside can reach". That was true of the IPv4 fixtures
and false of the IPv6 ones: the tests carried a real, globally routable `/64` from a
residential ISP, together with the host's interface identifiers. It had been invisible for
weeks because every check searched for *private* ranges, and a global address is not one.

A check whose scope excludes the failure reads exactly like a passing one. The gate looks at
IPv4, IPv6, MACs and names separately for that reason.
