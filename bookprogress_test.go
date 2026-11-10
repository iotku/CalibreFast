package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// minimalPDF is a valid PDF with n empty pages.
func minimalPDF(n int) []byte {
	var b bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	b.WriteString("%PDF-1.4\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	kids := make([]string, n)
	for i := range kids {
		kids[i] = fmt.Sprintf("%d 0 R", i+3)
	}
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	for range n {
		obj("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>")
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}

func TestTimeAgo(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		3 * time.Second:     "just now",
		5 * time.Second:     "just now",
		12 * time.Second:    "12 seconds ago",
		time.Minute:         "1 minute ago",
		59 * time.Minute:    "59 minutes ago",
		3 * time.Hour:       "3 hours ago",
		30 * time.Hour:      "yesterday",
		5 * 24 * time.Hour:  "5 days ago",
		40 * 24 * time.Hour: "on 2 Jan 2026",
	} {
		if got := timeAgo(d, at); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

func TestBookProgress(t *testing.T) {
	u, books := setupLibraryAndUser(t, 3)
	epub, pdf, other := books[0], books[1], books[2]
	addBookFile(t, epub, "Novel", "epub", testEPUB(t))
	addBookFile(t, pdf, "Scan", "pdf", minimalPDF(3))
	now := time.Now()
	ms := func(ago time.Duration) int64 { return now.Add(-ago).UnixMilli() }

	// Web reader positions…
	for _, p := range []struct {
		uuid, format, pos string
		pct               float64
		at                int64
	}{
		{epub, "epub", "epubcfi(/6/6!/4/4/8)", 0.25, ms(time.Hour)},
		{pdf, "pdf", "2", 0.66, ms(2 * time.Hour)},
		{other, "epub", "epubcfi(/6/2!/4)", 0, ms(time.Minute)},
	} {
		if _, err := appDB.Exec(`INSERT INTO reading_progress (user_id, book_uuid, format, position, percent, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, u.ID, p.uuid, p.format, p.pos, p.pct, p.at); err != nil {
			t.Fatal(err)
		}
	}
	// …and a newer one from a device, matched by file name; a device
	// position matching nothing is left out.
	for _, d := range []struct {
		doc, progress string
		pct           float64
		at            int64
	}{
		{fileNameDigest("Novel.epub"), "/body/DocFragment[3]/body/section/p[2]", 0.42, ms(2 * time.Minute)},
		{md5Hex("elsewhere"), "1", 0.9, ms(time.Second)},
	} {
		if _, err := appDB.Exec(`INSERT INTO device_progress (user_id, document, progress, percentage, device, updated_at)
			VALUES (?, ?, ?, ?, 'Kobo Clara', ?)`, u.ID, d.doc, d.progress, d.pct, d.at); err != nil {
			t.Fatal(err)
		}
	}

	all := latestProgress(u.ID, "")
	if p := all[epub]; p.Device != "Kobo Clara" || p.Percent != 0.42 || p.Format != "epub" {
		t.Fatalf("epub: newest should be the device's: %+v", p)
	}
	if p := all[pdf]; p.Device != "" || p.Position != "2" {
		t.Fatalf("pdf: %+v", p)
	}
	if len(all) != 3 {
		t.Fatalf("unmatched device position counted: %v", all)
	}
	if one := latestProgress(u.ID, pdf); len(one) != 1 || one[pdf].Position != "2" {
		t.Fatalf("one book: %v", one)
	}

	// The popup's note.
	if got := progressNote(epub, all[epub], now); got != "42% · synced 2 minutes ago from Kobo Clara" {
		t.Errorf("epub note: %q", got)
	}
	if got := progressNote(pdf, all[pdf], now); got != "Page 2 of 3 · synced 2 hours ago" {
		t.Errorf("pdf note: %q", got)
	}
	if got := progressNote(other, all[other], now); got != "Started · synced 1 minute ago" {
		t.Errorf("no percentage yet: %q", got)
	}

	// The grid's list: books being read, with how far along.
	for _, b := range []string{epub, other} {
		_ = setStatus(u.ID, b, "reading", "auto")
	}
	_ = setStatus(u.ID, pdf, "finished", "manual")
	var reading struct{ Reading map[string]*float64 }
	rec := call(t, userActionsMux(), u, "GET", "/api/me/reading", "")
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &reading) != nil {
		t.Fatalf("reading: %d %s", rec.Code, rec.Body)
	}
	if len(reading.Reading) != 2 || reading.Reading[epub] == nil || *reading.Reading[epub] != 0.42 ||
		reading.Reading[other] == nil || *reading.Reading[other] != 0 {
		t.Fatalf("reading: %s", rec.Body)
	}
	// A book marked as reading by hand, with no position: listed, progress unknown.
	_ = setStatus(u.ID, pdf, "reading", "manual")
	_, _ = appDB.Exec(`DELETE FROM reading_progress WHERE book_uuid = ?`, pdf)
	rec = call(t, userActionsMux(), u, "GET", "/api/me/reading", "")
	if !strings.Contains(rec.Body.String(), `"`+pdf+`":null`) {
		t.Fatalf("no position: %s", rec.Body)
	}
	if rec := call(t, userActionsMux(), nil, "GET", "/api/me/reading", ""); rec.Code != 401 {
		t.Fatalf("anonymous: %d", rec.Code)
	}

	// The popup shows the note instead of "set by the reader".
	html := call(t, userActionsMux(), u, "GET", "/book/me/"+epub, "").Body.String()
	if !strings.Contains(html, "42% · synced 2 minutes ago from Kobo Clara") || strings.Contains(html, "set by the reader") {
		t.Fatalf("popup: %s", html)
	}
}

func userActionsMux() *http.ServeMux {
	mux := kosyncTestMux()
	mux.HandleFunc("GET /book/me/{uuid}", bookUserActionsHandler)
	mux.HandleFunc("GET /api/me/reading", requireUser(readingHandler))
	return mux
}
