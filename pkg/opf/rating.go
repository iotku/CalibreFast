package opf

import (
	"math"
	"strconv"
	"strings"
)

// MaxRating is Calibre's top rating: 10 half-stars, shown as 5 stars.
const MaxRating = 10

// ParseRating reads a rating as Calibre writes it (calibre:rating, "8" or
// "8.0", 0–10, half-stars) or the editor sends it. Anything else, out of
// range or not a number, is 0: no rating.
func ParseRating(s string) int {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return CleanRating(int(math.Round(f)))
}

// CleanRating clamps r to 0–10.
func CleanRating(r int) int { return min(max(r, 0), MaxRating) }

// FillRating sets Rating from the calibre:rating meta, unless it's set.
func (m *Metadata) FillRating() {
	if m.Rating == 0 {
		m.Rating = ParseRating(m.GetMeta("calibre:rating"))
	}
}

// SyncRatingMeta writes Rating into the calibre:rating meta, replacing what
// was there, or removes it when there's no rating.
func (m *Metadata) SyncRatingMeta() {
	m.Rating = CleanRating(m.Rating)
	kept := m.Meta[:0:0]
	for _, c := range m.Meta {
		if c.Name != "calibre:rating" {
			kept = append(kept, c)
		}
	}
	if m.Rating > 0 {
		kept = append(kept, Meta{Name: "calibre:rating", Content: strconv.Itoa(m.Rating)})
	}
	m.Meta = kept
}
