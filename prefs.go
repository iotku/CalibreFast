package main

// Preferences (/preferences, web/preferences.ts). Those that follow the
// account to every device are kept here, in user_prefs, and sent with
// /api/me; those about one device (how long books stay in its storage, how
// much it may use) stay in that browser (web/prefs.ts).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// userPrefs: the account's preferences, as JSON in user_prefs.prefs.
type userPrefs struct {
	// Format a book with both an EPUB and a PDF opens in: "last" (the one
	// last read), "epub" or "pdf".
	Format string `json:"format"`
}

var prefFormats = []string{"last", "epub", "pdf"}

func defaultPrefs() userPrefs { return userPrefs{Format: "last"} }

func validPrefs(p userPrefs) bool {
	return slices.Contains(prefFormats, p.Format)
}

// userPrefsFor: u's preferences, the defaults where none are saved (or
// saved ones are no longer valid).
func userPrefsFor(u *User) (userPrefs, error) {
	var raw string
	err := appDB.QueryRow(`SELECT prefs FROM user_prefs WHERE user_id = ?`, u.ID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultPrefs(), nil
	}
	if err != nil {
		return defaultPrefs(), err
	}
	p := defaultPrefs()
	if json.Unmarshal([]byte(raw), &p) != nil || !validPrefs(p) {
		return defaultPrefs(), nil
	}
	return p, nil
}

// PUT /api/me/prefs: replace the account's preferences.
func putPrefsHandler(w http.ResponseWriter, r *http.Request) {
	p := defaultPrefs()
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&p); err != nil || !validPrefs(p) {
		http.Error(w, "invalid preferences", http.StatusBadRequest)
		return
	}
	js, err := json.Marshal(p)
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if _, err := appDB.Exec(`INSERT INTO user_prefs (user_id, prefs) VALUES (?, ?)
		ON CONFLICT (user_id) DO UPDATE SET prefs = excluded.prefs`, currentUser(r).ID, string(js)); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// savedBook: a book to download ahead of time: On Deck, or on the user's
// Saved list and when it was saved.
type savedBook struct {
	UUID    string `json:"uuid"`
	Title   string `json:"title"`
	Authors string `json:"authors"`
	SavedAt int64  `json:"saved_at,omitempty"` // unix ms; none for On Deck
	// OnDeck: the next book of a series the user finished one of
	// (series_pins.go), which has no status of its own.
	OnDeck bool `json:"on_deck,omitempty"`
}

// maxSavedBooks: the most /api/me/saved-books lists in one answer. Any
// number of books can be saved; a device only downloads a few (web/prefs.ts
// offers up to 50), so it asks for just those (?limit=).
const maxSavedBooks = 500

// GET /api/me/saved-books?limit=N: the first N books of Discover's "On deck
// / Saved for later" row, for the books kept on a device ahead of time
// (web/book-sync.ts): the On Deck books (the next in the series the user
// finished one of), then the Saved list in its page's order (oldest saved
// first unless arranged, list_order.go). Oldest, so saving more books later
// doesn't swap one download for another. Books not in the library yet
// (placeholders) are left out: nothing to download. Tagged: 304 when the
// device's copy is current.
func savedBooksHandler(w http.ResponseWriter, r *http.Request) {
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit < 1 || limit > maxSavedBooks {
		http.Error(w, "limit: 1 to "+strconv.Itoa(maxSavedBooks), http.StatusBadRequest)
		return
	}
	u := currentUser(r)
	marks, active, err := userReading(u)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	deck, err := onDeck(u, marks, active)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	out := make([]savedBook, 0, limit)
	listed := map[string]bool{}
	for _, b := range deck {
		if len(out) == limit {
			break
		}
		if b.Placeholder != nil || b.SeriesCard != nil {
			continue
		}
		listed[b.UUID] = true
		out = append(out, savedBook{UUID: b.UUID, Title: b.Title, Authors: b.Authors, OnDeck: true})
	}
	rows, err := appDB.Query(`SELECT book_uuid, updated_at FROM reading_status
		WHERE user_id = ? AND status = 'wanted' `+wantedOrder+` LIMIT ?`, u.ID, limit)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	var uuids []string
	at := map[string]int64{}
	for rows.Next() {
		var uuid string
		var t int64
		if rows.Scan(&uuid, &t) == nil && !listed[uuid] {
			uuids = append(uuids, uuid)
			at[uuid] = t * 1000
		}
	}
	rows.Close()
	books, err := booksByUUIDs(uuids)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	for _, b := range books {
		if len(out) == limit {
			break
		}
		if b.Placeholder != nil || b.SeriesCard != nil {
			continue // nothing to download
		}
		out = append(out, savedBook{UUID: b.UUID, Title: b.Title, Authors: b.Authors, SavedAt: at[b.UUID]})
	}
	writeJSONTagged(w, r, out)
}

// GET /preferences: the page; open to guests, whose preferences are all
// kept in their browser.
func preferencesPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "preferences.html", map[string]any{"Title": SiteTitle}), "render preferences")
}

// newAccountCookie marks a browser that has just made an account, so the
// preferences it had while signed out carry over to it (web/prefs.ts reads
// and clears it). Signing in to an existing account never sets it: that
// account's own preferences stand. Not a secret, so script may read it.
const newAccountCookie = "cf-new-account"

func markNewAccount(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: newAccountCookie, Value: "1", Path: "/", MaxAge: int((10 * time.Minute).Seconds()),
		Secure: secureCookies(r), SameSite: http.SameSiteLaxMode,
	})
}

func registerPrefsRoutes() {
	http.HandleFunc("GET /preferences", preferencesPage)
	http.HandleFunc("PUT /api/me/prefs", requireUser(putPrefsHandler))
	http.HandleFunc("GET /api/me/saved-books", requireUser(savedBooksHandler))
}
