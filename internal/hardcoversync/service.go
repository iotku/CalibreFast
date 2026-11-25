package hardcoversync

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
	// OAuth signs users in to Hardcover; nil: sync is off (no linking, and
	// nothing is sent). Revoker ends sign-ins when users unlink, even with
	// sync off; nil only with no client id at all.
	OAuth, Revoker *hardcover.OAuth
	// Catalog is the client to look books up with: the server's own token
	// when it has one (nil: none, so the user's own client is used).
	Catalog func() *hardcover.Client
	// UserAgent is sent with every request.
	UserAgent string
	// SyncURL is the GraphQL endpoint for users' clients ("": Hardcover's).
	SyncURL string
	// Budget is a user's plan limits (Hardcover's are per token). Default:
	// hardcover.FreePlan.
	Budget func() *hardcover.Budget
	// PollInterval is how often a sign-in is polled, given what Hardcover
	// asked for. Default: as asked.
	PollInterval func(time.Duration) time.Duration
	// Now is the clock. Default: time.Now.
	Now func() time.Time
}

// Host is what the Service needs of the rest of the app: its books and
// placeholders, users' reading, starred authors, secrets and
// notifications.
type Host interface {
	// Syncable: a book that can be sent to Hardcover (one in the library,
	// or a placeholder).
	Syncable(uuid string) bool
	// BookKeys: a book's ISBN-13s, Hardcover book id and slug.
	BookKeys(uuid string) (isbns []string, hcID int64, slug string, err error)
	// BookTitle: a book's title and authors ("A & B").
	BookTitle(uuid string) (title, authors string, ok bool)
	// Placeholder: uuid is a placeholder; the Hardcover book it names, if any.
	Placeholder(uuid string) (hcID int64, slug, title string, ok bool)
	// ClaimPlaceholder: the sync found placeholder uuid on Hardcover as ed;
	// one with no Hardcover book takes it, for everyone.
	ClaimPlaceholder(uuid string, ed hardcover.Edition, userID int64)

	// ReadStatus is userID's reading status for the book ("" for none).
	ReadStatus(userID int64, uuid string) string
	// Progress is how far along userID is in the book (0–1), and when.
	Progress(userID int64, uuid string) (percent float64, at time.Time, ok bool)

	// Seal and Open encrypt and decrypt a token for storage; context names
	// where it's kept, and must match.
	Seal(context, plain string) (string, error)
	Open(context, sealed string) (string, error)

	// ReplaceRelinkNotice replaces userID's "link Hardcover again"
	// notification with one naming features, in tx.
	ReplaceRelinkNotice(tx *sql.Tx, userID int64, features string) error
	// ClearRelinkNotice deletes it.
	ClearRelinkNotice(userID int64) error

	// ResolveAuthor is an author's name as browsed (after groups), and the
	// names grouped with it.
	ResolveAuthor(name string) (browsed string, members []string)
	// LibraryAuthors are the library's authors as browsed.
	LibraryAuthors() []LibraryAuthor
	// AuthorAsBrowsed is the library author name is, as browsed.
	AuthorAsBrowsed(name string) (string, bool)
	// MaxFavorites is how many starred authors (and the rest) a user may have.
	MaxFavorites() int
	// TouchFavorites records, in tx, that userID's stars changed at nowMs.
	TouchFavorites(tx *sql.Tx, userID, nowMs int64) error
	// FavoritesChanged: userID's stars changed outside a transaction.
	FavoritesChanged(userID int64)
}

// LibraryAuthor is an author as browsed, with the names grouped with it.
type LibraryAuthor struct {
	Name    string
	Members []string
}

// Service sends users' reading to their Hardcover accounts, links and
// unlinks them, and brings in or sends their followed authors. Methods on a
// nil Service that the rest of the app calls as things change (Touch,
// Status, QueueFollow, …) do nothing.
type Service struct {
	db           *sql.DB
	host         Host
	oauth        *hardcover.OAuth
	revoker      *hardcover.OAuth
	catalog      func() *hardcover.Client
	userAgent    string
	syncURL      string
	budget       func() *hardcover.Budget
	pollInterval func(time.Duration) time.Duration
	clock        func() time.Time

	mu      sync.Mutex
	linked  map[int64]bool // users with a working link
	clients map[int64]*hardcover.Client
	tokens  map[int64]*sync.Mutex // one refresh at a time per user
	pending map[int64]*PendingLink
	busy    map[int64]bool      // users the worker is sending for now
	lastRun map[int64]time.Time // when the worker last finished a step for a user
	wake    chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
}

// New makes a Service and reads which users have a working link. Start
// starts its worker.
func New(cfg Config) *Service {
	s := &Service{
		db:           cfg.DB,
		host:         cfg.Host,
		oauth:        cfg.OAuth,
		revoker:      cfg.Revoker,
		catalog:      cfg.Catalog,
		userAgent:    cfg.UserAgent,
		syncURL:      cfg.SyncURL,
		budget:       cfg.Budget,
		pollInterval: cfg.PollInterval,
		clock:        cfg.Now,
		linked:       map[int64]bool{},
		clients:      map[int64]*hardcover.Client{},
		tokens:       map[int64]*sync.Mutex{},
		pending:      map[int64]*PendingLink{},
		busy:         map[int64]bool{},
		lastRun:      map[int64]time.Time{},
		wake:         make(chan struct{}, 1),
	}
	if s.catalog == nil {
		s.catalog = func() *hardcover.Client { return nil }
	}
	if s.budget == nil {
		s.budget = hardcover.FreePlan
	}
	if s.pollInterval == nil {
		s.pollInterval = func(d time.Duration) time.Duration { return d }
	}
	if s.clock == nil {
		s.clock = time.Now
	}
	s.loadLinked()
	return s
}

func (s *Service) now() time.Time { return s.clock() }

// working marks userID busy while the worker sends something of theirs;
// the func it returns marks them done and records when.
func (s *Service) working(userID int64) func() {
	s.mu.Lock()
	s.busy[userID] = true
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.busy, userID)
		s.lastRun[userID] = s.now()
		s.mu.Unlock()
	}
}

// Activity: whether the worker is sending something of userID's now, and
// when it last finished one (zero: not since the server started). Kept in
// memory only.
func (s *Service) Activity(userID int64) (busy bool, lastRun time.Time) {
	if s == nil {
		return false, time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy[userID], s.lastRun[userID]
}

// formatDay is how dates are shown on the account page.
func formatDay(unix int64) string { return time.Unix(unix, 0).Format("2 Jan 2006 15:04") }

// scanStrings reads one string column.
func scanStrings(rows *sql.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
