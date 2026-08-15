package main

import "testing"

// The 2026-07-05 bug, fixed 2026-08-15. It survived five weeks for a reason worth recording: this
// box has had no global IPv6 in that whole time, so NOTHING on it could reproduce the case. It was
// verified live once, on a German Vodafone line, and then became unreachable to every observation
// available here. That is the exact shape "only a test catches an absent one" was written for.
//
// Real `ip -j addr show` output, with the ULA prefix swapped for a global one. Note all three
// coexist: a DHCPv6 /128, the rotating `temporary`, and the stable `mngtmpaddr` — the last two on
// the SAME /64.
const v6Addrs = `[{"ifname":"enp114s0","addr_info":[
 {"family":"inet6","local":"2a01:599:a1b:2c00::275","prefixlen":128,"scope":"global","dynamic":true,"noprefixroute":true},
 {"family":"inet6","local":"2a01:599:a1b:2c00:c055:97a:6906:8a07","prefixlen":64,"scope":"global","temporary":true,"dynamic":true},
 {"family":"inet6","local":"2a01:599:a1b:2c00:8805:48b:ecc6:f96f","prefixlen":64,"scope":"global","dynamic":true,"mngtmpaddr":true,"noprefixroute":true},
 {"family":"inet6","local":"fe80::28ec:1138:3380:275a","prefixlen":64,"scope":"link","noprefixroute":true}]}]`

// THE FIX. A rule pinned to one v6 address matches whichever address netgov happened to see first
// and misses the other — and RFC 6724 tells applications to prefer the TEMPORARY one, so the miss
// hits real traffic, not just the probe. Traffic matching no source rule falls through to the
// priority-29000 blackhole that implements v6=block, and the dashboard reported "no internet" on a
// working uplink.
func TestV6SrcPrefixes_CoversBothTheStableAndTheTemporaryAddress(t *testing.T) {
	got := v6SrcPrefixes(v6Addrs)
	want := map[string]bool{"2a01:599:a1b:2c00::/64": true, "2a01:599:a1b:2c00::275/128": true}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected prefix %q", p)
		}
		delete(want, p)
	}
	if len(want) != 0 {
		t.Fatalf("missing prefixes %v; got %v", want, got)
	}
	// The point of the whole change: ONE rule now covers stable and temporary alike, because they
	// share a /64. Fixing the source-address choice alone would have made the probe pass while
	// applications kept being blackholed — a worse outcome than the visible bug.
	//
	// Assert PRESENCE, not position. Both selectors point at the same table, so their relative
	// order changes nothing — an earlier version of this test demanded the /64 come first and
	// failed a correct implementation. A test that pins down more than the behaviour requires is
	// a future false alarm.
	var covers64 bool
	for _, p := range got {
		if p == "2a01:599:a1b:2c00::/64" {
			covers64 = true
		}
	}
	if !covers64 {
		t.Fatalf("no rule covers the /64 shared by the stable and temporary addresses; got %v", got)
	}
}

func TestV6SrcPrefixes_SkipsULAAndLinkLocalAndDedupes(t *testing.T) {
	const ula = `[{"addr_info":[
	 {"family":"inet6","local":"fd92:daa:d131:0:c055::1","prefixlen":64,"scope":"global","temporary":true},
	 {"family":"inet6","local":"fe80::1","prefixlen":64,"scope":"link"}]}]`
	if got := v6SrcPrefixes(ula); len(got) != 0 {
		t.Fatalf("ULA is not routable and link-local is not global; got %v", got)
	}
	const dup = `[{"addr_info":[
	 {"family":"inet6","local":"2a01:599:a1b:2c00::1","prefixlen":64,"scope":"global"},
	 {"family":"inet6","local":"2a01:599:a1b:2c00::2","prefixlen":64,"scope":"global","temporary":true}]}]`
	if got := v6SrcPrefixes(dup); len(got) != 1 {
		t.Fatalf("two addresses on one prefix is ONE rule; got %v", got)
	}
}

// The reporting half: a rotating address shown in the UI is true for about an hour, and a probe
// bound to one has an expiry date. Prefer the stable address — but never at the cost of reporting
// nothing when only a temporary exists.
func TestPickSrc_PrefersTheStableV6AddressOverTheRotatingOne(t *testing.T) {
	if got := pickSrc(v6Addrs, "6"); got != "2a01:599:a1b:2c00::275" {
		t.Fatalf("want the first stable global (the DHCPv6 /128 here), got %q", got)
	}
	const tempOnly = `[{"addr_info":[
	 {"family":"inet6","local":"2a01:599:a1b:2c00:c055::9","prefixlen":64,"scope":"global","temporary":true}]}]`
	if got := pickSrc(tempOnly, "6"); got != "2a01:599:a1b:2c00:c055::9" {
		t.Fatalf("a temporary address is usable and beats reporting nothing; got %q", got)
	}
}

// A deprecated address is being retired and can stop working mid-probe, which would read as a link
// fault. Skip it in both families.
func TestPickSrc_SkipsDeprecated(t *testing.T) {
	const dep = `[{"addr_info":[
	 {"family":"inet","local":"192.168.222.9","prefixlen":24,"scope":"global","deprecated":true},
	 {"family":"inet","local":"192.168.222.153","prefixlen":24,"scope":"global"}]}]`
	if got := pickSrc(dep, "4"); got != "192.168.222.153" {
		t.Fatalf("want the live address, got %q", got)
	}
}

// v4 numbering must not move: it is the identity pin, it is what the live box already has, and
// churning it would rewrite working rules on every host for no reason.
func TestSrcSelPri_V4NumberingUnchangedAndV6CannotCollide(t *testing.T) {
	if got := (srcSel{}).Pri(100); got != 20000 {
		t.Fatalf("v4 table 100 must stay at 20000, got %d", got)
	}
	if got := (srcSel{}).Pri(102); got != 20002 {
		t.Fatalf("v4 table 102 must stay at 20002, got %d", got)
	}
	// v6 gets ten slots per uplink, so the last slot of one table must stay below the first of the
	// next — otherwise two uplinks' rules would fight over one priority.
	if last, next := (srcSel{idx: 9, v6: true}).Pri(100), (srcSel{idx: 0, v6: true}).Pri(101); last >= next {
		t.Fatalf("v6 priorities collide across uplinks: table100[9]=%d, table101[0]=%d", last, next)
	}
	if top := (srcSel{idx: 9, v6: true}).Pri(198); top > ownedPriHi {
		t.Fatalf("the highest v6 priority %d escapes the owned band (<=%d)", top, ownedPriHi)
	}
}
