package main

import "strings"
import "testing"

// The route TYPE is the error message every downstream tool prints. Measured in a namespace:
// blackhole -> EINVAL "Invalid argument" (names nothing), unreachable -> EHOSTUNREACH
// "No route to host" (names the routing layer). A blocked connection must say which layer
// refused it, or the failure is investigated three layers away — a git push once was.
func TestBlockTableIsUnreachableNotBlackhole(t *testing.T) {
	for _, fam := range []string{"4", "6"} {
		st := &State{}
		if fam == "4" {
			st.DefaultV4 = "block"
		} else {
			st.DefaultV6 = "block"
		}
		var found, bad bool
		_, build := planFamily(st, fam)
		for _, c := range build {
			j := strings.Join(c, " ")
			if strings.Contains(j, "table 199") && strings.Contains(j, "unreachable") {
				found = true
			}
			if strings.Contains(j, "blackhole") {
				bad = true
			}
		}
		if !found {
			t.Errorf("fam %s: no unreachable default in the block table", fam)
		}
		if bad {
			t.Errorf("fam %s: blackhole still installed — it reports EINVAL, which names nothing", fam)
		}
	}
}
