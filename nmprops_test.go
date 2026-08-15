package main

import "testing"

// These exist because 2.21 gives up netgov's strongest safety property — "never writes an NM
// profile, so reset is always a complete restore" — and buys it back with save-and-restore. The
// whole trade depends on ONE behaviour: the original is recorded exactly once.
//
// If it re-records, the second apply saves netgov's own value as the pristine one. Reset then runs,
// reports success, restores nothing, and the box keeps netgov's routing forever. NOTHING
// OBSERVABLE ON A RUNNING SYSTEM DISTINGUISHES THAT from a working restore — the command succeeds,
// the property is set, the log is clean. It is failure #6 of n-228 exactly, in the mechanism whose
// correctness is the reason the operator allowed profile writes at all.

func boolp(b bool) *bool { return &b }

func TestNMRecordOriginal_RecordsOnceAndNeverAgain(t *testing.T) {
	st := &State{}

	if !nmRecordOriginal(st, "eth-lan", "ipv4.route-metric", "-1") {
		t.Fatal("first record must save the pre-netgov value")
	}
	// netgov now writes 50. A later apply sees 50 as "current" and must NOT save it.
	if nmRecordOriginal(st, "eth-lan", "ipv4.route-metric", "50") {
		t.Fatal("second record must be a no-op — saving netgov's own value makes reset restore nothing")
	}
	if nmRecordOriginal(st, "eth-lan", "ipv4.route-metric", "60") {
		t.Fatal("third record must be a no-op too")
	}
	if n := len(st.NMSaved); n != 1 {
		t.Fatalf("want exactly 1 save record, got %d: %+v", n, st.NMSaved)
	}
	if got := st.NMSaved[0].Original; got != "-1" {
		t.Fatalf("the saved original must still be the PRE-netgov value; got %q, want %q", got, "-1")
	}
}

// Distinct properties on one profile are independent — taking over the metric must not make the
// never-default record look already-saved.
func TestNMRecordOriginal_PerProfileAndProperty(t *testing.T) {
	st := &State{}
	nmRecordOriginal(st, "eth-lan", "ipv4.route-metric", "1000")
	nmRecordOriginal(st, "eth-lan", "ipv4.never-default", "yes")
	nmRecordOriginal(st, "CNNet", "ipv4.route-metric", "-1")
	if len(st.NMSaved) != 3 {
		t.Fatalf("want 3 independent records, got %d: %+v", len(st.NMSaved), st.NMSaved)
	}
}

// "" and "-1" are both how nmcli spells automatic for route-metric. If they compare unequal, every
// apply believes it has work to do — and this code runs from the NM dispatcher on carrier events,
// so a never-idempotent plan is a reapply loop. That is the shape of the outage this box had on
// 2026-08-13, so it is asserted rather than assumed.
func TestNMEquivalent_AutomaticSpellings(t *testing.T) {
	for _, c := range []struct {
		prop, cur, want string
		equal           bool
	}{
		{"ipv4.route-metric", "-1", "", true},
		{"ipv4.route-metric", "", "-1", true},
		{"ipv4.route-metric", "50", "50", true},
		{"ipv4.route-metric", "1000", "50", false},
		{"ipv4.never-default", "yes", "yes", true},
		{"ipv4.never-default", "yes", "no", false},
		// never-default has no automatic spelling: "" and "no" are NOT interchangeable here, and
		// treating them as such would silently skip a real change.
		{"ipv4.never-default", "", "no", false},
	} {
		if got := nmEquivalent(c.prop, c.cur, c.want); got != c.equal {
			t.Errorf("nmEquivalent(%q, %q, %q) = %v, want %v", c.prop, c.cur, c.want, got, c.equal)
		}
	}
}

// nil CanDefault means UNMANAGED and must produce no desired change. If nil were treated as false,
// netgov would take over never-default on every uplink the moment it was upgraded — adopting
// properties nobody asked it to hold, and then "restoring" values it invented.
func TestUplinkRoutingDesired_NilCanDefaultIsUnmanaged(t *testing.T) {
	st := &State{Uplinks: []Uplink{{Name: "cable", Dev: "definitely-not-a-real-dev-0"}}}
	for _, d := range uplinkRoutingDesired(st) {
		if d.Prop == "ipv4.never-default" {
			t.Fatalf("nil CanDefault must not produce a never-default change; got %+v", d)
		}
	}
}

// Upgrading the binary must not, on its own, hand netgov a host property. applyUplinkRouting runs
// from the NM dispatcher on carrier events, so a default-on ManageMetrics would take over
// ipv4.route-metric at the next link flap on every box that upgraded, with nobody having asked.
// Observation would not catch a wrong default here — it looks like the feature working.
func TestUplinkRoutingDesired_MetricsAreOptInAndOffByDefault(t *testing.T) {
	st := &State{
		ActivePattern: "LH",
		Patterns: []Pattern{{Name: "LH", Claim: &Claim{Address: "192.168.222.153",
			Claimants: []Claimant{{Dev: "enp114s0", Priority: 100}, {Dev: "wlo1", Priority: 50}}}}},
	}
	for _, d := range uplinkRoutingDesired(st) { // ManageMetrics nil = never set
		if d.Prop == "ipv4.route-metric" {
			t.Fatalf("metrics must be opt-in; a fresh upgrade produced %+v", d)
		}
	}
	off := false
	st.ManageMetrics = &off
	for _, d := range uplinkRoutingDesired(st) {
		if d.Prop == "ipv4.route-metric" {
			t.Fatalf("explicit off must also produce nothing; got %+v", d)
		}
	}
}

// The metric band must put a claimant ahead of NetworkManager's automatic values (ethernet 100,
// wifi 600), or declaring a claim priority changes nothing on a box whose other adapters are at
// the defaults — the tool would report a preference it never actually expressed.
func TestClaimMetricBand_OutranksNMAutomatic(t *testing.T) {
	const nmAutoEthernet, nmAutoWifi = 100, 600
	for rank := 0; rank < 3; rank++ {
		m := nmMetricBase + rank*nmMetricStep
		if m >= nmAutoEthernet {
			t.Errorf("rank %d metric %d does not outrank NM's automatic ethernet (%d)", rank, m, nmAutoEthernet)
		}
		if m >= nmAutoWifi {
			t.Errorf("rank %d metric %d does not outrank NM's automatic wifi (%d)", rank, m, nmAutoWifi)
		}
	}
	// And ordering must follow priority: rank 0 (highest claim priority) gets the lowest metric.
	if nmMetricBase >= nmMetricBase+nmMetricStep {
		t.Fatal("metric must increase with rank, so higher claim priority wins the route")
	}
}

func TestCanDefaultStr_AutoIsDistinctFromYes(t *testing.T) {
	if canDefaultStr(nil) == canDefaultStr(boolp(true)) {
		t.Fatal("unmanaged must not render the same as managed-yes — they are different states")
	}
	if canDefaultStr(boolp(true)) == canDefaultStr(boolp(false)) {
		t.Fatal("yes and no must differ")
	}
}

// A "restore" that INSTALLS a netgov artefact is the worst failure this file can have — it fires
// exactly when someone is already recovering from something else. Found live on .153 on
// 2026-08-15: state.json held `4A\:21\:0B\:6E\:06\:85` as eth-lan's pre-netgov cloned-mac-address,
// which is netgov's own parked MAC, so `netgov reset` would have pinned the cable leg to a MAC the
// router does not reserve .153 to. No measurement could have caught this: the wrong value is only
// read during a reset, and the reset would have looked like it worked.
func TestSavedMACIsOurs_RejectsAnIdentityMACRecordedAsAUserBaseline(t *testing.T) {
	st := &State{Patterns: []Pattern{{
		Name:  "LH",
		Claim: &Claim{Address: "192.168.222.153", IdentityMAC: "48:21:0b:6e:06:85"},
	}}}
	// nmcli's terse output escapes the colons; a MAC must not compare unequal to itself.
	drop, why := savedMACIsOurs(st, NMSaved{
		Profile: "eth-lan", Prop: "802-3-ethernet.cloned-mac-address",
		Original: `48\:21\:0B\:6E\:06\:85`,
	})
	if !drop {
		t.Fatalf("the identity MAC is netgov's own value and must never be restored as a baseline; why=%q", why)
	}
}

// The other half: a value netgov never writes is the user's, and dropping it would be netgov
// deciding it knows better than a setting it did not make.
func TestSavedMACIsOurs_KeepsAValueNetgovNeverWrites(t *testing.T) {
	st := &State{Patterns: []Pattern{{
		Name:  "LH",
		Claim: &Claim{Address: "192.168.222.153", IdentityMAC: "48:21:0b:6e:06:85"},
	}}}
	for _, v := range []string{"random", "stable", "02:11:22:33:44:55"} {
		if drop, _ := savedMACIsOurs(st, NMSaved{
			Profile: "eth-lan", Prop: "802-3-ethernet.cloned-mac-address", Original: v,
		}); drop {
			t.Errorf("%q is a user setting, not netgov's — it must be restored verbatim", v)
		}
	}
	// And nothing outside cloned-mac-address is this function's business.
	if drop, _ := savedMACIsOurs(st, NMSaved{
		Profile: "eth-lan", Prop: "ipv4.never-default", Original: "yes",
	}); drop {
		t.Error("never-default is not a MAC and must be left alone")
	}
}

// honestMACBaseline is the record-time half. It must not depend on a permanent-MAC lookup to
// recognise the identity, because the identity is knowable from the claim alone.
func TestHonestMACBaseline_WillNotRecordTheIdentityAsAnOriginal(t *testing.T) {
	cl := &Claim{Address: "192.168.222.153", IdentityMAC: "48:21:0b:6e:06:85"}
	if got := honestMACBaseline(`48\:21\:0B\:6E\:06\:85`, cl, "enp114s0"); got != "" {
		t.Fatalf("recording netgov's own identity as the user baseline is how the .153 record went wrong; got %q", got)
	}
	if got := honestMACBaseline("random", cl, "enp114s0"); got != "random" {
		t.Fatalf("a user's own clone setting must record verbatim; got %q", got)
	}
	if got := honestMACBaseline("", cl, "enp114s0"); got != "" {
		t.Fatalf("unset records as unset; got %q", got)
	}
}
