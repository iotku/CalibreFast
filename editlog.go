package main

// The edit log: every change made to the library from the web (by an
// editor or an admin) is a row of edit_log in the app DB, with who made it,
// when, and enough to undo it. That is what makes the editor role safe to
// hand out: a stolen or careless editor account can only make changes an
// admin can see at /admin/activity and undo, one at a time or all of one
// user's since a moment.
//
// How each kind of entry is undone (revertEntry):
//
//   - book: before/after are the book's metadata (bookSnap). Undoing puts
//     before back, if the book is still as after left it. A new book (an
//     upload, an import: before is NULL) goes to the Recycling Bin.
//   - cover: before/after name copies of cover.jpg kept in
//     <datadir>/edit-log/covers (by SHA-256); "" for no cover.
//   - placeholder: before/after are the placeholder (placeholderSnap), NULL
//     where there was none; placeholders are only ever soft-deleted.
//   - recycle: target is a Recycling Bin batch (a removal, a removed file,
//     or the books and files a merge or a new file replaced). Undoing
//     restores what of it is still in the bin.
//   - restore: books or files restored from the bin; undoing removes them
//     again (to a new batch).
//   - not-dup: a "not duplicates" marking made or unmarked.
//   - import: a book discarded from the import queue, whose folder was moved
//     to imports-discarded beside the queue's folder; undoing puts it back in the queue.
//
// Undoing never destroys anything either: what it replaces is in the log
// (as the undo's own entry) or the bin. An entry is only undone while
// nobody else changed the same thing since: a later entry of someone
// else's on the same target, or a book or placeholder no longer as the
// entry left it, is a conflict, reported and left alone.

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// editLogMu serializes undoing, so two admins can't undo the same entry
// twice at once.
var editLogMu sync.Mutex

const (
	logKindBook        = "book"
	logKindCover       = "cover"
	logKindPlaceholder = "placeholder"
	logKindRecycle     = "recycle"
	logKindRestore     = "restore"
	logKindNotDup      = "not-dup"
	logKindImport      = "import"
)

// editLogEntry is one row of edit_log (TS: ActivityEntry in
// web/admin-activity.ts).
type editLogEntry struct {
	ID         int64           `json:"id"`
	UserID     int64           `json:"user_id,omitempty"` // 0: account deleted
	Username   string          `json:"username"`
	At         int64           `json:"at"`
	Action     string          `json:"action"`
	Kind       string          `json:"kind"`
	Target     string          `json:"target"`
	Title      string          `json:"title"`
	Before     json.RawMessage `json:"before,omitempty"`
	After      json.RawMessage `json:"after,omitempty"`
	RevertedAt int64           `json:"reverted_at,omitempty"`
	RevertedBy string          `json:"reverted_by,omitempty"`
}

// Reverted reports whether the entry was undone (for the template).
func (e *editLogEntry) Reverted() bool { return e.RevertedAt != 0 }

// When is the entry's time, for the template.
func (e *editLogEntry) When() string { return time.Unix(e.At, 0).Format("2006-01-02 15:04:05") }

// Link is where the entry's target is seen, "" for none.
func (e *editLogEntry) Link() string {
	switch e.Kind {
	case logKindBook, logKindCover:
		return "/book/get/" + e.Target
	case logKindPlaceholder:
		return "/missing/" + e.Target
	case logKindRecycle, logKindRestore:
		return "/recycle-bin"
	}
	return ""
}

// logJSON is v as JSON for a before/after column: NULL for nil.
func logJSON(v any) any {
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil || string(data) == "null" {
		return nil
	}
	return string(data)
}

// logEdit records a change u made. A failure is logged, never returned:
// the change itself has been made.
func logEdit(u *User, action, kind, target, title string, before, after any) int64 {
	if appDB == nil {
		return 0
	}
	var uid any
	name := "(server)"
	if u != nil {
		name = u.Username
		if u.ID > 0 {
			uid = u.ID
		}
	}
	res, err := appDB.Exec(`INSERT INTO edit_log (user_id, username, at, action, kind, target, title, before, after) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uid, name, nowUnix(), action, kind, target, title, logJSON(before), logJSON(after))
	if err != nil {
		log.Printf("edit log: %s %s %s by %s: %v", action, kind, target, name, err)
		return 0
	}
	id, _ := res.LastInsertId()
	return id
}

const editLogCols = `id, COALESCE(user_id, 0), username, at, action, kind, target, title, before, after, COALESCE(reverted_at, 0), COALESCE(reverted_by, '')`

func scanEditLog(rows *sql.Rows) ([]editLogEntry, error) {
	defer rows.Close()
	out := []editLogEntry{}
	for rows.Next() {
		var e editLogEntry
		var before, after sql.NullString
		if err := rows.Scan(&e.ID, &e.UserID, &e.Username, &e.At, &e.Action, &e.Kind, &e.Target, &e.Title, &before, &after, &e.RevertedAt, &e.RevertedBy); err != nil {
			return nil, err
		}
		if before.Valid {
			e.Before = json.RawMessage(before.String)
		}
		if after.Valid {
			e.After = json.RawMessage(after.String)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// editLogQuery selects entries, newest first: by one user (userID > 0),
// since a moment (since > 0), at most limit (> 0).
func editLogQuery(userID, since int64, limit int) ([]editLogEntry, error) {
	q := `SELECT ` + editLogCols + ` FROM edit_log WHERE 1 = 1`
	var args []any
	if userID > 0 {
		q += ` AND user_id = ?`
		args = append(args, userID)
	}
	if since > 0 {
		q += ` AND at >= ?`
		args = append(args, since)
	}
	q += ` ORDER BY id DESC`
	if limit > 0 {
		q += ` LIMIT ` + strconv.Itoa(limit)
	}
	rows, err := appDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scanEditLog(rows)
}

func editLogByIDs(ids []int64) ([]editLogEntry, error) {
	if len(ids) == 0 {
		return []editLogEntry{}, nil
	}
	q := `SELECT ` + editLogCols + ` FROM edit_log WHERE id IN (?` + strings.Repeat(`, ?`, len(ids)-1) + `) ORDER BY id DESC`
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := appDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scanEditLog(rows)
}

// ---------------------------------------------------------------------------
// Books' metadata
// ---------------------------------------------------------------------------

// bookSnap is what an editor can change of a book's metadata
// (bookEditMetadata), as the log keeps it.
type bookSnap struct {
	Title       string          `json:"title"`
	Creators    []string        `json:"creators"`
	Description string          `json:"description"`
	Publisher   string          `json:"publisher"`
	Date        string          `json:"date"`
	Languages   []string        `json:"languages"`
	Subjects    []string        `json:"subjects"`
	Identifiers []opfIdentifier `json:"identifiers"`
	Series      string          `json:"series"`
	SeriesIndex string          `json:"series_index"`
	// Rating: nil in entries logged before ratings were editable, which an
	// undo then leaves alone.
	Rating *int `json:"rating,omitempty"`
}

// snapBook is the book's metadata now; nil if it isn't in the library.
func snapBook(uuid string) *bookSnap {
	m, err := bookEditMetadata(uuid)
	if err != nil {
		return nil
	}
	return &bookSnap{
		Title: m.Title, Creators: m.Creators, Description: m.Description, Publisher: m.Publisher,
		Date: m.Date, Languages: m.Languages, Subjects: m.Subjects, Identifiers: m.Identifiers,
		Series: m.Series, SeriesIndex: m.SeriesIndex, Rating: &m.Rating,
	}
}

// bookTitle is the book's title in metadata.db, "" if it isn't there.
func bookTitle(uuid string) string {
	var t string
	if calibreDB == nil {
		return ""
	}
	_ = calibreDB.QueryRow(`SELECT title FROM books WHERE uuid = ?`, uuid).Scan(&t)
	return t
}

func (s *bookSnap) metadata() OPFMetadata {
	m := OPFMetadata{
		Title: s.Title, Creators: slices.Clone(s.Creators), Description: s.Description, Publisher: s.Publisher,
		Date: s.Date, Languages: slices.Clone(s.Languages), Subjects: slices.Clone(s.Subjects),
		Identifiers: slices.Clone(s.Identifiers), Series: s.Series, SeriesIndex: s.SeriesIndex, SeriesSet: true,
	}
	if s.Rating != nil {
		m.Rating, m.RatingSet = *s.Rating, true
	}
	return m
}

// logBookEdit records a change to a book's metadata, given its metadata
// before (nil: a new book); the after is read now. Nothing is recorded
// for a change that changed nothing.
func logBookEdit(u *User, action, uuid string, before *bookSnap) {
	after := snapBook(uuid)
	if after == nil {
		return
	}
	if before != nil && bytes.Equal(mustJSONBytes(before), mustJSONBytes(after)) {
		return
	}
	logEdit(u, action, logKindBook, uuid, after.Title, before, after)
}

func mustJSONBytes(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

// sameJSON reports whether stored (a before/after column) is v.
func sameJSON(stored json.RawMessage, v any) bool {
	if v == nil || string(mustJSONBytes(v)) == "null" {
		return len(stored) == 0
	}
	return bytes.Equal(stored, mustJSONBytes(v))
}

// ---------------------------------------------------------------------------
// Covers
// ---------------------------------------------------------------------------

const editLogCoversDir = "edit-log/covers"

// snapCover keeps a copy of the book's cover.jpg (once per distinct image)
// and returns its name, "" when the book has no cover.
func snapCover(uuid string) (string, error) {
	path, err := resolveBookPath(uuid)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(baseDir, path, "cover.jpg"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:])
	dir := filepath.Join(dataDir, editLogCoversDir)
	dst := filepath.Join(dir, name+".jpg")
	if _, err := os.Stat(dst); err == nil {
		return name, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	return name, os.Rename(tmp, dst)
}

// coverSnapper keeps the book's cover before a change; done logs the change
// if the cover changed.
func coverSnapper(u *User, action, uuid, title string) (done func()) {
	before, err := snapCover(uuid)
	if err != nil {
		log.Printf("edit log: %s's cover: %v", uuid, err)
		return func() {}
	}
	return func() {
		after, err := snapCover(uuid)
		if err != nil {
			log.Printf("edit log: %s's cover: %v", uuid, err)
			return
		}
		if after != before {
			logEdit(u, action, logKindCover, uuid, title, before, after)
		}
	}
}

// setBookCover makes data the book's cover.jpg ("" data: no cover), as
// the cover dialog does: the old one is kept as cover_backup.jpg.
func setBookCover(uuid string, data []byte) error {
	bookPath, err := resolveBookPath(uuid)
	if err != nil {
		return errNoSuchBook
	}
	dir := filepath.Join(baseDir, bookPath)
	stamp, _ := bookCoverStamp(uuid)
	if data == nil {
		cover := filepath.Join(dir, "cover.jpg")
		if err := copyFile(cover, filepath.Join(dir, "cover_backup.jpg")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.Remove(cover); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if _, err := calibreDB.Exec(`UPDATE books SET has_cover = 0 WHERE uuid = ?`, uuid); err != nil {
			return err
		}
	} else {
		if _, err := saveCoverJPEG(dir, data); err != nil {
			return err
		}
		if _, err := calibreDB.Exec(`UPDATE books SET has_cover = 1 WHERE uuid = ?`, uuid); err != nil {
			return err
		}
	}
	if err := os.Remove(thumbCachePath(uuid, stamp)); err != nil && !os.IsNotExist(err) {
		log.Printf("cover %s: couldn't remove the old thumbnail: %v", uuid, err)
	}
	coverChanged(uuid)
	return nil
}

// ---------------------------------------------------------------------------
// Placeholders
// ---------------------------------------------------------------------------

// placeholderSnap is a placeholder as the log keeps it.
type placeholderSnap struct {
	Meta  OPFMetadata `json:"meta"`
	Cover string      `json:"cover"`
}

func snapPlaceholder(p *placeholder) *placeholderSnap {
	if p == nil {
		return nil
	}
	return &placeholderSnap{Meta: p.Meta, Cover: p.Cover}
}

// currentPlaceholderSnap is the placeholder now, nil if removed or unknown.
func currentPlaceholderSnap(id string) *placeholderSnap {
	return snapPlaceholder(placeholderByUUID(id))
}

// putPlaceholder makes the placeholder id what s says (nil: removed).
func putPlaceholder(id string, s *placeholderSnap) error {
	old := placeholderByUUID(id)
	if old != nil {
		defer gapsPlaceholderChanged(old.Meta.Series)
	}
	defer placeholdersChanged()
	if s == nil {
		_, err := appDB.Exec(`UPDATE placeholders SET deleted_at = ? WHERE uuid = ?`, nowUnix(), id)
		return err
	}
	defer gapsPlaceholderChanged(s.Meta.Series)
	meta, err := json.Marshal(s.Meta)
	if err != nil {
		return err
	}
	res, err := appDB.Exec(`UPDATE placeholders SET meta = ?, cover = ?, deleted_at = NULL, replaced_by = '', replaced_why = '', replaced_user = '', updated_at = ? WHERE uuid = ?`,
		string(meta), s.Cover, nowUnix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		now := nowUnix()
		_, err = appDB.Exec(`INSERT INTO placeholders (uuid, meta, cover, source, created_at, updated_at) VALUES (?, ?, ?, 'manual', ?, ?)`,
			id, string(meta), s.Cover, now, now)
	}
	return err
}

// ---------------------------------------------------------------------------
// Undoing
// ---------------------------------------------------------------------------

// revertResult is what undoing one entry came to (TS: RevertResult).
type revertResult struct {
	ID     int64  `json:"id"`
	Action string `json:"action"`
	Title  string `json:"title"`
	Done   bool   `json:"done"`
	Error  string `json:"error,omitempty"`
}

var errRevertConflict = errors.New("changed since by someone else")

// revertEntries undoes entries, newest first (each undoing leaves things
// as the next older one left them).
func revertEntries(entries []editLogEntry, by *User) []revertResult {
	editLogMu.Lock()
	defer editLogMu.Unlock()
	slices.SortFunc(entries, func(a, b editLogEntry) int { return int(b.ID - a.ID) })
	out := []revertResult{}
	for i := range entries {
		e := &entries[i]
		r := revertResult{ID: e.ID, Action: e.Action, Title: e.Title}
		var err error
		switch {
		case e.Reverted():
			err = errors.New("already undone")
		default:
			err = revertEntry(e, by)
		}
		if err != nil {
			r.Error = err.Error()
		} else {
			r.Done = true
			if _, err := appDB.Exec(`UPDATE edit_log SET reverted_at = ?, reverted_by = ? WHERE id = ?`, nowUnix(), by.Username, e.ID); err != nil {
				log.Printf("edit log: marking %d undone: %v", e.ID, err)
			}
			log.Printf("edit log: %s undid %s's %s of %q (#%d)", by.Username, e.Username, e.Action, e.Title, e.ID)
		}
		out = append(out, r)
	}
	return out
}

// changedSinceByOthers: a later entry, not undone, of someone else's on the
// same thing. Undoings don't count: an undo of a later entry of this
// user's left things as this entry did.
func changedSinceByOthers(e *editLogEntry) bool {
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM edit_log WHERE kind = ? AND target = ? AND id > ? AND reverted_at IS NULL AND username <> ? AND action NOT LIKE 'undo #%'`,
		e.Kind, e.Target, e.ID, e.Username).Scan(&n)
	return n > 0
}

func revertEntry(e *editLogEntry, by *User) error {
	if e.Target == "" {
		return errors.New("nothing to undo")
	}
	if changedSinceByOthers(e) {
		return errRevertConflict
	}
	undo := "undo #" + strconv.FormatInt(e.ID, 10)
	switch e.Kind {
	case logKindBook:
		return revertBook(e, by, undo)
	case logKindCover:
		return revertCover(e, by, undo)
	case logKindPlaceholder:
		return revertPlaceholder(e, by, undo)
	case logKindRecycle:
		return revertRecycle(e, by, undo)
	case logKindRestore:
		return revertRestore(e, by, undo)
	case logKindNotDup:
		return revertNotDup(e, by, undo)
	case logKindImport:
		return revertImportDiscard(e, by)
	}
	return fmt.Errorf("can't undo a %q", e.Action)
}

func revertBook(e *editLogEntry, by *User, undo string) error {
	var after bookSnap
	if err := json.Unmarshal(e.After, &after); err != nil {
		return err
	}
	now := snapBook(e.Target)
	if now == nil {
		return errors.New("the book isn't in the library")
	}
	cmpNow := *now
	if after.Rating == nil {
		cmpNow.Rating = nil // logged before ratings were kept: not part of the check
	}
	if !sameJSON(e.After, &cmpNow) {
		return errRevertConflict
	}
	if len(e.Before) == 0 { // a new book: to the bin
		batch, err := recycleBooks([]string{e.Target}, by.Username)
		if err != nil {
			return err
		}
		logEdit(by, undo, logKindRecycle, batch.ID, e.Title, nil, nil)
		return nil
	}
	var before bookSnap
	if err := json.Unmarshal(e.Before, &before); err != nil {
		return err
	}
	bulkMu.Lock()
	defer bulkMu.Unlock()
	m := before.metadata()
	if _, err := updateBookInCalibreDB(calibreDB, baseDir, e.Target, &m); err != nil {
		return err
	}
	bookMetadataCache.Delete(e.Target)
	catalogChanged(e.Target)
	refreshPagesFor(e.Target)
	refreshAllCaches()
	logBookEdit(by, undo, e.Target, now)
	return nil
}

func revertCover(e *editLogEntry, by *User, undo string) error {
	var before, after string
	if err := json.Unmarshal(e.Before, &before); err != nil {
		return err
	}
	if err := json.Unmarshal(e.After, &after); err != nil {
		return err
	}
	now, err := snapCover(e.Target)
	if err != nil {
		return err
	}
	if now != after {
		return errRevertConflict
	}
	var data []byte
	if before != "" {
		if data, err = os.ReadFile(filepath.Join(dataDir, editLogCoversDir, before+".jpg")); err != nil {
			return fmt.Errorf("the earlier cover is gone: %w", err)
		}
	}
	if err := setBookCover(e.Target, data); err != nil {
		return err
	}
	logEdit(by, undo, logKindCover, e.Target, e.Title, now, before)
	return nil
}

func revertPlaceholder(e *editLogEntry, by *User, undo string) error {
	now := currentPlaceholderSnap(e.Target)
	if !sameJSON(e.After, now) {
		return errRevertConflict
	}
	var before *placeholderSnap
	if len(e.Before) > 0 {
		before = &placeholderSnap{}
		if err := json.Unmarshal(e.Before, before); err != nil {
			return err
		}
	}
	if err := putPlaceholder(e.Target, before); err != nil {
		return err
	}
	logEdit(by, undo, logKindPlaceholder, e.Target, e.Title, now, before)
	return nil
}

func revertRecycle(e *editLogEntry, by *User, undo string) error {
	// A file a merge (or a new file) replaced comes back in place of the
	// one that replaced it, which goes to a batch of its own.
	recycleMu.Lock()
	batch, err := loadRecycleBatch(e.Target)
	recycleMu.Unlock()
	if err != nil {
		return err
	}
	for i := range batch.Formats {
		f := &batch.Formats[i]
		if !f.Replaced || f.RestoredAt != "" || !batch.formatInBin(f) {
			continue
		}
		var n int
		_ = calibreDB.QueryRow(`SELECT COUNT(*) FROM data d JOIN books b ON b.id = d.book WHERE b.uuid = ? AND lower(d.format) = lower(?)`, f.UUID, f.Format).Scan(&n)
		if n == 0 {
			continue
		}
		swapped, err := recycleFormatFile(f.UUID, f.Format, by.Username, true)
		if err != nil {
			return fmt.Errorf("%s's %s: %w", f.Title, f.Format, err)
		}
		logEdit(by, undo, logKindRecycle, swapped.ID, f.Title, nil, nil)
	}
	results, err := restoreFromBin(e.Target, nil, nil, by.Username)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range results {
		if r.Error != "" {
			errs = append(errs, fmt.Errorf("%s: %s", r.Title, r.Error))
		}
	}
	logRestore(by, undo, e.Target, results)
	return errors.Join(errs...)
}

// restoredItems is a restore entry's after: what came back from the bin.
type restoredItems struct {
	UUIDs   []string `json:"uuids,omitempty"`
	Formats []string `json:"formats,omitempty"` // "<uuid>/<FORMAT>"
}

func revertRestore(e *editLogEntry, by *User, undo string) error {
	var items restoredItems
	if err := json.Unmarshal(e.After, &items); err != nil {
		return err
	}
	var errs []error
	var still []string
	for _, u := range items.UUIDs {
		if _, err := resolveBookPath(u); err == nil {
			still = append(still, u)
		}
	}
	if len(still) > 0 {
		batch, err := recycleBooks(still, by.Username)
		if err != nil {
			errs = append(errs, err)
		} else {
			logEdit(by, undo, logKindRecycle, batch.ID, e.Title, nil, nil)
		}
	}
	for _, k := range items.Formats {
		u, f, _ := strings.Cut(k, "/")
		batch, err := recycleFormat(u, strings.ToLower(f), by.Username)
		switch {
		case errors.Is(err, errNoSuchBook), errors.Is(err, errNoSuchFormat):
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", k, err))
		default:
			logEdit(by, undo, logKindRecycle, batch.ID, e.Title, nil, nil)
		}
	}
	return errors.Join(errs...)
}

func revertNotDup(e *editLogEntry, by *User, undo string) error {
	if len(e.Before) == 0 { // a marking: unmark it
		var set notDuplicateSet
		if err := json.Unmarshal(e.After, &set); err != nil {
			return err
		}
		if _, err := appDB.Exec(`DELETE FROM not_duplicate_sets WHERE id = ?`, set.ID); err != nil {
			return err
		}
		logEdit(by, undo, logKindNotDup, e.Target, e.Title, set, nil)
		return nil
	}
	var set notDuplicateSet
	if err := json.Unmarshal(e.Before, &set); err != nil {
		return err
	}
	id, err := markNotDuplicates(set.UUIDs, by.Username)
	if err != nil {
		return err
	}
	set.ID = id
	logEdit(by, undo, logKindNotDup, strconv.FormatInt(id, 10), e.Title, nil, set)
	return nil
}

// discardedDir is where a book discarded from the queue goes: beside the
// queue's folder (<datadir>/imports-discarded, or -import-dir's name with
// "-discarded"), so on the same drive.
func (b *importInbox) discardedDir() string { return b.dir + "-discarded" }

func revertImportDiscard(e *editLogEntry, by *User) error {
	if imports == nil || !isSafeUUIDComponent(e.Target) {
		return errors.New("the import queue isn't available")
	}
	src := filepath.Join(imports.discardedDir(), e.Target)
	dst := imports.itemDir(e.Target)
	if _, err := os.Stat(dst); err == nil {
		return errors.New("it's in the queue already")
	}
	if err := moveDir(src, dst); err != nil {
		return err
	}
	it, err := loadPendingImport(dst, e.Target)
	if err != nil {
		return err
	}
	imports.put(it)
	return nil
}

// ---------------------------------------------------------------------------
// The admin's activity page
// ---------------------------------------------------------------------------

const activityPageSize = 200

type activityPageData struct {
	Title   string
	Users   []User
	User    int64 // shown: one user's, 0 for everyone's
	Entries []editLogEntry
	Me      *User
}

func activityPage(w http.ResponseWriter, r *http.Request) {
	users, err := listUsers()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	uid, _ := strconv.ParseInt(r.URL.Query().Get("user"), 10, 64)
	entries, err := editLogQuery(uid, 0, activityPageSize)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "admin-activity.html", activityPageData{
		Title: SiteTitle, Users: users, User: uid, Entries: entries, Me: currentUser(r),
	}), "render admin-activity")
}

// GET /api/admin/activity?user=ID&since=UNIX&limit=N
func activityHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	uid, _ := strconv.ParseInt(q.Get("user"), 10, 64)
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = activityPageSize
	}
	entries, err := editLogQuery(uid, since, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// activityRevertRequest names entries to undo: by id, or all of one user's
// since a moment (unix seconds).
type activityRevertRequest struct {
	IDs    []int64 `json:"ids"`
	UserID int64   `json:"user_id"`
	Since  int64   `json:"since"`
}

// POST /api/admin/activity/revert
func activityRevertHandler(w http.ResponseWriter, r *http.Request) {
	var req activityRevertRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	var entries []editLogEntry
	var err error
	switch {
	case len(req.IDs) > 0:
		if len(req.IDs) > 1000 {
			http.Error(w, "at most 1000 at a time", http.StatusBadRequest)
			return
		}
		entries, err = editLogByIDs(req.IDs)
	case req.UserID > 0 && req.Since > 0:
		entries, err = editLogQuery(req.UserID, req.Since, 0)
	default:
		http.Error(w, "name entries, or a user and a time", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": revertEntries(entries, currentUser(r))})
}

// summary names a batch's books for the log: a title, or "N books".
func (b *recycleBatch) summary() string {
	switch {
	case len(b.Books) == 1:
		return b.Books[0].Title
	case len(b.Books) > 1:
		return fmt.Sprintf("%s and %d more", b.Books[0].Title, len(b.Books)-1)
	case len(b.Formats) > 0:
		return b.Formats[0].Title
	}
	return ""
}

// logRestore records what a restore from batch brought back.
func logRestore(u *User, action, batch string, results []restoreResult) {
	var items restoredItems
	title := ""
	for _, r := range results {
		if r.Error != "" {
			continue
		}
		if title == "" {
			title = r.Title
		}
		if r.Format != "" {
			items.Formats = append(items.Formats, r.UUID+"/"+r.Format)
		} else {
			items.UUIDs = append(items.UUIDs, r.UUID)
		}
	}
	if len(items.UUIDs)+len(items.Formats) > 0 {
		logEdit(u, action, logKindRestore, batch, title, nil, items)
	}
}

// logMerge records a merge (or a new file added to a book): the kept
// book's metadata and cover, and the batch holding the books and files it
// replaced.
func logMerge(u *User, action string, res *mergeResult, before *bookSnap, coverDone func()) {
	if before != nil {
		logBookEdit(u, action, res.UUID, before)
	}
	coverDone()
	if res.Batch != "" {
		logEdit(u, action, logKindRecycle, res.Batch, res.Title, nil, nil)
	}
}
