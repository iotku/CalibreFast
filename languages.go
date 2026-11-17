package main

import (
	"encoding/json"
	"net/http"

	"github.com/iotku/CalibreFast/pkg/langcodes"
)

// languageLabel is what to show for a raw language value: its group's name
// if the user grouped it, otherwise its display name (or the raw code).
func languageLabel(code string) string {
	if name, _, grouped := groups.resolve("language", code); grouped {
		return name
	}
	name, _ := langcodes.DisplayName(code)
	return name
}

// languageCodesHandler: GET /api/languages/codes — every known code ("en",
// "eng", "fre", …) mapped to its language's name and preferred code.
func languageCodesHandler(w http.ResponseWriter, r *http.Request) {
	out := langcodes.Codes()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=86400")
	logErr(json.NewEncoder(w).Encode(out), "encode language codes")
}
