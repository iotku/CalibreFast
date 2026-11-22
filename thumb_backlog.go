package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Thumbnails made later, while no cover-thumb requests are coming in, so
// that they come from the cache when they're asked for without making anyone
// wait now:
//   - those whose request was cancelled (the cover scrolled away);
//   - those known to be coming (warmThumbs): what a Discover row shows next,
//     on the next visit or the next click (a tag row's next strip, the rest
//     of the Shuffle page being walked, Recommended's next page, books just
//     added to Recently added). Not the whole library: that's
//     -build-thumbnails (thumb_build.go).

const (
	backlogSize      = 1024            // cancelled thumbnails remembered; more are dropped
	backlogIdleAfter = 2 * time.Second // quiet time before the backlog runs
)

var (
	thumbBacklog = make(chan string, backlogSize)
	backlogged   sync.Map // uuid -> struct{}: in thumbBacklog

	backlogRunning  atomic.Bool  // runThumbBacklog is making them: warmThumbs queues
	thumbRequests   atomic.Int64 // cover-thumb requests in progress
	lastThumbFinish atomic.Int64 // unix nanoseconds
)

var errThumbsBusy = errors.New("cover-thumb requests waiting")

// thumbRequestStarted counts a cover-thumb request; call the result when it ends.
func thumbRequestStarted() func() {
	thumbRequests.Add(1)
	return func() {
		lastThumbFinish.Store(time.Now().UnixNano())
		thumbRequests.Add(-1)
	}
}

func thumbsIdle() bool {
	return thumbRequests.Load() == 0 && time.Since(time.Unix(0, lastThumbFinish.Load())) >= backlogIdleAfter
}

// backlogThumb queues a book's thumbnail for the backlog (once; dropped when
// the backlog is full).
func backlogThumb(uuid string) {
	if _, queued := backlogged.LoadOrStore(uuid, struct{}{}); queued {
		return
	}
	select {
	case thumbBacklog <- uuid:
	default:
		backlogged.Delete(uuid)
	}
}

// warmThumbs queues the thumbnails of books about to be shown (see the top),
// leaving out those with nothing to make: in the cache already (a stat), or
// known to have no cover. Books the backlog already has cost nothing.
func warmThumbs(uuids ...string) {
	if !backlogRunning.Load() {
		return // nothing would make them (tests, -build-thumbnails)
	}
	for _, u := range uuids {
		if u == "" {
			continue
		}
		if _, missing := noCover.Load(u); missing {
			continue
		}
		if _, queued := backlogged.Load(u); queued {
			continue
		}
		if !thumbCached(u) {
			backlogThumb(u)
		}
	}
}

// runThumbBacklog makes backlogged thumbnails one at a time while the server
// is idle. A cover-thumb request arriving meanwhile stops the current one,
// which goes back in the queue.
func runThumbBacklog() {
	backlogRunning.Store(true)
	yield := func() error {
		if thumbRequests.Load() > 0 {
			return errThumbsBusy
		}
		return nil
	}
	for uuid := range thumbBacklog {
		backlogged.Delete(uuid)
		for !thumbsIdle() {
			time.Sleep(backlogIdleAfter / 4)
		}
		imageSem <- struct{}{}
		_, _, err := loadThumb(uuid, yield)
		<-imageSem
		if errors.Is(err, errThumbsBusy) {
			backlogThumb(uuid)
		}
	}
}
