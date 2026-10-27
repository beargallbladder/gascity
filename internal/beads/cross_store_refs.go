package beads

import (
	"errors"
	"strings"
)

// Cross-store references: the one place gc decides what a blocking edge's
// target names and how to read the beads such targets name.
//
// bd (beads v1.3.2, issueops.IsExternalDepTarget) never blocks a dependent on
// an "external:" target or on a target whose id prefix differs from the
// dependent's own. gc keeps that answer byte for byte on a city with no class
// binding. On a split city gc enforces the owner rule instead — a blocker
// held in another store blocks until it is closed wherever it is held — and to
// do that it must say which bead an edge names:
//
//   - "external:<id>" names the bead <id> held somewhere else: the prefix is
//     stripped and the real bead is looked up.
//   - "external:<rig>:<capability>" names a capability, not a bead. bd treats
//     it as never blocking, and so does gc.
//   - anything else names a bead id as written.

// externalDepPrefix is bd's marker for a dependency target outside the store.
const externalDepPrefix = "external:"

// ExternalBlockerTarget reports which bead a blocking edge's target names.
// capability is true for an "external:<rig>:<capability>" ref, which names no
// bead and never blocks; target is then "". Otherwise target is the bead id,
// with an "external:" marker stripped.
func ExternalBlockerTarget(dependsOnID string) (target string, capability bool) {
	id := strings.TrimSpace(dependsOnID)
	rest, ok := strings.CutPrefix(id, externalDepPrefix)
	if !ok {
		return id, false
	}
	rest = strings.TrimSpace(rest)
	if rest == "" || strings.Contains(rest, ":") {
		return "", true
	}
	return rest, false
}

// BdIDPrefix is bd's id prefix (types.ExtractPrefix): everything up to and
// including the first "-", or "" for an id with none. bd judges a dependency
// target with the same prefix as its dependent as held by the dependent's own
// database, and any other as external.
func BdIDPrefix(id string) string {
	idx := strings.Index(id, "-")
	if idx < 0 {
		return ""
	}
	return id[:idx+1]
}

// getManyChunk bounds one batched read's id list.
const getManyChunk = 200

// GetMany reads the beads ids name from store in batched, cache-bypassing reads
// — one round trip per getManyChunk ids — open and closed alike, and returns
// the ones the store holds. An id the store does not hold is simply absent
// from the result; a failed read is an error.
//
// It uses the store's exact batch read where it has one and its LIVE handle's
// List by id everywhere else (both tiers), so a caching store answers from its
// backing store, never from its read cache.
func GetMany(store Store, ids []string) (map[string]Bead, error) {
	out := make(map[string]Bead, len(ids))
	if store == nil || len(ids) == 0 {
		return out, nil
	}
	seen := make(map[string]bool, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	for start := 0; start < len(unique); start += getManyChunk {
		chunk := unique[start:min(start+getManyChunk, len(unique))]
		if getter, ok := store.(ExactBatchGetter); ok {
			found, _, err := getter.GetExactBatch(chunk)
			if err == nil {
				for id, b := range found {
					out[id] = b
				}
				continue
			}
			if !errors.Is(err, ErrExactBatchGetUnsupported) {
				return nil, err
			}
		}
		rows, err := HandlesFor(store).Live.List(ListQuery{IDs: chunk, IncludeClosed: true})
		if err != nil {
			return nil, err
		}
		for _, b := range rows {
			if seen[b.ID] {
				out[b.ID] = b
			}
		}
	}
	return out, nil
}
