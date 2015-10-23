package gazette

import (
	"bytes"
	"flag"
	"io"
	"io/ioutil"
	"os"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"

	"github.com/pippio/api-server/varz"
	"github.com/pippio/gazette/async"
	"github.com/pippio/gazette/journal"
)

var writeConcurrency = flag.Int("gazetteWriteConcurrency", 2,
	"Concurrency of asynchronous, locally-spooled Gazette write client")

// Time to wait in between broker write errors.
var kWriteClientCooloffTimeout = time.Second * 5

const (
	kMaxWriteSpoolSize = 1 << 27 // A single spool is up to 128MiB.
	kWriteQueueSize    = 1024    // Allows a total of 128GiB of spooled writes.

	// Local disk-backed temporary directory where pending writes are spooled.
	kWriteTmpDirectory = "/var/tmp/gazette-writes"
)

type pendingWrite struct {
	journal journal.Name
	file    *os.File
	offset  int64
	started time.Time

	// Signals successful write.
	promise async.Promise
}

var pendingWritePool = sync.Pool{
	New: func() interface{} {
		err := os.MkdirAll(kWriteTmpDirectory, 0700)
		if err != nil {
			return err
		}
		f, err := ioutil.TempFile(kWriteTmpDirectory, "gazette-write")
		if err != nil {
			return err
		}
		// File is collected as soon as this final descriptor is closed.
		// Note this means that Stat/Truncate/etc will no longer succeed.
		os.Remove(f.Name())

		write := &pendingWrite{file: f}
		return write
	}}

func releasePendingWrite(p *pendingWrite) {
	*p = pendingWrite{file: p.file}
	if _, err := p.file.Seek(0, 0); err != nil {
		log.WithField("err", err).Warn("failed to seek(0) releasing pending write")
	} else {
		pendingWritePool.Put(p)
	}
}

func writeAllOrNone(write *pendingWrite, r io.Reader) error {
	n, err := io.Copy(write.file, r)
	if err == nil {
		write.offset += int64(n)
	} else {
		write.file.Seek(write.offset, 0)
	}
	return err
}

// WriteClient wraps a Client to provide asynchronous batching and automatic retries
// of writes to Gazette journals. Writes to each journal are spooled to local
// disk (and never memory), so back-pressure from slow or down brokers does not
// affect busy writers (at least, until disk runs out). Writes are retried
// indefinitely, until aknowledged by a broker.
type WriteClient struct {
	client *Client
	closed async.Promise

	writeQueue chan *pendingWrite
	// Indexes pendingWrite's which are in |writeQueue|, and still append-able.
	writeIndex   map[journal.Name]*pendingWrite
	writeIndexMu sync.Mutex
}

func NewWriteClient(client *Client) *WriteClient {
	writer := &WriteClient{
		client:     client,
		closed:     make(async.Promise),
		writeQueue: make(chan *pendingWrite, kWriteQueueSize),
		writeIndex: make(map[journal.Name]*pendingWrite),
	}
	for i := 0; i != *writeConcurrency; i++ {
		go writer.serveWrites()
	}
	return writer
}

func (c *WriteClient) Close() {
	close(c.writeQueue)
	c.closed.Wait()
}

func (c *WriteClient) obtainWrite(name journal.Name) (*pendingWrite, bool, error) {
	// Is a non-full pendingWrite for this journal already in |writeQueue|?
	write, ok := c.writeIndex[name]
	if ok && write.offset < kMaxWriteSpoolSize {
		return write, false, nil
	}
	popped := pendingWritePool.Get()

	if err, ok := popped.(error); ok {
		return nil, false, err
	} else {
		write = popped.(*pendingWrite)
		write.journal = name
		write.promise = make(async.Promise)
		write.started = time.Now()
		c.writeIndex[name] = write
		return write, true, nil
	}
}

// Appends |buffer| to |journal|. Either all of |buffer| is written, or none
// of it is. Returns a Promise which is resolved when the write has been
// fully committed.
func (c *WriteClient) Write(name journal.Name, buf []byte) (async.Promise, error) {
	return c.ReadFrom(name, bytes.NewReader(buf))
}

// Appends |r|'s content to |journal|, by reading until io.EOF. Either all of
// |r| is written, or none of it is. Returns a Promise which is resolved when
// the write has been fully committed.
func (c *WriteClient) ReadFrom(name journal.Name, r io.Reader) (async.Promise, error) {
	var promise async.Promise

	c.writeIndexMu.Lock()
	write, isNew, err := c.obtainWrite(name)
	if err == nil {
		err = writeAllOrNone(write, r)
		promise = write.promise // Retain, as we can't access |write| after unlock.
	}
	c.writeIndexMu.Unlock()

	if err != nil {
		return nil, err
	}
	if isNew {
		c.writeQueue <- write
	}
	return promise, nil
}

func (c *WriteClient) serveWrites() {
	for {
		write := <-c.writeQueue
		if write == nil {
			// Signals Close().
			break
		}

		c.writeIndexMu.Lock()
		if c.writeIndex[write.journal] == write {
			delete(c.writeIndex, write.journal)
		}
		c.writeIndexMu.Unlock()

		if err := c.onWrite(write); err != nil {
			log.WithFields(log.Fields{"journal": write.journal, "err": err}).
				Error("write failed")
		}
	}
	c.closed.Resolve()
}

func (c *WriteClient) onWrite(write *pendingWrite) error {
	// We now have exclusive ownership of |write|. Iterate
	// attempting to write to server, until it's acknowledged.
	for i := 0; true; i++ {
		if i != 0 {
			time.Sleep(kWriteClientCooloffTimeout)
		}

		if _, err := write.file.Seek(0, 0); err != nil {
			return err // Not recoverable
		}
		err := c.client.Put(journal.AppendArgs{
			Journal: write.journal,
			Content: io.LimitReader(write.file, write.offset),
		})

		if err != nil {
			log.WithFields(log.Fields{"journal": write.journal, "err": err}).
				Warn("write failed")
			continue
		}
		// Success. Notify any waiting clients.
		write.promise.Resolve()

		varz.ObtainAverage("gazette", "avgWriteSize").
			Add(float64(write.offset))
		varz.ObtainAverage("gazette", "avgWriteMs").
			Add(float64(time.Now().Sub(write.started)) / float64(time.Millisecond))
		varz.ObtainCount("gazette", "writeBytes").Add(write.offset)

		releasePendingWrite(write)
		return nil
	}
	panic("not reached")
}
