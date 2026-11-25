package main

// Server-Timing (with -server-timing): how long the server took for each
// response, shown next to the network's time in the browser's developer
// tools (the request's Timing tab). Through a tunnel or a slow link, it
// tells the server's own time from the trip there and back. Off by default:
// a header on every response, only of use while looking into speed.
// Requests slower than slowRequest are logged either way.
//
//	Server-Timing: auth;dur=0.41, app;dur=12.30
//
// app is from the request reaching the server to the response's headers
// (everything before the body is sent); auth is the part of it spent
// finding the session's user (withAuth). Handlers may add their own stages
// (timeStage), listed between the two.

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// serverTimingHeader: -server-timing.
var serverTimingHeader bool

// slowRequest: requests that take longer to answer are logged.
const slowRequest = 500 * time.Millisecond

type timingKey struct{}

// requestTiming is filled in as a request goes through the server.
type requestTiming struct {
	start  time.Time
	auth   time.Duration
	stages []stageTime
}

type stageTime struct {
	name string // a fixed token: our own names, never data
	dur  time.Duration
}

// timeStage times part of a handler for the Server-Timing header: call the
// function it returns when that part is done. name is a fixed token
// ([a-z-]), our own word for the stage. Only with -server-timing; it costs
// nothing otherwise.
func timeStage(r *http.Request, name string) func() {
	t, ok := r.Context().Value(timingKey{}).(*requestTiming)
	if !ok || !serverTimingHeader {
		return func() {}
	}
	start := time.Now()
	return func() { t.stages = append(t.stages, stageTime{name, time.Since(start)}) }
}

// noteAuthTime records how long the session lookup took (withAuth).
func noteAuthTime(r *http.Request, d time.Duration) {
	if t, ok := r.Context().Value(timingKey{}).(*requestTiming); ok {
		t.auth = d
	}
}

// serverTiming times every request: logged when slow, and with
// -server-timing, sent as the Server-Timing header.
func serverTiming(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := &requestTiming{start: time.Now()}
		tw := &timingWriter{ResponseWriter: w, t: t}
		h.ServeHTTP(tw, r.WithContext(context.WithValue(r.Context(), timingKey{}, t)))
		if !tw.wrote { // nothing written: the server answers 200 with no body
			tw.WriteHeader(http.StatusOK)
		}
		if tw.took >= slowRequest {
			log.Printf("slow request: %s %s %d in %v (session %v)", r.Method, r.URL.Path, tw.status, tw.took.Round(time.Millisecond), t.auth.Round(time.Millisecond))
		}
	})
}

// timingWriter sets the header when the response's headers go out.
type timingWriter struct {
	http.ResponseWriter
	t      *requestTiming
	wrote  bool
	status int
	took   time.Duration
}

func (w *timingWriter) WriteHeader(status int) {
	if !w.wrote {
		w.wrote, w.status = true, status
		w.took = time.Since(w.t.start)
		if serverTimingHeader {
			var b strings.Builder
			fmt.Fprintf(&b, "auth;dur=%.2f", durMS(w.t.auth))
			for _, s := range w.t.stages {
				fmt.Fprintf(&b, ", %s;dur=%.2f", s.name, durMS(s.dur))
			}
			fmt.Fprintf(&b, ", app;dur=%.2f", durMS(w.took))
			w.Header().Add("Server-Timing", b.String())
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *timingWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// ReadFrom keeps io.Copy's fast path (sendfile) for files served whole.
func (w *timingWriter) ReadFrom(src io.Reader) (int64, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(w.ResponseWriter, src)
}

func (w *timingWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection's own writer.
func (w *timingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func durMS(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
