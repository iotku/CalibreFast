package opf

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxSeriesName is the longest series name CleanSeries accepts, in runes.
const MaxSeriesName = 255

// FillSeries sets Series and SeriesIndex from the OPF's meta, unless
// they're set already: Calibre's calibre:series, or EPUB 3's
// belongs-to-collection (with its group-position).
func (m *Metadata) FillSeries() {
	if m.Series != "" {
		return
	}
	if name := strings.TrimSpace(m.GetMeta("calibre:series")); name != "" {
		m.Series = name
		m.SeriesIndex = FormatSeriesIndex(m.GetMeta("calibre:series_index"))
		return
	}
	for _, c := range m.Meta {
		if c.Property != "belongs-to-collection" || strings.TrimSpace(c.Value) == "" {
			continue
		}
		// A "set" is a collection too; a series is either untyped or typed so.
		kind, pos := "", ""
		for _, r := range m.Meta {
			if c.ID == "" || r.Refines != "#"+c.ID {
				continue
			}
			switch r.Property {
			case "collection-type":
				kind = strings.TrimSpace(r.Value)
			case "group-position":
				pos = strings.TrimSpace(r.Value)
			}
		}
		if kind != "" && kind != "series" {
			continue
		}
		m.Series = strings.TrimSpace(c.Value)
		m.SeriesIndex = FormatSeriesIndex(pos)
		return
	}
}

// FormatSeriesIndex: "2.0" -> "2", "2.50" -> "2.5"; "" or not a number -> "".
func FormatSeriesIndex(s string) string {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return ""
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// CleanSeries checks what the editor sent: a name (or none) and its
// number, which defaults to 1 as in Calibre.
func CleanSeries(name, index string) (string, float64, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", 1, nil
	}
	if utf8.RuneCountInString(name) > MaxSeriesName {
		return "", 0, errors.New("the series name is too long")
	}
	index = strings.TrimSpace(index)
	if index == "" {
		return name, 1, nil
	}
	f, err := strconv.ParseFloat(index, 64)
	if err != nil || f < 0 || f > 1e6 || math.IsNaN(f) || math.IsInf(f, 0) {
		return "", 0, errors.New("the number in the series must be a number, such as 2 or 2.5")
	}
	return name, f, nil
}

// SyncSeriesMeta writes Series and SeriesIndex into the OPF meta
// (calibre:series, calibre:series_index), replacing what was there, or
// removes them when there's no series. EPUB 3 series meta is removed too,
// so the file says one thing.
func (m *Metadata) SyncSeriesMeta() {
	name, index, _ := CleanSeries(m.Series, m.SeriesIndex)
	collections := map[string]bool{}
	for _, c := range m.Meta {
		if c.Property == "belongs-to-collection" && c.ID != "" {
			collections["#"+c.ID] = true
		}
	}
	kept := m.Meta[:0:0]
	for _, c := range m.Meta {
		if c.Name == "calibre:series" || c.Name == "calibre:series_index" ||
			c.Property == "belongs-to-collection" || collections[c.Refines] {
			continue
		}
		kept = append(kept, c)
	}
	if name != "" {
		kept = append(kept,
			Meta{Name: "calibre:series", Content: name},
			Meta{Name: "calibre:series_index", Content: strconv.FormatFloat(index, 'f', -1, 64)})
	}
	m.Meta = kept
	m.Series, m.SeriesIndex = name, ""
	if name != "" {
		m.SeriesIndex = strconv.FormatFloat(index, 'f', -1, 64)
	}
}
