package gazette

import (
	"io"
	"time"

	log "github.com/Sirupsen/logrus"

	"github.com/pippio/api-server/cloudstore"
)

const (
	kIndexWatcherPeriod              = 5 * time.Minute
	kIndexWatcherIncrementalLoadSize = 50
)

type IndexWatcher struct {
	journal string

	cfs    cloudstore.FileSystem
	cursor interface{}

	// Channel into which discovered fragments are produced.
	updates chan<- Fragment

	stop        chan struct{}
	initialLoad chan struct{}
}

func NewIndexWatcher(journal string, cfs cloudstore.FileSystem,
	updates chan<- Fragment) *IndexWatcher {
	w := &IndexWatcher{
		journal:     journal,
		cfs:         cfs,
		updates:     updates,
		stop:        make(chan struct{}),
		initialLoad: make(chan struct{}),
	}
	return w
}

func (w *IndexWatcher) StartWatchingIndex() *IndexWatcher {
	go w.loop()
	return w
}

func (w *IndexWatcher) WaitForInitialLoad() {
	<-w.initialLoad
}

func (w *IndexWatcher) Stop() {
	w.stop <- struct{}{}
	<-w.stop // Blocks until loop() exits.
}

func (w *IndexWatcher) loop() {
	// Copy so we can locally nil it after closing.
	initialLoad := w.initialLoad

	ticker := time.NewTicker(kIndexWatcherPeriod)
loop:
	for {
		if err := w.onRefresh(); err != nil {
			log.WithFields(log.Fields{"journal": w.journal, "err": err}).
				Warn("failed to refresh index")
		} else if initialLoad != nil {
			close(initialLoad)
			initialLoad = nil
		}

		select {
		case <-ticker.C:
		case <-w.stop:
			break loop
		}
	}
	if initialLoad != nil {
		// Attempts to wait for initial load will no longer block.
		close(initialLoad)
	}
	ticker.Stop()
	log.WithField("journal", w.journal).Info("index watch loop exiting")
	close(w.stop)
}

func (w *IndexWatcher) onRefresh() error {
	// Add a trailing slash to unambiguously represent a directory. Some cloud
	// FileSystems (eg, GCS) require this if no subordinate files are present.
	dirPath := w.journal + "/"

	// Open the fragment directory, making it first if necessary.
	if err := w.cfs.MkdirAll(dirPath, 0750); err != nil {
		return err
	}
	dir, err := w.cfs.Open(w.journal + "/")
	if err != nil {
		return err
	}
	// Perform iterative incremental loads until no new fragments are available.
	for {
		files, err := dir.Readdir(kIndexWatcherIncrementalLoadSize)

		for _, file := range files {
			if file.IsDir() {
				log.WithField("path", file.Name()).
					Warning("unexpected directory in fragment index")
				continue
			}
			fragment, err := ParseFragment(w.journal, file.Name())
			if err != nil {
				log.WithFields(log.Fields{"path": file.Name(), "err": err}).
					Warning("failed to parse content-name")
			} else {
				w.updates <- fragment
			}
		}

		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
	}
	return nil
}
