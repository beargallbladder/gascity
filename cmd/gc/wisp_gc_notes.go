package main

import (
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// wispGCRefusalRecheck is how long a refused closure (one holding a bead that
// is not closed) is left alone before the purge walks it again, as long as its
// root has not changed. A root whose updated_at moves is re-walked at once.
// Package var so tests can shrink it.
var wispGCRefusalRecheck = time.Hour

// wispGCNotes is what one wisp GC tracker remembers across passes so a pass
// neither repeats expensive work nor floods the log:
//
//   - refusals: a closure the purge refused is not walked again until its
//     root changes or wispGCRefusalRecheck passes. On a ledger served by bd
//     every walk is a burst of subprocesses, and a refusal re-walked every
//     pass would cost that burst forever while removing nothing;
//   - per-root detail: a pass logs one summary line per arm; the line naming
//     a single root is printed only when that root's state is new since the
//     previous pass (or with GC_DEBUG on), so a stable backlog does not repeat
//     thousands of lines a day.
//
// A nil *wispGCNotes remembers nothing: every refusal is re-walked and every
// detail line printed, which is what a one-off caller wants.
type wispGCNotes struct {
	refused  map[string]wispGCRefusal
	reported map[string]struct{}
	seen     map[string]struct{}
}

type wispGCRefusal struct {
	updatedAt time.Time
	until     time.Time
}

// endPass rolls the per-root detail memo over: what this pass reported is
// what the next pass treats as already said.
func (n *wispGCNotes) endPass() {
	if n == nil {
		return
	}
	n.reported, n.seen = n.seen, nil
}

// detail reports whether the per-root line keyed by key should be printed:
// always under GC_DEBUG, else only when the previous pass did not print it.
func (n *wispGCNotes) detail(key string) bool {
	if n == nil {
		return true
	}
	if n.seen == nil {
		n.seen = make(map[string]struct{})
	}
	n.seen[key] = struct{}{}
	if gcDebugEnabled() {
		return true
	}
	_, said := n.reported[key]
	return !said
}

// refusedNow reports whether root's closure was refused recently enough, and
// root is unchanged since, that the purge should not walk it this pass.
func (n *wispGCNotes) refusedNow(arm string, root beads.Bead, now time.Time) bool {
	if n == nil {
		return false
	}
	r, ok := n.refused[arm+"\x00"+root.ID]
	return ok && r.updatedAt.Equal(root.UpdatedAt) && now.Before(r.until)
}

// noteRefusal records that root's closure was refused at now.
func (n *wispGCNotes) noteRefusal(arm string, root beads.Bead, now time.Time) {
	if n == nil {
		return
	}
	if n.refused == nil {
		n.refused = make(map[string]wispGCRefusal)
	}
	n.refused[arm+"\x00"+root.ID] = wispGCRefusal{updatedAt: root.UpdatedAt, until: now.Add(wispGCRefusalRecheck)}
}

// forgetRefusal drops root's refusal, after its closure was purged.
func (n *wispGCNotes) forgetRefusal(arm, rootID string) {
	if n == nil {
		return
	}
	delete(n.refused, arm+"\x00"+rootID)
}
