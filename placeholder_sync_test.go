package main

import (
	"context"
	"testing"
)

// fakeSync is a sync service that names books "fake-id" (and describes
// them with "fake-slug").
type fakeSync struct{ changed, replaced []string }

func (*fakeSync) Schemes() []string                      { return []string{"fake-id", "fake-slug"} }
func (*fakeSync) FieldValue(m OPFMetadata) string        { return identifierValue(m, "fake-id") }
func (f *fakeSync) IdentityChanged(uuid string, _ int64) { f.changed = append(f.changed, uuid) }
func (f *fakeSync) Replaced(uuid, book string)           { f.replaced = append(f.replaced, uuid+">"+book) }
func (*fakeSync) ParseField(_ context.Context, field string) ([]opfIdentifier, error) {
	if field == "" {
		return nil, nil
	}
	return []opfIdentifier{{Scheme: "fake-id", Value: field}}, nil
}

func TestPlaceholderSyncIDs(t *testing.T) {
	setupLibraryAndUser(t, 0)
	origData, origWrites, origSyncs := dataDir, allowWrites, placeholderSyncs
	dataDir, allowWrites = t.TempDir(), true
	placeholdersChanged()
	f := &fakeSync{}
	placeholderSyncs = []placeholderSync{f}
	t.Cleanup(func() {
		dataDir, allowWrites, placeholderSyncs = origData, origWrites, origSyncs
		placeholdersChanged()
	})
	p := &placeholder{Meta: OPFMetadata{Title: "Soon", Identifiers: []opfIdentifier{{Scheme: "ISBN", Value: "9780316129084"}}}}
	if err := savePlaceholder(p, nil); err != nil {
		t.Fatal(err)
	}
	ids := func(v string) []opfIdentifier {
		return []opfIdentifier{{Scheme: "fake-id", Value: v}, {Scheme: "fake-slug", Value: "s-" + v}}
	}

	// The first to find it names it; the ISBN stays.
	if changed, err := setPlaceholderSyncIDs(p.UUID, f, ids("1"), nil, true); !changed || err != nil {
		t.Fatalf("first: %v %v", changed, err)
	}
	m := placeholderByUUID(p.UUID).Meta
	if f.FieldValue(m) != "1" || identifierValue(m, "fake-slug") != "s-1" || placeholderISBN(m) == "" {
		t.Fatalf("after the first: %+v", m.Identifiers)
	}
	// Then only an editor changes it; the same book again is no change.
	if changed, err := setPlaceholderSyncIDs(p.UUID, f, ids("2"), nil, true); changed || err != errPlaceholderSyncSet {
		t.Errorf("second: %v %v", changed, err)
	}
	if changed, err := setPlaceholderSyncIDs(p.UUID, f, ids("1"), nil, true); changed || err != nil {
		t.Errorf("same: %v %v", changed, err)
	}
	if changed, err := setPlaceholderSyncIDs(p.UUID, f, ids("2"), nil, false); !changed || err != nil {
		t.Errorf("editor: %v %v", changed, err)
	}

	// The editor's field replaces all of its identifiers; nil leaves them.
	m = placeholderByUUID(p.UUID).Meta
	field := "3"
	if err := placeholderSyncField(context.Background(), f, &m, &field); err != nil {
		t.Fatal(err)
	}
	if f.FieldValue(m) != "3" || identifierValue(m, "fake-slug") != "" || placeholderISBN(m) == "" || sameSyncIDs(m, placeholderByUUID(p.UUID).Meta, f) {
		t.Errorf("field: %+v", m.Identifiers)
	}
	if err := placeholderSyncField(context.Background(), f, &m, nil); err != nil || f.FieldValue(m) != "3" {
		t.Errorf("no field: %+v", m.Identifiers)
	}

	placeholderReplacedForSyncs(p.UUID, "book")
	if len(f.replaced) != 1 || f.replaced[0] != p.UUID+">book" {
		t.Errorf("replaced: %v", f.replaced)
	}
}
