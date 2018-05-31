package v3_allocator

import (
	"context"
	"math"

	gc "github.com/go-check/check"
)

type AllocStateSuite struct{}

func (s *AllocStateSuite) TestExtractOverFixture(c *gc.C) {
	var client, ctx = etcdCluster.RandClient(), context.Background()
	buildAllocKeySpaceFixture(c, ctx, client)

	var ks = NewAllocatorKeySpace("/root", testAllocDecoder{})
	c.Check(ks.Load(ctx, client, 0), gc.IsNil)

	var state, err = NewState(ks, MemberKey(ks, "us-west", "baz"))
	c.Assert(err, gc.IsNil)

	// Expect |KS| was partitioned on entity type.
	c.Check(state.KS, gc.Equals, ks)
	c.Check(state.Assignments, gc.HasLen, 6)
	c.Check(state.Items, gc.HasLen, 2)
	c.Check(state.Members, gc.HasLen, 3)

	// Expect that local member state was extracted.
	c.Check(state.LocalKey, gc.Equals, "/root/members/us-west|baz")
	c.Check(state.LocalMemberInd, gc.Equals, 2)
	c.Check(state.LocalItems, gc.DeepEquals, []LocalItem{
		// /root/assign/item-1/us-west/baz/0
		{Item: state.Items[0], Assignments: state.Assignments[0:2], Index: 1},
		// /root/assign/item-two/us-west/baz/1
		{Item: state.Items[1], Assignments: state.Assignments[3:6], Index: 2},
		// Note /root/assign/item-missing/us-west/baz/0 is omitted (because the Item is missing).
	})

	// Again, with another local key. Verify local state extraction.
	state, err = NewState(ks, MemberKey(ks, "us-east", "bar"))
	c.Assert(err, gc.IsNil)

	c.Check(state.LocalKey, gc.Equals, "/root/members/us-east|bar")
	c.Check(state.LocalMemberInd, gc.Equals, 0)
	c.Check(state.LocalItems, gc.DeepEquals, []LocalItem{
		// /root/assign/item-two/us-east/bar/0
		{Item: state.Items[1], Assignments: state.Assignments[3:6], Index: 1},
	})

	// Again, with yet another local key.
	state, err = NewState(ks, MemberKey(ks, "us-east", "foo"))
	c.Assert(err, gc.IsNil)

	c.Check(state.LocalKey, gc.Equals, "/root/members/us-east|foo")
	c.Check(state.LocalMemberInd, gc.Equals, 1)
	c.Check(state.LocalItems, gc.DeepEquals, []LocalItem{
		// /root/assign/item-1/us-east/foo/1
		{Item: state.Items[0], Assignments: state.Assignments[0:2], Index: 0},
	})

	// Expect ordered Zones and slot counts were extracted.
	c.Check(state.Zones, gc.DeepEquals, []string{"us-east", "us-west"})
	c.Check(state.MemberSlots, gc.Equals, 6)
	c.Check(state.ItemSlots, gc.Equals, 3)
	c.Check(state.NetworkHash, gc.Equals, uint64(0x110ea3fec3194585))

	// Member counts were sized and initialized with current Assignment counts.
	// Expect counts for Assignments with missing Items were omitted.
	c.Check(state.MemberTotalCount, gc.DeepEquals, []int{1, 1, 2})
	c.Check(state.MemberPrimaryCount, gc.DeepEquals, []int{1, 0, 1})

	// Expect it returns an error if the member key is not found.
	state, err = NewState(ks, MemberKey(ks, "does-not", "exist"))
	c.Check(err, gc.ErrorMatches, "member key not found: /root/members/does-not|exist")
	c.Check(state, gc.IsNil)
}

func (s *AllocStateSuite) TestLeaderSelection(c *gc.C) {
	var client, ctx = etcdCluster.RandClient(), context.Background()
	// Note the fixture adds keys in random order (the leader may differ each run).
	buildAllocKeySpaceFixture(c, ctx, client)

	var ks = NewAllocatorKeySpace("/root", testAllocDecoder{})
	c.Check(ks.Load(ctx, client, 0), gc.IsNil)

	var count int
	for _, m := range ks.Prefixed(ks.Root + MembersPrefix) {
		var state, err = NewState(ks, string(m.Raw.Key))
		c.Assert(err, gc.IsNil)

		if state.isLeader() {
			count++
		}
	}
	c.Check(count, gc.Equals, 1) // Expect exactly one Member is leader.
}

func (s *AllocStateSuite) TestExitCondition(c *gc.C) {
	var client, ctx = etcdCluster.RandClient(), context.Background()
	buildAllocKeySpaceFixture(c, ctx, client)

	var _, err = client.Put(ctx, "/root/members/us-east|allowed-to-exit", `{"R": 0}`)
	c.Assert(err, gc.IsNil)

	var ks = NewAllocatorKeySpace("/root", testAllocDecoder{})
	c.Check(ks.Load(ctx, client, 0), gc.IsNil)

	state, err := NewState(ks, MemberKey(ks, "us-east", "foo"))
	c.Assert(err, gc.IsNil)
	c.Check(state.shouldExit(), gc.Equals, false)

	state, err = NewState(ks, MemberKey(ks, "us-east", "allowed-to-exit"))
	c.Assert(err, gc.IsNil)
	c.Check(state.shouldExit(), gc.Equals, true)

	// While we're at it, If |NetworkHash| changed with the new member.
	c.Check(state.NetworkHash, gc.Equals, uint64(0x3ebc60d2a3d8a9d))
}

func (s *AllocStateSuite) TestLoadRatio(c *gc.C) {
	var client, ctx = etcdCluster.RandClient(), context.Background()
	buildAllocKeySpaceFixture(c, ctx, client)

	var ks = NewAllocatorKeySpace("/root", testAllocDecoder{})
	c.Check(ks.Load(ctx, client, 0), gc.IsNil)

	state, err := NewState(ks, MemberKey(ks, "us-east", "foo"))
	c.Assert(err, gc.IsNil)

	// Verify expected load ratios, computed using these counts. Note Assignments are:
	//   item-1/us-east/foo/1       (Member R: 2)
	// 	 item-1/us-west/baz/0       (R: 3)
	//   item-missing/us-west/baz/0 (R: 3, but missing Items Then not contribute to Member counts)
	//   item-two/missing/member/2  (Missing Member defaults to infinite load ratio)
	// 	 item-two/us-east/bar/0     (R: 1)
	//   item-two/us-west/baz/1     (R: 3)
	for i, f := range []float32{1.0 / 2.0, 2.0 / 3.0, 2.0 / 3.0, math.MaxFloat32, 1.0 / 1.0, 2.0 / 3.0} {
		c.Check(memberLoadRatio(state.KS, state.Assignments[i], state.MemberTotalCount), gc.Equals, f)
	}
	for i, f := range []float32{0, 1.0 / 3.0, 1.0 / 3.0, math.MaxFloat32, 1.0 / 1.0, 1.0 / 3.0} {
		c.Check(memberLoadRatio(state.KS, state.Assignments[i], state.MemberPrimaryCount), gc.Equals, f)
	}
}

var _ = gc.Suite(&AllocStateSuite{})
