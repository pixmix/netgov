// claim.go — same-address arbitration.
//
// PROBLEM. Two adapters on one host can both be reserved to the same LAN address (a dual-MAC
// DHCP reservation is dnsmasq's documented way of giving a box ONE stable identity across
// adapters). dnsmasq(8) states the precondition and that it cannot enforce it: "only one of the
// hardware addresses is active at any time and there is no way for dnsmasq to enforce this."
// This file is that missing arbiter, host-side.
//
// WHY IT LIVES IN A PATTERN. A claim group is declared ON a pattern and is arbitrated only while
// that pattern is the active one. Address identity is a property of a SITE, not of the box: the
// same laptop wants one-address-two-adapters at the home lab and nothing of the sort on a venue
// network. Putting the claim in the pattern means it travels with the site profile and simply
// stops applying when you are somewhere else.
//
// THE FOUR INVARIANTS (from the 2026-08-10 .186 split-brain review; each is load-bearing):
//
//  1. ARBITRATE THE LEASE, NOT THE ROUTE. In the incident BOTH adapters asked for .186 and both
//     were GRANTED it; the routing table was merely where the damage showed. A route-level
//     arbiter would have hidden a live address collision behind a metric. So a standby must
//     neither hold nor request the lease.
//  2. ELIGIBILITY = CARRIER + ASSOCIATION ONLY. Never NetworkManager's connectivity verdict.
//     The incident's flap was NM penalising a device it judged to have no connectivity (+20000),
//     which changed which path was broken, which changed the verdict. An arbiter keyed on that
//     verdict puts its input downstream of its own output and oscillates.
//  3. CLAIM BEFORE RELEASE. If no claimant is eligible, the current holder KEEPS the address —
//     the arbiter only ever MOVES an address, never merely removes one. A de-addressing bug on a
//     remote box with no console is unrecoverable without a physical visit.
//  4. GRATUITOUS ARP ON CLAIM. The host moving its own address proves nothing about what the
//     segment believes; peers cache the old MAC. Verify from a THIRD box.
//
// Boots disarmed. Dry-run prints the plan and touches nothing.
package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Claimant is one adapter's bid for the claim address.
type Claimant struct {
	Dev      string   `json:"dev"`             // adapter, e.g. "enp114s0"
	SSIDs    []string `json:"ssids,omitempty"` // Wi-Fi only: eligible ONLY when associated to one of these
	Priority int      `json:"priority"`        // higher wins
}

// Claim is a claim group: one address, N claimants, exactly one holder.
type Claim struct {
	Address   string     `json:"address"`
	Claimants []Claimant `json:"claimants"`

	// IdentityMAC turns the whole mechanism inside out, and it is the operator's design
	// (2026-08-14). Set it and the address is no longer arbitrated by DHCP at all: the router
	// reserves the address to ONE MAC, each adapter keeps its OWN reservation, and failover moves
	// the *MAC* to whichever adapter should currently be the identity. See claimMACPlan.
	IdentityMAC string `json:"identity_mac,omitempty"`
}

// claimArmedFlag mirrors the pw-failover convention: presence of the file = armed, and removing
// it disarms instantly with no restart and no daemon involvement.
const claimArmedFlag = "/etc/netgov-claim.armed"

func claimArmed() bool { _, err := os.Stat(claimArmedFlag); return err == nil }

// statePathFor resolves the state file the same way main() does, honouring --state so `claim set`
// can be exercised against a copy rather than live state.
func statePathFor(args []string) string {
	if v, ok := flagVal(args, "--state"); ok {
		return v
	}
	return statePath()
}

// stripFlags removes "--flag value" pairs from a positional argument list. Without it `--state
// <path>` is parsed as a claimant spec and the command fails with a baffling message about the
// flag name — which is exactly what it did the first time it was run.
func stripFlags(args []string, flags ...string) []string {
	out := []string{}
	for i := 0; i < len(args); i++ {
		isFlag := false
		for _, f := range flags {
			if args[i] == f {
				isFlag = true
				i++ // also skip its value
				break
			}
		}
		if !isFlag {
			out = append(out, args[i])
		}
	}
	return out
}

// devCarrier reports physical link, read straight from the kernel. Deliberately NOT nmcli:
// invariant 2 forbids consulting anything that carries NetworkManager's own judgement.
func devCarrier(dev string) bool {
	b, err := os.ReadFile("/sys/class/net/" + dev + "/carrier")
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

// devIsWireless reports whether dev is a radio. Without this the nmcli fallback below happily
// returns a WIRED device's connection PROFILE NAME (e.g. "eth-lan") and it gets compared against
// the SSID list — a profile name standing in for an association, which is the same proxy-for-the-
// thing error this arbiter exists to avoid, and here it would drive an address move.
func devIsWireless(dev string) bool {
	if _, err := os.Stat("/sys/class/net/" + dev + "/wireless"); err == nil {
		return true
	}
	_, err := os.Stat("/sys/class/net/" + dev + "/phy80211")
	return err == nil
}

// devSSID returns the SSID dev is ASSOCIATED to, preferring `iw` (the association itself) and
// falling back to nmcli's view. Association is a radio fact; reachability is not consulted.
func devSSID(dev string) string {
	if out, err := run("iw", "dev", dev, "link"); err == nil {
		for _, ln := range strings.Split(out, "\n") {
			ln = strings.TrimSpace(ln)
			if strings.HasPrefix(ln, "SSID:") {
				return strings.TrimSpace(strings.TrimPrefix(ln, "SSID:"))
			}
		}
	}
	out, err := run("nmcli", "-t", "-f", "GENERAL.CONNECTION", "device", "show", dev)
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(out, "\n") {
		if p := strings.SplitN(ln, ":", 2); len(p) == 2 && strings.TrimSpace(p[1]) != "--" {
			return strings.TrimSpace(p[1])
		}
	}
	return ""
}

// Probe parameters for the gateway reachability test. See devGatewayAnswers.
//
// N=10 with a 10% ceiling, NOT the old "-c 2, any reply". arping exits 0 if ANY reply arrives, so
// the old test passed on 1-of-2 — a 1-loss^2 = 42% chance of certifying c-016's measured 76%-loss
// leg, and under an any-reply rule MORE PROBES MAKE A WRONG PASS MORE LIKELY (N=5 -> 75%,
// N=10 -> 94%). Anyone "hardening" it by raising -c would have weakened it. The rule had to change,
// not the sample size.
const (
	claimProbeCount    = 10 // arping -i takes whole seconds on iputils, so this is ~10s
	claimProbeDeadline = 14 // must exceed count, or -w truncates and healthy legs read as lossy
	claimMaxLossPct    = 10

	// After a FAILED attempt, suppress further attempts for this long. Without it the arbiter
	// loops: every apply cycles an interface, which fires the NM dispatcher, which applies again.
	// The mutex in the hook prevents CONCURRENT runs; nothing prevented SEQUENTIAL re-triggering,
	// and convergence — which I claimed made repeat runs harmless — only converges when the winner
	// CAN acquire. On 2026-08-14 it ran at 00:42:26, 00:43:26, 00:43:56 and was still going.
	claimFailCooldown = 300 // seconds

	// How long to wait for the winner to acquire the address before rolling back. DHCP with a
	// discover/offer/request/ack round plus ACD is seconds, not milliseconds; the old code waited
	// zero and reported the resulting miss as a warning.
	claimAcquireTimeout = 20
)

// arpingStats parses arping's OWN REPORT rather than its exit code.
//
// The exit code is a proxy and it lies in both directions: it is 0 if any single reply arrives
// (hiding 76% loss), and this very session observed `arping -q ... -i 0.1` exit 0 while printing
// "invalid argument" and probing nothing at all. The counts are the measurement; rc is an opinion.
func arpingStats(out string) (sent, recv int, ok bool) {
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Fields(ln)
		if len(f) >= 2 && f[0] == "Sent" {
			sent, _ = strconv.Atoi(f[1])
		}
		if len(f) >= 2 && f[0] == "Received" {
			recv, _ = strconv.Atoi(f[1])
		}
	}
	return sent, recv, sent > 0
}

// defaultGateway returns the LAN gateway from the MAIN routing table. Used only as the ARP
// probe target for a claimant with no lease of its own; it is never used to make a routing
// decision, so reading the main table here does not make eligibility depend on the arbiter's
// own output the way NetworkManager's connectivity verdict would.
func defaultGateway() string {
	out, err := run("ip", "-4", "route", "show", "default")
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Fields(ln)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "via" {
				if ip := net.ParseIP(f[i+1]); ip != nil {
					return f[i+1]
				}
			}
		}
	}
	return ""
}

// devGatewayAnswers measures the LOSS FRACTION to the LAN gateway out of this specific interface.
//
// WHY THIS EXISTS: CARRIER IS NOT A HEALTH SIGNAL. 2026-08-13: a Raspberry Pi fell off its support
// and hung by its own RJ45 connectors. The mechanical load lost 80-95% of frames while the link
// still negotiated 1000/full — carrier 1, error counters 0 at BOTH ends (a frame lost on the wire
// never arrives to be counted, and a tx counter counts what was SENT, not what LANDED). An arbiter
// trusting carrier finds that leg eligible, outranks a working wireless leg, and MOVES THE ADDRESS
// ONTO THE BROKEN CABLE — failing at its one job while looking correct.
//
// WHY A FRACTION AND NOT A YES/NO: this fault does not look slow, it looks PERFECT, INTERMITTENTLY.
// Measured survivors on the bad leg ran 0.31-0.83 ms — better than many healthy links. Low latency
// on the survivors is characteristic of the fault, not evidence against it, so any test that asks
// "did something come back?" reads it as textbook health. Only the ratio distinguishes them.
//
// Why this is not the circularity invariant 3 forbids: it is a MEASUREMENT taken on the interface
// itself, not a verdict computed downstream of this arbiter's own output. NetworkManager's
// connectivity flag reflects the default route, which the arbiter moves, so consulting it means
// reading your own output as your input. An ARP exchange needs no address on the interface and
// ignores the routing table, so a lease-less STANDBY is testable — which it must be, since you
// cannot fail over to a leg whose health you were never able to measure.
//
// Fail-OPEN only when the probe could not RUN (no arping, no known gateway, unparseable output).
// A measured bad result fails CLOSED. Those are different things: losing the ability to test a leg
// is not evidence against it (invariant 4), but measuring it and finding it broken is.
func devGatewayAnswers(dev string) (bool, string) {
	// Prefer the gateway this interface was itself offered. A STANDBY claimant may hold no lease
	// (that is the point of gating it), so fall back to the LAN default gateway — every claimant
	// for one address is on one subnet by definition, so it is the correct target for all of them.
	gw := dhcpRouter(dev)
	if gw == "" {
		gw = defaultGateway()
	}
	if gw == "" {
		return true, "" // nothing to probe against — do not penalise
	}
	if _, err := exec.LookPath("arping"); err != nil {
		return true, "" // tool absent: cannot test, fail open
	}
	out, _ := run("arping", "-I", dev, "-c", strconv.Itoa(claimProbeCount),
		"-w", strconv.Itoa(claimProbeDeadline), gw) // rc deliberately ignored — see arpingStats
	sent, recv, ok := arpingStats(out)
	if !ok {
		return true, "gateway probe unreadable — NOT verified"
	}
	// DUPLICATE REPLIES: recv can EXCEED sent. Observed live — "10/9 replies, -11% loss" — because
	// arping counts responses, and more than one host answering inflates the count (a deadline can
	// also truncate the sent count after a reply is already in flight).
	//
	// Left unhandled this is dangerous in exactly the wrong direction: negative loss sails under
	// any ceiling, so a leg with duplicate responders reports PERFECT health. And duplicate ARP
	// replies for one address are the split-brain signature this arbiter exists to arbitrate — the
	// pathological case would have been the one that looked cleanest.
	//
	//	"A detector reporting its own target condition as ideal health is worse than no
	//	 detector, because it manufactures confidence."   — c-016, 2026-08-14
	//
	// That is why this clamp exists, and why the duplicates are REPORTED rather than merely
	// absorbed: the count is evidence of the very thing the arbiter is for.
	dup := 0
	if recv > sent {
		dup = recv - sent
		recv = sent
	}
	loss := 100 * (sent - recv) / sent
	stat := fmt.Sprintf("%d/%d replies to %s, %d%% loss", recv, sent, gw, loss)
	if dup > 0 {
		stat += fmt.Sprintf(" (+%d DUPLICATE replies — more than one host may be answering for %s)", dup, gw)
	}
	if loss > claimMaxLossPct {
		// Report the FRACTION, not the verdict: "3/20 replies (85% loss)" tells an operator what
		// happened and is checkable; "not eligible" throws the measurement away.
		return false, stat + " — over the " + strconv.Itoa(claimMaxLossPct) + "% ceiling"
	}
	return true, stat
}

// claimantEligible applies invariant 2 and nothing else.
func claimantEligible(c Claimant) (bool, string) {
	if !devCarrier(c.Dev) {
		return false, "no carrier"
	}
	// Carrier says the PHY agreed on a link; it does not say frames cross it. See above.
	ok, stat := devGatewayAnswers(c.Dev)
	if !ok {
		return false, stat
	}
	// Carry the MEASUREMENT into the pass reason, not just the verdict. "10/10 replies, 0% loss"
	// is checkable by an operator and shows the probe actually ran; "carrier up" hides both.
	if stat == "" {
		stat = "carrier up, gateway not probed"
	}
	if len(c.SSIDs) == 0 {
		return true, stat
	}
	if !devIsWireless(c.Dev) {
		// Config error rather than a state: an SSID list on a wired adapter can never be
		// satisfied meaningfully. Carrier is the whole test; say so loudly instead of
		// silently comparing against whatever nmcli happens to return.
		return true, stat + " (SSID list ignored — " + c.Dev + " is not wireless)"
	}
	cur := devSSID(c.Dev)
	if cur == "" {
		return false, "not associated"
	}
	for _, s := range c.SSIDs {
		if strings.EqualFold(strings.TrimSpace(s), cur) {
			return true, "on " + cur + "; " + stat
		}
	}
	return false, "associated to " + cur + " (not a listed SSID)"
}

// ---- route disposition: HOLDING the address is not BEING the path ----

// routeGet asks the KERNEL which device and source address it would use to reach target, exactly
// as a socket would. `from` optionally pins the source, modelling a socket bound to an address.
//
// It parses `ip route get` rather than `ip route show` on purpose: `show` is the input to the
// decision and would have to be re-derived here (metrics, rule priorities, per-table lookups,
// which of two on-link routes for the same prefix wins); `get` IS the decision, already made, by
// the code that will make it for real. Re-implementing the kernel's selection in order to report
// on it is how you get a report that disagrees with the system it describes.
//
// Returns ("", "") when the lookup fails — an unroutable target is not a finding to shout about.
func routeGet(fam, target, from string) (dev, src string) {
	argv := []string{"ip", "-" + fam, "route", "get", target}
	if from != "" {
		argv = append(argv, "from", from)
	}
	out, err := run(argv...)
	if err != nil {
		return "", ""
	}
	return parseRouteGet(out, from)
}

func parseRouteGet(out, from string) (dev, src string) {
	f := strings.Fields(out)
	for i := 0; i+1 < len(f); i++ {
		switch f[i] {
		case "dev":
			if dev == "" {
				dev = f[i+1]
			}
		case "src":
			if src == "" {
				src = f[i+1]
			}
		}
	}
	// `ip route get X from Y` echoes the pin instead of printing `src`, which is the same answer
	// phrased differently. Say it, rather than reporting an empty source for a bound socket.
	if src == "" && from != "" {
		src = from
	}
	return dev, src
}

// devSubnetAddrs returns the addresses dev carries that sit in the SAME subnet as ref, excluding
// ref itself. The prefix comes from whichever adapter carries ref, so there is nothing to
// configure and nothing to guess.
//
// This is the mechanism behind a HOLDS-is-not-PATH split, and it is worth naming separately from
// the symptom. Measured on .153, 2026-08-14, with the router's own config as the evidence:
//
//	dhcp.r98bd80ec68cd.ip  = '192.168.222.153'
//	dhcp.r98bd80ec68cd.mac = '98:bd:80:ec:68:cd' '48:21:0b:6e:06:85'   <- BOTH NUC adapters
//
// That dual-MAC reservation is the dnsmasq case this whole file exists for. Arbitration works:
// the standby does not get .153. But dnsmasq, refused from issuing the reserved address twice,
// hands the standby a POOL address instead of nothing — and NetworkManager's default wifi metric
// (600) beats the wired profile's explicit 1000, so the standby's consolation address becomes the
// source and the standby becomes the path. Three leases in one hour here (.236 -> .238 -> .239),
// each reassociation taking a fresh one, so the box's effective identity churns while the identity
// the arbiter guards stays rock solid.
//
// Invariant 1 says a standby must neither hold nor request THE LEASE. This is the case it does not
// cover: the standby is on the segment holding a DIFFERENT address, which is the same
// standby-on-segment condition this project's own 2026-08-13 design correction named after
// ms-rosy .186 — and, with arp_ignore=0, the standby still answers ARP for the guarded address
// too. Reported, not acted on: taking a second address away is a policy decision about the
// operator's live path, not something an arbiter should infer.
func devSubnetAddrs(dev, ref string) []string {
	_, netw, err := net.ParseCIDR(refCIDR(dev, ref))
	if err != nil {
		return nil
	}
	out, err := run("ip", "-4", "-o", "addr", "show", "dev", dev)
	if err != nil {
		return nil
	}
	var got []string
	for _, f := range strings.Fields(out) {
		ip, _, err := net.ParseCIDR(f)
		if err != nil || ip.String() == ref || !netw.Contains(ip) {
			continue
		}
		got = append(got, ip.String())
	}
	return got
}

// refCIDR finds ref's prefix by asking the adapter that carries it. Falls back to a /24, which is
// the LAN case this runs in; a wrong guess here can only under-report.
func refCIDR(holder, ref string) string {
	if out, err := run("ip", "-4", "-o", "addr", "show", "dev", holder); err == nil {
		for _, f := range strings.Fields(out) {
			if strings.HasPrefix(f, ref+"/") {
				return f
			}
		}
	}
	return ref + "/24"
}

// ---- identity-MAC failover: move the MAC, not the lease ----
//
// THE OPERATOR'S IDEA, and it is a better architecture rather than a trick (2026-08-14).
//
// dnsmasq(8) states the precondition for a multi-MAC reservation and then says it cannot police
// it: "only one of the hardware addresses is active at any time and there is no way for dnsmasq to
// enforce this." EVERY fault this file has chased descends from that one unenforceable sentence:
//
//   - the wifi leg is offered the shared address on each reassociation, its own RFC 5227 conflict
//     detection finds the address answered by THIS HOST's other interface (arp_ignore=0), it
//     DECLINEs, and dnsmasq benches the reservation for ten minutes — during which NEITHER leg can
//     hold it;
//   - and when the wifi leg briefly does hold it, the arbiter releases it with `nmcli connection
//     down`, which NetworkManager will not autoconnect back. The standby stays dead. Invariant 1
//     was destroying the very leg it existed to preserve. Measured: 19 minutes, twice over.
//
// The fix inverts what is arbitrated. THE ROUTER RESERVES THE ADDRESS TO ONE MAC, and each adapter
// additionally holds its own reservation, so:
//
//	48:21:0b:6e:06:85 (wired) -> 192.168.222.153     the IDENTITY
//	98:bd:80:ec:68:cd (wifi)  -> 192.168.222.154     always reachable, never contended
//
// Failover then sets the winner's `cloned-mac-address` to the identity MAC. A host CAN enforce
// "only one of my NICs wears this MAC" — it is local config with a single authority — which is
// exactly the invariant dnsmasq cannot enforce remotely. An unenforceable distributed precondition
// becomes an enforceable local one.
//
// TWO CONSEQUENCES WORTH STATING, because they reverse earlier rules in this file:
//
//  1. RELEASE BEFORE CLAIM, here. Two NICs wearing one MAC on one segment makes the bridge learn
//     it on two ports and flap — corrupting the SEGMENT's view, which is worse than a gap that
//     only affects us. So the loser gives the MAC back FIRST. That is safe now, and only now,
//     because the standby keeps its own reservation: invariant 3 forbade release-before-claim to
//     avoid de-addressing a box with no console, and with .154 permanently present the box is
//     never without an address. The operator's second reservation is what dissolves the
//     constraint, and it is the part of his design that makes the rest legal.
//  2. NOBODY IS EVER TAKEN DOWN. Losers keep their profile, their own address and their
//     association; they simply stop wearing the identity. The standby-death bug cannot recur.
//
// Gated by the arm flag, per the operator: arming means "you may move the identity".
// devPermMAC returns the adapter's FACTORY MAC, which is what the clear-vs-park decision turns on.
// `ethtool -P` and not nmcli: this box's NetworkManager rejects GENERAL.PERM-HWADDR outright
// ("invalid field"), and it returned empty rather than erroring in a way the caller noticed — which
// silently fed an empty permanent MAC into the planner and degraded it back into the collision the
// parking exists to prevent. A lookup that can fail quietly is a lookup that will.
func devPermMAC(dev string) string {
	out, err := run("ethtool", "-P", dev)
	if err != nil {
		return ""
	}
	if i := strings.LastIndex(out, ":"); i >= 0 {
		f := strings.Fields(out)
		cand := strings.ToLower(f[len(f)-1])
		if _, err := net.ParseMAC(cand); err == nil {
			return cand
		}
	}
	return ""
}

func devCurrentMAC(dev string) string {
	b, err := os.ReadFile("/sys/class/net/" + dev + "/address")
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(b)))
}

// parkedMAC derives an address for an adapter that must stop wearing the identity but must stay
// up: its own permanent MAC with the LOCALLY-ADMINISTERED bit set (0x02 in the first octet).
//
// That bit is precisely what distinguishes a locally-assigned address from a manufacturer-assigned
// one, so the result cannot collide with any factory MAC on the segment — a stronger guarantee
// than incrementing an octet, which could in principle land on a real neighbour. Deterministic and
// stable, so an adapter parks on the same address every time and the router's leases stay legible.
func parkedMAC(perm string) string {
	var b []byte
	if m, err := net.ParseMAC(perm); err == nil {
		b = []byte(m)
	}
	if len(b) != 6 {
		return ""
	}
	b[0] |= 0x02
	out := make([]string, 6)
	for i, x := range b {
		out[i] = fmt.Sprintf("%02x", x)
	}
	return strings.Join(out, ":")
}

// macOp is one adapter's identity change, with the reason attached so the plan explains itself.
type macOp struct {
	Dev     string
	Profile string
	Set     string // cloned-mac value to write ("" = clear, i.e. use the permanent MAC)
	Why     string
	Refuse  bool // this is not an operation: it is a refusal to plan, and must not be executed
}

// claimMACPlan returns the ops needed so that exactly `winner` wears IdentityMAC, LOSERS FIRST.
// Pure: it takes the observed MACs, so the ordering rule can be tested without two NICs.
func claimMACPlan(cl *Claim, winner string, perm, cur map[string]string) []macOp {
	if cl.IdentityMAC == "" || winner == "" {
		return nil
	}
	id := strings.ToLower(cl.IdentityMAC)
	var ops []macOp

	// 1. Losers give it back FIRST — never overlap two NICs on one MAC.
	//
	// AND THE LOSER MUST END UP NOT WEARING IT, which is not the same as "clear the override".
	// The operator caught this before it shipped: the WIRED NIC's PERMANENT MAC *is* the identity
	// here, so clearing its clone leaves it wearing the identity exactly as before — and the winner
	// would then assume a MAC its predecessor never gave up. Two NICs, one MAC, one segment: the
	// bridge learns it on two ports and flaps, corrupting the SEGMENT's view rather than just ours.
	// The test that "proved" the ordering asserted this wrong behaviour, which is a good reminder
	// that a test only checks the property you thought to state.
	//
	// So: clear when clearing is enough, and PARK otherwise. Parking sets the locally-administered
	// bit on the adapter's own permanent MAC (48:21:… -> 4a:21:…), which is the standard derivation
	// for an address guaranteed not to collide with any factory-assigned one, is deterministic and
	// stable per adapter, and — the operator's point — **keeps the NIC UP**. Taking an interface
	// down to resolve a MAC conflict is a bigger hammer than the problem; wearing a different MAC
	// costs that leg only its DHCP reservation, and it lands on a pool address still reachable.
	for _, c := range cl.Claimants {
		if c.Dev == winner || cur[c.Dev] != id {
			continue
		}
		if perm[c.Dev] == "" {
			// REFUSE rather than guess. With an unknown permanent MAC we cannot tell whether
			// clearing releases the identity or leaves the adapter wearing it, and guessing wrong
			// puts two NICs on one MAC. Fail closed: no ops, and the caller reports why.
			return []macOp{{Dev: c.Dev, Set: "", Why: "REFUSED: cannot read " + c.Dev +
				"'s permanent MAC (ethtool -P), so releasing it cannot be done safely", Refuse: true}}
		}
		if perm[c.Dev] != id {
			ops = append(ops, macOp{Dev: c.Dev, Set: "",
				Why: c.Dev + " releases the identity MAC (back to its permanent " + perm[c.Dev] + ", its own reservation)"})
			continue
		}
		parked := parkedMAC(perm[c.Dev])
		ops = append(ops, macOp{Dev: c.Dev, Set: parked,
			Why: c.Dev + " PARKS on " + parked + " — its permanent MAC is the identity, so clearing would not release it"})
	}
	// 2. Then the winner takes it.
	if cur[winner] != id {
		set := id
		why := winner + " assumes the identity MAC " + id
		if perm[winner] == id {
			// Its own permanent MAC IS the identity: clear the clone rather than pinning it, so
			// the profile carries no override it does not need.
			set = ""
			why = winner + " returns to its permanent MAC, which IS the identity " + id
		}
		ops = append(ops, macOp{Dev: winner, Set: set, Why: why})
	}
	return ops
}

// claimNeedsAttention is the LIVENESS pre-check: cheap, probe-free, and the answer to the gap
// c-001 measured on 2026-08-14 (n-243 §3).
//
// THE GAP. Arbitration is EDGE-TRIGGERED — it runs from the NM dispatcher on carrier events and
// nowhere else. A failed claim sets a 300s cooldown to stop a hot loop, which it does. But if the
// network then goes QUIET, no further edge arrives, the cooldown lapses with nobody watching, and
// the guarded address stays held by NOBODY indefinitely. Measured: 19 minutes on .153, ended only
// by a human running `nmcli connection up`. The cooldown correctly prevents a hot loop and
// accidentally guarantees a cold one never resolves — and THE QUIETER THE NETWORK, THE LONGER IT
// STAYS STRANDED, which is the opposite of what anyone assumes, and why it survived review.
//
// WHY A PRE-CHECK RATHER THAN JUST RE-RUNNING ARBITRATION ON A TIMER. Eligibility costs a 10-probe
// arping per claimant by construction (~10s wall). Running that every minute on every box would
// burn the segment for nothing, on the overwhelmingly common case where the address is exactly
// where it should be. This asks two questions that need no packets:
//
//  1. Does ANYBODY hold the address? Nobody ⇒ stranded ⇒ act. This is the measured failure.
//  2. Does a HIGHER-PRIORITY claimant have carrier while a lower-priority one holds it? That is
//     the preferred leg having come back after being released — the come-back case. Carrier alone
//     does not make it eligible (carrier is not health, invariant), so this only decides whether
//     the expensive test is WORTH RUNNING; claimEvaluate still decides the outcome.
//
// Everything else is a no-op, so the timer is nearly free and the probe still gates every move.
func claimNeedsAttention(cl *Claim) (bool, string) {
	holder := currentHolder(cl)
	if holder == "" {
		return true, "STRANDED: " + cl.Address + " is held by NOBODY"
	}
	held := 0
	for _, c := range cl.Claimants {
		if c.Dev == holder {
			held = c.Priority
		}
	}
	for _, c := range cl.Claimants {
		if c.Dev == holder || c.Priority <= held {
			continue
		}
		if devCarrier(c.Dev) {
			return true, "RETURNED: " + c.Dev + " (priority " + strconv.Itoa(c.Priority) +
				") has carrier and outranks the holder " + holder + " (priority " + strconv.Itoa(held) + ")"
		}
	}
	return false, "holder " + holder + " is present and no higher-priority claimant has carrier"
}

// currentHolder returns the claimant device that actually carries the address right now, or "".
// Deliberately the OBSERVED holder rather than claimEvaluate's winner: the point of the path
// report is to describe the box as it stands, and the winner is what the arbiter would move to.
func currentHolder(cl *Claim) string {
	for _, c := range cl.Claimants {
		if devHoldsAddr(c.Dev, cl.Address) {
			return c.Dev
		}
	}
	return ""
}

// claimPaths reports which adapter this host actually TALKS on, next to the one that HOLDS the
// claimed address.
//
// WHY THIS EXISTS. On 2026-08-14 c-019 measured this box and found `claim status` reporting
// `OK: enp114s0 already holds 192.168.222.153 exclusively`. That verdict was true — the address
// was held, exclusively, by an eligible adapter probing 0% loss. And every packet the host
// originated left over a *second* adapter, which had taken its own DHCP address on the same
// subnet at a lower route metric. The arbiter's guarantee (exactly one adapter holds the address)
// and the property a reader takes from it (the host talks on that adapter) had come apart
// cleanly, on an armed box, with every check green.
//
// It is the mechanism this project already named on ms-rosy .186 — THE PATH IS CHOSEN BY ROUTE
// METRIC, NOT BY WHO HOLDS THE ADDRESS. There it explained a fault; here it is a standing
// condition that the tool's own output could not see. A verdict that is true and misread is a
// reporting defect, and the fix for a reporting defect is to report the other half.
//
// Deliberately READ-ONLY, and deliberately NOT part of claimReconcile:
//
//   - The arbiter's remit is the ADDRESS. A route metric is the host's own policy and there are
//     good reasons for the split — on this very box the wired profile is `ipv4.never-default` on
//     purpose, so that cabling to the router cannot capture the session's internet lifeline. An
//     arbiter that "fixed" that would be overriding a deliberate guard it knows nothing about.
//   - It must not run on the dispatcher hot path. Arbitration already costs a bounded probe per
//     claimant on a carrier event; adding lookups that change no decision would be pure latency.
//
// So: report it, do not arbitrate it. The operator decides whether the split is intended.
func claimPaths(cl *Claim, holder string) []string {
	if holder == "" {
		return nil
	}
	gw := dhcpRouter(holder)
	if gw == "" {
		gw = defaultGateway()
	}

	obs := pathObs{Gateway: gw}
	if gw != "" {
		obs.OnDev, obs.OnSrc = routeGet("4", gw, "")
		obs.BoundDev, _ = routeGet("4", gw, cl.Address)
	}
	obs.OffDev, obs.OffSrc = routeGet("4", "1.1.1.1", "")
	for _, c := range cl.Claimants {
		if c.Dev == holder {
			continue
		}
		for _, a := range devSubnetAddrs(c.Dev, cl.Address) {
			obs.Extra = append(obs.Extra, c.Dev+" "+a)
		}
	}
	return pathLines(cl.Address, holder, obs)
}

// pathObs is what the kernel answered. Separated from the verdict below so the verdict is a pure
// function of measurements — otherwise the only way to check that the split is reported is to
// arrange a two-adapter host, which is exactly the condition nobody has when the code is written.
type pathObs struct {
	Gateway        string
	OnDev, OnSrc   string   // unbound, to the on-link gateway
	OffDev, OffSrc string   // unbound, off-link
	BoundDev       string   // bound to the claimed address, on-link
	Extra          []string // "dev addr": a NON-holder holding another address on the guarded subnet
}

// pathLines turns the observations into the report. Pure: no I/O, so a test can assert the effect.
func pathLines(addr, holder string, o pathObs) []string {
	var out []string
	if o.OnDev != "" {
		out = append(out, fmt.Sprintf(" path: %-24s -> %-14s src %s", "on-link ("+o.Gateway+")", o.OnDev, orDash(o.OnSrc)))
	}
	if o.OffDev != "" {
		out = append(out, fmt.Sprintf(" path: %-24s -> %-14s src %s", "off-link (1.1.1.1)", o.OffDev, orDash(o.OffSrc)))
	}
	// A standby holding its own address on the guarded subnet is reportable on its own account,
	// whichever way the routes currently fall: it is on the segment, it competes for the reply
	// path, and under arp_ignore=0 it answers ARP for the guarded address too.
	for _, e := range o.Extra {
		out = append(out, " ⚠ standby on the guarded subnet: "+e+
			" — a second address on this subnet, so the standby is still on the segment")
	}
	if o.OnDev == "" {
		return out
	}
	if o.OnDev == holder && o.OnSrc == addr {
		return append(out, " path: holder is the path — traffic on this subnet leaves via "+holder+" as "+addr)
	}

	// The finding. Name the consequence a peer would see, not the abstraction: "peers see .238"
	// is checkable from the other end; "route disposition differs" is not.
	msg := " ⚠ HOLDS is not PATH: " + holder + " holds " + addr +
		", but traffic this host originates on that subnet leaves via " + o.OnDev
	if o.OnSrc != "" && o.OnSrc != addr {
		msg += " as " + o.OnSrc + " — peers see " + o.OnSrc + ", not " + addr
	}
	out = append(out, msg)
	out = append(out, "   the path is chosen by ROUTE METRIC, not by who holds the address; arbitration"+
		" does not change it and is not reporting a fault here (`ip route get "+o.Gateway+"`)")

	// A socket BOUND to the claimed address is the case that still works, and saying so keeps the
	// warning from reading as "the address is unusable" when it is not.
	if o.BoundDev != "" {
		out = append(out, "   bound to "+addr+": on-link -> "+o.BoundDev)
	}
	return out
}

// devHoldsAddr reports whether dev currently carries addr.
func devHoldsAddr(dev, addr string) bool {
	out, err := run("ip", "-4", "-o", "addr", "show", "dev", dev)
	if err != nil {
		return false
	}
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, addr+"/") || f == addr {
			return true
		}
	}
	return false
}

// devProfile returns the NM connection profile for dev — the ACTIVE one, or failing that the
// profile BOUND to that interface.
//
// The fallback is not cosmetic. `GENERAL.CONNECTION` is empty for a device with no active
// connection, which is precisely the state of a leg whose cable has been pulled — so the identity
// swap logged "no active profile on enp114s0 — skipped" and never wrote the parked MAC. The leg
// then came back up still wearing the identity that the wifi had meanwhile assumed: two NICs, one
// MAC, exactly the collision the parking exists to prevent, reached by the failover path itself.
// Found by pulling a cable rather than by reading the code.
func devProfile(dev string) string {
	out, err := run("nmcli", "-t", "-f", "GENERAL.CONNECTION", "device", "show", dev)
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(out, "\n") {
		if p := strings.SplitN(ln, ":", 2); len(p) == 2 {
			v := strings.TrimSpace(p[1])
			if v != "" && v != "--" {
				return v
			}
		}
	}
	return profileForIface(dev)
}

// profileForIface finds the connection bound to an interface even when nothing is active on it.
func profileForIface(dev string) string {
	out, err := run("nmcli", "-t", "-f", "NAME", "connection", "show")
	if err != nil {
		return ""
	}
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(strings.ReplaceAll(name, "\\:", ":"))
		if name == "" {
			continue
		}
		if v, e := run("nmcli", "-g", "connection.interface-name", "connection", "show", name); e == nil &&
			strings.TrimSpace(v) == dev {
			return name
		}
	}
	return ""
}

type claimVerdict struct {
	Winner  string   // dev that should hold the address ("" = nobody eligible)
	Holders []string // devs currently holding it
	Lines   []string // human-readable reasoning, one per claimant
}

// claimEvaluate decides who SHOULD hold the address and who currently DOES. Read-only.
func claimEvaluate(cl *Claim) claimVerdict {
	v := claimVerdict{}
	cs := append([]Claimant(nil), cl.Claimants...)
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Priority > cs[j].Priority })

	// Probe every claimant CONCURRENTLY. The gateway loss test is ~10s of wall time by
	// construction (arping's -i takes whole seconds on iputils, so N probes cost N seconds), and
	// this runs from the NM dispatcher hook on a carrier event. Serially it would be 10s PER
	// claimant and hold the hook for the sum; concurrently the cost is the slowest single leg.
	// The probes are independent reads on different interfaces, so there is nothing to serialise.
	type res struct {
		ok  bool
		why string
	}
	rs := make([]res, len(cs))
	var wg sync.WaitGroup
	for i := range cs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, why := claimantEligible(cs[i])
			rs[i] = res{ok, why}
		}(i)
	}
	wg.Wait()

	for i, c := range cs {
		ok, why := rs[i].ok, rs[i].why
		holds := devHoldsAddr(c.Dev, cl.Address)
		if holds {
			v.Holders = append(v.Holders, c.Dev)
		}
		mark := "  "
		if ok && v.Winner == "" {
			v.Winner = c.Dev
			mark = "->"
		}
		state := "not eligible"
		if ok {
			state = "eligible"
		}
		held := ""
		if holds {
			held = "  [HOLDS " + cl.Address + "]"
		}
		v.Lines = append(v.Lines, fmt.Sprintf("%s %-14s prio=%-3d %-12s (%s)%s", mark, c.Dev, c.Priority, state, why, held))
	}
	return v
}

// claimFailFile records the last failed attempt. On /run (tmpfs), so a reboot is a legitimate
// fresh start — a box that comes up clean should be allowed to try.
// var, not const, so tests can point it at a writable temp path. /run is root-owned, which is
// correct for the path that matters: the loop happens on the DISPATCHER-triggered runs, and those
// are root. A hand-run `claim apply` as an unprivileged user cannot write it — and rather than
// swallow that, recordClaimFailure returns the error so the caller can say so. The first version
// of this ignored the write error with `_ =` and the cooldown silently did nothing; the unit test
// caught it, which is the only reason it is not shipping that way.
var claimFailFile = "/run/netgov-claim.failed"

// claimFailedRecently reports whether an attempt on this address failed inside the cooldown.
func claimFailedRecently(addr string) (bool, int) {
	b, err := os.ReadFile(claimFailFile)
	if err != nil {
		return false, 0
	}
	f := strings.Fields(strings.TrimSpace(string(b)))
	if len(f) != 2 || f[0] != addr {
		return false, 0
	}
	ts, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return false, 0
	}
	left := claimFailCooldown - int(time.Now().Unix()-ts)
	return left > 0, left
}

func recordClaimFailure(addr string) error {
	return os.WriteFile(claimFailFile, []byte(addr+" "+strconv.FormatInt(time.Now().Unix(), 10)+"\n"), 0o644)
}

func clearClaimFailure() { _ = os.Remove(claimFailFile) }

// hookPath is where `netgov install` writes the NM dispatcher hook — the thing that actually runs
// arbitration on a carrier event.
const hookPath = "/etc/NetworkManager/dispatcher.d/90-netgov"

// claimEnforcement answers c-016's question: is arbitration ENFORCING, or merely armed?
//
//	"armed=true is not a state, it is an intention. The enforceable state is
//	 armed AND hook present AND arping capable — any one of the three missing gives you an
//	 armed box that arbitrates nothing, and only the conjunction should be reportable as
//	 'arbitration is on'."   — c-016, 2026-08-14
//
// That is three-for-three on the night of 2026-08-13/14, and every one presented as SUCCESS from
// every angle available to an operator — systemctl clean, `claim status` giving a confident
// verdict, the arm flag present, `grep -c claim` returning 8:
//
//	hook absent          ms-rosy 22:16-00:39   install had never been run there
//	hook cannot execute  ms-rosy 00:30:53      pointed at /root/bin/netgov, logged "not found"
//	probe inert          ms-rosy on 2.0        arping installed and capable; the build had no probe
//
// Reporting a conjunction as a single boolean is what let all three hide. So the tool now detects
// the silent condition it creates itself, rather than requiring three manual checks that only a
// custodian who has already been bitten knows to run. Same move as `ver --shadows`.
func claimEnforcement() (bool, []string) {
	var out []string
	ok := true

	if claimArmed() {
		out = append(out, "armed: yes ("+claimArmedFlag+")")
	} else {
		out = append(out, "armed: NO — arbitration will not act (netgov claim arm)")
		ok = false
	}

	b, err := os.ReadFile(hookPath)
	switch {
	case err != nil:
		out = append(out, "hook: MISSING ("+hookPath+") — run `netgov install`")
		ok = false
	case !strings.Contains(string(b), "claim apply"):
		out = append(out, "hook: present but NOT claim-aware (pre-2.7) — re-run `netgov install`")
		ok = false
	default:
		bin := hookBinary(string(b))
		switch {
		case bin == "":
			out = append(out, "hook: claim-aware, but its binary path could not be read")
			ok = false
		default:
			if fi, err := os.Stat(bin); err != nil {
				out = append(out, "hook: claim-aware but CANNOT EXECUTE — "+bin+" does not exist")
				ok = false
			} else if fi.Mode()&0o111 == 0 {
				out = append(out, "hook: claim-aware but "+bin+" is not executable")
				ok = false
			} else {
				out = append(out, "hook: OK ("+bin+", executable)")
			}
		}
	}

	if p, err := exec.LookPath("arping"); err != nil {
		out = append(out, "probe: arping ABSENT — the loss test fails open, so a negotiating-but-dead link counts as eligible")
		ok = false
	} else {
		out = append(out, "probe: OK ("+p+")")
	}
	return ok, out
}

// hookBinary pulls the netgov path out of the generated hook, so the check is against what the
// hook will ACTUALLY run rather than what we assume install wrote.
func hookBinary(hook string) string {
	for _, ln := range strings.Split(hook, "\n") {
		if !strings.Contains(ln, "claim apply") {
			continue
		}
		for _, f := range strings.Fields(ln) {
			if strings.HasSuffix(f, "/netgov") {
				return f
			}
		}
	}
	return ""
}

// claimReconcile enforces exactly-one-holder. dry => plan only, mutate nothing.
//
// Ordering implements invariant 3: the winner is brought UP before any loser is taken down, so
// the box is never simultaneously without a live claimant. If nobody is eligible we return
// without touching anything at all — the current holder keeps the address by construction.
func claimReconcile(cl *Claim, dry bool) []string {
	v := claimEvaluate(cl)
	log := append([]string(nil), v.Lines...)

	if v.Winner == "" {
		log = append(log, "NO-OP: no eligible claimant — current holder keeps "+cl.Address+" (claim-before-release)")
		return log
	}

	// IDENTITY-MAC MODE (2.27) — preferred when the claim declares one. Moves the MAC rather than
	// arbitrating the lease, so no adapter is ever taken down and no adapter ever contends for the
	// address. See claimMACPlan for why this is the better mechanism.
	if cl.IdentityMAC != "" {
		perm, cur := map[string]string{}, map[string]string{}
		for _, c := range cl.Claimants {
			perm[c.Dev], cur[c.Dev] = devPermMAC(c.Dev), devCurrentMAC(c.Dev)
		}
		ops := claimMACPlan(cl, v.Winner, perm, cur)
		// NOT FROM INSIDE THE NM DISPATCHER. `nmcli connection modify` re-enters NetworkManager
		// while NM is still processing the dispatcher event that invoked us, and it fails: measured
		// `ABORT: could not set 802-11-wireless.cloned-mac-address on CNNet (exit status 1)` on the
		// hook path, while the identical command from the systemd timer succeeded seconds later.
		// Worse than failing, it recorded a 300s cooldown that then SUPPRESSED the timer — the one
		// path that could have done the job. So: plan, log, defer, and leave no cooldown behind.
		// The liveness timer picks it up within 60s and that is the correct owner of the work.
		if len(ops) > 0 && !dry && fromDispatcher() {
			log = append(log, "DEFERRED: NetworkManager will not accept a connection change from"+
				" inside its own dispatcher; the claim-watch timer applies this within 60s")
			for _, o := range ops {
				log = append(log, "  would: "+o.Why)
			}
			return log
		}
		if len(ops) == 1 && ops[0].Refuse {
			return append(log, "ABORT: "+ops[0].Why+" — nothing changed")
		}
		if len(ops) == 0 {
			log = append(log, "OK: "+v.Winner+" already wears the identity MAC "+cl.IdentityMAC+
				"; every other claimant is on its own address")
			return log
		}
		for _, o := range ops {
			log = append(log, "ACTION: "+o.Why)
		}
		if dry {
			return append(log, "DRY-RUN: nothing applied")
		}
		if recent, left := claimFailedRecently(cl.Address); recent {
			return append(log, "SUPPRESSED: an attempt on "+cl.Address+" failed recently; not retrying for "+
				strconv.Itoa(left)+"s. Clear "+claimFailFile+" to retry sooner.")
		}
		return append(log, applyMACOps(cl, ops)...)
	}

	var losers []string
	for _, h := range v.Holders {
		if h != v.Winner {
			losers = append(losers, h)
		}
	}
	if devHoldsAddr(v.Winner, cl.Address) && len(losers) == 0 {
		log = append(log, "OK: "+v.Winner+" already holds "+cl.Address+" exclusively")
		return log
	}
	if len(losers) == 0 {
		log = append(log, "ACTION: bring up "+v.Winner+" to claim "+cl.Address)
	} else {
		log = append(log, "ACTION: move "+cl.Address+" -> "+v.Winner+", release from "+strings.Join(losers, ","))
	}

	if dry {
		log = append(log, "DRY-RUN: nothing applied")
		return log
	}

	// COOLDOWN GATE — the loop stopper. A failed attempt cycles an interface, the dispatcher sees
	// the carrier event and calls us again; without this the pair re-triggers indefinitely and
	// every iteration leaves the address unheld.
	if recent, left := claimFailedRecently(cl.Address); recent {
		log = append(log, "SUPPRESSED: an attempt on "+cl.Address+" failed recently; not retrying for "+
			strconv.Itoa(left)+"s. Clear "+claimFailFile+" to retry sooner.")
		return log
	}

	// 1. Claim: ensure the winner's profile is up FIRST (invariant 3).
	if p := devProfile(v.Winner); p != "" {
		if err := runPriv("nmcli", "connection", "up", p); err != nil {
			log = append(log, "ABORT: could not bring up "+v.Winner+" ("+err.Error()+") — nothing released")
			return log
		}
	}
	// 2. Release: losers must neither hold nor request the lease (invariant 1). Taking the
	//    profile down stops its DHCP client too; a route-level change would not.
	for _, l := range losers {
		p := devProfile(l)
		if p == "" {
			continue
		}
		if err := runPriv("nmcli", "connection", "down", p); err != nil {
			log = append(log, "WARN: could not release "+l+" ("+err.Error()+")")
			continue
		}
		log = append(log, "released "+l)
	}
	// 3. Re-request on the winner now the MAC contention is gone, so the reservation lands.
	if len(losers) > 0 {
		if p := devProfile(v.Winner); p != "" {
			_ = runPriv("nmcli", "connection", "up", p)
		}
	}

	// 3b. WAIT for the winner to ACTUALLY acquire, then roll back if it does not.
	//
	// THIS IS THE STEP THAT WAS MISSING, and its absence broke invariant 4 in practice. The old
	// code asked devHoldsAddr immediately after `nmcli connection up` and DHCP is not instant, so
	// the check lost the race every time: on 2026-08-13 it logged "released wlo1" then "WARN: did
	// not acquire", leaving a real window with NOBODY holding the address on the operator's own
	// workstation. It recovered only because a later DHCP round happened to succeed. A warning is
	// not a safety mechanism.
	//
	// Why a release has to happen at all: with a dual-MAC DHCP reservation, dnsmasq will not hand
	// the address to the second MAC while the first still holds it, so a literal acquire-then-
	// release is impossible. The invariant's INTENT — never leave the box without the address —
	// is therefore preserved by making the move a TRANSACTION: release, wait, and on failure
	// restore the previous holder rather than leaving the address unheld.
	got := false
	for i := 0; i < claimAcquireTimeout; i++ {
		if devHoldsAddr(v.Winner, cl.Address) {
			got = true
			break
		}
		time.Sleep(time.Second)
	}
	if !got {
		// BOTH failure paths land here. The previous guard was `!got && len(losers) > 0`, so a
		// claim with NO incumbent — winner fails, nobody to restore — skipped this block entirely
		// and fell through to a passing WARN. That is the case that stranded .186 on 2026-08-14:
		// I wrote the handler for the move and left claim-with-no-incumbent exactly as broken,
		// then reported the transaction as fixed. A failure must be loud and must set the
		// cooldown whether or not there is anything to roll back.
		if err := recordClaimFailure(cl.Address); err != nil {
			// Loud, because the consequence is the loop coming back: if the cooldown cannot be
			// recorded, nothing stops the next dispatcher event from retrying immediately.
			log = append(log, "WARN: could not record the failure cooldown ("+err.Error()+
				") — retries are NOT suppressed. Run as root, or disarm until this is resolved.")
		}
		if len(losers) == 0 {
			log = append(log, "FAILED: "+v.Winner+" did not acquire "+cl.Address+" within "+
				strconv.Itoa(claimAcquireTimeout)+"s. Nothing was released, so there is nothing to "+
				"restore — but "+cl.Address+" is held by NOBODY. Check the router reservation covers "+
				v.Winner+"'s MAC, and that no stale lease binds that MAC to a pool address.")
			log = append(log, "COOLDOWN: further attempts suppressed for "+
				strconv.Itoa(claimFailCooldown)+"s (a dispatcher-triggered retry would loop).")
			return log
		}
		log = append(log, "ROLLBACK: "+v.Winner+" did not acquire "+cl.Address+" within "+
			strconv.Itoa(claimAcquireTimeout)+"s — restoring the previous holder")
		for _, l := range losers {
			p := devProfile(l)
			if p == "" {
				continue
			}
			if err := runPriv("nmcli", "connection", "up", p); err != nil {
				// The one outcome worse than not moving: moved from, not moved to, and not
				// restored. Say so unmistakably — this is the state a human must resolve.
				log = append(log, "CRITICAL: rollback of "+l+" FAILED ("+err.Error()+") — "+
					cl.Address+" may be held by NOBODY. Intervene.")
				continue
			}
			log = append(log, "restored "+l+" — "+cl.Address+" stays where it was")
		}
		log = append(log, "NO-OP: move abandoned; check the router reservation covers "+v.Winner+"'s MAC")
		log = append(log, "COOLDOWN: further attempts suppressed for "+
			strconv.Itoa(claimFailCooldown)+"s (a dispatcher-triggered retry would loop).")
		return log
	}

	// 4. Gratuitous ARP (invariant 4) — the segment must be told, not assumed. Verify from a
	//    third box; this only announces.
	if got || devHoldsAddr(v.Winner, cl.Address) {
		if err := runPriv("arping", "-U", "-c", "3", "-I", v.Winner, cl.Address); err != nil {
			log = append(log, "WARN: gratuitous ARP failed ("+err.Error()+") — peers may still cache the old MAC")
		} else {
			log = append(log, "gratuitous ARP sent on "+v.Winner+" for "+cl.Address)
		}
		clearClaimFailure()
		log = append(log, "APPLIED: "+v.Winner+" holds "+cl.Address)
	} else {
		log = append(log, "WARN: "+v.Winner+" did not acquire "+cl.Address+" — check the router reservation covers its MAC")
	}
	return log
}

// claimForActive returns the claim of the currently active pattern, if any. A claim is inert
// unless its own pattern is the active one.
func claimForActive(st *State) *Claim {
	for i := range st.Patterns {
		if st.Patterns[i].Name == st.ActivePattern {
			return st.Patterns[i].Claim
		}
	}
	return nil
}

// applyPatternClaim runs arbitration for a pattern as it is activated. It is a no-op unless the
// pattern declares a claim AND the arbiter is armed, so an unarmed box behaves exactly as it did
// before this file existed — which is the state it ships in.
func applyPatternClaim(p *Pattern) {
	if p == nil || p.Claim == nil {
		return
	}
	if !claimArmed() {
		fmt.Fprintf(os.Stderr, "netgov: pattern %s declares a claim on %s but the arbiter is DISARMED — not touching addresses\n", p.Name, p.Claim.Address)
		return
	}
	for _, l := range claimReconcile(p.Claim, false) {
		fmt.Fprintln(os.Stderr, "netgov claim: "+l)
	}
}

// parseClaimant parses "dev:priority[:ssid,ssid,...]" — e.g. "enp114s0:100" or "wlo1:50:CNNet,TowerNet".
func parseClaimant(spec string) (Claimant, error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 {
		return Claimant{}, fmt.Errorf("claimant %q: want dev:priority[:ssid,ssid]", spec)
	}
	prio, err := strconv.Atoi(parts[1])
	if err != nil {
		return Claimant{}, fmt.Errorf("claimant %q: priority %q is not a number", spec, parts[1])
	}
	c := Claimant{Dev: parts[0], Priority: prio}
	if len(parts) > 2 && parts[2] != "" {
		for _, s := range strings.Split(parts[2], ",") {
			if s = strings.TrimSpace(s); s != "" {
				c.SSIDs = append(c.SSIDs, s)
			}
		}
	}
	if _, err := os.Stat("/sys/class/net/" + c.Dev); err != nil {
		return c, fmt.Errorf("claimant %q: no such interface on this host", c.Dev)
	}
	if len(c.SSIDs) > 0 && !devIsWireless(c.Dev) {
		return c, fmt.Errorf("claimant %q: SSIDs given but %s is not wireless — they would be ignored", spec, c.Dev)
	}
	return c, nil
}

// parseClaimForm parses the dashboard's single-line claim field into a Claim.
//
//	"192.168.222.153 enp114s0:100 wlo1:50:CNNet"   -> a claim
//	""                                             -> nil (no claim)
//
// DELIBERATELY the same grammar as `netgov claim set`, so the CLI and the dashboard cannot drift
// into two dialects of the same thing and one set of docs covers both.
func parseClaimForm(spec string) (*Claim, error) {
	f := strings.Fields(strings.TrimSpace(spec))
	if len(f) == 0 {
		return nil, nil
	}
	if len(f) < 2 {
		return nil, fmt.Errorf("claim: want '<address> <dev:prio[:ssid,ssid]> ...'")
	}
	if net.ParseIP(f[0]) == nil {
		return nil, fmt.Errorf("claim: %q is not an IP address", f[0])
	}
	cl := &Claim{Address: f[0]}
	seen := map[string]bool{}
	for _, s := range f[1:] {
		// identity=<mac> declares the MAC that FOLLOWS the address between adapters (2.27).
		// `=` rather than `:` because a MAC is full of colons and the claimant grammar splits on them.
		if strings.HasPrefix(strings.ToLower(s), "identity=") {
			m := strings.ToLower(strings.TrimSpace(s[len("identity="):]))
			if _, err := net.ParseMAC(m); err != nil {
				return nil, fmt.Errorf("claim: %q is not a MAC address", m)
			}
			cl.IdentityMAC = m
			continue
		}
		c, err := parseClaimant(s)
		if err != nil {
			return nil, err
		}
		if seen[c.Dev] {
			return nil, fmt.Errorf("claim: %s listed twice — one entry per adapter", c.Dev)
		}
		seen[c.Dev] = true
		cl.Claimants = append(cl.Claimants, c)
	}
	return cl, nil
}

// claimFormString renders a Claim back into the field's own grammar, so loading a pattern into
// the builder round-trips exactly what saving it would write.
func claimFormString(cl *Claim) string {
	if cl == nil {
		return ""
	}
	out := cl.Address
	if cl.IdentityMAC != "" {
		out += " identity=" + cl.IdentityMAC
	}
	for _, c := range cl.Claimants {
		out += " " + c.Dev + ":" + strconv.Itoa(c.Priority)
		if len(c.SSIDs) > 0 {
			out += ":" + strings.Join(c.SSIDs, ",")
		}
	}
	return out
}

// claimSet declares a claim group on a pattern. This exists because the alternative was
// hand-editing state.json — which is live state written by the daemon, not a file a human should
// author. A claim group IS configuration, so it needs a real setter.
func claimSet(st *State, sp string, args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: netgov claim set <pattern> <address> <dev:prio[:ssid,ssid]> [more...]")
		os.Exit(1)
	}
	name, addr, specs := args[0], args[1], args[2:]

	var p *Pattern
	for i := range st.Patterns {
		if st.Patterns[i].Name == name {
			p = &st.Patterns[i]
		}
	}
	if p == nil {
		fmt.Fprintf(os.Stderr, "no such pattern %q\n", name)
		os.Exit(1)
	}
	if net.ParseIP(addr) == nil {
		fmt.Fprintf(os.Stderr, "address %q is not an IP\n", addr)
		os.Exit(1)
	}

	// Reuse parseClaimForm rather than re-walking the specs here. This function used to have its
	// own copy of the claimant loop, and when 2.27 added `identity=<mac>` to the grammar the CLI
	// silently ignored it while the web form accepted it — two parsers for one grammar is one
	// parser too many.
	cl, err := parseClaimForm(strings.Join(append([]string{addr}, specs...), " "))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// A single claimant is legal but almost never intended: arbitration between one candidate is
	// a no-op, so say so rather than let it look configured.
	if len(cl.Claimants) < 2 {
		fmt.Fprintf(os.Stderr, "note: only one claimant — nothing to arbitrate between\n")
	}
	p.Claim = cl
	if err := saveState(st, sp); err != nil {
		fmt.Fprintln(os.Stderr, "save failed:", err)
		os.Exit(1)
	}
	fmt.Printf("claim set on pattern %s: %s\n", name, addr)
	for _, c := range cl.Claimants {
		ss := ""
		if len(c.SSIDs) > 0 {
			ss = "  ssids=" + strings.Join(c.SSIDs, ",")
		}
		fmt.Printf("  %-14s prio=%d%s\n", c.Dev, c.Priority, ss)
	}
	fmt.Println("arbitration stays INERT until this pattern is active AND `netgov claim arm` is run.")
}

// claimClear removes a claim group from a pattern.
func claimClear(st *State, sp string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: netgov claim clear <pattern>")
		os.Exit(1)
	}
	for i := range st.Patterns {
		if st.Patterns[i].Name == args[0] {
			if st.Patterns[i].Claim == nil {
				fmt.Printf("pattern %s has no claim\n", args[0])
				return
			}
			st.Patterns[i].Claim = nil
			if err := saveState(st, sp); err != nil {
				fmt.Fprintln(os.Stderr, "save failed:", err)
				os.Exit(1)
			}
			fmt.Printf("claim cleared on pattern %s\n", args[0])
			return
		}
	}
	fmt.Fprintf(os.Stderr, "no such pattern %q\n", args[0])
	os.Exit(1)
}

// cmdClaim implements `netgov claim [status|eval|apply|set|clear|arm|disarm]`.
func cmdClaim(st *State, args []string) {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	cl := claimForActive(st)

	switch sub {
	case "set":
		claimSet(st, statePathFor(args), stripFlags(args[1:], "--state"))
		return
	case "clear":
		claimClear(st, statePathFor(args), stripFlags(args[1:], "--state"))
		return
	case "tick":
		// The timer entry point (netgov-claim-watch.timer). Deliberately silent and cheap when
		// there is nothing to do, so it can run every minute without filling the journal or the
		// segment. Refuses to act when disarmed, exactly like `apply`.
		if cl == nil {
			return
		}
		need, why := claimNeedsAttention(cl)
		if !need {
			return
		}
		if !claimArmed() {
			fmt.Println("claim tick: " + why + " — NOT ARMED, taking no action")
			return
		}
		fmt.Println("claim tick: " + why + " — re-attempting")
		for _, l := range claimReconcile(cl, false) {
			fmt.Println(" " + l)
		}
		return
	case "arm":
		if err := runPriv("touch", claimArmedFlag); err != nil {
			fmt.Fprintln(os.Stderr, "arm failed:", err)
			os.Exit(1)
		}
		fmt.Println("claim arbitration ARMED (flag " + claimArmedFlag + "; remove it to disarm instantly)")
		return
	case "disarm":
		if err := runPriv("rm", "-f", claimArmedFlag); err != nil {
			fmt.Fprintln(os.Stderr, "disarm failed:", err)
			os.Exit(1)
		}
		fmt.Println("claim arbitration DISARMED")
		return
	}

	if cl == nil {
		fmt.Printf("no claim group on the active pattern (%s) — arbitration inert\n", orDash(st.ActivePattern))
		return
	}
	enforcing, lines := claimEnforcement()
	verdict := "ENFORCING"
	if !enforcing {
		verdict = "NOT ENFORCING"
	}
	fmt.Printf("claim %s   pattern=%s   arbitration: %s\n", cl.Address, st.ActivePattern, verdict)
	for _, l := range lines {
		fmt.Println("   " + l)
	}

	switch sub {
	case "status", "eval":
		for _, l := range claimReconcile(cl, true) {
			fmt.Println(" " + l)
		}
		// The holder is what the arbiter guarantees; the path is what the reader assumes it
		// guarantees. Print both, so "OK" cannot be read as the second one. (2.20)
		for _, l := range claimPaths(cl, currentHolder(cl)) {
			fmt.Println(l)
		}
		// And WHO decides that path. A version says the capability is on disk; this says whether
		// it governs anything. (2.22, c-001)
		for _, l := range governanceLines(st) {
			fmt.Println(" " + l)
		}
		// Liveness: arbitration on carrier edges alone leaves a stranded address stranded when the
		// network goes quiet. Say whether the watchdog that covers that is actually running. (2.25)
		fmt.Println(" " + claimWatchLine())
		if cl.IdentityMAC != "" {
			for _, c := range cl.Claimants {
				mark := "  "
				if devCurrentMAC(c.Dev) == strings.ToLower(cl.IdentityMAC) {
					mark = "->"
				}
				fmt.Printf(" identity: %s %-14s mac=%s perm=%s\n", mark, c.Dev, dash(devCurrentMAC(c.Dev)), dash(devPermMAC(c.Dev)))
			}
		} else {
			fmt.Println(" identity: no identity MAC declared — falling back to lease arbitration," +
				" which takes a losing adapter DOWN and NetworkManager will not autoconnect it back")
		}
	case "apply":
		if !claimArmed() {
			fmt.Println(" refusing to apply: not armed (netgov claim arm) — showing the plan instead")
			for _, l := range claimReconcile(cl, true) {
				fmt.Println(" " + l)
			}
			return
		}
		for _, l := range claimReconcile(cl, false) {
			fmt.Println(" " + l)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: netgov claim [status|eval|apply|set|clear|arm|disarm]")
		os.Exit(1)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// claimWatchLine reports whether the LIVENESS timer is running — the same conjunction discipline
// as claimEnforcement. Arbitration that only fires on carrier edges cannot recover a stranded
// address on a quiet network, so "armed and enforcing" is not the whole story: it says what
// happens when something CHANGES, and this says what happens when nothing does.
func claimWatchLine() string {
	out, err := run("systemctl", "is-active", "netgov-claim-watch.timer")
	switch {
	case err == nil && strings.TrimSpace(out) == "active":
		return "liveness: claim-watch timer ACTIVE — a stranded address self-recovers within ~60s"
	case strings.TrimSpace(out) == "inactive" || strings.TrimSpace(out) == "failed":
		return "liveness: claim-watch timer NOT RUNNING — arbitration fires on carrier events ONLY," +
			" so a failed claim on a quiet network stays stranded (`netgov install` to fix)"
	default:
		return "liveness: claim-watch timer NOT INSTALLED — arbitration fires on carrier events ONLY," +
			" so a failed claim on a quiet network stays stranded (`netgov install` to fix)"
	}
}

// applyMACOps executes an identity-MAC plan IN ORDER (losers release before the winner claims),
// writing NetworkManager's cloned-mac-address rather than raw `ip link` so the change is
// declarative, survives NM reasserting itself, and is restored by `netgov reset`.
//
// Each op needs the profile cycled for the MAC to take effect — a MAC cannot change on a live
// interface. That is a real gap on the leg being changed, which is precisely why the loser goes
// first and why every claimant keeps its own reservation: the box is never without an address.
func applyMACOps(cl *Claim, ops []macOp) []string {
	var log []string
	for _, o := range ops {
		prof := devProfile(o.Dev)
		if prof == "" {
			log = append(log, "WARN: no active profile on "+o.Dev+" — skipped ("+o.Why+")")
			continue
		}
		key := "802-3-ethernet.cloned-mac-address"
		if devIsWireless(o.Dev) {
			key = "802-11-wireless.cloned-mac-address"
		}
		// Record the pre-netgov value ONCE so `reset` restores it (nmprops.go rules apply).
		if st := loadState(statePath()); st != nil {
			if curv, ok := nmGet(prof, key); ok && nmRecordOriginal(st, prof, key, curv) {
				saveStateKeepOwner(st, statePath())
			}
		}
		if err := runPriv("nmcli", "connection", "modify", prof, key, o.Set); err != nil {
			log = append(log, "ABORT: could not set "+key+" on "+prof+" ("+err.Error()+") — nothing further attempted")
			_ = recordClaimFailure(cl.Address)
			return log
		}
		// `connection up` re-activates in place; the MAC applies on activation. It CANNOT succeed
		// on a leg with no carrier — which is the ordinary failover case, a cable pulled out. That
		// is not a failure: `connection modify` has already written the MAC, so it takes effect
		// whenever the link returns, and a leg that cannot transmit cannot collide with anything
		// meanwhile. Treating it as fatal would abort the swap before the winner ever took the
		// identity, i.e. failover would work only when the failed leg was not really failed.
		if err := runPriv("nmcli", "connection", "up", prof); err != nil {
			if !devCarrier(o.Dev) {
				log = append(log, "staged: "+o.Why+" (no carrier on "+o.Dev+
					" — the MAC is written and applies when the link returns)")
				continue
			}
			log = append(log, "ABORT: "+prof+" did not come back up after the MAC change ("+err.Error()+
				") and it still has carrier — refusing to continue with a possible MAC conflict")
			_ = recordClaimFailure(cl.Address)
			return log
		}
		log = append(log, "applied: "+o.Why)
	}

	// Verify the EFFECT, not the exit codes. Seven nmcli commands returning 0 is what "applied
	// (11 cmds, 0 errors)" looked like on the night this file learned not to trust them.
	for i := 0; i < claimAcquireTimeout; i++ {
		if devCurrentMAC(macWinner(ops)) == strings.ToLower(cl.IdentityMAC) && devHoldsAddr(macWinner(ops), cl.Address) {
			clearClaimFailure()
			// Invariant 4: the segment must be TOLD, not assumed — and after a MAC move the
			// peers' caches are wrong in the most confusing way possible, mapping the identity
			// address to an adapter that no longer wears it.
			_ = runPriv("arping", "-U", "-c", "3", "-I", macWinner(ops), cl.Address)
			return append(log, "APPLIED: "+macWinner(ops)+" wears "+cl.IdentityMAC+" and holds "+cl.Address)
		}
		time.Sleep(time.Second)
	}
	_ = recordClaimFailure(cl.Address)
	return append(log, "FAILED: "+macWinner(ops)+" did not end up holding "+cl.Address+" within "+
		strconv.Itoa(claimAcquireTimeout)+"s. Nothing was taken down and every claimant keeps its own address, "+
		"so the box remains reachable — check the router reserves "+cl.Address+" to "+cl.IdentityMAC+".")
}

// macWinner returns the device the plan ends on — the one that takes the identity.
func macWinner(ops []macOp) string {
	if len(ops) == 0 {
		return ""
	}
	return ops[len(ops)-1].Dev
}

// fromDispatcher reports whether we were invoked by NetworkManager's dispatcher, where a
// `connection modify` re-enters NM and fails. The generated hook passes the flag explicitly;
// NM's own environment is the fallback so an OLD hook (installed before this build) is still
// recognised — otherwise upgrading the binary without re-running `netgov install` would leave
// exactly the failure this detects.
func fromDispatcher() bool {
	for _, a := range os.Args {
		if a == "--from-dispatcher" {
			return true
		}
	}
	return os.Getenv("NM_DISPATCHER_ACTION") != "" || os.Getenv("CONNECTION_UUID") != ""
}
