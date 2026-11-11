package main

import "testing"

func TestNormalizePhone(t *testing.T) {
	ok := map[string]string{
		"(555) 010-0123":   "+15550100123",
		"555.010.0123":     "+15550100123",
		"1 555 010 0123":   "+15550100123",
		"+44 20 7946 0000": "+442079460000",
	}
	for in, want := range ok {
		got, err := normalizePhone(in)
		if err != nil || got != want {
			t.Errorf("normalizePhone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "Mom", "12345", "0123456789012", "555-0100x"} {
		if got, err := normalizePhone(bad); err == nil {
			t.Errorf("normalizePhone(%q) = %q, want error", bad, got)
		}
	}
}
