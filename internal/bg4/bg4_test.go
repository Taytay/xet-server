package bg4

import (
	"bytes"
	"testing"
)

// TestApply_ReferenceVector reproduces zig-xet's own published test vector
// ("byte grouping with 4-byte aligned data", src/compression.zig) for the
// forward transform, so a byte-order mistake in this port fails against an
// independently authored implementation's known-correct output.
func TestApply_ReferenceVector(t *testing.T) {
	input := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	want := []byte{1, 5, 2, 6, 3, 7, 4, 8}

	got := Apply(input)
	if !bytes.Equal(got, want) {
		t.Errorf("Apply(%v) = %v, want %v", input, got, want)
	}
}

func TestReverse_UndoesApply(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3, 4, 5, 7, 8, 100, 131072} {
		data := make([]byte, n)
		for i := range data {
			data[i] = byte(i % 256)
		}
		grouped := Apply(data)
		if len(grouped) != n {
			t.Fatalf("Apply() len = %d, want %d", len(grouped), n)
		}
		back := Reverse(grouped)
		if !bytes.Equal(back, data) {
			t.Errorf("Reverse(Apply(data)) != data for n=%d", n)
		}
	}
}

func TestReverse_ReferenceVector(t *testing.T) {
	grouped := []byte{1, 5, 2, 6, 3, 7, 4, 8}
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	got := Reverse(grouped)
	if !bytes.Equal(got, want) {
		t.Errorf("Reverse(%v) = %v, want %v", grouped, got, want)
	}
}
