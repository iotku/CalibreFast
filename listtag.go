package main

// Lists the browser keeps a copy of (the reading marks, the Saved list, the
// Reading list for the offline page) are sent with a tag: a hash of the
// answer. The browser sends back the tag of its copy (If-None-Match, or
// ?have= on /api/me), and when nothing changed gets 304 (or a note) instead
// of the whole list again. The server still builds the list to hash it: a
// little more work here for a lot less sent over a slow connection.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// listTag: the tag of a JSON answer, as an ETag (quoted).
func listTag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:12]) + `"`
}

// marshalTagged: v as JSON (as writeJSON sends it) and its tag.
func marshalTagged(v any) ([]byte, string, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, "", err
	}
	body = append(body, '\n')
	return body, listTag(body), nil
}

// tagMatches: whether an If-None-Match header names tag.
func tagMatches(header, tag string) bool {
	for _, t := range strings.Split(header, ",") {
		if t = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), "W/")); t == tag || t == "*" {
			return true
		}
	}
	return false
}

// writeJSONTagged: writeJSON with the answer's tag, or 304 Not Modified when
// the request's If-None-Match already has it. Still no-store: the browser's
// copy is the page's own (localStorage), not the HTTP cache's.
func writeJSONTagged(w http.ResponseWriter, r *http.Request, v any) {
	body, tag, err := marshalTagged(v)
	if err != nil {
		http.Error(w, "could not encode", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("ETag", tag)
	if tagMatches(r.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
