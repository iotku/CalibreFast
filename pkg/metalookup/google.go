package metalookup

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"

	"github.com/iotku/CalibreFast/pkg/isbn"
	"github.com/iotku/CalibreFast/pkg/langcodes"
	"github.com/iotku/CalibreFast/pkg/opf"
)

// Google Books: https://developers.google.com/books/docs/v1/using

type gbVolume struct {
	ID         string `json:"id"`
	VolumeInfo struct {
		Title               string   `json:"title"`
		Subtitle            string   `json:"subtitle"`
		Authors             []string `json:"authors"`
		Publisher           string   `json:"publisher"`
		PublishedDate       string   `json:"publishedDate"`
		Description         string   `json:"description"`
		Language            string   `json:"language"` // "en"
		Categories          []string `json:"categories"`
		IndustryIdentifiers []struct {
			Type       string `json:"type"` // ISBN_10, ISBN_13, OTHER
			Identifier string `json:"identifier"`
		} `json:"industryIdentifiers"`
	} `json:"volumeInfo"`
}

var googleIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

func (c *Client) googleBooksLookup(ctx context.Context, q Query) ([]Match, error) {
	v := url.Values{}
	if q.ISBN != "" {
		v.Set("q", "isbn:"+q.ISBN)
	} else {
		s := `intitle:"` + strings.ReplaceAll(q.Title, `"`, "") + `"`
		if q.Author != "" {
			s += ` inauthor:"` + strings.ReplaceAll(q.Author, `"`, "") + `"`
		}
		v.Set("q", s)
	}
	v.Set("maxResults", "1")
	if c.googleKey != "" {
		v.Set("key", c.googleKey)
	}
	var found struct {
		Items []gbVolume `json:"items"`
	}
	ok, err := c.getJSON(ctx, GoogleBooks, c.googleBooks+"/volumes?"+v.Encode(), &found)
	if rl := (*rateLimitedError)(nil); errors.As(err, &rl) && c.googleKey == "" {
		return nil, errors.New("Google's shared quota is used up; use your own API key")
	}
	if err != nil || !ok || len(found.Items) == 0 {
		return nil, err
	}
	vol := found.Items[0]
	info := vol.VolumeInfo
	m := &opf.Metadata{
		Title:     joinSubtitle(info.Title, info.Subtitle),
		Creators:  info.Authors,
		Publisher: info.Publisher,
		Date:      lookupDate(info.PublishedDate),
		Subjects:  splitGoogleCategories(info.Categories),
	}
	if strings.Contains(info.Description, "<") {
		m.Description = info.Description // already HTML
	} else {
		m.Description = textToHTML(info.Description)
	}
	if info.Language != "" {
		m.Languages = []string{langcodes.Canonical(info.Language)}
	}
	isbn13 := q.ISBN
	for _, id := range info.IndustryIdentifiers {
		if isbn13 == "" && strings.HasPrefix(id.Type, "ISBN") {
			isbn13 = isbn.Normalize(id.Identifier)
		}
	}
	if isbn13 != "" {
		m.Identifiers = append(m.Identifiers, opf.Identifier{Scheme: "isbn", Value: isbn13})
	}
	if googleIDRe.MatchString(vol.ID) {
		m.Identifiers = append(m.Identifiers, opf.Identifier{Scheme: "google", Value: vol.ID})
	}
	return []Match{{Meta: m}}, nil
}

// splitGoogleCategories turns "Computers / Programming / General" into
// separate tags, without "General" and repeats.
func splitGoogleCategories(cats []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range cats {
		for _, part := range strings.Split(c, "/") {
			part = strings.TrimSpace(part)
			k := strings.ToLower(part)
			if part == "" || k == "general" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, part)
		}
	}
	return out
}
