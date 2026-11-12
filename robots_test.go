package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setRobotsFlags(t *testing.T, read, robots bool) {
	t.Helper()
	origRead, origCat, origRobots := publicRead, publicCatalog, allowRobots
	t.Cleanup(func() { publicRead, publicCatalog, allowRobots = origRead, origCat, origRobots })
	publicRead, publicCatalog, allowRobots = read, read, robots
}

// Only -public-read together with -robots-allowed lets search engines in.
func TestRobots(t *testing.T) {
	setupTestAppDB(t)
	mux := http.NewServeMux()
	withMux(mux, func() {
		http.HandleFunc("/", indexHandler)
		registerRobotsRoutes()
	})
	h := robotsHeader(withAuth(mux))
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	for _, c := range []struct{ read, robots, allowed bool }{
		{false, false, false},
		{false, true, false}, // -robots-allowed alone does nothing
		{true, false, false},
		{true, true, true},
	} {
		setRobotsFlags(t, c.read, c.robots)

		// robots.txt is served to anonymous visitors even on a private server.
		rec := get("/robots.txt")
		want := robotsDeny
		if c.allowed {
			want = robotsAllow
		}
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("%+v robots.txt: %d %q, want %q", c, rec.Code, rec.Body.String(), want)
		}

		// The header is on everything, even a refused request or a 404.
		for _, p := range []string{"/robots.txt", "/", "/no-such-page"} {
			got := get(p).Header().Get("X-Robots-Tag")
			if c.allowed != (got == "") {
				t.Errorf("%+v %s: X-Robots-Tag %q", c, p, got)
			}
		}
		if c.read {
			body := get("/").Body.String()
			if c.allowed == strings.Contains(body, `name="robots"`) {
				t.Errorf("%+v: library page robots meta wrong", c)
			}
		}
	}
	if strings.Contains(robotsDeny, "/admin") || strings.Count(robotsDeny, "Disallow") != 1 {
		t.Errorf("deny-all robots.txt should name no paths: %q", robotsDeny)
	}
}

// Every page template has the robots meta tag.
func TestTemplatesHaveRobotsMeta(t *testing.T) {
	files, _ := filepath.Glob("templates/*.html")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if !strings.Contains(strings.ToLower(s), "<meta charset") {
			continue // a partial, or a page that takes its head from one
		}
		if !strings.Contains(s, "{{ robotsMeta }}") && !strings.Contains(s, `name="robots" content="noindex, nofollow"`) {
			t.Errorf("%s has no robots meta tag", f)
		}
	}
}
