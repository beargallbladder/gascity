package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/session"
)

// The CLI's lane producers (design slice 7): gc session submit's local
// fallback, mail --notify, sling --nudge and the hook-claim continuation.

// laneOpByKey reads the op principal's key names on the fixture's session.
func (c *cliLaneCity) laneOpByKey(t *testing.T, principal, key string) opjournal.Op {
	t.Helper()
	st := c.laneStore(t)
	id, err := opjournal.DeriveID(st.Prefix(), c.id, principal, key)
	if err != nil {
		t.Fatal(err)
	}
	return c.get(t, st, id)
}

// assertLaneNotice checks op is a lane notice from principal, of policy
// wake, whose delivered text is golden and names no identifier.
func assertLaneNotice(t *testing.T, op opjournal.Op, principal string, wake opjournal.Wake, golden string) {
	t.Helper()
	m := op.Policy.Message
	if op.Executor != opjournal.ExecutorLane || op.Producer.Principal != principal || m.Role != opjournal.RoleNotice ||
		m.When != opjournal.WhenBoundary || m.Wake != wake {
		t.Fatalf("op = %+v, want a lane notice from %s waking %s", op, principal, wake)
	}
	text, err := opjournal.RenderDelivery([]opjournal.Op{op}, opjournal.TextReminder, false)
	if err != nil {
		t.Fatal(err)
	}
	if text != golden {
		t.Fatalf("delivered text = %q, want %q", text, golden)
	}
	for _, id := range []string{op.ID, op.ClientKey, op.Conversation, op.Causation} {
		if id != "" && strings.Contains(text, id) {
			t.Fatalf("delivered text %q carries identifier %q", text, id)
		}
	}
}

// reminderGolden is the delivered text of one notice labeled label.
func reminderGolden(label, text string) string {
	return "<system-reminder>\nYou have a deferred reminder that was queued until a safe boundary:\n\n- [" + label + "] " + text + "\n\nHandle them after this turn.\n</system-reminder>\n"
}

func (c *cliLaneCity) submitLane(t *testing.T, intent session.SubmitIntent, key string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, handled := submitSessionLane(sessionSubmitLane{
		cityPath: c.dir, cfg: c.cfg, store: c.store, sessionID: c.id, target: c.target.agentKey(),
		message: cliLaneTestMessage, intent: intent, key: key,
		stdout: &stdout, stderr: &stderr, sp: c.sp,
	})
	if !handled {
		t.Fatal("submitSessionLane did not handle a session a lane serves")
	}
	return code, stdout.String(), stderr.String()
}

// The fallback proposes the op the API would: the operator's message under
// the request's key with api.LaneMessageOpPolicy, so its op id is the one the
// API derives, and a boundary prompt is delivered by the inline pass.
func TestSessionSubmitFallbackProposesTheAPIsLaneOp(t *testing.T) {
	c := newCLILaneCity(t)
	code, stdout, stderr := c.submitLane(t, session.SubmitIntentDefault, "submit-1")
	if code != 0 || stdout != "Submitted to "+c.target.agentKey()+"\n" {
		t.Fatalf("submit = %d, %q; stderr %s", code, stdout, stderr)
	}
	op := c.laneOpByKey(t, "operator", "submit-1")
	want := api.LaneMessageOpPolicy(session.SubmitIntentDefault)
	m := op.Policy.Message
	if op.Executor != opjournal.ExecutorLane || m.When != want.Message.When || m.Wake != want.Message.Wake || m.Role != want.Message.Role || op.Payload != cliLaneTestMessage ||
		op.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = %+v, want the API's lane op delivered", op)
	}
	if n := len(c.nudges()); n != 1 {
		t.Fatalf("deliveries = %d, want the inline pass's one", n)
	}
	// The same request again replays the op.
	code, stdout, stderr = c.submitLane(t, session.SubmitIntentDefault, "submit-1")
	if code != 0 || !strings.Contains(stderr, "replayed op "+op.ID) || len(c.nudges()) != 1 {
		t.Fatalf("replay = %d, %q, %q; deliveries %d", code, stdout, stderr, len(c.nudges()))
	}
}

// interrupt_now is a deliver-now unit, which is control: the CLI's pass
// leaves it to the controller's lane host, interrupting nothing.
func TestSessionSubmitFallbackInterruptNowWaitsForTheController(t *testing.T) {
	c := newCLILaneCity(t)
	c.sp.Calls = nil
	code, stdout, stderr := c.submitLane(t, session.SubmitIntentInterruptNow, "submit-now")
	if code != 0 || stdout != "Queued follow-up for "+c.target.agentKey()+"\n" {
		t.Fatalf("submit = %d, %q; stderr %s", code, stdout, stderr)
	}
	if calls := c.controlCalls(); len(calls) != 0 || len(c.nudges()) != 0 {
		t.Fatalf("the CLI interrupted or delivered: %+v / %+v", calls, c.nudges())
	}
	op := c.laneOpByKey(t, "operator", "submit-now")
	if op.Status == opjournal.StatusClosed || op.Policy.Message.When != opjournal.WhenNow {
		t.Fatalf("op = %+v, want an open deliver-now message", op)
	}
}

func TestMailNotifyUnderLaneProposesANotice(t *testing.T) {
	c := newCLILaneCity(t)
	id, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", "msg-1")
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	op := c.laneOpByKey(t, mailNoticePrincipal, "mail:msg-1")
	if id != op.ID || op.Causation != "msg-1" || op.Status == opjournal.StatusClosed {
		t.Fatalf("op %s = %+v, want an open notice caused by msg-1", id, op)
	}
	assertLaneNotice(t, op, mailNoticePrincipal, opjournal.WakeIfNeeded, reminderGolden("mail", "You have mail from alice"))
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
	if len(c.rings) != 1 || c.rings[0] != c.id+":"+op.ID {
		t.Fatalf("rings = %v, want one for the notice", c.rings)
	}
}

// Reading or archiving a message withdraws its open lane notices.
func TestMailReadCancelsItsNotice(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(p mail.Provider, id string) error
	}{
		{"read", func(p mail.Provider, id string) error { _, err := p.Read(id); return err }},
		{"mark read", func(p mail.Provider, id string) error { return p.MarkRead(id) }},
		{"archive", func(p mail.Provider, id string) error { return p.Archive(id) }},
		{"archive many", func(p mail.Provider, id string) error { _, err := p.ArchiveMany([]string{id}); return err }},
		{"delete", func(p mail.Provider, id string) error { return p.Delete(id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			inner := mail.NewFake()
			msg, err := inner.Send("alice", c.target.agentKey(), "hi", "body")
			if err != nil {
				t.Fatal(err)
			}
			opID, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", msg.ID)
			if err != nil {
				t.Fatal(err)
			}
			var warn bytes.Buffer
			p := withMailNoticeWithdrawal(inner, c.store, c.cfg, &warn)
			if err := tc.do(p, msg.ID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			op := c.get(t, c.laneStore(t), opID)
			if op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmCanceled {
				t.Fatalf("notice after %s = %s %+v, want it canceled; warnings %s", tc.name, op.Status, op.Outcome, warn.String())
			}
		})
	}
	// Without message listed the provider is not wrapped.
	c := newCLILaneCity(t)
	c.cfg.Ops.Authoritative = nil
	inner := mail.NewFake()
	if p := withMailNoticeWithdrawal(inner, c.store, c.cfg, nil); p != mail.Provider(inner) {
		t.Fatalf("provider wrapped with message unlisted: %T", p)
	}
}

// The city's bead mail provider keeps every capability its readers ask for
// (the session inbox, the handoff archive) under the withdrawal, and each of
// its reads and archives withdraws the message's open lane notices.
func TestBeadMailReadCancelsItsNotice(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(p mail.Provider, id string) error
	}{
		{"read", func(p mail.Provider, id string) error { _, err := p.Read(id); return err }},
		{"mark read", func(p mail.Provider, id string) error { return p.MarkRead(id) }},
		{"archive", func(p mail.Provider, id string) error { return p.Archive(id) }},
		{"archive many", func(p mail.Provider, id string) error { _, err := p.ArchiveMany([]string{id}); return err }},
		{"archive matching", func(p mail.Provider, _ string) error {
			_, _, err := p.(*beadmail.Provider).ArchiveMatching(beadmail.ArchiveFilter{From: "alice", IncludeRead: true})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			inner := beadmail.New(c.store)
			msg, err := inner.Send("alice", c.target.agentKey(), "hi", "body")
			if err != nil {
				t.Fatal(err)
			}
			opID, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", msg.ID)
			if err != nil {
				t.Fatal(err)
			}
			var warn bytes.Buffer
			p := withMailNoticeWithdrawal(inner, c.store, c.cfg, &warn)
			if _, reads := p.(beadmail.SessionInboxReader); !reads {
				t.Fatalf("the withdrawal hides beadmail.SessionInboxReader (%T)", p)
			}
			if _, archives := p.(inboxAutoHandoffArchiver); !archives {
				t.Fatalf("the withdrawal hides the handoff archive (%T)", p)
			}
			if err := tc.do(p, msg.ID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			op := c.get(t, c.laneStore(t), opID)
			if op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmCanceled {
				t.Fatalf("notice after %s = %s %+v, want it canceled; warnings %s", tc.name, op.Status, op.Outcome, warn.String())
			}
		})
	}
}

// With message listed, an injected auto-handoff is still archived after its
// injection, so the next prompt's mail check does not inject it again.
func TestMailNoticeWithdrawalKeepsTheHandoffArchive(t *testing.T) {
	for _, listed := range []bool{false, true} {
		store := beads.NewMemStore()
		cfg := &config.City{}
		if listed {
			cfg.Ops.Authoritative = []string{"message"}
		}
		mp := withMailNoticeWithdrawal(beadmail.New(store), store, cfg, io.Discard)
		auto, err := store.Create(beads.Bead{
			Title: "context cycle", Type: "message", Assignee: "mayor", From: "mayor",
			Labels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel},
		})
		if err != nil {
			t.Fatal(err)
		}
		var first, second bytes.Buffer
		if code := doMailCheck(mp, "mayor", true, &first, io.Discard); code != 0 {
			t.Fatalf("message listed %v: first mail check = %d", listed, code)
		}
		if !bytes.Contains(first.Bytes(), []byte(auto.ID)) {
			t.Fatalf("message listed %v: the handoff %s was not injected: %s", listed, auto.ID, first.String())
		}
		_ = doMailCheck(mp, "mayor", true, &second, io.Discard)
		if b, _ := store.Get(auto.ID); b.Status != "closed" || bytes.Contains(second.Bytes(), []byte(auto.ID)) {
			t.Fatalf("message listed %v: handoff %s is %s after its injection and injected again: %v", listed, auto.ID, b.Status, bytes.Contains(second.Bytes(), []byte(auto.ID)))
		}
	}
}

func TestSlingNudgeUnderLaneProposesANotice(t *testing.T) {
	c := newCLILaneCity(t)
	var stdout, stderr bytes.Buffer
	deliverSlingNudge(c.target, c.sp, c.store, c.dir, "gc-bead-1", &stdout, &stderr)
	if !strings.Contains(stdout.String(), "in the conversation lane") || stderr.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	op := c.laneOpByKey(t, slingNoticePrincipal, "sling:gc-bead-1")
	assertLaneNotice(t, op, slingNoticePrincipal, opjournal.WakeIfNeeded, reminderGolden("sling", slingNudgeText))
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
}

// A second gc sling --nudge of the same bead to the same session, after the
// first notice closed, proposes a new notice under a minted key rather than
// finding the closed one and reporting a nudge it never queued; while the
// first is still open the second coalesces into it.
func TestSlingNudgeUnderLaneReslingAfterTheNoticeClosed(t *testing.T) {
	c := newCLILaneCity(t)
	var stdout, stderr bytes.Buffer
	deliverSlingNudge(c.target, c.sp, c.store, c.dir, "gc-bead-1", &stdout, &stderr)
	first := c.laneOpByKey(t, slingNoticePrincipal, "sling:gc-bead-1")
	stdout.Reset()
	deliverSlingNudge(c.target, c.sp, c.store, c.dir, "gc-bead-1", &stdout, &stderr)
	if n := len(c.ops(t)); n != 1 {
		t.Fatalf("re-sling with the notice open wrote %d ops, want the one it coalesces into", n)
	}
	if _, err := c.laneStore(t).Close(first.ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:test"); err != nil {
		t.Fatalf("close: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	deliverSlingNudge(c.target, c.sp, c.store, c.dir, "gc-bead-1", &stdout, &stderr)
	if !strings.Contains(stdout.String(), "in the conversation lane") || stderr.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	var open []opjournal.Op
	for _, op := range c.ops(t) {
		if op.Status != opjournal.StatusClosed {
			open = append(open, op)
		}
	}
	if len(open) != 1 || open[0].ID == first.ID || open[0].ClientKey == first.ClientKey || open[0].Causation != "gc-bead-1" {
		t.Fatalf("open ops after the re-sling = %+v, want one new notice caused by the bead", open)
	}
	assertLaneNotice(t, open[0], slingNoticePrincipal, opjournal.WakeIfNeeded, reminderGolden("sling", slingNudgeText))
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
}

// The hook-claim continuation of the same root again, in a later generation
// of the session (the generation is in the request's digest, not its key) or
// after the first notice closed, is a new lane notice under a minted key,
// never the legacy queue.
func TestHookClaimContinuationUnderLaneRenews(t *testing.T) {
	for _, nextGeneration := range []bool{false, true} {
		c := newCLILaneCity(t)
		if !continuationLaneNotice(c.target, c.store, c.sp, "gc-root-1") {
			t.Fatal("first continuation did not propose")
		}
		first := c.laneOpByKey(t, hookClaimContinuationPrincipal, "hook-claim:gc-root-1")
		if _, err := c.laneStore(t).Close(first.ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:test"); err != nil {
			t.Fatalf("close: %v", err)
		}
		target := c.target
		if nextGeneration {
			if err := c.store.SetMetadata(c.id, "generation", "2"); err != nil {
				t.Fatalf("bump generation: %v", err)
			}
			target = resolveNudgeTargetFromSessionInfo(c.dir, c.cfg, c.rowInfo(t))
		}
		if !continuationLaneNotice(target, c.store, c.sp, "gc-root-1") {
			t.Fatalf("next generation %v: the continuation was not proposed on the lane", nextGeneration)
		}
		var open []opjournal.Op
		for _, op := range c.ops(t) {
			if op.Status != opjournal.StatusClosed {
				open = append(open, op)
			}
		}
		wantGen := int64(1)
		if nextGeneration {
			wantGen = 2
		}
		if len(open) != 1 || open[0].ID == first.ID || open[0].Causation != "gc-root-1" || open[0].Scope.Generation != wantGen {
			t.Fatalf("next generation %v: open ops = %+v, want one new notice for generation %d", nextGeneration, open, wantGen)
		}
		if c.queuedItems(t) != 0 {
			t.Fatalf("next generation %v: the continuation was queued on the legacy queue", nextGeneration)
		}
	}
}

func TestHookClaimContinuationUnderLaneProposesANotice(t *testing.T) {
	c := newCLILaneCity(t)
	if !continuationLaneNotice(c.target, c.store, c.sp, "gc-root-1") {
		t.Fatal("continuationLaneNotice did not propose")
	}
	op := c.laneOpByKey(t, hookClaimContinuationPrincipal, "hook-claim:gc-root-1")
	if op.Causation != "gc-root-1" || op.Scope.Kind != opjournal.ScopeSession || op.Scope.Generation != 1 {
		t.Fatalf("op = %+v, want caused by the root and scoped to the generation", op)
	}
	assertLaneNotice(t, op, hookClaimContinuationPrincipal, opjournal.WakeLiveOnly, reminderGolden("hook-claim-continuation", hookClaimContinuationText))
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
}

// With message unlisted every CLI producer keeps its legacy path and writes
// no lane op.
func TestCLIProducersWithoutLaneKeepLegacy(t *testing.T) {
	c := newCLILaneCity(t)
	c.cfg.Ops.Authoritative = nil
	c.target.cfg = c.cfg
	if prod, err := cliLaneProduce(context.Background(), cliLaneProduction{
		target: c.target, store: c.store, sp: c.sp,
		req: cliLaneNotice(mailNoticePrincipal, "mail:m", "m", "x", opjournal.WakeIfNeeded),
	}); prod.handled || err != nil {
		t.Fatalf("cliLaneProduce with message unlisted = %+v, %v; want unhandled", prod, err)
	}
	var stdout, stderr bytes.Buffer
	deliverSlingNudge(c.target, c.sp, c.store, c.dir, "gc-bead-1", &stdout, &stderr)
	if strings.Contains(stdout.String(), "conversation lane") {
		t.Fatalf("sling took the lane: %q", stdout.String())
	}
	if continuationLaneNotice(c.target, c.store, c.sp, "gc-root-1") {
		t.Fatal("the continuation took the lane")
	}
	for _, op := range c.ops(t) {
		if op.Executor == opjournal.ExecutorLane {
			t.Fatalf("lane op %+v written with message unlisted", op)
		}
	}
}
