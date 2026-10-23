package scout

import (
	"os"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// foldAccents removes diacritics, including letters that have no Unicode
// decomposition ("Stanisław" -> "Stanislaw", "Søren" -> "Soren").
func foldAccents(s string) string {
	t, _, err := transform.String(transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC), s)
	if err == nil {
		s = t
	}
	return accentLetters.Replace(s)
}

var accentLetters = strings.NewReplacer(
	"ł", "l", "Ł", "L", "ø", "o", "Ø", "O", "đ", "d", "Đ", "D", "ß", "ss",
	"æ", "ae", "Æ", "AE", "œ", "oe", "Œ", "OE", "þ", "th", "Þ", "TH", "ð", "d", "ı", "i",
)

// normalize reduces a title or name to lower-case letters and digits, so
// "Project Hail Mary" and "project-hail-mary" compare equal.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(foldAccents(fractions.Replace(s))) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fractions spells out vulgar fractions, so "7½" and "7 1/2" agree.
var fractions = strings.NewReplacer("½", " 1/2", "¼", " 1/4", "¾", " 3/4", "⅓", " 1/3", "⅔", " 2/3")

func titleWords(s string) map[string]bool {
	s = strings.NewReplacer("'", "", "’", "").Replace(strings.ToLower(foldAccents(s)))
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		words[w] = true
	}
	return words
}

// titleMatches: equal after normalizing, or one title is the other plus a
// series prefix or subtitle ("Mistborn: The Final Empire" / "The Final
// Empire"), unless the extra words name another instalment.
func titleMatches(candidate, want string) bool {
	c, w := normalize(candidate), normalize(want)
	if c == "" || w == "" {
		return false
	}
	if c == w {
		return true
	}
	if addsInstalment(candidate, want) || addsInstalment(want, candidate) {
		return false
	}
	if len(w) >= 6 && (strings.HasPrefix(c, w) || strings.HasSuffix(c, w)) && len(w)*2 >= len(c) {
		return true
	}
	if len(c) >= 6 && (strings.HasPrefix(w, c) || strings.HasSuffix(w, c)) && len(c)*2 >= len(w) {
		return true
	}
	return false
}

var instalmentWords = map[string]bool{
	"season": true, "part": true, "book": true, "volume": true, "vol": true, "episode": true,
	"ii": true, "iii": true, "iv": true, "two": true, "three": true, "four": true, "five": true,
	"second": true, "third": true, "sequel": true,
}

// addsInstalment reports whether a has words beyond b that number or name
// another instalment ("The Space Within Season 2" vs "The Space Within").
func addsInstalment(a, b string) bool {
	bw := titleWords(b)
	for w := range titleWords(a) {
		if bw[w] {
			continue
		}
		if instalmentWords[w] {
			return true
		}
		for _, r := range w {
			if unicode.IsDigit(r) {
				return true
			}
		}
	}
	return false
}

func authorCredited(names []string, author string) bool {
	want := normalize(author)
	for _, n := range names {
		if normalize(n) == want {
			return true
		}
	}
	return false
}

// Placeholder authors: records like "Various" or "Unknown" collect
// anthologies and badly credited books. They aren't people and are never
// shown or searched. SCOUT_BLOCKED_AUTHORS adds names (comma-separated).
var placeholderAuthors = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range []string{
		"Various", "Various Authors", "Various Artists", "Various Writers", "Various Contributors",
		"Misc", "Misc.", "Miscellaneous", "Miscellaneous Authors",
		"Unknown", "Unknown Author", "Author Unknown", "[Unknown]",
		"Multiple Authors", "Several Authors", "Many Authors",
		"N/A", "NA", "None", "No Author",
	} {
		m[normalize(n)] = true
	}
	for _, n := range strings.Split(os.Getenv("SCOUT_BLOCKED_AUTHORS"), ",") {
		if k := normalize(n); k != "" {
			m[k] = true
		}
	}
	return m
}()

// placeholderWords: a name made only of these ("Various Various", "Various
// Poets", "Unknown Author") is a placeholder too.
var placeholderWords = map[string]bool{
	"various": true, "misc": true, "miscellaneous": true, "unknown": true, "anon": true,
	"author": true, "authors": true, "artist": true, "artists": true, "writer": true, "writers": true,
	"poet": true, "poets": true, "contributor": true, "contributors": true, "multiple": true, "and": true,
}

func IsPlaceholderAuthor(name string) bool {
	if placeholderAuthors[normalize(name)] {
		return true
	}
	words := titleWords(name)
	if len(words) == 0 || !words["various"] && !words["unknown"] && !words["misc"] && !words["miscellaneous"] {
		return false
	}
	for w := range words {
		if !placeholderWords[w] {
			return false
		}
	}
	return true
}

// placeholderTitle: announced-but-unwritten works ("Untitled Stormlight
// Archive #6"), which have no edition to find.
func placeholderTitle(t string) bool {
	w := titleWords(t)
	return w["untitled"] || w["tba"]
}
