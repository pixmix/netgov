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
	"os"
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
	demoted := demotedDevs()
	cs := rankClaimants(cl.Claimants, demoted)
	for rank, c := range cs {
		prof := devProfile(c.Dev)
		if prof == "" {
			continue
		}
		m := nmMetricBase + rank*nmMetricStep
		why := fmt.Sprintf("claim %s: %s is priority %d (rank %d) for %s", cl.Address, c.Dev, c.Priority, rank+1, cl.Address)
		if demoted[c.Dev] {
			why += " — DEMOTED below every eligible leg: the arbiter judged it ineligible on " +
				strconv.Itoa(claimDemoteStreak) + "+ consecutive evaluations"
		}
		want = append(want, nmDesired{prof, "ipv4.route-metric", strconv.Itoa(m), why})
	}
	return want
}

// rankClaimants orders claimants for the route metric: BY THE VERDICT FIRST, THE DECLARED PRIORITY
// SECOND (2.33).
//
// Priority alone says which leg we would PREFER; it says nothing about which leg currently WORKS.
// Ranking on preference alone is how ms-rosy came to advertise .186 from a healthy leg while
// sourcing its traffic through one losing 70-100% of frames: the arbiter had already rejected that
// leg and the metric never heard about it. A demoted leg keeps its relative order among the other
// demoted legs, so a box whose legs are all failing does not shuffle for no reason.
//
// Pure, so the policy is testable without NetworkManager — the sort is the whole fix, and it is
// exactly the part that must not be got subtly wrong.
func rankClaimants(claimants []Claimant, demoted map[string]bool) []Claimant {
	cs := append([]Claimant(nil), claimants...)
	sort.SliceStable(cs, func(i, j int) bool {
		if di, dj := demoted[cs[i].Dev], demoted[cs[j].Dev]; di != dj {
			return !di // an eligible leg always outranks a demoted one, whatever the priorities say
		}
		return cs[i].Priority > cs[j].Priority
	})
	return cs
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
//
// MEASURED 2026-08-16, after this comment had been recorded as a suspected lie. A single observation
// on 2026-08-15 — a manual `nmcli device reapply enp114s0` during an unrelated repair, on a box
// already in an anomalous state — was generalised into "reapply drops the address, and netgov's own
// comment denies it". It was carried in project memory for a day as a defect awaiting a fix.
//
// It does not reproduce. On an isolated veth+dnsmasq rig, five conditions, address preserved in all:
//
//	static IPv4 + reapply                      -> address kept
//	DHCP + plain reapply                       -> address kept
//	DHCP + ipv4.route-metric change + reapply  -> address kept, metric applied IN PLACE (101 -> 50)
//	DHCP + ipv4.never-default change + reapply -> address kept, default route removed in place
//	DHCP + on-link route or address DELETED    -> reapply RESTORED them
//
// So reapply is the repair operation, not a destructive one, and this comment is correct for the two
// properties applied here. The 2026-08-15 event remains unexplained; what is settled is that it is
// not caused by what this function does. ⚠️ The correction was itself an unmeasured generalisation
// from one observation — the same error it was recording, one level up. A single observation during
// a manual repair on an anomalous box is a QUESTION, not a finding.
//
// What the rig DID find is the failure below: a pending `cloned-mac-address` makes NM refuse the
// whole reapply, so these properties silently never take effect on a box running failover.
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
			note := "! reapply " + dev + " -> " + err.Error() + " " + out
			// NM refuses a reapply WHOLESALE if any pending setting cannot be applied in place, and
			// `cloned-mac-address` is exactly such a setting — measured 2026-08-16:
			//   Error: ... failed: Can't reapply changes to '802-3-ethernet.cloned-mac-address' setting
			// The routing properties beside it then never take effect, and the old note ("takes
			// effect on the next connection up") was true and far too calm: nothing ever does a
			// connection up.
			//
			// ⚠️ THE HAZARD IS STATE-DEPENDENT, NOT PERMANENT — and that is worse. My first reading
			// was "a box running identity-MAC failover always carries a cloned MAC, so metrics are
			// inert there"; c-001 measured ms-rosy and it is false. A leg whose PERMANENT MAC is
			// already the identity MAC needs no clone, so in the steady state the profile carries
			// none and reapply works. A clone is present only while the design needs one: a leg
			// PARKED on its locally-administered variant, or a standby WEARING the identity.
			//
			// So this path is armed ONLY DURING A FAILOVER — exactly when the metric decides which
			// leg carries traffic, and exactly when nobody is reading a route table. A test on a
			// healthy box says the feature works, and it does, right up to the moment it matters.
			// Do not infer the clone from the box running failover: uplinkRoutingVerify below reads
			// the kernel instead, which is the only check that holds in every state.
			if strings.Contains(out, "cloned-mac-address") {
				note += "\n    ^ this profile carries a cloned-mac-address, which NM cannot reapply in" +
					" place, so it fails the WHOLE reapply — the route metric below is NOT in effect." +
					" It lands on the next `nmcli connection up " + devProfileName(st, dev) + "` (a down/up," +
					" so not something netgov does to a live leg by itself)."
			}
			notes = append(notes, note)
		}
	}
	notes = append(notes, uplinkRoutingVerify(st)...)
	return n, notes
}

// devProfileName is a display helper for the note above — the profile a device is known by, or the
// device name when netgov has no profile for it.
func devProfileName(st *State, dev string) string {
	if p := devProfile(dev); p != "" {
		return p
	}
	return dev
}

// uplinkRoutingVerify asserts the EFFECT of the route-metric writes instead of trusting that a
// successful `nmcli connection modify` reached the kernel.
//
// This is 2.29's lesson applied to the property path, which had been left counting exit codes: a
// modify succeeds, a reapply fails for an unrelated pending setting, and every log line reads like
// the metric was applied while the kernel still uses the old one. Nothing downstream notices,
// because a wrong metric does not break connectivity — it just silently picks the wrong leg, which
// is the entire fault class 2.20 exists to report.
func uplinkRoutingVerify(st *State) []string {
	var out []string
	for _, d := range uplinkRoutingDesired(st) {
		if d.Prop != "ipv4.route-metric" {
			continue
		}
		dev := profileDev(st, d.Profile)
		if dev == "" {
			continue
		}
		want, err := strconv.Atoi(d.Value)
		if err != nil {
			continue
		}
		live, ok := liveRouteMetric(dev)
		if !ok {
			continue // no on-link route to read: unconfigured or down, not a divergence
		}
		if live != want {
			out = append(out, fmt.Sprintf("⚠ %s: route metric is %d in the kernel, netgov set %d —"+
				" NOT IN EFFECT, so the path may not follow the claim", dev, live, want))
		}
	}
	return out
}

// liveRouteMetric reads the metric the KERNEL is currently using for dev's on-link prefix route —
// the one that exists whenever the device is addressed, unlike the default route which
// `ipv4.never-default` may legitimately remove.
func liveRouteMetric(dev string) (int, bool) {
	out, err := run("ip", "route", "show", "dev", dev)
	if err != nil {
		return 0, false
	}
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Fields(ln)
		if len(f) == 0 || f[0] == "default" {
			continue
		}
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "metric" {
				if n, err := strconv.Atoi(f[i+1]); err == nil {
					return n, true
				}
			}
		}
	}
	return 0, false
}

// savedMACIsOurs reports whether a recorded "original" cloned-mac-address is in fact a value netgov
// writes — the identity MAC of any claim, or the device's own permanent or parked MAC. Pure enough
// to test: everything it needs comes from st plus two lookups on the named device.
//
// The pair with honestMACBaseline is deliberate. That one stops NEW bad records; this one disarms
// the ones already written, which is the half that matters on a box that has already run failover.
// Both are cheap, and the failure they prevent only ever shows up during a reset — i.e. while
// someone is already recovering from something else.
func savedMACIsOurs(st *State, s NMSaved) (bool, string) {
	if s.Original == "" || !strings.HasSuffix(s.Prop, "cloned-mac-address") {
		return false, ""
	}
	cur := strings.ToLower(unescapeNM(s.Original))
	for _, p := range st.Patterns {
		if p.Claim != nil && p.Claim.IdentityMAC != "" && strings.EqualFold(cur, p.Claim.IdentityMAC) {
			return true, "this is claim " + p.Claim.Address + "'s identity MAC, which netgov sets"
		}
	}
	dev := profileDev(st, s.Profile)
	if dev == "" {
		return false, ""
	}
	perm := devPermMAC(dev)
	if perm == "" {
		return false, ""
	}
	if strings.EqualFold(cur, perm) {
		return true, dev + "'s permanent MAC — a clone of the hardware address is netgov clearing, not a user setting"
	}
	if strings.EqualFold(cur, parkedMAC(perm)) {
		return true, "netgov's own parked MAC for " + dev
	}
	return false, ""
}

// restoreNMProps puts every taken-over property back to the value it had before netgov touched it,
// and forgets the records. This is what keeps `netgov reset` a complete restore now that netgov
// writes NM — the whole justification for allowing it to write at all.
func restoreNMProps(st *State) []string {
	var out []string
	devs := map[string]bool{}
	for _, s := range st.NMSaved {
		v := s.Original
		// A recorded "original" that is recognisably netgov's own would make reset INSTALL an
		// artefact rather than remove one — and a parked MAC installed on the leg the router
		// reserves the address to would cost the box its identity at the worst moment. Re-check
		// here as well as at record time, because the bad records already exist in the field:
		// .153 carried one from the 2.27 live failover test until 2.28.
		if drop, why := savedMACIsOurs(st, s); drop {
			out = append(out, "dropped "+s.Profile+" "+s.Prop+" = "+dash(s.Original)+" ("+why+"); unsetting instead")
			v = ""
		}
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

// liveNeverDefault reports what NetworkManager ACTUALLY has for a device's active profile —
// "yes", "no", or "" when it cannot be read.
//
// This is the measured half of the pair. `CanDefault` is netgov's intention and they diverge
// whenever netgov is not managing the property, which is the default. Reporting only the
// intention is how the dashboard came to show "auto" on a NIC that could not carry a default
// route at all — the exact invisibility that motivated 2.21, surviving inside its own fix.
func liveNeverDefault(dev string) string {
	prof := devProfile(dev)
	if prof == "" {
		return ""
	}
	v, ok := nmGet(prof, "ipv4.never-default")
	if !ok {
		return ""
	}
	return v
}

// runningBinaryReplaced reports whether the binary this process is EXECUTING has been replaced on
// disk since it started — i.e. the long-running service is stale relative to what is installed.
//
// WHY, and it is the third layer of one mistake. 2.24 made a browser page notice it had outlived
// its serving process. It cannot notice THIS: the page stamp and /api/state both come from the
// same process, so they agree perfectly while that process runs code nobody has shipped for an
// hour. On 2026-08-14 I built 2.25, ran `netgov install`, and did not restart netgov-web — the
// operator then asked why his dashboard said 2.24. It said 2.24 because it WAS 2.24; the file was
// 2.25 and `netgov --version` happily confirmed the file. I verified the artefact and not the
// running thing, which is the error this codebase has spent the day fixing in other people's
// systems.
//
// Linux gives the answer for free: when a running executable's inode is unlinked (which is what
// `go build -o` over a live binary does), /proc/self/exe still resolves but the kernel appends
// " (deleted)". No version parsing, no re-exec, no ambiguity — and it is the CAUSE (this process
// is not that file) rather than a proxy for it.
func runningBinaryReplaced() (bool, string) {
	p, err := os.Readlink("/proc/self/exe")
	if err != nil {
		return false, ""
	}
	if strings.HasSuffix(p, " (deleted)") {
		return true, strings.TrimSuffix(p, " (deleted)")
	}
	return false, p
}
