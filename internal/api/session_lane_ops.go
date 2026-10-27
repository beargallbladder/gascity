package api

// Session commands under lane authority (DES-O1 §7.5, §10.1): when the
// city's running conversation lane host executes a kind ([ops] authoritative
// listed it at the controller's start), the request surface that produces
// that kind proposes a lane op and answers from its outcome instead of
// running the legacy path inline. The ops.* gates do not govern a listed
// kind: a request whose op cannot be written is refused, 503
// op_journal_unavailable, nothing done.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// laneControlAnswerBound is how long a control command under lane authority
// awaits its op's outcome before it answers in_flight: under the generated
// Go client's 60 s timeout, and above an interrupt's settle. A var so tests
// can shorten it.
var laneControlAnswerBound = 45 * time.Second

// laneAwaitPoll is how often awaitLaneOp re-reads an op when the State's
// lane host offers no pass-end wakeup.
const laneAwaitPoll = 250 * time.Millisecond

// laneExecutes reports whether the city's running lane host executes kind.
func (s *Server) laneExecutes(kind opjournal.Kind) bool {
	return slices.Contains(s.conversationLaneKinds(), string(kind))
}

// proposeLaneOp proposes req, pokes the lane when it created the op, and maps
// a refusal onto the wire (proposeError).
func (s *Server) proposeLaneOp(st *opjournal.Store, req opjournal.Request) (opjournal.Op, bool, error) {
	op, created, err := st.Propose(req)
	if err != nil {
		return opjournal.Op{}, false, proposeError(err)
	}
	if created {
		s.pokeConversationLane(req.Conversation)
	}
	return op, created, nil
}

// awaitLaneOp waits, at most bound, for op id of conv to close, and returns
// it as last read: closed, or still open at the bound. It waits through the
// lane host's AwaitConversationOp, which wakes at the end of each pass over
// conv; it polls the op store when the State offers no lane host. A read
// that fails is 503 op_journal_unavailable.
func (s *Server) awaitLaneOp(ctx context.Context, st *opjournal.Store, conv, id string, bound time.Duration) (opjournal.Op, error) {
	actx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	if h, ok := s.state.(ConversationLaneHost); ok {
		op, err := h.AwaitConversationOp(actx, conv, id)
		if err != nil {
			return op, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: awaiting op %s: %v", id, err)
		}
		return op, nil
	}
	for {
		op, err := st.Get(id)
		if err != nil {
			return op, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: reading op %s: %v", id, err)
		}
		if op.Status == opjournal.StatusClosed {
			return op, nil
		}
		t := time.NewTimer(laneAwaitPoll)
		select {
		case <-actx.Done():
			t.Stop()
			return op, nil
		case <-t.C:
		}
	}
}

// keyedLaneOp reads the op the operator's client key names on conv, kind
// kind: found false when key is empty or names no op. A row of another kind
// is idempotency_mismatch, and a read that fails 503
// op_journal_unavailable.
func keyedLaneOp(st *opjournal.Store, kind opjournal.Kind, conv, key string) (opjournal.Op, bool, error) {
	if key == "" {
		return opjournal.Op{}, false, nil
	}
	id, err := opjournal.DeriveID(st.Prefix(), conv, legacyop.PrincipalOperator, key)
	if err != nil {
		return opjournal.Op{}, false, apierr.InvalidIdempotencyKey.Msg("invalid_idempotency_key: " + err.Error())
	}
	op, err := st.Get(id)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return opjournal.Op{}, false, nil
	case err != nil:
		return opjournal.Op{}, false, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: reading op %s: %v", id, err)
	case op.Kind != kind:
		return opjournal.Op{}, false, apierr.IdempotencyMismatch.Msgf("idempotency_mismatch: Idempotency-Key reused with a different request: it names a %s op", op.Kind)
	}
	return op, true, nil
}

// followCoalesced is the op op answers through: the op it coalesced into
// (the row a named stop leaves under its own key), else op itself.
func followCoalesced(st *opjournal.Store, op opjournal.Op) (opjournal.Op, error) {
	into := op.Outcome.Result.CoalescedInto
	if op.Status != opjournal.StatusClosed || into == "" {
		return op, nil
	}
	target, err := st.Get(into)
	if err != nil {
		return opjournal.Op{}, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: reading op %s, which op %s coalesced into, so nothing was done: %v", into, op.ID, err)
	}
	return target, nil
}

// laneOpInFlight is the answer of a command whose lane op is still open at
// laneControlAnswerBound: 409 in_flight naming the op and its key, under
// which a retry adopts it.
func laneOpInFlight(op opjournal.Op) error {
	return apierr.IdempotencyInFlight.Msgf("in_flight: op %s is still running; retry with Idempotency-Key %q to await it", op.ID, op.ClientKey)
}

// --- /stop and run cancel ---

// stopSessionLane is /stop under lane authority (interrupt listed): it
// proposes an interrupt op the lane host runs, awaits its outcome at most
// laneControlAnswerBound, and answers as OJ-4 answers that outcome. key is
// the Idempotency-Key header and named the turn the request names, "" for
// none.
//
// A key that already names a stop is that stop's retry: an open lane op is
// awaited, a closed op replayed, and the row a named stop left under its
// key followed to the turn's op. A named turn keeps OJ-4's front-door
// checks (a refusal writes nothing), is keyed stop:<generation>:<turn> and
// scoped to that turn, and leaves a row under the request's own key that
// coalesced into it; a target-keyed op that closed without success is
// retried under the request's key. Without a named turn the op is
// session(generation)-scoped under the request's key, or a minted one, and
// the executor reads the turn itself.
func (s *Server) stopSessionLane(ctx context.Context, store beads.Store, id, key, named string) (syncAnswer, error) {
	st, err := s.conversationOpStore()
	if err != nil {
		return syncAnswer{}, err
	}
	prior, retry, err := keyedLaneOp(st, opjournal.KindInterrupt, id, key)
	if err != nil {
		return syncAnswer{}, err
	}
	if retry {
		if named != "" && prior.Scope.Turn != "" && prior.Scope.Turn != named {
			return syncAnswer{}, apierr.IdempotencyMismatch.Msg(fmt.Sprintf("idempotency_mismatch: Idempotency-Key reused with a different request: it names a stop of turn %q, not turn %q", prior.Scope.Turn, named))
		}
		target, err := followCoalesced(st, prior)
		if err != nil {
			return syncAnswer{}, err
		}
		return s.answerLaneStop(ctx, st, id, target, target.Scope.Turn != "", true)
	}
	if named != "" {
		if _, err := s.namedStopPlan(ctx, store, id, named); err != nil {
			return syncAnswer{}, err
		}
	}
	scope, why, whyErr := sessionOpScope(store, id)
	if why != "" {
		cause := why
		if whyErr != nil {
			cause += ": " + whyErr.Error()
		}
		return syncAnswer{}, apierr.OpJournalUnavailable.Msg(fmt.Sprintf("op_journal_unavailable: the operation journal cannot record this stop (%s); nothing was done", cause))
	}
	clientKey, minted := key, false
	if clientKey == "" {
		if clientKey, err = legacyop.MintKey(); err != nil {
			return syncAnswer{}, apierr.Internal.Msg(err.Error())
		}
		minted = true
	}
	req := opjournal.Request{
		Conversation: id,
		Kind:         opjournal.KindInterrupt,
		Executor:     opjournal.ExecutorLane,
		ClientKey:    clientKey,
		MintedKey:    minted,
		Producer:     opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Policy:       interruptOpPolicy,
		Scope:        scope,
	}
	if named == "" {
		op, created, err := s.proposeLaneOp(st, req)
		if err != nil {
			return syncAnswer{}, err
		}
		return s.answerLaneStop(ctx, st, id, op, false, !created)
	}
	req.Scope.Turn = named
	targetKey, err := opjournal.InterruptKey(scope.Generation, named)
	if err != nil {
		return syncAnswer{}, apierr.ValidationFailed.Msg("invalid_turn: " + err.Error())
	}
	targeted := req
	targeted.ClientKey, targeted.MintedKey = targetKey, false
	op, created, err := s.proposeLaneOp(st, targeted)
	if err != nil {
		return syncAnswer{}, err
	}
	if op.Status == opjournal.StatusClosed && !laneStopSucceeded(op) {
		// A failed stop of the turn never uses it up: this request retries
		// it under its own key.
		if op, created, err = s.proposeLaneOp(st, req); err != nil {
			return syncAnswer{}, err
		}
		return s.answerLaneStop(ctx, st, id, op, true, !created)
	}
	if key != "" {
		if err := recordLaneStopUnderKey(st, req, op.ID); err != nil {
			return syncAnswer{}, err
		}
	}
	return s.answerLaneStop(ctx, st, id, op, true, !created)
}

// recordLaneStopUnderKey records, under a named stop's own Idempotency-Key
// (req.ClientKey), that the turn's target-key op target answers it: an
// API-executed interrupt row of the same scope, closed
// skipped{coalesced_into: target}, so a retry under the key follows it back
// to that op, as OJ-4's recordStopUnderKey does. A key that already names a
// row is left as it is.
func recordLaneStopUnderKey(st *opjournal.Store, req opjournal.Request, target string) error {
	req.Executor = opjournal.ExecutorLegacyAPI
	row, created, err := st.Propose(req)
	if err != nil {
		return proposeError(err)
	}
	if !created {
		return nil
	}
	if _, err := st.Close(row.ID, opjournal.Outcome{
		Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonCoalescedInto, Result: opjournal.Result{CoalescedInto: target},
	}, "api"); err != nil {
		return apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: recording the stop under its Idempotency-Key: %v", err)
	}
	return nil
}

// laneStopSucceeded reports whether a closed interrupt op answers its stop
// with success: it succeeded, or found nothing to stop (a legacy op's
// skipped{not_needed}).
func laneStopSucceeded(op opjournal.Op) bool {
	arm, reason := op.Outcome.Effective()
	return arm == opjournal.ArmSucceeded || arm == opjournal.ArmSkipped && reason == opjournal.ReasonNotNeeded
}

// answerLaneStop awaits op, the interrupt a /stop of session id proposed or
// found, and answers from its outcome as OJ-4 answers the same outcome:
// succeeded, or a stop that found nothing to stop, is 200; skipped{stale} is
// 200 for a stop that named no turn (nothing ran) and 409 turn_not_current
// for a named one; failed{unsupported} 501; failed{no_boundary} the legacy
// not-ended answer; provider_unavailable and expired_before_start 503; any
// other failure, or a cancel, 409 op_already_failed; and an op still open
// at laneControlAnswerBound 409 in_flight. A legacy op a key names is
// answered as OJ-4 replays it.
func (s *Server) answerLaneStop(ctx context.Context, st *opjournal.Store, id string, op opjournal.Op, named, replayed bool) (syncAnswer, error) {
	if op.Executor != opjournal.ExecutorLane {
		if op.Status != opjournal.StatusClosed {
			return syncAnswer{}, requestInFlight()
		}
		if syncOpReplaysAsSuccess(op) {
			return syncAnswer{ID: id, syncOpFields: syncOpFields{IdempotencyKey: op.ClientKey, Replayed: true, Op: conversationOpWire(op)}}, nil
		}
		return syncAnswer{}, replayedSyncError(op, humaStopError)
	}
	if op.Status != opjournal.StatusClosed {
		var err error
		if op, err = s.awaitLaneOp(ctx, st, id, op.ID, laneControlAnswerBound); err != nil {
			return syncAnswer{}, err
		}
		if op.Status != opjournal.StatusClosed {
			return syncAnswer{}, laneOpInFlight(op)
		}
	}
	ok := syncAnswer{ID: id, syncOpFields: syncOpFields{IdempotencyKey: op.ClientKey, Replayed: replayed, Op: conversationOpWire(op)}}
	arm, reason := op.Outcome.Effective()
	closed := func(what string) string {
		return fmt.Sprintf("op %s closed %s{%s}: %s", op.ID, arm, reason, what)
	}
	switch {
	case arm == opjournal.ArmSucceeded:
		return ok, nil
	case arm == opjournal.ArmSkipped && reason == opjournal.ReasonStale && named:
		return syncAnswer{}, humaStopError(fmt.Errorf("%s: %w", closed(fmt.Sprintf("turn %q of session %s was not the current turn when the stop ran", op.Scope.Turn, id)), runtime.ErrTurnNotCurrent))
	case arm == opjournal.ArmSkipped:
		return ok, nil
	case arm == opjournal.ArmFailed && reason == opjournal.ReasonUnsupported:
		cause := session.ErrTurnInterruptUnsupported
		if op.Scope.Turn != "" {
			cause = runtime.ErrTurnIdentityUnsupported
		}
		return syncAnswer{}, humaStopError(fmt.Errorf("%s: %w", closed("the session cannot be stopped this way"), cause))
	case arm == opjournal.ArmFailed && reason == opjournal.ReasonNoBoundary:
		return syncAnswer{}, humaStopError(fmt.Errorf("%s: %w", closed("the interrupted turn was not seen to end"), runtime.ErrTurnNotEnded))
	case arm == opjournal.ArmFailed && (reason == opjournal.ReasonProviderUnavailable || reason == opjournal.ReasonExpiredBeforeStart):
		return syncAnswer{}, humaStopError(fmt.Errorf("%s: %w", closed("the runtime did not answer before the stop's start_by"), runtime.ErrRuntimeUnavailable))
	}
	return syncAnswer{}, apierr.OpAlreadyFailed.Msg(fmt.Sprintf("%s: op %s closed %s; resend with a new Idempotency-Key to act again", opAlreadyFailedCode, op.ID, laneOutcomeText(op)))
}

// laneOutcomeText names a closed op's outcome, arm{reason}, and its last
// error when it has one.
func laneOutcomeText(op opjournal.Op) string {
	arm, reason := op.Outcome.Effective()
	text := string(arm)
	if reason != "" {
		text += "{" + reason + "}"
	}
	if last := strings.TrimSpace(op.LastError); last != "" {
		text += " (" + last + ")"
	}
	return text
}

// runCancelLaneKey is the client key of the interrupt a run cancel proposes
// for each holder session.
func runCancelLaneKey(runID string) string { return "run-cancel:" + runID }

// runCancelInterruptFailed reports whether op, a holder's run-cancel
// interrupt, closed failed or canceled: neither interrupted the holder nor
// found nothing to interrupt.
func runCancelInterruptFailed(op opjournal.Op) bool {
	if op.Status != opjournal.StatusClosed {
		return false
	}
	arm, _ := op.Outcome.Effective()
	return arm != opjournal.ArmSucceeded && arm != opjournal.ArmSkipped
}

// interruptRunHoldersLane is interruptRunHolders under lane authority: one
// interrupt op per holder session ids names, keyed run-cancel:<run id> with
// the run as its causation, awaited together at most
// laneControlAnswerBound. A later cancel that finds a holder's op closed
// failed or canceled retries it under a minted key, so a failure is never
// replayed. A session that closed succeeded is interrupted;
// one whose op failed, or is still open at the bound, is a failure; one
// that closed skipped (nothing to stop) is neither.
func (s *Server) interruptRunHoldersLane(ctx context.Context, store beads.Store, runID string, ids []string, fail func(string, error)) []string {
	st, err := s.conversationOpStore()
	if err != nil {
		for _, id := range ids {
			fail(id, err)
		}
		return nil
	}
	type proposed struct {
		id string
		op opjournal.Op
	}
	var ops []proposed
	for _, id := range ids {
		scope, why, whyErr := sessionOpScope(store, id)
		if why != "" {
			fail(id, fmt.Errorf("the operation journal cannot record its interrupt (%s): %w", why, errors.Join(whyErr, errors.New("nothing sent"))))
			continue
		}
		req := opjournal.Request{
			Conversation: id,
			Kind:         opjournal.KindInterrupt,
			Executor:     opjournal.ExecutorLane,
			ClientKey:    runCancelLaneKey(runID),
			Producer:     opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
			Policy:       interruptOpPolicy,
			Scope:        scope,
			Causation:    runID,
		}
		op, _, err := s.proposeLaneOp(st, req)
		if err == nil && runCancelInterruptFailed(op) {
			// A failed interrupt of the holder never uses the run's key up:
			// this cancel retries it under a minted key.
			if req.ClientKey, err = legacyop.MintKey(); err == nil {
				req.MintedKey = true
				op, _, err = s.proposeLaneOp(st, req)
			} else {
				err = apierr.Internal.Msg(err.Error())
			}
		}
		if err != nil {
			fail(id, err)
			continue
		}
		ops = append(ops, proposed{id: id, op: op})
	}
	actx, cancel := context.WithTimeout(ctx, laneControlAnswerBound)
	defer cancel()
	results := make([]opjournal.Op, len(ops))
	errs := make([]error, len(ops))
	done := make(chan int, len(ops))
	for i, p := range ops {
		go func() {
			results[i], errs[i] = s.awaitLaneOp(actx, st, p.id, p.op.ID, laneControlAnswerBound)
			done <- i
		}()
	}
	for range ops {
		<-done
	}
	var interrupted []string
	for i, p := range ops {
		op := results[i]
		arm, _ := op.Outcome.Effective()
		switch {
		case errs[i] != nil:
			fail(p.id, errs[i])
		case op.Status != opjournal.StatusClosed:
			fail(p.id, fmt.Errorf("op %s still running", op.ID))
		case arm == opjournal.ArmSucceeded:
			interrupted = append(interrupted, p.id)
		case arm == opjournal.ArmSkipped:
		default:
			fail(p.id, fmt.Errorf("op %s closed %s", op.ID, laneOutcomeText(op)))
		}
	}
	return interrupted
}

// --- /respond ---

// laneRespondAnswerBound is how long a /respond under lane authority awaits
// its op before it answers 202 accepted with the op still open, 202's
// meaning since OJ-4. A var so tests can shorten it.
var laneRespondAnswerBound = 20 * time.Second

// respondSessionLane is /respond under lane authority (respond listed): a
// respond op keyed by the interaction occurrence it answers, which the lane
// host runs at most once. key is the Idempotency-Key header.
//
// A key that already names a respond is its retry, followed through the row
// the first request left under the key: an open op is awaited and a closed
// one replayed, so a retry never resolves the occurrence again, and never
// answers an identical prompt raised later. Otherwise the front door reads
// the pending interaction as today and writes nothing on a refusal (no
// pending 409 no_pending, another request id 409 invalid_interaction,
// unsupported 501). The occurrence is the latest unresolved raising of the
// pending request id, or the next one recorded now (Store.RecordRaising),
// numbered as the lane numbers it, and the op is keyed RespondKey of it:
// a different answer to the same occurrence from the same principal is 409
// key_reuse, and the first answer wins. A request that carries a key also
// leaves an API-executed respond row under it, of the legacy op's frozen
// shape, closed skipped{coalesced_into: the lane op}.
func (s *Server) respondSessionLane(mgr *session.Manager, store beads.Store, id, key string, resp runtime.InteractionResponse) (syncAnswer, error) {
	st, err := s.conversationOpStore()
	if err != nil {
		return syncAnswer{}, err
	}
	prior, retry, err := keyedLaneOp(st, opjournal.KindRespond, id, key)
	if err != nil {
		return syncAnswer{}, err
	}
	if retry {
		target, err := followCoalesced(st, prior)
		if err != nil {
			return syncAnswer{}, err
		}
		return s.answerLaneRespond(st, id, target, true)
	}
	pending, supported, err := mgr.Pending(id)
	switch {
	case err != nil:
		return syncAnswer{}, humaSessionManagerError(err)
	case !supported:
		return syncAnswer{}, humaSessionManagerError(fmt.Errorf("session %s: %w", id, session.ErrInteractionUnsupported))
	case pending == nil || pending.RequestID == "":
		return syncAnswer{}, humaSessionManagerError(fmt.Errorf("session %s: %w", id, session.ErrNoPendingInteraction))
	case resp.RequestID != "" && resp.RequestID != pending.RequestID:
		return syncAnswer{}, humaSessionManagerError(fmt.Errorf("%w: pending interaction %q does not match request %q", session.ErrInteractionMismatch, pending.RequestID, resp.RequestID))
	}
	scope, why, whyErr := sessionOpScope(store, id)
	if why != "" {
		cause := why
		if whyErr != nil {
			cause += ": " + whyErr.Error()
		}
		return syncAnswer{}, apierr.OpJournalUnavailable.Msg(fmt.Sprintf("op_journal_unavailable: the operation journal cannot record this respond (%s); nothing was done", cause))
	}
	raising, _, err := st.RecordRaising(id, scope.Generation, pending.RequestID, opjournal.FactBasisObserved, time.Now())
	if err != nil {
		return syncAnswer{}, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: recording the interaction's raising: %v", err)
	}
	laneKey, err := opjournal.RespondKey(raising.Raised)
	if err != nil {
		return syncAnswer{}, apierr.Internal.Msgf("keying the respond: %v", err)
	}
	payload, err := opjournal.EncodeRespondPayload(opjournal.RespondPayload{Action: resp.Action, Text: resp.Text, Metadata: resp.Metadata})
	if err != nil {
		return syncAnswer{}, apierr.ValidationFailed.Msg("invalid_request: " + err.Error())
	}
	laneScope := scope
	laneScope.Interaction = raising.Raised.Interaction
	op, created, err := s.proposeLaneOp(st, opjournal.Request{
		Conversation: id,
		Kind:         opjournal.KindRespond,
		Executor:     opjournal.ExecutorLane,
		ClientKey:    laneKey,
		Producer:     opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:      payload,
		Policy:       opjournal.Policy{Guarantee: opjournal.GuaranteeAtMostOnce},
		Scope:        laneScope,
	})
	if err != nil {
		return syncAnswer{}, err
	}
	if key != "" {
		rowScope := scope
		rowScope.Interaction = pending.RequestID
		if err := recordLaneRespondUnderKey(st, id, key, rowScope, resp, op.ID); err != nil {
			return syncAnswer{}, err
		}
	}
	return s.answerLaneRespond(st, id, op, !created)
}

// recordLaneRespondUnderKey records, under a lane /respond's
// Idempotency-Key, that lane op target answers it: an API-executed respond
// row of the legacy op's frozen shape (the request-id scope, respondOpPayload,
// at_least_once), closed skipped{coalesced_into: target}, so a retry under
// the key follows it back. A key that already names a row is left as it is.
func recordLaneRespondUnderKey(st *opjournal.Store, conv, key string, scope opjournal.Scope, resp runtime.InteractionResponse, target string) error {
	payload := respondOpPayload{Action: resp.Action, Text: resp.Text}
	if len(resp.Metadata) > 0 {
		payload.Metadata = resp.Metadata
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return apierr.Internal.Msg("encoding the respond row payload: " + err.Error())
	}
	row, created, err := st.Propose(opjournal.Request{
		Conversation: conv,
		Kind:         opjournal.KindRespond,
		Executor:     opjournal.ExecutorLegacyAPI,
		ClientKey:    key,
		Producer:     opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:      string(raw),
		Policy:       respondOpPolicy,
		Scope:        scope,
	})
	if err != nil {
		return proposeError(err)
	}
	if !created {
		return nil
	}
	if _, err := st.Close(row.ID, opjournal.Outcome{
		Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonCoalescedInto, Result: opjournal.Result{CoalescedInto: target},
	}, "api"); err != nil {
		return apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: recording the respond under its Idempotency-Key: %v", err)
	}
	return nil
}

// answerLaneRespond awaits op, the respond a /respond of session id proposed
// or found, at most laneRespondAnswerBound, and answers from its outcome:
// succeeded is 202 accepted with the op; skipped{stale} 409 no_pending;
// failed{unsupported} 501; refused:action the runtime's refusal, 409
// invalid_interaction; any other failure, or a cancel, 409
// op_already_failed; and an op still open 202 accepted with the open op. A
// legacy op a key names is answered as OJ-4 replays it.
func (s *Server) answerLaneRespond(st *opjournal.Store, id string, op opjournal.Op, replayed bool) (syncAnswer, error) {
	if op.Executor != opjournal.ExecutorLane {
		if op.Status != opjournal.StatusClosed {
			return syncAnswer{}, requestInFlight()
		}
		if syncOpReplaysAsSuccess(op) {
			return syncAnswer{ID: id, syncOpFields: syncOpFields{IdempotencyKey: op.ClientKey, Replayed: true, Op: conversationOpWire(op)}}, nil
		}
		return syncAnswer{}, replayedSyncError(op, humaSessionManagerError)
	}
	if op.Status != opjournal.StatusClosed {
		var err error
		if op, err = s.awaitLaneOp(context.Background(), st, id, op.ID, laneRespondAnswerBound); err != nil {
			return syncAnswer{}, err
		}
	}
	ok := syncAnswer{ID: id, syncOpFields: syncOpFields{IdempotencyKey: op.ClientKey, Replayed: replayed, Op: conversationOpWire(op)}}
	if op.Status != opjournal.StatusClosed {
		return ok, nil
	}
	arm, reason := op.Outcome.Effective()
	closed := fmt.Sprintf("op %s closed %s", op.ID, laneOutcomeText(op))
	switch {
	case arm == opjournal.ArmSucceeded:
		return ok, nil
	case arm == opjournal.ArmSkipped && reason == opjournal.ReasonStale:
		return syncAnswer{}, humaSessionManagerError(fmt.Errorf("%s: the prompt it answers is no longer pending: %w", closed, session.ErrNoPendingInteraction))
	case arm == opjournal.ArmFailed && reason == opjournal.ReasonUnsupported:
		return syncAnswer{}, humaSessionManagerError(fmt.Errorf("%s: %w", closed, session.ErrInteractionUnsupported))
	case arm == opjournal.ArmFailed && op.Outcome.Reason == opjournal.ReasonRefusedAction:
		return syncAnswer{}, humaSessionManagerError(fmt.Errorf("%s: %w: %w", closed, session.ErrInteractionMismatch, runtime.ErrInteractionActionUnavailable))
	}
	return syncAnswer{}, apierr.OpAlreadyFailed.Msg(fmt.Sprintf("%s: %s; resend with a new Idempotency-Key to act again", opAlreadyFailedCode, closed))
}

// --- /submit, /messages and extmsg notices ---

// laneSubmitSettle is how long a /submit or /messages under lane authority
// waits for its op before it emits the request's terminal event: closed
// succeeded is the result, queued false; closed otherwise request.failed;
// still open the result, queued true, as a legacy message that was queued
// succeeded. Its later fate is op.changed's. A var so tests can shorten it.
var laneSubmitSettle = 5 * time.Second

// extmsgNoticePrincipal is the principal of the notices extmsg proposes for a
// conversation's members; their label is extmsg.
const extmsgNoticePrincipal = "gc:extmsg"

// LaneMessageOpPolicy is the lane policy of a submit intent: default and
// follow_up deliver at the session's boundary and interrupt_now as a
// deliver-now unit (D-DN: a failed interrupt makes the message wait, and
// nothing restarts the session); /messages is the default. Every one wakes
// the session if needed and is a prompt. gc session submit's local fallback
// proposes its lane op under the same policy, so the request the API would
// write and the CLI's derive one op digest.
func LaneMessageOpPolicy(intent session.SubmitIntent) opjournal.Policy {
	when := opjournal.WhenBoundary
	if intent == session.SubmitIntentInterruptNow {
		when = opjournal.WhenNow
	}
	return opjournal.Policy{Message: opjournal.MessagePolicy{When: when, Wake: opjournal.WakeIfNeeded, Role: opjournal.RolePrompt}}
}

// acceptMessageLane is /submit and /messages under lane authority (message
// listed): it proposes req as a lane message op, which the lane host
// delivers, and answers 202 once the op is committed, its request_id the op
// id. A configured named session's row is materialized before the Propose;
// resume: true first runs the operator wake that consumes the session's
// hold, else a held session keeps the op waiting. A same-key retry of a
// closed op replays it (OJ-2b's path), of an open lane op answers 202
// replayed (the lane runs it), and of an open legacy op 409 in_flight. The
// request's terminal event follows laneSubmitSettle after the commit.
func (s *Server) acceptMessageLane(ctx context.Context, store beads.Store, req *messageOpRequest, ep events.Recorder, cursor string) (asyncAcceptedBody, error) {
	st, err := s.conversationOpStore()
	if err != nil {
		return asyncAcceptedBody{}, err
	}
	key, minted := req.key, false
	if key == "" {
		if key, err = legacyop.MintKey(); err != nil {
			return asyncAcceptedBody{}, apierr.Internal.Msg(err.Error())
		}
		minted = true
	} else if err := opjournal.ValidateClientKey(key); err != nil {
		return asyncAcceptedBody{}, apierr.InvalidIdempotencyKey.Msg("invalid_idempotency_key: " + err.Error())
	}
	oreq := opjournal.Request{
		Conversation: previewConversation,
		Kind:         opjournal.KindMessage,
		Executor:     opjournal.ExecutorLane,
		ClientKey:    key,
		MintedKey:    minted,
		Producer:     opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:      req.text,
		Policy:       LaneMessageOpPolicy(req.intent),
	}
	if _, err := st.Preview(oreq); err != nil {
		return asyncAcceptedBody{}, laneMessageProposeError(err)
	}
	conv := req.conv
	if conv == "" {
		if conv, err = s.resolveSessionIDMaterializingNamedWithContext(ctx, store, req.target); err != nil {
			return asyncAcceptedBody{}, humaResolveError(err)
		}
	}
	oreq.Conversation = conv
	// A retry runs nothing: its resume wakes nothing. A lane op under the
	// key answers only the same request (Propose refuses another as key
	// reuse); a legacy op under it, recorded before the flip, replays.
	if prior, found, err := keyedLaneOp(st, opjournal.KindMessage, conv, key); err != nil {
		return asyncAcceptedBody{}, err
	} else if found && prior.Executor != opjournal.ExecutorLane {
		return s.replayLaneMessage(prior, key, req.operation, cursor)
	} else if found {
		op, _, err := st.Propose(oreq)
		if err != nil {
			return asyncAcceptedBody{}, laneMessageProposeError(err)
		}
		return s.replayLaneMessage(op, key, req.operation, cursor)
	}
	if req.resume {
		if err := s.wakeSession(beads.SessionStore{Store: store}, conv); err != nil {
			if _, conflict := session.WakeConflictState(err); !conflict {
				return asyncAcceptedBody{}, wakeHTTPError(conv, err)
			}
		}
	}
	op, created, err := st.Propose(oreq)
	if err != nil {
		return asyncAcceptedBody{}, laneMessageProposeError(err)
	}
	if !created {
		return s.replayLaneMessage(op, key, req.operation, cursor)
	}
	finish, err := s.recordAsyncAcceptance(ep, op.ID, req.operation)
	if err != nil {
		// Nothing is delivered for a request whose acceptance the event log
		// refused: its op is canceled before the lane host is rung.
		if _, _, cerr := st.Cancel(op.ID, legacyop.PrincipalOperator, "the request's acceptance could not be recorded"); cerr != nil {
			s.ops.logf("api: canceling op %s, whose acceptance could not be recorded: %v", op.ID, cerr)
		}
		return asyncAcceptedBody{}, err
	}
	s.pokeConversationLane(conv)
	go func() {
		defer finish()
		s.emitLaneMessageResult(st, op, req.operation, string(req.intent))
	}()
	return asyncAcceptedBody{Status: "accepted", RequestID: op.ID, EventCursor: cursor, IdempotencyKey: key, Op: conversationOpWire(op)}, nil
}

// laneMessageProposeError maps a refused lane message proposal onto the
// wire as the legacy admission maps it: a key that names another request is
// 422 idempotency_mismatch.
func laneMessageProposeError(err error) error {
	if errors.Is(err, opjournal.ErrKeyReuse) {
		return apierr.IdempotencyMismatch.Msg("idempotency_mismatch: Idempotency-Key reused with a different request")
	}
	return proposeError(err)
}

// replayLaneMessage answers a same-key retry of message op op: a closed op
// replays its outcome with its terminal event re-emitted (storedReplayAnswer),
// an open lane op answers 202 replayed, and an open legacy op 409 in_flight.
func (s *Server) replayLaneMessage(op opjournal.Op, key, operation, cursor string) (asyncAcceptedBody, error) {
	switch {
	case op.Status == opjournal.StatusClosed:
		body, err := s.storedReplayAnswer(op, key, operation, cursor)
		if err != nil {
			return asyncAcceptedBody{}, err
		}
		return *body, nil
	case op.Executor != opjournal.ExecutorLane:
		return asyncAcceptedBody{}, requestInFlight()
	}
	return asyncAcceptedBody{Status: "accepted", RequestID: op.ID, EventCursor: cursor, IdempotencyKey: key, Replayed: true, Op: conversationOpWire(op)}, nil
}

// emitLaneMessageResult waits laneSubmitSettle for lane message op op and
// emits its request's terminal event for operation: the result, queued
// false, once it closed succeeded; request.failed once it closed otherwise;
// the result, queued true, while it is still open.
func (s *Server) emitLaneMessageResult(st *opjournal.Store, op opjournal.Op, operation, intent string) {
	cur, err := s.awaitLaneOp(context.Background(), st, op.Conversation, op.ID, laneSubmitSettle)
	if err != nil {
		s.ops.logf("api: op %s: reading its outcome for the request's result: %v", op.ID, err)
		cur = op
	}
	wire := conversationOpWire(cur)
	arm, reason := cur.Outcome.Effective()
	switch {
	case cur.Status == opjournal.StatusClosed && arm != opjournal.ArmSucceeded:
		s.emitRequestFailedOp(op.ID, operation, messageReplayErrorCode(operation, reason), fmt.Sprintf("op %s closed %s", op.ID, laneOutcomeText(cur)), wire)
	case operation == RequestOperationSessionSubmit:
		s.emitSessionSubmitSucceeded(op.ID, op.Conversation, messageOutcome{SubmitOutcome: session.SubmitOutcome{Queued: cur.Status != opjournal.StatusClosed}}, intent, wire)
	default:
		s.emitSessionMessageSucceeded(op.ID, op.Conversation, messageOutcome{SubmitOutcome: session.SubmitOutcome{Queued: cur.Status != opjournal.StatusClosed}}, wire)
	}
}

// proposeExtmsgNotice is an extmsg member notice under lane authority: a lane
// notice from gc:extmsg, delivered at the session's boundary, waking it if
// needed, under a minted key (the notice path carries no message id).
func (s *Server) proposeExtmsgNotice(id, message string) error {
	st, err := s.conversationOpStore()
	if err != nil {
		return err
	}
	key, err := legacyop.MintKey()
	if err != nil {
		return err
	}
	_, _, err = s.proposeLaneOp(st, opjournal.Request{
		Conversation: id,
		Kind:         opjournal.KindMessage,
		Executor:     opjournal.ExecutorLane,
		ClientKey:    key,
		MintedKey:    true,
		Producer:     opjournal.Producer{Principal: extmsgNoticePrincipal, Actor: opjournal.ActorBackground},
		Payload:      message,
		Policy:       opjournal.Policy{Message: opjournal.MessagePolicy{Role: opjournal.RoleNotice, When: opjournal.WhenBoundary, Wake: opjournal.WakeIfNeeded}},
	})
	return err
}
