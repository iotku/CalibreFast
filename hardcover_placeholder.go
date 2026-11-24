package main

// Placeholders on Hardcover (placeholder_sync.go has the rules): a user who
// Saved a placeholder can send it to their Hardcover account as want to
// read, like any book they share (internal/hardcoversync). Hardcover knows
// it by the placeholder's hardcover-id and slug, else its ISBN: the first
// user to find it on Hardcover (by its ISBN when they sync, or picking it
// from a search) gives it its hardcover-id and slug, for everyone.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/internal/hardcoversync"
	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/isbn"
)

// hcPlaceholderBookID: the Hardcover book id placeholder p is known by, 0
// for none.
func hcPlaceholderBookID(p *placeholder) int64 {
	id, err := strconv.ParseInt(identifierValue(p.Meta, hardcover.IDScheme), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// hcPlaceholderKeys: what Hardcover knows a placeholder by (hcBookKeys).
func hcPlaceholderKeys(p *placeholder) (isbns []string, hcID int64, slug string) {
	for _, id := range p.Meta.Identifiers {
		if strings.EqualFold(id.Scheme, "isbn") {
			if n := isbn.Normalize(id.Value); n != "" {
				isbns = append(isbns, n)
			}
		}
	}
	if s := identifierValue(p.Meta, hardcover.SlugScheme); hardcover.ValidSlug(s) {
		slug = s
	}
	return isbns, hcPlaceholderBookID(p), slug
}

// hcEditionIdentifiers: the identifiers that name Hardcover book ed.
func hcEditionIdentifiers(ed hardcover.Edition) []opfIdentifier {
	ids := []opfIdentifier{{Scheme: hardcover.IDScheme, Value: strconv.FormatInt(ed.BookID, 10)}}
	if hardcover.ValidSlug(ed.Slug) {
		ids = append(ids, opfIdentifier{Scheme: hardcover.SlugScheme, Value: ed.Slug})
	}
	return ids
}

// hcSetPlaceholderBook makes placeholder id Hardcover book ed, for
// everyone (setPlaceholderSyncIDs). Every user syncing it but except has it
// sent again as the new book.
func hcSetPlaceholderBook(id string, ed hardcover.Edition, by *User, onlyIfUnset bool, except int64) error {
	if ed.BookID <= 0 {
		return fmt.Errorf("no Hardcover book")
	}
	changed, err := setPlaceholderSyncIDs(id, hcPlaceholders, hcEditionIdentifiers(ed), by, onlyIfUnset)
	if changed {
		hcSync.BookChanged(id, &ed, except)
	}
	return err
}

// hcClaimPlaceholder: the sync found placeholder id on Hardcover as ed
// (from its ISBN or slug). A placeholder with no hardcover-id takes it.
func hcClaimPlaceholder(id string, ed hardcover.Edition, userID int64) {
	p := placeholderByUUID(id)
	if p == nil || hcPlaceholderBookID(p) > 0 || ed.BookID <= 0 {
		return
	}
	by, _ := getUserByID(userID)
	if err := hcSetPlaceholderBook(id, ed, by, true, userID); err != nil && !errors.Is(err, errPlaceholderSyncSet) {
		log.Printf("hardcover placeholder %s: %v", id, err)
	}
}

// hardcoverPlaceholders is Hardcover as a placeholderSync.
type hardcoverPlaceholders struct{}

// hcPlaceholders is the one there is.
var hcPlaceholders = hardcoverPlaceholders{}

func (hardcoverPlaceholders) Schemes() []string {
	return []string{hardcover.IDScheme, hardcover.SlugScheme}
}

// FieldValue: its hardcover.app link, else its id.
func (hardcoverPlaceholders) FieldValue(m OPFMetadata) string {
	if s := identifierValue(m, hardcover.SlugScheme); hardcover.ValidSlug(s) {
		return "https://hardcover.app/books/" + s
	}
	return identifierValue(m, hardcover.IDScheme)
}

// ParseField takes a book id or its hardcover.app link. With the server's
// Hardcover token, Hardcover is asked, so a link gets its id and an id its
// slug, and a book it hasn't is refused.
func (hardcoverPlaceholders) ParseField(ctx context.Context, field string) ([]opfIdentifier, error) {
	field = strings.TrimSpace(field)
	if field == "" {
		return nil, nil
	}
	slug, id, ok := hardcoversync.LinkTarget(field)
	if !ok {
		return nil, fmt.Errorf("Hardcover: give the book's id, or its link on hardcover.app")
	}
	if hardcoverEnabled() {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		found, ok, err := hcClient.FindEdition(ctx, nil, id, slug)
		if err != nil {
			log.Println("hardcover placeholder:", err)
			return nil, fmt.Errorf("couldn't ask Hardcover about that book; try again later")
		}
		if !ok {
			return nil, fmt.Errorf("Hardcover has no such book")
		}
		return hcEditionIdentifiers(found), nil
	}
	if id > 0 {
		return []opfIdentifier{{Scheme: hardcover.IDScheme, Value: strconv.FormatInt(id, 10)}}, nil
	}
	return []opfIdentifier{{Scheme: hardcover.SlugScheme, Value: slug}}, nil
}

// IdentityChanged: what was looked up for it is forgotten (it's looked up
// again when it's next sent).
func (hardcoverPlaceholders) IdentityChanged(uuid string, except int64) {
	hcSync.BookChanged(uuid, nil, except)
}

func (hardcoverPlaceholders) Replaced(uuid, bookUUID string) {
	hcSync.PlaceholderReplaced(uuid, bookUUID)
}
