package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/iotku/CalibreFast/pkg/langcodes"
)

func TestLanguageCodesHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	languageCodesHandler(rec, httptest.NewRequest("GET", "/api/languages/codes", nil))
	var got map[string]langcodes.Code
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for code, want := range map[string]langcodes.Code{
		"en":  {Name: "English", Canonical: "eng"},
		"eng": {Name: "English", Canonical: "eng"},
		"fre": {Name: "French", Canonical: "fra"},
		"fil": {Name: "Filipino", Canonical: "fil"},
	} {
		if got[code] != want {
			t.Errorf("%s = %+v; want %+v", code, got[code], want)
		}
	}
}
