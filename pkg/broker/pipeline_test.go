package broker

import (
	"context"
	"errors"
	"testing"

	"github.com/LiveRamp/gazette/pkg/fragment"
	pb "github.com/LiveRamp/gazette/pkg/protocol"
	gc "github.com/go-check/check"
)

type PipelineSuite struct{}

func (s *PipelineSuite) TestBasicLifeCycle(c *gc.C) {
	var rm = newReplicationMock(c)
	defer rm.cancel()

	var pln = rm.newPipeline(rm.header(0, 100))

	var req = &pb.ReplicateRequest{Content: []byte("foobar")}
	pln.scatter(req)

	c.Check(pln.sendErr(), gc.IsNil)
	c.Check(<-rm.brokerA.replReqCh, gc.DeepEquals, req)
	c.Check(<-rm.brokerC.replReqCh, gc.DeepEquals, req)

	var proposal = pln.spool.Next()
	req = &pb.ReplicateRequest{Proposal: &proposal}
	pln.scatter(req)

	c.Check(pln.sendErr(), gc.IsNil)
	c.Check(<-rm.brokerA.replReqCh, gc.DeepEquals, req)
	c.Check(<-rm.brokerC.replReqCh, gc.DeepEquals, req)

	var waitFor1, closeAfter1 = pln.barrier()

	// Second client issues a write and close.
	pln.scatter(&pb.ReplicateRequest{Content: []byte("bazbing")})
	_, _ = <-rm.brokerA.replReqCh, <-rm.brokerC.replReqCh
	pln.closeSend()

	c.Check(pln.sendErr(), gc.IsNil)
	c.Check(<-rm.brokerA.replReqCh, gc.IsNil) // Expect EOF.
	c.Check(<-rm.brokerC.replReqCh, gc.IsNil) // Expect EOF.

	var waitFor2, closeAfter2 = pln.barrier()

	// First client reads its response.
	<-waitFor1

	rm.brokerA.replRespCh <- &pb.ReplicateResponse{Status: pb.Status_OK}
	rm.brokerC.replRespCh <- &pb.ReplicateResponse{Status: pb.Status_OK}

	pln.gatherOK()
	c.Check(pln.recvErr(), gc.IsNil)
	c.Check(pln.recvResp, gc.DeepEquals, []pb.ReplicateResponse{{}, {}, {}})

	close(closeAfter1)

	// Second client reads its response.
	<-waitFor2

	rm.brokerA.errCh <- nil // Send EOF.
	rm.brokerC.errCh <- nil // Send EOF.

	pln.gatherEOF()
	c.Check(pln.recvErr(), gc.IsNil)

	close(closeAfter2)
}

func (s *PipelineSuite) TestPeerErrorCases(c *gc.C) {
	var rm = newReplicationMock(c)
	defer rm.cancel()

	var pln = rm.newPipeline(rm.header(0, 100))

	var req = &pb.ReplicateRequest{Content: []byte("foo")}
	pln.scatter(req)

	c.Check(pln.sendErr(), gc.IsNil)
	c.Check(<-rm.brokerA.replReqCh, gc.DeepEquals, req)
	c.Check(<-rm.brokerC.replReqCh, gc.DeepEquals, req)

	// Have peer A return an error. Peer B returns a non-OK response status (where OK is expected).
	rm.brokerA.errCh <- errors.New("error!")
	rm.brokerC.replRespCh <- &pb.ReplicateResponse{Status: pb.Status_FRAGMENT_MISMATCH}

	// Expect pipeline retains the first recv error for each peer.
	pln.gatherOK()
	c.Check(pln.recvErrs[0], gc.ErrorMatches, `rpc error: code = Unknown desc = error!`)
	c.Check(pln.recvErrs[1], gc.IsNil)
	c.Check(pln.recvErrs[2], gc.ErrorMatches, `unexpected !OK response: status:FRAGMENT_MISMATCH `)

	// Expect recvErr decorates the first error with peer metadata.
	c.Check(pln.recvErr(), gc.ErrorMatches, `recv from zone:"A" suffix:"1" : rpc error: .*`)

	pln.spool.Fragment.File.Close() // Force a write error of the local Spool.

	req = &pb.ReplicateRequest{Content: []byte("bar"), ContentDelta: 3}
	pln.scatter(req)

	// Expect pipeline retains the first send error for each peer, including the local Spool.
	c.Check(pln.sendErrs[0], gc.ErrorMatches, `EOF`)
	c.Check(pln.sendErrs[1], gc.ErrorMatches, `write .*: file already closed`)
	c.Check(pln.sendErrs[2], gc.IsNil) // Send-side of connection is still valid (only recv is broken).

	c.Check(<-rm.brokerC.replReqCh, gc.DeepEquals, req)

	// Expect sendErr decorates the first error with peer metadata.
	c.Check(pln.sendErr(), gc.ErrorMatches, `send to zone:"A" suffix:"1" : EOF`)

	pln.closeSend()

	// Finish shutdown by having brokerC receive and send EOF.
	c.Check(<-rm.brokerC.replReqCh, gc.IsNil)
	rm.brokerC.errCh <- nil
	pln.gatherEOF()

	// Restart a new pipeline. Immediately send an EOF, and test handling of
	// an unexpected received message prior to peer EOF.
	pln = rm.newPipeline(rm.header(0, 100))
	pln.closeSend()

	c.Check(<-rm.brokerA.replReqCh, gc.IsNil) // Read EOF.
	c.Check(<-rm.brokerC.replReqCh, gc.IsNil) // Read EOF.

	rm.brokerA.errCh <- nil                                                       // Send EOF.
	rm.brokerC.replRespCh <- &pb.ReplicateResponse{Status: pb.Status_WRONG_ROUTE} // Unexpected response.
	rm.brokerC.errCh <- nil                                                       // Now, send EOF.

	pln.gatherEOF()
	c.Check(pln.recvErrs[0], gc.IsNil)
	c.Check(pln.recvErrs[1], gc.IsNil)
	c.Check(pln.recvErrs[2], gc.ErrorMatches, `unexpected response: status:WRONG_ROUTE `)
}

func (s *PipelineSuite) TestGatherSyncCases(c *gc.C) {
	var rm = newReplicationMock(c)
	defer rm.cancel()

	var pln = rm.newPipeline(rm.header(0, 100))

	var req = &pb.ReplicateRequest{
		Header:      rm.header(1, 100),
		Journal:     "a/journal",
		Proposal:    &pb.Fragment{Journal: "a/journal", Begin: 123, End: 123},
		Acknowledge: true,
	}
	pln.scatter(req)

	// Expect each peer sees |req| with its ID in the Header.
	req.Header = rm.header(0, 100)
	c.Check(<-rm.brokerA.replReqCh, gc.DeepEquals, req)
	req.Header = rm.header(2, 100)
	c.Check(<-rm.brokerC.replReqCh, gc.DeepEquals, req)

	// Craft a peer response Header at a later revision, with a different Route.
	var wrongRouteHdr = rm.header(0, 4567)
	wrongRouteHdr.Route.Brokers[0].Suffix = "other"

	rm.brokerA.replRespCh <- &pb.ReplicateResponse{
		Status: pb.Status_WRONG_ROUTE,
		Header: wrongRouteHdr,
	}
	rm.brokerC.replRespCh <- &pb.ReplicateResponse{
		Status:   pb.Status_FRAGMENT_MISMATCH,
		Fragment: &pb.Fragment{Begin: 567, End: 890},
	}

	// Expect the new Fragment offset and etcd revision to read through are returned.
	var rollToOffset, readRev = pln.gatherSync(*req.Proposal)
	c.Check(rollToOffset, gc.Equals, int64(890))
	c.Check(readRev, gc.Equals, int64(4567))
	c.Check(pln.recvErr(), gc.IsNil)
	c.Check(pln.sendErr(), gc.IsNil)

	// Again. This time peers return success.
	req.Proposal = &pb.Fragment{Journal: "a/journal", Begin: 890, End: 890}
	pln.scatter(req)

	_, _ = <-rm.brokerA.replReqCh, <-rm.brokerC.replReqCh
	rm.brokerA.replRespCh <- &pb.ReplicateResponse{Status: pb.Status_OK}
	rm.brokerC.replRespCh <- &pb.ReplicateResponse{Status: pb.Status_OK}

	rollToOffset, readRev = pln.gatherSync(*req.Proposal)
	c.Check(rollToOffset, gc.Equals, int64(0))
	c.Check(readRev, gc.Equals, int64(0))
	c.Check(pln.recvErr(), gc.IsNil)
	c.Check(pln.sendErr(), gc.IsNil)

	// Again. This time, peers return !OK status with invalid responses.
	pln.scatter(req)

	_, _ = <-rm.brokerA.replReqCh, <-rm.brokerC.replReqCh

	rm.brokerA.replRespCh <- &pb.ReplicateResponse{
		Status: pb.Status_WRONG_ROUTE,
		Header: rm.header(0, 99), // Revision not greater than |pln|'s.
	}
	rm.brokerC.replRespCh <- &pb.ReplicateResponse{
		Status:   pb.Status_FRAGMENT_MISMATCH,
		Fragment: &pb.Fragment{Begin: 567, End: 889}, // End offset < proposal.
	}

	rollToOffset, readRev = pln.gatherSync(*req.Proposal)
	c.Check(rollToOffset, gc.Equals, int64(0))
	c.Check(readRev, gc.Equals, int64(0))
	c.Check(pln.sendErr(), gc.IsNil)
	c.Check(pln.recvErr(), gc.NotNil)

	c.Check(pln.recvErrs[0], gc.ErrorMatches, `unexpected WRONG_ROUTE: broker_id:.*`)
	c.Check(pln.recvErrs[1], gc.IsNil)
	c.Check(pln.recvErrs[2], gc.ErrorMatches, `unexpected FRAGMENT_MISMATCH: begin:567 end:889 .*`)
}

func (s *PipelineSuite) TestPipelineSync(c *gc.C) {
	var rm = newReplicationMock(c)
	defer rm.cancel()

	// Tweak Spool to have a different End & Sum.
	var spool = <-rm.spoolCh
	spool.Fragment.End, spool.Fragment.Sum = 123, pb.SHA1Sum{Part1: 999}
	rm.spoolCh <- spool

	var pln = rm.newPipeline(rm.header(0, 100))

	go func() {
		// Read sync request.
		c.Check(<-rm.brokerA.replReqCh, gc.DeepEquals, &pb.ReplicateRequest{
			Journal:     "a/journal",
			Header:      rm.header(0, 100),
			Proposal:    &pb.Fragment{Journal: "a/journal", Begin: 0, End: 123, Sum: pb.SHA1Sum{Part1: 999}},
			Acknowledge: true,
		})
		_ = <-rm.brokerC.replReqCh

		// Peers disagree on Fragment End.
		rm.brokerA.replRespCh <- &pb.ReplicateResponse{
			Status:   pb.Status_FRAGMENT_MISMATCH,
			Fragment: &pb.Fragment{Begin: 567, End: 892},
		}
		rm.brokerC.replRespCh <- &pb.ReplicateResponse{
			Status:   pb.Status_FRAGMENT_MISMATCH,
			Fragment: &pb.Fragment{Begin: 567, End: 890},
		}

		// Next iteration. Expect proposal is updated to reflect largest offset.
		c.Check(<-rm.brokerA.replReqCh, gc.DeepEquals, &pb.ReplicateRequest{
			Journal:     "a/journal",
			Header:      rm.header(0, 100),
			Proposal:    &pb.Fragment{Journal: "a/journal", Begin: 892, End: 892},
			Acknowledge: true,
		})
		_ = <-rm.brokerC.replReqCh

		// Peers agree.
		rm.brokerA.replRespCh <- &pb.ReplicateResponse{Status: pb.Status_OK}
		rm.brokerC.replRespCh <- &pb.ReplicateResponse{Status: pb.Status_OK}

		// Next round.
		_, _ = <-rm.brokerA.replReqCh, <-rm.brokerC.replReqCh

		// Peer C response with a larger Etcd revision.
		var wrongRouteHdr = rm.header(0, 4567)
		wrongRouteHdr.Route.Brokers[0].Suffix = "other"

		rm.brokerA.replRespCh <- &pb.ReplicateResponse{Status: pb.Status_OK}
		rm.brokerC.replRespCh <- &pb.ReplicateResponse{
			Status: pb.Status_WRONG_ROUTE,
			Header: wrongRouteHdr,
		}

		// Expect start() sends EOF.
		c.Check(<-rm.brokerA.replReqCh, gc.IsNil)
		c.Check(<-rm.brokerC.replReqCh, gc.IsNil)
		rm.brokerA.errCh <- nil
		rm.brokerC.errCh <- nil

		// Next round sends an error.
		_, _ = <-rm.brokerA.replReqCh, <-rm.brokerC.replReqCh
		rm.brokerA.errCh <- errors.New("an error")
		rm.brokerC.replRespCh <- &pb.ReplicateResponse{Status: pb.Status_OK}

		// Expect EOF.
		c.Check(<-rm.brokerA.replReqCh, gc.IsNil)
		c.Check(<-rm.brokerC.replReqCh, gc.IsNil)
		rm.brokerC.errCh <- nil // |brokerA| has already closed.
	}()

	c.Check(pln.synchronize(), gc.IsNil)
	c.Check(pln.readThroughRev, gc.Equals, int64(0))

	// Next round. This time, the pipeline is closed and readThroughRev is set.
	c.Check(pln.synchronize(), gc.IsNil)
	c.Check(pln.readThroughRev, gc.Equals, int64(4567))

	// Next round with new pipeline. Peer returns an error, and it's passed through.
	pln = rm.newPipeline(rm.header(0, 100))
	c.Check(pln.synchronize(), gc.ErrorMatches, `recv from zone:"A" suffix:"1" : rpc error: .*`)
}

type replicationMock struct {
	ctx    context.Context
	cancel context.CancelFunc

	brokerA, brokerC *mockPeer
	dialer           dialer

	spoolCh chan fragment.Spool

	commits   []fragment.Fragment
	completes []fragment.Spool
}

func newReplicationMock(c *gc.C) *replicationMock {
	var ctx, cancel = context.WithCancel(context.Background())
	var brokerA, brokerC = newMockPeer(c, ctx), newMockPeer(c, ctx)

	var dialer, err = newDialer(8)
	c.Assert(err, gc.IsNil)

	var m = &replicationMock{
		ctx:     ctx,
		cancel:  cancel,
		brokerA: brokerA,
		brokerC: brokerC,
		dialer:  dialer,
		spoolCh: make(chan fragment.Spool, 1),
	}
	m.spoolCh <- fragment.NewSpool("a/journal", m)

	return m
}

func (m *replicationMock) header(id int, rev int64) *pb.Header {
	var hdr = &pb.Header{
		Route: pb.Route{
			Primary: 1,
			Brokers: []pb.BrokerSpec_ID{
				{Zone: "A", Suffix: "1"},
				{Zone: "B", Suffix: "2"},
				{Zone: "C", Suffix: "3"},
			},
			Endpoints: []pb.Endpoint{
				m.brokerA.endpoint(),
				pb.Endpoint("http://[100::]"),
				m.brokerC.endpoint(),
			},
		},
		Etcd: pb.Header_Etcd{
			ClusterId: 12,
			MemberId:  34,
			Revision:  rev,
			RaftTerm:  78,
		},
	}
	hdr.BrokerId = hdr.Route.Brokers[id]
	return hdr
}

func (m *replicationMock) newPipeline(hdr *pb.Header) *pipeline {
	return newPipeline(m.ctx, *hdr, <-m.spoolCh, m.spoolCh, m.dialer)
}

func (m *replicationMock) SpoolCommit(f fragment.Fragment) { m.commits = append(m.commits, f) }
func (m *replicationMock) SpoolComplete(s fragment.Spool)  { m.completes = append(m.completes, s) }

var _ = gc.Suite(&PipelineSuite{})

func Test(t *testing.T) { gc.TestingT(t) }
