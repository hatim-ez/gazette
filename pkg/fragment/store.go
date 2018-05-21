package fragment

import (
	"io"

	pb "github.com/LiveRamp/gazette/pkg/protocol"
)

type store interface {
	Persist(Spool)

	Open(fragment pb.Fragment, offset int64) (io.ReadCloser, error)

	Sign(fragment pb.Fragment) (string, error)
}

type noop struct{}

func (noop) Persist(Spool) {}

func (noop) Open(fragment pb.Fragment, offset int64) (io.ReadCloser, error) { panic("unexpected") }

func (noop) Sign(fragment pb.Fragment) (string, error) { panic("unexpected") }

var Store store = noop{}