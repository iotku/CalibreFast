package main

import "testing"

// coverHue must match coverHue in web/utils.ts (values from running it in a
// browser), so the book page's placeholder matches the library card.
func TestCoverHueMatchesJS(t *testing.T) {
	for uuid, want := range map[string]int{
		"1fb32edc-688c-461b-94fb-6fd25800189a": 143,
		"cb771cce-d1f7-4953-b432-3c1baef06ed9": 286,
		"afb0981a-7ab9-4475-aad1-3327c80d5462": 93,
		"f28b0d18-24c5-4921-84eb-ec719f344b17": 278,
		"a":                                    97,
		"":                                     0,
	} {
		if got := coverHue(uuid); got != want {
			t.Errorf("coverHue(%q) = %d, want %d", uuid, got, want)
		}
	}
}
