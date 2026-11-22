package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// Stopping waits for a handler still at work (its connection already cut)
// before the databases close, and names one that doesn't finish in time.
func TestWaitForHandlers(t *testing.T) {
	if left := waitForHandlers(time.Now().Add(time.Second)); left != nil {
		t.Fatalf("nothing running: got %v", left)
	}

	release := make(chan struct{})
	started := make(chan struct{}, 2)
	h := trackHandlers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
	}))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/upload", nil))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/book/edit/1", nil))
	<-started
	<-started

	left := waitForHandlers(time.Now().Add(50 * time.Millisecond))
	if want := []string{"POST /api/upload", "POST /book/edit/1"}; !reflect.DeepEqual(left, want) {
		t.Fatalf("still running: got %v, want %v", left, want)
	}

	// Once they return, the wait ends without using up its time.
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()
	start := time.Now()
	if left := waitForHandlers(time.Now().Add(10 * time.Second)); left != nil {
		t.Fatalf("after they returned: got %v", left)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("waited %s for handlers that had returned", waited)
	}
}
