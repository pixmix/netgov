package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

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
	Dev    string `json:"dev"`
	SSID   string `json:"ssid"`
	Band   string `json:"band"`
	Active bool   `json:"active"`
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
		Armed: st.Armed, Active: st.ActivePattern}
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
		})
	}
	for _, u := range st.Uplinks {
		if servingDev(st, u.Dev) {
			continue // shadowed by an AP
		}
		v.Uplinks = append(v.Uplinks, uplinkView{Name: u.Name, Dev: u.Dev, Table: u.Table, V4: famOf(u, "4"), V6: famOf(u, "6")})
	}
	for _, a := range st.APs {
		v.APs = append(v.APs, apView{Dev: a.Dev, SSID: a.SSID, Band: a.Band, Active: apActive(a.Dev) == "up"})
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
	return v
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
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
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(pageHTML))
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
		dev := resolveDev(s, r.FormValue("dev"))
		switch r.FormValue("action") {
		case "on":
			band := r.FormValue("band")
			if band == "" {
				band = "bg"
			}
			ch := 0
			if v := r.FormValue("channel"); v != "" {
				ch, _ = strconv.Atoi(v)
			}
			ensureUplink(s, dev)
			a := AP{Dev: dev, SSID: r.FormValue("ssid"), PSK: r.FormValue("psk"), Band: band, Channel: ch}
			var keep []AP
			for _, x := range s.APs {
				if x.Dev != dev {
					keep = append(keep, x)
				}
			}
			s.APs = append(keep, a)
			_ = saveState(s, statePath())
			out, err := apUp(a)
			writeJSON(w, map[string]any{"ok": err == nil, "out": out, "state": buildView()})
			return
		case "off":
			var keep []AP
			for _, x := range s.APs {
				if x.Dev != dev {
					keep = append(keep, x)
				}
			}
			s.APs = keep
			_ = saveState(s, statePath())
			out, _ := apDown(dev)
			writeJSON(w, map[string]any{"ok": true, "out": out, "state": buildView()})
			return
		}
		writeJSON(w, buildView())
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
	bin := filepath.Join(homeDir(), "bin", "netgov")
	sp := statePath()
	script := fmt.Sprintf(`#!/bin/sh
# netgov NM dispatcher hook — re-apply host policy routing on link up/down.
case "$2" in up|down|vpn-up|vpn-down) ;; *) exit 0 ;; esac
%s __apply --state %s >> /var/log/netgov-dispatch.log 2>&1 || true
`, bin, sp)
	tmp := filepath.Join(os.TempDir(), "90-netgov")
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
	tmpU := filepath.Join(os.TempDir(), "netgov-roled.service")
	must(os.WriteFile(tmpU, []byte(unit), 0o644))
	uDst := "/etc/systemd/system/netgov-roled.service"
	cmdU := exec.Command("sudo", "-A", "install", "-m", "0644", "-o", "root", "-g", "root", tmpU, uDst)
	cmdU.Env = askpassEnv()
	cmdU.Stdin, cmdU.Stdout, cmdU.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmdU.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "unit install failed:", err)
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
<header><h1>NETGOV</h1><span class="mut" id="sub">host switchboard</span>
<span style="flex:1"></span><button class="go" onclick="apply()">APPLY ▸</button>
<button onclick="load()" title="refresh status">↻ refresh</button></header>
<main>
<section><h2>Uplinks</h2><table id="ut"><thead><tr><th>name</th><th>iface</th>
<th>IPv4</th><th>IPv6</th><th>tbl</th><th></th></tr></thead><tbody></tbody></table>
<div class="row"><span class="mut">define:</span><input id="un" placeholder="name" size="8">
<input id="ud" placeholder="iface" size="14"><input id="ug" placeholder="gateway (optional)" size="15">
<button onclick="defUp()">+ uplink</button></div></section>

<section><h2>Access points — WiFi interface → AP (shadows its uplink)</h2>
<table id="at"><tbody></tbody></table>
<div class="row"><select id="aif"></select><input id="assid" placeholder="SSID e.g. myhotspot" size="14">
<input id="apsk" placeholder="passphrase" size="14">
<select id="aband"><option value="bg">2.4GHz</option><option value="a">5GHz</option></select>
<button onclick="apOn()">+ enable AP</button></div></section>

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
<div class="row" style="align-items:flex-start;flex-wrap:wrap">
<input id="pn" placeholder="name" size="8"><input id="pp" placeholder="prio" size="4" value="50">
<span class="mut">v4</span><select id="pv4"></select><span class="mut">v6</span><select id="pv6"></select>
<span class="mut">require up</span><select id="prq" multiple size="2" style="min-width:74px" title="uplinks that must be UP"></select>
<span class="mut">SSID</span><input id="pssid" placeholder="e.g. Motionlab-Member" size="17"><span class="mut">on</span><select id="pssidif" title="Wi-Fi uplink to scan"></select>
<span class="mut">AP on</span><select id="pap" multiple size="2" style="min-width:74px" title="access points to keep up"></select>
<textarea id="prules" rows="3" cols="26" placeholder="rules, one per line:&#10;api.anthropic.com WiFi0&#10;from:172.18.0.0/16 cable"></textarea>
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
 return o+S.uplinks.map(u=>'<option '+(u.name===cur?'selected':'')+'>'+u.name+'</option>').join('')}
function famOpts(cur){return ['both','4','6'].map(f=>'<option '+(f===(cur||'both')?'selected':'')+'>'+f+'</option>').join('')}
function fcell(f){if(!f.up)return '<span class="down">—</span>';
 return '<span class="up">up</span> <span class="mut">'+(f.src||'-')+' gw '+(f.gw||'-')+'</span> '+(f.internet?'<span class="pill up">internet ✓</span>':'<span class="pill warn">no internet</span>')}
function render(){
 $('#ut tbody').innerHTML=S.uplinks.map(u=>'<tr><td class="acc">'+u.name+'</td><td>'+u.dev+'</td><td>'+fcell(u.v4)+
  '</td><td>'+fcell(u.v6)+'</td><td class="mut">'+u.table+'</td><td><button title="restore link to NM profile" onclick="reapply(\''+u.dev+'\')">↺</button> <button onclick="delUp(\''+u.name+'\')">×</button></td></tr>').join('')||'<tr><td colspan=6 class=mut>none — run “netgov init”</td></tr>';
 const dr=S.rules.filter(r=>r.kind==='dest');
 $('#rt tbody').innerHTML=(dr.length?dr:[]).map(r=>'<tr><td>'+r.sel+'</td><td class="acc">'+r.via+'</td><td class="mut">['+r.fam+']</td><td class="mut">'+((r.ips||[]).join(' ')||'unresolved')+'</td><td><button onclick="delRule({domain:\''+r.sel+'\'})">×</button></td></tr>').join('')||'<tr><td class=mut colspan=5>none</td></tr>';
 const sr=S.rules.filter(r=>r.kind==='src');
 $('#st tbody').innerHTML=(sr.length?sr:[]).map(r=>'<tr><td>'+r.sel+'</td><td class="acc">'+r.via+'</td><td class="mut">['+r.fam+']</td><td><button onclick="delRule({from:\''+r.sel+'\'})">×</button></td></tr>').join('')||'<tr><td class=mut colspan=4>none</td></tr>';
 $('#at tbody').innerHTML=(S.aps||[]).map(a=>'<tr><td class="acc">'+a.dev+'</td><td>SSID '+a.ssid+'</td><td class="mut">'+(a.band==='a'?'5GHz':'2.4GHz')+'</td><td>'+(a.active?'<span class="pill up">on air</span>':'<span class="pill warn">down</span>')+'</td><td><button onclick="apOff(\''+a.dev+'\')">disable</button></td></tr>').join('')||'<tr><td class=mut colspan=5>none — enabling an AP hides that interface from Uplinks above</td></tr>';
 $('#aif').innerHTML=(S.wifi_if||[]).map(d=>'<option>'+d+'</option>').join('');
 $('#rv').innerHTML=ulOpts('',[['block','block']]);$('#rf').innerHTML=famOpts();
 $('#sv').innerHTML=ulOpts('',[['block','block']]);$('#sf').innerHTML=famOpts();
 $('#sfrom').innerHTML=S.bridges.map(b=>'<option value="'+b.name+'">'+b.name+(b.subnet?' ('+b.subnet+')':'')+'</option>').join('')+'<option value="">— custom CIDR —</option>';
 $('#brs').textContent=S.bridges.length?'':'(no container/VM bridges detected)';
 $('#d4').innerHTML=ulOpts(S.default_v4,[['','(none)'],['block','block']]);
 $('#d6').innerHTML=ulOpts(S.default_v6,[['','(none)'],['block','block']]);
 $('#pt tbody').innerHTML=(S.patterns||[]).map(p=>{let trig=[...(p.require||[]),(p.ssid?'📶'+p.ssid:'')].filter(Boolean).join(' ')||'-';let ra=p.rules+(p.aps&&p.aps.length?' +AP':'');return '<tr><td class=mut>'+p.priority+'</td><td class=acc>'+p.name+(p.floor?' <span class=mut>(floor)</span>':'')+'</td><td class=mut>'+trig+'</td><td>'+p.v4+'</td><td>'+p.v6+'</td><td class=mut>'+ra+'</td><td>'+(p.active?'<span class="pill up">ACTIVE</span> ':'')+(p.satisfiable?'<span class="pill up">ok</span>':'<span class="pill warn">not-now</span>')+'</td><td><button onclick="patApply(\''+p.name+'\')">activate</button> <button onclick="patEdit(\''+p.name+'\')">edit</button> <button onclick="patDel(\''+p.name+'\')">×</button></td></tr>'}).join('')||'<tr><td class=mut colspan=8>none — build one below (a floor is auto-added on arm)</td></tr>';
 $('#armbadge').innerHTML=S.armed?'<span class="pill up">ARMED · '+S.armed+'</span>':'<span class="pill">disarmed</span>';
 $('#prq').innerHTML=S.uplinks.map(u=>'<option>'+u.name+'</option>').join('');
 $('#pssidif').innerHTML=S.uplinks.map(u=>'<option'+(u.name==='WiFi0'?' selected':'')+'>'+u.name+'</option>').join('');
 $('#pap').innerHTML=(S.aps||[]).map(a=>'<option value="'+a.dev+'">'+a.ssid+'</option>').join('')||'<option disabled>no APs</option>';
 $('#pv4').innerHTML=ulOpts('direct',[['direct','direct'],['block','block']]);
 $('#pv6').innerHTML=ulOpts('block',[['direct','direct'],['block','block']]);
 $('#sub').textContent='default v4='+(S.default_v4||'none')+'  v6='+(S.default_v6||'none')+(S.armed?'  · ARMED('+S.armed+')':'');
}
async function load(){S=await (await fetch('/api/state')).json();render()}
function log(m){$('#log').textContent=m}
async function post(u,d){return (await fetch(u,{method:'POST',body:new URLSearchParams(d)})).json()}
async function defUp(){S=await post('/api/uplink',{action:'define',name:$('#un').value,dev:$('#ud').value,gw:$('#ug').value});render();$('#un').value='';$('#ud').value='';$('#ug').value=''}
async function delUp(n){if(confirm('remove uplink '+n+'?')){S=await post('/api/uplink',{action:'del',name:n});render()}}
async function addDest(){S=await post('/api/rule',{action:'add',domain:$('#rd').value,via:$('#rv').value,fam:$('#rf').value});render();$('#rd').value=''}
async function addSrc(){let f=$('#sfrom').value;if(f===''){f=prompt('source CIDR (e.g. 172.20.0.0/16) or interface name');if(!f)return}
 S=await post('/api/rule',{action:'add',from:f,via:$('#sv').value,fam:$('#sf').value});render()}
async function delRule(d){S=await post('/api/rule',Object.assign({action:'del'},d));render()}
async function setDef(){S=await post('/api/default',{v4:$('#d4').value,v6:$('#d6').value});render()}
async function apOn(){const psk=$('#apsk').value;if(psk.length<8){alert('passphrase must be >=8 chars');return}
 log('enabling AP on '+$('#aif').value+'…');const r=await post('/api/ap',{action:'on',dev:$('#aif').value,ssid:$('#assid').value,psk:psk,band:$('#aband').value});
 log(r.out||(r.ok?'AP up':'failed'));if(r.state){S=r.state;render()}else load();$('#apsk').value=''}
async function apOff(dev){if(!confirm('disable AP on '+dev+'? (it returns to the Uplinks list)'))return;log('disabling AP '+dev+'…');const r=await post('/api/ap',{action:'off',dev:dev});log(r.out||'done');if(r.state){S=r.state;render()}else load()}
async function reapply(dev){log('reapplying '+dev+'…');const r=await post('/api/link',{action:'reapply',dev:dev});log(r.out||'done');load()}
async function arm(mode){log((mode==='off'?'disarming':'arming '+mode)+'… (approve the sudo dialog)');const r=await post('/api/arm',{mode:mode});log(r.out||(r.ok?'done':'failed'));if(r.state){S=r.state;render()}else load()}
async function evalNow(){log('evaluating… (approve the sudo dialog)');const r=await post('/api/pattern',{action:'eval'});log(r.out||'done');if(r.state){S=r.state;render()}else load()}
async function patSave(){if(!$('#pn').value){alert('name required');return}
 let rq=[...$('#prq').selectedOptions].map(o=>o.value).join(',');
 let ap=[...$('#pap').selectedOptions].map(o=>o.value).join(',');
 S=await post('/api/pattern',{action:'set',name:$('#pn').value,priority:$('#pp').value||'50',require:rq,ssid:$('#pssid').value,ssid_iface:$('#pssidif').value,aps:ap,v4:$('#pv4').value,v6:$('#pv6').value,rules:$('#prules').value});render();$('#pn').value='';$('#prules').value='';$('#pssid').value=''}
function patSnapshot(){ // fill the builder's egress fields from the CURRENT live config
 $('#pv4').innerHTML=ulOpts(S.default_v4||'direct',[['direct','direct'],['block','block']]);
 $('#pv6').innerHTML=ulOpts(S.default_v6||'block',[['direct','direct'],['block','block']]);
 $('#prules').value=(S.rules||[]).filter(r=>r.sel).map(r=>(r.kind==='src'?'from:'+r.sel:r.sel)+' '+r.via+' '+(r.fam||'both')).join('\n');
 [...$('#pap').options].forEach(o=>o.selected=(S.aps||[]).some(a=>a.dev===o.value&&a.active));
 log('snapshotted current egress into the builder — add a name + trigger, then + save')}
async function patDel(n){if(confirm('delete pattern '+n+'?')){S=await post('/api/pattern',{action:'del',name:n});render()}}
async function patApply(n){log('activating '+n+'… (approve the sudo dialog)');const r=await post('/api/pattern',{action:'apply',name:n});log(r.out||'done');if(r.state){S=r.state;render()}else load()}
function patEdit(n){const p=(S.patterns||[]).find(x=>x.name===n);if(!p)return;
 $('#pn').value=p.name;$('#pp').value=p.priority;
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
