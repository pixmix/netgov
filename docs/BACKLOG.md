# Backlog

Requested work that is **not** in the current release. One entry per item: what, why, and how the
next person will know it is done. An item without an acceptance test is a wish, not a backlog entry.

---

## Show the host's name — requested 2026-09-13, for the next release

**What.** netgov must say **which machine it is talking about**: the hostname in the dashboard (title
and page heading, so it is visible in a browser tab) and in `netgov status`.

**Why, and it is not cosmetic.** netgov is operated on several boxes at once through SSH-forwarded
dashboards — `localhost:8474` is this host, `:8475`, `:8476`, `:8477` are other machines — and **every
one of those pages is currently identical**. There is nothing on the screen that distinguishes the
production server's switchboard from a laptop's. The tool's whole surface is *"change what this box
does with its network"*, which makes "which box?" the one question the page must never leave to the
reader's memory of which tab is which.

📌 The cost was demonstrated on 2026-09-13 before the feature was asked for: a host's time policy
changed and *whose hand it was* could not be answered (n-841). Provenance was half the answer and
shipped in 2.40 (`by=panel|cli`); **identity is the other half** — a click lands on the box whose tab
you are looking at, and today the page does not tell you which that is.

**Acceptance.**
- `netgov status` names the host on its first line.
- The dashboard carries it in `<title>` (so the browser tab shows it without focusing the window)
  **and** in the page heading.
- It reads the running kernel's hostname at request time — **not** a value stored in state. A
  hostname copied into state is wrong the moment a box is renamed or a state file is restored onto
  another machine, and restoring state onto another machine is a thing the host custodian does.
- Verified by fetching two dashboards through their forwards and seeing two different names.

⚠️ **Do not use it as an identity for anything but display.** Rules, claims and audit lines key on
addresses, interfaces and users; a hostname is a label a human reads, and the moment something keys
on it, renaming a box becomes a config change nobody expects it to be.
