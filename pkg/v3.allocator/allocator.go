// Package allocator implements a distributed algorithm for assigning a number
// of Items across a number of Members, where each Member runs an instance of
// the Allocator. Items and Members may come and go over time; each may have
// constraints on desired replication and assignment limits which must be
// satisfied, and replicas may be placed across distinct failure zones.
// Allocator coordinates over Etcd, and uses a greedy, incremental maximum-flow
// solver to quickly determine minimal re-assignments which best balance Items
// across Members (subject to constraints).
package v3_allocator

import (
	"context"
	"fmt"
	"strings"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/etcdserver/etcdserverpb"
	"github.com/gogo/protobuf/proto"
	log "github.com/sirupsen/logrus"

	"github.com/LiveRamp/gazette/pkg/keyspace"
	"github.com/LiveRamp/gazette/pkg/v3.allocator/push_relabel"
)

// Allocator is responsible for assigning a collection of Items, represented
// under an Etcd Allocator KeySpace, across a number of Members, also
// captured within that KeySpace.
type Allocator struct {
	KeySpace           *keyspace.KeySpace
	LocalKey           string // Unique MemberKey of this Allocator instance.
	LocalItemsCallback        // Callback invoked with local Assignments.

	// testHook is an optional testing hook, invoked after each convergence round.
	testHook func(round int, isIdle bool)
}

// Serve loads and watches the Allocator KeySpace, and if this Allocator
// instance is the current leader, performs scheduling rounds to ensure
// the allocation of all Items to Members. Serve exits on an unrecoverable
// error, or if:
//   * The local Member has an ItemLimit of Zero, AND
//   * No Assignments to the current Member remain.
//
// Eg, Serve should be gracefully stopped by updating Allocator.LocalKey's
// ItemLimit to zero (perhaps as part of a SIGTERM signal handler) and then
// waiting for Serve to exit, which it will once all of this instance's
// Assignments have been re-assigned to other Members.
func (a *Allocator) Serve(ctx context.Context, client *clientv3.Client) error {
	// Load initial state of KeySpace.
	if err := a.KeySpace.Load(ctx, client, 0); err != nil {
		return err
	}

	var failErr error
	var signalCh = make(chan struct{})

	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)

	// Begin a goroutine which will run on each signaled KeySpace update. It will
	// minimally call back with local assignments. If this Member is the current
	// leader, it will also perform a scheduling iteration.
	go func() {
		a.KeySpace.Mu.RLock()

		defer cancel()
		defer a.KeySpace.Mu.RUnlock()

		// flowNetwork is local to a single pass of the scheduler, but we retain a
		// single instance and re-use it each iteration to reduce allocation.
		var fn = new(flowNetwork)
		// Response of the last transaction we applied. We'll ensure we've minimally
		// watched through its revision before driving further action.
		var txnResponse *clientv3.TxnResponse
		// The leader runs push/relabel to re-compute a |desired| network only when
		// the allocState |networkHash| changes. Otherwise, it incrementally converges
		// towards the previous solution, which is still a valid maximum assignment.
		// This caching is both more efficient, and also mitigates the impact of
		// small instabilities in the prioritized push/relabel solution.
		var desired []Assignment
		var lastNetworkHash uint64

		for round := 0; true; {
			var as, err = newAllocState(a.KeySpace, a.LocalKey)
			if err != nil {
				failErr = err // Treat as a non-recoverable error.
				break
			} else if as.shouldExit() {
				break
			}

			var revision = a.KeySpace.Header.Revision
			a.LocalItemsCallback(as.localItems)

			// TODO(johnny): Remove when the Allocator is further along in integration testing.
			as.debugLog()

			if as.isLeader() && (txnResponse == nil || revision >= txnResponse.Header.Revision) {

				// Do we need to re-solve for a maximum assignment?
				if as.networkHash != lastNetworkHash {
					lastNetworkHash = as.networkHash

					// Build a prioritized flowNetwork and solve for maximum flow.
					fn.init(as)
					push_relabel.FindMaxFlow(&fn.source, &fn.sink)

					// Extract desired max-flow Assignments for each Item.
					desired = desired[:0]
					for item := range as.items {
						desired = extractItemFlow(as, fn, item, desired)
					}
				}

				// Use batched transactions to amortize the network cost of Etcd updates,
				// and re-verify our Member key with each flush to ensure we're still leader.
				var txn = newBatchedTxn(ctx, client,
					modRevisionUnchanged(as.members[as.localMemberInd]))

				// Converge the current state towards |desired|.
				if err = converge(txn, as, desired); err == nil {
					txnResponse, err = txn.Commit()
				}

				if err != nil {
					log.WithFields(log.Fields{"err": err, "round": round, "rev": revision}).
						Warn("converge iteration failed (will retry)")
				} else {
					if a.testHook != nil {
						a.testHook(round, revision == txnResponse.Header.Revision)
					}
					round++
				}
			}

			// Await the next KeySpace change.
			a.KeySpace.Mu.RUnlock()
			var _, ok = <-signalCh
			a.KeySpace.Mu.RLock()

			if !ok {
				break
			}
		}
	}()

	// Blocking watching KeySpace until |cancel| is called, while the above goroutine runs.
	if err := a.KeySpace.Watch(ctx, client, signalCh); err != context.Canceled {
		failErr = err
	}
	return failErr
}

// converge identifies and applies incremental changes which bring the current
// state closer to the |desired| state, subject to Item and Member constraints.
func converge(txn checkpointTxn, as *allocState, desired []Assignment) error {
	var itemState = itemState{global: as}
	var lastCRE int // cur.rightEnd of the previous iteration.

	// Walk Items, joined with their current Assignments. Simultaneously walk
	// |desired| Assignments to join against those as well.
	var it = leftJoin{
		lenL: len(as.items),
		lenR: len(as.assignments),
		compare: func(l, r int) int {
			return strings.Compare(itemAt(as.items, l).ID, assignmentAt(as.assignments, r).ItemID)
		},
	}
	for cur, ok := it.next(); ok; cur, ok = it.next() {
		// Remove any Assignments skipped between the last cursor iteration, and this
		// one. They must not have an Associated Item (eg, it was deleted).
		if err := removeDeadAssignments(txn, as.ks, as.assignments[lastCRE:cur.rightBegin]); err != nil {
			return err
		}
		lastCRE = cur.rightEnd

		var itemID, limit = itemAt(as.items, cur.left).ID, 0
		// Determine leading sub-slice of |desired| which are Assignments of |itemID|.
		for ; limit != len(desired) && desired[limit].ItemID == itemID; limit++ {
		}

		// Initialize |itemState|, computing the delta of current and |desired| Item Assignments.
		itemState.init(cur.left, as.assignments[cur.rightBegin:cur.rightEnd], desired[:limit])
		if err := itemState.constrainAndBuildOps(txn); err != nil {
			return err
		}
		desired = desired[limit:]
	}
	// Remove any trailing, dead Assignments.
	if err := removeDeadAssignments(txn, as.ks, as.assignments[lastCRE:]); err != nil {
		return err
	}

	return nil
}

// removeDeadAssignments removes Assignments |asn|, after verifying each has no associated Item.
func removeDeadAssignments(txn checkpointTxn, ks *keyspace.KeySpace, asn keyspace.KeyValues) error {
	for len(asn) != 0 {
		var itemID, limit = assignmentAt(asn, 0).ItemID, 1
		// Determine leading sub-slice of |assignments| which are Assignments of |itemID|.
		for ; limit != len(asn) && assignmentAt(asn, limit).ItemID == itemID; limit++ {
		}
		// Verify Item does not exist.
		txn.If(clientv3.Compare(clientv3.CreateRevision(ItemKey(ks, itemID)), "=", 0))
		// Verify each Assignment has not changed, then remove it.
		for i := 0; i != limit; i++ {
			txn.If(modRevisionUnchanged(asn[i]))
			txn.Then(clientv3.OpDelete(string(asn[i].Raw.Key)))
		}
		if err := txn.Checkpoint(); err != nil {
			return err
		}
		asn = asn[limit:]
	}
	return nil
}

// modRevisionUnchanged returns a Cmp which verifies the key has not changed
// from the current KeyValue.
func modRevisionUnchanged(kv keyspace.KeyValue) clientv3.Cmp {
	return clientv3.Compare(clientv3.ModRevision(string(kv.Raw.Key)), "=", kv.Raw.ModRevision)
}

// checkpointTxn runs transactions. It's modeled on clientv3.Txn, but:
//  * It introduces "checkpoints", whereby many checkpoints may be grouped into
//    a smaller number of underlying Txns, while still providing a guarantee
//    that If/Thens of a checkpoint will be issued together in one Txn.
//  * It allows If and Then to be called multiple times.
//  * It removes Else, as incompatible with the checkpoint model. As such,
//    a Txn which does not succeed becomes an error.
type checkpointTxn interface {
	If(...clientv3.Cmp) checkpointTxn
	Then(...clientv3.Op) checkpointTxn
	Commit() (*clientv3.TxnResponse, error)

	// Checkpoint ensures that all If and Then invocations since the last
	// Checkpoint are issued in the same underlying Txn. It may partially
	// flush the transaction to Etcd.
	Checkpoint() error
}

// batchedTxn implements the checkpointTxn interface, potentially queuing across
// multiple transaction checkpoints and applying them together as a single
// larger transaction. This can alleviate network RTT, amortizing delay across
// many checkpoints.
type batchedTxn struct {
	// txnDo executes a OpTxn.
	txnDo func(txn clientv3.Op) (*clientv3.TxnResponse, error)
	// Completed checkpoints ready to flush.
	cmps []clientv3.Cmp
	ops  []clientv3.Op
	// Checkpoint currently being built.
	nextCmps []clientv3.Cmp
	nextOps  []clientv3.Op
	// Cmps which should be asserted on every underlying Txn.
	fixedCmps []clientv3.Cmp
}

// newBatchedTxn returns a batchedTxn using the given Context and KV. It will
// apply |fixedCmps| on every underlying Txn it issues (eg, they needn't be added
// with If to each checkpoint).
func newBatchedTxn(ctx context.Context, kv clientv3.KV, fixedCmps ...clientv3.Cmp) *batchedTxn {
	return &batchedTxn{
		txnDo: func(txn clientv3.Op) (*clientv3.TxnResponse, error) {
			if r, err := kv.Do(ctx, txn); err != nil {
				return nil, err
			} else {
				return r.Txn(), nil
			}
		},
		fixedCmps: fixedCmps,
	}
}

func (b *batchedTxn) If(c ...clientv3.Cmp) checkpointTxn {
	b.nextCmps = append(b.nextCmps, c...)
	return b
}
func (b *batchedTxn) Then(o ...clientv3.Op) checkpointTxn {
	b.nextOps = append(b.nextOps, o...)
	return b
}

func (b *batchedTxn) Checkpoint() error {
	if len(b.cmps) == 0 {
		b.cmps = append(b.cmps, b.fixedCmps...)
	}

	var nc, no = b.nextCmps, b.nextOps
	b.nextCmps, b.nextOps = b.nextCmps[:0], b.nextOps[:0]

	if lc, lo := len(b.cmps)+len(nc), len(b.ops)+len(no); lc > maxTxnOps || lo > maxTxnOps {
		if _, err := b.Commit(); err != nil {
			return err
		}
		b.cmps = append(b.cmps, b.fixedCmps...)
	}

	b.cmps = append(b.cmps, nc...)
	b.ops = append(b.ops, no...)
	return nil
}

func (b *batchedTxn) Commit() (*clientv3.TxnResponse, error) {
	if len(b.nextCmps) != 0 || len(b.nextOps) != 0 {
		panic("must call Checkpoint before flush")
	}

	if r, err := b.txnDo(clientv3.OpTxn(b.cmps, b.ops, nil)); err != nil {
		return nil, err
	} else if !r.Succeeded {
		return r, fmt.Errorf("transaction checks did not succeed")
	} else {
		b.cmps, b.ops = b.cmps[:0], b.ops[:0]
		return r, nil
	}
}

func debugLogTxn(cmps []clientv3.Cmp, ops []clientv3.Op) {
	for _, c := range cmps {
		log.WithField("cmp", proto.CompactTextString((*etcdserverpb.Compare)(&c))).Info("cmp")
	}
	for _, o := range ops {
		if o.IsPut() {
			log.WithFields(log.Fields{
				"key":   string(o.KeyBytes()),
				"value": string(o.ValueBytes()),
			}).Info("put")
		} else if o.IsDelete() {
			log.WithFields(log.Fields{
				"key": string(o.KeyBytes()),
			}).Info("delete")
		}
	}
}

var maxTxnOps = 128