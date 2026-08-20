package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func setupTestDB(t *testing.T) (*sql.DB, string) {
	tempDir, err := os.MkdirTemp("", "calibre-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	dbPath := filepath.Join(tempDir, "metadata.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}

	schema := `
	CREATE TABLE books (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		title TEXT NOT NULL DEFAULT 'Unknown',
		sort TEXT,
		author_sort TEXT,
		timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		pubdate TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		series_index REAL NOT NULL DEFAULT 1.0,
		isbn TEXT DEFAULT "",
		lccn TEXT DEFAULT "",
		path TEXT NOT NULL DEFAULT "",
		flags INTEGER NOT NULL DEFAULT 1,
		uuid TEXT,
		has_cover BOOL DEFAULT 0,
		last_modified TIMESTAMP NOT NULL DEFAULT "2000-01-01 00:00:00+00:00"
	);
	CREATE TABLE authors (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		sort TEXT,
		link TEXT NOT NULL DEFAULT ""
	);
	CREATE TABLE books_authors_link (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		book INTEGER NOT NULL,
		author INTEGER NOT NULL,
		UNIQUE(book, author)
	);
	CREATE TABLE tags (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE
	);
	CREATE TABLE books_tags_link (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		book INTEGER NOT NULL,
		tag INTEGER NOT NULL,
		UNIQUE(book, tag)
	);
	CREATE TABLE publishers (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		sort TEXT
	);
	CREATE TABLE books_publishers_link (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		book INTEGER NOT NULL,
		publisher INTEGER NOT NULL,
		UNIQUE(book, publisher)
	);
	CREATE TABLE series (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		sort TEXT
	);
	CREATE TABLE books_series_link (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		book INTEGER NOT NULL,
		series INTEGER NOT NULL,
		UNIQUE(book, series)
	);
	CREATE TABLE languages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		lang_code TEXT NOT NULL UNIQUE
	);
	CREATE TABLE books_languages_link (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		book INTEGER NOT NULL,
		lang_code INTEGER NOT NULL,
		item_order INTEGER NOT NULL DEFAULT 0,
		UNIQUE(book, lang_code)
	);
	CREATE TABLE identifiers (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		book INTEGER NOT NULL,
		type TEXT NOT NULL DEFAULT "isbn",
		val TEXT NOT NULL,
		UNIQUE(book, type)
	);
	CREATE TABLE comments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		book INTEGER NOT NULL,
		text TEXT NOT NULL,
		UNIQUE(book)
	);
	CREATE TABLE data (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		book INTEGER NOT NULL,
		format TEXT NOT NULL,
		uncompressed_size INTEGER NOT NULL,
		name TEXT NOT NULL
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("failed to create schema: %v", err)
	}

	return db, tempDir
}

func TestUpdateBookInCalibreDB_OrphanAuthorCleanup(t *testing.T) {
	db, tempDir := setupTestDB(t)
	defer os.RemoveAll(tempDir)
	defer db.Close()

	// Insert a single book with author "Unique Author"
	meta := &UploadedBookMeta{
		OPFMetadata: OPFMetadata{
			Title:    "Original Title",
			Creators: []string{"Unique Author"},
		},
	}
	formats := []formatEntry{}
	_, bookUUID, bookPath, err := insertBookIntoCalibreDB(db, meta, formats)
	if err != nil {
		t.Fatalf("insert book failed: %v", err)
	}

	// Create directory and dummy metadata.opf
	fullBookDir := filepath.Join(tempDir, bookPath)
	_ = os.MkdirAll(fullBookDir, 0755)

	// Verify "Unique Author" exists in authors
	var authorCount int
	err = db.QueryRow("SELECT COUNT(*) FROM authors WHERE name = 'Unique Author'").Scan(&authorCount)
	if err != nil || authorCount != 1 {
		t.Fatalf("expected 1 author 'Unique Author', got count %d, err: %v", authorCount, err)
	}

	// Update metadata: change author to "New Author"
	updateMeta := &OPFMetadata{
		Title:    "Updated Title",
		Creators: []string{"New Author"},
	}
	_, err = updateBookInCalibreDB(db, tempDir, bookUUID, updateMeta)
	if err != nil {
		t.Fatalf("updateBookInCalibreDB failed: %v", err)
	}

	// Verify "Unique Author" was cleanly deleted since no other book references it
	err = db.QueryRow("SELECT COUNT(*) FROM authors WHERE name = 'Unique Author'").Scan(&authorCount)
	if err != nil || authorCount != 0 {
		t.Errorf("expected 0 'Unique Author' (orphan deleted), got count %d", authorCount)
	}

	// Verify "New Author" exists
	err = db.QueryRow("SELECT COUNT(*) FROM authors WHERE name = 'New Author'").Scan(&authorCount)
	if err != nil || authorCount != 1 {
		t.Errorf("expected 1 'New Author', got count %d", authorCount)
	}

	// Verify books table has updated title and author_sort
	var title, authorSort string
	err = db.QueryRow("SELECT title, author_sort FROM books WHERE uuid = ?", bookUUID).Scan(&title, &authorSort)
	if err != nil {
		t.Fatalf("query books failed: %v", err)
	}
	if title != "Updated Title" {
		t.Errorf("expected title 'Updated Title', got %q", title)
	}
	if authorSort != "Author, New" {
		t.Errorf("expected authorSort 'Author, New', got %q", authorSort)
	}
}

func TestUpdateBookInCalibreDB_SharedAuthorPreserved(t *testing.T) {
	db, tempDir := setupTestDB(t)
	defer os.RemoveAll(tempDir)
	defer db.Close()

	// Insert Book 1 with "Shared Author"
	meta1 := &UploadedBookMeta{
		OPFMetadata: OPFMetadata{
			Title:    "Book One",
			Creators: []string{"Shared Author"},
		},
	}
	_, uuid1, path1, err := insertBookIntoCalibreDB(db, meta1, nil)
	if err != nil {
		t.Fatalf("insert book 1 failed: %v", err)
	}
	_ = os.MkdirAll(filepath.Join(tempDir, path1), 0755)

	// Insert Book 2 with "Shared Author"
	meta2 := &UploadedBookMeta{
		OPFMetadata: OPFMetadata{
			Title:    "Book Two",
			Creators: []string{"Shared Author"},
		},
	}
	_, uuid2, path2, err := insertBookIntoCalibreDB(db, meta2, nil)
	if err != nil {
		t.Fatalf("insert book 2 failed: %v", err)
	}
	_ = os.MkdirAll(filepath.Join(tempDir, path2), 0755)

	// Verify "Shared Author" count is 1 in authors table, linked to 2 books
	var authorCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM authors WHERE name = 'Shared Author'").Scan(&authorCount)
	if authorCount != 1 {
		t.Fatalf("expected 1 author record for 'Shared Author', got %d", authorCount)
	}

	// Update Book 1 to have "Solo Author" instead of "Shared Author"
	updateMeta := &OPFMetadata{
		Title:    "Book One Updated",
		Creators: []string{"Solo Author"},
	}
	_, err = updateBookInCalibreDB(db, tempDir, uuid1, updateMeta)
	if err != nil {
		t.Fatalf("update Book 1 failed: %v", err)
	}

	// Verify "Shared Author" is STILL PRESENT in authors because Book 2 is still linked to it!
	err = db.QueryRow("SELECT COUNT(*) FROM authors WHERE name = 'Shared Author'").Scan(&authorCount)
	if err != nil || authorCount != 1 {
		t.Errorf("expected 'Shared Author' to be preserved for Book 2, got count %d", authorCount)
	}

	// Verify "Solo Author" is created and linked to Book 1
	err = db.QueryRow("SELECT COUNT(*) FROM authors WHERE name = 'Solo Author'").Scan(&authorCount)
	if err != nil || authorCount != 1 {
		t.Errorf("expected 'Solo Author' to exist, got count %d", authorCount)
	}

	// Verify Book 2's link to Shared Author remains intact
	var b2Author string
	err = db.QueryRow(`
		SELECT a.name
		FROM books b
		JOIN books_authors_link bal ON bal.book = b.id
		JOIN authors a ON a.id = bal.author
		WHERE b.uuid = ?`, uuid2).Scan(&b2Author)
	if err != nil || b2Author != "Shared Author" {
		t.Errorf("expected Book 2 to still be linked to 'Shared Author', got %q (err: %v)", b2Author, err)
	}
}

func TestUpdateBookInCalibreDB_TagsPublishersCommentsIdentifiers(t *testing.T) {
	db, tempDir := setupTestDB(t)
	defer os.RemoveAll(tempDir)
	defer db.Close()

	meta := &UploadedBookMeta{
		OPFMetadata: OPFMetadata{
			Title:       "Original Title",
			Creators:    []string{"Author A"},
			Subjects:    []string{"TagToDrop", "TagToKeep"},
			Publisher:   "Old Publisher",
			Description: "<p>Original Description</p>",
			Language:    "eng",
			Identifiers: []opfIdentifier{
				{Scheme: "isbn", Value: "9781234567890"},
			},
		},
	}
	_, bookUUID, bookPath, err := insertBookIntoCalibreDB(db, meta, nil)
	if err != nil {
		t.Fatalf("insert book failed: %v", err)
	}
	fullBookDir := filepath.Join(tempDir, bookPath)
	_ = os.MkdirAll(fullBookDir, 0755)

	// Update metadata
	updateMeta := &OPFMetadata{
		Title:       "New Title",
		Creators:    []string{"Author A", "Author B"},
		Subjects:    []string{"TagToKeep", "BrandNewTag"},
		Publisher:   "New Publisher",
		Description: "<p>Updated Description</p>",
		Language:    "spa",
		Identifiers: []opfIdentifier{
			{Scheme: "isbn", Value: "9780987654321"},
			{Scheme: "google", Value: "google-id-123"},
		},
	}
	_, err = updateBookInCalibreDB(db, tempDir, bookUUID, updateMeta)
	if err != nil {
		t.Fatalf("updateBookInCalibreDB failed: %v", err)
	}

	// 1. Check orphan tag cleanup
	var tagCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM tags WHERE name = 'TagToDrop'").Scan(&tagCount)
	if tagCount != 0 {
		t.Errorf("expected 'TagToDrop' to be deleted, got %d", tagCount)
	}
	_ = db.QueryRow("SELECT COUNT(*) FROM tags WHERE name = 'TagToKeep'").Scan(&tagCount)
	if tagCount != 1 {
		t.Errorf("expected 'TagToKeep' to exist, got %d", tagCount)
	}
	_ = db.QueryRow("SELECT COUNT(*) FROM tags WHERE name = 'BrandNewTag'").Scan(&tagCount)
	if tagCount != 1 {
		t.Errorf("expected 'BrandNewTag' to exist, got %d", tagCount)
	}

	// 2. Check publisher cleanup
	var pubCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM publishers WHERE name = 'Old Publisher'").Scan(&pubCount)
	if pubCount != 0 {
		t.Errorf("expected 'Old Publisher' to be deleted, got %d", pubCount)
	}
	_ = db.QueryRow("SELECT COUNT(*) FROM publishers WHERE name = 'New Publisher'").Scan(&pubCount)
	if pubCount != 1 {
		t.Errorf("expected 'New Publisher' to exist, got %d", pubCount)
	}

	// 3. Check comments table
	var commentText string
	err = db.QueryRow(`
		SELECT text FROM comments c
		JOIN books b ON b.id = c.book
		WHERE b.uuid = ?`, bookUUID).Scan(&commentText)
	if err != nil || commentText != "<p>Updated Description</p>" {
		t.Errorf("expected comment '<p>Updated Description</p>', got %q (err: %v)", commentText, err)
	}

	// 4. Check identifiers
	var idCount int
	_ = db.QueryRow(`
		SELECT COUNT(*) FROM identifiers i
		JOIN books b ON b.id = i.book
		WHERE b.uuid = ?`, bookUUID).Scan(&idCount)
	if idCount != 2 {
		t.Errorf("expected 2 identifiers, got %d", idCount)
	}

	// 5. Check metadata.opf on disk
	opf, err := loadOPF(filepath.Join(tempDir, bookPath, "metadata.opf"))
	if err != nil {
		t.Fatalf("failed to read written metadata.opf: %v", err)
	}
	if opf.Metadata.Title != "New Title" {
		t.Errorf("expected OPF title 'New Title', got %q", opf.Metadata.Title)
	}
	if opf.Metadata.Publisher != "New Publisher" {
		t.Errorf("expected OPF publisher 'New Publisher', got %q", opf.Metadata.Publisher)
	}
}

func TestApplyMetadataHandler_HTTP(t *testing.T) {
	db, tempDir := setupTestDB(t)
	defer os.RemoveAll(tempDir)
	defer db.Close()

	origDB := calibreDB
	origBaseDir := baseDir
	calibreDB = db
	baseDir = tempDir
	defer func() {
		calibreDB = origDB
		baseDir = origBaseDir
	}()

	meta := &UploadedBookMeta{
		OPFMetadata: OPFMetadata{
			Title:    "HTTP Test Title",
			Creators: []string{"HTTP Author"},
		},
	}
	_, bookUUID, bookPath, err := insertBookIntoCalibreDB(db, meta, nil)
	if err != nil {
		t.Fatalf("insert failed: %v", err)
	}
	_ = os.MkdirAll(filepath.Join(tempDir, bookPath), 0755)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /metadata/{uuid}/apply", applyMetadataHandler)
	mux.HandleFunc("GET /metadata/{uuid}", GetOptionsForBook)

	server := httptest.NewServer(mux)
	defer server.Close()

	// Apply updated metadata
	payload := `{
		"title": "Updated HTTP Title",
		"creators": "First New Author, Second New Author",
		"publisher": "New HTTP Publisher",
		"date": "2024-01-15",
		"language": "eng",
		"subjects": "Golang, HTTP, Testing",
		"identifiers": "isbn: 9781112223334",
		"description": "A comprehensive guide to HTTP testing."
	}`

	resp, err := http.Post(server.URL+"/metadata/"+bookUUID+"/apply", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var resMap map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&resMap); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resMap["success"] != true {
		t.Errorf("expected success: true, got %v", resMap["success"])
	}

	// Verify old author was cleaned up
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM authors WHERE name = 'HTTP Author'").Scan(&count)
	if count != 0 {
		t.Errorf("expected old author 'HTTP Author' to be removed, count=%d", count)
	}

	// Verify new authors exist
	_ = db.QueryRow("SELECT COUNT(*) FROM authors WHERE name = 'First New Author'").Scan(&count)
	if count != 1 {
		t.Errorf("expected 'First New Author' to exist, count=%d", count)
	}

	// Verify GET /metadata/{uuid} returns the updated "current" metadata
	getResp, err := http.Get(server.URL + "/metadata/" + bookUUID)
	if err != nil {
		t.Fatalf("GET /metadata request failed: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metadata expected status 200, got %d", getResp.StatusCode)
	}

	var metas []*UploadedBookMeta
	if err := json.NewDecoder(getResp.Body).Decode(&metas); err != nil {
		t.Fatalf("failed to decode GET /metadata: %v", err)
	}
	if len(metas) == 0 {
		t.Fatalf("expected at least 1 meta in response, got 0")
	}
	current := metas[0]
	if current.Title != "Updated HTTP Title" {
		t.Errorf("expected Title 'Updated HTTP Title', got %q", current.Title)
	}
	if current.Publisher != "New HTTP Publisher" {
		t.Errorf("expected Publisher 'New HTTP Publisher', got %q", current.Publisher)
	}
}

