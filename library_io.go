package main

// libraryIO counts operations on the library: statements prepared against
// metadata.db (through the sqlite3_calibre driver) and the stat, open and
// directory reads of the hot paths under -basedir. The library may be on
// slow or network storage, so page loads should leave it at zero; tests
// check that (docs/notes/Discover.md, "Storage model").

import (
	"os"
	"sync/atomic"

	"github.com/mattn/go-sqlite3"
)

var libraryIO atomic.Int64

// countLibraryStatements is the sqlite3_calibre connections' authorizer,
// which SQLite calls while preparing each statement: one count per
// statement (its SELECT, INSERT, UPDATE or DELETE), never refusing.
func countLibraryStatements(op int, _, _, _ string) int {
	switch op {
	case sqlite3.SQLITE_SELECT, sqlite3.SQLITE_INSERT, sqlite3.SQLITE_UPDATE, sqlite3.SQLITE_DELETE:
		libraryIO.Add(1)
	}
	return sqlite3.SQLITE_OK
}

func libraryStat(path string) (os.FileInfo, error) {
	libraryIO.Add(1)
	return os.Stat(path)
}

func libraryReadDir(path string) ([]os.DirEntry, error) {
	libraryIO.Add(1)
	return os.ReadDir(path)
}

func libraryOpen(path string) (*os.File, error) {
	libraryIO.Add(1)
	return os.Open(path)
}

func libraryReadFile(path string) ([]byte, error) {
	libraryIO.Add(1)
	return os.ReadFile(path)
}
