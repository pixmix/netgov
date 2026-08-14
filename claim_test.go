package main

import (
	"os"
	"testing"
)

// The first tests in this repo, and they exist because of a rule this project wrote:
//
//	"Measurement catches a wrong value. Only a test catches an absent one."
//
// The 2.20 change is a REPORT. Its entire purpose is to make something appear in output that
// previously did not, and the condition it reports on requires two adapters holding two addresses
// on one subnet at different route metrics. Nobody has that arranged when they edit this file, so
// "run it and look" verifies the case that was already fine and never touches the one that
// matters. That is precisely the shape of failure #6 in n-228 — a fix whose effect did not exist,
// invisible to every observation available on the running system.

func has(t *testing.T, lines []string, want string) bool {
	t.Helper()
	for _, l := range lines {
		if contains(l, want) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The condition c-019 measured on .153 on 2026-08-14: the arbiter is correct, the holder is
// correct, and the host talks on the other adapter as a different address.
func TestPathLines_ReportsSplitWhenHolderIsNotThePath(t *testing.T) {
	got := pathLines("192.168.222.153", "enp114s0", pathObs{
		Gateway:  "192.168.222.1",
		OnDev:    "wlo1",
		OnSrc:    "192.168.222.238",
		OffDev:   "wlo1",
		OffSrc:   "192.168.222.238",
		BoundDev: "wlo1",
	})
	if !has(t, got, "HOLDS is not PATH") {
		t.Fatalf("a holder that is not the path must be reported; got %q", got)
	}
	// The consequence has to be checkable from the OTHER end, or it is not actionable.
	if !has(t, got, "peers see 192.168.222.238, not 192.168.222.153") {
		t.Errorf("the warning must name the address peers actually see; got %q", got)
	}
	if !has(t, got, "bound to 192.168.222.153") {
		t.Errorf("must say the bound-socket path still works, or this reads as 'address unusable'; got %q", got)
	}
}

// The healthy case must NOT warn — a report that cries wolf on every box teaches operators to
// skip it, which costs more than never having written it.
func TestPathLines_SilentWhenHolderIsThePath(t *testing.T) {
	got := pathLines("192.168.222.153", "enp114s0", pathObs{
		Gateway: "192.168.222.1",
		OnDev:   "enp114s0",
		OnSrc:   "192.168.222.153",
		OffDev:  "enp114s0",
		OffSrc:  "192.168.222.153",
	})
	if has(t, got, "HOLDS is not PATH") {
		t.Fatalf("must not warn when the holder IS the path; got %q", got)
	}
	if !has(t, got, "holder is the path") {
		t.Errorf("the healthy case still has to SAY so — silence is indistinguishable from not checking; got %q", got)
	}
}

// A split on the off-link path alone is NOT a finding: `ipv4.never-default` on the wired profile
// is a deliberate guard on this very box, keeping a cable to the router from capturing the
// session's internet lifeline. Warning about it would be the tool second-guessing a policy it
// knows nothing about.
func TestPathLines_OffLinkDifferenceAloneIsNotAFinding(t *testing.T) {
	got := pathLines("192.168.222.153", "enp114s0", pathObs{
		Gateway: "192.168.222.1",
		OnDev:   "enp114s0",
		OnSrc:   "192.168.222.153",
		OffDev:  "wlo1", // internet leaves via wifi ON PURPOSE
		OffSrc:  "192.168.222.238",
	})
	if has(t, got, "HOLDS is not PATH") {
		t.Fatalf("a deliberate never-default wired leg must not be reported as a split; got %q", got)
	}
}

// Losing the ability to measure is not evidence of a fault — invariant 4, the same rule the
// gateway probe follows when arping is missing.
func TestPathLines_UnmeasurableIsNotAFinding(t *testing.T) {
	got := pathLines("192.168.222.153", "enp114s0", pathObs{Gateway: "192.168.222.1"})
	if has(t, got, "HOLDS is not PATH") {
		t.Fatalf("no route answer must not produce a verdict; got %q", got)
	}
}

// The standby-on-segment condition is reportable even when the routes happen to fall the right
// way — being on the segment is the hazard, not the current metric. This one would never be
// caught by running the tool, because arranging it means giving a standby its own lease.
func TestPathLines_StandbyOnGuardedSubnetIsReportedEvenWhenPathIsCorrect(t *testing.T) {
	got := pathLines("192.168.222.153", "enp114s0", pathObs{
		Gateway: "192.168.222.1",
		OnDev:   "enp114s0",
		OnSrc:   "192.168.222.153",
		Extra:   []string{"wlo1 192.168.222.239"},
	})
	if !has(t, got, "standby on the guarded subnet: wlo1 192.168.222.239") {
		t.Fatalf("a standby holding its own address on the guarded subnet must be reported; got %q", got)
	}
	if has(t, got, "HOLDS is not PATH") {
		t.Errorf("that is a separate condition and must not be conflated with a path split; got %q", got)
	}
}

func TestRouteGetParse(t *testing.T) {
	// `ip route get X from Y` echoes the pin and prints NO src. Reporting an empty source there
	// would read as "no source address", which is a different and alarming claim.
	for _, c := range []struct{ out, from, dev, src string }{
		{"192.168.222.1 dev wlo1 src 192.168.222.238 uid 1000", "", "wlo1", "192.168.222.238"},
		{"1.1.1.1 via 192.168.222.1 dev wlo1 src 192.168.222.238 uid 1000", "", "wlo1", "192.168.222.238"},
		{"1.1.1.1 from 192.168.222.153 via 192.168.222.1 dev enp114s0 table 100 uid 1000", "192.168.222.153", "enp114s0", "192.168.222.153"},
	} {
		dev, src := parseRouteGet(c.out, c.from)
		if dev != c.dev || src != c.src {
			t.Errorf("parseRouteGet(%q, %q) = (%q, %q), want (%q, %q)", c.out, c.from, dev, src, c.dev, c.src)
		}
	}
}

// The liveness gap c-001 measured: a failed claim plus a quiet network left .153 held by NOBODY
// for 19 minutes, because arbitration only runs on carrier edges and no edge arrived. These assert
// the pre-check that decides whether the timer does anything — and it is exactly the kind of logic
// you cannot check by watching a healthy box, because a healthy box never enters the state.

func claimOf(holderHasAddr string) *Claim {
	return &Claim{Address: "192.168.222.186", Claimants: []Claimant{
		{Dev: "eno1", Priority: 100}, {Dev: "wlp0s20f3", SSIDs: []string{"CNNet"}, Priority: 50}}}
}

// Nobody holds it. This is the measured 19-minute failure and it MUST trigger a re-attempt.
func TestClaimNeedsAttention_StrandedAddressTriggers(t *testing.T) {
	cl := claimOf("")
	// Devices named here do not exist on the test host, so currentHolder finds no holder —
	// which is precisely the stranded state.
	need, why := claimNeedsAttention(cl)
	if !need {
		t.Fatalf("an address held by nobody must trigger a re-attempt; got no-op (%s)", why)
	}
	if !contains(why, "STRANDED") {
		t.Errorf("the reason must name the condition so a log line is actionable; got %q", why)
	}
}

// The pre-check must never be the thing that DECIDES a move — it only decides whether the
// expensive probe is worth running. Carrier is not health (the invariant this project paid for
// twice), so a carrier-up higher-priority leg is a reason to LOOK, not a reason to move.
func TestClaimNeedsAttention_IsAGateNotAVerdict(t *testing.T) {
	// A claim whose only claimant is absent: no holder, so it asks for a re-attempt. The point is
	// that claimNeedsAttention returns a REASON TO EVALUATE; claimEvaluate still runs the probe
	// and can decline. If this ever short-circuits to an action, the loss-fraction gate is bypassed.
	cl := &Claim{Address: "10.99.99.99", Claimants: []Claimant{{Dev: "nosuchdev0", Priority: 10}}}
	need, _ := claimNeedsAttention(cl)
	if !need {
		t.Fatal("no holder must ask for evaluation")
	}
	// And the verdict path must still be the probe-driven one: an absent device cannot be eligible.
	v := claimEvaluate(cl)
	if v.Winner != "" {
		t.Fatalf("an absent device must not win on the pre-check's say-so; got winner %q", v.Winner)
	}
}

// Identity-MAC failover. The ORDER is the safety property: two NICs wearing one MAC on one segment
// makes the bridge learn it on two ports and flap, which corrupts the segment's view rather than
// just ours. You cannot check that by running the tool on a healthy box — it never enters the state.

func macMaps() (perm, cur map[string]string) {
	return map[string]string{"eth0": "48:21:0b:6e:06:85", "wlan0": "98:bd:80:ec:68:cd"},
		map[string]string{"eth0": "48:21:0b:6e:06:85", "wlan0": "98:bd:80:ec:68:cd"}
}

func idClaim() *Claim {
	return &Claim{Address: "192.168.222.153", IdentityMAC: "48:21:0b:6e:06:85",
		Claimants: []Claimant{{Dev: "eth0", Priority: 100}, {Dev: "wlan0", Priority: 50}}}
}

// Steady state: the wired leg's PERMANENT mac is the identity and it already wears it. No ops.
func TestClaimMACPlan_NoOpWhenWinnerAlreadyWearsIt(t *testing.T) {
	perm, cur := macMaps()
	if ops := claimMACPlan(idClaim(), "eth0", perm, cur); len(ops) != 0 {
		t.Fatalf("steady state must produce no ops; got %+v", ops)
	}
}

// Failover to wifi: wifi must ASSUME the identity, and since eth0 still wears it, eth0 must give
// it up FIRST. Overlap is the fault this ordering exists to prevent.
func TestClaimMACPlan_LoserReleasesBeforeWinnerClaims(t *testing.T) {
	perm, cur := macMaps()
	ops := claimMACPlan(idClaim(), "wlan0", perm, cur)
	if len(ops) != 2 {
		t.Fatalf("want release-then-claim (2 ops); got %+v", ops)
	}
	// The loser's PERMANENT mac IS the identity, so clearing would not release it — it must PARK.
	// This assertion is the bug the operator caught: the original said Set == "".
	if ops[0].Dev != "eth0" || ops[0].Set != "4a:21:0b:6e:06:85" {
		t.Errorf("the FIRST op must PARK the loser off the identity (perm==identity, so clearing is a no-op); got %+v", ops[0])
	}
	if ops[1].Dev != "wlan0" || ops[1].Set != "48:21:0b:6e:06:85" {
		t.Errorf("the SECOND op must be the winner assuming it; got %+v", ops[1])
	}
}

// Failing back: wifi currently wears it, eth0 wins. eth0's PERMANENT mac is the identity, so the
// clone must be CLEARED rather than pinned — a profile should carry no override it does not need.
func TestClaimMACPlan_WinnerWithPermanentIdentityClearsRatherThanPins(t *testing.T) {
	perm, cur := macMaps()
	cur["wlan0"] = "48:21:0b:6e:06:85" // wifi is currently the identity
	cur["eth0"] = "48:21:0b:6e:06:85"  // and eth0 still has its permanent one
	ops := claimMACPlan(idClaim(), "eth0", perm, cur)
	if len(ops) != 1 || ops[0].Dev != "wlan0" || ops[0].Set != "" {
		t.Fatalf("want exactly one op: wlan0 releases; got %+v", ops)
	}
}

// No identity MAC declared = the old lease-arbitration path. Must produce nothing rather than
// silently doing half of a mechanism.
func TestClaimMACPlan_InertWithoutAnIdentityMAC(t *testing.T) {
	cl := idClaim()
	cl.IdentityMAC = ""
	perm, cur := macMaps()
	if ops := claimMACPlan(cl, "wlan0", perm, cur); ops != nil {
		t.Fatalf("no identity MAC must mean no ops; got %+v", ops)
	}
}

func TestParseClaimForm_IdentityMAC(t *testing.T) {
	cl, err := parseClaimForm("192.168.222.153 identity=48:21:0B:6E:06:85 enp114s0:100 wlo1:50:CNNet")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cl.IdentityMAC != "48:21:0b:6e:06:85" {
		t.Errorf("identity MAC must be parsed and lowercased; got %q", cl.IdentityMAC)
	}
	if len(cl.Claimants) != 2 {
		t.Errorf("identity= must not be consumed as a claimant; got %d claimants", len(cl.Claimants))
	}
	if got := claimFormString(cl); !contains(got, "identity=48:21:0b:6e:06:85") {
		t.Errorf("must round-trip through the form string; got %q", got)
	}
	if _, err := parseClaimForm("192.168.222.153 identity=nonsense enp114s0:100"); err == nil {
		t.Error("a malformed MAC must be rejected, not silently ignored")
	}
}

// The collision the parked MAC exists to prevent, stated directly: after ANY plan, no two adapters
// may end up on the same MAC. This is the property that actually matters, and the original
// ordering test passed while violating it.
func TestClaimMACPlan_NeverLeavesTwoAdaptersOnOneMAC(t *testing.T) {
	for _, winner := range []string{"eth0", "wlan0"} {
		perm, cur := macMaps()
		if winner == "eth0" {
			cur["wlan0"] = "48:21:0b:6e:06:85" // failback: wifi currently holds the identity
		}
		final := map[string]string{}
		for d, v := range cur {
			final[d] = v
		}
		for _, o := range claimMACPlan(idClaim(), winner, perm, cur) {
			if o.Set == "" {
				final[o.Dev] = perm[o.Dev]
			} else {
				final[o.Dev] = o.Set
			}
		}
		seen := map[string]string{}
		for d, m := range final {
			if prev, dup := seen[m]; dup {
				t.Fatalf("winner=%s left %s and %s both on %s", winner, prev, d, m)
			}
			seen[m] = d
		}
		if final[winner] != "48:21:0b:6e:06:85" {
			t.Errorf("winner=%s must end up on the identity; got %s", winner, final[winner])
		}
	}
}

func TestParkedMAC_SetsLocallyAdministeredBit(t *testing.T) {
	if got := parkedMAC("48:21:0b:6e:06:85"); got != "4a:21:0b:6e:06:85" {
		t.Errorf("parkedMAC = %q, want 4a:21:0b:6e:06:85 (0x48|0x02 = 0x4a)", got)
	}
	// Already locally administered: must stay put rather than drift on every call.
	if got := parkedMAC("4a:21:0b:6e:06:85"); got != "4a:21:0b:6e:06:85" {
		t.Errorf("must be idempotent; got %q", got)
	}
	if got := parkedMAC("nonsense"); got != "" {
		t.Errorf("unparseable input must yield empty, not a bogus MAC; got %q", got)
	}
}

// An unknown permanent MAC must FAIL CLOSED. The first implementation read it from a field this
// box's nmcli rejects, so it came back empty — and empty fell through to "clear", which is exactly
// the collision parking exists to prevent. A silent degradation into the original bug.
func TestClaimMACPlan_RefusesWhenPermanentMACIsUnknown(t *testing.T) {
	perm, cur := macMaps()
	perm["eth0"] = "" // lookup failed
	ops := claimMACPlan(idClaim(), "wlan0", perm, cur)
	if len(ops) != 1 || !ops[0].Refuse {
		t.Fatalf("an unreadable permanent MAC must refuse to plan, not guess; got %+v", ops)
	}
}

// --from-dispatcher must be recognised, because MAC ops from inside NM's dispatcher fail and then
// poison the timer with a cooldown. Asserting the detector rather than the behaviour it guards:
// the guard is only as good as this returning true.
func TestFromDispatcher_RecognisesTheExplicitFlag(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = []string{"netgov", "claim", "apply", "--from-dispatcher", "--state", "/x"}
	if !fromDispatcher() {
		t.Fatal("the explicit flag must be recognised — the generated hook passes it")
	}
	os.Args = []string{"netgov", "claim", "apply", "--state", "/x"}
	if fromDispatcher() && os.Getenv("NM_DISPATCHER_ACTION") == "" && os.Getenv("CONNECTION_UUID") == "" {
		t.Fatal("a hand-run must NOT look like a dispatcher run, or MAC ops never apply at all")
	}
}
