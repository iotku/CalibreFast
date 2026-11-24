package hardcoverauthors

import (
	"database/sql"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// Config configures a Service. DB and Host are required.
type Config struct {
	// DB is the app's database, which holds hardcover_authors (created by
	// the app's migrations).
	DB   *sql.DB
	Host Host
	// Client asks Hardcover with the server's token; nil: none (lookups
	// fail).
	Client func() *hardcover.Client
	// Now is the clock. Default: time.Now.
	Now func() time.Time
	// StoreImage keeps an author's picture (a JPEG) and names it; nil:
	// pictures aren't kept.
	StoreImage func(jpg []byte) (string, error)
	// RemoveImage deletes a picture StoreImage named, once no author has
	// it; nil: they're left.
	RemoveImage func(name string)
}

// Host is what the Service needs of the rest of the app.
type Host interface {
	// Members are the authors an author as browsed stands for: a group's
	// members, or just the name.
	Members(name string) []string
	// TitleKeys are the library's titles by these authors
	// (textkey.Title), for telling authors of the same name apart.
	TitleKeys(members []string) map[string]bool
}

// Service looks authors up and keeps what Hardcover said. A nil *Service
// knows no author.
type Service struct {
	db   *sql.DB
	host Host
	cfg  Config
}

// New returns a Service for cfg.
func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Client == nil {
		cfg.Client = func() *hardcover.Client { return nil }
	}
	return &Service{db: cfg.DB, host: cfg.Host, cfg: cfg}
}

func (s *Service) now() time.Time { return s.cfg.Now() }
