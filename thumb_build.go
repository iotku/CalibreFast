package main

// -build-thumbnails: make every cover thumbnail the cache doesn't have yet,
// then exit. The server only makes a thumbnail when someone first asks for
// it (or, when idle, ones it knows are coming: thumb_backlog.go), so a large
// library starts with most of them missing; this makes them all at once,
// ahead of time. Safe while the server runs: thumbnails are written whole
// (writeThumbFile), and either side finding one already there uses it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type thumbBuildCounts struct {
	made, had, noCover, failed atomic.Int64
	bytes                      atomic.Int64
}

// buildThumbnails makes the missing thumbnails of every book that has a
// cover, a few at a time (one per core). Stopping ctx stops it between
// thumbnails; what's made is kept.
func buildThumbnails(ctx context.Context, out io.Writer) error {
	rows, err := calibreDB.QueryContext(ctx, `SELECT uuid, path, last_modified FROM books WHERE has_cover = 1`)
	if err != nil {
		return err
	}
	var uuids []string
	for rows.Next() {
		var uuid, path, lastModified string
		if err := rows.Scan(&uuid, &path, &lastModified); err != nil {
			continue
		}
		// What the catalog keeps for each book, so loadThumb needn't ask metadata.db.
		uuidPathIndex.Store(uuid, path)
		modTimeCache.Store(uuid, coverStamp(lastModified))
		uuids = append(uuids, uuid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	dir, _ := filepath.Abs(filepath.Join(cacheDir, "covers", "thumb"))
	fmt.Fprintf(out, "%d books with a cover; thumbnails go in %s\n", len(uuids), dir)
	var n thumbBuildCounts
	start := time.Now()
	progress := func() {
		done := n.made.Load() + n.had.Load() + n.noCover.Load() + n.failed.Load()
		fmt.Fprintf(out, "%d/%d: %d made, %d already there, %d without cover.jpg, %d failed (%s)\n",
			done, len(uuids), n.made.Load(), n.had.Load(), n.noCover.Load(), n.failed.Load(), time.Since(start).Round(time.Second))
	}

	jobs := make(chan string)
	var wg sync.WaitGroup
	var outMu sync.Mutex
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for uuid := range jobs {
				if thumbCached(uuid) {
					n.had.Add(1)
				} else {
					data, hit, err := loadThumb(uuid, ctx.Err)
					switch {
					case errors.Is(err, context.Canceled):
						continue
					case errors.Is(err, fs.ErrNotExist):
						n.noCover.Add(1)
					case err != nil:
						n.failed.Add(1)
						outMu.Lock()
						fmt.Fprintf(out, "%s: %v\n", uuid, err)
						outMu.Unlock()
					case hit:
						n.had.Add(1)
					default:
						n.made.Add(1)
						n.bytes.Add(int64(len(data)))
					}
				}
				if done := n.made.Load() + n.had.Load() + n.noCover.Load() + n.failed.Load(); done%1000 == 0 {
					outMu.Lock()
					progress()
					outMu.Unlock()
				}
			}
		}()
	}
feed:
	for _, uuid := range uuids {
		select {
		case jobs <- uuid:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	progress()
	if made := n.made.Load(); made > 0 {
		fmt.Fprintf(out, "made %d thumbnails, %.1f MB\n", made, float64(n.bytes.Load())/(1<<20))
	}
	return ctx.Err()
}
