package main

// Covers for placeholders made from a series on Hardcover: the book's cover
// there (its series' snapshot names it: hardcover.Entry.Image), fetched in
// the background into placeholderCoversDir. Only an https image on
// hardcover.app is fetched, without the API token (hardcover.FetchImage),
// and it's re-encoded as a JPEG, so nothing Hardcover sends is served as
// it came.
//
// A fetched cover is marked so (placeholders.cover_fetched). When its
// placeholder is replaced by a library book or removed, the file goes
// (dropFetchedPlaceholderCover); one brought back (undo, restore) fetches
// it again. A cover an editor sets instead is theirs, kept as before.

import (
	"context"
	"errors"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

var (
	hcCoverKick = make(chan struct{}, 1)
	hcCoverOnce sync.Once
	// hcCoverAuto: fetch in the background when placeholders change. Off in
	// tests, whose fake Hardcover counts every request (they call
	// hcFillPlaceholderCovers themselves).
	hcCoverAuto = !testing.Testing()
)

// hcFillPlaceholderCoversSoon fetches, in the background, the covers of
// Hardcover's placeholders that have none (or whose file is gone). Calls
// while one runs make one more run, not one each.
func hcFillPlaceholderCoversSoon() {
	if !hcCoverAuto || hcEnabledClient() == nil || appDB == nil {
		return
	}
	hcCoverOnce.Do(func() {
		go func() {
			for range hcCoverKick {
				hcFillPlaceholderCovers(context.Background())
			}
		}()
	})
	select {
	case hcCoverKick <- struct{}{}:
	default:
	}
}

// hcPlaceholderImages: Hardcover book id -> its cover, from the linked
// series' snapshots (merged books' ids too).
func hcPlaceholderImages() map[int64]string {
	links, err := hcLinks.AllLinks()
	if err != nil {
		log.Println("hardcover placeholder covers:", err)
		return nil
	}
	images := map[int64]string{}
	for _, l := range links {
		if l.Snapshot == nil {
			continue
		}
		for i := range l.Snapshot.Entries {
			e := &l.Snapshot.Entries[i]
			if e.Image == "" {
				continue
			}
			for _, id := range e.IDs() {
				if _, ok := images[id]; !ok {
					images[id] = e.Image
				}
			}
		}
	}
	return images
}

// placeholderCoverExists: the cover file is there.
func placeholderCoverExists(name string) bool {
	if name == "" || !aggregateImageRe.MatchString(name) {
		return false
	}
	_, err := os.Stat(filepath.Join(dataDir, placeholderCoversDir, name))
	return err == nil
}

// hcFillPlaceholderCovers fetches the missing covers now. The image
// URLs come from the linked series' snapshots; a book they don't name an
// image for (a snapshot saved before they were kept) is asked about by id.
func hcFillPlaceholderCovers(ctx context.Context) {
	c := hcEnabledClient()
	if c == nil {
		return
	}
	var want []*placeholder
	for _, p := range allPlaceholders() {
		if p.Source == "hardcover" && !placeholderCoverExists(p.Cover) && hcPlaceholderBookID(p) != 0 {
			want = append(want, p)
		}
	}
	if len(want) == 0 {
		return
	}
	images := hcPlaceholderImages()
	if images == nil {
		images = map[int64]string{}
	}
	var ask []int64
	for _, p := range want {
		if id := hcPlaceholderBookID(p); images[id] == "" {
			ask = append(ask, id)
		}
	}
	for chunk := range slices.Chunk(ask, 300) {
		qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		got, err := c.BookImages(qctx, chunk)
		cancel()
		if err != nil {
			log.Println("hardcover placeholder covers:", err)
			break
		}
		maps.Copy(images, got)
	}
	for _, p := range want {
		u := images[hcPlaceholderBookID(p)]
		if u == "" {
			continue
		}
		name, err := hcSaveCover(ctx, c, u)
		if err != nil {
			log.Printf("hardcover placeholder %q: cover: %v", p.Meta.Title, err)
			continue
		}
		setFetchedPlaceholderCover(p, name)
	}
}

// hcSaveCover fetches the image at u (on hardcover.app) and keeps it as a
// JPEG in placeholderCoversDir; it returns the file's name.
func hcSaveCover(ctx context.Context, c *hardcover.Client, u string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, _, err := c.FetchImage(ctx, u)
	if err != nil {
		return "", err
	}
	jpg, err := coverJPEG(data)
	if err != nil {
		return "", err
	}
	if len(jpg) == 0 {
		return "", errors.New("empty image")
	}
	return saveHashedImage(placeholderCoversDir, &aggImage{jpg, "jpg"})
}

// setFetchedPlaceholderCover makes name p's cover, unless p changed
// meanwhile (another cover, or removed): then the file goes again.
func setFetchedPlaceholderCover(p *placeholder, name string) {
	res, err := appDB.Exec(`UPDATE placeholders SET cover = ?, cover_fetched = 1 WHERE uuid = ? AND cover = ? AND deleted_at IS NULL`, name, p.UUID, p.Cover)
	n := int64(0)
	if err == nil {
		n, _ = res.RowsAffected()
	} else {
		log.Printf("hardcover placeholder %q: cover: %v", p.Meta.Title, err)
	}
	if n == 0 {
		removeUnusedPlaceholderCover(name)
		return
	}
	placeholdersChanged()
}

// dropFetchedPlaceholderCover: placeholder id was just removed. A cover
// fetched from Hardcover goes with it (unless another placeholder uses the
// same picture); one an editor chose stays, for an undo.
func dropFetchedPlaceholderCover(id string) {
	var name string
	err := appDB.QueryRow(`SELECT cover FROM placeholders WHERE uuid = ? AND deleted_at IS NOT NULL AND cover_fetched = 1`, id).Scan(&name)
	if err != nil || name == "" {
		return
	}
	if _, err := appDB.Exec(`UPDATE placeholders SET cover = '', cover_fetched = 0 WHERE uuid = ?`, id); err != nil {
		log.Printf("placeholder cover %s: %v", name, err)
		return
	}
	var n int
	if appDB.QueryRow(`SELECT COUNT(*) FROM placeholders WHERE cover = ?`, name).Scan(&n) != nil || n > 0 || !aggregateImageRe.MatchString(name) {
		return
	}
	if err := os.Remove(filepath.Join(dataDir, placeholderCoversDir, name)); err != nil && !os.IsNotExist(err) {
		log.Printf("placeholder cover %s: %v", name, err)
	}
}
