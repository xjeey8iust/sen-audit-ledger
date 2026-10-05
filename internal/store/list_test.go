package store

import "testing"

func TestCompareInstants(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want int
	}{
		{"identical", "2026-10-05T00:00:00Z", "2026-10-05T00:00:00Z", 0},
		{"fraction trailing zero", "2026-10-05T00:00:00.5Z", "2026-10-05T00:00:00.50Z", 0},
		{"fraction vs none", "2026-10-05T00:00:00Z", "2026-10-05T00:00:00.0Z", 0},
		{"fraction longer wins", "2026-10-05T00:00:00.5Z", "2026-10-05T00:00:00.5001Z", -1},
		{"fraction leading zero", "2026-10-05T00:00:00.09Z", "2026-10-05T00:00:00.1Z", -1},
		{"whole second beats fraction", "2026-10-05T00:00:01Z", "2026-10-05T00:00:00.999Z", 1},
		{"beyond nanoseconds", "2026-10-05T00:00:00.000000000000001Z", "2026-10-05T00:00:00.000000000000002Z", -1},
		{"year range", "0000-01-01T00:00:00Z", "9999-12-31T23:59:59Z", -1},
		{"date order", "2026-10-05T00:00:00Z", "2026-10-06T00:00:00Z", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CompareInstants(tc.a, tc.b); got != tc.want {
				t.Fatalf("CompareInstants(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
			if got := CompareInstants(tc.b, tc.a); got != -tc.want {
				t.Fatalf("CompareInstants(%q, %q) = %d, want %d", tc.b, tc.a, got, -tc.want)
			}
		})
	}
}
