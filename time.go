package main

// Time sync — the host's clock policy, and HOST-LOCAL BY DESIGN (2.36).
//
// WHY THIS IS IN NETGOV AT ALL. A clock is not obviously a routing concern, and it became one
// the day a LAN was measured with no synchronised clock on it: NTP is UDP 123, the gateway was
// sending 123 through a proxy that does not carry it, and every client reported `NTP: yes` with
// `NTPSynchronized: no` — a client that is running and polling, and nothing answering. The fix
// on that gateway is the gateway's business. What belongs HERE is the half a host must be able
// to answer alone: *which source this machine uses, and over which leg*, because the case that
// matters is the one with no cooperative router in it — a venue AP that filters NTP, a phone
// tether, a direct cable to a box. netgov already owns "which leg does this destination leave
// by"; a time source is a destination.
//
// ⚠️ netgov DOES NOT MIRROR A ROUTER. It holds no opinion about, and no copy of, any gateway's
// time policy: the two are independent, and a host that reads its own clock policy off a router
// it may not be behind tomorrow has learned nothing. The only thing netgov asks a gateway is
// *what time it thinks it is* (see timeProbe) — a measurement, never a configuration.
//
// WHAT IT WRITES. Exactly one file, /etc/systemd/timesyncd.conf.d/50-netgov.conf, and nothing
// else. That is the whole restore story: a drop-in netgov owns can be deleted, and deleting it
// returns the host to whatever timesyncd.conf said before netgov existed. No saved-baseline
// bookkeeping (the NMSaved dance) is needed, because netgov never edits a file it did not create.

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func parseFloat(s string) (float64, error) { return strconv.ParseFloat(s, 64) }
func atoiSafe(s string) int                { n, _ := strconv.Atoi(s); return n }

// TimeSync is the host's declared clock policy. nil State.Time means UNMANAGED, not "pool":
// netgov must not adopt a host property nobody asked it to hold — the same rule Uplink.CanDefault
// follows, and for the same reason (`reset` would otherwise "restore" a value netgov invented).
type TimeSync struct {
	// Mode: "pool" (public servers, used directly) | "server" (named source: a gateway's own
	// NTP, an appliance, anything reachable) | "htpdate" (the HTTPS-Date fallback tool, which is
	// TCP and therefore survives a path that eats UDP 123).
	Mode string `json:"mode"`

	// Servers is what mode=pool and mode=server ask. For "pool" an empty list means the
	// distribution's own pool; for "server" it is required.
	Servers []string `json:"servers,omitempty"`

	// Via pins the time source's traffic to one uplink by name, using an ordinary netgov
	// destination rule per resolved address. Empty = no pin: time leaves by the default route
	// like everything else. This is the field that makes the feature netgov's rather than
	// systemd's — "take time over the tether, not over the cable".
	Via string `json:"via,omitempty"`
}

const (
	timeDropIn  = "/etc/systemd/timesyncd.conf.d/50-netgov.conf"
	timeRuleTag = "netgov-time" // marks the destination rules netgov owns for the time source
)

// htpdateCandidates: where the HTTPS-Date fallback tool is installed. It is ANOTHER PROJECT'S
// ARTEFACT (debug-workspace / c-001) and netgov never edits, replaces or removes it — it calls
// it, and defers to it: the tool stands down by itself whenever NTPSynchronized=yes, and that
// check sits at the right layer, so netgov must not be able to override it.
var htpdateCandidates = []string{
	"/usr/local/sbin/htpdate-fallback",
	"/usr/sbin/htpdate-fallback",
}

// The dependency, stated where the choice is made (operator's ruling, n-840): netgov DETECTS this
// tool, it does not ship or install it, so a panel that offers the source and not its provenance
// asks for a decision it has withheld the facts for.
const (
	htpdateMinVersion = "1.3"
	htpdateOwner      = "c-001 (debug-workspace)"
	htpdateSource     = "~/dev/debug/tools/htpdate-fallback"
	htpdateWhat       = "reads the HTTPS Date header from three independent hosts and takes the consensus — TCP, so it works on a path that does not carry UDP 123"
	htpdateWhyMin     = "1.1 ignores --report, cannot take its lock as a non-root user, prints nothing and EXITS 0 — so an old build and no build at all look identical to a caller that trusts the exit status"
)

// htpdateVersion reads the installed build's declared version: `--version` first, then the
// artefact marker in the file, because the build that most needs identifying is the OLD one and it
// may not answer --version at all.
func htpdateVersion() string {
	p := htpdatePath()
	if p == "" {
		return ""
	}
	if out, err := run(p, "--version"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "htpdate-fallback/") {
				return strings.TrimSpace(l)
			}
		}
	}
	if b, err := os.ReadFile(p); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if i := strings.Index(l, "artefact-version:"); i >= 0 {
				return strings.TrimSpace(l[i+len("artefact-version:"):])
			}
		}
	}
	return "unknown"
}

// htpdateUsable: installed AND actually answering --report. Deliberately a MEASUREMENT, not a
// version-string comparison — the version is what the file claims, and what matters is whether the
// contract answers. A build that says 1.3 and cannot report is unusable; the panel says which.
func htpdateUsable() (bool, string) {
	if htpdatePath() == "" {
		return false, "not installed"
	}
	if _, err := htpdateRunReport(""); err != nil {
		return false, err.Error()
	}
	return true, ""
}

func htpdatePath() string {
	for _, p := range htpdateCandidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// foreignTimeDropIns lists OTHER tools' timesyncd drop-ins that set a server, newest-sorting
// last. This is not tidiness: drop-ins are applied in lexical order, netgov's is 50-, and a
// well-behaved peer tool that wrote 10-something loses silently to it. Measured on .153 the day
// this was written — another project had just placed `10-lan-time-source.conf` pointing the host
// at its gateway, and netgov's own file would have outranked it with no trace anywhere that a
// second tool had an opinion. ⇒ netgov may win, because the operator chose netgov; it may not win
// QUIETLY. `time set unmanaged` removes only netgov's file, which hands the property straight
// back to whoever else is holding it.
func foreignTimeDropIns() []string {
	dir := filepath.Dir(timeDropIn)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || filepath.Join(dir, e.Name()) == timeDropIn {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(b), "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "NTP=") && l != "NTP=" {
				out = append(out, e.Name()+" ("+strings.TrimPrefix(l, "NTP=")+")")
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// ---------- htpdate-fallback, as a measurable source (contract: c-001, n-839) ----------
//
// `htpdate-fallback --report` measures without touching the clock, needs no root and takes no
// lock, so netgov may call it per candidate leg as its own user. Two parts of its contract are
// load-bearing here and neither is obvious:
//
//	· IT MEASURES EVEN WHEN NTPSynchronized=yes (status=ok-ntp-synced). Standing down governs
//	  SETTING the clock, never measuring it — otherwise the stratum-16 case, a source that
//	  answers and is not a clock, cannot be displayed at all.
//	· EXIT 10 (stood down) AND 11 (within threshold) ARE HEALTHY. A caller that reads non-zero
//	  as failure reports a working tool as broken — the defect c-001 found in their own unit,
//	  and the reason this code lists the healthy codes explicitly instead of testing != 0.
//
// ⚠️ AND THE CASE THAT IS NOT IN THE CONTRACT: an OLDER build (1.1) does not know `--report`,
// ignores it, and as a non-root user exits **0 having printed nothing at all**. Measured on the
// dev host 2026-09-13. So the check is THE PRESENCE OF A PARSEABLE LINE, never the exit status:
// `rc=0` with no output is the most convincing way for a tool to tell you nothing.
type htpdateReport struct {
	Status  string  // ok-ntp-synced | ok-ntp-unsynced | too-few-sources | …
	Offset  float64 // seconds; our clock against the HTTPS Date consensus
	HasOff  bool    // false when the tool reported "-" (it could not measure)
	Sources int
	Spread  string
	Bind    string
	Raw     string
}

// htpdateRunReport measures via the HTTPS-Date tool, optionally bound to one leg. For this source
// "over a leg" is the tool's own BIND (curl --interface), NOT a netgov route rule: three HTTPS
// hosts pinned by destination would be the wrong mechanism for the same intent, and a bound leg
// with no route refuses outright rather than returning a plausible number.
func htpdateRunReport(bind string) (*htpdateReport, error) {
	p := htpdatePath()
	if p == "" {
		return nil, fmt.Errorf("htpdate-fallback is not installed")
	}
	cmd := exec.Command(p, "--report")
	cmd.Env = append(os.Environ(), "BIND="+bind)
	out, err := cmd.CombinedOutput()
	var rc int
	if ee, ok := err.(*exec.ExitError); ok {
		rc = ee.ExitCode()
	}
	r := parseHtpdateLine(string(out))
	if r == nil {
		return nil, fmt.Errorf("no report line (needs htpdate-fallback/1.3+; an older build ignores --report and can exit 0 silently) rc=%d", rc)
	}
	switch rc {
	case 0, 10, 11: // acted/reported · stood down · within threshold — all healthy
		return r, nil
	case 2:
		return r, fmt.Errorf("too few sources (a bound leg with no route refuses rather than guessing)")
	case 3:
		return r, fmt.Errorf("sources disagreed beyond the spread")
	default:
		return r, fmt.Errorf("htpdate-fallback exit %d", rc)
	}
}

// parseHtpdateLine pulls the report out of whatever the tool printed, or nil if there is no report
// in it. Separated from the exec so the contract can be tested without a binary present.
func parseHtpdateLine(out string) *htpdateReport {
	for _, l := range strings.Split(out, "\n") {
		if !strings.Contains(l, "mode=report") {
			continue
		}
		r := &htpdateReport{Raw: strings.TrimSpace(l)}
		for _, tok := range strings.Fields(l) {
			k, v, ok := strings.Cut(tok, "=")
			if !ok {
				continue
			}
			switch k {
			case "status":
				r.Status = v
			case "offset":
				if v != "-" {
					if f, e := parseFloat(v); e == nil {
						r.Offset, r.HasOff = f, true
					}
				}
			case "sources":
				r.Sources = atoiSafe(v)
			case "spread":
				r.Spread = v
			case "bind":
				r.Bind = v
			}
		}
		return r
	}
	return nil
}

// ---------- SNTP: ask for the time, which is the only honest check ----------

// sntpQuery sends one SNTP request and returns the server's stratum and the offset of OUR clock
// against it (positive = we are ahead). Deliberately dependency-free and deliberately reports
// STRATUM: an ntpd that cannot reach its own upstream still answers, at stratum 16 with a
// plausible-looking time, so "something replied" is not the check and never was.
func sntpQuery(addr string, timeout time.Duration) (stratum int, offset time.Duration, err error) {
	if !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(addr, "123")
	}
	c, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return 0, 0, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))

	req := make([]byte, 48)
	req[0] = 0x1b // LI 0, VN 3, mode 3 (client)
	t1 := time.Now()
	if _, err = c.Write(req); err != nil {
		return 0, 0, err
	}
	resp := make([]byte, 48)
	if _, err = c.Read(resp); err != nil {
		return 0, 0, err
	}
	t4 := time.Now()

	const epoch = 2208988800 // NTP epoch (1900) -> Unix epoch (1970), in seconds
	ntpTime := func(b []byte) time.Time {
		secs := binary.BigEndian.Uint32(b[0:4])
		frac := binary.BigEndian.Uint32(b[4:8])
		if secs == 0 {
			return time.Time{}
		}
		return time.Unix(int64(secs)-epoch, int64(frac)*1e9>>32)
	}
	t2 := ntpTime(resp[32:40]) // server receive
	t3 := ntpTime(resp[40:48]) // server transmit
	if t2.IsZero() || t3.IsZero() {
		return int(resp[1]), 0, fmt.Errorf("reply carries no timestamp")
	}
	// Standard NTP offset: the mean of the two one-way discrepancies, which cancels the
	// symmetric part of the round trip.
	offset = ((t2.Sub(t1)) + (t3.Sub(t4))) / 2
	return int(resp[1]), -offset, nil
}

// timesyncdState reports what the host's own client believes. Two booleans, and the gap between
// them is the whole reason this feature exists: NTP=yes says the client is running; only
// NTPSynchronized=yes says something answered it.
func timesyncdState() (enabled, synced bool, server string) {
	out, err := run("timedatectl", "show",
		"--property=NTP", "--property=NTPSynchronized")
	if err == nil {
		for _, l := range strings.Split(out, "\n") {
			k, v, _ := strings.Cut(strings.TrimSpace(l), "=")
			switch k {
			case "NTP":
				enabled = v == "yes"
			case "NTPSynchronized":
				synced = v == "yes"
			}
		}
	}
	// show-timesync exists from systemd 247; absence is not an error, just no name to print.
	if out, err := run("timedatectl", "show-timesync",
		"--property=ServerName", "--property=ServerAddress"); err == nil {
		var name, addr string
		for _, l := range strings.Split(out, "\n") {
			k, v, _ := strings.Cut(strings.TrimSpace(l), "=")
			switch k {
			case "ServerName":
				name = v
			case "ServerAddress":
				addr = v
			}
		}
		switch {
		case name != "" && addr != "" && name != addr:
			server = name + " (" + addr + ")"
		case addr != "":
			server = addr
		default:
			server = name
		}
	}
	return
}

// systemNTPServers is the list the CLIENT is actually using — the merged result of every drop-in,
// which is the only place the concatenation behaviour is visible. The file netgov wrote is not
// evidence about this; that was the 2.37 defect.
func systemNTPServers() []string {
	out, err := run("timedatectl", "show-timesync", "--property=SystemNTPServers")
	if err != nil {
		return nil
	}
	_, v, _ := strings.Cut(strings.TrimSpace(out), "=")
	return strings.Fields(v)
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ca, cb := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(ca)
	sort.Strings(cb)
	for i := range ca {
		if ca[i] != cb[i] {
			return false
		}
	}
	return true
}

// defaultGateway4 returns the host's current IPv4 default gateway, or "". It is READ, never
// stored: `netgov time set server --gateway` resolves it once at the moment the operator asks,
// so no gateway address is ever baked into netgov's state or its source.
func defaultGateway4() string {
	out, err := run("ip", "-4", "route", "show", "default")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "via" {
				return f[i+1]
			}
		}
	}
	return ""
}

// timeSources is what a given policy will actually ask, in order.
func timeSources(ts *TimeSync) []string {
	if ts == nil {
		return nil
	}
	if len(ts.Servers) > 0 {
		return ts.Servers
	}
	if ts.Mode == "pool" {
		return defaultPool
	}
	return nil
}

// defaultPool: the public pool, used when mode=pool names no server of its own. A well-known
// service rather than anyone's machine, so it belongs in the source as itself.
var defaultPool = []string{
	"0.pool.ntp.org", "1.pool.ntp.org", "2.pool.ntp.org", "3.pool.ntp.org",
}

// ---------- the command ----------

func cmdTime(st *State, args []string) {
	if len(args) == 0 || args[0] == "status" {
		timeStatus(st)
		return
	}
	switch args[0] {
	case "probe":
		targets := nonFlagArgs(args[1:])
		if len(targets) == 0 {
			targets = timeProbeTargets(st)
		}
		timeProbe(targets)

	case "set":
		if len(args) < 2 {
			fmt.Println("usage: netgov time set unmanaged|pool|server|htpdate [--servers a,b] [--gateway] [--via <uplink>]")
			return
		}
		mode := args[1]
		via, _ := flagVal(args, "--via")
		if via != "" && upByName(st, via) == nil {
			fmt.Fprintf(os.Stderr, "netgov: no uplink named %q — `netgov uplink list`\n", via)
			os.Exit(2)
		}
		switch mode {
		case "unmanaged", "none", "off":
			st.Time = nil
		case "pool":
			ts := &TimeSync{Mode: "pool", Via: via}
			if v, ok := flagVal(args, "--servers"); ok {
				ts.Servers = splitCSV(v)
			}
			st.Time = ts
		case "server":
			ts := &TimeSync{Mode: "server", Via: via}
			if v, ok := flagVal(args, "--servers"); ok {
				ts.Servers = splitCSV(v)
			}
			if hasFlag(args, "--gateway") {
				gw := defaultGateway4()
				if gw == "" {
					fmt.Fprintln(os.Stderr, "netgov: no IPv4 default gateway to read — name the server instead")
					os.Exit(2)
				}
				ts.Servers = append(ts.Servers, gw)
			}
			if len(ts.Servers) == 0 {
				fmt.Fprintln(os.Stderr, "netgov: mode=server needs --servers <a[,b]> or --gateway")
				os.Exit(2)
			}
			st.Time = ts
		case "htpdate":
			if p := htpdatePath(); p == "" {
				fmt.Fprintf(os.Stderr, "netgov: htpdate-fallback is not installed (looked in %s)\n",
					strings.Join(htpdateCandidates, ", "))
				fmt.Fprintln(os.Stderr, "  it is another project's artefact (debug-workspace); netgov calls it, never ships it")
				os.Exit(2)
			}
			st.Time = &TimeSync{Mode: "htpdate", Via: via}
		default:
			fmt.Fprintf(os.Stderr, "netgov: unknown time mode %q (unmanaged|pool|server|htpdate)\n", mode)
			os.Exit(2)
		}
		syncTimeRules(st)
		must(saveState(st, statePath()))
		timeStatus(st)
		fmt.Println("\n# declared, NOT yet in effect — `netgov time apply` writes it (sudo -A)")

	case "apply":
		if os.Geteuid() == 0 {
			timeApplyRoot(st)
			saveStateKeepOwner(st, statePath())
			return
		}
		sudoSelf("__time-apply")

	case "__apply":
		timeApplyRoot(st)
		saveStateKeepOwner(st, statePath())

	default:
		fmt.Println("usage: netgov time [status | set <mode> … | apply | probe [addr…]]")
	}
}

// syncTimeRules keeps the destination pins in State.Rules in step with the declared policy.
// The pin is an ORDINARY netgov rule, not a private mechanism: the same `ip rule`/table machinery
// every other pin uses, so `netgov status`, `plan` and `reset` all already understand it.
func syncTimeRules(st *State) {
	var keep []Rule
	for _, r := range st.Rules {
		if r.Note != timeRuleTag {
			keep = append(keep, r)
		}
	}
	st.Rules = keep
	if st.Time == nil || st.Time.Via == "" {
		return
	}
	for _, s := range timeSources(st.Time) {
		st.Rules = append(st.Rules, Rule{Domain: s, Via: st.Time.Via, Note: timeRuleTag})
	}
}

func timeApplyRoot(st *State) {
	// UNMANAGED: remove our drop-in and hand the host back. Nothing else to undo — netgov never
	// wrote anything else.
	if st.Time == nil {
		if err := os.Remove(timeDropIn); err == nil {
			fmt.Println("netgov: removed", timeDropIn, "— host is back on its own timesyncd config")
			_, _ = run("systemctl", "restart", "systemd-timesyncd")
		} else if os.IsNotExist(err) {
			fmt.Println("netgov: time unmanaged (nothing of ours installed)")
		} else {
			fmt.Fprintln(os.Stderr, "netgov: cannot remove", timeDropIn, err)
		}
		return
	}

	if st.Time.Mode == "htpdate" {
		p := htpdatePath()
		if p == "" {
			fmt.Fprintln(os.Stderr, "netgov: htpdate-fallback has gone missing — refusing to claim a source that is not there")
			os.Exit(1)
		}
		// Its own timer keeps the schedule; netgov selects it and asks for one run now, so the
		// operator sees the effect of the choice immediately rather than at the next hour.
		// EXIT 10 AND 11 ARE HEALTHY (stood down / within threshold) — testing `err != nil` here
		// would report a working tool as broken, which is the exact defect c-001 fixed in their
		// own unit with SuccessExitStatus=10 11.
		bind := ""
		if st.Time.Via != "" {
			if u := upByName(st, st.Time.Via); u != nil {
				bind = u.Dev
			}
		}
		cmd := exec.Command(p)
		cmd.Env = append(os.Environ(), "BIND="+bind)
		out, err := cmd.CombinedOutput()
		rc := 0
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		}
		if s := strings.TrimSpace(string(out)); s != "" {
			fmt.Println("  " + strings.ReplaceAll(s, "\n", "\n  "))
		}
		switch rc {
		case 0:
			fmt.Println("  ✓ ran")
		case 10:
			fmt.Println("  ✓ stood down — NTP is synchronised, which is the tool deferring correctly, not a failure")
		case 11:
			fmt.Println("  ✓ within threshold — nothing to correct")
		default:
			fmt.Fprintf(os.Stderr, "netgov: htpdate-fallback exit %d\n", rc)
		}
		if bind != "" {
			fmt.Println("  bound to", bind, "— for this source a leg pin is the tool's own BIND (curl --interface), not a route rule")
		}
		fmt.Println("netgov: time source = htpdate-fallback (HTTPS Date; TCP, so a path that eats UDP 123 does not matter)")
		fmt.Println("  ⚠️ it stands down by itself when NTPSynchronized=yes — that is its rule, not netgov's, and netgov does not override it")
		return
	}

	srcs := timeSources(st.Time)
	if len(srcs) == 0 {
		fmt.Fprintln(os.Stderr, "netgov: no time source to write")
		os.Exit(2)
	}
	// ⚠️ THE EMPTY `NTP=` IS LOAD-BEARING AND IT IS THE WHOLE REASON THIS FILE WORKS.
	// systemd CONCATENATES `NTP=` across drop-ins — it does not let a later file replace an earlier
	// one — and the servers of the EARLIEST file are tried FIRST. So 2.37's file, which just set
	// `NTP=<ours>`, was appended after another tool's `10-` drop-in and netgov's selection became a
	// FALLBACK while the panel said it had taken effect. Measured on the dev host 2026-09-13:
	// `SystemNTPServers=<the gateway> 0.pool.ntp.org …` after netgov applied the pool, and
	// `ServerName=<the gateway>` — the other tool's server, still in use.
	// An empty assignment RESETS the accumulated list, so ours is the whole list. Asserted below
	// against the live `SystemNTPServers`, never inferred from the file having been written.
	body := "# Written by netgov (" + artefactVersion + "). netgov owns THIS FILE ONLY;\n" +
		"# `netgov time set unmanaged && netgov time apply` deletes it and restores the host's own config.\n" +
		"# The bare `NTP=` RESETS the list systemd accumulates from earlier drop-ins; without it this\n" +
		"# file only APPENDS and an earlier file's servers keep priority. Do not remove it.\n" +
		"[Time]\nNTP=\nNTP=" + strings.Join(srcs, " ") + "\n"
	if err := os.MkdirAll(filepath.Dir(timeDropIn), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "netgov:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(timeDropIn, []byte(body), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "netgov:", err)
		os.Exit(1)
	}
	if _, err := run("timedatectl", "set-ntp", "true"); err != nil {
		fmt.Fprintln(os.Stderr, "netgov: could not enable the NTP client:", err)
	}
	if _, err := run("systemctl", "restart", "systemd-timesyncd"); err != nil {
		fmt.Fprintln(os.Stderr, "netgov: systemd-timesyncd restart failed:", err)
	}
	fmt.Println("netgov: time source =", strings.Join(srcs, " "))
	for _, f := range foreignTimeDropIns() {
		fmt.Println("  ⚠ superseding another tool's drop-in:", f)
		fmt.Println("    Its file is untouched and takes effect again the moment netgov is set to unmanaged.")
	}

	// ASSERT THE EFFECT ON THE RIGHT AXIS. Synchronisation is not the check: a host that was
	// already synchronised from somebody else's server stays synchronised, and reports success,
	// while netgov's selection sits inert — which is exactly what 2.37 did. So check the LIST the
	// client is actually using first, and only then whether it has synchronised.
	if got := systemNTPServers(); len(got) > 0 {
		if !sameStringSet(got, srcs) {
			fmt.Println("  ⚠ THE CLIENT'S SERVER LIST IS NOT WHAT NETGOV ASKED FOR:")
			fmt.Println("      asked:", strings.Join(srcs, " "))
			fmt.Println("      live: ", strings.Join(got, " "))
			for _, f := range foreignTimeDropIns() {
				fmt.Println("      another drop-in is contributing:", f)
			}
			fmt.Println("      systemd CONCATENATES NTP= across drop-ins and prefers the earliest;")
			fmt.Println("      netgov resets the list with a bare NTP= to prevent that. If this warning")
			fmt.Println("      stands, something is stripping the reset — do not trust the panel until it clears.")
		} else {
			fmt.Println("  ✓ client's server list is netgov's:", strings.Join(got, " "))
		}
	}
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if _, synced, srv := timesyncdState(); synced {
			fmt.Println("  ✓ synchronised from", dash(srv))
			return
		}
		time.Sleep(1500 * time.Millisecond)
	}
	fmt.Println("  ⚠ NOT synchronised yet after 12 s — `netgov time probe` asks the source directly.")
	fmt.Println("    A source that answers at stratum 16 is a server that cannot reach ITS upstream:")
	fmt.Println("    it will keep answering, with a plausible time, and never synchronise you.")
}

func timeProbeTargets(st *State) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range timeSources(st.Time) {
		add(s)
	}
	add(defaultGateway4()) // whatever is in front of us today, asked rather than assumed
	if st.Time == nil {
		for _, s := range defaultPool[:1] {
			add(s)
		}
	}
	return out
}

func timeProbe(targets []string) {
	if len(targets) == 0 && htpdatePath() == "" {
		fmt.Println("nothing to probe")
		return
	}
	fmt.Println("# asking each source for the time (stratum 16 = it cannot reach its own upstream)")
	sort.Strings(targets)
	for _, t := range targets {
		str, off, err := sntpQuery(t, 3*time.Second)
		switch {
		case err != nil:
			fmt.Printf("  %-28s no answer      (%v)\n", t, err)
		case str == 0 || str >= 16:
			fmt.Printf("  %-28s stratum %-2d     ⚠ UNSYNCHRONISED source — answers, but is not a clock\n", t, str)
		default:
			fmt.Printf("  %-28s stratum %-2d     our offset %+.3f s\n", t, str, off.Seconds())
		}
	}
	// The HTTPS-Date source is differently shaped — TCP, three hosts, a consensus — which is
	// exactly why it belongs beside the NTP rows: it is the instrument that can contradict them.
	if htpdatePath() != "" {
		bind := ""
		if st := loadState(statePath()); st.Time != nil && st.Time.Mode == "htpdate" && st.Time.Via != "" {
			if u := upByName(st, st.Time.Via); u != nil {
				bind = u.Dev
			}
		}
		r, err := htpdateRunReport(bind)
		label := "htpdate-fallback (HTTPS)"
		switch {
		case r == nil:
			fmt.Printf("  %-28s unavailable    (%v)\n", label, err)
		case !r.HasOff:
			fmt.Printf("  %-28s %-14s could not measure (sources=%d)\n", label, r.Status, r.Sources)
		default:
			note := ""
			if err != nil {
				note = "  ⚠ " + err.Error()
			}
			fmt.Printf("  %-28s %-14s our offset %+.3f s (sources=%d spread=%s)%s\n",
				label, r.Status, r.Offset, r.Sources, dash(r.Spread), note)
		}
	}
}

func timeStatus(st *State) {
	enabled, synced, srv := timesyncdState()
	mode := "unmanaged"
	if st.Time != nil {
		mode = st.Time.Mode
	}
	fmt.Printf("time: netgov=%s", mode)
	if st.Time != nil && st.Time.Via != "" {
		fmt.Printf(" via=%s", st.Time.Via)
	}
	fmt.Println()
	if srcs := timeSources(st.Time); len(srcs) > 0 {
		fmt.Println("  declared source:", strings.Join(srcs, " "))
	}
	if st.Time != nil && st.Time.Mode == "htpdate" {
		fmt.Println("  declared source: htpdate-fallback", dash(htpdatePath()), "(HTTPS Date, TCP)")
	}
	// The dependency is printed whenever it is RELEVANT — selected, or offered and missing — and
	// never only in the failure case: a reader deciding between sources needs it before choosing.
	if true { // ALWAYS: a reader choosing between sources needs the dependency before choosing,
		// not only after picking the one that is missing (operator's ruling, n-840).
		ok, why := htpdateUsable()
		fmt.Println("  htpdate source (a SEPARATE INSTALL — netgov detects it, never ships it):")
		fmt.Printf("    what:    %s\n", htpdateWhat)
		fmt.Printf("    from:    %s, owner %s\n", htpdateSource, htpdateOwner)
		fmt.Printf("    need:    htpdate-fallback/%s or newer at %s\n", htpdateMinVersion, strings.Join(htpdateCandidates, " or "))
		if ok {
			fmt.Printf("    here:    %s — answering --report ✓\n", htpdateVersion())
		} else {
			fmt.Printf("    here:    %s — UNUSABLE: %s\n", dash(htpdateVersion()), why)
			fmt.Printf("    why:     %s\n", htpdateWhyMin)
		}
	}
	// The two booleans, side by side and never merged: this line is the measurement that a
	// whole LAN's clocks were wrong behind.
	fmt.Printf("  client: NTP=%s  NTPSynchronized=%s", yn(enabled), yn(synced))
	if srv != "" {
		fmt.Printf("  from=%s", srv)
	}
	fmt.Println()
	if enabled && !synced {
		fmt.Println("  ⚠ the client is RUNNING and NOTHING HAS ANSWERED IT. That is not a slow start:")
		fmt.Println("    it is the state a host sits in indefinitely while its clock drifts on the RTC alone.")
	}
	if _, err := os.Stat(timeDropIn); err == nil {
		fmt.Println("  netgov drop-in:", timeDropIn)
	}
	if fs := foreignTimeDropIns(); len(fs) > 0 {
		fmt.Println("  ANOTHER TOOL ALSO SETS A TIME SOURCE HERE:")
		for _, f := range fs {
			fmt.Println("    " + f)
		}
		if st.Time != nil {
			fmt.Println("    netgov's drop-in RESETS the list (a bare `NTP=`) and then sets its own, so netgov's")
			fmt.Println("    selection is the whole list — NOT because 50 sorts after 10: systemd CONCATENATES")
			fmt.Println("    NTP= across drop-ins and prefers the EARLIEST, so without the reset netgov's choice")
			fmt.Println("    would be a fallback behind this file. Nothing of theirs is edited, and")
			fmt.Println("    `netgov time set unmanaged && netgov time apply` hands the property straight back.")
		}
	}
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// nonFlagArgs keeps positional arguments, dropping flags and their values.
func nonFlagArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
			}
			continue
		}
		out = append(out, args[i])
	}
	return out
}
