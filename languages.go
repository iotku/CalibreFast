package main

import "strings"

// Display names for the ~100 most widely used languages, keyed by every code
// they commonly appear under in book metadata: ISO 639-1 (2-letter),
// ISO 639-2/T and 639-2/B (3-letter, e.g. "fra"/"fre"), plus a few
// deprecated codes still found in older files. This only affects labels;
// grouping values together stays a user decision (see groups.go).
var languageTable = []struct {
	name  string
	codes []string
}{
	{"English", []string{"en", "eng"}},
	{"Chinese", []string{"zh", "zho", "chi"}},
	{"Spanish", []string{"es", "spa"}},
	{"Hindi", []string{"hi", "hin"}},
	{"Arabic", []string{"ar", "ara"}},
	{"Bengali", []string{"bn", "ben"}},
	{"Portuguese", []string{"pt", "por"}},
	{"Russian", []string{"ru", "rus"}},
	{"Japanese", []string{"ja", "jpn"}},
	{"Punjabi", []string{"pa", "pan"}},
	{"German", []string{"de", "deu", "ger"}},
	{"Javanese", []string{"jv", "jav"}},
	{"Korean", []string{"ko", "kor"}},
	{"French", []string{"fr", "fra", "fre"}},
	{"Telugu", []string{"te", "tel"}},
	{"Marathi", []string{"mr", "mar"}},
	{"Turkish", []string{"tr", "tur"}},
	{"Tamil", []string{"ta", "tam"}},
	{"Vietnamese", []string{"vi", "vie"}},
	{"Urdu", []string{"ur", "urd"}},
	{"Italian", []string{"it", "ita"}},
	{"Thai", []string{"th", "tha"}},
	{"Gujarati", []string{"gu", "guj"}},
	{"Persian", []string{"fa", "fas", "per"}},
	{"Polish", []string{"pl", "pol"}},
	{"Pashto", []string{"ps", "pus"}},
	{"Kannada", []string{"kn", "kan"}},
	{"Malayalam", []string{"ml", "mal"}},
	{"Odia", []string{"or", "ori"}},
	{"Burmese", []string{"my", "mya", "bur"}},
	{"Ukrainian", []string{"uk", "ukr"}},
	{"Sundanese", []string{"su", "sun"}},
	{"Hausa", []string{"ha", "hau"}},
	{"Romanian", []string{"ro", "ron", "rum", "mo", "mol"}},
	{"Dutch", []string{"nl", "nld", "dut"}},
	{"Indonesian", []string{"id", "ind", "in"}},
	{"Malay", []string{"ms", "msa", "may"}},
	{"Tagalog", []string{"tl", "tgl"}},
	{"Filipino", []string{"fil"}},
	{"Swahili", []string{"sw", "swa"}},
	{"Amharic", []string{"am", "amh"}},
	{"Yoruba", []string{"yo", "yor"}},
	{"Igbo", []string{"ig", "ibo"}},
	{"Uzbek", []string{"uz", "uzb"}},
	{"Sindhi", []string{"sd", "snd"}},
	{"Nepali", []string{"ne", "nep"}},
	{"Sinhala", []string{"si", "sin"}},
	{"Khmer", []string{"km", "khm"}},
	{"Azerbaijani", []string{"az", "aze"}},
	{"Kazakh", []string{"kk", "kaz"}},
	{"Somali", []string{"so", "som"}},
	{"Zulu", []string{"zu", "zul"}},
	{"Xhosa", []string{"xh", "xho"}},
	{"Afrikaans", []string{"af", "afr"}},
	{"Greek", []string{"el", "ell", "gre"}},
	{"Ancient Greek", []string{"grc"}},
	{"Hungarian", []string{"hu", "hun"}},
	{"Czech", []string{"cs", "ces", "cze"}},
	{"Swedish", []string{"sv", "swe"}},
	{"Danish", []string{"da", "dan"}},
	{"Norwegian", []string{"no", "nor"}},
	{"Norwegian Bokmål", []string{"nb", "nob"}},
	{"Norwegian Nynorsk", []string{"nn", "nno"}},
	{"Finnish", []string{"fi", "fin"}},
	{"Hebrew", []string{"he", "heb", "iw"}},
	{"Yiddish", []string{"yi", "yid", "ji"}},
	{"Bulgarian", []string{"bg", "bul"}},
	{"Serbian", []string{"sr", "srp"}},
	{"Croatian", []string{"hr", "hrv"}},
	{"Bosnian", []string{"bs", "bos"}},
	{"Slovak", []string{"sk", "slk", "slo"}},
	{"Slovenian", []string{"sl", "slv"}},
	{"Lithuanian", []string{"lt", "lit"}},
	{"Latvian", []string{"lv", "lav"}},
	{"Estonian", []string{"et", "est"}},
	{"Belarusian", []string{"be", "bel"}},
	{"Georgian", []string{"ka", "kat", "geo"}},
	{"Armenian", []string{"hy", "hye", "arm"}},
	{"Albanian", []string{"sq", "sqi", "alb"}},
	{"Macedonian", []string{"mk", "mkd", "mac"}},
	{"Icelandic", []string{"is", "isl", "ice"}},
	{"Irish", []string{"ga", "gle"}},
	{"Welsh", []string{"cy", "cym", "wel"}},
	{"Scottish Gaelic", []string{"gd", "gla"}},
	{"Basque", []string{"eu", "eus", "baq"}},
	{"Catalan", []string{"ca", "cat"}},
	{"Galician", []string{"gl", "glg"}},
	{"Latin", []string{"la", "lat"}},
	{"Esperanto", []string{"eo", "epo"}},
	{"Sanskrit", []string{"sa", "san"}},
	{"Mongolian", []string{"mn", "mon"}},
	{"Tibetan", []string{"bo", "bod", "tib"}},
	{"Lao", []string{"lo", "lao"}},
	{"Kurdish", []string{"ku", "kur"}},
	{"Tajik", []string{"tg", "tgk"}},
	{"Turkmen", []string{"tk", "tuk"}},
	{"Kyrgyz", []string{"ky", "kir"}},
	{"Tatar", []string{"tt", "tat"}},
	{"Uyghur", []string{"ug", "uig"}},
	{"Malagasy", []string{"mg", "mlg"}},
	{"Kinyarwanda", []string{"rw", "kin"}},
	{"Shona", []string{"sn", "sna"}},
	{"Chichewa", []string{"ny", "nya"}},
	{"Haitian Creole", []string{"ht", "hat"}},
	{"Maltese", []string{"mt", "mlt"}},
	{"Luxembourgish", []string{"lb", "ltz"}},
	{"Faroese", []string{"fo", "fao"}},
	{"Western Frisian", []string{"fy", "fry"}},
	{"Assamese", []string{"as", "asm"}},
	{"Māori", []string{"mi", "mri", "mao"}},
	{"Hawaiian", []string{"haw"}},
	{"Samoan", []string{"sm", "smo"}},
	{"Quechua", []string{"qu", "que"}},
	{"Occitan", []string{"oc", "oci"}},
	{"Breton", []string{"br", "bre"}},
	{"Corsican", []string{"co", "cos"}},
	{"Cebuano", []string{"ceb"}},
	{"Hmong", []string{"hmn"}},
	// Codes Calibre and other tools use for "no specific language".
	{"Undetermined", []string{"und"}},
	{"Multiple languages", []string{"mul"}},
	{"No linguistic content", []string{"zxx"}},
}

var languageNames = func() map[string]string {
	m := make(map[string]string, 3*len(languageTable))
	for _, l := range languageTable {
		for _, c := range l.codes {
			m[c] = l.name
		}
	}
	return m
}()

// languageDisplayName returns a human name for a language code, e.g.
// "eng" -> "English", "en-US"/"en_us" -> "English (US)",
// "zh-Hant" -> "Chinese (Hant)". ok is false for unknown codes.
func languageDisplayName(code string) (name string, ok bool) {
	code = strings.TrimSpace(code)
	base, rest, _ := strings.Cut(strings.ReplaceAll(code, "_", "-"), "-")
	name, ok = languageNames[strings.ToLower(base)]
	if !ok {
		return code, false
	}
	if rest == "" {
		return name, true
	}
	// Keep subtags as written, but conventionally case: region upper,
	// script title-case (en-us -> US, zh-hant -> Hant).
	parts := strings.Split(rest, "-")
	for i, p := range parts {
		switch {
		case len(p) == 2:
			parts[i] = strings.ToUpper(p)
		case len(p) == 4:
			parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
		}
	}
	return name + " (" + strings.Join(parts, "-") + ")", true
}

// languageLabel is what to show for a raw language value: its group's name
// if the user grouped it, otherwise its display name (or the raw code).
func languageLabel(code string) string {
	if name, _, grouped := groups.resolve("language", code); grouped {
		return name
	}
	name, _ := languageDisplayName(code)
	return name
}
