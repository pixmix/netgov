package main

import "testing"

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
