package broker

import (
	"io"

	gc "github.com/go-check/check"

	pb "github.com/LiveRamp/gazette/pkg/protocol"
)

type ReplicateSuite struct{}

func (s *ReplicateSuite) TestStreamAndCommit(c *gc.C) {
	runBrokerTestCase(c, func(f brokerFixture) {
		var stream, err = f.client.Replicate(f.ctx)
		c.Assert(err, gc.IsNil)

		var resolution, _ = f.resolver.resolve("peer/journal", false, false)

		c.Check(stream.Send(&pb.ReplicateRequest{
			Journal: "peer/journal",
			Route:   resolution.route,
			Proposal: &pb.Fragment{
				Journal: "peer/journal",
			},
			Acknowledge: true,
		}), gc.IsNil)
		expectReplResponse(c, stream, &pb.ReplicateResponse{Status: pb.Status_OK})

		c.Check(stream.Send(&pb.ReplicateRequest{Content: []byte("foobar"), ContentDelta: 0}), gc.IsNil)
		c.Check(stream.Send(&pb.ReplicateRequest{Content: []byte("bazbing"), ContentDelta: 6}), gc.IsNil)

		c.Check(resolution.replica.(*replicaImpl).index.EndOffset(), gc.Equals, int64(0))

		c.Check(stream.Send(&pb.ReplicateRequest{
			Proposal: &pb.Fragment{
				Journal: "peer/journal",
				Begin:   0,
				End:     13,
				Sum:     sumOf("foobarbazbing"),
			},
			Acknowledge: true,
		}), gc.IsNil)
		expectReplResponse(c, stream, &pb.ReplicateResponse{Status: pb.Status_OK})

		c.Check(resolution.replica.(*replicaImpl).index.EndOffset(), gc.Equals, int64(13))

		c.Check(stream.CloseSend(), gc.IsNil)
		_, err = stream.Recv()
		c.Check(err, gc.Equals, io.EOF)
	})
}

// TestWrongOffset

// TestSumMismatch

func expectReplResponse(c *gc.C, stream pb.Broker_ReplicateClient, expect *pb.ReplicateResponse) {
	var resp, err = stream.Recv()
	c.Check(err, gc.IsNil)
	c.Check(resp, gc.DeepEquals, expect)
}

var _ = gc.Suite(&ReplicateSuite{})
