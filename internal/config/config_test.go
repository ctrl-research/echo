package config

import "testing"

func TestParseBytes(t *testing.T) {
	ok := map[string]int64{
		"0":      0,
		"1024":   1024,
		"512B":   512,
		"5GB":    5 << 30,
		"5gb":    5 << 30,
		"5GiB":   5 << 30,
		"512MB":  512 << 20,
		"1.5GB":  1610612736,
		"2 TB":   2 << 40,
		" 4KiB ": 4 << 10,
	}
	for in, want := range ok {
		got, err := ParseBytes(in)
		if err != nil {
			t.Errorf("ParseBytes(%q) returned error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseBytes(%q) = %d, want %d", in, got, want)
		}
	}

	bad := []string{"", "GB", "5XB", "-1GB", "abc", "5 GBB"}
	for _, in := range bad {
		if got, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q) = %d, want error", in, got)
		}
	}
}
