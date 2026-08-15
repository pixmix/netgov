package main

import "testing"

// The 2.29 fix, and the reason it is a correctness fix rather than a tidy-up.
//
// Measured on .153, 2026-08-15 03:44, in the dispatcher log:
//
//	! ip route add default via 192.168.222.1 dev enp114s0 table 100 -> Error: Nexthop has invalid gateway.
//	netgov: applied (13 cmds, 1 errors)
//
// Thirteen, not fourteen: the on-link route was skipped because linkNet asked the ROUTE table for a
// prefix NetworkManager had not installed yet (`noprefixroute` — NM adds the address and the prefix
// route as two separate steps), while uplinkLive had already gated on the ADDRESS from the first
// step. Asking the address instead removes the disagreement at its source, because it is the same
// observation the caller already made.
func TestParseLinkNet_DerivesThePrefixFromTheAddressNotTheRouteTable(t *testing.T) {
	const out = `[{"ifname":"enp114s0","addr_info":[
		{"family":"inet","local":"192.168.222.153","prefixlen":24,"scope":"global"}]}]`
	got := parseLinkNet(out, "4")
	// Masked to the NETWORK. The unmasked 192.168.222.153/24 as a destination would not cover the
	// gateway the way the default route added next needs it to.
	if got != "192.168.222.0/24" {
		t.Fatalf("want the masked prefix 192.168.222.0/24, got %q", got)
	}
}

// A /32 or an address with no prefix must not be turned into a bogus on-link route.
func TestParseLinkNet_IgnoresWhatItCannotUse(t *testing.T) {
	for _, c := range []struct{ name, out, fam string }{
		{"no addresses at all", `[{"ifname":"eth0","addr_info":[]}]`, "4"},
		{"link-local only is not a global prefix",
			`[{"ifname":"eth0","addr_info":[{"family":"inet","local":"169.254.3.4","prefixlen":16,"scope":"link"}]}]`, "4"},
		{"a v6 ULA is not routable and must be skipped, matching devSrc",
			`[{"ifname":"eth0","addr_info":[{"family":"inet6","local":"fd00::1","prefixlen":64,"scope":"global"}]}]`, "6"},
		{"wrong family", `[{"ifname":"eth0","addr_info":[{"family":"inet6","local":"2001:db8::1","prefixlen":64,"scope":"global"}]}]`, "4"},
		{"unparseable", `not json`, "4"},
	} {
		if got := parseLinkNet(c.out, c.fam); got != "" {
			t.Errorf("%s: want no prefix, got %q", c.name, got)
		}
	}
	// ...but a real global v6 prefix IS usable.
	const v6 = `[{"ifname":"eth0","addr_info":[{"family":"inet6","local":"2001:db8::5","prefixlen":64,"scope":"global"}]}]`
	if got := parseLinkNet(v6, "6"); got != "2001:db8::/64" {
		t.Errorf("want 2001:db8::/64, got %q", got)
	}
}

// An error COUNT is not a consequence. "applied (13 cmds, 1 errors)" was the only signal for a
// table left empty behind a live rule for eleven hours — the same line whether the failure is
// harmless or total, which is what makes it ignorable. Assert the wording names what stopped
// working and how to check it, because arranging the real failure means breaking a live link.
func TestTableGapLine_NamesTheConsequenceNotTheCount(t *testing.T) {
	got := tableGapLine("cable", "192.168.222.153", 100, "4")
	for _, want := range []string{
		"table 100 has NO default",              // the state
		"from 192.168.222.153 table 100",        // the rule that makes it matter
		"falls through to main and still works", // why nobody notices
		"NOT in effect",                         // the actual loss
		"netgov apply",                          // the remedy
	} {
		if !contains(got, want) {
			t.Errorf("the warning must contain %q; got %q", want, got)
		}
	}
}
