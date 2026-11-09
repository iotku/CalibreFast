package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Thumbnails whose request was cancelled (the cover scrolled away) are made
// later, while no cover-thumb requests are coming in, so that the next
// visit gets them from the cache without making anyone wait now.

const (
	backlogSize      = 1024            // cancelled thumbnails remembered; more are dropped
	backlogIdleAfter = 2 * time.Second // quiet time before the backlog runs
)

var (
	thumbBacklog = make(chan string, backlogSize)
	backlogged   sync.Map // uuid -> struct{}: in thumbBacklog

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

// runThumbBacklog makes backlogged thumbnails one at a time while the server
// is idle. A cover-thumb request arriving meanwhile stops the current one,
// which goes back in the queue.
func runThumbBacklog() {
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
