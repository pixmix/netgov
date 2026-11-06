//go:build linux

package main

import "testing"

// stubResolve swaps resolveFam for a fixture; returns the restore func.
func stubResolve(m map[string][]string) func() {
	old := resolveFam
	resolveFam = func(domain, fam string) []string {
		if fam == "6" {
			return nil
		}
		return m[domain]
	}
	return func() { resolveFam = old }
}

// stubUplinkLive forces liveness for the named uplinks (v4); everything else
// falls through to the real implementation (absent devices ⇒ dead, hermetic).
func stubUplinkLive(live map[string]bool) func() {
	old := uplinkLive
	uplinkLive = func(u Uplink, fam string) (string, string, bool) {
		if fam == "4" && live[u.Name] {
			return "10.0.0.7", "", true // repo fixture range (see .githooks/pre-commit)
		}
		return old(u, fam)
	}
	return func() { uplinkLive = old }
}

func findRuleTo(build [][]string, ip string) []string {
	for _, c := range build {
		for i, a := range c {
			if a == "to" && i+1 < len(c) && c[i+1] == ip {
				return c
			}
		}
	}
	return nil
}

func tableOf(cmd []string) string {
	for i, a := range cmd {
		if a == "table" && i+1 < len(cmd) {
			return cmd[i+1]
		}
	}
	return ""
}

// TestPins_SurvivePatternActivation reproduces the NB incident (2026-11-05): a
// pattern activation rewrote st.Rules to null and silently disarmed all fifteen
// model-API pins. Pins live in their own store; activation must not touch them
// and the plan must still carry them afterwards.
func TestPins_SurvivePatternActivation(t *testing.T) {
	defer stubResolve(map[string][]string{
		"api.anthropic.com": {"160.79.104.10"},
		"example.old":       {"10.0.0.9"},
	})()
	defer stubUplinkLive(map[string]bool{"funnel": true})() // lo's 127.x is not a usable src by design; stub liveness
	st := &State{
		Uplinks: []Uplink{{Name: "funnel", Dev: "lo", Table: 150}},
		Rules:   []Rule{{Domain: "example.old", Via: "funnel", Fam: "4"}},
		Pins:    []Pin{{Domain: "api.anthropic.com", Via: "funnel", Fam: "4"}},
	}
	activatePattern(st, &Pattern{Name: "venue", V4: "direct", Rules: nil}) // the wipe
	if len(st.Pins) != 1 {
		t.Fatalf("activatePattern touched the pin store: %v", st.Pins)
	}
	_, build := planFamily(st, "4")
	r := findRuleTo(build, "160.79.104.10")
	if r == nil {
		t.Fatalf("pin did not survive pattern activation; build=%v", build)
	}
	if tableOf(r) != "150" {
		t.Fatalf("pin went to table %s, want the live uplink's 150", tableOf(r))
	}
	if findRuleTo(build, "10.0.0.9") != nil {
		t.Fatal("the wiped rule survived activation — fixture is wrong")
	}
}

// TestPins_FailClosedOnDeadUplink: a pin whose uplink exists in state but is not
// live must realise as UNREACHABLE (the block table), never vanish silently —
// the directive is "no connection or the pinned path", never raw default egress.
func TestPins_FailClosedOnDeadUplink(t *testing.T) {
	defer stubResolve(map[string][]string{"api.anthropic.com": {"160.79.104.10"}})()
	st := &State{
		Uplinks: []Uplink{{Name: "funnel", Dev: "nosuchdev0", Table: 150}},
		Pins:    []Pin{{Domain: "api.anthropic.com", Via: "funnel", Fam: "4"}},
	}
	_, build := planFamily(st, "4")
	r := findRuleTo(build, "160.79.104.10")
	if r == nil {
		t.Fatalf("dead-uplink pin vanished from the plan (silent fall-through); build=%v", build)
	}
	if tableOf(r) != itoa(tableBlock) {
		t.Fatalf("dead-uplink pin went to table %s, want block table %d", tableOf(r), tableBlock)
	}
}

// TestPins_UnknownViaAlsoFailClosed: a pin naming an uplink that no longer exists
// (deleted/renamed) must also realise unreachable, not skip.
func TestPins_UnknownViaAlsoFailClosed(t *testing.T) {
	defer stubResolve(map[string][]string{"api.z.ai": {"47.246.13.13"}})()
	st := &State{Pins: []Pin{{Domain: "api.z.ai", Via: "gone-uplink", Fam: "4"}}}
	_, build := planFamily(st, "4")
	r := findRuleTo(build, "47.246.13.13")
	if r == nil || tableOf(r) != itoa(tableBlock) {
		t.Fatalf("unknown-uplink pin not fail-closed; build=%v", build)
	}
}

// TestPinsOff: the operator's switch excludes pins from every build (the store
// itself is untouched — off is a build-time gate, not a deletion).
func TestPinsOff(t *testing.T) {
	defer stubResolve(map[string][]string{"api.anthropic.com": {"160.79.104.10"}})()
	st := &State{
		Uplinks: []Uplink{{Name: "funnel", Dev: "lo", Table: 150}},
		Pins:    []Pin{{Domain: "api.anthropic.com", Via: "funnel", Fam: "4"}},
		PinsOff: true,
	}
	if len(st.Pins) != 1 {
		t.Fatal("fixture lost its pin before planning")
	}
	_, build := planFamily(st, "4")
	if findRuleTo(build, "160.79.104.10") != nil {
		t.Fatalf("pins built while PinsOff; build=%v", build)
	}
}
