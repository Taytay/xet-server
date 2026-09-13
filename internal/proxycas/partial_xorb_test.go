package proxycas

// partial_xorb_test.go pins the behavior around reconstructions whose
// fetch_info cites only PART of a xorb - the common real-world shape,
// since a xorb packs chunks from many files and each file's
// reconstruction cites only the chunk ranges that file needs.
//
// The bytes behind such a citation cannot be turned into the complete,
// hash-verifiable xorb a content-addressed store requires, and the
// presigned URL is signature-scoped to exactly the cited range (asking
// for the whole object returns 403), so there is no way to widen it.
// What matters is that this degrades to a live relay - the client's
// download MUST still succeed - rather than failing the request.

import (
	"testing"

	"github.com/guilt/xet-server/internal/reconwire"
)

func TestCitesXorbFromStart(t *testing.T) {
	cases := []struct {
		name  string
		infos []reconwire.FetchInfoEntry
		want  bool
	}{
		{
			name:  "whole xorb cited from chunk 0",
			infos: []reconwire.FetchInfoEntry{{Range: reconwire.IndexRange{Start: 0, End: 1047}}},
			want:  true,
		},
		{
			name:  "mid-xorb slice (real shape: file used chunks 15-21 of a shared xorb)",
			infos: []reconwire.FetchInfoEntry{{Range: reconwire.IndexRange{Start: 15, End: 21}}},
			want:  false,
		},
		{
			name: "multiple entries, one starting at 0",
			infos: []reconwire.FetchInfoEntry{
				{Range: reconwire.IndexRange{Start: 40, End: 60}},
				{Range: reconwire.IndexRange{Start: 0, End: 39}},
			},
			want: true,
		},
		{
			name: "multiple entries, none starting at 0",
			infos: []reconwire.FetchInfoEntry{
				{Range: reconwire.IndexRange{Start: 40, End: 60}},
				{Range: reconwire.IndexRange{Start: 61, End: 90}},
			},
			want: false,
		},
		{
			name:  "no entries",
			infos: nil,
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := citesXorbFromStart(tc.infos); got != tc.want {
				t.Errorf("citesXorbFromStart() = %v, want %v", got, tc.want)
			}
		})
	}
}
