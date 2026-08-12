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
	"sort"
	"strconv"
	"strings"
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

// claimantEligible applies invariant 2 and nothing else.
func claimantEligible(c Claimant) (bool, string) {
	if !devCarrier(c.Dev) {
		return false, "no carrier"
	}
	if len(c.SSIDs) == 0 {
		return true, "carrier up"
	}
	if !devIsWireless(c.Dev) {
		// Config error rather than a state: an SSID list on a wired adapter can never be
		// satisfied meaningfully. Carrier is the whole test; say so loudly instead of
		// silently comparing against whatever nmcli happens to return.
		return true, "carrier up (SSID list ignored — " + c.Dev + " is not wireless)"
	}
	cur := devSSID(c.Dev)
	if cur == "" {
		return false, "not associated"
	}
	for _, s := range c.SSIDs {
		if strings.EqualFold(strings.TrimSpace(s), cur) {
			return true, "associated to " + cur
		}
	}
	return false, "associated to " + cur + " (not a listed SSID)"
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

// devProfile returns the NM connection profile currently active on dev, or "".
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
	for _, c := range cs {
		ok, why := claimantEligible(c)
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
	// 4. Gratuitous ARP (invariant 4) — the segment must be told, not assumed. Verify from a
	//    third box; this only announces.
	if devHoldsAddr(v.Winner, cl.Address) {
		if err := runPriv("arping", "-U", "-c", "3", "-I", v.Winner, cl.Address); err != nil {
			log = append(log, "WARN: gratuitous ARP failed ("+err.Error()+") — peers may still cache the old MAC")
		} else {
			log = append(log, "gratuitous ARP sent on "+v.Winner+" for "+cl.Address)
		}
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

	cl := &Claim{Address: addr}
	seen := map[string]bool{}
	for _, s := range specs {
		c, err := parseClaimant(s)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if seen[c.Dev] {
			fmt.Fprintf(os.Stderr, "claimant %s listed twice — one entry per adapter\n", c.Dev)
			os.Exit(1)
		}
		seen[c.Dev] = true
		cl.Claimants = append(cl.Claimants, c)
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
	fmt.Printf("claim %s   pattern=%s   armed=%v\n", cl.Address, st.ActivePattern, claimArmed())

	switch sub {
	case "status", "eval":
		for _, l := range claimReconcile(cl, true) {
			fmt.Println(" " + l)
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
