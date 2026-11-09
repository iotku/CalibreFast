package main

import (
	"context"
	"net/http/httptest"
	"testing"
)

func drainBacklog() {
	for {
		select {
		case uuid := <-thumbBacklog:
			backlogged.Delete(uuid)
		default:
			return
		}
	}
}

func TestBacklogThumbOnce(t *testing.T) {
	defer drainBacklog()
	backlogThumb("a-1")
	backlogThumb("a-1")
	backlogThumb("b-2")
	if n := len(thumbBacklog); n != 2 {
		t.Errorf("backlog has %d entries, want 2", n)
	}
}

// A request cancelled while waiting for a slot goes to the backlog instead.
func TestCoverThumbCancelledWhileWaiting(t *testing.T) {
	defer drainBacklog()
	for range cap(imageSem) {
		imageSem <- struct{}{}
	}
	defer func() {
		for range cap(imageSem) {
			<-imageSem
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/cover-thumb/c-3", nil).WithContext(ctx)
	coverThumbHandler(httptest.NewRecorder(), req)

	if _, ok := backlogged.Load("c-3"); !ok {
		t.Error("cancelled request wasn't backlogged")
	}
	if thumbRequests.Load() != 0 {
		t.Error("request still counted as in progress")
	}
}
