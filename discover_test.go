package main

import (
	"regexp"
	"strings"
	"testing"
)

// n-1001: a peer read the help, found no verb for a claim group and hand-edited state.json —
// while `claim set` had existed since 2.1. Every verb a person types must be in the help.
func TestUsageNamesEveryClaimVerb(t *testing.T) {
	for _, v := range claimVerbs {
		if v == "tick" { // the timer's entry point, deliberately not advertised
			continue
		}
		re := regexp.MustCompile(`claim (\[[^\]]*\b` + v + `\b[^\]]*\]|` + v + `\b)`)
		if !re.MatchString(usageText) {
			t.Errorf("usageText does not name `claim %s`", v)
		}
		if !re.MatchString(claimUsage) {
			t.Errorf("claimUsage does not name `claim %s`", v)
		}
	}
	for name, txt := range map[string]string{"usageText": usageText, "claimUsage": claimUsage} {
		if !strings.Contains(txt, "identity=<mac>") {
			t.Errorf("%s omits identity=<mac>, the mechanism the docs say to prefer", name)
		}
	}
}

func TestClaimInertMsgNamesTheVerb(t *testing.T) {
	if m := claimInertMsg("LH"); !strings.Contains(m, "netgov claim set LH <address>") {
		t.Errorf("inert message does not say how to declare a group on the active pattern: %q", m)
	}
	if m := claimInertMsg(""); !strings.Contains(m, "netgov claim set <pattern>") {
		t.Errorf("with no active pattern the hint must use a placeholder, got %q", m)
	}
	if claimVerbKnown("add") || claimVerbKnown("help") || !claimVerbKnown("set") {
		t.Error("claimVerbKnown disagrees with the dispatcher")
	}
}

// The stale-binary banner must name the unit, scope and account that really run the process —
// a service account's user manager, while the operator logs in as someone else.
func TestRestartHintFrom(t *testing.T) {
	cases := []struct {
		name, cgroup, who string
		want, reject      []string
	}{
		{"user unit under another account",
			"0::/user.slice/user-1001.slice/user@1001.service/app.slice/netgov-web.service\n", "svc",
			[]string{"as svc: systemctl --user restart netgov-web.service", "sudo systemctl --user -M svc@ restart netgov-web.service"}, nil},
		{"system unit",
			"0::/system.slice/netgov-web.service\n", "root",
			[]string{"sudo systemctl restart netgov-web.service"}, []string{"--user"}},
		{"renamed user unit",
			"0::/user.slice/user-1000.slice/user@1000.service/app.slice/netgov-dash.service\n", "pm",
			[]string{"restart netgov-dash.service"}, []string{"netgov-web"}},
		{"run by hand from a terminal inside the user manager",
			"0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-org.gnome.Terminal.slice/vte-spawn-1.scope\n", "pm",
			[]string{"not a systemd service", "PID 42"}, []string{"systemctl"}},
		{"run by hand from an ssh session",
			"0::/user.slice/user-1000.slice/session-4.scope\n", "pm",
			[]string{"not a systemd service"}, []string{"systemctl"}},
		{"cgroup v1",
			"12:pids:/user.slice\n1:name=systemd:/user.slice/user-1001.slice/user@1001.service/app.slice/netgov-web.service\n", "svc",
			[]string{"-M svc@ restart netgov-web.service"}, nil},
	}
	for _, c := range cases {
		got := restartHintFrom(c.cgroup, c.who, 42)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: hint %q lacks %q", c.name, got, w)
			}
		}
		for _, r := range c.reject {
			if strings.Contains(got, r) {
				t.Errorf("%s: hint %q must not contain %q", c.name, got, r)
			}
		}
	}
	if strings.Contains(pageHTML, "systemctl --user restart netgov-web") {
		t.Error("the page still hard-codes a restart command instead of using restart_hint")
	}
}

// Backlog acceptance (2026-09-13): the host is in <title> and the heading, read per request,
// and escaped because it is operator-chosen text going into markup.
func TestRenderPageCarriesHost(t *testing.T) {
	p := renderPage("box<1>")
	for _, w := range []string{"<title>netgov · box&lt;1&gt;</title>", `<span id="host">box&lt;1&gt;</span>`} {
		if !strings.Contains(p, w) {
			t.Errorf("rendered page lacks %q", w)
		}
	}
	if strings.Contains(p, "__HOST__") || strings.Contains(p, "__PAGE_BUILD__") {
		t.Error("a placeholder survived rendering")
	}
	if a, b := renderPage("alpha"), renderPage("beta"); etagOf(a) == etagOf(b) {
		t.Error("two hosts produced one ETag — a cached page could name the wrong box")
	}
}

func TestStatusHeaderNamesHost(t *testing.T) {
	if h := statusHeader(); !strings.HasPrefix(h, "HOST "+hostLabel()+" ") || !strings.Contains(h, artefactVersion) {
		t.Errorf("status header %q does not lead with the host and build", h)
	}
}
