package main

// shelfMark: a shelf and whether a given book is on it (for the book popup).
type shelfMark struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Global  bool   `json:"global"`
	Has     bool   `json:"has"`
	CanEdit bool   `json:"can_edit"`
}

// shelfMarks lists the shelves a user can add this book to (filled in by the
// bookshelves feature).
func shelfMarks(u *User, uuid string) []shelfMark { return []shelfMark{} }
