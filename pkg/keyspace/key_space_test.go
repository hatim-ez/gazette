package keyspace

import (
	"context"
	"testing"

	"github.com/coreos/etcd/clientv3"
	epb "github.com/coreos/etcd/etcdserver/etcdserverpb"
	"github.com/coreos/etcd/integration"
	gc "github.com/go-check/check"
)

type KeySpaceSuite struct{}

func (s *KeySpaceSuite) TestLoadAndWatch(c *gc.C) {
	var client = etcdCluster.RandClient()
	var ctx, cancel = context.WithCancel(context.Background())

	var _, err = client.Delete(ctx, "", clientv3.WithPrefix())
	c.Assert(err, gc.IsNil)

	for k, v := range map[string]string{
		"/one":   "1",
		"/three": "3",
		"/foo":   "invalid value is logged and skipped",
	} {
		var _, err = client.Put(ctx, k, v)
		c.Assert(err, gc.IsNil)
	}

	var ks = NewKeySpace("/", testDecoder)
	c.Check(ks.Load(ctx, client, 0), gc.IsNil)
	verifyDecodedKeyValues(c, ks.KeyValues, map[string]int{"/one": 1, "/three": 3})

	var signalCh = make(chan struct{})
	go func() {
		for _, op := range []clientv3.Op{
			clientv3.OpPut("/two", "2"),
			clientv3.OpPut("/bar", "invalid key/value is also logged and skipped"),
			clientv3.OpDelete("/one"),
			clientv3.OpPut("/three", "4"),
			clientv3.OpPut("/foo", "5"), // Formerly invalid key/value is now consistent.
		} {
			var _, err = client.Do(ctx, op)
			c.Check(err, gc.IsNil)

			var _, ok = <-signalCh // Expect a signal is delivered.
			c.Check(ok, gc.Equals, true)
		}
		cancel()
	}()

	c.Check(ks.Watch(ctx, client, signalCh), gc.Equals, context.Canceled)

	// Expect |signalCh| was closed.
	var _, ok = <-signalCh
	c.Check(ok, gc.Equals, false)

	verifyDecodedKeyValues(c, ks.KeyValues,
		map[string]int{"/two": 2, "/three": 4, "/foo": 5})
}

func (s *KeySpaceSuite) TestHeaderPatching(c *gc.C) {
	var h epb.ResponseHeader

	var other = epb.ResponseHeader{
		ClusterId: 8675309,
		MemberId:  111111,
		Revision:  123,
		RaftTerm:  232323,
	}
	c.Check(patchHeader(&h, other, true), gc.IsNil)
	c.Check(h, gc.Equals, other)

	other.MemberId = 222222
	c.Check(patchHeader(&h, other, true), gc.IsNil)
	c.Check(h, gc.Equals, other)

	// Revision must be equal.
	other.Revision = 122
	c.Check(patchHeader(&h, other, true), gc.ErrorMatches,
		`etcd Revision mismatch \(expected = 123, got 122\)`)

	other.Revision = 124
	other.MemberId = 333333
	other.RaftTerm = 3434343
	c.Check(patchHeader(&h, other, false), gc.IsNil)
	c.Check(h, gc.Equals, other)

	// Revision must be monotonically increasing.
	c.Check(patchHeader(&h, other, false), gc.ErrorMatches,
		`etcd Revision mismatch \(expected > 124, got 124\)`)

	// ClusterId cannot change.
	other.Revision = 125
	other.ClusterId = 1337
	c.Check(patchHeader(&h, other, false), gc.ErrorMatches,
		`etcd ClusterID mismatch \(expected 8675309, got 1337\)`)
}

func (s *KeySpaceSuite) TestWatchResponseApply(c *gc.C) {
	var ks = KeySpace{decode: testDecoder}

	var resp = []clientv3.WatchResponse{{
		Header: epb.ResponseHeader{ClusterId: 9999, Revision: 10},
		Events: []*clientv3.Event{
			putEvent("/some/key", "99", 10, 10, 1),
			putEvent("/other/key", "100", 10, 10, 1),
		},
	}}

	c.Check(ks.apply(resp), gc.IsNil)
	verifyDecodedKeyValues(c, ks.KeyValues,
		map[string]int{"/some/key": 99, "/other/key": 100})

	// Key/value inconsistencies are logged but returned as an error.
	resp = []clientv3.WatchResponse{{
		Header: epb.ResponseHeader{ClusterId: 9999, Revision: 11},
		Events: []*clientv3.Event{
			putEvent("/some/key", "101", 10, 11, 2),
			delEvent("/not/here", 11),
		},
	}}
	c.Check(ks.apply(resp), gc.IsNil)
	verifyDecodedKeyValues(c, ks.KeyValues,
		map[string]int{"/some/key": 101, "/other/key": 100})

	// Header inconsistencies fail the apply.
	resp = []clientv3.WatchResponse{{
		Header: epb.ResponseHeader{ClusterId: 10000, Revision: 12},
		Events: []*clientv3.Event{
			delEvent("/not/here", 11),
		},
	}}
	c.Check(ks.apply(resp), gc.ErrorMatches, `etcd ClusterID mismatch .*`)

	// Multiple WatchResponses may be applied at once. Keys may be in any order,
	// and mutated multiple times within the batch apply.
	resp = []clientv3.WatchResponse{
		{
			Header: epb.ResponseHeader{ClusterId: 9999, Revision: 12},
			Events: []*clientv3.Event{
				putEvent("/aaaa", "1111", 12, 12, 1),
				putEvent("/bbbb", "2222", 12, 12, 1),
				putEvent("/cccc", "invalid", 12, 12, 1),
				putEvent("/to-delete", "0000", 12, 12, 1),
			},
		},
		{
			Header: epb.ResponseHeader{ClusterId: 9999, Revision: 13},
			Events: []*clientv3.Event{
				putEvent("/bbbb", "3333", 12, 13, 2),
				putEvent("/cccc", "4444", 12, 13, 2),
			},
		},
		{
			Header: epb.ResponseHeader{ClusterId: 9999, Revision: 14},
			Events: []*clientv3.Event{
				putEvent("/aaaa", "5555", 12, 14, 2),
				delEvent("/to-delete", 14),
			},
		},
		{
			Header: epb.ResponseHeader{ClusterId: 9999, Revision: 15},
			Events: []*clientv3.Event{
				putEvent("/bbbb", "6666", 12, 15, 3),
				putEvent("/eeee", "7777", 15, 15, 1),
			},
		},
	}
	c.Check(ks.apply(resp), gc.IsNil)
	verifyDecodedKeyValues(c, ks.KeyValues,
		map[string]int{
			"/some/key":  101,
			"/other/key": 100,

			"/aaaa": 5555,
			"/bbbb": 6666,
			"/cccc": 4444,
			"/eeee": 7777,
		})
}

var (
	_           = gc.Suite(&KeySpaceSuite{})
	etcdCluster *integration.ClusterV3
)

func Test(t *testing.T) {
	etcdCluster = integration.NewClusterV3(t, &integration.ClusterConfig{Size: 1})
	gc.TestingT(t)
	etcdCluster.Terminate(t)
}