package main

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// fakeNTP answers one SNTP request with the stratum given and a clock skewed by `skew` from
// ours, then closes. Loopback only: the point is the arithmetic and the stratum, not a network.
func fakeNTP(t *testing.T, stratum byte, skew time.Duration) (addr string, stop func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64)
		n, peer, err := pc.ReadFrom(buf)
		if err != nil || n < 48 {
			return
		}
		const epoch = 2208988800
		put := func(b []byte, tm time.Time) {
			secs := uint64(tm.Unix() + epoch)
			frac := uint64(tm.Nanosecond()) << 32 / 1e9
			binary.BigEndian.PutUint32(b[0:4], uint32(secs))
			binary.BigEndian.PutUint32(b[4:8], uint32(frac))
		}
		resp := make([]byte, 48)
		resp[0] = 0x24 // LI 0, VN 4, mode 4 (server)
		resp[1] = stratum
		now := time.Now().Add(skew)
		put(resp[32:40], now) // receive
		put(resp[40:48], now) // transmit
		_, _ = pc.WriteTo(resp, peer)
	}()
	return pc.LocalAddr().String(), func() { _ = pc.Close(); <-done }
}

// A source that answers is not a clock. This is the whole point of reporting stratum: an ntpd
// that cannot reach its own upstream keeps replying, at stratum 16, with a plausible time.
func TestSNTPQueryReportsStratum16(t *testing.T) {
	addr, stop := fakeNTP(t, 16, 0)
	defer stop()
	str, _, err := sntpQuery(addr, 2*time.Second)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if str != 16 {
		t.Fatalf("stratum = %d, want 16 — the caller cannot tell an unsynchronised source apart without it", str)
	}
}

// The offset must carry the SIGN of the discrepancy and the right magnitude: a source 5 s ahead
// of us means our clock is 5 s BEHIND, and reporting that backwards would send an operator to
// correct a clock in the wrong direction.
func TestSNTPQueryOffsetSignAndMagnitude(t *testing.T) {
	addr, stop := fakeNTP(t, 2, 5*time.Second)
	defer stop()
	_, off, err := sntpQuery(addr, 2*time.Second)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if off > -4*time.Second || off < -6*time.Second {
		t.Fatalf("offset = %v, want about -5s (we are behind a source that is ahead)", off)
	}
}

func TestSNTPQueryNoAnswer(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close() // nothing is listening now
	if _, _, err := sntpQuery(addr, 300*time.Millisecond); err == nil {
		t.Fatal("want an error when nothing answers; a silent source must never read as a working one")
	}
}

func TestTimeSourcesByMode(t *testing.T) {
	if got := timeSources(nil); got != nil {
		t.Fatalf("unmanaged must declare NO source, got %v", got)
	}
	if got := timeSources(&TimeSync{Mode: "pool"}); len(got) != len(defaultPool) {
		t.Fatalf("pool with no servers must fall back to the public pool, got %v", got)
	}
	got := timeSources(&TimeSync{Mode: "server", Servers: []string{"10.0.0.1"}})
	if len(got) != 1 || got[0] != "10.0.0.1" {
		t.Fatalf("server mode must ask exactly what it was given, got %v", got)
	}
}

// syncTimeRules owns ONLY the rules it tagged. The case that matters is the operator's own pin
// on the same destination: a feature that re-derives its state must not consume a hand-made rule
// that happens to name the same address.
func TestSyncTimeRulesOwnsOnlyItsOwn(t *testing.T) {
	st := &State{
		Uplinks: []Uplink{{Name: "cable", Dev: "eth-doc", Table: 100}},
		Rules: []Rule{
			{Domain: "10.0.0.1", Via: "cable"},                    // the operator's, untagged
			{Domain: "10.0.0.9", Via: "cable", Note: timeRuleTag}, // a stale one of ours
		},
		Time: &TimeSync{Mode: "server", Servers: []string{"10.0.0.1"}, Via: "cable"},
	}
	syncTimeRules(st)

	var tagged, plain int
	for _, r := range st.Rules {
		if r.Note == timeRuleTag {
			tagged++
			if r.Domain != "10.0.0.1" {
				t.Fatalf("stale tagged rule survived: %+v", r)
			}
		} else {
			plain++
		}
	}
	if plain != 1 {
		t.Fatalf("the operator's untagged rule must survive untouched, plain=%d", plain)
	}
	if tagged != 1 {
		t.Fatalf("want exactly one tagged pin for one source, got %d", tagged)
	}

	// No pin declared => no tagged rules at all, and still not the operator's.
	st.Time.Via = ""
	syncTimeRules(st)
	for _, r := range st.Rules {
		if r.Note == timeRuleTag {
			t.Fatalf("a policy with no --via must leave NO pin behind: %+v", r)
		}
	}
	if len(st.Rules) != 1 {
		t.Fatalf("want the operator's single rule left, got %d", len(st.Rules))
	}

	// Unmanaged removes ours and keeps theirs.
	st.Time = nil
	syncTimeRules(st)
	if len(st.Rules) != 1 || st.Rules[0].Note == timeRuleTag {
		t.Fatalf("unmanaged must clear only netgov's pins, got %+v", st.Rules)
	}
}
