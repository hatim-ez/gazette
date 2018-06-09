package broker

import (
	"context"
	"crypto/sha1"
	"io"
	"net"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/etcdserver/etcdserverpb"
	"github.com/coreos/etcd/mvcc/mvccpb"
	gc "github.com/go-check/check"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	"github.com/LiveRamp/gazette/pkg/keyspace"
	pb "github.com/LiveRamp/gazette/pkg/protocol"
	"github.com/LiveRamp/gazette/pkg/v3.allocator"
)

type testBroker struct {
	id pb.BrokerSpec_ID
	*testServer

	*mockPeer // nil if not built with newMockBroker.
	*resolver // nil if not built with newTestBroker.
}

func newTestBroker(c *gc.C, ctx context.Context, ks *keyspace.KeySpace, id pb.BrokerSpec_ID) *testBroker {
	var state = v3_allocator.NewObservedState(ks, v3_allocator.MemberKey(ks, id.Zone, id.Suffix))
	var resolver = newResolver(state)

	var dialer, err = newDialer(16)
	c.Assert(err, gc.IsNil)

	var srv = newTestServer(c, ctx, &Service{
		resolver: resolver,
		dialer:   dialer,
	})

	c.Check(ks.Apply(etcdEvent(ks, "put", state.LocalKey, (&pb.BrokerSpec{
		Id:       id,
		Endpoint: srv.endpoint(),
	}).MarshalString())), gc.IsNil)

	return &testBroker{
		id:         id,
		testServer: srv,
		resolver:   resolver,
	}
}

func newMockBroker(c *gc.C, ctx context.Context, ks *keyspace.KeySpace, id pb.BrokerSpec_ID) *testBroker {
	var mock = newMockPeer(c, ctx)
	var srv = mock.testServer
	var key = v3_allocator.MemberKey(ks, id.Zone, id.Suffix)

	c.Check(ks.Apply(etcdEvent(ks, "put", key, (&pb.BrokerSpec{
		Id:       id,
		Endpoint: srv.endpoint(),
	}).MarshalString())), gc.IsNil)

	return &testBroker{
		id:         id,
		testServer: srv,
		mockPeer:   mock,
	}
}

func newTestJournal(c *gc.C, ks *keyspace.KeySpace, journal pb.Journal, replication int32, ids ...pb.BrokerSpec_ID) {
	var tkv []string

	// Create JournalSpec.
	tkv = append(tkv, "put",
		v3_allocator.ItemKey(ks, journal.String()),
		(&pb.JournalSpec{
			Name:        journal,
			Replication: replication,
			Fragment: pb.JournalSpec_Fragment{
				Length:           1024,
				CompressionCodec: pb.CompressionCodec_SNAPPY,
				RefreshInterval:  time.Second,
			},
		}).MarshalString())

	// Create broker assignments.
	for slot, id := range ids {
		if id == (pb.BrokerSpec_ID{}) {
			continue
		}

		var key = v3_allocator.AssignmentKey(ks, v3_allocator.Assignment{
			ItemID:       journal.String(),
			MemberZone:   id.Zone,
			MemberSuffix: id.Suffix,
			Slot:         slot,
		})

		tkv = append(tkv, "put", key, "")
	}
	c.Check(ks.Apply(etcdEvent(ks, tkv...)), gc.IsNil)
}

func etcdEvent(ks *keyspace.KeySpace, typeKeyValue ...string) clientv3.WatchResponse {
	if len(typeKeyValue)%3 != 0 {
		panic("not type/key/value")
	}

	defer ks.Mu.RUnlock()
	ks.Mu.RLock()

	var resp = clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{
			ClusterId: 0xfeedbeef,
			MemberId:  0x01234567,
			RaftTerm:  0x11223344,
			Revision:  ks.Header.Revision + 1,
		},
	}

	for i := 0; i != len(typeKeyValue); i += 3 {
		var typ, key, value = typeKeyValue[i], typeKeyValue[i+1], typeKeyValue[i+2]

		var event = &clientv3.Event{
			Kv: &mvccpb.KeyValue{
				Key:         []byte(key),
				Value:       []byte(value),
				ModRevision: ks.Header.Revision + 1,
			},
		}
		var ind, ok = ks.Search(key)

		switch typ {
		case "put":
			event.Type = clientv3.EventTypePut
		case "del":
			event.Type = clientv3.EventTypeDelete
			if !ok {
				panic("!ok")
			}
		default:
			panic(typ)
		}

		if ok {
			var cur = ks.KeyValues[ind].Raw
			event.Kv.CreateRevision = cur.CreateRevision
			event.Kv.Version = cur.Version + 1
			event.Kv.Lease = cur.Lease
		} else {
			event.Kv.CreateRevision = event.Kv.ModRevision
			event.Kv.Version = 1
		}

		resp.Events = append(resp.Events, event)
	}
	return resp
}

type testServer struct {
	c        *gc.C
	ctx      context.Context
	listener net.Listener
	srv      *grpc.Server
}

func newTestServer(c *gc.C, ctx context.Context, srv pb.BrokerServer) *testServer {
	var l, err = net.Listen("tcp", "127.0.0.1:0")
	c.Assert(err, gc.IsNil)

	var p = &testServer{
		c:        c,
		ctx:      ctx,
		listener: l,
		srv:      grpc.NewServer(),
	}

	pb.RegisterBrokerServer(p.srv, srv)
	go p.srv.Serve(p.listener)

	go func() {
		<-ctx.Done()
		p.srv.GracefulStop()
	}()

	return p
}

func (s *testServer) endpoint() pb.Endpoint {
	return pb.Endpoint("http://" + s.listener.Addr().String() + "/path")
}

func (s *testServer) dial(ctx context.Context) (*grpc.ClientConn, error) {
	return grpc.DialContext(ctx, s.listener.Addr().String(), grpc.WithInsecure())
}

func (s *testServer) mustClient() pb.BrokerClient {
	var conn, err = s.dial(s.ctx)
	s.c.Assert(err, gc.IsNil)
	return pb.NewBrokerClient(conn)
}

type mockPeer struct {
	*testServer

	replReqCh  chan *pb.ReplicateRequest
	replRespCh chan *pb.ReplicateResponse

	readReqCh  chan *pb.ReadRequest
	readRespCh chan *pb.ReadResponse

	appendReqCh  chan *pb.AppendRequest
	appendRespCh chan *pb.AppendResponse

	errCh chan error
}

func newMockPeer(c *gc.C, ctx context.Context) *mockPeer {
	var p = &mockPeer{
		replReqCh:    make(chan *pb.ReplicateRequest),
		replRespCh:   make(chan *pb.ReplicateResponse),
		readReqCh:    make(chan *pb.ReadRequest),
		readRespCh:   make(chan *pb.ReadResponse),
		appendReqCh:  make(chan *pb.AppendRequest),
		appendRespCh: make(chan *pb.AppendResponse),
		errCh:        make(chan error),
	}
	p.testServer = newTestServer(c, ctx, p)
	return p
}

func (p *mockPeer) Replicate(srv pb.Broker_ReplicateServer) error {
	// Start a read loop of requests from |srv|.
	go func() {
		log.WithField("id", p.endpoint()).Info("replicate read loop started")
		for done := false; !done; {
			var msg, err = srv.Recv()

			if err == io.EOF {
				msg, err, done = nil, nil, true
			} else if err != nil {
				done = true

				p.c.Check(err, gc.ErrorMatches, `rpc error: code = Canceled desc = context canceled`)
			}

			log.WithFields(log.Fields{"ep": p.endpoint(), "msg": msg, "err": err, "done": done}).Info("read")

			select {
			case p.replReqCh <- msg:
				// Pass.
			case <-p.ctx.Done():
				done = true
			}
		}
	}()

	for {
		select {
		case resp := <-p.replRespCh:
			p.c.Check(srv.Send(resp), gc.IsNil)
			log.WithFields(log.Fields{"ep": p.endpoint(), "resp": resp}).Info("sent")
		case err := <-p.errCh:
			log.WithFields(log.Fields{"ep": p.endpoint(), "err": err}).Info("closing")
			return err
		case <-p.ctx.Done():
			log.WithFields(log.Fields{"ep": p.endpoint()}).Info("cancelled")
			return p.ctx.Err()
		}
	}
}

func (p *mockPeer) Read(req *pb.ReadRequest, srv pb.Broker_ReadServer) error {
	select {
	case p.readReqCh <- req:
		// Pass.
	case <-p.ctx.Done():
		return p.ctx.Err()
	}

	for {
		select {
		case resp := <-p.readRespCh:
			p.c.Check(srv.Send(resp), gc.IsNil)
			log.WithFields(log.Fields{"ep": p.endpoint(), "resp": resp}).Info("sent")
		case err := <-p.errCh:
			log.WithFields(log.Fields{"ep": p.endpoint(), "err": err}).Info("closing")
			return err
		case <-p.ctx.Done():
			log.WithFields(log.Fields{"ep": p.endpoint()}).Info("cancelled")
			return p.ctx.Err()
		}
	}
}

func (p *mockPeer) Append(srv pb.Broker_AppendServer) error {
	// Start a read loop of requests from |srv|.
	go func() {
		log.WithField("ep", p.endpoint()).Info("append read loop started")
		for done := false; !done; {
			var msg, err = srv.Recv()

			if err == io.EOF {
				msg, err, done = nil, nil, true
			} else if err != nil {
				done = true

				p.c.Check(err, gc.ErrorMatches, `rpc error: code = Canceled desc = context canceled`)
			}

			log.WithFields(log.Fields{"ep": p.endpoint(), "msg": msg, "err": err, "done": done}).Info("read")

			select {
			case p.appendReqCh <- msg:
				// Pass.
			case <-p.ctx.Done():
				done = true
			}
		}
	}()

	for {
		select {
		case resp := <-p.appendRespCh:
			log.WithFields(log.Fields{"ep": p.endpoint(), "resp": resp}).Info("sending")
			return srv.SendAndClose(resp)
		case err := <-p.errCh:
			log.WithFields(log.Fields{"ep": p.endpoint(), "err": err}).Info("closing")
			return err
		case <-p.ctx.Done():
			log.WithFields(log.Fields{"ep": p.endpoint()}).Info("cancelled")
			return p.ctx.Err()
		}
	}
}

func sumOf(s string) pb.SHA1Sum {
	var d = sha1.Sum([]byte(s))
	return pb.SHA1SumFromDigest(d[:])
}
