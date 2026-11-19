package main

// The Server page's storage bars: how much room is left on each disk one
// of the server's folders is on.
// The markup and classes are the Preferences page's storage breakdown
// (bar() in web/preferences.ts).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// lowDiskBytes and lowDiskShare: a disk with less free than either is low.
const (
	lowDiskBytes = 1 << 30
	lowDiskShare = 0.10
)

// diskInfo is a filesystem's size and the room left on it.
type diskInfo struct {
	ID    uint64 // tells filesystems apart
	Total int64
	Free  int64 // available to this process (not what's kept for root)
}

// diskOf finds the filesystem holding a folder (statDisk, per platform);
// false when it can't tell. A variable so tests can stand in for disks.
var diskOf = statDisk

// storageSeg is one part of a storage bar.
type storageSeg struct {
	Label string
	Class string
	Bytes int64
	Size  string
	Width float64 // percent of the bar
}

// storageMeter is one bar: a disk, with the folders on it.
type storageMeter struct {
	Title string
	Aria  string
	Segs  []storageSeg
	Paths []string // "Data folder: data", one per folder on this disk
	Low   string   // a warning when the disk is nearly full
}

// storageDir is a folder the server reads or writes.
type storageDir struct {
	Name string
	Path string
}

// existingDir is p made absolute, or the nearest folder above it that
// exists: where a folder not made yet (an empty recycle bin) would go.
func existingDir(p string) string {
	p = realPath(p)
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// storageTTL is how long a measurement stands: the Server page reads the
// cached one, so reloading it never asks the OS more than this often.
const storageTTL = 15 * time.Second

var storageCache struct {
	sync.Mutex
	at     time.Time
	meters []storageMeter
}

// serverStorage is measureStorage, at most once every storageTTL.
func serverStorage() []storageMeter {
	storageCache.Lock()
	defer storageCache.Unlock()
	if storageCache.meters == nil || time.Since(storageCache.at) >= storageTTL {
		storageCache.meters = measureStorage()
		storageCache.at = time.Now()
	}
	return storageCache.meters
}

// measureStorage puts the active folders (data folder, recycle bin, import
// queue, library, cache, watched folder) on bars by the disk they're on,
// with each disk's used and free space. Only the OS's figures: a stat of
// calibrefast.db and its -wal and -shm, and a statfs of each folder. No
// folder is walked, so it stays fast however big the recycle bin gets.
func measureStorage() []storageMeter {
	dirs := []storageDir{
		{"Data folder", dataDir},
		{"Recycle bin", recycleBinDir()},
		{"Import queue", importQueueDir()},
		{"Library", baseDir},
		{"Cache", cacheDir},
		{"Watched folder", watchDir},
	}
	const dataIdx = 0
	var db int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if st, err := os.Stat(filepath.Join(dataDir, appDBFile+suffix)); err == nil {
			db += st.Size()
		}
	}

	// The disks, in the order their first folder comes in dirs.
	type disk struct {
		info  diskInfo
		known bool
		dirs  []int
	}
	var disks []*disk
	byID := map[uint64]*disk{}
	var unknown *disk // folders whose disk we can't tell: one bar, no free space
	for i, d := range dirs {
		if d.Path == "" {
			continue
		}
		info, ok := diskOf(existingDir(d.Path))
		var dk *disk
		switch {
		case !ok && unknown != nil:
			dk = unknown
		case !ok:
			unknown = &disk{}
			dk = unknown
			disks = append(disks, dk)
		case byID[info.ID] != nil:
			dk = byID[info.ID]
		default:
			dk = &disk{info: info, known: true}
			byID[info.ID] = dk
			disks = append(disks, dk)
		}
		dk.dirs = append(dk.dirs, i)
	}

	var meters []storageMeter
	for _, dk := range disks {
		var m storageMeter
		var names []string
		var ours int64
		for _, i := range dk.dirs {
			names = append(names, dirs[i].Name)
			// Only the folder's own name: the page doesn't show where on the server it is.
			m.Paths = append(m.Paths, dirs[i].Name+": "+filepath.Base(filepath.Clean(dirs[i].Path)))
			if i == dataIdx {
				ours = db
			}
		}
		segs := []storageSeg{{Label: "Database", Class: "seg-database", Bytes: ours}}
		whole := ours
		if dk.known {
			used := dk.info.Total - dk.info.Free
			segs = append(segs,
				storageSeg{Label: "Other files", Class: "seg-disk-other", Bytes: max(0, used-ours)},
				storageSeg{Label: "Free", Class: "seg-free", Bytes: dk.info.Free},
			)
			whole = max(dk.info.Total, ours+dk.info.Free)
			m.Title = joinNames(names) + ": " + humanSize(dk.info.Free) + " free of " + humanSize(dk.info.Total)
			if dk.info.Free < lowDiskBytes || float64(dk.info.Free) < lowDiskShare*float64(dk.info.Total) {
				m.Low = fmt.Sprintf("Running out of space on this disk: only %s (%.0f%%) left.",
					humanSize(dk.info.Free), float64(dk.info.Free)/float64(max(dk.info.Total, 1))*100)
			}
		} else {
			m.Title = joinNames(names) + ": free space unknown"
		}
		var aria []string
		for _, s := range segs {
			if s.Bytes <= 0 {
				continue
			}
			s.Size = humanSize(s.Bytes)
			// At least a sliver, so a small part still shows.
			s.Width = max(0.6, float64(s.Bytes)/float64(max(whole, 1))*100)
			aria = append(aria, s.Label+" "+s.Size)
			m.Segs = append(m.Segs, s)
		}
		m.Aria = m.Title + ": " + strings.Join(aria, ", ")
		meters = append(meters, m)
	}
	return meters
}

// joinNames is "A", "A and B" or "A, B and C", with only the first capitalised.
func joinNames(names []string) string {
	for i := 1; i < len(names); i++ {
		names[i] = strings.ToLower(names[i])
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
