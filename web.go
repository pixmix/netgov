package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// In-app help: the repo docs are embedded so /help is self-contained in the binary.
//
//go:embed docs/UI.md
var uiHelpMD string

//go:embed docs/CLI.md
var cliHelpMD string

// OPERATING.md is the custodian's view — what a verb DOES to a live host (mutates? needs root?),
// as opposed to what the feature is for. Embedded like the others so /help carries it onto every
// box: the operator who needs it most is on a headless peer with no repo checkout.
//
//go:embed docs/OPERATING.md
var opHelpMD string

type famView struct {
	Up       bool   `json:"up"`
	Src      string `json:"src"`
	GW       string `json:"gw"`
	Internet bool   `json:"internet"`
}
type uplinkView struct {
	Name  string  `json:"name"`
	Dev   string  `json:"dev"`
	Table int     `json:"table"`
	V4    famView `json:"v4"`
	V6    famView `json:"v6"`
	// CanDefault is tri-state and must survive as null in JSON — omitempty would erase the
	// difference between "unmanaged" and "may not carry the default", which are opposite states.
	CanDefault *bool `json:"can_default"`
}
type ruleView struct {
	Key  string   `json:"key"`  // domain or "from:..."
	Kind string   `json:"kind"` // "dest" | "src"
	Sel  string   `json:"sel"`  // raw domain / from value
	Via  string   `json:"via"`
	Fam  string   `json:"fam"`
	IPs  []string `json:"ips"`
}
type apView struct {
	Name   string `json:"name"`
	Dev    string `json:"dev"`
	SSID   string `json:"ssid"`
	Band   string `json:"band"`
	On     bool   `json:"on"`     // configured-on (intent)
	Active bool   `json:"active"` // live (iw says AP)
}
type patternView struct {
	Name        string   `json:"name"`
	Priority    int      `json:"priority"`
	Require     []string `json:"require"`
	SSID        string   `json:"ssid"`
	SSIDIface   string   `json:"ssid_iface"`
	APs         []string `json:"aps"`
	V4          string   `json:"v4"`
	V6          string   `json:"v6"`
	Rules       int      `json:"rules"`
	RulesText   string   `json:"rules_text"` // editable representation for the builder
	Floor       bool     `json:"floor"`
	Satisfiable bool     `json:"satisfiable"`
	Active      bool     `json:"active"`
	// Claim is surfaced because this is the one pattern property that can MOVE AN ADDRESS. A
	// feature whose whole risk is "it changes which adapter answers to an IP" must be visible in
	// the UI, or the operator cannot tell a pattern carries one. Nil when no claim is declared.
	Claim *Claim `json:"claim,omitempty"`
}
type stateView struct {
	Uplinks   []uplinkView  `json:"uplinks"`
	APs       []apView      `json:"aps"`
	WifiIf    []string      `json:"wifi_if"`
	Rules     []ruleView    `json:"rules"`
	Bridges   []bridgeInfo  `json:"bridges"`
	DefaultV4 string        `json:"default_v4"`
	DefaultV6 string        `json:"default_v6"`
	Patterns  []patternView `json:"patterns"`
	Armed     string        `json:"armed"`
	Active    string        `json:"active"`

	// Version/Source are the artefact's own declaration, carried into the UI so the page can
	// say WHICH BUILD drew it. Until now netgov declared its version only over `--version`, so
	// the dashboard could not answer "is this the build I just installed?" — and the operator
	// asked exactly that. Served from /api/state (Cache-Control: no-store), so a stale page
	// cannot show a stale version: the two are fetched together or not at all.
	Version string `json:"version"`
	Source  string `json:"source"`

	// ClaimArmed is the ADDRESS ARBITER's arm flag — deliberately separate from Armed above,
	// which is the pattern failover loop. Two switches, one word "arm": the operator armed the
	// loop on 2026-08-13 and reasonably believed arbitration was live. The UI must show them
	// apart, so the state that decides whether an address can MOVE is never inferred from the
	// state of something else.
	ClaimArmed bool `json:"claim_armed"`

	// ClaimEnforcing is the CONJUNCTION, not the arm flag. c-016: "armed=true is not a state, it
	// is an intention." A badge that reads ARMED while the hook is missing or cannot execute is
	// the same lie the CLI used to tell, one surface along.
	ClaimEnforcing bool     `json:"claim_enforcing"`
	ClaimChecks    []string `json:"claim_checks"`

	// ClaimPaths is the route disposition next to the holder: which adapter the host actually
	// TALKS on. Enforcing tells you arbitration works; this tells you what that bought. They can
	// disagree — on .153 on 2026-08-14 the arbiter was enforcing correctly and every packet the
	// box originated left on the other adapter. ClaimPathSplit is that condition as one flag, so
	// the badge does not have to grep prose. (2.20)
	ClaimPaths     []string `json:"claim_paths"`
	ClaimPathSplit bool     `json:"claim_path_split"`

	// MetricGovernance says whether netgov or NetworkManager actually decides the path. The
	// version tells you the capability is installed; this tells you it governs. (2.22, c-001)
	MetricGovernance []string `json:"metric_governance"`
}

// patternRulesText renders a pattern's rules as one "selector via [fam]" line each
// (selector = domain, or "from:CIDR"); parsePatternRules is the inverse.
func patternRulesText(rs []Rule) string {
	var b strings.Builder
	for _, r := range rs {
		sel := r.Domain
		if sel == "" {
			sel = "from:" + r.From
		}
		fmt.Fprintf(&b, "%s %s %s\n", sel, r.Via, dash(r.Fam))
	}
	return b.String()
}

// parsePatternRules parses the builder textarea back into Rules.
func parsePatternRules(s string) []Rule {
	var out []Rule
	for _, ln := range strings.Split(s, "\n") {
		f := strings.Fields(strings.TrimSpace(ln))
		if len(f) < 2 {
			continue
		}
		fam := "both"
		if len(f) >= 3 && f[2] != "-" {
			fam = f[2]
		}
		if strings.HasPrefix(f[0], "from:") {
			out = append(out, Rule{From: strings.TrimPrefix(f[0], "from:"), Via: f[1], Fam: fam})
		} else {
			out = append(out, Rule{Domain: f[0], Via: f[1], Fam: fam})
		}
	}
	return out
}

func famOf(u Uplink, fam string) famView {
	src, gw, live := uplinkLive(u, fam)
	fv := famView{Up: live, Src: src, GW: gw}
	if live {
		fv.Internet = pingVia(src, fam)
	}
	return fv
}

func buildView() stateView {
	st := loadState(statePath())
	v := stateView{DefaultV4: st.DefaultV4, DefaultV6: st.DefaultV6, Bridges: scanBridges(), WifiIf: wifiIfaces(),
		Armed: st.Armed, Active: st.ActivePattern,
		Version: artefactVersion, Source: artefactSource(), ClaimArmed: claimArmed()}
	v.ClaimEnforcing, v.ClaimChecks = claimEnforcement()
	v.MetricGovernance = governanceLines(st)
	if cl := claimForActive(st); cl != nil {
		v.ClaimPaths = claimPaths(cl, currentHolder(cl))
		for _, l := range v.ClaimPaths {
			if strings.Contains(l, "HOLDS is not PATH") {
				v.ClaimPathSplit = true
			}
		}
	}
	for _, p := range patternsByPrio(st) {
		v4, v6 := normDefault(p.V4), normDefault(p.V6)
		if v4 == "" {
			v4 = "direct"
		}
		if v6 == "" {
			v6 = "direct"
		}
		v.Patterns = append(v.Patterns, patternView{
			Name: p.Name, Priority: p.Priority, Require: p.Require, SSID: p.SSID, SSIDIface: p.SSIDIface,
			APs: p.APs, V4: v4, V6: v6, Rules: len(p.Rules), RulesText: patternRulesText(p.Rules),
			Floor: p.Floor, Satisfiable: patternSatisfiable(st, p), Active: p.Name == st.ActivePattern,
			Claim: p.Claim,
		})
	}
	for _, u := range st.Uplinks {
		if servingDev(st, u.Dev) {
			continue // shadowed by an AP
		}
		v.Uplinks = append(v.Uplinks, uplinkView{Name: u.Name, Dev: u.Dev, Table: u.Table, V4: famOf(u, "4"), V6: famOf(u, "6"), CanDefault: u.CanDefault})
	}
	for _, a := range st.APs {
		v.APs = append(v.APs, apView{Name: a.Name, Dev: a.Dev, SSID: a.SSID, Band: a.Band, On: a.On, Active: apActive(a.Dev) == "up"})
	}
	// A nil slice marshals as JSON `null`, not `[]`. That is how ms-rosy's uplink-less dashboard
	// died: ulOpts() mapped over a null uplinks array, threw, and every render step AFTER it never
	// ran — the patterns table, the claim badge, and the version badge the operator had asked
	// about four times. One box configured for pure arbitration (no uplinks, by our own design)
	// was therefore the one box whose safety surface silently blanked.
	//
	// Guarding the client is necessary but this is the fix that generalises: EVERY consumer of
	// /api/state — this page, the dashboard tiles, any script — gets an array where the schema
	// says array. An empty collection is a fact; null is an absence, and they are not the same.
	if v.Uplinks == nil {
		v.Uplinks = []uplinkView{}
	}
	if v.APs == nil {
		v.APs = []apView{}
	}
	if v.Patterns == nil {
		v.Patterns = []patternView{}
	}
	if v.Bridges == nil {
		v.Bridges = []bridgeInfo{}
	}
	if v.WifiIf == nil {
		v.WifiIf = []string{}
	}

	for _, r := range st.Rules {
		rv := ruleView{Via: r.Via, Fam: r.Fam}
		if r.Domain != "" {
			rv.Kind, rv.Sel, rv.Key, rv.IPs = "dest", r.Domain, r.Domain, resolveFam(r.Domain, "4")
		} else {
			rv.Kind, rv.Sel, rv.Key = "src", r.From, "from:"+r.From
		}
		v.Rules = append(v.Rules, rv)
	}
	if v.Rules == nil {
		v.Rules = []ruleView{}
	}
	return v
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Live state must never be cached: a stale /api/state would show the operator a network
	// posture that no longer exists, which is worse than showing him nothing.
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// serveStatic serves an in-binary asset with a cache VALIDATOR.
//
// WHY: the page is a static shell compiled into the binary and served from a URL that never
// changes, so with no ETag/Last-Modified/Cache-Control a browser may heuristically cache it and a
// normal reload legitimately serves the PRE-DEPLOY HTML. Meanwhile /api/state updates fine — so
// the operator gets fresh data rendered by stale JavaScript, and "deployed correctly" becomes
// visually identical to "did not deploy at all". That happened twice on 2026-08-12 (c-001, n-214):
// the second time the UI really had changed, everything server-side was correct, and he still
// could not see it. A deploy whose success is indistinguishable from its failure is not shippable.
//
// The ETag is a hash of the CONTENT, not of the build commit: it then changes exactly when the UI
// changes, so rebuilds that do not touch the page still answer 304 instead of forcing a re-fetch.
// `no-cache` means REVALIDATE (not "don't cache"), so the normal case stays a cheap 304.
func serveStatic(w http.ResponseWriter, r *http.Request, ctype, body string) {
	etag := etagOf(body)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", ctype)
	// A client may send several validators, or `*`; match any of them.
	for _, c := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		if c = strings.TrimSpace(c); c == etag || c == "*" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	_, _ = w.Write([]byte(body))
}

func etagOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return `"` + hex.EncodeToString(h[:8]) + `"`
}

func cmdWeb(st *State, args []string) {
	addr := "127.0.0.1:8474"
	if st.WebAddr != "" {
		addr = st.WebAddr
	}
	if v, ok := flagVal(args, "--addr"); ok {
		addr = v
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") && !strings.HasPrefix(addr, "localhost:") {
		fmt.Fprintln(os.Stderr, "refusing non-localhost bind (host-ports-nuc12wsh-b contract):", addr)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveStatic(w, r, "text/html; charset=utf-8", pageHTML)
	})
	mux.HandleFunc("/help", func(w http.ResponseWriter, r *http.Request) {
		serveStatic(w, r, "text/html; charset=utf-8", helpHTML)
	})
	mux.HandleFunc("/api/help", func(w http.ResponseWriter, r *http.Request) {
		serveStatic(w, r, "text/plain; charset=utf-8", opHelpMD+"\n\n---\n\n"+uiHelpMD+"\n\n---\n\n"+cliHelpMD)
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, buildView()) })

	mux.HandleFunc("/api/uplink", func(w http.ResponseWriter, r *http.Request) {
		s := loadState(statePath())
		_ = r.ParseForm()
		switch r.FormValue("action") {
		case "define":
			name, dev, gw := r.FormValue("name"), r.FormValue("dev"), r.FormValue("gw")
			t := nextTable(s)
			if tv := r.FormValue("table"); tv != "" {
				t, _ = strconv.Atoi(tv)
			}
			if u := upByName(s, name); u != nil {
				if dev != "" {
					u.Dev = dev
				}
				u.Table = t
				if gw != "" {
					u.Gateway = gw
				}
			} else {
				s.Uplinks = append(s.Uplinks, Uplink{Name: name, Dev: dev, Table: t, Gateway: gw})
			}
		case "default-route":
			// Tri-state, and "auto" must stay reachable from the UI: it is the only way to hand a
			// property back to NetworkManager once netgov has been asked to hold it. (2.21)
			if u := upByName(s, r.FormValue("name")); u != nil {
				switch r.FormValue("value") {
				case "yes":
					t := true
					u.CanDefault = &t
				case "no":
					f := false
					u.CanDefault = &f
				default:
					u.CanDefault = nil
				}
			}
		case "del":
			name := r.FormValue("name")
			var keep []Uplink
			for _, u := range s.Uplinks {
				if u.Name != name {
					keep = append(keep, u)
				}
			}
			s.Uplinks = keep
		}
		_ = saveState(s, statePath())
		writeJSON(w, buildView())
	})

	mux.HandleFunc("/api/ap", func(w http.ResponseWriter, r *http.Request) {
		s := loadState(statePath())
		_ = r.ParseForm()
		name := r.FormValue("name")
		switch r.FormValue("action") {
		case "save": // DEFINE/update a named AP (does not activate it)
			if name == "" {
				writeJSON(w, map[string]any{"ok": false, "out": "name required", "state": buildView()})
				return
			}
			a := apByName(s, name)
			if a == nil {
				s.APs = append(s.APs, AP{Name: name})
				a = &s.APs[len(s.APs)-1]
			}
			if dev := resolveDev(s, r.FormValue("dev")); dev != "" && isWifi(dev) {
				a.Dev = dev
				ensureUplink(s, dev)
			}
			if v := r.FormValue("ssid"); v != "" {
				a.SSID = v
			}
			if v := r.FormValue("psk"); v != "" {
				a.PSK = v
			}
			if v := r.FormValue("band"); v != "" {
				a.Band = v
			}
			if a.Band == "" {
				a.Band = "bg"
			}
			if v := r.FormValue("channel"); v != "" {
				a.Channel, _ = strconv.Atoi(v)
			}
			_ = saveState(s, statePath())
			writeJSON(w, buildView())
		case "on":
			err := apActivateOne(s, name)
			_ = saveState(s, statePath())
			out := ""
			if err != nil {
				out = err.Error()
			}
			writeJSON(w, map[string]any{"ok": err == nil, "out": out, "state": buildView()})
		case "off":
			if a := apByName(s, name); a != nil {
				_, _ = apDown(a.Dev)
				a.On = false
			}
			_ = saveState(s, statePath())
			writeJSON(w, map[string]any{"ok": true, "state": buildView()})
		case "del":
			var keep []AP
			for _, x := range s.APs {
				if x.Name == name {
					if x.On {
						_, _ = apDown(x.Dev)
					}
				} else {
					keep = append(keep, x)
				}
			}
			s.APs = keep
			_ = saveState(s, statePath())
			writeJSON(w, buildView())
		default:
			writeJSON(w, buildView())
		}
	})
	mux.HandleFunc("/api/rule", func(w http.ResponseWriter, r *http.Request) {
		s := loadState(statePath())
		_ = r.ParseForm()
		dom, from := r.FormValue("domain"), r.FormValue("from")
		switch r.FormValue("action") {
		case "add":
			fam := r.FormValue("fam")
			if fam == "" {
				fam = "both"
			}
			var keep []Rule
			for _, x := range s.Rules {
				if (dom != "" && x.Domain == dom) || (from != "" && x.From == from) {
					continue
				}
				keep = append(keep, x)
			}
			s.Rules = append(keep, Rule{Domain: dom, From: from, Via: r.FormValue("via"), Fam: fam})
		case "del":
			var keep []Rule
			for _, x := range s.Rules {
				if (dom != "" && x.Domain == dom) || (from != "" && x.From == from) {
					continue
				}
				keep = append(keep, x)
			}
			s.Rules = keep
		}
		_ = saveState(s, statePath())
		writeJSON(w, buildView())
	})

	mux.HandleFunc("/api/default", func(w http.ResponseWriter, r *http.Request) {
		s := loadState(statePath())
		_ = r.ParseForm()
		if v := r.FormValue("v4"); v != "" || r.Form.Has("v4") {
			s.DefaultV4 = v
		}
		if v := r.FormValue("v6"); v != "" || r.Form.Has("v6") {
			s.DefaultV6 = v
		}
		_ = saveState(s, statePath())
		writeJSON(w, buildView())
	})

	mux.HandleFunc("/api/link", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		var out string
		var err error
		switch r.FormValue("action") {
		case "reapply":
			out, err = run("nmcli", "device", "reapply", r.FormValue("dev"))
		default:
			out, err = run("nmcli", "con", r.FormValue("action"), r.FormValue("name"))
		}
		writeJSON(w, map[string]any{"ok": err == nil, "out": out})
	})

	mux.HandleFunc("/api/pattern", func(w http.ResponseWriter, r *http.Request) {
		s := loadState(statePath())
		_ = r.ParseForm()
		switch r.FormValue("action") {
		case "set":
			name := r.FormValue("name")
			if name == "" {
				writeJSON(w, map[string]any{"ok": false, "out": "name required", "state": buildView()})
				return
			}
			// Parse the claim BEFORE mutating anything, so a bad claimant spec rejects the whole
			// save rather than leaving a half-applied pattern behind.
			var claim *Claim
			claimGiven := r.Form.Has("claim")
			if claimGiven {
				var cerr error
				claim, cerr = parseClaimForm(r.FormValue("claim"))
				if cerr != nil {
					writeJSON(w, map[string]any{"ok": false, "out": cerr.Error(), "state": buildView()})
					return
				}
			}
			prio, _ := strconv.Atoi(r.FormValue("priority"))
			p := patByName(s, name)
			if p == nil {
				s.Patterns = append(s.Patterns, Pattern{Name: name})
				p = &s.Patterns[len(s.Patterns)-1]
			}
			p.Priority = prio
			p.V4 = normDefault(r.FormValue("v4"))
			p.V6 = normDefault(r.FormValue("v6"))
			p.Require = splitCSV(r.FormValue("require"))
			p.SSID = strings.TrimSpace(r.FormValue("ssid"))
			p.SSIDIface = r.FormValue("ssid_iface")
			p.APs = splitCSV(r.FormValue("aps"))
			p.Rules = parsePatternRules(r.FormValue("rules"))
			// Only touch Claim when the client actually sent the field. A stale browser tab
			// (or any older client) that POSTs without it would otherwise SILENTLY WIPE a
			// configured claim — losing address arbitration as a side effect of editing a
			// pattern's priority.
			if claimGiven {
				p.Claim = claim
			}
			_ = saveState(s, statePath())
			writeJSON(w, buildView())
		case "del":
			var keep []Pattern
			for _, p := range s.Patterns {
				if p.Name != r.FormValue("name") {
					keep = append(keep, p)
				}
			}
			s.Patterns = keep
			_ = saveState(s, statePath())
			writeJSON(w, buildView())
		case "apply":
			p := patByName(s, r.FormValue("name"))
			if p == nil {
				writeJSON(w, map[string]any{"ok": false, "out": "no such pattern", "state": buildView()})
				return
			}
			activatePattern(s, p)
			_ = saveState(s, statePath())
			self, _ := os.Executable()
			cmd := exec.Command("sudo", "-A", self, "__apply", "--state", statePath())
			cmd.Env = askpassEnv()
			out, err := cmd.CombinedOutput()
			writeJSON(w, map[string]any{"ok": err == nil, "out": string(out), "state": buildView()})
		case "eval":
			self, _ := os.Executable()
			cmd := exec.Command("sudo", "-A", self, "__eval-apply", "--state", statePath())
			cmd.Env = askpassEnv()
			out, err := cmd.CombinedOutput()
			writeJSON(w, map[string]any{"ok": err == nil, "out": string(out), "state": buildView()})
		default:
			writeJSON(w, buildView())
		}
	})

	// Arms/disarms the ADDRESS ARBITER only. Kept separate from /api/arm because the two arm
	// different things and conflating them in one endpoint is how they get conflated in a UI.
	mux.HandleFunc("/api/claim-arm", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		argv := []string{"rm", "-f", claimArmedFlag}
		if r.FormValue("mode") == "on" {
			argv = []string{"touch", claimArmedFlag}
		}
		cmd := exec.Command("sudo", append([]string{"-A"}, argv...)...)
		cmd.Env = askpassEnv()
		out, err := cmd.CombinedOutput()
		writeJSON(w, map[string]any{"ok": err == nil, "out": string(out), "state": buildView()})
	})

	mux.HandleFunc("/api/arm", func(w http.ResponseWriter, r *http.Request) {
		s := loadState(statePath())
		_ = r.ParseForm()
		mode := r.FormValue("mode") // armed | dry | off
		argv := []string{"systemctl", "enable", "--now", "netgov-roled.service"}
		if mode == "off" {
			s.Armed = ""
			argv = []string{"systemctl", "disable", "--now", "netgov-roled.service"}
		} else {
			ensureFloor(s)
			s.Armed = mode
		}
		_ = saveState(s, statePath())
		cmd := exec.Command("sudo", append([]string{"-A"}, argv...)...)
		cmd.Env = askpassEnv()
		out, err := cmd.CombinedOutput()
		writeJSON(w, map[string]any{"ok": err == nil, "out": string(out), "state": buildView()})
	})

	priv := func(verb string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			self, _ := os.Executable()
			cmd := exec.Command("sudo", "-A", self, verb, "--state", statePath())
			cmd.Env = askpassEnv()
			out, err := cmd.CombinedOutput()
			writeJSON(w, map[string]any{"ok": err == nil, "out": string(out)})
		}
	}
	mux.HandleFunc("/api/apply", priv("__apply"))
	mux.HandleFunc("/api/reset", priv("__reset"))

	fmt.Printf("netgov dashboard on http://%s\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func cmdInstall() {
	// THE BINARY PATH MUST COME FROM THE RUNNING BINARY, NOT FROM $HOME.
	//
	// This was filepath.Join(homeDir(), "bin", "netgov"). Under `sudo netgov install` homeDir() is
	// ROOT's home, so the dispatcher hook and the systemd unit were written pointing at
	// /root/bin/netgov — a path that does not exist. Both would then fail silently on every
	// carrier event: the hook redirects to a log nobody reads, and an armed arbiter would simply
	// never run. That is the same shape as the armed-but-unenforcing state 2.7 was written to end,
	// reintroduced through the install path, and it is invisible unless you read the generated
	// file. `sudo netgov install` is a plausible thing to type — OPERATING.md even documents that
	// install needs root — so this was reachable by following the instructions.
	//
	// os.Executable() names the binary that is actually running, whoever invoked it and however
	// PATH resolved. Fall back to the old derivation only if the kernel cannot tell us.
	bin := filepath.Join(homeDir(), "bin", "netgov")
	if exe, err := os.Executable(); err == nil && exe != "" {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != "" {
			bin = resolved
		} else {
			bin = exe
		}
	}
	// c-001's belt-and-braces (n-226): REFUSE to install a hook whose binary path does not exist.
	// os.Executable() makes that unreachable in practice, but the $HOME fallback above could still
	// produce a dead path, and the failure mode is silent by construction — the hook line ends in
	// `|| true`, so the dispatcher exits 0, systemctl looks clean, and the only trace is a log
	// nobody reads. Turning that into an install-time error is the difference between a tool that
	// cannot be misinstalled and one that merely usually isn't.
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintln(os.Stderr, "install REFUSED: the hook would point at "+bin+
			", which does not exist. Nothing written. Run install from the installed binary.")
		os.Exit(1)
	}
	sp := statePath()
	script := fmt.Sprintf(`#!/bin/sh
# netgov NM dispatcher hook — runs as root on link up/down.
case "$2" in up|down|vpn-up|vpn-down) ;; *) exit 0 ;; esac
LOG=/var/log/netgov-dispatch.log
LOCK=/run/netgov-claim.lock

# ---- 1. ADDRESS ARBITRATION (armed boxes only) --------------------------------------------
# THIS IS WHAT MAKES THE ARM FLAG MEAN SOMETHING. Arbitration used to run only when a PATTERN
# was activated, which is the wrong trigger: a cable dying does not change which pattern is
# satisfiable, so the floor stayed active, no activation happened, and the arbiter was never
# consulted on the one event it exists for. Pattern selection and claimant eligibility change
# on DIFFERENT events. This hook is the carrier event.
#
# Runs BEFORE the routing re-apply below: arbitration decides which adapter holds the claimed
# address, and the routing policy is computed from the addresses that result.
#
# "netgov claim apply" refuses unless /etc/netgov-claim.armed exists, and writes no state file,
# so this is inert on an unarmed box and cannot leave a root-owned state.json behind.
if [ -f /etc/netgov-claim.armed ]; then
  # Arbitration MOVES an address, which itself produces further dispatcher events, so the hook
  # can re-enter itself. mkdir is the atomic mutex. A stale lock (hook killed mid-run) must not
  # disable arbitration forever, so anything older than a minute is cleared first.
  [ -d "$LOCK" ] && find "$LOCK" -maxdepth 0 -mmin +1 -exec rmdir {} \; 2>/dev/null
  if mkdir "$LOCK" 2>/dev/null; then
    trap 'rmdir "$LOCK" 2>/dev/null' EXIT INT TERM
    echo "[$(date -Is)] $1 $2 -> claim apply" >> $LOG
    %s claim apply --state %s >> $LOG 2>&1 || true
  else
    echo "[$(date -Is)] $1 $2 -> claim apply SKIPPED (already in progress)" >> $LOG
  fi
fi

# ---- 2. re-apply host policy routing ------------------------------------------------------
%s __apply --state %s >> $LOG 2>&1 || true
`, bin, sp, bin, sp)
	// STAGE IN A PRIVATE RANDOM DIRECTORY, NOT FIXED NAMES IN /tmp.
	//
	// This used to write /tmp/90-netgov and /tmp/netgov-roled.service by fixed name. On a box with
	// `fs.protected_regular=2` (default on Ubuntu), a sticky world-writable directory refuses even
	// ROOT a write-open on a regular file it does not own — so once anyone had run `install`
	// unprivileged, every later `sudo netgov install` failed with "permission denied" on a path it
	// had itself created. c-001 hit it twice deploying 2.10.
	//
	// Three faults in one line: order-dependent (so intermittent, and it survived this long by
	// only ever being run one way on any given box), a symlink/pre-creation hazard in a
	// world-writable directory, and — worst — it could half-complete, leaving the hook updated
	// and the unit stale. For the one verb whose entire purpose is to make the hook match the
	// binary, a partial success is the most damaging outcome available.
	//
	// MkdirTemp gives a fresh 0700 directory owned by the caller: no name collision, not
	// world-writable, so protected_regular does not apply. Both artefacts are written BEFORE
	// anything is installed, so a write failure costs nothing on the live system.
	stage, err := os.MkdirTemp("", "netgov-install-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "install failed: could not create staging dir:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(stage)

	tmp := filepath.Join(stage, "90-netgov")
	must(os.WriteFile(tmp, []byte(script), 0o755))
	dst := "/etc/NetworkManager/dispatcher.d/90-netgov"
	cmd := exec.Command("sudo", "-A", "install", "-m", "0755", "-o", "root", "-g", "root", tmp, dst)
	cmd.Env = append(os.Environ(), "SUDO_ASKPASS="+filepath.Join(homeDir(), "bin", "sudo-askpass-zenity"))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "install failed:", err)
		os.Exit(1)
	}
	fmt.Println("installed dispatcher hook:", dst)
	// The hook and unit are generated with THESE paths baked in. If install was run as a user
	// whose state file is not the one the daemon will use, say so now rather than let an armed
	// arbiter read an empty config on the next carrier event.
	if _, err := os.Stat(sp); err != nil {
		fmt.Fprintln(os.Stderr, "WARNING: baked state path does not exist: "+sp+
			" — run install as the user that owns netgov's state, or the hook will find no claim.")
	}

	// netgov-roled.service — the root failover loop. Installed but NOT enabled
	// (boots disarmed); `netgov arm` enables+starts it, `netgov disarm` stops it.
	unit := fmt.Sprintf(`[Unit]
Description=netgov roled failover loop (armed auto-pattern selection)
After=network-online.target NetworkManager.service
Wants=network-online.target

[Service]
ExecStart=%s roled-loop --state %s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, bin, sp)
	tmpU := filepath.Join(stage, "netgov-roled.service")
	must(os.WriteFile(tmpU, []byte(unit), 0o644))
	uDst := "/etc/systemd/system/netgov-roled.service"
	cmdU := exec.Command("sudo", "-A", "install", "-m", "0644", "-o", "root", "-g", "root", tmpU, uDst)
	cmdU.Env = askpassEnv()
	cmdU.Stdin, cmdU.Stdout, cmdU.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmdU.Run(); err != nil {
		// Say what DID land. "install failed" after the hook was already replaced leaves a reader
		// believing nothing changed, when in fact the box is now hook-new/unit-old.
		fmt.Fprintln(os.Stderr, "unit install failed:", err)
		fmt.Fprintln(os.Stderr, "PARTIAL INSTALL: the dispatcher hook at "+dst+" WAS replaced; the systemd unit was NOT. Re-run `netgov install` once the cause is cleared.")
		os.Exit(1)
	}
	reload := exec.Command("sudo", "-A", "systemctl", "daemon-reload")
	reload.Env = askpassEnv()
	reload.Stdin, reload.Stdout, reload.Stderr = os.Stdin, os.Stdout, os.Stderr
	_ = reload.Run()
	fmt.Println("installed systemd unit:", uDst, "(disarmed; enable with `netgov arm`)")
}

const pageHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>netgov</title><style>
:root{--bg:#0e0f12;--fg:#d7dae0;--mut:#7a8290;--ln:#2a2e36;--ok:#5fd68a;--no:#e06c75;--acc:#6fb3ff;--warn:#e5a24a}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:13px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
header{padding:14px 20px;border-bottom:1px solid var(--ln);display:flex;align-items:center;gap:14px}
h1{font-size:14px;font-weight:600;letter-spacing:.08em;margin:0}
main{max-width:980px;margin:0 auto;padding:20px}
section{border:1px solid var(--ln);border-radius:6px;margin:0 0 18px;overflow:hidden}
section>h2{font-size:11px;letter-spacing:.12em;color:var(--mut);text-transform:uppercase;margin:0;padding:9px 14px;border-bottom:1px solid var(--ln);font-weight:600}
section.danger{border-color:#5a2b2b}section.danger>h2{color:var(--no);border-color:#5a2b2b}
table{width:100%;border-collapse:collapse}td,th{padding:7px 14px;text-align:left;font-weight:400;border-bottom:1px solid var(--ln);vertical-align:middle}
th{color:var(--mut);font-size:11px}tr:last-child td{border-bottom:none}
.pill{display:inline-block;padding:0 7px;border-radius:10px;border:1px solid var(--ln);font-size:11px}
.up{color:var(--ok);border-color:#274}.down{color:var(--mut)}
.mut{color:var(--mut)}.acc{color:var(--acc)}.warn{color:var(--warn)}
button{background:transparent;color:var(--fg);border:1px solid var(--ln);border-radius:4px;padding:3px 10px;font:inherit;cursor:pointer}
button:hover{border-color:var(--acc);color:var(--acc)}button.go{border-color:#274;color:var(--ok)}button.bad{border-color:#5a2b2b;color:var(--no)}
button.bad:hover{border-color:var(--no)}
input,select{background:#16181d;color:var(--fg);border:1px solid var(--ln);border-radius:4px;padding:4px 7px;font:inherit}
.row{display:flex;gap:8px;align-items:center;padding:10px 14px;flex-wrap:wrap;border-top:1px solid var(--ln)}
#log{white-space:pre-wrap;color:var(--mut);padding:10px 14px;font-size:12px}
small{color:var(--mut)}
</style></head><body>
<header><h1>NETGOV</h1><span class="mut" id="ver" title="the build that drew this page">…</span><span class="mut" id="sub">host switchboard</span>
<span style="flex:1"></span><button class="go" onclick="apply()">APPLY ▸</button>
<button onclick="load()" title="refresh status">↻ refresh</button>
<a href="/help" target="_blank" title="open the help page" style="color:var(--mut);border:1px solid var(--ln);border-radius:4px;padding:3px 10px;text-decoration:none;margin-left:6px">? help</a></header>
<main>
<section><h2>Uplinks</h2><table id="ut"><thead><tr><th>name</th><th>iface</th>
<th>IPv4</th><th>IPv6</th><th>tbl</th><th title="may this uplink carry the DEFAULT ROUTE? netgov owns ipv4.never-default when this is yes/no; auto leaves it to NetworkManager">default route</th><th></th></tr></thead><tbody></tbody></table>
<div class="row"><span class="mut">define:</span><input id="un" placeholder="name" size="8">
<input id="ud" placeholder="iface" size="14"><input id="ug" placeholder="gateway (optional)" size="15">
<button onclick="defUp()">+ uplink</button></div></section>

<section><h2>Access points — named definitions (a pattern picks one by name)</h2>
<table id="at"><thead><tr><th>name</th><th>radio</th><th>SSID</th><th>band</th><th>state</th><th></th></tr></thead><tbody></tbody></table>
<div class="row"><input id="apn" placeholder="name e.g. CNNet" size="10"><select id="aif" title="Wi-Fi radio"></select>
<input id="assid" placeholder="SSID" size="12"><input id="apsk" placeholder="passphrase (≥8)" size="14">
<select id="aband"><option value="bg">2.4GHz</option><option value="a">5GHz</option></select>
<button onclick="apSave()">save</button></div>
<small style="display:block;padding:2px 14px 10px">Saving <b>defines</b> an AP — it doesn't switch on. Use <b>on/off</b> for a standalone AP, or list it in a pattern's “AP on”. One AP per radio at a time; turning one on shadows that radio's uplink.</small></section>

<section><h2>Destination rules — domain → uplink</h2><table id="rt"><tbody></tbody></table>
<div class="row"><input id="rd" placeholder="domain e.g. example.com" size="26"><span class="mut">→</span>
<select id="rv"></select><select id="rf"></select><button onclick="addDest()">+ pin</button></div></section>

<section><h2>Source rules — containers / VMs / subnet → uplink</h2><table id="st"><tbody></tbody></table>
<div class="row"><select id="sfrom"></select><span class="mut">→</span><select id="sv"></select>
<select id="sf"></select><button onclick="addSrc()">+ pin</button>
<small id="brs"></small></div></section>

<section><h2>Overall default — unpinned traffic</h2><div class="row">
<span class="mut">IPv4 →</span><select id="d4" onchange="setDef()"></select>
<span class="mut" style="margin-left:14px">IPv6 →</span><select id="d6" onchange="setDef()"></select>
<small style="margin-left:14px">local/LAN always stays direct · “block” = drop (leak-protect)</small></div></section>

<section><h2>Patterns — roled <span id="armbadge"></span></h2><table id="pt"><thead><tr>
<th>pri</th><th>name</th><th>trigger</th><th>v4</th><th>v6</th><th>rules/AP</th><th>state</th><th></th></tr></thead><tbody></tbody></table>
<div class="row"><span class="mut">automation:</span>
<button class="go" onclick="arm('armed')">Arm</button><button onclick="arm('dry')">Dry-run</button>
<button class="bad" onclick="arm('off')">Disarm</button><button onclick="evalNow()" title="re-evaluate &amp; apply the best pattern now">↻ eval now</button>
<small>armed = a root loop auto-selects the best satisfiable + internet-validated pattern (poll + debounce)</small></div>
<div class="row"><span class="mut">address arbitration:</span> <span id="clbadge"></span>
<button class="go" onclick="claimArm(true)" title="allow the active pattern's claim to move its address between adapters">Arm arbitration</button>
<button class="bad" onclick="claimArm(false)">Disarm</button>
<small id="clnote">separate from the loop above — this is what lets a claim MOVE AN ADDRESS between adapters</small></div>
<div class="row" style="align-items:flex-start;flex-wrap:wrap">
<input id="pn" placeholder="name" size="8"><input id="pp" placeholder="prio" size="4" value="50">
<span class="mut">v4</span><select id="pv4"></select><span class="mut">v6</span><select id="pv6"></select>
<span class="mut">require up</span><select id="prq" multiple size="2" style="min-width:74px" title="uplinks that must be UP"></select>
<span class="mut">SSID</span><input id="pssid" placeholder="e.g. Motionlab-Member" size="17"><span class="mut">on</span><select id="pssidif" title="Wi-Fi uplink to scan"></select>
<span class="mut">AP on</span><select id="pap" multiple size="2" style="min-width:74px" title="access points to keep up"></select>
<textarea id="prules" rows="3" cols="26" placeholder="rules, one per line:&#10;api.anthropic.com WiFi0&#10;from:172.18.0.0/16 cable"></textarea>
<input id="pclaim" placeholder="claim: 192.168.222.153 enp114s0:100 wlo1:50:CNNet" size="46" title="Same-address arbitration for THIS pattern: one address, then one dev:priority[:ssid,ssid] per adapter. Highest-priority ELIGIBLE adapter (carrier + associated to a listed SSID) holds the address; the others hold none. Leave blank for no claim. Inert until the pattern is active AND the claim arbiter is armed.">
<button onclick="patSnapshot()" title="fill v4/v6/rules + active AP from the CURRENT live config">↧ snapshot current</button>
<button onclick="patSave()">+ save</button></div>
<small style="display:block;padding:2px 14px 10px">trigger = required uplinks UP <i>and</i> (optional) an SSID in range on the chosen Wi-Fi. Uplink details live in NetworkManager / OS settings; APs in the card above. A “floor” fallback is auto-added. Click <b>edit</b> on a row to load it here.</small></section>

<section class="danger"><h2>Restore</h2><div class="row">
<button class="bad" onclick="reset()">⟲ Restore to NetworkManager</button>
<small>removes ALL netgov rules &amp; tables → the OS/NM baseline reappears (netgov never edits NM itself)</small></div></section>

<section><h2>Log</h2><div id="log">—</div></section>
</main>
<script>
let S={uplinks:[],aps:[],wifi_if:[],rules:[],bridges:[],default_v4:"",default_v6:"",patterns:[],armed:"",active:""};
const $=s=>document.querySelector(s);
function ulOpts(cur,extra){let o=(extra||[]).map(e=>'<option value="'+e[0]+'" '+(e[0]===cur?'selected':'')+'>'+e[1]+'</option>').join('');
 return o+(S.uplinks||[]).map(u=>'<option '+(u.name===cur?'selected':'')+'>'+u.name+'</option>').join('')}
function famOpts(cur){return ['both','4','6'].map(f=>'<option '+(f===(cur||'both')?'selected':'')+'>'+f+'</option>').join('')}
function fcell(f){if(!f.up)return '<span class="down">—</span>';
 return '<span class="up">up</span> <span class="mut">'+(f.src||'-')+' gw '+(f.gw||'-')+'</span> '+(f.internet?'<span class="pill up">internet ✓</span>':'<span class="pill warn">no internet</span>')}
// A THROWN ERROR IN render() USED TO BLANK EVERYTHING AFTER THE THROW POINT, SILENTLY.
// On a host with no uplinks, ulOpts() mapped over a null uplinks array and every later step — patterns
// table, claim badge, version badge — simply never ran. The page looked deployed-but-empty, which
// is indistinguishable from a bad deploy, and on the one box configured for pure arbitration it
// hid the badge that says whether the arbiter can move an address.
//
// The null guards below and in buildView fix THAT instance; this catch fixes the CLASS. A render
// bug must announce itself rather than truncate the page: partial UI that looks whole is the worst
// failure mode a status dashboard has.
function render(){
 try{ renderBody() }catch(e){
   log('RENDER ERROR: '+(e&&e.message?e.message:e)+' — the page below may be incomplete. This is a bug; the data in /api/state is unaffected.');
   const v=$('#ver'); if(v&&S&&S.version)v.textContent=S.version;
   throw e
 }
}
function renderBody(){
 // "default route" is a SELECT, not a checkbox: the property is tri-state and "auto" (hand it
 // back to NetworkManager) is a real, reachable choice, not the absence of one. A checkbox would
 // collapse unmanaged and no into the same unticked box. (2.21)
 $('#ut tbody').innerHTML=(S.uplinks||[]).map(u=>{
   const cd=(u.can_default===true?'yes':(u.can_default===false?'no':'auto'));
   const sel='<select title="may this uplink carry the DEFAULT ROUTE? netgov owns ipv4.never-default; auto = leave it to NetworkManager. Takes effect on apply; netgov reset restores the original." onchange="setDefRoute(\''+u.name+'\',this.value)">'
     +['yes','no','auto'].map(v=>'<option value="'+v+'"'+(v===cd?' selected':'')+'>'+(v==='auto'?'auto':v)+'</option>').join('')+'</select>';
   return '<tr><td class="acc">'+u.name+'</td><td>'+u.dev+'</td><td>'+fcell(u.v4)+
  '</td><td>'+fcell(u.v6)+'</td><td class="mut">'+u.table+'</td><td>'+sel+'</td><td><button title="restore link to NM profile" onclick="reapply(\''+u.dev+'\')">↺</button> <button onclick="delUp(\''+u.name+'\')">×</button></td></tr>'}).join('')||'<tr><td colspan=7 class=mut>none — run “netgov init”</td></tr>';
 const dr=(S.rules||[]).filter(r=>r.kind==='dest');
 $('#rt tbody').innerHTML=(dr.length?dr:[]).map(r=>'<tr><td>'+r.sel+'</td><td class="acc">'+r.via+'</td><td class="mut">['+r.fam+']</td><td class="mut">'+((r.ips||[]).join(' ')||'unresolved')+'</td><td><button onclick="delRule({domain:\''+r.sel+'\'})">×</button></td></tr>').join('')||'<tr><td class=mut colspan=5>none</td></tr>';
 const sr=(S.rules||[]).filter(r=>r.kind==='src');
 $('#st tbody').innerHTML=(sr.length?sr:[]).map(r=>'<tr><td>'+r.sel+'</td><td class="acc">'+r.via+'</td><td class="mut">['+r.fam+']</td><td><button onclick="delRule({from:\''+r.sel+'\'})">×</button></td></tr>').join('')||'<tr><td class=mut colspan=4>none</td></tr>';
 $('#at tbody').innerHTML=(S.aps||[]).map(a=>'<tr><td class="acc">'+a.name+'</td><td class="mut">'+a.dev+'</td><td>'+a.ssid+'</td><td class="mut">'+(a.band==='a'?'5G':'2.4G')+'</td><td>'+(a.on?'<span class="pill up">ON</span>'+(a.active?'':' <span class="warn">…</span>'):'<span class="pill">off</span>')+'</td><td>'+(a.on?'<button onclick="apOff(\''+a.name+'\')">off</button>':'<button class="go" onclick="apOn(\''+a.name+'\')">on</button>')+' <button onclick="apEdit(\''+a.name+'\')">edit</button> <button onclick="apDel(\''+a.name+'\')">×</button></td></tr>').join('')||'<tr><td class=mut colspan=6>none — define one below (saving defines it; “on” or a pattern activates it)</td></tr>';
 $('#aif').innerHTML=(S.wifi_if||[]).map(d=>'<option>'+d+'</option>').join('');
 $('#rv').innerHTML=ulOpts('',[['block','block']]);$('#rf').innerHTML=famOpts();
 $('#sv').innerHTML=ulOpts('',[['block','block']]);$('#sf').innerHTML=famOpts();
 $('#sfrom').innerHTML=(S.bridges||[]).map(b=>'<option value="'+b.name+'">'+b.name+(b.subnet?' ('+b.subnet+')':'')+'</option>').join('')+'<option value="">— custom CIDR —</option>';
 $('#brs').textContent=(S.bridges||[]).length?'':'(no container/VM bridges detected)';
 $('#d4').innerHTML=ulOpts(S.default_v4,[['','(none)'],['block','block']]);
 $('#d6').innerHTML=ulOpts(S.default_v6,[['','(none)'],['block','block']]);
 $('#pt tbody').innerHTML=(S.patterns||[]).map(p=>{let trig=[...(p.require||[]),(p.ssid?'📶'+p.ssid:'')].filter(Boolean).join(' ')||'-';let ra=p.rules+(p.aps&&p.aps.length?' +AP':'');if(p.claim){ra+=' <span class="pill warn" title="same-address arbitration: '+p.claim.address+'">⇄'+p.claim.address+'</span>'}return '<tr><td class=mut>'+p.priority+'</td><td class=acc>'+p.name+(p.floor?' <span class=mut>(floor)</span>':'')+'</td><td class=mut>'+trig+'</td><td>'+p.v4+'</td><td>'+p.v6+'</td><td class=mut>'+ra+'</td><td>'+(p.active?'<span class="pill up">ACTIVE</span> ':'')+(p.satisfiable?'<span class="pill up">ok</span>':'<span class="pill warn">not-now</span>')+'</td><td><button onclick="patApply(\''+p.name+'\')">activate</button> <button onclick="patEdit(\''+p.name+'\')">edit</button> <button onclick="patDel(\''+p.name+'\')">×</button></td></tr>'}).join('')||'<tr><td class=mut colspan=8>none — build one below (a floor is auto-added on arm)</td></tr>';
 $('#armbadge').innerHTML=S.armed?'<span class="pill up">ARMED · '+S.armed+'</span>':'<span class="pill">disarmed</span>';
 // The arbiter badge names the ADDRESS at stake, not just a state: "armed" alone does not tell
 // you what it is allowed to move, and that is the only fact that matters before arming.
 {const ap=(S.patterns||[]).find(p=>p.active), cl=ap&&ap.claim;
  $('#clbadge').innerHTML = !S.claim_armed ? '<span class="pill">disarmed</span>'
    : (S.claim_enforcing ? '<span class="pill up">ARMED · enforcing</span>'
                         : '<span class="pill" style="color:var(--warn);border-color:var(--warn)">ARMED · NOT ENFORCING</span>');
  if(S.claim_armed&&!S.claim_enforcing){$('#clbadge').title=(S.claim_checks||[]).join(' | ')}
  // ENFORCING is about the arbiter; the split is about the host. Both green and still every
  // packet leaving on the other adapter is a real state (.153, 2026-08-14), so it gets its own
  // badge rather than a footnote — the whole failure was a true verdict being read as more.
  if(S.claim_path_split){$('#clbadge').innerHTML += ' <span class="pill" style="color:var(--warn);border-color:var(--warn)" title="'+(S.claim_paths||[]).join(' | ').replace(/"/g,'&quot;')+'">HOLDS ≠ PATH</span>'}
  $('#clnote').textContent = cl
    ? 'active pattern '+ap.name+' can move '+cl.address+' between '+(cl.claimants||[]).map(c=>c.dev).join(' / ')
      +((S.claim_paths||[]).length?' — '+S.claim_paths.filter(l=>l.indexOf('path:')>=0).map(l=>l.trim()).join('; '):'')
    : 'no claim on the active pattern — arming changes nothing until one is declared';}
 $('#prq').innerHTML=(S.uplinks||[]).map(u=>'<option>'+u.name+'</option>').join('');
 $('#pssidif').innerHTML=(S.uplinks||[]).map(u=>'<option'+(u.name==='WiFi0'?' selected':'')+'>'+u.name+'</option>').join('');
 $('#pap').innerHTML=(S.aps||[]).map(a=>'<option value="'+a.name+'">'+a.name+' ('+a.ssid+'@'+a.dev+')</option>').join('')||'<option disabled>no APs — define one in the AP card</option>';
 $('#pv4').innerHTML=ulOpts('direct',[['direct','direct'],['block','block']]);
 $('#pv6').innerHTML=ulOpts('block',[['direct','direct'],['block','block']]);
 // The version badge is drawn from the SAME /api/state payload as the data below it, so a
 // cached page cannot show a fresh version (or vice versa) — they arrive together or not at all.
 if(S.version){const v=$('#ver');v.textContent=S.version;v.title=(S.source||'')+' — the build that drew this page'}
 $('#sub').textContent='default v4='+(S.default_v4||'none')+'  v6='+(S.default_v6||'none')+(S.armed?'  · ARMED('+S.armed+')':'');
}
async function load(){S=await (await fetch('/api/state')).json();render()}
function log(m){$('#log').textContent=m}
async function post(u,d){return (await fetch(u,{method:'POST',body:new URLSearchParams(d)})).json()}
async function defUp(){S=await post('/api/uplink',{action:'define',name:$('#un').value,dev:$('#ud').value,gw:$('#ug').value});render();$('#un').value='';$('#ud').value='';$('#ug').value=''}
async function delUp(n){if(confirm('remove uplink '+n+'?')){S=await post('/api/uplink',{action:'del',name:n});render()}}
// Saves the PREFERENCE only. Deliberately does not apply: this writes an NM profile property, and
// the standing rule for this box is that anything which can flip routing is inspectable first.
async function setDefRoute(n,v){S=await post('/api/uplink',{action:'default-route',name:n,value:v});render();
 alert('Saved: '+n+' default-route='+v+'\n\nNot applied yet. Run "netgov plan" to see exactly what would change, then Apply.\n\nnetgov saves the profile original value the first time it takes it over, and "netgov reset" restores it.')}
async function addDest(){S=await post('/api/rule',{action:'add',domain:$('#rd').value,via:$('#rv').value,fam:$('#rf').value});render();$('#rd').value=''}
async function addSrc(){let f=$('#sfrom').value;if(f===''){f=prompt('source CIDR (e.g. 172.20.0.0/16) or interface name');if(!f)return}
 S=await post('/api/rule',{action:'add',from:f,via:$('#sv').value,fam:$('#sf').value});render()}
async function delRule(d){S=await post('/api/rule',Object.assign({action:'del'},d));render()}
async function setDef(){S=await post('/api/default',{v4:$('#d4').value,v6:$('#d6').value});render()}
async function apSave(){if(!$('#apn').value){alert('name required');return}const psk=$('#apsk').value;if(psk&&psk.length<8){alert('passphrase must be ≥8 chars');return}
 S=await post('/api/ap',{action:'save',name:$('#apn').value,dev:$('#aif').value,ssid:$('#assid').value,psk:psk,band:$('#aband').value});render();$('#apn').value='';$('#assid').value='';$('#apsk').value='';log('AP defined — switch it on here, or add it to a pattern')}
async function apOn(n){log('enabling AP '+n+'…');const r=await post('/api/ap',{action:'on',name:n});log(r.out||(r.ok?'AP up':'failed'));if(r.state){S=r.state;render()}else load()}
async function apOff(n){if(!confirm('switch AP '+n+' off?'))return;log('disabling AP '+n+'…');const r=await post('/api/ap',{action:'off',name:n});log(r.out||'done');if(r.state){S=r.state;render()}else load()}
function apEdit(n){const a=(S.aps||[]).find(x=>x.name===n);if(!a)return;$('#apn').value=a.name;[...$('#aif').options].forEach(o=>o.selected=o.value===a.dev);$('#assid').value=a.ssid;$('#apsk').value='';[...$('#aband').options].forEach(o=>o.selected=o.value===a.band);log('editing AP '+n+' (leave passphrase blank to keep it)')}
async function apDel(n){if(confirm('delete AP '+n+'?')){S=await post('/api/ap',{action:'del',name:n});render()}}
async function reapply(dev){log('reapplying '+dev+'…');const r=await post('/api/link',{action:'reapply',dev:dev});log(r.out||'done');load()}
async function arm(mode){log((mode==='off'?'disarming':'arming '+mode)+'… (approve the sudo dialog)');const r=await post('/api/arm',{mode:mode});log(r.out||(r.ok?'done':'failed'));if(r.state){S=r.state;render()}else load()}
async function claimArm(on){
 const ap=(S.patterns||[]).find(p=>p.active), cl=ap&&ap.claim;
 if(on){
   if(!cl){ if(!confirm('The active pattern carries no claim, so arming changes nothing right now. Arm anyway?')) return; }
   else if(!confirm('Arm address arbitration?\n\n'+cl.address+' may be MOVED between '+(cl.claimants||[]).map(c=>c.dev+' (prio '+c.priority+')').join(' and ')+'.\n\nThe holder keeps the address if no claimant is eligible, but a move will drop connections bound to the old adapter.')) return;
 }
 const r=await post('/api/claim-arm',{mode:on?'on':'off'});
 if(r.state)S=r.state; render(); log(r.ok?('arbitration '+(on?'ARMED':'disarmed')):('failed: '+(r.out||'')));
}
async function evalNow(){log('evaluating… (approve the sudo dialog)');const r=await post('/api/pattern',{action:'eval'});log(r.out||'done');if(r.state){S=r.state;render()}else load()}
async function patSave(){if(!$('#pn').value){alert('name required');return}
 let rq=[...$('#prq').selectedOptions].map(o=>o.value).join(',');
 let ap=[...$('#pap').selectedOptions].map(o=>o.value).join(',');
 S=await post('/api/pattern',{action:'set',name:$('#pn').value,priority:$('#pp').value||'50',require:rq,ssid:$('#pssid').value,ssid_iface:$('#pssidif').value,aps:ap,v4:$('#pv4').value,v6:$('#pv6').value,rules:$('#prules').value,claim:$('#pclaim').value});if(S && S.ok===false){alert('claim not saved: '+S.out);S=S.state}render();$('#pn').value='';$('#prules').value='';$('#pssid').value='';$('#pclaim').value=''}
function patSnapshot(){ // fill the builder's egress fields from the CURRENT live config
 $('#pv4').innerHTML=ulOpts(S.default_v4||'direct',[['direct','direct'],['block','block']]);
 $('#pv6').innerHTML=ulOpts(S.default_v6||'block',[['direct','direct'],['block','block']]);
 $('#prules').value=(S.rules||[]).filter(r=>r.sel).map(r=>(r.kind==='src'?'from:'+r.sel:r.sel)+' '+r.via+' '+(r.fam||'both')).join('\n');
 [...$('#pap').options].forEach(o=>o.selected=(S.aps||[]).some(a=>a.name===o.value&&a.on));
 log('snapshotted current egress into the builder — add a name + trigger, then + save')}
async function patDel(n){if(confirm('delete pattern '+n+'?')){S=await post('/api/pattern',{action:'del',name:n});render()}}
async function patApply(n){log('activating '+n+'… (approve the sudo dialog)');const r=await post('/api/pattern',{action:'apply',name:n});log(r.out||'done');if(r.state){S=r.state;render()}else load()}
function patEdit(n){const p=(S.patterns||[]).find(x=>x.name===n);if(!p)return;
 $('#pn').value=p.name;$('#pp').value=p.priority;
 $('#pclaim').value=p.claim?([p.claim.address].concat((p.claim.claimants||[]).map(c=>c.dev+':'+c.priority+(c.ssids&&c.ssids.length?':'+c.ssids.join(','):''))).join(' ')):'';
 $('#pv4').innerHTML=ulOpts(p.v4,[['direct','direct'],['block','block']]);$('#pv6').innerHTML=ulOpts(p.v6,[['direct','direct'],['block','block']]);
 [...$('#prq').options].forEach(o=>o.selected=(p.require||[]).includes(o.value));
 $('#pssid').value=p.ssid||'';[...$('#pssidif').options].forEach(o=>o.selected=(o.value===(p.ssid_iface||'WiFi0')));
 [...$('#pap').options].forEach(o=>o.selected=(p.aps||[]).includes(o.value));
 $('#prules').value=p.rules_text||'';window.scrollTo(0,document.body.scrollHeight)}
async function apply(){log('applying… (approve the sudo dialog on screen)');const r=await post('/api/apply',{});log(r.out||(r.ok?'applied':'failed'));load()}
async function reset(){if(!confirm('Remove ALL netgov rules and restore the NetworkManager baseline?'))return;log('restoring…');const r=await post('/api/reset',{});log(r.out||'done');load()}
load();
setInterval(()=>{const a=document.activeElement;if(a&&/^(INPUT|SELECT|TEXTAREA)$/.test(a.tagName))return;load()},15000);
</script></body></html>`

// helpHTML renders the embedded docs (UI.md + CLI.md) with a tiny client-side markdown
// converter (stdlib-only: no server-side renderer). Backticks are written as ` so
// this stays a valid Go raw string.
const helpHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>netgov · help</title><style>
:root{--bg:#0e0f12;--fg:#d7dae0;--mut:#7a8290;--ln:#2a2e36;--ok:#5fd68a;--no:#e06c75;--acc:#6fb3ff;--warn:#e5a24a}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.65 ui-sans-serif,system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
header{padding:14px 20px;border-bottom:1px solid var(--ln);display:flex;align-items:center;gap:14px;position:sticky;top:0;background:var(--bg);z-index:1}
h1{font-size:14px;font-weight:600;letter-spacing:.08em;margin:0}
a{color:var(--acc)}
main{max-width:820px;margin:0 auto;padding:8px 22px 60px}
#md h1{font-size:22px;border-bottom:1px solid var(--ln);padding-bottom:.3em;margin:1.5em 0 .6em}
#md h2{font-size:17px;color:var(--acc);margin:1.9em 0 .5em;border-bottom:1px solid var(--ln);padding-bottom:.2em}
#md h3{font-size:15px;margin:1.4em 0 .4em}
#md code{background:#16181d;border:1px solid var(--ln);border-radius:4px;padding:1px 5px;font:12.5px ui-monospace,Menlo,Consolas,monospace}
#md pre{background:#16181d;border:1px solid var(--ln);border-radius:6px;padding:12px 14px;overflow:auto}
#md pre code{background:none;border:none;padding:0}
#md blockquote{border-left:3px solid var(--warn);margin:1em 0;padding:.3em 14px;color:var(--mut)}
#md table{border-collapse:collapse;margin:1em 0;width:100%}
#md th,#md td{border:1px solid var(--ln);padding:6px 10px;text-align:left}
#md th{color:var(--mut)}
#md hr{border:none;border-top:1px solid var(--ln);margin:1.8em 0}
#md ul,#md ol{padding-left:1.4em}#md li{margin:.2em 0}
.back{color:var(--mut);border:1px solid var(--ln);border-radius:4px;padding:3px 10px;text-decoration:none}.back:hover{border-color:var(--acc);color:var(--acc)}
</style></head><body>
<header><h1>NETGOV · HELP</h1><span style="flex:1"></span><a class="back" href="/">← dashboard</a></header>
<main><div id="md">loading…</div></main>
<script>
const BT='\u0060',BT3=BT+BT+BT,codeRe=new RegExp(BT+'([^'+BT+']+)'+BT,'g');
function esc(s){return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;')}
function inl(s){return esc(s).replace(codeRe,'<code>$1</code>').replace(/\*\*([^*]+)\*\*/g,'<strong>$1</strong>').replace(/\*([^*]+)\*/g,'<em>$1</em>').replace(/\[([^\]]+)\]\(([^)]+)\)/g,'<a href="$2" target="_blank">$1</a>')}
function cells(r){return r.split('|').slice(1,-1).map(c=>c.trim())}
function md2html(md){const L=md.split('\n');let o=[],i=0;
 while(i<L.length){let ln=L[i];
  if(ln.slice(0,3)===BT3){let c=[];i++;while(i<L.length&&L[i].slice(0,3)!==BT3){c.push(esc(L[i]));i++}i++;o.push('<pre><code>'+c.join('\n')+'</code></pre>');continue}
  if(ln.startsWith('### ')){o.push('<h3>'+inl(ln.slice(4))+'</h3>');i++;continue}
  if(ln.startsWith('## ')){o.push('<h2>'+inl(ln.slice(3))+'</h2>');i++;continue}
  if(ln.startsWith('# ')){o.push('<h1>'+inl(ln.slice(2))+'</h1>');i++;continue}
  // Accept a BARE '>' too — a blank line inside a blockquote. It used to match neither this
  // branch (which required '> ') nor the paragraph branch (which refuses anything starting '>'),
  // so i never advanced and md2html span the browser tab forever. A doc must not be able to hang
  // the help page; see also the belt-and-braces guard at the end of the loop.
  if(ln.charAt(0)==='>'){let q=[];while(i<L.length&&L[i].charAt(0)==='>'){
    let t=L[i].startsWith('> ')?L[i].slice(2):L[i].slice(1); q.push(t.trim()===''?'<br>':inl(t)); i++}
   o.push('<blockquote>'+q.join(' ')+'</blockquote>');continue}
  if(/^---+\s*$/.test(ln)){o.push('<hr>');i++;continue}
  if(ln.startsWith('|')){let rows=[];while(i<L.length&&L[i].startsWith('|')){rows.push(L[i]);i++}
   let h=cells(rows[0]),b=rows.slice(2),t='<table><thead><tr>'+h.map(x=>'<th>'+inl(x)+'</th>').join('')+'</tr></thead><tbody>';
   b.forEach(r=>{t+='<tr>'+cells(r).map(c=>'<td>'+inl(c)+'</td>').join('')+'</tr>'});o.push(t+'</tbody></table>');continue}
  // Lists must ABSORB wrapped continuation lines. The docs are hard-wrapped at ~100 cols, so a
  // bullet's text routinely continues on an indented line that does NOT start with "- ". Treating
  // that line as a new block ended the list and emitted the remainder as its own <p> — which is
  // what put line breaks in the middle of sentences on the help page (11 such lines in UI/CLI.md).
  // A continuation = indented, non-blank, and not itself a new list item/heading/table/fence.
  const cont=s=>/^\s+\S/.test(s)&&!/^\s*([-*] |\d+\. |#|>|\||---)/.test(s)&&s.slice(0,3)!==BT3;
  if(/^\s*[-*] /.test(ln)){let it=[];while(i<L.length&&/^\s*[-*] /.test(L[i])){let t=L[i].replace(/^\s*[-*] /,'');i++;
    while(i<L.length&&cont(L[i])){t+=' '+L[i].trim();i++}it.push('<li>'+inl(t)+'</li>')}o.push('<ul>'+it.join('')+'</ul>');continue}
  if(/^\s*\d+\. /.test(ln)){let it=[];while(i<L.length&&/^\s*\d+\. /.test(L[i])){let t=L[i].replace(/^\s*\d+\. /,'');i++;
    while(i<L.length&&cont(L[i])){t+=' '+L[i].trim();i++}it.push('<li>'+inl(t)+'</li>')}o.push('<ol>'+it.join('')+'</ol>');continue}
  if(ln.trim()===''){i++;continue}
  let p=[];while(i<L.length&&L[i].trim()!==''&&!/^(#|>|\||---|\s*[-*] |\s*\d+\. )/.test(L[i])&&L[i].slice(0,3)!==BT3){p.push(inl(L[i]));i++}
  // A paragraph that consumed nothing means some line matched no branch and was not skipped.
  // Rather than spin, emit it and move on: an odd-looking line is a cosmetic defect, an infinite
  // loop is a hung page. This guard is what makes the renderer total over arbitrary input.
  if(p.length===0){o.push('<p>'+inl(L[i])+'</p>');i++;continue}
  o.push('<p>'+p.join(' ')+'</p>')}
 return o.join('\n')}
fetch('/api/help').then(r=>r.text()).then(t=>{document.getElementById('md').innerHTML=md2html(t)}).catch(e=>{document.getElementById('md').textContent='failed to load help: '+e});
</script></body></html>`
