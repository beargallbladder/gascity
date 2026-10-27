package main

// The CLI's conversation-lane producers (DES-O1 §7.5): gc session submit's
// local fallback, mail --notify, sling --nudge and the hook-claim
// continuation propose lane message ops when [ops] authoritative lists
// message and a lane host can type into the target session
// (laneServesSession), as gc session nudge does. A target with no session
// row, or one no lane serves (a protocol-mode session without a host
// stamp), takes the producer's legacy path. A proposal that cannot be
// written is refused (cliLaneUnavailable); the producer reports it.
//
// After the Propose the producer rings the controller's lane host
// (lane:<conversation>:<op id>) once, best effort. Only a prompt the caller
// asks it for runs the CLI's content-only inline pass (cliLaneInlinePass);
// every notice waits for the lane host.

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// errCLILaneUnavailable classifies a CLI lane proposal the op journal could
// not record: nothing was written, nothing delivered.
var errCLILaneUnavailable = errors.New("op_journal_unavailable")

// cliLaneProduction is one CLI producer's lane op.
type cliLaneProduction struct {
	target nudgeTarget
	// store is the producer's bead store; the op store opens over its
	// sessions class.
	store beads.Store
	sp    runtime.Provider
	// req is the op without its conversation; an empty ClientKey is minted.
	req opjournal.Request
	// sessionScoped scopes the op to the session's current generation.
	sessionScoped bool
	// renew makes req's key coalesce a notice only while its op is open:
	// one the key names already closed, or an earlier request the key no
	// longer matches (another generation's, KeyReuseError), is proposed
	// again under a minted key, so a repeated event is delivered again.
	renew bool
	// inline runs the content-only inline pass after the Propose.
	inline bool
	// warn takes the inline pass's warnings; nil discards them.
	warn io.Writer
}

// cliLaneProduced is what cliLaneProduce did: handled false when the
// producer takes its legacy path, nothing written; else the op as read
// after the Propose (and the inline pass), whether the Propose created it,
// and the pass's result.
type cliLaneProduced struct {
	handled bool
	op      opjournal.Op
	created bool
	res     opjournal.PassResult
}

// cliLaneProduce proposes p's op on the conversation lane when [ops]
// authoritative lists message, p's target names a session row and a lane
// host can type into the session. It returns handled false, having written
// nothing, otherwise. A proposal it cannot write is an error wrapping
// errCLILaneUnavailable, handled true.
func cliLaneProduce(ctx context.Context, p cliLaneProduction) (cliLaneProduced, error) {
	if !cliLaneExecutesMessage(p.target.cfg) || p.target.sessionID == "" || isNilBeadStore(p.store) {
		return cliLaneProduced{}, nil
	}
	sessions := cliSessionStore(p.store, p.target.cfg, p.target.cityPath)
	unavailable := func(err error) (cliLaneProduced, error) {
		return cliLaneProduced{handled: true}, fmt.Errorf("%w: %s", errCLILaneUnavailable, cliLaneUnavailable(err))
	}
	st, err := opjournal.NewLaneStore(beads.SessionStore{Store: sessions}, cliOpClock)
	if err != nil {
		return unavailable(err)
	}
	view, absent, err := readConversationLaneViewOnly(sessionFrontDoor(sessions), p.target.sessionID)
	switch {
	case err != nil:
		return unavailable(fmt.Errorf("reading session %s: %w", p.target.sessionID, err))
	case absent:
		return cliLaneProduced{}, nil
	}
	serves, err := laneServesSession(p.sp, st, p.target.sessionName, view)
	if err != nil {
		return unavailable(err)
	}
	if !serves {
		return cliLaneProduced{}, nil
	}
	req := p.req
	req.Conversation = p.target.sessionID
	req.Executor = opjournal.ExecutorLane
	req.Kind = opjournal.KindMessage
	if req.ClientKey == "" {
		key, err := legacyop.MintKey()
		if err != nil {
			return unavailable(err)
		}
		req.ClientKey, req.MintedKey = key, true
	}
	if p.sessionScoped {
		req.Scope = opjournal.Scope{Kind: opjournal.ScopeSession, Generation: view.Generation}
	}
	op, created, err := st.Propose(req)
	var reuse *opjournal.KeyReuseError
	if p.renew && !req.MintedKey && (err == nil && !created && op.Status == opjournal.StatusClosed || errors.As(err, &reuse)) {
		key, kerr := legacyop.MintKey()
		if kerr != nil {
			return unavailable(kerr)
		}
		req.ClientKey, req.MintedKey = key, true
		op, created, err = st.Propose(req)
	}
	if err != nil {
		if errors.As(err, &reuse) {
			return cliLaneProduced{handled: true}, fmt.Errorf("the key %q names a different request (op %s)", req.ClientKey, reuse.Existing.ID)
		}
		return unavailable(err)
	}
	out := cliLaneProduced{handled: true, op: op, created: created}
	if p.inline && op.Status != opjournal.StatusClosed {
		warn := p.warn
		if warn == nil {
			warn = io.Discard
		}
		res, passErr := cliLaneInlinePass(ctx, p.target, sessions, p.sp, st, func(format string, args ...any) {
			fmt.Fprintf(warn, "warning: "+format+"\n", args...) //nolint:errcheck // best-effort warning
		})
		out.res = res
		if passErr != nil {
			fmt.Fprintf(warn, "warning: the inline delivery did not run (%v); op %s waits for the controller\n", passErr, op.ID) //nolint:errcheck // best-effort warning
		}
		if cur, err := st.Get(op.ID); err == nil {
			out.op = cur
		}
	}
	if out.op.Status != opjournal.StatusClosed {
		// Best effort: the controller's anti-entropy scan finds the op too.
		_, _ = cliLaneRing(p.target.cityPath, p.target.sessionID, op.ID)
	}
	return out, nil
}

// cliLaneNotice is the lane request of a CLI notice from principal under key
// (minted when empty), caused by causation, delivered at the boundary with
// wake.
func cliLaneNotice(principal, key, causation, text string, wake opjournal.Wake) opjournal.Request {
	return opjournal.Request{
		Kind:      opjournal.KindMessage,
		Executor:  opjournal.ExecutorLane,
		ClientKey: key,
		Producer:  opjournal.Producer{Principal: principal, Actor: opjournal.ActorBackground},
		Payload:   text,
		Causation: causation,
		Policy:    opjournal.Policy{Message: opjournal.MessagePolicy{Role: opjournal.RoleNotice, When: opjournal.WhenBoundary, Wake: wake}},
	}
}

// sessionSubmitLane is one gc session submit's local fallback under lane
// authority.
type sessionSubmitLane struct {
	cityPath  string
	cfg       *config.City
	store     beads.Store
	sessionID string
	target    string
	message   string
	intent    session.SubmitIntent
	// key is the op's client key, --key or minted (minted).
	key    string
	minted bool
	asJSON bool
	stdout io.Writer
	stderr io.Writer
	// sp is the session provider; nil opens the city's (newSessionProvider).
	sp runtime.Provider
}

// submitSessionLane proposes the lane op the API would for s: the operator's
// message under s's key with api.LaneMessageOpPolicy, so it finds an op the
// API proposed before the fallback. A boundary prompt runs the CLI's
// content-only inline pass; a deliver-now (interrupt_now) unit is control,
// which waits for the controller's lane host. It prints delivered or queued
// as gc session nudge does, and reports handled false, having written
// nothing, when no lane serves the session.
func submitSessionLane(s sessionSubmitLane) (code int, handled bool) {
	fail := func(err error) (int, bool) {
		fmt.Fprintf(s.stderr, "gc session submit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	sp := s.sp
	if sp == nil {
		opened, err := newSessionProvider()
		if err != nil {
			return fail(err)
		}
		sp = opened
	}
	info, err := sessionFrontDoor(cliSessionStore(s.store, s.cfg, s.cityPath)).Get(s.sessionID)
	if err != nil {
		return fail(fmt.Errorf("%w: %s", errCLILaneUnavailable, cliLaneUnavailable(fmt.Errorf("reading session %s: %w", s.sessionID, err))))
	}
	prod, err := cliLaneProduce(context.Background(), cliLaneProduction{
		target: resolveNudgeTargetFromSessionInfo(s.cityPath, s.cfg, info),
		store:  s.store,
		sp:     sp,
		req: opjournal.Request{
			ClientKey: s.key,
			MintedKey: s.minted,
			Producer:  opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
			Payload:   s.message,
			Policy:    api.LaneMessageOpPolicy(s.intent),
		},
		inline: s.intent != session.SubmitIntentInterruptNow,
		warn:   prefixWriter{prefix: "gc session submit: ", w: s.stderr},
	})
	if !prod.handled {
		return 0, false
	}
	if err != nil {
		return fail(err)
	}
	op := prod.op
	if !prod.created {
		fmt.Fprintf(s.stderr, "gc session submit: replayed op %s\n", op.ID) //nolint:errcheck // best-effort stderr
	}
	arm, _ := op.Outcome.Effective()
	if op.Status == opjournal.StatusClosed && arm != opjournal.ArmSucceeded {
		return fail(fmt.Errorf("op %s closed %s", op.ID, cliOpOutcomeText(op.Outcome)))
	}
	return emitSessionSubmitResult(s.stdout, s.stderr, s.target, s.intent, op.Status != opjournal.StatusClosed, s.asJSON,
		sessionSubmitOp{id: op.ID, key: s.key, replayed: !prod.created}), true
}

// prefixWriter writes each write to w behind prefix.
type prefixWriter struct {
	prefix string
	w      io.Writer
}

func (p prefixWriter) Write(b []byte) (int, error) {
	if _, err := io.WriteString(p.w, p.prefix); err != nil {
		return 0, err
	}
	return p.w.Write(b)
}
