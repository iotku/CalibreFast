package main

// The Server page's storage bars: what CalibreFast keeps on the server's
// disks, and how much room is left on each disk one of its folders is on.
// The markup and classes are the Preferences page's storage breakdown
// (bar() in web/preferences.ts).

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	Paths []string // "Data folder: /srv/cf/data", one per folder on this disk
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

// serverStorage measures -datadir, the recycle bin and the import queue,
// and puts them on bars by the disk they're on, with the free space of each
// disk any active folder (those three, the library, the cache and the
// watched folder) is on.
func serverStorage() []storageMeter {
	dirs := []storageDir{
		{"Data folder", dataDir},
		{"Recycle bin", recycleBinDir()},
		{"Import queue", importQueueDir()},
		{"Library", baseDir},
		{"Cache", cacheDir},
		{"Watched folder", watchDir},
	}
	const dataIdx, binIdx, queueIdx = 0, 1, 2

	data := realPath(dataDir)
	bin := realPath(recycleBinDir())
	queue := realPath(importQueueDir())
	binInside := insideDir(bin, data)
	queueInside := insideDir(queue, data) && !insideDir(queue, bin)
	var db, binBytes, queueBytes, other int64
	_ = filepath.WalkDir(data, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		n := info.Size()
		switch {
		case binInside && insideDir(p, bin):
			binBytes += n
		case queueInside && insideDir(p, queue):
			queueBytes += n
		case filepath.Dir(p) == data && strings.HasPrefix(d.Name(), appDBFile):
			db += n // calibrefast.db and its -wal and -shm
		default:
			other += n
		}
		return nil
	})
	if !binInside {
		binBytes = dirSize(bin)
	}
	if !queueInside {
		queueBytes = dirSize(queue)
	}
	type part struct {
		seg storageSeg
		dir int // index in dirs: the folder it's in
	}
	parts := []part{
		{storageSeg{Label: "Database", Class: "seg-database", Bytes: db}, dataIdx},
		{storageSeg{Label: "Recycle bin", Class: "seg-recycle", Bytes: binBytes}, binIdx},
		{storageSeg{Label: "Import queue", Class: "seg-imports", Bytes: queueBytes}, queueIdx},
		{storageSeg{Label: "Other data", Class: "seg-other-data", Bytes: other}, dataIdx},
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
	diskOfDir := make([]*disk, len(dirs))
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
		diskOfDir[i] = dk
	}

	var meters []storageMeter
	for _, dk := range disks {
		var m storageMeter
		var names []string
		for _, i := range dk.dirs {
			names = append(names, dirs[i].Name)
			m.Paths = append(m.Paths, dirs[i].Name+": "+dirs[i].Path)
		}
		var ours int64
		var segs []storageSeg
		for _, p := range parts {
			if diskOfDir[p.dir] == dk {
				segs = append(segs, p.seg)
				ours += p.seg.Bytes
			}
		}
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
			m.Title = joinNames(names) + ": " + humanSize(ours) + " (free space unknown)"
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
