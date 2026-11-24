package hardcoverseries

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// Config configures a Service. DB and Host are required.
type Config struct {
	// DB is the app's database, which holds the tables listed in the
	// package doc (created by the app's migrations).
	DB   *sql.DB
	Host Host
	// Client asks Hardcover with the server's token; nil: none (jobs fail,
	// and the worker doesn't start).
	Client func() *hardcover.Client
	// Now is the clock. Default: time.Now.
	Now func() time.Time
	// Background is a kind of job the app runs through the same queue, after
	// editors' requests (author lookups); nil for none.
	Background *Background
}

// Background is a kind of job that waits behind every other, one at most
// every Spacing. Run gets the job's Series field; a *HeldError from it
// holds every job of the kind, while the rest still run.
type Background struct {
	Kind    string
	Spacing time.Duration
	Run     func(ctx context.Context, name string) (any, error)
}

// HeldError holds a Background kind's jobs until Until.
type HeldError struct {
	Until  time.Time
	Reason string
}

func (e *HeldError) Error() string { return e.Reason }

// Host is what the Service needs of the rest of the app: its series and
// books as matching sees them, and its placeholders.
type Host interface {
	// Series is a series as browsed (after groups); false when no library
	// book is in it.
	Series(name string) (SeriesInfo, bool)
	// Library is the library for matching d against series si (d nil: the
	// series' own books only). The Service fills in Refused.
	Library(si SeriesInfo, d *hardcover.SeriesDetail) (Library, error)
	// SeriesISBNs are the ISBN-13s of the series' library books (at most
	// 50), for finding it on Hardcover.
	SeriesISBNs(si SeriesInfo) ([]string, error)
	// SeriesTitles and SeriesAuthors are its library books' titles and
	// authors.
	SeriesTitles(si SeriesInfo) []string
	SeriesAuthors(si SeriesInfo) []string
	// Placeholders are the library's placeholders.
	Placeholders() []Placeholder
	// StatusChanged: a link, or what's dismissed, changed.
	StatusChanged()
	// ReviewURL is a series' review page.
	ReviewURL(series string) string
}

// SeriesInfo is a series as browsed: its name (its group's, if any), its
// members, and the member to write onto books and placeholders (the one
// with the most books).
type SeriesInfo struct {
	Name    string
	Members []string
	Write   string
}

// Placeholder is a placeholder as matching sees it.
type Placeholder struct {
	UUID, Title, Series string
	Authors             []string
	Index               float64
	// HCID is its Hardcover book id identifier, as stored ("" for none).
	HCID string
}

// Service keeps series' links to Hardcover and runs the queue of requests
// to it.
type Service struct {
	db     *sql.DB
	host   Host
	client func() *hardcover.Client
	clock  func() time.Time
	bg     *Background

	mu      sync.Mutex
	wake    chan struct{}
	waiters map[int64][]chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
	// nextBackground: no Background job runs before it (Spacing).
	nextBackground time.Time
}

// New makes a Service. Start starts its worker.
func New(cfg Config) *Service {
	s := &Service{db: cfg.DB, host: cfg.Host, client: cfg.Client, clock: cfg.Now, bg: cfg.Background,
		wake: make(chan struct{}, 1), waiters: map[int64][]chan struct{}{}}
	if s.client == nil {
		s.client = func() *hardcover.Client { return nil }
	}
	if s.clock == nil {
		s.clock = time.Now
	}
	return s
}

func (s *Service) now() time.Time { return s.clock() }

func (s *Service) bgKind() string {
	if s.bg == nil {
		return ""
	}
	return s.bg.Kind
}

func (s *Service) bgSpacing() time.Duration {
	if s.bg == nil {
		return 0
	}
	return s.bg.Spacing
}
