package hardcoverseries

import (
	"testing"

	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/textkey"
)

func TestRankCandidates(t *testing.T) {
	cand := func(id int64, name, author string, hits int, books ...string) Candidate {
		return Candidate{Series: hardcover.Series{ID: id, Name: name, Author: author, Books: books}, Hits: hits}
	}
	picked := func(cs []Candidate) (ids []int64, sure bool) {
		for _, c := range cs {
			if c.Pick {
				ids = append(ids, c.ID)
				sure = c.Sure
			}
		}
		return ids, sure
	}
	// Hardcover's split series: a library series of another name; the
	// group holding its ISBNs is picked whole, parts without hits too.
	cs := rankCandidates([]Candidate{
		cand(10, "Doctor Who: BBC Radio Collection", "", 1),
		cand(1, "Doctor Who: Target Books", "Gerry Davis", 6),
		cand(2, "Doctor Who: Target Books", "Terrance Dicks", 3),
		cand(3, "Doctor Who: Target Books", "Malcolm Hulke", 0),
	}, map[string]bool{textkey.Series("Doctor Who: Target"): true}, nil, nil)
	ids, sure := picked(cs)
	if len(ids) != 3 || !sure || cs[0].ID != 1 || cs[3].ID != 10 {
		t.Errorf("split series: picked %v sure %v, order %v", ids, sure, cs)
	}
	// Same name, by the library's author: picked and sure; other authors'
	// same-named series are another group only if their name differs.
	cs = rankCandidates([]Candidate{
		cand(5, "Sherlock Holmes", "Val Andrews", 0),
		cand(6, "Sherlock Holmes", "Arthur Conan Doyle", 0),
		cand(7, "Sherlock Holmes Novels", "Arthur Conan Doyle", 0),
	}, map[string]bool{textkey.Series("Sherlock Holmes"): true}, []string{"Doyle, Arthur Conan"}, nil)
	if ids, sure := picked(cs); len(ids) != 2 || !sure {
		t.Errorf("same name: picked %v sure %v", ids, sure)
	}
	// Evidence by titles only, under another name: picked, not sure.
	cs = rankCandidates([]Candidate{cand(8, "Barsoom Novels", "", 0, "A Princess of Mars")},
		map[string]bool{textkey.Series("Barsoom"): true}, nil, nil)
	if ids, _ := picked(cs); len(ids) != 0 || cs[0].Evidence != "" {
		t.Errorf("no ISBNs, other name: picked %v", ids)
	}
	cs = rankCandidates([]Candidate{cand(8, "Barsoom", "", 0, "A Princess of Mars")},
		map[string]bool{textkey.Series("Barsoom"): true}, nil, map[string]bool{textkey.FullTitle("A Princess of Mars"): true})
	if ids, sure := picked(cs); len(ids) != 1 || !sure || cs[0].Evidence != "title" {
		t.Errorf("titles: picked %v sure %v %+v", ids, sure, cs[0])
	}
	// Titles only, under another name: the whole group ticked, not linked by a batch.
	cs = rankCandidates([]Candidate{
		cand(11, "Doctor Who: Target Books", "Gerry Davis", 0, "Doctor Who: Shada"),
		cand(12, "Doctor Who: Target Books", "Ian Briggs", 0),
		cand(13, "Doctor Who: Novels", "", 0),
	}, map[string]bool{textkey.Series("Doctor Who: Target"): true}, nil, map[string]bool{textkey.FullTitle("Doctor Who: Shada"): true})
	if ids, sure := picked(cs); len(ids) != 2 || sure {
		t.Errorf("titles, other name: picked %v sure %v", ids, sure)
	}
	// A weak lead (1 ISBN, another name) is ticked but not linked by a batch.
	cs = rankCandidates([]Candidate{cand(9, "Something Else", "", 1)}, map[string]bool{"x": true}, nil, nil)
	if ids, sure := picked(cs); len(ids) != 1 || sure {
		t.Errorf("weak lead: picked %v sure %v", ids, sure)
	}
}
