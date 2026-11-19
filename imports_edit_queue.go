package main

// Saving quick edits of waiting books (imports_edit.go) without waiting for
// the import queue's folder, which -import-dir may put on a slow disk.
//
// An edit is committed to calibrefast.db (import_edit_journal: the book's
// Merged metadata as the edit left it) and put in importInbox.items, which
// everything reads waiting books from, before the editor is answered. A
// writer then brings each book's item.json up to date, one at a time,
// holding importInbox.flushMu but not mu, so the queue keeps answering. A
// journal row goes once its book's item.json says the same.
//
// Nothing writes over a newer state:
//   - item.json is only ever written whole from items (or from the book
//     being discarded), so the last write is the newest state.
//   - Taking a book (adding, discarding, splitting) holds flushMu, so no
//     write of it is under way or starts while it's out. Discarding saves
//     its edits into its item.json first (it can be restored), and fails if
//     it can't; adding uses the metadata in memory, then drops the row;
//     splitting starts both books again from their files, and drops it.
//   - On start (replayImportEdits), a row for a book in the queue puts its
//     Merged back over what item.json says, and it's written again. A row
//     for a book no longer in the folder (added or discarded) is dropped;
//     one for a folder that's there but couldn't be read, or for another
//     queue folder (-import-dir changed), is kept and logged.
//   - A write that fails is tried again later; the edit is safe in the
//     journal meanwhile.

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"time"
)

// importFlushRetry: how soon an item.json that couldn't be written is tried
// again.
const importFlushRetry = 30 * time.Second

// saveEdit saves a quick edit of a waiting book: to the journal, the book
// marked for its item.json to be written. Without a writer (tests),
// item.json is written at once. Under b.mu.
func (b *importInbox) saveEdit(next *pendingImport) error {
	if b.flushWake == nil || appDB == nil {
		// The folder of the item found, by its own id: never a path made
		// from the request's (which validate checked).
		return writeItemJSON(b.itemDir(next.ID), next)
	}
	merged, err := json.Marshal(next.Merged)
	if err != nil {
		return err
	}
	v := b.editSeq + 1
	if _, err := appDB.Exec(`INSERT INTO import_edit_journal (item_id, dir, version, merged, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (item_id) DO UPDATE SET dir = excluded.dir, version = excluded.version, merged = excluded.merged, updated_at = excluded.updated_at`,
		next.ID, b.dir, v, string(merged), time.Now().Unix()); err != nil {
		return err
	}
	b.editSeq = v
	b.dirty[next.ID] = v
	select {
	case b.flushWake <- struct{}{}:
	default:
	}
	return nil
}

// startItemWriter starts writing quick edits to item.json in the
// background.
func (b *importInbox) startItemWriter() {
	if appDB == nil {
		return
	}
	wake := make(chan struct{}, 1)
	b.mu.Lock()
	b.flushWake = wake
	b.mu.Unlock()
	go func() {
		retry := time.NewTicker(importFlushRetry)
		defer retry.Stop()
		for {
			select {
			case <-wake:
			case <-retry.C:
			}
			b.flushAll(time.Time{})
		}
	}()
}

// flushAll writes every book's item.json that's behind, until the deadline
// (zero: none). It says whether any are still behind.
func (b *importInbox) flushAll(deadline time.Time) bool {
	b.mu.Lock()
	ids := make([]string, 0, len(b.dirty))
	for id := range b.dirty {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	for _, id := range ids {
		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}
		if err := b.flushOne(id); err != nil {
			log.Printf("imports: saving %s's edits to item.json (tried again later; they're kept meanwhile): %v", id, err)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.dirty) > 0
}

// flushOne writes one book's item.json from items, if it's behind.
func (b *importInbox) flushOne(id string) error {
	b.flushMu.Lock()
	defer b.flushMu.Unlock()
	b.mu.Lock()
	it := b.items[id]
	v, behind := b.dirty[id]
	b.mu.Unlock()
	if !behind || it == nil {
		return nil // up to date, or taken (whoever took it sees to it)
	}
	if err := writeItemJSON(b.itemDir(id), it); err != nil {
		return err
	}
	b.mu.Lock()
	if b.dirty[id] == v {
		delete(b.dirty, id)
	}
	b.mu.Unlock()
	// Only that version: a newer edit keeps its row until it's written.
	_, err := appDB.Exec(`DELETE FROM import_edit_journal WHERE item_id = ? AND version = ?`, id, v)
	return err
}

// flushTaken writes a taken book's item.json if quick edits of it aren't in
// it yet, and drops its journal row. Under b.flushMu.
func (b *importInbox) flushTaken(it *pendingImport) error {
	b.mu.Lock()
	_, behind := b.dirty[it.ID]
	b.mu.Unlock()
	if !behind {
		return nil
	}
	if err := writeItemJSON(b.itemDir(it.ID), it); err != nil {
		return err
	}
	b.dropPendingLocked(it.ID)
	return nil
}

// dropPending forgets a book's quick edits not yet in its item.json: it
// has left the queue with them, or starts again from its files.
func (b *importInbox) dropPending(id string) {
	b.flushMu.Lock()
	defer b.flushMu.Unlock()
	b.dropPendingLocked(id)
}

// dropPendingLocked is dropPending under b.flushMu.
func (b *importInbox) dropPendingLocked(id string) {
	b.mu.Lock()
	delete(b.dirty, id)
	b.mu.Unlock()
	if appDB != nil {
		_, err := appDB.Exec(`DELETE FROM import_edit_journal WHERE item_id = ?`, id)
		logErr(err, "imports: dropping saved edits of "+id)
	}
}

// replayImportEdits, as the server starts, puts quick edits not yet in
// their item.json back on their waiting books, to be written again.
func (b *importInbox) replayImportEdits() error {
	if appDB == nil {
		return nil
	}
	rows, err := appDB.Query(`SELECT item_id, dir, version, merged FROM import_edit_journal ORDER BY version`)
	if err != nil {
		return err
	}
	type row struct {
		id, dir, merged string
		version         int64
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.dir, &r.version, &r.merged); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	replayed, gone := 0, 0
	for _, r := range all {
		b.editSeq = max(b.editSeq, r.version)
		if r.dir != b.dir {
			log.Printf("imports: edits of waiting book %s are kept for queue folder %s, not this one (%s): they're made when it's in use again", r.id, r.dir, b.dir)
			continue
		}
		it := b.items[r.id]
		if it == nil {
			if !isSafeUUIDComponent(r.id) {
				continue
			}
			if _, err := os.Stat(b.itemDir(r.id)); errors.Is(err, os.ErrNotExist) {
				// Added or discarded (with them) since.
				if _, err := appDB.Exec(`DELETE FROM import_edit_journal WHERE item_id = ?`, r.id); err == nil {
					gone++
				}
			} else {
				log.Printf("imports: edits of waiting book %s are kept: its folder couldn't be read", r.id)
			}
			continue
		}
		next := cloneImport(it)
		if err := json.Unmarshal([]byte(r.merged), &next.Merged); err != nil {
			log.Printf("imports: edits of waiting book %s are kept, but can't be read: %v", r.id, err)
			continue
		}
		b.items[r.id] = next
		b.dirty[r.id] = r.version
		replayed++
	}
	if replayed > 0 {
		log.Printf("imports: %d waiting book(s) had edits not yet in item.json; saving them now", replayed)
	}
	if gone > 0 {
		log.Printf("imports: dropped saved edits of %d book(s) no longer waiting", gone)
	}
	return nil
}

// stopItemWriter, as the server stops, writes what it can of the item.json
// files still behind before the deadline. The rest are written from the
// journal when it starts again.
func (b *importInbox) stopItemWriter(deadline time.Time) {
	if b == nil || b.flushWake == nil {
		return
	}
	if b.flushAll(deadline) {
		log.Println("imports: some quick edits aren't in item.json yet; they're kept and saved when the server starts")
	}
}
