package main

// A book's place on Hardcover, from its identifiers: "hardcover" is the
// book's slug (as Calibre's Hardcover plugin names it, and as the metadata
// editor's Hardcover lookup sets it), "hardcover-id" its Hardcover book id
// (placeholders made from a series on Hardcover). With only the id, the
// slug is found in the saved list of the series it's in.

import (
	"strconv"
	"strings"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// hcSlugScheme names a Hardcover book slug in identifiers.
const hcSlugScheme = "hardcover"

func identifierValue(m OPFMetadata, scheme string) string {
	for _, id := range m.Identifiers {
		if strings.EqualFold(id.Scheme, scheme) {
			return strings.TrimSpace(id.Value)
		}
	}
	return ""
}

// hcBookSlug: m's Hardcover slug, from its identifier or, with only a
// Hardcover book id, from its series' saved list; "" when neither has one.
func hcBookSlug(m OPFMetadata) string {
	if s := identifierValue(m, hcSlugScheme); hardcover.ValidSlug(s) {
		return s
	}
	id, err := strconv.ParseInt(identifierValue(m, hcIdentifierScheme), 10, 64)
	if err != nil || id <= 0 || appDB == nil || strings.TrimSpace(m.Series) == "" {
		return ""
	}
	name, _, _ := groups.resolve("series", m.Series)
	for _, n := range []string{name, m.Series} {
		l, err := hcLinkFor(n)
		if err != nil || l == nil || l.Snapshot == nil {
			continue
		}
		for i := range l.Snapshot.Entries {
			e := &l.Snapshot.Entries[i]
			if e.BookID == id && hardcover.ValidSlug(e.Slug) {
				return e.Slug
			}
		}
		return ""
	}
	return ""
}

// hcMetaURL: m's page on Hardcover, or "".
func hcMetaURL(m OPFMetadata) string {
	if s := hcBookSlug(m); s != "" {
		return "https://hardcover.app/books/" + s
	}
	return ""
}

// hcIdentifiers: m's Hardcover identifiers, the slug included when only
// the series' list knows it.
func hcIdentifiers(m OPFMetadata) []opfIdentifier {
	var out []opfIdentifier
	if id := identifierValue(m, hcIdentifierScheme); id != "" {
		out = append(out, opfIdentifier{Scheme: hcIdentifierScheme, Value: id})
	}
	if s := hcBookSlug(m); s != "" {
		out = append(out, opfIdentifier{Scheme: hcSlugScheme, Value: s})
	}
	return out
}

// withIdentifiers: ids with add in place of any of the same scheme.
func withIdentifiers(ids, add []opfIdentifier) []opfIdentifier {
	var out []opfIdentifier
	for _, id := range ids {
		if !slicesContainsScheme(add, id.Scheme) {
			out = append(out, id)
		}
	}
	return append(out, add...)
}

func slicesContainsScheme(ids []opfIdentifier, scheme string) bool {
	for _, id := range ids {
		if strings.EqualFold(id.Scheme, scheme) {
			return true
		}
	}
	return false
}
