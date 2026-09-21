package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeExec swaps execRun for a counting fake for the length of one test.
func fakeExec(t *testing.T, answer func(argv []string) (string, error)) *[]string {
	t.Helper()
	var calls []string
	orig := execRun
	execRun = func(argv []string) (string, error) {
		calls = append(calls, strings.Join(argv, " "))
		return answer(argv)
	}
	t.Cleanup(func() { execRun = orig })
	return &calls
}

// lean-execution/1: profileForIface forked 1+N nmcli (N = saved profiles; 53 on .153, 1.62 s of CPU
// per dashboard refresh). The property is a CONSTANT fork count, asserted at a size where the old
// code would have forked 54 times.
func TestProfileForIfaceForksTwiceWhateverTheProfileCount(t *testing.T) {
	const n = 53
	uuid := func(i int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", i) }
	calls := fakeExec(t, func(argv []string) (string, error) {
		a := strings.Join(argv, " ")
		switch {
		case a == "nmcli -t -f UUID,NAME connection show":
			var b strings.Builder
			for i := 0; i < n; i++ {
				name := fmt.Sprintf("Profile %d", i)
				if i == 40 {
					name = `Cafe\:Wifi 5G` // terse output escapes the separator
				}
				fmt.Fprintf(&b, "%s:%s\n", uuid(i), name)
			}
			return b.String(), nil
		case strings.HasPrefix(a, "nmcli -t -f connection.uuid,connection.interface-name connection show "):
			var b strings.Builder
			for _, u := range argv[7:] {
				dev := "wlo1"
				if u == uuid(40) || u == uuid(45) { // two profiles bound: the FIRST listed must win
					dev = "enx0"
				}
				fmt.Fprintf(&b, "connection.uuid:%s\nconnection.interface-name:%s\n\n", u, dev)
			}
			return b.String(), fmt.Errorf("exit status 10") // one vanished profile must not blind it
		}
		return "", fmt.Errorf("unexpected %q", a)
	})
	if got := profileForIface("enx0"); got != "Cafe:Wifi 5G" {
		t.Errorf("profileForIface = %q, want the first bound profile, unescaped", got)
	}
	if len(*calls) != 2 {
		t.Errorf("profileForIface forked %d times for %d profiles, want 2:\n%s", len(*calls), n, strings.Join(*calls, "\n"))
	}
	if got := profileForIface("eno9"); got != "" {
		t.Errorf("unbound interface resolved to %q", got)
	}
}

// Inside one view build a repeated read-only query forks ONCE; a command that may change the host
// empties the memo so the next read is fresh; outside a build nothing is remembered at all.
func TestReadMemoDedupesReadsAndNeverOutlivesAWrite(t *testing.T) {
	gen := 0
	calls := fakeExec(t, func(argv []string) (string, error) { gen++; return fmt.Sprint(gen), nil })
	read := []string{"ip", "-j", "addr", "show"}

	readMemo.begin()
	a, _ := run(read...)
	b, _ := run(read...)
	_, _ = run("ping", "-c", "1", "10.0.0.1") // a neutral probe must not flush the memo
	c, _ := run(read...)
	if a != b || b != c || len(*calls) != 2 {
		t.Fatalf("repeated read within a build: %q %q %q in %d forks, want one answer and 2 forks", a, b, c, len(*calls))
	}
	_, _ = run("ip", "rule", "add", "from", "10.0.0.10", "table", "100")
	if d, _ := run(read...); d == a {
		t.Error("a read after a mutation was served from the memo")
	}
	readMemo.end()

	before := len(*calls)
	x, _ := run(read...)
	y, _ := run(read...)
	if x == y || len(*calls) != before+2 {
		t.Error("outside a view build a read was memoised")
	}
}

func TestExecClass(t *testing.T) {
	cases := map[string]execKind{
		"ip -j addr show":                                           classRead,
		"ip -4 -o addr":                                             classRead,
		"ip -4 route get 1.1.1.1":                                   classRead,
		"ip -j route show table 100":                                classRead,
		"/usr/sbin/ip -j rule show":                                 classRead,
		"ip rule add from 10.0.0.2 table 100":                       classMutate,
		"ip link set dev eth0 down":                                 classMutate,
		"ip route flush table 100":                                  classMutate,
		"ip -n ns1 route replace default via 10.0.0.1":              classMutate,
		"nmcli -t -f GENERAL.CONNECTION device show wlo1":           classRead,
		"nmcli -g ipv4.never-default connection show Wired 1":       classRead,
		"nmcli -t -f SSID device wifi list ifname wlo1 --rescan no": classRead,
		"nmcli device wifi list":                                    classMutate, // may trigger a scan
		"nmcli connection modify LH ipv4.route-metric 50":           classMutate,
		"nmcli con up LH":                                           classMutate,
		"nmcli device reapply wlo1":                                 classMutate,
		"ping -c 1 10.0.0.1":                                       classNeutral,
		"ethtool -P eth0":                                           classNeutral,
		"ethtool -s eth0 wol g":                                     classMutate,
		"iw dev wlo1 link":                                          classNeutral,
		"iw dev wlo1 set power_save off":                            classMutate,
		"systemctl restart netgov-web":                              classMutate,
		"something-new --flag":                                      classMutate, // unknown = assume it changes the host
	}
	for cmd, want := range cases {
		if got := execClass(strings.Fields(cmd)); got != want {
			t.Errorf("execClass(%q) = %d, want %d", cmd, got, want)
		}
	}
}

// A hidden tab must not poll: every refresh costs the HOST, and one tab per box stays open.
func TestHiddenTabDoesNotPoll(t *testing.T) {
	i := strings.Index(pageHTML, "setInterval(")
	if i < 0 {
		t.Fatal("no refresh timer found")
	}
	if !strings.Contains(pageHTML[i:i+120], "document.hidden") {
		t.Error("the refresh timer polls regardless of whether the tab is visible")
	}
	if !strings.Contains(pageHTML, "visibilitychange") {
		t.Error("a tab that becomes visible does not refresh at once")
	}
}

// The panel's htpdate verdict is a measurement with an age, not a measurement per refresh: 17 of
// the 54 forks left in a refresh, plus three HTTPS requests and a journal line, every 15 s.
func TestHtpdateVerdictIsReusedUntilItExpires(t *testing.T) {
	n, ok := 0, true
	orig := htpMeasure
	htpMeasure = func() (bool, string) { n++; return ok, "" }
	t.Cleanup(func() { htpMeasure = orig; htpView.at = time.Time{} })
	htpView.at = time.Time{}

	htpdateUsableForView()
	htpdateUsableForView()
	if n != 1 {
		t.Fatalf("two refreshes inside the window measured %d times, want 1", n)
	}
	htpView.at = time.Now().Add(-htpViewFreshOK - time.Second)
	htpdateUsableForView()
	if n != 2 {
		t.Fatalf("an expired verdict was not re-measured (n=%d)", n)
	}
	ok = false
	htpView.at = time.Now().Add(-htpViewFreshOK - time.Second)
	htpdateUsableForView() // now failing
	htpView.at = time.Now().Add(-htpViewFreshFail - time.Second)
	htpdateUsableForView()
	if n != 4 {
		t.Errorf("a FAILED verdict must be re-checked after %v, not %v (n=%d)", htpViewFreshFail, htpViewFreshOK, n)
	}
}
