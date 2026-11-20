package main

// The API budgets on the Server page (/admin/server): how much of each
// online service's allowance (Hardcover, Open Library, Google Books) the
// server has used, as its own limiters count it. Reading them only takes
// each limiter's lock: nothing is asked of the services, so the page can
// refresh them every few seconds.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// apiBudget is one service's allowance, as the Server page shows it.
type apiBudget struct {
	Name string
	// Off says why the server doesn't use the service ("" when it does);
	// the rest is then empty.
	Off    string
	Used   int
	Limit  int
	Width  float64 // percent of the day's limit used
	Aria   string
	Daily  string // "120 of 4,500 requests today"
	Resets string // "resets at midnight UTC, in 7h 12m"
	Rate   string // "8 of 10 ready now, refilling 60 a minute"
	Paused string // "Paused until 15:04 after a 429 …" ("" when not)
	Note   string // what the count leaves out
}

// apiBudgets is every service's allowance at now.
func apiBudgets(now time.Time) []apiBudget {
	return []apiBudget{hardcoverBudget(now), lookupBudget(openLibraryName, "", now), lookupBudget(googleBooksName, googleOff(), now)}
}

func hardcoverBudget(now time.Time) apiBudget {
	b := apiBudget{Name: hardcoverName}
	switch {
	case noLookups:
		b.Off = "Off: the server runs with -no-lookups."
		return b
	case noHardcover:
		b.Off = "Off: the server runs with -no-hardcover."
		return b
	case hcClient == nil:
		b.Off = "Not set up: CF_HARDCOVER_TOKEN isn't set."
		return b
	}
	u := hcClient.Budget.Usage(now)
	used := u.Used
	if u.Remote >= 0 && u.Limit-used > u.Remote {
		used = u.Limit - u.Remote // Hardcover counts other tools' use of the token too
		b.Note = "Hardcover's own count, which includes other tools using the same token."
	}
	fillBudget(&b, used, u.Limit, u.Tokens, u.Burst, u.PerMinute, "a minute")
	b.Resets = "Resets at midnight UTC, in " + hcDuration(u.ResetsAt.Sub(now)) + "."
	if !u.PausedUntil.IsZero() {
		b.Paused = "Paused until " + u.PausedUntil.UTC().Format("15:04 UTC") + ": Hardcover asked the server to slow down, or today's allowance is used up."
	}
	return b
}

func googleOff() string {
	switch {
	case noLookups:
		return "Off: the server runs with -no-lookups."
	case noGoogleBooks:
		return "Off: the server runs with -no-google-books."
	case googleBooksKey == "":
		return "Not set up: CF_GOOGLE_BOOKS_KEY isn't set."
	}
	return ""
}

func lookupBudget(source, off string, now time.Time) apiBudget {
	b := apiBudget{Name: source, Off: off}
	if b.Off == "" && noLookups {
		b.Off = "Off: the server runs with -no-lookups."
	}
	if b.Off != "" {
		return b
	}
	u, ok := bookLookups.Usage(source, now)
	if !ok {
		b.Off = "Unknown source."
		return b
	}
	fillBudget(&b, u.Used, u.Daily, u.Tokens, u.Burst, u.PerSecond, "a second")
	b.Resets = "Resets at midnight server time, in " + hcDuration(u.ResetsAt.Sub(now)) + "."
	if !u.PausedUntil.IsZero() {
		b.Paused = "Paused until " + u.PausedUntil.Format("15:04") + ": " + source + " asked the server to slow down."
	}
	if source == openLibraryName {
		b.Note = "A lookup is up to 7 requests, plus a cover."
	}
	return b
}

// fillBudget sets the day's count and the bucket's state.
func fillBudget(b *apiBudget, used, limit int, tokens, burst, rate float64, per string) {
	b.Used, b.Limit = used, limit
	if limit > 0 {
		b.Width = min(100, max(0, float64(used)*100/float64(limit)))
	}
	b.Daily = strings.TrimSpace(countLabel(used, "", "")) + " of " + countLabel(limit, "request", "requests") + " used today"
	b.Aria = fmt.Sprintf("%s: %s", b.Name, b.Daily)
	ready := int(max(0, tokens)) // whole requests that can go at once
	b.Rate = fmt.Sprintf("%d of %s ready now, refilling %s %s.", ready, strconv.Itoa(int(burst)), rateLabel(rate), per)
}

// rateLabel is a refill rate: "60", "1", "0.1".
func rateLabel(r float64) string {
	return strconv.FormatFloat(r, 'f', -1, 64)
}
