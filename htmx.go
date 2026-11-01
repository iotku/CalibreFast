package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// getBooksForPage retrieves books for a specific page from the pre-generated JSON cache.
// Each page contains up to 30 books.
func getBooksForPage(page int) ([]Book, error) {
	filename := filepath.Join("pages", "page"+strconv.Itoa(page)+".json")
	return readPageJSON(filename)
}

// readPageJSON reads a pre-generated page JSON file and returns the books
func readPageJSON(filename string) ([]Book, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var books []Book
	if err := json.NewDecoder(file).Decode(&books); err != nil {
		return nil, err
	}

	return books, nil
}

// renderBooksPageFragment renders a single page of books as an HTML fragment
// for HTMX to append to the page.
func renderBooksPageFragment(w io.Writer, books []Book, pageNumber int) error {
	const pageFragment = `<div class="page" data-page="{{ .PageNumber }}">
{{ range .Books }}<div class="book" data-book-uuid="{{ .UUID }}">
    <div class="cover-wrapper" style="background:{{ colorFromBook .UUID }}">
        <img class="book-cover" data-src="/cover-thumb/{{ .UUID }}" fetchpriority="low" />
    </div>

    <div class="book-info">
        <h2>{{ .Title }}</h2>
        <div class="book-author">
            {{ .AuthorSort }}
        </div>
        <div class="formats"></div>
    </div>
</div>
{{ end }}</div>`

	tmpl, err := template.New("pageFragment").Funcs(template.FuncMap{
		"colorFromBook": colorFromBook,
	}).Parse(pageFragment)
	if err != nil {
		return err
	}

	data := map[string]interface{}{
		"PageNumber": pageNumber,
		"Books":      books,
	}

	return tmpl.Execute(w, data)
}

// colorFromBook generates a deterministic color based on the book UUID
func colorFromBook(uuid string) string {
	h := hashString(uuid)
	return fmt.Sprintf("hsl(%d, 55%%, 35%%)", h%360)
}

// hashString computes a hash of a string (matches JavaScript implementation)
func hashString(str string) int {
	h := 0
	for _, c := range str {
		h = ((h << 5) - h + int(c)) | 0
	}
	if h < 0 {
		h = -h
	}
	return h
}

// renderBookDetailFragment renders a book detail view for the modal
func renderBookDetailFragment(w io.Writer, uuid string) error {
	bookPath, ok := uuidPathIndex.Load(uuid)
	if !ok {
		var err error
		bookPath, err = resolveBookPath(uuid)
		if err != nil {
			return err
		}
	}

	opfPath := filepath.Join(baseDir, bookPath.(string), "metadata.opf")
	opf, err := loadOPF(opfPath)
	if err != nil {
		return err
	}

	const bookDetailFragment = `<div class="book-page" data-uuid="{{ .UUID }}">
    <div class="book-page-cover">
        <img src="/cover/{{ .UUID }}" alt="Cover" style="max-width: 100%;" />
    </div>
    <div class="book-page-info">
        <h1>{{ .Metadata.Title }}</h1>
        <div class="meta-row">
            <strong>Author:</strong> {{ .AuthorSort }}
        </div>
        {{ if .Metadata.Publisher }}<div class="meta-row">
            <strong>Publisher:</strong> {{ .Metadata.Publisher }}
        </div>{{ end }}
        {{ if .Metadata.Date }}<div class="meta-row">
            <strong>Published:</strong> {{ .Metadata.Date }}
        </div>{{ end }}
        {{ if .Metadata.Description }}<div class="description">
            {{ .Metadata.Description }}
        </div>{{ end }}
        {{ if .Metadata.Subjects }}<div class="meta-row">
            {{ range .Metadata.Subjects }}<span class="tag">{{ . }}</span>{{ end }}
        </div>{{ end }}
        <div id="read-buttons" style="margin-top: 1.5rem;"></div>
    </div>
</div>`

	tmpl, err := template.New("bookDetail").Parse(bookDetailFragment)
	if err != nil {
		return err
	}

	authorSort := strings.Join(opf.Metadata.Creators, ", ")
	data := map[string]interface{}{
		"UUID":        uuid,
		"Metadata":    opf.Metadata,
		"AuthorSort":  authorSort,
	}

	return tmpl.Execute(w, data)
}
