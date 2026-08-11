package library

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"The Beatles":                 "beatles",
		"Beatles":                     "beatles",
		"the beatles":                 "beatles",
		"THE BEATLES":                 "beatles",
		"Björk":                       "bjork",
		"BJÖRK":                       "bjork",
		"bjork":                       "bjork",
		"Sigur Rós":                   "sigur ros",
		"Motörhead":                   "motorhead",
		"A Tribe Called Q":            "tribe called q",
		"An Amazing Band":             "amazing band",
		"  Spaced   Out  ":            "spaced out",
		"AC/DC":                       "ac dc",
		"Panic! at the Disco":         "panic at the disco",
		"Godspeed You! Black Emperor": "godspeed you black emperor",
		"":                            "",
		"   ":                         "",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// These pairs must reconcile to one artist; that is the whole point of the key.
func TestNormalizeCollapsesVariants(t *testing.T) {
	groups := [][]string{
		{"The Beatles", "Beatles", "the  beatles", "THE BEATLES"},
		{"Björk", "Bjork", "BJÖRK", "bjork"},
		{"Sigur Rós", "Sigur Ros", "sigur rós"},
		{"AC/DC", "AC-DC", "AC DC"},
	}
	for _, group := range groups {
		want := Normalize(group[0])
		for _, variant := range group[1:] {
			if got := Normalize(variant); got != want {
				t.Errorf("Normalize(%q) = %q, want %q (same as %q)",
					variant, got, want, group[0])
			}
		}
	}
}

// An artist genuinely named after an article must not normalise away to
// nothing, which would make them unreconcilable.
func TestNormalizeKeepsBareArticles(t *testing.T) {
	for _, name := range []string{"The", "A", "An"} {
		if got := Normalize(name); got == "" {
			t.Errorf("Normalize(%q) = %q; an article-only name must survive", name, got)
		}
	}
}

// Distinct artists must not collide, or one would swallow the other's tracks.
func TestNormalizeKeepsDistinctNamesApart(t *testing.T) {
	pairs := [][2]string{
		{"The Cure", "The Cars"},
		{"Air", "Hair"},
		{"Prince", "Prints"},
	}
	for _, p := range pairs {
		if Normalize(p[0]) == Normalize(p[1]) {
			t.Errorf("Normalize collapsed %q and %q to %q", p[0], p[1], Normalize(p[0]))
		}
	}
}

func TestSplitGenres(t *testing.T) {
	cases := map[string][]string{
		"Rock":             {"Rock"},
		"Rock; Pop":        {"Rock", "Pop"},
		"Rock/Pop":         {"Rock", "Pop"},
		"Rock, Pop, Jazz":  {"Rock", "Pop", "Jazz"},
		"Rock|Metal":       {"Rock", "Metal"},
		"Rock; rock":       {"Rock"}, // case-insensitive dedupe
		"17":               {},       // bare ID3v1 index carries no meaning
		"Rock; 17":         {"Rock"},
		"":                 {},
		"  Rock  ;  Pop  ": {"Rock", "Pop"},
	}
	for in, want := range cases {
		got := splitGenres(in)
		if len(got) != len(want) {
			t.Errorf("splitGenres(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("splitGenres(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}

func TestIsAudioFile(t *testing.T) {
	audio := []string{"a.mp3", "a.FLAC", "a.m4a", "a.opus", "/x/y/z.ogg", "a.wav"}
	for _, p := range audio {
		if !IsAudioFile(p) {
			t.Errorf("IsAudioFile(%q) = false, want true", p)
		}
	}
	other := []string{"cover.jpg", "notes.txt", "a.cue", "playlist.m3u", "a", "a.mp3.part"}
	for _, p := range other {
		if IsAudioFile(p) {
			t.Errorf("IsAudioFile(%q) = true, want false", p)
		}
	}
}

func TestShouldSkipDir(t *testing.T) {
	skip := []string{".git", "node_modules", "@eaDir", ".Trash", "$RECYCLE.BIN", ".hidden"}
	for _, d := range skip {
		if !shouldSkipDir(d) {
			t.Errorf("shouldSkipDir(%q) = false, want true", d)
		}
	}
	keep := []string{"Music", "The Beatles", "Abbey Road", "CD1", "."}
	for _, d := range keep {
		if shouldSkipDir(d) {
			t.Errorf("shouldSkipDir(%q) = true, want false", d)
		}
	}
}
