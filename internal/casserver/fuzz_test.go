package casserver

// Fuzz target for parseByteRange: parses the client-supplied HTTP Range
// header on both the xorb-fetch and reconstruction endpoints. Entirely
// attacker-controlled string input.

import "testing"

func FuzzParseByteRange(f *testing.F) {
	f.Add("bytes=0-99", int64(1000))
	f.Add("", int64(1000))
	f.Add("bytes=", int64(1000))
	f.Add("bytes=-", int64(1000))
	f.Add("bytes=0-", int64(1000))
	f.Add("bytes=-100", int64(1000))
	f.Add("bytes=100-0", int64(1000))                  // reversed
	f.Add("bytes=0-99999999999999999999", int64(1000)) // overflow-shaped
	f.Add("bytes=-1--1", int64(1000))
	f.Add("BYTES=0-99", int64(1000))          // wrong case
	f.Add("bytes=0-99, 100-199", int64(1000)) // multi-range (unsupported)
	f.Add("bytes=99999999999999999999999999-", int64(1000))

	f.Fuzz(func(t *testing.T, header string, total int64) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("parseByteRange panicked on header=%q total=%d: %v", header, total, r)
			}
		}()
		start, end, hasRange, err := parseByteRange(header, total)
		if err != nil || !hasRange {
			return
		}
		// Any successfully parsed range must be internally consistent and
		// within bounds — the whole point of this function is to hand
		// callers a range they can trust without re-validating.
		if start < 0 || end < start {
			t.Fatalf("parseByteRange(%q, %d) = start=%d end=%d, want 0 <= start <= end", header, total, start, end)
		}
		if total >= 0 && end >= total {
			t.Fatalf("parseByteRange(%q, %d) = end=%d, want end < total", header, total, end)
		}
	})
}
