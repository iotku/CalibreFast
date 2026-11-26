package main

// Which books' bulk-editor rows changed, and when: each catalog update
// (catalogFlush) and each change to the bulk queue's rows notes the books it
// touched, under an increasing number. A browser that has the rows as of one
// number asks for the changes since (?since=, table_rows.go) and gets just
// those books' rows instead of the whole library again.
//
// The journal keeps the last tableJournalMax books noted. Anything it can't
// answer for (a restart, a full rebuild of the catalog, changes older than
// it keeps) makes the browser load every row again, as before.

import (
	"slices"
	"sync"
)

// tableJournalMax: how many noted books the journal keeps.
const tableJournalMax = 20000

type tableChange struct {
	seq   uint64
	uuids []string
}

var tableJournal struct {
	sync.Mutex
	seq     uint64
	floor   uint64 // changes at or before this aren't kept: asking since then can't be answered
	changes []tableChange
	size    int // uuids in changes
}

// tableChanged notes that these books' rows changed; nil: every row (a new
// catalog built from scratch). Call it after the change is visible to
// readers (the catalog stored, the queue's rows set), so whoever reads the
// number first then reads rows at least that new.
func tableChanged(uuids []string) {
	j := &tableJournal
	j.Lock()
	defer j.Unlock()
	j.seq++
	if uuids == nil {
		j.changes, j.size, j.floor = nil, 0, j.seq
		return
	}
	if len(uuids) == 0 {
		return
	}
	j.changes = append(j.changes, tableChange{seq: j.seq, uuids: slices.Clone(uuids)})
	j.size += len(uuids)
	for j.size > tableJournalMax && len(j.changes) > 0 {
		j.floor = j.changes[0].seq
		j.size -= len(j.changes[0].uuids)
		j.changes = j.changes[1:]
	}
}

// tableSeq is the current number. Read it before reading rows.
func tableSeq() uint64 {
	tableJournal.Lock()
	defer tableJournal.Unlock()
	return tableJournal.seq
}

// tableChangedSince: the books whose rows changed after seq, each once;
// false when the journal can't tell (it didn't keep that far back, or seq
// is from the future: another run).
func tableChangedSince(seq uint64) ([]string, bool) {
	j := &tableJournal
	j.Lock()
	defer j.Unlock()
	if seq < j.floor || seq > j.seq {
		return nil, false
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range j.changes {
		if c.seq <= seq {
			continue
		}
		for _, u := range c.uuids {
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
	}
	return out, true
}
