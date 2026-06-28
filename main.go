// netgov — host-level multi-homing / policy-routing manager for Linux.
//
// Model: named UPLINKS (handles over network interfaces: built-in Wi-Fi, a USB Wi-Fi
// adapter, an Ethernet link, a USB phone-tether, etc.) + RULES that steer traffic by
// destination domain or by source (containers/VMs/subnets) to a chosen uplink, per
// address family, plus a per-family overall DEFAULT. LOCAL/RFC1918 traffic always stays
// on the connected link. Realised purely client-side via per-uplink routing tables +
// `ip rule`/`ip route` — works with or without an upstream router present.
//
// PATTERNS add a roled-style layer: named, prioritised policy snapshots with
// satisfiability criteria; arm/dry/disarm; and a root failover loop that auto-selects
// the best satisfiable+validated pattern (poll-validate + debounce). Boots disarmed.
//
// SAFE BY DESIGN: netgov only ever ADDS rules in its own priority band [8000,29999]
// and its own private tables [100,199]; it never edits the main table or NM profiles.
// So `netgov reset` removes everything and the NetworkManager baseline reappears intact.
//
// The privileged steps (apply/reset/arm) re-exec via `sudo -A` (honours SUDO_ASKPASS).
// An NM dispatcher hook re-applies as root on link-up (no dialog); when armed, a root
// systemd service (netgov-roled.service) runs the failover loop and applies dialog-free.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ip rule priority bands we own (lower number wins).
const (
	priLocal   = 8000  // RFC1918 / link-local -> main (local always direct)
	priDomain  = 10000 // destination (domain) pins
	priSrcPin  = 15000 // source pins (containers/VMs/custom)
	priSrcRet  = 20000 // per-uplink source-return rules
	priDefault = 29000 // overall default (per family)
	ownedPriLo = 8000
	ownedPriHi = 29999
	tableBase  = 100
	tableMax   = 198
	tableBlock = 199 // reserved: contains a blackhole default (leak-protect)
)

type Uplink struct {
	Name     string `json:"name"`
	Dev      string `json:"dev"`
	Table    int    `json:"table"`
	Gateway  string `json:"gw,omitempty"`  // explicit v4 gw override (never-default LAN uplink)
	Gateway6 string `json:"gw6,omitempty"` // explicit v6 gw override
}

// Rule steers traffic to an uplink (Via) or drops it (Via=="block"). Exactly one of
// Domain (destination) or From (source: CIDR or interface name) is set.
type Rule struct {
	Domain string `json:"domain,omitempty"`
	From   string `json:"from,omitempty"`
	Via    string `json:"via"`
	Fam    string `json:"fam,omitempty"` // "4" | "6" | "both"/""
}

// AP turns a WiFi interface into an access point (NM `ipv4.method shared`: DHCP+NAT,
// clients egress via the host default). While an AP is active on a dev, that dev's
// uplink is shadowed (removed from egress duty); disabling the AP restores it.
type AP struct {
	Dev     string `json:"dev"`
	SSID    string `json:"ssid"`
	PSK     string `json:"psk"`
	Band    string `json:"band"`              // bg (2.4) | a (5)
	Channel int    `json:"channel,omitempty"` // 0 = auto
}

// Pattern is a named, prioritised egress-policy snapshot (the roled-style layer).
// Activating it copies V4/V6/Rules into the live State and applies. Selection (when
// armed) picks the highest-priority pattern whose Require uplinks are live and whose
// default uplink validates internet, falling back to the Floor pattern.
type Pattern struct {
	Name      string   `json:"name"`
	Priority  int      `json:"priority"`
	Require   []string `json:"require,omitempty"`    // uplink names that must be LIVE (hard criteria)
	SSID      string   `json:"ssid,omitempty"`       // require this SSID (or any of a comma-list) IN RANGE
	SSIDIface string   `json:"ssid_iface,omitempty"` // uplink name (or raw dev) whose Wi-Fi to scan for SSID
	V4        string   `json:"v4,omitempty"`         // uplink name | "block" | "direct"/"" (main table)
	V6        string   `json:"v6,omitempty"`
	Rules     []Rule   `json:"rules,omitempty"` // domain/source pins active under this pattern
	APs       []string `json:"aps,omitempty"`   // AP devices to ensure ENABLED when this pattern activates
	Floor     bool     `json:"floor,omitempty"` // always-satisfiable fallback (Require ignored)
}

type State struct {
	Uplinks   []Uplink `json:"uplinks"`
	APs       []AP     `json:"aps,omitempty"`
	Rules     []Rule   `json:"rules"`
	DefaultV4 string   `json:"default_v4,omitempty"` // uplink | "block" | ""
	DefaultV6 string   `json:"default_v6,omitempty"`
	WebAddr   string   `json:"web_addr,omitempty"`

	Patterns      []Pattern `json:"patterns,omitempty"`
	Armed         string    `json:"armed,omitempty"`          // "" | "armed" | "dry"
	ActivePattern string    `json:"active_pattern,omitempty"` // last selected/activated pattern

	LegacyDefault string `json:"default,omitempty"` // migrated from v1
}

func servingDev(st *State, dev string) bool {
	for _, a := range st.APs {
		if a.Dev == dev {
			return true
		}
	}
	return false
}

func apConName(dev string) string { return "netgov-ap-" + dev }

// apUp/apDown drive the NM access-point connection for a device.
func apUp(a AP) (string, error) {
	con := apConName(a.Dev)
	_, _ = run("nmcli", "con", "delete", con)
	if out, err := run("nmcli", "con", "add", "type", "wifi", "ifname", a.Dev, "con-name", con, "ssid", a.SSID); err != nil {
		return out, err
	}
	band := a.Band
	if band == "" {
		band = "bg"
	}
	args := []string{"con", "modify", con,
		"802-11-wireless.mode", "ap", "802-11-wireless.band", band,
		"wifi-sec.key-mgmt", "wpa-psk", "wifi-sec.psk", a.PSK,
		"wifi-sec.proto", "rsn", "wifi-sec.pairwise", "ccmp", "wifi-sec.group", "ccmp",
		"ipv4.method", "shared", "connection.autoconnect", "yes"}
	if a.Channel > 0 {
		args = append(args, "802-11-wireless.channel", itoa(a.Channel))
	}
	if out, err := run(append([]string{"nmcli"}, args...)...); err != nil {
		return out, err
	}
	return run("nmcli", "con", "up", con)
}

func apDown(dev string) (string, error) {
	con := apConName(dev)
	_, _ = run("nmcli", "con", "down", con)
	return run("nmcli", "con", "delete", con)
}

const blockVia = "block"

func itoa(i int) string { return strconv.Itoa(i) }

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "."
}

// askpassEnv builds the environment for a `sudo -A` re-exec. It honours a pre-set
// SUDO_ASKPASS (any askpass helper works), otherwise defaults to the common
// ~/bin/sudo-askpass-zenity convention. Set SUDO_ASKPASS or run as root to override.
func askpassEnv() []string {
	if os.Getenv("SUDO_ASKPASS") != "" {
		return os.Environ()
	}
	return append(os.Environ(), "SUDO_ASKPASS="+filepath.Join(homeDir(), "bin", "sudo-askpass-zenity"))
}

func statePath() string {
	if p := os.Getenv("NETGOV_STATE"); p != "" {
		return p
	}
	return filepath.Join(homeDir(), ".config", "netgov", "state.json")
}

func loadState(path string) *State {
	st := &State{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, st)
	}
	// migrate v1 single default -> v4 default + v6 leak-protect
	if st.LegacyDefault != "" && st.DefaultV4 == "" && st.DefaultV6 == "" {
		st.DefaultV4 = st.LegacyDefault
		st.DefaultV6 = blockVia
		st.LegacyDefault = ""
	}
	return st
}

func saveState(st *State, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(path, b, 0o644)
}

func run(argv ...string) (string, error) {
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ---- `ip -j` parsing ----
type ipAddr struct {
	AddrInfo []struct {
		Family string `json:"family"`
		Local  string `json:"local"`
		Scope  string `json:"scope"`
	} `json:"addr_info"`
}
type ipRoute struct {
	Dst     string `json:"dst"`
	Gateway string `json:"gateway"`
	Dev     string `json:"dev"`
}
type ipRule struct {
	Priority int    `json:"priority"`
	Src      string `json:"src"`
	Dst      string `json:"dst"`
	Table    string `json:"table"`
}

func ipfam(fam string, rest ...string) []string {
	if fam == "6" {
		return append([]string{"ip", "-6"}, rest...)
	}
	return append([]string{"ip"}, rest...)
}

func devExists(dev string) bool { _, err := os.Stat("/sys/class/net/" + dev); return err == nil }

func devOperUp(dev string) bool {
	b, err := os.ReadFile("/sys/class/net/" + dev + "/operstate")
	if err != nil {
		return false
	}
	s := strings.TrimSpace(string(b))
	return s == "up" || s == "unknown" || s == "dormant"
}

func devSrc(dev, fam string) string {
	out, err := run("ip", "-j", "addr", "show", "dev", dev)
	if err != nil {
		return ""
	}
	var addrs []ipAddr
	if json.Unmarshal([]byte(out), &addrs) != nil {
		return ""
	}
	want := "inet"
	if fam == "6" {
		want = "inet6"
	}
	for _, a := range addrs {
		for _, ai := range a.AddrInfo {
			// for v6 prefer a global, non-ULA address (ULA fd00::/8 isn't routable)
			if ai.Family == want && ai.Scope == "global" {
				if fam == "6" && strings.HasPrefix(strings.ToLower(ai.Local), "fd") {
					continue
				}
				return ai.Local
			}
		}
	}
	return ""
}

func defaultGW(dev, fam string) string {
	out, err := run(ipfam(fam, "-j", "route", "show", "default")...)
	if err != nil {
		return ""
	}
	var rs []ipRoute
	if json.Unmarshal([]byte(out), &rs) != nil {
		return ""
	}
	for _, r := range rs {
		if r.Dev == dev && r.Gateway != "" {
			return r.Gateway
		}
	}
	return ""
}

func linkNet(dev, fam string) string {
	out, err := run(ipfam(fam, "-j", "route", "show", "dev", dev, "scope", "link")...)
	if err != nil {
		return ""
	}
	var rs []ipRoute
	if json.Unmarshal([]byte(out), &rs) != nil {
		return ""
	}
	for _, r := range rs {
		if r.Dst != "" && r.Dst != "default" {
			return r.Dst
		}
	}
	return ""
}

func parseRules(fam string) []ipRule {
	out, err := run(ipfam(fam, "-j", "rule", "show")...)
	if err != nil {
		return nil
	}
	var rs []ipRule
	_ = json.Unmarshal([]byte(out), &rs)
	return rs
}

func dhcpRouter(dev string) string {
	out, err := run("nmcli", "-g", "DHCP4.OPTION", "device", "show", dev)
	if err != nil {
		return ""
	}
	for _, f := range strings.FieldsFunc(out, func(r rune) bool { return r == ',' || r == '|' || r == '\n' }) {
		f = strings.TrimSpace(f)
		if strings.HasPrefix(f, "routers") {
			if i := strings.IndexByte(f, '='); i >= 0 {
				return strings.TrimSpace(f[i+1:])
			}
		}
	}
	return ""
}

func resolveFam(domain, fam string) []string {
	r := &net.Resolver{}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	ips, err := r.LookupIP(ctx, "ip", domain)
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, ip := range ips {
		isV4 := ip.To4() != nil
		if (fam == "4") != isV4 {
			continue
		}
		if s := ip.String(); !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func uplinkLive(u Uplink, fam string) (src, gw string, live bool) {
	if !devExists(u.Dev) || !devOperUp(u.Dev) {
		return "", "", false
	}
	if src = devSrc(u.Dev, fam); src == "" {
		return "", "", false
	}
	if fam == "4" {
		gw = u.Gateway
	} else if u.Gateway6 != "" {
		gw = u.Gateway6
	}
	if gw == "" {
		gw = defaultGW(u.Dev, fam)
	}
	if gw == "" && fam == "4" {
		gw = dhcpRouter(u.Dev)
	}
	return src, gw, true
}

func famMatch(rfam, fam string) bool { return rfam == "" || rfam == "both" || rfam == fam }

// cidrFam returns the family of a CIDR/IP ("4"/"6"), or "" for an interface name.
func cidrFam(s string) string {
	if !strings.ContainsAny(s, "/.:") {
		return "" // bare interface name
	}
	if strings.Contains(s, ":") {
		return "6"
	}
	return "4"
}

func selFor(from string) []string {
	if cidrFam(from) == "" {
		return []string{"iif", from}
	}
	return []string{"from", from}
}

// ---- plan ----
func planFamily(st *State, fam string) (clean, build [][]string) {
	for _, r := range parseRules(fam) {
		if r.Priority >= ownedPriLo && r.Priority <= ownedPriHi {
			clean = append(clean, ipfam(fam, "rule", "del", "priority", itoa(r.Priority)))
		}
	}
	for _, u := range st.Uplinks {
		clean = append(clean, ipfam(fam, "route", "flush", "table", itoa(u.Table)))
	}
	clean = append(clean, ipfam(fam, "route", "flush", "table", itoa(tableBlock)))

	// blackhole table for leak-protect / "block"
	build = append(build, ipfam(fam, "route", "add", "blackhole", "default", "table", itoa(tableBlock)))

	live := map[string]Uplink{}
	for _, u := range st.Uplinks {
		if servingDev(st, u.Dev) {
			continue // this interface is an AP now, not an uplink
		}
		src, gw, ok := uplinkLive(u, fam)
		if !ok {
			continue
		}
		live[u.Name] = u
		if n := linkNet(u.Dev, fam); n != "" {
			build = append(build, ipfam(fam, "route", "add", n, "dev", u.Dev, "table", itoa(u.Table)))
		}
		if gw != "" {
			build = append(build, ipfam(fam, "route", "add", "default", "via", gw, "dev", u.Dev, "table", itoa(u.Table)))
		}
		build = append(build, ipfam(fam, "rule", "add", "from", src, "table", itoa(u.Table), "priority", itoa(priSrcRet+u.Table-tableBase)))
	}

	// local always direct (via main)
	locals := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	if fam == "6" {
		locals = []string{"fc00::/7", "fe80::/10"}
	}
	for i, cidr := range locals {
		build = append(build, ipfam(fam, "rule", "add", "to", cidr, "table", "main", "priority", itoa(priLocal+i)))
	}

	tableOrBlock := func(via string) (int, bool) {
		if via == blockVia {
			return tableBlock, true
		}
		if u, ok := live[via]; ok {
			return u.Table, true
		}
		return 0, false
	}

	// destination (domain) pins
	pri := priDomain
	seenIP := map[string]bool{}
	for _, r := range st.Rules {
		if r.Domain == "" || !famMatch(r.Fam, fam) {
			continue
		}
		t, ok := tableOrBlock(r.Via)
		if !ok {
			continue
		}
		for _, ip := range resolveFam(r.Domain, fam) {
			if pri >= priSrcPin || seenIP[ip] {
				continue
			}
			seenIP[ip] = true
			build = append(build, ipfam(fam, "rule", "add", "to", ip, "table", itoa(t), "priority", itoa(pri)))
			pri++
		}
	}

	// source pins (containers / VMs / custom)
	pri = priSrcPin
	for _, r := range st.Rules {
		if r.From == "" || !famMatch(r.Fam, fam) {
			continue
		}
		if cf := cidrFam(r.From); cf != "" && cf != fam {
			continue // a v4 CIDR has nothing to do in the v6 plan
		}
		t, ok := tableOrBlock(r.Via)
		if !ok || pri >= priSrcRet {
			continue
		}
		args := append([]string{"rule", "add"}, selFor(r.From)...)
		args = append(args, "table", itoa(t), "priority", itoa(pri))
		build = append(build, ipfam(fam, args...))
		pri++
	}

	// overall default (per family)
	def := st.DefaultV4
	if fam == "6" {
		def = st.DefaultV6
	}
	if def != "" {
		if t, ok := tableOrBlock(def); ok {
			build = append(build, ipfam(fam, "rule", "add", "from", "all", "table", itoa(t), "priority", itoa(priDefault)))
		}
	}
	return
}

func fullPlan(st *State) (clean, build [][]string) {
	for _, fam := range []string{"4", "6"} {
		c, b := planFamily(st, fam)
		clean = append(clean, c...)
		build = append(build, b...)
	}
	return
}

func applyRoot(st *State) {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "must run as root")
		os.Exit(1)
	}
	_, _ = run("sysctl", "-qw", "net.ipv4.conf.all.rp_filter=2")
	_, _ = run("sysctl", "-qw", "net.ipv4.conf.default.rp_filter=2")
	clean, build := fullPlan(st)
	for _, c := range clean {
		_, _ = run(c...)
	}
	var errs int
	for _, c := range build {
		if out, err := run(c...); err != nil {
			errs++
			fmt.Fprintf(os.Stderr, "  ! %s -> %v %s\n", strings.Join(c, " "), err, out)
		}
	}
	fmt.Printf("netgov: applied (%d cmds, %d errors)\n", len(build), errs)
}

func resetRoot(st *State) {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "must run as root")
		os.Exit(1)
	}
	clean, _ := fullPlan(st)
	for _, c := range clean {
		_, _ = run(c...)
	}
	fmt.Printf("netgov: reset — %d cleanup cmds; NetworkManager baseline restored\n", len(clean))
}

// privileged re-exec via sudo -A (zenity)
func sudoSelf(verb string) {
	self, _ := os.Executable()
	cmd := exec.Command("sudo", "-A", self, verb, "--state", statePath())
	cmd.Env = askpassEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, verb, "failed:", err)
		os.Exit(1)
	}
}

func cmdApply(st *State) {
	_, build := fullPlan(st)
	fmt.Printf("# netgov apply — %d build cmds\n", len(build))
	for _, c := range build {
		fmt.Println("  " + strings.Join(c, " "))
	}
	if os.Geteuid() == 0 {
		applyRoot(st)
		return
	}
	sudoSelf("__apply")
}

// ---- helpers for editing state ----
func upByName(st *State, name string) *Uplink {
	for i := range st.Uplinks {
		if st.Uplinks[i].Name == name {
			return &st.Uplinks[i]
		}
	}
	return nil
}

func nextTable(st *State) int {
	used := map[int]bool{}
	for _, u := range st.Uplinks {
		used[u.Table] = true
	}
	for t := tableBase; t <= tableMax; t++ {
		if !used[t] {
			return t
		}
	}
	return tableBase
}

func isUSB(dev string) bool {
	p, err := filepath.EvalSymlinks("/sys/class/net/" + dev)
	return err == nil && strings.Contains(p, "/usb")
}

func isInternalBridge(n string) bool {
	return n == "docker0" || strings.HasPrefix(n, "virbr") || strings.HasPrefix(n, "br-") ||
		strings.HasPrefix(n, "lxcbr")
}

func scanDevices() []string {
	ents, _ := os.ReadDir("/sys/class/net")
	var devs []string
	for _, e := range ents {
		n := e.Name()
		switch {
		case n == "lo", isInternalBridge(n),
			strings.HasPrefix(n, "veth"), strings.HasPrefix(n, "docker"),
			strings.HasPrefix(n, "vnet"), strings.HasPrefix(n, "tap"),
			strings.HasPrefix(n, "tun"), strings.HasPrefix(n, "wg"),
			strings.HasPrefix(n, "p2p"):
			continue
		}
		devs = append(devs, n)
	}
	sort.Strings(devs)
	return devs
}

type bridgeInfo struct {
	Name   string `json:"name"`
	Subnet string `json:"subnet"`
}

func scanBridges() []bridgeInfo {
	ents, _ := os.ReadDir("/sys/class/net")
	var out []bridgeInfo
	for _, e := range ents {
		n := e.Name()
		if !isInternalBridge(n) {
			continue
		}
		out = append(out, bridgeInfo{Name: n, Subnet: linkNet(n, "4")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func devBaseName(dev string) string {
	switch {
	case strings.HasPrefix(dev, "wlx"):
		return "dongle"
	case strings.HasPrefix(dev, "wl"):
		return "wifi"
	case isUSB(dev):
		return "tether"
	default:
		return "cable"
	}
}

func uniqueName(st *State, base string) string {
	used := map[string]bool{}
	for _, u := range st.Uplinks {
		used[u.Name] = true
	}
	if !used[base] {
		return base
	}
	for i := 2; ; i++ {
		if n := base + itoa(i); !used[n] {
			return n
		}
	}
}

// resolveDev maps an uplink name OR a raw interface to a device name.
func resolveDev(st *State, arg string) string {
	if u := upByName(st, arg); u != nil {
		return u.Dev
	}
	return arg
}

// ensureUplink makes sure dev has an uplink entry (so it reappears when its AP is disabled).
func ensureUplink(st *State, dev string) {
	for _, u := range st.Uplinks {
		if u.Dev == dev {
			return
		}
	}
	st.Uplinks = append(st.Uplinks, Uplink{Name: uniqueName(st, devBaseName(dev)), Dev: dev, Table: nextTable(st)})
}

func isWifi(dev string) bool {
	_, err := os.Stat("/sys/class/net/" + dev + "/phy80211")
	return err == nil
}

func wifiIfaces() []string {
	var out []string
	for _, d := range scanDevices() {
		if isWifi(d) {
			out = append(out, d)
		}
	}
	return out
}

// apActive reports the AP's live SSID via `iw` ("up SSID=...") or "(down)".
func apActive(dev string) string {
	out, _ := run("iw", "dev", dev, "info")
	if strings.Contains(out, "type AP") {
		return "up"
	}
	return "(down)"
}

func cmdAP(st *State, args []string) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		for _, a := range st.APs {
			fmt.Printf("  %-16s SSID=%s band=%s ch=%d %s\n", a.Dev, a.SSID, a.Band, a.Channel, apActive(a.Dev))
		}
		if len(st.APs) == 0 {
			fmt.Println("  (no APs) — wifi interfaces available:", strings.Join(wifiIfaces(), " "))
		}
	case "on":
		if len(args) < 2 {
			fmt.Println("usage: netgov ap on <iface|uplink> --ssid <s> --psk <p> [--band bg|a] [--channel N]")
			os.Exit(2)
		}
		dev := resolveDev(st, args[1])
		if !isWifi(dev) {
			fmt.Println(dev, "is not a wifi interface")
			os.Exit(2)
		}
		ssid, _ := flagVal(args, "--ssid")
		psk, _ := flagVal(args, "--psk")
		band, _ := flagVal(args, "--band")
		if band == "" {
			band = "bg"
		}
		ch := 0
		if v, ok := flagVal(args, "--channel"); ok {
			ch, _ = strconv.Atoi(v)
		}
		if ssid == "" || psk == "" {
			fmt.Println("usage: netgov ap on <iface|uplink> --ssid <s> --psk <p> [--band bg|a] [--channel N]")
			os.Exit(2)
		}
		ensureUplink(st, dev) // so it reappears as an uplink when the AP is turned off
		a := AP{Dev: dev, SSID: ssid, PSK: psk, Band: band, Channel: ch}
		var keep []AP
		for _, x := range st.APs {
			if x.Dev != dev {
				keep = append(keep, x)
			}
		}
		st.APs = append(keep, a)
		must(saveState(st, statePath()))
		if out, err := apUp(a); err != nil {
			fmt.Fprintln(os.Stderr, "AP up failed:", out, err)
			os.Exit(1)
		}
		fmt.Printf("AP up: %s SSID=%s band=%s (uplink shadowed)\n", dev, ssid, band)
	case "off":
		if len(args) < 2 {
			fmt.Println("usage: netgov ap off <iface|uplink>")
			os.Exit(2)
		}
		dev := resolveDev(st, args[1])
		var keep []AP
		for _, x := range st.APs {
			if x.Dev != dev {
				keep = append(keep, x)
			}
		}
		st.APs = keep
		must(saveState(st, statePath()))
		out, _ := apDown(dev)
		fmt.Printf("AP down: %s (uplink restored) %s\n", dev, out)
	default:
		fmt.Println("usage: netgov ap [list|on <iface|uplink> --ssid <s> --psk <p> [--band bg|a] [--channel N]|off <iface|uplink>]")
	}
}

func cmdInit(st *State) {
	have := map[string]bool{}
	usedNames := map[string]bool{}
	for _, u := range st.Uplinks {
		have[u.Dev] = true
		usedNames[u.Name] = true
	}
	uniq := func(base string) string {
		if !usedNames[base] {
			usedNames[base] = true
			return base
		}
		for i := 2; ; i++ {
			if n := base + itoa(i); !usedNames[n] {
				usedNames[n] = true
				return n
			}
		}
	}
	for _, dev := range scanDevices() {
		if have[dev] {
			continue
		}
		var name string
		switch {
		case strings.HasPrefix(dev, "wlx"):
			name = "dongle"
		case strings.HasPrefix(dev, "wl"):
			name = "wifi"
		case isUSB(dev):
			name = "tether"
		default:
			name = "cable"
		}
		name = uniq(name)
		u := Uplink{Name: name, Dev: dev, Table: nextTable(st)}
		st.Uplinks = append(st.Uplinks, u)
		fmt.Printf("uplink + %-8s dev=%s table=%d\n", name, dev, u.Table)
	}
	if len(st.Rules) == 0 {
		// "Lifeline" idea: pin a critical destination to a stable uplink so it stays on a
		// known-good path regardless of the overall default. Left for the user to define, e.g.:
		//   netgov rule add --domain example.com --via wifi
		fmt.Println("tip   pin a critical domain to a stable uplink (lifeline), e.g. netgov rule add --domain example.com --via wifi")
	}
	if st.DefaultV6 == "" {
		st.DefaultV6 = blockVia
		fmt.Println("default v6 -> block (leak-protect; no global IPv6 present)")
	}
	must(saveState(st, statePath()))
}

func cmdStatus(st *State) {
	if len(st.APs) > 0 {
		fmt.Println("ACCESS POINTS (serving; uplink shadowed)")
		for _, a := range st.APs {
			ssid := apActive(a.Dev)
			fmt.Printf("  %-16s SSID=%-12s band=%-3s ch=%-3d %s\n", a.Dev, a.SSID, a.Band, a.Channel, ssid)
		}
	}
	fmt.Println("UPLINKS                              IPv4                              IPv6")
	for _, u := range st.Uplinks {
		if servingDev(st, u.Dev) {
			continue
		}
		s4, g4, l4 := uplinkLive(u, "4")
		s6, g6, l6 := uplinkLive(u, "6")
		fmt.Printf("  %-8s dev=%-16s tbl=%-3d v4[%s src=%s gw=%s net=%s]  v6[%s]\n",
			u.Name, u.Dev, u.Table, upDown(l4), dash(s4), dash(g4), netOK(l4, s4, "4"),
			famSummary(l6, s6, g6))
	}
	fmt.Println("DESTINATION RULES (domain -> uplink)")
	for _, r := range st.Rules {
		if r.Domain == "" {
			continue
		}
		fmt.Printf("  %-26s -> %-8s [%s] %s\n", r.Domain, r.Via, dash(r.Fam), strings.Join(resolveFam(r.Domain, "4"), " "))
	}
	fmt.Println("SOURCE RULES (containers/VMs/custom -> uplink)")
	for _, r := range st.Rules {
		if r.From == "" {
			continue
		}
		fmt.Printf("  %-26s -> %-8s [%s]\n", r.From, r.Via, dash(r.Fam))
	}
	fmt.Printf("OVERALL DEFAULT:  v4=%s  v6=%s\n", dash(st.DefaultV4), dash(st.DefaultV6))
	fmt.Println("DETECTED INTERNAL BRIDGES:")
	for _, b := range scanBridges() {
		fmt.Printf("  %-16s %s\n", b.Name, dash(b.Subnet))
	}
}

func famSummary(live bool, src, gw string) string {
	if !live {
		return "no global v6"
	}
	return "src=" + dash(src) + " gw=" + dash(gw)
}
func upDown(b bool) string {
	if b {
		return "UP"
	}
	return "--"
}
func netOK(live bool, bind, fam string) string {
	if !live {
		return "-"
	}
	if pingVia(bind, fam) {
		return "OK"
	}
	return "--"
}

// pingVia probes the internet bound to a SOURCE ADDRESS (not a device), so the
// probe is subject to our `from <src> table N` policy rule and reflects the path
// traffic from that uplink actually takes once apply has run.
func pingVia(bind, fam string) bool {
	if bind == "" {
		return false
	}
	args := []string{"ping", "-c1", "-W2", "-I", bind, "1.1.1.1"}
	if fam == "6" {
		args = []string{"ping", "-6", "-c1", "-W2", "-I", bind, "2606:4700:4700::1111"}
	}
	_, err := run(args...)
	return err == nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdLink(args []string) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		out, _ := run("nmcli", "-t", "-f", "NAME,TYPE,DEVICE,STATE", "con", "show")
		fmt.Println("CONNECTIONS (name|type|device|state)\n" + out)
		out2, _ := run("nmcli", "-t", "-f", "DEVICE,TYPE,STATE,CONNECTION", "device", "status")
		fmt.Println("\nDEVICES (device|type|state|connection)\n" + out2)
	case "up", "down":
		if len(args) < 2 {
			fmt.Println("usage: netgov link", args[0], "<connection-name>")
			os.Exit(2)
		}
		out, err := run("nmcli", "con", args[0], args[1])
		fmt.Println(out)
		if err != nil {
			os.Exit(1)
		}
	case "reapply":
		if len(args) < 2 {
			fmt.Println("usage: netgov link reapply <device>")
			os.Exit(2)
		}
		out, _ := run("nmcli", "device", "reapply", args[1])
		fmt.Println(out)
	default:
		fmt.Println("usage: netgov link [list|up <name>|down <name>|reapply <dev>]")
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func flagVal(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"status"}
	}
	cmd, rest := args[0], args[1:]

	sp := statePath()
	if v, ok := flagVal(rest, "--state"); ok {
		sp = v
	}
	st := loadState(sp)

	switch cmd {
	case "status":
		cmdStatus(st)
	case "init":
		cmdInit(st)
	case "apply", "refresh":
		cmdApply(st)
	case "reset":
		if os.Geteuid() == 0 {
			resetRoot(st)
		} else {
			sudoSelf("__reset")
		}
	case "__apply":
		applyRoot(st)
	case "__reset":
		resetRoot(st)
	case "plan":
		_, build := fullPlan(st)
		fmt.Printf("# netgov dry-run — %d cmds (nothing executed)\n", len(build))
		for _, c := range build {
			fmt.Println("  " + strings.Join(c, " "))
		}
	case "link":
		cmdLink(rest)
	case "web":
		cmdWeb(st, rest)
	case "install":
		cmdInstall()
	case "uplink":
		cmdUplink(st, rest)
	case "ap":
		cmdAP(st, rest)
	case "rule":
		cmdRule(st, rest)
	case "default":
		cmdDefault(st, rest)
	case "pat-list", "pat-set", "pat-del", "pat-apply":
		cmdPattern(st, cmd, rest)
	case "eval":
		if hasFlag(rest, "--apply") {
			if os.Geteuid() == 0 {
				fmt.Println("eval ->", evalPattern(st, true))
				saveStateKeepOwner(st, sp)
			} else {
				sudoSelf("__eval-apply")
			}
		} else {
			fmt.Println("would select:", evalPattern(st, false))
		}
	case "__eval-apply":
		fmt.Println("eval ->", evalPattern(st, true))
		saveStateKeepOwner(st, sp)
	case "arm":
		cmdArm(st, rest)
	case "disarm":
		cmdDisarm(st)
	case "roled-loop":
		roledLoop(sp)
	default:
		usage()
	}
}

func cmdUplink(st *State, args []string) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		for _, u := range st.Uplinks {
			fmt.Printf("  %-8s dev=%-16s table=%d gw=%s\n", u.Name, u.Dev, u.Table, dash(u.Gateway))
		}
	case "define":
		if len(args) < 2 {
			fmt.Println("usage: netgov uplink define <name> --dev <iface> [--gw <ip>] [--table N]")
			os.Exit(2)
		}
		name := args[1]
		dev, _ := flagVal(args, "--dev")
		gw, _ := flagVal(args, "--gw")
		t := nextTable(st)
		if v, ok := flagVal(args, "--table"); ok {
			t, _ = strconv.Atoi(v)
		}
		if u := upByName(st, name); u != nil {
			if dev != "" {
				u.Dev = dev
			}
			u.Table = t
			if gw != "" {
				u.Gateway = gw
			}
		} else {
			st.Uplinks = append(st.Uplinks, Uplink{Name: name, Dev: dev, Table: t, Gateway: gw})
		}
		must(saveState(st, statePath()))
		fmt.Printf("uplink %s -> dev=%s table=%d gw=%s\n", name, dev, t, dash(gw))
	case "del":
		var keep []Uplink
		for _, u := range st.Uplinks {
			if u.Name != args[1] {
				keep = append(keep, u)
			}
		}
		st.Uplinks = keep
		must(saveState(st, statePath()))
		fmt.Println("removed uplink", args[1])
	default:
		fmt.Println("usage: netgov uplink [list|define <name> --dev <i> [--gw <ip>]|del <name>]")
	}
}

func cmdRule(st *State, args []string) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		for _, r := range st.Rules {
			key := r.Domain
			if key == "" {
				key = "from:" + r.From
			}
			fmt.Printf("  %-28s -> %-8s [%s]\n", key, r.Via, dash(r.Fam))
		}
	case "add":
		dom, _ := flagVal(args, "--domain")
		from, _ := flagVal(args, "--from")
		via, _ := flagVal(args, "--via")
		fam, _ := flagVal(args, "--fam")
		if fam == "" {
			fam = "both"
		}
		if via == "" || (dom == "" && from == "") {
			fmt.Println("usage: netgov rule add (--domain <d> | --from <cidr|iface>) --via <uplink|block> [--fam 4|6|both]")
			os.Exit(2)
		}
		var keep []Rule
		for _, r := range st.Rules {
			if (dom != "" && r.Domain == dom) || (from != "" && r.From == from) {
				continue
			}
			keep = append(keep, r)
		}
		st.Rules = append(keep, Rule{Domain: dom, From: from, Via: via, Fam: fam})
		must(saveState(st, statePath()))
		fmt.Println("rule added")
	case "del":
		dom, _ := flagVal(args, "--domain")
		from, _ := flagVal(args, "--from")
		var keep []Rule
		for _, r := range st.Rules {
			if (dom != "" && r.Domain == dom) || (from != "" && r.From == from) {
				continue
			}
			keep = append(keep, r)
		}
		st.Rules = keep
		must(saveState(st, statePath()))
		fmt.Println("rule removed")
	default:
		fmt.Println("usage: netgov rule [list|add ...|del ...]")
	}
}

func cmdDefault(st *State, args []string) {
	if len(args) == 0 {
		fmt.Printf("default v4=%s v6=%s\n", dash(st.DefaultV4), dash(st.DefaultV6))
		return
	}
	switch args[0] {
	case "set":
		if v, ok := flagVal(args, "--v4"); ok {
			st.DefaultV4 = v
		}
		if v, ok := flagVal(args, "--v6"); ok {
			st.DefaultV6 = v
		}
		must(saveState(st, statePath()))
		fmt.Printf("default v4=%s v6=%s\n", dash(st.DefaultV4), dash(st.DefaultV6))
	case "clear":
		st.DefaultV4, st.DefaultV6 = "", ""
		must(saveState(st, statePath()))
		fmt.Println("defaults cleared")
	default:
		fmt.Println("usage: netgov default [set [--v4 <u>] [--v6 <u|block>]|clear]")
	}
}

// ---------- patterns / roled-style failover ----------

const (
	checkInterval = 20 * time.Second // loop cadence
	valMax        = 45 * time.Second // max time to wait for a just-applied uplink to reach the internet
	valPoll       = 3 * time.Second  // poll granularity within a validation/debounce window
	debounceN     = 3                // consecutive failures before a disruptive re-eval (anti-flap)
)

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
func hasFlag(args []string, f string) bool {
	for _, a := range args {
		if a == f {
			return true
		}
	}
	return false
}

// normDefault maps the user-facing "direct" to the internal "" (main table).
func normDefault(v string) string {
	if v == "direct" {
		return ""
	}
	return v
}
func famDefault(p *Pattern, fam string) string {
	if fam == "6" {
		return p.V6
	}
	return p.V4
}

// patternHasDuty: false only if BOTH families are blocked (a deliberate no-egress pattern).
func patternHasDuty(p *Pattern) bool {
	return !(normDefault(p.V4) == blockVia && normDefault(p.V6) == blockVia)
}

func patByName(st *State, name string) *Pattern {
	for i := range st.Patterns {
		if st.Patterns[i].Name == name {
			return &st.Patterns[i]
		}
	}
	return nil
}
func patternsByPrio(st *State) []*Pattern {
	ps := make([]*Pattern, 0, len(st.Patterns))
	for i := range st.Patterns {
		ps = append(ps, &st.Patterns[i])
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Priority > ps[j].Priority })
	return ps
}

// ssidIfaceDev resolves a pattern's SSIDIface (an uplink name, or a raw device) to a netdev.
func ssidIfaceDev(st *State, p *Pattern) string {
	if p.SSIDIface == "" {
		return ""
	}
	if u := upByName(st, p.SSIDIface); u != nil {
		return u.Dev
	}
	return p.SSIDIface
}

// ssidVisible reports whether SSID is currently in range on dev, using NetworkManager's
// CACHED scan (--rescan no) so we never force a disruptive scan on a connected STA (the
// lifeline). NM refreshes the cache on its own background cadence.
func ssidVisible(dev, ssid string) bool {
	if dev == "" || ssid == "" {
		return false
	}
	out, err := run("nmcli", "-t", "-f", "SSID", "device", "wifi", "list", "ifname", dev, "--rescan", "no")
	if err != nil {
		return false
	}
	for _, ln := range strings.Split(out, "\n") {
		if strings.ReplaceAll(strings.TrimSpace(ln), `\:`, ":") == ssid {
			return true
		}
	}
	return false
}

// patternSSIDOK: true if no SSID criterion, or ANY of the comma-listed SSIDs is in range.
func patternSSIDOK(st *State, p *Pattern) bool {
	if p.SSID == "" {
		return true
	}
	dev := ssidIfaceDev(st, p)
	for _, s := range splitCSV(p.SSID) {
		if ssidVisible(dev, s) {
			return true
		}
	}
	return false
}

// ensurePatternAPs enables (gently, never tears down) the APs a pattern declares.
func ensurePatternAPs(st *State, p *Pattern) {
	for _, dev := range p.APs {
		for _, a := range st.APs {
			if a.Dev == dev && apActive(dev) != "up" {
				_, _ = apUp(a)
			}
		}
	}
}

// patternSatisfiable: every Require uplink must exist and be live (v4 or v6), and the
// SSID criterion (if any) must be in range.
func patternSatisfiable(st *State, p *Pattern) bool {
	if p.Floor {
		return true
	}
	for _, name := range p.Require {
		u := upByName(st, name)
		if u == nil {
			return false
		}
		_, _, l4 := uplinkLive(*u, "4")
		_, _, l6 := uplinkLive(*u, "6")
		if !l4 && !l6 {
			return false
		}
	}
	return patternSSIDOK(st, p)
}

// ensureFloor guarantees a reachable fallback exists (direct v4, blocked v6, no rules).
func ensureFloor(st *State) {
	for i := range st.Patterns {
		if st.Patterns[i].Floor {
			return
		}
	}
	st.Patterns = append(st.Patterns, Pattern{Name: "floor", Priority: 0, Floor: true, V4: "direct", V6: blockVia})
}

func pingPlain(fam string) bool {
	args := []string{"ping", "-c1", "-W2", "1.1.1.1"}
	if fam == "6" {
		args = []string{"ping", "-6", "-c1", "-W2", "2606:4700:4700::1111"}
	}
	_, err := run(args...)
	return err == nil
}

// patternInternetOK: true if any non-blocked family reaches the internet via its default
// (direct = plain ping; uplink = ping bound to that uplink's source, honouring our rules).
func patternInternetOK(st *State, p *Pattern) bool {
	for _, fam := range []string{"4", "6"} {
		v := normDefault(famDefault(p, fam))
		if v == blockVia {
			continue
		}
		if v == "" {
			if pingPlain(fam) {
				return true
			}
			continue
		}
		if u := upByName(st, v); u != nil {
			if src, _, live := uplinkLive(*u, fam); live && pingVia(src, fam) {
				return true
			}
		}
	}
	return false
}

// validatePattern polls patternInternetOK up to valMax, succeeding as soon as the link is up.
func validatePattern(st *State, p *Pattern) bool {
	deadline := time.Now().Add(valMax)
	for {
		if patternInternetOK(st, p) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(valPoll)
	}
}

// patternReallyDown confirms a sustained outage (debounceN consecutive failures).
func patternReallyDown(st *State, p *Pattern) bool {
	for i := 0; i < debounceN; i++ {
		if patternInternetOK(st, p) {
			return false
		}
		if i < debounceN-1 {
			time.Sleep(valPoll)
		}
	}
	return true
}

// activatePattern copies a pattern's snapshot into the live State (does NOT apply).
func activatePattern(st *State, p *Pattern) {
	st.DefaultV4 = normDefault(p.V4)
	st.DefaultV6 = normDefault(p.V6)
	st.Rules = append([]Rule(nil), p.Rules...)
	st.ActivePattern = p.Name
}

// evalPattern selects (and, if apply, activates+applies) the best pattern. Returns its name.
func evalPattern(st *State, apply bool) string {
	ensureFloor(st)
	var floor *Pattern
	for _, p := range patternsByPrio(st) {
		if p.Floor {
			if floor == nil {
				floor = p
			}
			continue
		}
		if !patternSatisfiable(st, p) {
			continue
		}
		if !apply {
			return p.Name // dry: highest-priority satisfiable
		}
		activatePattern(st, p)
		applyRoot(st)
		ensurePatternAPs(st, p)
		if !patternHasDuty(p) || validatePattern(st, p) {
			return p.Name
		}
		// no internet on this uplink -> walk to the next pattern
	}
	if floor != nil {
		if apply {
			activatePattern(st, floor)
			applyRoot(st)
			ensurePatternAPs(st, floor)
		}
		return floor.Name
	}
	return ""
}

// saveStateKeepOwner writes state.json and, when run as root (the loop), restores the
// file's original owner so the user's config doesn't become root-owned.
func saveStateKeepOwner(st *State, path string) {
	uid, gid := -1, -1
	if fi, err := os.Stat(path); err == nil {
		if s, ok := fi.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(s.Uid), int(s.Gid)
		}
	}
	_ = saveState(st, path)
	if uid >= 0 {
		_ = os.Chown(path, uid, gid)
	}
}

func armState(st *State) string {
	if st.Armed == "" {
		return "off (disarmed)"
	}
	return "ON (mode=" + st.Armed + ", active=" + dash(st.ActivePattern) + ")"
}

// runPriv runs a privileged command directly when root, else via `sudo -A` (askpass).
func runPriv(argv ...string) error {
	if os.Geteuid() == 0 {
		_, err := run(argv...)
		return err
	}
	cmd := exec.Command("sudo", append([]string{"-A"}, argv...)...)
	cmd.Env = askpassEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func cmdPattern(st *State, verb string, args []string) {
	switch verb {
	case "pat-list":
		ensureFloor(st)
		fmt.Println("patterns (priority desc):")
		for _, p := range patternsByPrio(st) {
			sat := "not-now"
			if patternSatisfiable(st, p) {
				sat = "ok"
			}
			tag := ""
			if p.Floor {
				tag += " floor"
			}
			if p.Name == st.ActivePattern {
				tag += " *ACTIVE"
			}
			trig := dash(strings.Join(p.Require, ","))
			if p.SSID != "" {
				trig += " +ssid:" + p.SSID + "@" + dash(p.SSIDIface)
			}
			fmt.Printf("  [%3d] %-14s (%s)%s  v4=%s v6=%s trigger=%s rules=%d aps=%s\n",
				p.Priority, p.Name, sat, tag, dash(normDefault(p.V4)), dash(normDefault(p.V6)),
				trig, len(p.Rules), dash(strings.Join(p.APs, ",")))
		}
		fmt.Println("automation:", armState(st))
	case "pat-set":
		if len(args) < 2 {
			fmt.Println("usage: netgov pat-set <name> <prio> [--require a,b] [--v4 U|block|direct] [--v6 ...] [--snapshot] [--floor]")
			os.Exit(2)
		}
		name := args[0]
		prio, _ := strconv.Atoi(args[1])
		p := patByName(st, name)
		if p == nil {
			st.Patterns = append(st.Patterns, Pattern{Name: name})
			p = &st.Patterns[len(st.Patterns)-1]
		}
		p.Priority = prio
		if hasFlag(args, "--snapshot") { // capture the current live policy as this pattern
			p.V4 = st.DefaultV4
			p.V6 = st.DefaultV6
			p.Rules = append([]Rule(nil), st.Rules...)
		}
		if v, ok := flagVal(args, "--v4"); ok {
			p.V4 = v
		}
		if v, ok := flagVal(args, "--v6"); ok {
			p.V6 = v
		}
		if v, ok := flagVal(args, "--require"); ok {
			p.Require = splitCSV(v)
		}
		if v, ok := flagVal(args, "--ssid"); ok {
			p.SSID = v
		}
		if v, ok := flagVal(args, "--ssid-iface"); ok {
			p.SSIDIface = v
		}
		if v, ok := flagVal(args, "--ap"); ok {
			p.APs = splitCSV(v)
		}
		if hasFlag(args, "--floor") {
			p.Floor = true
		}
		must(saveState(st, statePath()))
		fmt.Println("pattern saved:", name)
	case "pat-del":
		if len(args) < 1 {
			fmt.Println("usage: netgov pat-del <name>")
			os.Exit(2)
		}
		var keep []Pattern
		for _, p := range st.Patterns {
			if p.Name != args[0] {
				keep = append(keep, p)
			}
		}
		st.Patterns = keep
		must(saveState(st, statePath()))
		fmt.Println("pattern removed:", args[0])
	case "pat-apply":
		if len(args) < 1 {
			fmt.Println("usage: netgov pat-apply <name>")
			os.Exit(2)
		}
		p := patByName(st, args[0])
		if p == nil {
			fmt.Println("no such pattern:", args[0])
			os.Exit(1)
		}
		activatePattern(st, p)
		must(saveState(st, statePath()))
		fmt.Println("activated", p.Name, "-> applying")
		if os.Geteuid() == 0 {
			applyRoot(st)
		} else {
			sudoSelf("__apply")
		}
		ensurePatternAPs(st, p)
	}
}

func cmdArm(st *State, args []string) {
	mode := "armed"
	if hasFlag(args, "--dry") || hasFlag(args, "dry") {
		mode = "dry"
	}
	ensureFloor(st)
	st.Armed = mode
	must(saveState(st, statePath()))
	if err := runPriv("systemctl", "enable", "--now", "netgov-roled.service"); err != nil {
		fmt.Fprintln(os.Stderr, "could not enable netgov-roled.service (run `netgov install`?):", err)
		os.Exit(1)
	}
	fmt.Println("armed (mode=" + mode + ") — netgov-roled.service enabled")
}

func cmdDisarm(st *State) {
	st.Armed = ""
	must(saveState(st, statePath()))
	if err := runPriv("systemctl", "disable", "--now", "netgov-roled.service"); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not disable service:", err)
	}
	fmt.Println("disarmed — netgov-roled.service stopped")
}

// roledLoop is the root failover loop started by netgov-roled.service. It boots/idles
// until armed, then keeps the active pattern's internet healthy (poll + debounce), and
// re-evaluates on a sustained outage. Logs to stderr (captured by journald).
func roledLoop(sp string) {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "roled-loop must run as root")
		os.Exit(1)
	}
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "[netgov-roled] "+format+"\n", a...) }
	logf("loop start")
	evaluated := false
	for {
		st := loadState(sp)
		if st.Armed == "" || len(st.Patterns) == 0 {
			evaluated = false
			time.Sleep(checkInterval)
			continue
		}
		active := patByName(st, st.ActivePattern)
		need := !evaluated || active == nil
		if active != nil && patternHasDuty(active) && patternReallyDown(st, active) {
			logf("active '%s' internet down (confirmed x%d)", active.Name, debounceN)
			need = true
		}
		if need {
			if st.Armed == "dry" {
				logf("re-eval (DRY): would select '%s'", evalPattern(st, false))
			} else {
				name := evalPattern(st, true)
				saveStateKeepOwner(st, sp)
				logf("re-eval -> selected '%s'", name)
			}
			evaluated = true
		}
		time.Sleep(checkInterval)
	}
}

func usage() {
	fmt.Println(`netgov — host multi-homing / policy-routing switchboard
  status                                       uplinks, rules, defaults (per family), bridges
  init                                         auto-detect interfaces -> seed uplinks
  uplink list|define <n> --dev <i> [--gw ip]|del
  ap    list|on <iface|uplink> --ssid <s> --psk <p> [--band bg|a] [--channel N]|off <iface|uplink>
  link  list|up <name>|down <name>|reapply <dev>   (reapply = restore link to NM profile)
  rule  add (--domain <d>|--from <cidr|iface>) --via <u|block> [--fam 4|6|both] | del | list
  default set [--v4 <u>] [--v6 <u|block>] | clear
  apply | refresh                              realise state (sudo -A)
  reset                                        flush all netgov rules -> restore NM baseline (sudo -A)
  plan                                         dry-run, nothing executed
  pat-list                                     patterns + satisfiability + arm state
  pat-set <name> <prio> [--require a,b] [--ssid S[,S2] --ssid-iface <uplink>] [--ap <dev,..>]
                        [--v4 U|block|direct] [--v6 ..] [--snapshot] [--floor]
  pat-del <name> | pat-apply <name>            delete / manually activate a pattern
  eval [--apply]                               pick best satisfiable pattern (dry, or apply)
  arm [--dry] | disarm                         root failover loop (auto pattern selection); boots disarmed
  web [--addr 127.0.0.1:8474] | install`)
}
