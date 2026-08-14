// nmprops.go — netgov's ownership of the NetworkManager profile properties that decide WHICH
// ADAPTER CARRIES TRAFFIC, and the save/restore that keeps `netgov reset` honest while it does.
//
// WHY THIS FILE EXISTS, AND WHY IT IS A DELIBERATE EXCEPTION.
//
// Until 2.21 netgov's safety promise was absolute and simple: it only ever ADDS ip rules in the
// 8000-29999 band and routes in tables 100-199, and never touches the main table or an NM profile.
// That is what made `netgov reset` a guaranteed complete restore, which is what made the tool safe
// to run on a box with no console. Giving that up needs a better reason than convenience.
//
// The reason is that the promise was buying less than it looked. Two NM profile properties decide
// the outcome netgov exists to control, and netgov could neither see nor set either of them:
//
//   - `ipv4.never-default` — whether an uplink may carry the default route AT ALL. netgov would
//     happily accept `default set --v4 cable`, report it applied, and be silently overruled. The
//     only trace of the property in the whole source was a field comment calling it a workaround.
//   - `ipv4.route-metric` — which adapter is actually the path. On WorkStation this was hand-set
//     to 1000 on the wired profile, pushing it BELOW wifi's automatic 600, so the claim arbiter
//     held .153 on a leg the host never spoke from. Reported correct, by a tool with no way to
//     know better (see claim.go claimPaths, 2.20).
//
// Operator ruling, 2026-08-14: *"If someone still needs to manually tamper something else, then
// netgov is not doing all the job it should."* Both properties become netgov's, and the safety
// promise is KEPT RATHER THAN ABANDONED by making reset save-and-restore instead of a no-op:
// before netgov writes a property for the first time it records the ORIGINAL value in state, and
// `netgov reset` puts every one of them back.
//
// THE ORIGINAL IS RECORDED EXACTLY ONCE. Re-recording on every apply would, on the second apply,
// save netgov's own value as the pristine one and quietly make reset a no-op — the restore would
// still run, still report success, and restore nothing. That is the swallowed-effect failure this
// project wrote a rule about (n-228), so it is asserted in a test rather than observed.
package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Route metrics derived from claim priority. The band is deliberately BELOW NetworkManager's
// automatic values (ethernet 100, wifi 600) so that a claimant always outranks an unmanaged
// interface — otherwise declaring a claim priority would change nothing on a box whose other
// adapters sit at the defaults. Distinctive on sight, too: a metric of 50 or 60 in `ip route` is
// netgov's, not something NM chose.
const (
	nmMetricBase = 50
	nmMetricStep = 10
)

// NMSaved is one profile property netgov has taken over, with the value it had BEFORE netgov
// first wrote it. Original is stored verbatim as nmcli reports it, including the empty string
// and the literal "-1" that means "automatic" — restoring "automatic" is a real outcome and must
// not be confused with "nothing was saved".
type NMSaved struct {
	Profile  string `json:"profile"`
	Prop     string `json:"prop"`
	Original string `json:"original"`
}

// governanceLines answers "who actually decides the path here — netgov or NetworkManager?"
//
// WHY (c-001, n-243, 2026-08-14): after 2.21 landed they verified the box and found the capability
// present and governing nothing — the toggle was off, so NM still decided every metric. Their
// point: *"the version number and the effective state are two different facts, and on this box only
// the first one changed. netgov/2.21 tells you the capability exists on disk, not that it governs
// anything."*
//
// That is `armed=true is not a state` one level up, and it misreads in the dangerous direction: a
// future session reads a deployment table, sees 2.21, concludes netgov owns the metric here, and
// therefore does NOT look at NM — which is still the thing deciding. A version is an intention; the
// enforceable state is version AND toggle AND properties actually adopted.
func governanceLines(st *State) []string {
	on := st.ManageMetrics != nil && *st.ManageMetrics
	if !on {
		return []string{"metrics: NETWORKMANAGER decides the path — netgov capability present but not governing" +
			" (netgov uplink manage-metrics on); claim priority does NOT set the route metric"}
	}
	var adopted []string
	for _, s := range st.NMSaved {
		if s.Prop == "ipv4.route-metric" {
			adopted = append(adopted, s.Profile+" (was "+dash(s.Original)+")")
		}
	}
	if len(adopted) == 0 {
		return []string{"metrics: netgov is ENABLED but has adopted nothing yet — run `netgov apply`" +
			" (nothing is saved, so `reset` currently has nothing to restore)"}
	}
	return []string{"metrics: NETGOV decides the path, from claim priority — adopted " +
		strings.Join(adopted, ", ") + "; `netgov reset` restores those originals"}
}

// canDefaultStr renders the tri-state for humans. "auto" and "yes" must not look alike: one means
// netgov holds the property, the other means it does not hold it at all.
func canDefaultStr(b *bool) string {
	if b == nil {
		return "auto (unmanaged)"
	}
	if *b {
		return "yes"
	}
	return "no"
}

// nmGet reads one property off a profile. Returns ("", false) when the profile or property does
// not exist, which must stay distinguishable from a property that exists and is empty.
func nmGet(profile, prop string) (string, bool) {
	out, err := run("nmcli", "-g", prop, "connection", "show", profile)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// nmSavedFor finds an existing save record. Presence is the whole guard against re-recording.
func nmSavedFor(st *State, profile, prop string) *NMSaved {
	for i := range st.NMSaved {
		if st.NMSaved[i].Profile == profile && st.NMSaved[i].Prop == prop {
			return &st.NMSaved[i]
		}
	}
	return nil
}

// nmRecordOriginal saves the pre-netgov value the FIRST time a property is taken over, and is a
// no-op every time after. Returns true if it recorded something.
//
// The no-op-on-subsequent-calls half is the load-bearing half. See the file header.
func nmRecordOriginal(st *State, profile, prop, current string) bool {
	if nmSavedFor(st, profile, prop) != nil {
		return false
	}
	st.NMSaved = append(st.NMSaved, NMSaved{Profile: profile, Prop: prop, Original: current})
	return true
}

// nmDesired is one property netgov wants set, and why — the reason travels with the change so
// `netgov plan` explains itself rather than printing bare nmcli lines.
type nmDesired struct {
	Profile string
	Prop    string
	Value   string
	Why     string
}

// uplinkRoutingDesired computes every NM property netgov wants to own right now. Pure apart from
// reading current claim state, and it does NOT write — so `netgov plan` can show the whole set
// before anything happens, which is the operator's standing requirement that changes to a
// load-bearing box be inspectable in safe mode first.
func uplinkRoutingDesired(st *State) []nmDesired {
	var want []nmDesired

	// 1. never-default, per uplink, only where the operator has expressed a preference. A nil
	//    CanDefault means UNMANAGED: netgov must not adopt a property nobody asked it to hold,
	//    or a reset would "restore" a value netgov invented.
	for _, u := range st.Uplinks {
		if u.CanDefault == nil {
			continue
		}
		prof := devProfile(u.Dev)
		if prof == "" {
			continue
		}
		v, why := "yes", "uplink "+u.Name+" may NOT carry the default route"
		if *u.CanDefault {
			v, why = "no", "uplink "+u.Name+" may carry the default route"
		}
		want = append(want, nmDesired{prof, "ipv4.never-default", v, why})
	}

	// 2. route-metric from CLAIM PRIORITY. This is the operator's point: the claim already states
	//    which adapter is preferred, so stating it twice — once as a priority and once as a metric
	//    a human types into NM — is exactly the manual step that should not exist.
	//
	//    OPT-IN, and defaulting to OFF on upgrade is not timidity. applyUplinkRouting runs inside
	//    applyRoot, which the NM dispatcher invokes on every carrier event — so shipping this
	//    enabled would mean that upgrading the binary silently hands netgov two host properties at
	//    the next link flap, on boxes whose operators never asked. That is the armed-but-not-
	//    enforcing surprise of 2026-08-13 with the sign reversed: enforcing something nobody armed.
	//    One deliberate switch is consent, not the manual tampering the operator objected to.
	if st.ManageMetrics == nil || !*st.ManageMetrics {
		return want
	}
	cl := claimForActive(st)
	if cl == nil {
		return want
	}
	cs := append([]Claimant(nil), cl.Claimants...)
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Priority > cs[j].Priority })
	for rank, c := range cs {
		prof := devProfile(c.Dev)
		if prof == "" {
			continue
		}
		m := nmMetricBase + rank*nmMetricStep
		want = append(want, nmDesired{prof, "ipv4.route-metric", strconv.Itoa(m),
			fmt.Sprintf("claim %s: %s is priority %d (rank %d) for %s", cl.Address, c.Dev, c.Priority, rank+1, cl.Address)})
	}
	return want
}

// uplinkRoutingPlan turns the desired set into the nmcli commands still OUTSTANDING, skipping
// anything already correct. Idempotence matters more than usual here: this runs from the NM
// dispatcher on carrier events, and a plan that always has work to do would reapply interfaces in
// a loop — the failure mode that took this very box down for five minutes on 2026-08-13.
//
// Returns the commands, plus the devices needing a reapply for the change to take effect.
func uplinkRoutingPlan(st *State) (cmds [][]string, reapply []string, notes []string) {
	seenDev := map[string]bool{}
	for _, d := range uplinkRoutingDesired(st) {
		cur, ok := nmGet(d.Profile, d.Prop)
		if !ok {
			notes = append(notes, "skip "+d.Profile+" "+d.Prop+": profile not found")
			continue
		}
		if nmEquivalent(d.Prop, cur, d.Value) {
			continue
		}
		notes = append(notes, d.Profile+" "+d.Prop+": "+dash(cur)+" -> "+d.Value+"   ("+d.Why+")")
		cmds = append(cmds, []string{"nmcli", "connection", "modify", d.Profile, d.Prop, d.Value})
		if dev := profileDev(st, d.Profile); dev != "" && !seenDev[dev] {
			seenDev[dev] = true
			reapply = append(reapply, dev)
		}
	}
	return cmds, reapply, notes
}

// nmEquivalent compares an nmcli-reported value against what netgov wants, absorbing the spellings
// nmcli uses for the same state. `-1` and “ both mean "automatic" for route-metric, and getting
// this wrong makes every apply think it has work to do — see the idempotence note above.
func nmEquivalent(prop, cur, want string) bool {
	if cur == want {
		return true
	}
	if prop == "ipv4.route-metric" && (cur == "" || cur == "-1") && (want == "" || want == "-1") {
		return true
	}
	return false
}

// profileDev maps a profile name back to the device netgov knows it by.
func profileDev(st *State, profile string) string {
	for _, u := range st.Uplinks {
		if devProfile(u.Dev) == profile {
			return u.Dev
		}
	}
	if cl := claimForActive(st); cl != nil {
		for _, c := range cl.Claimants {
			if devProfile(c.Dev) == profile {
				return c.Dev
			}
		}
	}
	return ""
}

// applyUplinkRouting writes the outstanding properties, recording each original exactly once
// first. Root only. Returns the number of properties written.
//
// `nmcli device reapply` rather than `connection up`: reapply re-reads the IP configuration in
// place, where up/down drops the link. On this box wlo1 carries the operator's session, so a
// down/up to change a route metric would be a self-inflicted outage on the path being tuned.
func applyUplinkRouting(st *State) (int, []string) {
	cmds, reapply, notes := uplinkRoutingPlan(st)
	if len(cmds) == 0 {
		return 0, notes
	}
	// Record every original BEFORE the first write. Doing it per-command would leave a partial
	// record if a later write failed, and a partial record is worse than none: reset would restore
	// some properties and silently keep netgov's values on the rest.
	for _, d := range uplinkRoutingDesired(st) {
		if cur, ok := nmGet(d.Profile, d.Prop); ok {
			nmRecordOriginal(st, d.Profile, d.Prop, cur)
		}
	}
	n := 0
	for _, c := range cmds {
		if out, err := run(c...); err != nil {
			notes = append(notes, "! "+strings.Join(c, " ")+" -> "+err.Error()+" "+out)
			continue
		}
		n++
	}
	for _, dev := range reapply {
		if out, err := run("nmcli", "device", "reapply", dev); err != nil {
			notes = append(notes, "! reapply "+dev+" -> "+err.Error()+" "+out+
				" (property is set; it takes effect on the next connection up)")
		}
	}
	return n, notes
}

// restoreNMProps puts every taken-over property back to the value it had before netgov touched it,
// and forgets the records. This is what keeps `netgov reset` a complete restore now that netgov
// writes NM — the whole justification for allowing it to write at all.
func restoreNMProps(st *State) []string {
	var out []string
	devs := map[string]bool{}
	for _, s := range st.NMSaved {
		v := s.Original
		if v == "" {
			// nmcli rejects an empty value; the reset word for "back to automatic" is "".
			// Passing the literal below is how nmcli spells "unset this property".
			v = ""
		}
		if _, err := run("nmcli", "connection", "modify", s.Profile, s.Prop, v); err != nil {
			out = append(out, "! restore "+s.Profile+" "+s.Prop+" -> "+err.Error())
			continue
		}
		out = append(out, "restored "+s.Profile+" "+s.Prop+" = "+dash(s.Original))
		if d := profileDev(st, s.Profile); d != "" {
			devs[d] = true
		}
	}
	for d := range devs {
		_, _ = run("nmcli", "device", "reapply", d)
	}
	st.NMSaved = nil
	return out
}
