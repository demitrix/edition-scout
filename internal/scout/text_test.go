package scout

import "testing"

func TestTitleMatches(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"Project Hail Mary", "project hail mary", true},
		{"Mistborn: The Final Empire", "The Final Empire", true},
		{"The Way of Kings", "The Way of Kings (The Stormlight Archive, Book 1)", false}, // "book 1" names an instalment
		{"Words of Radiance", "Words of Radiance: Book Two of the Stormlight Archive", false},
		{"The Space Within", "The Space Within Season 2", false},
		{"Dune", "Dune Messiah", false},
		{"Solaris", "Sólaris", true},
		{"Network Effect", "Network Effect", true},
		{"The 7½ Deaths of Evelyn Hardcastle", "The 7 1/2 Deaths of Evelyn Hardcastle", true},
		{"It", "It Ends with Us", false},
	}
	for _, c := range cases {
		if got := titleMatches(c.a, c.b); got != c.want {
			t.Errorf("titleMatches(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestNormalizeFoldsAccents(t *testing.T) {
	for in, want := range map[string]string{
		"Stanisław Lem":     "stanislawlem",
		"Søren Kierkegaard": "sorenkierkegaard",
		"J. R. R. Tolkien":  "jrrtolkien",
		"The 7½ Deaths":     "the712deaths",
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlaceholders(t *testing.T) {
	for _, n := range []string{"Various", "various authors", "Misc.", "Unknown", "N/A", "Various Various", "Various Poets", "Unknown Author"} {
		if !IsPlaceholderAuthor(n) {
			t.Errorf("%q should be a placeholder author", n)
		}
	}
	for _, n := range []string{"Martha Wells", "Anonymous", "Various Smith", "Marvel Various", "Author"} {
		if IsPlaceholderAuthor(n) {
			t.Errorf("%q should not be a placeholder author", n)
		}
	}
	if !placeholderTitle("Untitled Stormlight Archive #6") || placeholderTitle("Rhythm of War") {
		t.Error("placeholder title detection")
	}
}

func TestGapKeyAndStatus(t *testing.T) {
	g := NewGaps(nil, "")
	b := Book{ID: 7, Title: "Book", Ebook: []Edition{{Source: "hardcover", ID: "1", ISBN13: "9780000000001"}}}
	if st := g.Status(b, FormatEbook); st.State != StateOnHardcover || st.Editions[0].Identifier() != "ISBN 9780000000001" {
		t.Fatalf("ebook status = %+v", st)
	}
	if st := g.Status(b, FormatAudio); st.State != StateNotChecked {
		t.Fatalf("audio status = %+v", st)
	}
	e := Edition{Format: FormatAudio, ISBN13: "9781452686349", ASIN: "B00HG06ZVU"}
	e.addID("goodreads", "190165394")
	e.addID("goodreads", "190165394") // recorded once
	ids := e.Identifiers()
	if len(ids) != 3 || ids[0].Label != "ISBN-13" || ids[1].URL != "https://www.audible.com/pd/B00HG06ZVU" ||
		ids[2].URL != "https://www.goodreads.com/book/show/190165394" {
		t.Fatalf("Identifiers() = %+v", ids)
	}
	if gapKey(7, FormatAudio) != "7/audiobook" {
		t.Fatal("gap key format changed; persisted state would be orphaned")
	}
}
