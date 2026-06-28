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
type stateView struct {
	Uplinks   []uplinkView `json:"uplinks"`
	APs       []apView     `json:"aps"`
	WifiIf    []string     `json:"wifi_if"`
	Rules     []ruleView   `json:"rules"`
	Bridges   []bridgeInfo `json:"bridges"`
	DefaultV4 string       `json:"default_v4"`
	DefaultV6 string       `json:"default_v6"`
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
	v := stateView{DefaultV4: st.DefaultV4, DefaultV6: st.DefaultV6, Bridges: scanBridges(), WifiIf: wifiIfaces()}
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

	priv := func(verb string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			self, _ := os.Executable()
			cmd := exec.Command("sudo", "-A", self, verb, "--state", statePath())
			cmd.Env = append(os.Environ(), "SUDO_ASKPASS="+filepath.Join(homeDir(), "bin", "sudo-askpass-zenity"))
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

<section class="danger"><h2>Restore</h2><div class="row">
<button class="bad" onclick="reset()">⟲ Restore to NetworkManager</button>
<small>removes ALL netgov rules &amp; tables → the OS/NM baseline reappears (netgov never edits NM itself)</small></div></section>

<section><h2>Log</h2><div id="log">—</div></section>
</main>
<script>
let S={uplinks:[],aps:[],wifi_if:[],rules:[],bridges:[],default_v4:"",default_v6:""};
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
 $('#sub').textContent='default v4='+(S.default_v4||'none')+'  v6='+(S.default_v6||'none');
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
async function apply(){log('applying… (approve the sudo dialog on screen)');const r=await post('/api/apply',{});log(r.out||(r.ok?'applied':'failed'));load()}
async function reset(){if(!confirm('Remove ALL netgov rules and restore the NetworkManager baseline?'))return;log('restoring…');const r=await post('/api/reset',{});log(r.out||'done');load()}
load();
setInterval(()=>{const a=document.activeElement;if(a&&/^(INPUT|SELECT)$/.test(a.tagName))return;load()},15000);
</script></body></html>`
