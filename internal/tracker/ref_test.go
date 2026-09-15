package tracker

import "testing"

func TestParseRef(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"ENG-123", "ENG-123", false},
		{"  ENG-123  ", "ENG-123", false},
		{"https://linear.app/acme/issue/ENG-123/some-title", "ENG-123", false},
		{"https://linear.app/acme/issue/ENG-123", "ENG-123", false},
		{"garbage", "", true},
		{"https://example.com/issue/ENG-123", "", true},
		{"eng-123", "", true},
	} {
		got, err := ParseRef(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParseRef(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
