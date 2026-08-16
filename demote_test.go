package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The defect this file guards (c-001, ms-rosy 2026-08-16): the arbiter rejected a leg measuring
// 70-100% frame loss, and that leg kept the preferred ipv4.route-metric — so the box held .186 on a
// healthy adapter while sourcing its traffic from the broken one. Every assertion here is about the
// metric FOLLOWING THE VERDICT rather than the declared priority.

func verdict(winner string, eligible map[string]bool) claimVerdict {
	return claimVerdict{Winner: winner, Eligible: eligible}
}

// The core of the fix: a demoted leg must rank below an eligible one EVEN THOUGH its declared
// priority is higher. Testing the sort directly, because the sort IS the fix.
func TestRankClaimants_DemotedFallsBelowEligible(t *testing.T) {
	cs := []Claimant{{Dev: "eno1", Priority: 100}, {Dev: "wlp0s20f3", Priority: 50}}

	got := rankClaimants(cs, nil)
	if got[0].Dev != "eno1" {
		t.Fatalf("with nothing demoted the declared priority must win; got %s first", got[0].Dev)
	}

	got = rankClaimants(cs, map[string]bool{"eno1": true})
	if got[0].Dev != "wlp0s20f3" {
		t.Fatalf("a demoted prio-100 leg must rank below an eligible prio-50 leg; got %s first", got[0].Dev)
	}
	// And the resulting metrics must actually differ, or the reordering changes nothing on the box.
	if m0, m1 := nmMetricBase, nmMetricBase+nmMetricStep; m0 >= m1 {
		t.Fatalf("rank 0 metric %d must be better than rank 1 metric %d", m0, m1)
	}
}

// All-demoted must not shuffle: with no healthy leg to prefer, churn is the only possible outcome.
func TestRankClaimants_AllDemotedKeepsPriorityOrder(t *testing.T) {
	cs := []Claimant{{Dev: "eno1", Priority: 100}, {Dev: "wlp0s20f3", Priority: 50}}
	got := rankClaimants(cs, map[string]bool{"eno1": true, "wlp0s20f3": true})
	if got[0].Dev != "eno1" {
		t.Fatalf("when every leg is demoted the declared order must stand; got %s first", got[0].Dev)
	}
}

// SLOW TO PUNISH: one bad verdict must not move a metric, or a single transient probe failure
// re-ranks the box's egress. The streak is what keeps this from fighting NetworkManager every tick.
func TestNextDemotions_RequiresAStreakBeforeDemoting(t *testing.T) {
	cs := []Claimant{{Dev: "eno1", Priority: 100}, {Dev: "wlp0s20f3", Priority: 50}}
	v := verdict("wlp0s20f3", map[string]bool{"eno1": false, "wlp0s20f3": true})

	first := nextDemotions(map[string]int{}, v, cs)
	if first["eno1"] >= claimDemoteStreak {
		t.Fatalf("one failure must not reach the demotion threshold; got streak %d", first["eno1"])
	}
	second := nextDemotions(first, v, cs)
	if second["eno1"] < claimDemoteStreak {
		t.Fatalf("consecutive failures must reach the threshold; got streak %d", second["eno1"])
	}
	if second["wlp0s20f3"] != 0 {
		t.Fatalf("an eligible leg must carry no streak; got %d", second["wlp0s20f3"])
	}
}

// FAST TO FORGIVE, and asymmetric on purpose: a demotion that outlives the fault is a self-inflicted
// `HOLDS is not PATH` — the very thing this feature exists to prevent, with the sign flipped.
func TestNextDemotions_OneGoodVerdictClearsTheStreak(t *testing.T) {
	cs := []Claimant{{Dev: "eno1", Priority: 100}}
	recovered := nextDemotions(map[string]int{"eno1": 9},
		verdict("eno1", map[string]bool{"eno1": true}), cs)
	if recovered["eno1"] != 0 {
		t.Fatalf("recovery must clear the streak immediately; got %d", recovered["eno1"])
	}
}

// NOBODY ELIGIBLE => DEMOTE NOBODY. With no healthy leg to move the path toward, re-ranking cannot
// improve anything and only churns NM. Same reasoning as claim-before-release: when there is no
// winner, the correct action is to leave everything exactly as it is.
func TestNextDemotions_NoWinnerDemotesNobody(t *testing.T) {
	cs := []Claimant{{Dev: "eno1", Priority: 100}, {Dev: "wlp0s20f3", Priority: 50}}
	got := nextDemotions(map[string]int{"eno1": 5},
		verdict("", map[string]bool{"eno1": false, "wlp0s20f3": false}), cs)
	if len(got) != 0 {
		t.Fatalf("no eligible claimant must produce no demotions; got %v", got)
	}
}

// A SWALLOWED WRITE LEAVES NOTHING TO MEASURE. /run is root-owned and the 2026-08-13 cooldown bug was
// exactly this: an unprivileged process wrote nothing, the cooldown silently did not exist, and only
// a unit test could ever have found it. So recordDemotions must RETURN the error, and the round trip
// must actually round-trip.
func TestRecordDemotions_WriteIsAssertedNotSwallowed(t *testing.T) {
	dir := t.TempDir()
	orig := claimDemoteFile
	defer func() { claimDemoteFile = orig }()

	claimDemoteFile = filepath.Join(dir, "demoted")
	if err := recordDemotions(map[string]int{"eno1": 2}); err != nil {
		t.Fatalf("write to a writable path must succeed: %v", err)
	}
	if got := demotedDevs(); !got["eno1"] {
		t.Fatalf("a recorded streak at the threshold must read back as demoted; got %v", got)
	}

	claimDemoteFile = filepath.Join(dir, "no-such-dir", "demoted")
	if err := recordDemotions(map[string]int{"eno1": 2}); err == nil {
		t.Fatal("an unwritable path must return an error, not be swallowed — " +
			"a silent failure here leaves the metric following the config while the log says otherwise")
	}
}

// STALE OR ABSENT => NO DEMOTION. The record is runtime state, not a saved baseline: every failure
// mode must degrade to the pre-2.33 behaviour. This is the property that keeps it out of the 2.28
// class, where a forgotten record applied a confident wrong value.
func TestDemotedDevs_StaleOrAbsentMeansNoDemotion(t *testing.T) {
	dir := t.TempDir()
	orig := claimDemoteFile
	defer func() { claimDemoteFile = orig }()

	claimDemoteFile = filepath.Join(dir, "absent")
	if got := demotedDevs(); len(got) != 0 {
		t.Fatalf("an absent record must demote nobody; got %v", got)
	}

	stale := strconv.FormatInt(time.Now().Unix()-claimDemoteTTL-1, 10) + " eno1:9\n"
	claimDemoteFile = filepath.Join(dir, "stale")
	if err := os.WriteFile(claimDemoteFile, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := demotedDevs(); len(got) != 0 {
		t.Fatalf("a verdict older than the TTL is not evidence about now; got %v", got)
	}

	claimDemoteFile = filepath.Join(dir, "garbage")
	if err := os.WriteFile(claimDemoteFile, []byte("not-a-timestamp eno1:9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := demotedDevs(); len(got) != 0 {
		t.Fatalf("an unparseable record must demote nobody; got %v", got)
	}
}

// A streak BELOW the threshold must not demote — the file records progress toward a decision, not
// the decision itself. Guards against a future edit that treats "present in the file" as "demoted".
func TestDemotedDevs_BelowThresholdIsNotDemoted(t *testing.T) {
	dir := t.TempDir()
	orig := claimDemoteFile
	defer func() { claimDemoteFile = orig }()

	claimDemoteFile = filepath.Join(dir, "demoted")
	if err := recordDemotions(map[string]int{"eno1": claimDemoteStreak - 1}); err != nil {
		t.Fatal(err)
	}
	if got := demotedDevs(); got["eno1"] {
		t.Fatalf("a streak below the threshold must not demote; got %v", got)
	}
}

// The written form must be deterministic, or an unchanged verdict rewrites the file with different
// bytes every tick — noise that makes a real change impossible to spot in a log.
func TestRecordDemotions_IsDeterministic(t *testing.T) {
	dir := t.TempDir()
	orig := claimDemoteFile
	defer func() { claimDemoteFile = orig }()
	claimDemoteFile = filepath.Join(dir, "demoted")

	read := func() string {
		b, err := os.ReadFile(claimDemoteFile)
		if err != nil {
			t.Fatal(err)
		}
		// Drop the leading timestamp; only the dev:streak part must be stable.
		_, rest, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
		return rest
	}
	m := map[string]int{"wlp0s20f3": 3, "eno1": 2, "enx0024": 1}
	if err := recordDemotions(m); err != nil {
		t.Fatal(err)
	}
	first := read()
	if err := recordDemotions(m); err != nil {
		t.Fatal(err)
	}
	if second := read(); first != second {
		t.Fatalf("same input must produce identical bytes; %q vs %q", first, second)
	}
}
