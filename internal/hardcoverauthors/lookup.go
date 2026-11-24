package hardcoverauthors

import (
	"context"
	"errors"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/internal/hardcoverseries"
	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/imageutil"
	"github.com/iotku/CalibreFast/pkg/textkey"
)

const (
	// JobKind is author lookups' kind of job in the Hardcover queue.
	JobKind = "author"
	// Spacing: the least time between two author lookups.
	Spacing = 20 * time.Second
	// Reserve: the share of the day's limit author lookups leave to
	// editors' series requests.
	Reserve = 0.2
	// RetryAfter: a lookup that failed (not one that found nothing) may be
	// asked for again after this.
	RetryAfter = 24 * time.Hour

	imageMaxWidth  = 600
	imageKeepBytes = 400 << 10
	imageQuality   = 88
)

// Result is a lookup job's result.
type Result struct {
	Found  bool   `json:"found"`
	HCName string `json:"hcName,omitempty"`
}

var errNoClient = errors.New("hardcover isn't configured")

// Held: author lookups wait for the day's limit to reset, leaving what's
// left of it to editors. Unlike a hardcover.WaitError it holds only
// author jobs.
func Held(until time.Time) error {
	return &hardcoverseries.HeldError{Until: until,
		Reason: "most of today's Hardcover limit is used; author lookups wait until " + until.UTC().Format("15:04 UTC")}
}

// budgetLeft: whether an author lookup may spend from today's limit now;
// else when it may.
func (s *Service) budgetLeft(c *hardcover.Client) (bool, time.Time) {
	u := c.Budget.Usage(s.now())
	left := u.Limit - u.Used
	if u.Remote >= 0 && u.Remote < left {
		left = u.Remote
	}
	// Three requests at most per lookup.
	if float64(left-3) < Reserve*float64(u.Limit) {
		return false, u.ResetsAt
	}
	return true, time.Time{}
}

// Lookup looks the author (as browsed) up on Hardcover and keeps what it
// says: the queue's Run for JobKind. A *hardcoverseries.HeldError: it
// waits for the day's limit; a hardcover.WaitError: for the plan's.
func (s *Service) Lookup(ctx context.Context, name string) (any, error) {
	c := s.cfg.Client()
	if c == nil {
		return nil, errNoClient
	}
	if ok, until := s.budgetLeft(c); !ok {
		return nil, Held(until)
	}
	members := s.host.Members(name)
	if len(members) == 0 {
		members = []string{name}
	}
	a, err := s.find(ctx, c, members)
	var wait *hardcover.WaitError
	switch {
	case errors.As(err, &wait) || ctx.Err() != nil:
		return nil, err
	case err != nil:
		// Not asked again for a while, whoever opens the page.
		_ = s.Save(&Author{Name: name, HCID: -1, CheckedAt: s.now().Unix()})
		return nil, err
	}
	row := &Author{Name: name, CheckedAt: s.now().Unix()}
	if a != nil {
		row.HCID, row.Slug, row.HCName, row.Bio, row.Born, row.Died = a.ID, a.Slug, a.Name, a.Bio, a.BornYear, a.DeathYear
		if a.ImageURL != "" {
			row.Image = s.saveImage(ctx, c, a.ImageURL)
		}
	}
	if err := s.Save(row); err != nil {
		return nil, err
	}
	return Result{Found: a != nil, HCName: row.HCName}, nil
}

// saveImage fetches and keeps an author's picture; "" when it can't be
// had (the profile is kept without it).
func (s *Service) saveImage(ctx context.Context, c *hardcover.Client, u string) string {
	if s.cfg.StoreImage == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, _, err := c.FetchImage(ctx, u)
	if err != nil {
		return ""
	}
	jpg, err := imageutil.CoverJPEG(data, imageutil.CoverOptions{
		MaxWidth:     imageMaxWidth,
		KeepMaxBytes: imageKeepBytes,
		Quality:      imageQuality,
	})
	if err != nil {
		return ""
	}
	name, err := s.cfg.StoreImage(jpg)
	if err != nil {
		log.Println("author image:", err)
		return ""
	}
	return name
}

// find finds the Hardcover author who is members (an author, or the names
// an author group holds): by exact name first, then by search, keeping
// only those with the same name. Of several, the one who wrote the most
// of the library's books, then the most read. nil: none, or none sure.
func (s *Service) find(ctx context.Context, c *hardcover.Client, members []string) (*hardcover.Author, error) {
	keys := map[string]bool{}
	var names []string
	for _, m := range members {
		keys[textkey.Name(m)] = true
		for _, v := range nameVariants(m) {
			if !slices.Contains(names, v) {
				names = append(names, v)
			}
		}
	}
	same := func(a hardcover.Author) bool { return keys[textkey.Name(a.Name)] }

	cands, err := c.AuthorsByName(ctx, names)
	if err != nil {
		return nil, err
	}
	cands = slices.DeleteFunc(cands, func(a hardcover.Author) bool { return !same(a) })
	if len(cands) == 0 {
		hits, err := c.SearchAuthors(ctx, members[0])
		if err != nil {
			return nil, err
		}
		var ids []int64
		for _, h := range hits {
			ok := keys[textkey.Name(h.Name)]
			for _, alt := range h.Alternates {
				ok = ok || keys[textkey.Name(alt)]
			}
			if ok && len(ids) < 5 {
				ids = append(ids, h.ID)
			}
		}
		if len(ids) == 0 {
			return nil, nil
		}
		if cands, err = c.AuthorsByID(ctx, ids); err != nil {
			return nil, err
		}
	}
	return pickAuthor(cands, s.host.TitleKeys(members)), nil
}

// pickAuthor: of authors of the same name, the one whose books the
// library has most of. When the library has none of anyone's (as far as
// their most read books show), only an author who is the only one, or far
// more read than the next, is taken.
func pickAuthor(cands []hardcover.Author, titles map[string]bool) *hardcover.Author {
	if len(cands) == 0 {
		return nil
	}
	overlap := func(a hardcover.Author) int {
		n := 0
		for _, t := range a.Titles {
			if titles[textkey.Title(t)] {
				n++
			}
		}
		return n
	}
	sorted := slices.Clone(cands)
	slices.SortStableFunc(sorted, func(a, b hardcover.Author) int {
		if d := overlap(b) - overlap(a); d != 0 {
			return d
		}
		return b.UsersCount - a.UsersCount
	})
	best := sorted[0]
	switch {
	case overlap(best) > 0, len(sorted) == 1:
	case best.UsersCount >= 5*max(sorted[1].UsersCount, 1):
	default:
		return nil
	}
	return &best
}

var (
	initialsRe   = regexp.MustCompile(`\b(\p{Lu})\.\s*`)
	initialEndRe = regexp.MustCompile(`(\p{Lu}\.)(\p{L}{2,})`)
)

// nameVariants: the spellings of a name Hardcover may have it under: as
// given, "Last, First" turned round, and initials with and without spaces
// ("J. R. R. Tolkien", "J.R.R. Tolkien").
func nameVariants(name string) []string {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return nil
	}
	bases := []string{name}
	if last, first, ok := strings.Cut(name, ", "); ok && !strings.Contains(first, ",") && first != "" {
		bases = append(bases, first+" "+last)
	}
	var out []string
	add := func(s string) {
		if s = strings.Join(strings.Fields(s), " "); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, b := range bases {
		add(b)
		// Spaced: "J.R.R. Tolkien" -> "J. R. R. Tolkien".
		add(initialsRe.ReplaceAllString(b, "$1. "))
		// Packed: "J. R. R. Tolkien" -> "J.R.R. Tolkien".
		packed := initialsRe.ReplaceAllString(b, "$1.")
		packed = initialEndRe.ReplaceAllString(packed, "$1 $2")
		add(packed)
	}
	return out
}
