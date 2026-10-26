package splittest

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// Projection is the projection split over one physical test store: a work
// view and a class view of the same rows, each hiding the other side's.
//
// A physical split (NewSplitStores) moves every infrastructure bead into its
// own store, which means every fixture that seeds a session or a molecule into
// "the city store" has to move too. The projection split catches the same
// wrong-handle READS without moving any fixture: over the one physical test
// store it hands out two views.
//
//   - The WORK view hides every row coordclass.Classify puts in an
//     infrastructure class. Production's work accessors return it: the city
//     store, rig stores, the CLI's work store.
//   - The CLASS view hides every work row. Production's class accessors
//     return it: the sessions, graph, mail, nudges and orders stores.
//
// Fixtures keep seeding the raw store. Production code then sees what it would
// see on a split city, and every row a view hid from a caller that asked for it
// is a violation charged to the caller's production call site:
//
//   - a by-id read (Get, DepList) of a wrong-class row;
//   - a list read whose query SELECTED the hidden row: by id, parent, type,
//     label, metadata or assignee. A broad scan (ListOpen, an unfiltered
//     Ready, a status-only List) also hides rows but is not flagged: it was
//     not looking for them, it merely filters what it finds;
//   - a write (Update, Close, SetMetadata, DepAdd, ...) that touches a
//     wrong-class row, which then fails with ErrNotFound as it would on a split
//     city;
//   - a Create of a bead whose class belongs to the other side. The row lands,
//     but only the view that created it can see it afterwards, exactly as a
//     misrouted write on a split city is invisible to the class's readers.
//
// A read inside beads.FederatedRead is a declared federation leg or residence
// probe: it is filtered but not flagged.
//
// Three refinements keep the gate quiet about what is not a routing defect.
// Infrastructure beads a MemStore leaf mints get the class binding's "gcg-"
// prefix (MemStore.SetMintPrefixFunc), so by-id routers can route by reserved
// prefix the way they do on a born-split city. Broad scans are not flagged (see
// above). Rig stores are work views like the city store: graph beads always
// live in the city binding, so a rig-store graph write is a violation of its
// own, and the sites that do it today sit in the baseline as known violations
// rather than on an allow-list.
type Projection struct {
	leaf beads.Store
	sink *violationSink

	mu sync.Mutex
	// misplaced pins the side of a row created through the wrong view: a
	// misrouted write is visible only where it was written.
	misplaced map[string]projectionSide

	work  beads.Store
	class beads.Store
}

type projectionSide int

const (
	sideWork projectionSide = iota + 1
	sideClass
)

func (s projectionSide) String() string {
	if s == sideWork {
		return "work"
	}
	return "class"
}

var (
	projectionsMu sync.Mutex
	projections   = map[any]*Projection{}
)

// ProjectionOf returns the memoized projection over leaf, building it on first
// use. A view passed as leaf is unwrapped to its physical store, so asking for
// the class view of a work view yields the class view of the same store, and
// view identity is stable for the life of the process. A *beads.MemStore leaf
// gets the gate's mint hook: infrastructure beads it mints from then on carry
// the class binding's prefix.
func ProjectionOf(leaf beads.Store) *Projection {
	return projectionOf(leaf, gateSink)
}

func projectionOf(leaf beads.Store, sink *violationSink) *Projection {
	if v, ok := leaf.(projectionView); ok {
		leaf = v.projection().leaf
	}
	key, keyable := projectionKey(leaf)
	if keyable {
		projectionsMu.Lock()
		defer projectionsMu.Unlock()
		if p, ok := projections[key]; ok {
			return p
		}
	}
	p := &Projection{leaf: leaf, sink: sink, misplaced: map[string]projectionSide{}}
	p.work = newProjectionView(p, sideWork)
	p.class = newProjectionView(p, sideClass)
	if minter, ok := leaf.(mintPrefixSetter); ok {
		minter.SetMintPrefixFunc(classMintPrefix)
	}
	if keyable {
		projections[key] = p
	}
	return p
}

// mintPrefixSetter is the MemStore mint hook, also promoted through FileStore.
type mintPrefixSetter interface {
	SetMintPrefixFunc(func(beads.Bead) string)
}

// projectionKey returns leaf as a map key when its dynamic value is comparable.
func projectionKey(leaf beads.Store) (any, bool) {
	if leaf == nil {
		return nil, false
	}
	if !reflect.TypeOf(leaf).Comparable() {
		return nil, false
	}
	return leaf, true
}

// classMintPrefix is the gate's mint hook: infrastructure beads mint under the
// whole-split binding's prefix, work beads under the store's own.
func classMintPrefix(b beads.Bead) string {
	if coordclass.Classify(b).IsInfrastructure() {
		prefix, _ := config.ReservedClassPrefix(config.BeadClassGraph)
		return prefix
	}
	return ""
}

// WorkView returns the view a work accessor hands out.
func (p *Projection) WorkView() beads.Store { return p.work }

// ClassView returns the view a coordination-class accessor hands out.
func (p *Projection) ClassView() beads.Store { return p.class }

// Leaf returns the physical store the views share.
func (p *Projection) Leaf() beads.Store { return p.leaf }

// sideOf reports which side of the split owns b.
func (p *Projection) sideOf(b beads.Bead) projectionSide {
	p.mu.Lock()
	pinned, ok := p.misplaced[b.ID]
	p.mu.Unlock()
	if ok {
		return pinned
	}
	if coordclass.Classify(b).IsInfrastructure() {
		return sideClass
	}
	return sideWork
}

func (p *Projection) pin(id string, side projectionSide) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.misplaced[id] = side
}

// projectionView is implemented by both view variants.
type projectionView interface {
	projection() *Projection
}

// view is one side of a projection. It embeds the physical store for the
// methods it does not need to police (Ping, local strings) and overrides every
// read and write that names or returns a row.
type view struct {
	beads.Store
	p    *Projection
	side projectionSide
}

// storageView is the StorageCreateStore-preserving variant, built only over a
// leaf that has CreateWithStorage, so the policy layer's storage fallback keeps
// firing for leaves without it.
type storageView struct {
	*view
	storage beads.StorageCreateStore
}

func newProjectionView(p *Projection, side projectionSide) beads.Store {
	v := &view{Store: p.leaf, p: p, side: side}
	if storage, ok := p.leaf.(beads.StorageCreateStore); ok {
		return &storageView{view: v, storage: storage}
	}
	return v
}

var (
	_ beads.Store                            = (*view)(nil)
	_ beads.StorageCreateStore               = (*storageView)(nil)
	_ beads.ContextReadyReader               = (*view)(nil)
	_ beads.Counter                          = (*view)(nil)
	_ beads.GraphApplyHandleProvider         = (*view)(nil)
	_ beads.ConditionalWriterHandleProvider  = (*view)(nil)
	_ beads.ConditionalWritesResolveTargeter = (*view)(nil)
	_ beads.GuardedBatcherHandleProvider     = (*view)(nil)
	_ beads.MetadataCASWriterHandleProvider  = (*view)(nil)
	_ beads.AtomicTxStore                    = (*view)(nil)
	_ beads.ParentProjectionWaiter           = (*view)(nil)
	_ beads.DepMetadataReader                = (*view)(nil)
	_ beads.BatchDeleter                     = (*view)(nil)
	_ beads.ConditionalAssignmentReleaser    = (*view)(nil)
)

func (v *view) projection() *Projection { return v.p }

func (v *view) visible(b beads.Bead) bool { return v.p.sideOf(b) == v.side }

// flag charges one hidden row to the caller's production call site, unless the
// caller declared the read a federation leg.
func (v *view) flag(op string, b beads.Bead) {
	facts := captureStack()
	if facts.federated {
		return
	}
	v.p.sink.record(SplitViolation{
		View:  v.side.String(),
		Op:    op,
		Class: coordclass.Classify(b).String(),
		ID:    b.ID,
		Site:  facts.site,
		Via:   facts.via,
		Test:  facts.test,
	})
}

// filter drops the rows this view cannot see, flagging them when the query
// selected them.
func (v *view) filter(op string, selective bool, rows []beads.Bead, err error) ([]beads.Bead, error) {
	if err != nil || len(rows) == 0 {
		return rows, err
	}
	out := make([]beads.Bead, 0, len(rows))
	for _, b := range rows {
		if v.visible(b) {
			out = append(out, b)
			continue
		}
		if selective {
			v.flag(op, b)
		}
	}
	return out, nil
}

// notHere is the error a split city's store returns for a row it does not hold.
func notHere(id string) error {
	return fmt.Errorf("bead %q is not in this store: %w", id, beads.ErrNotFound)
}

// guardID answers whether a by-id write may proceed: nil when the row is this
// view's (or does not exist, so the leaf reports its own error), ErrNotFound
// after flagging when it belongs to the other side.
func (v *view) guardID(op, id string) error {
	b, err := v.p.leaf.Get(id)
	if err != nil {
		return nil
	}
	if v.visible(b) {
		return nil
	}
	v.flag(op, b)
	return notHere(id)
}

// Get hides a wrong-class row.
func (v *view) Get(id string) (beads.Bead, error) {
	b, err := v.p.leaf.Get(id)
	if err != nil {
		return b, err
	}
	if !v.visible(b) {
		v.flag("get", b)
		return beads.Bead{}, notHere(id)
	}
	return b, nil
}

// List hides wrong-class rows, flagging them when the query selected them.
func (v *view) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := v.p.leaf.List(q)
	return v.filter("list", listQuerySelective(q), rows, err)
}

// ListOpen is a broad scan: it hides wrong-class rows without flagging.
func (v *view) ListOpen(status ...string) ([]beads.Bead, error) {
	rows, err := v.p.leaf.ListOpen(status...)
	return v.filter("list-open", false, rows, err)
}

// Ready hides wrong-class rows, flagging them when the query selected them.
func (v *view) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	rows, err := v.p.leaf.Ready(q...)
	return v.filter("ready", readyQuerySelective(q), rows, err)
}

// ReadyContext is Ready with the leaf's deadline-aware read when it has one.
func (v *view) ReadyContext(ctx context.Context, q ...beads.ReadyQuery) ([]beads.Bead, error) {
	var rows []beads.Bead
	var err error
	if reader, ok := v.p.leaf.(beads.ContextReadyReader); ok {
		rows, err = reader.ReadyContext(ctx, q...)
	} else {
		rows, err = v.p.leaf.Ready(q...)
	}
	return v.filter("ready", readyQuerySelective(q), rows, err)
}

// Children hides wrong-class children; asking for a parent's children selects
// them, so every hidden child is flagged.
func (v *view) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	rows, err := v.p.leaf.Children(parentID, opts...)
	return v.filter("children", true, rows, err)
}

// ListByLabel hides and flags wrong-class rows.
func (v *view) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	rows, err := v.p.leaf.ListByLabel(label, 0, opts...)
	rows, err = v.filter("list-by-label", true, rows, err)
	return limitRows(rows, limit), err
}

// ListByAssignee hides and flags wrong-class rows.
func (v *view) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	rows, err := v.p.leaf.ListByAssignee(assignee, status, 0)
	rows, err = v.filter("list-by-assignee", true, rows, err)
	return limitRows(rows, limit), err
}

// ListByMetadata hides and flags wrong-class rows.
func (v *view) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	rows, err := v.p.leaf.ListByMetadata(filters, 0, opts...)
	rows, err = v.filter("list-by-metadata", true, rows, err)
	return limitRows(rows, limit), err
}

func limitRows(rows []beads.Bead, limit int) []beads.Bead {
	if limit > 0 && len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

// Count is unsupported on a view: a count cannot be filtered by class, so
// callers fall back to List, which can.
func (v *view) Count(context.Context, beads.ListQuery, ...string) (int, error) {
	return 0, fmt.Errorf("counting beads through a split-strict view: %w", beads.ErrCountUnsupported)
}

// Create lands the row, flagging a bead of the other side's class and pinning
// it to this view, the only one that will see it.
func (v *view) Create(b beads.Bead) (beads.Bead, error) {
	created, err := v.p.leaf.Create(b)
	if err != nil {
		return created, err
	}
	v.afterCreate(b, created)
	return created, nil
}

// CreateWithStorage is Create through the leaf's storage-tier create.
func (v *storageView) CreateWithStorage(b beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	created, err := v.storage.CreateWithStorage(b, storage)
	if err != nil {
		return created, err
	}
	v.afterCreate(b, created)
	return created, nil
}

// afterCreate flags and pins a row created through the wrong view. The class
// is the REQUESTED bead's (falling back to the returned row's): a store may hand
// back a row without the labels the caller asked for, and the caller's request
// is what decides where the row belongs.
func (v *view) afterCreate(requested, created beads.Bead) {
	owner := sideWork
	if coordclass.Classify(requested).IsInfrastructure() || coordclass.Classify(created).IsInfrastructure() {
		owner = sideClass
	}
	if owner == v.side {
		return
	}
	v.flag("create", created)
	v.p.pin(created.ID, v.side)
}

// Update refuses a wrong-class row.
func (v *view) Update(id string, opts beads.UpdateOpts) error {
	if err := v.guardID("update", id); err != nil {
		return err
	}
	return v.p.leaf.Update(id, opts)
}

// Close refuses a wrong-class row.
func (v *view) Close(id string) error {
	if err := v.guardID("close", id); err != nil {
		return err
	}
	return v.p.leaf.Close(id)
}

// Reopen refuses a wrong-class row.
func (v *view) Reopen(id string) error {
	if err := v.guardID("reopen", id); err != nil {
		return err
	}
	return v.p.leaf.Reopen(id)
}

// CloseAll closes this view's rows and flags the rest, which a split city's
// store would not hold.
func (v *view) CloseAll(ids []string, metadata map[string]string) (int, error) {
	mine := make([]string, 0, len(ids))
	for _, id := range ids {
		if v.guardID("close-all", id) == nil {
			mine = append(mine, id)
		}
	}
	if len(mine) == 0 {
		return 0, nil
	}
	return v.p.leaf.CloseAll(mine, metadata)
}

// SetMetadata refuses a wrong-class row.
func (v *view) SetMetadata(id, key, value string) error {
	if err := v.guardID("set-metadata", id); err != nil {
		return err
	}
	return v.p.leaf.SetMetadata(id, key, value)
}

// SetMetadataBatch refuses a wrong-class row.
func (v *view) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := v.guardID("set-metadata", id); err != nil {
		return err
	}
	return v.p.leaf.SetMetadataBatch(id, kvs)
}

// Delete refuses a wrong-class row.
func (v *view) Delete(id string) error {
	if err := v.guardID("delete", id); err != nil {
		return err
	}
	return v.p.leaf.Delete(id)
}

// DeleteBatch deletes this view's rows, refusing the batch when any row is the
// other side's.
func (v *view) DeleteBatch(ids []string) error {
	for _, id := range ids {
		if err := v.guardID("delete", id); err != nil {
			return err
		}
	}
	deleter, ok := v.p.leaf.(beads.BatchDeleter)
	if !ok {
		return fmt.Errorf("split-strict view: leaf store %T does not support orphan-preserving batch delete", v.p.leaf)
	}
	return deleter.DeleteBatch(ids)
}

// DepAdd refuses an edge from a wrong-class row, and an edge to one: no split
// city's store can hold a dependency on a row it does not have. A parent-child
// edge that restates the bead's own ParentID passes, as in StrictStore.
func (v *view) DepAdd(issueID, dependsOnID, depType string) error {
	if depType == "parent-child" {
		if b, err := v.p.leaf.Get(issueID); err == nil && b.ParentID == dependsOnID {
			return v.p.leaf.DepAdd(issueID, dependsOnID, depType)
		}
	}
	if err := v.guardID("dep-add", issueID); err != nil {
		return err
	}
	if err := v.guardID("dep-add", dependsOnID); err != nil {
		return err
	}
	return v.p.leaf.DepAdd(issueID, dependsOnID, depType)
}

// DepRemove refuses a wrong-class source row.
func (v *view) DepRemove(issueID, dependsOnID string) error {
	if err := v.guardID("dep-remove", issueID); err != nil {
		return err
	}
	return v.p.leaf.DepRemove(issueID, dependsOnID)
}

// DepList reports no edges for a wrong-class row, which a split city's store
// would not hold.
func (v *view) DepList(id, direction string) ([]beads.Dep, error) {
	if b, err := v.p.leaf.Get(id); err == nil && !v.visible(b) {
		v.flag("dep-list", b)
		return nil, nil
	}
	return v.p.leaf.DepList(id, direction)
}

// DepListBatch is DepList per id.
func (v *view) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	out := make(map[string][]beads.Dep, len(ids))
	for _, id := range ids {
		if b, err := v.p.leaf.Get(id); err == nil && !v.visible(b) {
			v.flag("dep-list", b)
			out[id] = nil
			continue
		}
		deps, err := v.p.leaf.DepList(id, "down")
		if err != nil {
			return nil, fmt.Errorf("listing deps for %q: %w", id, err)
		}
		out[id] = deps
	}
	return out, nil
}

// DepMetadata forwards the leaf's edge-payload read.
func (v *view) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	reader, ok := v.p.leaf.(beads.DepMetadataReader)
	if !ok {
		return "", false, fmt.Errorf("reading dependency metadata %s -> %s: leaf %T exposes no edge-payload read", issueID, dependsOnID, v.p.leaf)
	}
	return reader.DepMetadata(issueID, dependsOnID)
}

// Tx polices creates and by-id writes inside the callback like their
// store-level twins.
func (v *view) Tx(commitMsg string, fn func(beads.Tx) error) error {
	return v.p.leaf.Tx(commitMsg, func(tx beads.Tx) error {
		return fn(&viewTx{tx: tx, v: v})
	})
}

type viewTx struct {
	tx beads.Tx
	v  *view
}

func (t *viewTx) Create(b beads.Bead) (beads.Bead, error) {
	created, err := t.tx.Create(b)
	if err != nil {
		return created, err
	}
	t.v.afterCreate(b, created)
	return created, nil
}

func (t *viewTx) Update(id string, opts beads.UpdateOpts) error {
	if err := t.v.guardID("update", id); err != nil {
		return err
	}
	return t.tx.Update(id, opts)
}

func (t *viewTx) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := t.v.guardID("set-metadata", id); err != nil {
		return err
	}
	return t.tx.SetMetadataBatch(id, kvs)
}

func (t *viewTx) Close(id string) error {
	if err := t.v.guardID("close", id); err != nil {
		return err
	}
	return t.tx.Close(id)
}

// ReleaseIfCurrent refuses a wrong-class row, then forwards.
func (v *view) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	if err := v.guardID("release", id); err != nil {
		return false, err
	}
	releaser, ok := v.p.leaf.(beads.ConditionalAssignmentReleaser)
	if !ok {
		return false, beads.ErrConditionalReleaseUnsupported
	}
	return releaser.ReleaseIfCurrent(id, expectedAssignee)
}

// GraphApplyHandle forwards the leaf's graph apply, flagging a plan whose
// class belongs to the other side and pinning the rows it creates to this view.
func (v *view) GraphApplyHandle() (beads.GraphApplyStore, bool) {
	applier, ok := beads.GraphApplyFor(v.p.leaf)
	if !ok {
		return nil, false
	}
	return viewGraphApplier{v: v, applier: applier}, true
}

type viewGraphApplier struct {
	v       *view
	applier beads.GraphApplyStore
}

func (a viewGraphApplier) ApplyGraphPlan(ctx context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	result, err := a.applier.ApplyGraphPlan(ctx, plan)
	if err != nil || result == nil {
		return result, err
	}
	owner := sideWork
	if coordclass.ClassifyGraphPlan(plan).IsInfrastructure() {
		owner = sideClass
	}
	if owner == a.v.side {
		return result, nil
	}
	flagged := false
	for _, id := range result.IDs {
		b, getErr := a.v.p.leaf.Get(id)
		if getErr != nil {
			continue
		}
		if !flagged {
			a.v.flag("graph-apply", b)
			flagged = true
		}
		a.v.p.pin(id, a.v.side)
	}
	return result, nil
}

// ConditionalWriterHandle forwards the leaf's conditional writes. They are not
// policed by class: they act on rows a caller already read through this view.
func (v *view) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	return beads.ConditionalWriterFor(v.p.leaf)
}

// ConditionalWritesResolveTarget declares the leaf as the conditional-writes
// resolution target, as the typed class wrappers do.
func (v *view) ConditionalWritesResolveTarget() beads.Store { return v.p.leaf }

// GuardedBatcherHandle forwards the leaf's guarded batches.
func (v *view) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	return beads.GuardedBatcherFor(v.p.leaf)
}

// MetadataCASWriterHandle forwards the leaf's metadata compare-and-set.
func (v *view) MetadataCASWriterHandle() (beads.MetadataCASWriter, bool) {
	return beads.MetadataCASWriterFor(v.p.leaf)
}

// AtomicTx reports the leaf's transactional guarantee.
func (v *view) AtomicTx() bool { return beads.StoreSupportsAtomicTx(v.p.leaf) }

// WaitForParentProjection forwards the leaf's projection wait.
func (v *view) WaitForParentProjection(ctx context.Context, id, oldParentID, newParentID string) error {
	if waiter, ok := v.p.leaf.(beads.ParentProjectionWaiter); ok {
		return waiter.WaitForParentProjection(ctx, id, oldParentID, newParentID)
	}
	return nil
}

// CloseStore releases the leaf's backing handle when it has one.
func (v *view) CloseStore() error {
	if closer, ok := v.p.leaf.(interface{ CloseStore() error }); ok {
		return closer.CloseStore()
	}
	return nil
}

// listQuerySelective reports a list query that picks rows by something a
// class owns: ids, a parent, a type, a label, metadata or an assignee. A query
// constrained only by status, time, limit or tier is a broad scan.
func listQuerySelective(q beads.ListQuery) bool {
	return len(q.IDs) > 0 || q.ParentID != "" || len(q.ParentIDs) > 0 || q.Type != "" ||
		q.Label != "" || len(q.Metadata) > 0 || q.Assignee != "" || len(q.Assignees) > 0
}

// readyQuerySelective reports a ready query that picks rows by assignee or
// metadata; an unfiltered ready scan is broad.
func readyQuerySelective(qs []beads.ReadyQuery) bool {
	for _, q := range qs {
		if q.Assignee != "" || len(q.MetadataEquals) > 0 {
			return true
		}
	}
	return false
}
