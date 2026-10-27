package main

import (
	"github.com/gastownhall/gascity/internal/config"
)

// promptQueryTopology is the topology the four discovery placeholders of an
// agent's prompt render against. A cross-store-eligible agent's `gc hook`
// already fans out over the city and every rig store, so its query stays the
// single-store `bd` form and runs once per store. Its placeholders run
// verbatim in the agent's own shell with no such fan-out, so on a city with
// rigs they read through the federated `gc ready` instead of one bd store —
// the only reader that spans the stores a routed bead may live in.
//
// The trigger is CONFIGURED rigs, not serving rigs: suspension is runtime
// state, and keying on it would make the rendered prompt flip on every
// `gc rig suspend`/`resume`. `gc ready` skips suspended rigs at read time.
//
// FederatedReady is only ever set here, never cleared: a city that already
// federates (a relocated coordination class) keeps the topology its resolver
// answered, and every other shape — rig-scoped agents, rig-less cities — is
// untouched, so their prompts render byte-for-byte what base rendered.
func promptQueryTopology(topo config.QueryTopology, a *config.Agent, rigs []config.Rig) config.QueryTopology {
	if !topo.FederatedReady && agentIsCrossStoreEligible(a) && len(rigs) > 0 {
		topo.FederatedReady = true
	}
	return topo
}
