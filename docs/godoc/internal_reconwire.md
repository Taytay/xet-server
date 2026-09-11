# `xet-server/internal/reconwire`

```
package reconwire // import "xet-server/internal/reconwire"

Package reconwire defines the wire types and response-building logic for GET
/v1|v2/reconstructions/{file_id} — extracted out of internal/casserver so the
clipping/grouping logic behind both endpoints has a single, independently-tested
home. casserver's own handlers still do the clipping (calling BuildV1/BuildV2
directly); internal/proxycas only consumes the wire types (ResponseV1) to
decode an upstream reconstruction response before feeding its terms into
casserver.Server.IngestFileRecon — casserver's own delegation then runs the
same BuildV1/BuildV2 logic over that ingested data. Because entries reaching
BuildV1/BuildV2 by that path originated from an untrusted upstream response,
physicalRange bounds-checks every chunk-index field it uses to index a footer's
slices rather than trusting entries to already be internally consistent.

This package has no notion of "where entries come from" — callers supply the
file's entries and a FooterLookup callback (each server's own xorbFooters index
has a different concrete type/locking strategy), keeping this package a pure
function of its inputs.

FUNCTIONS

func FileSize(entries []shardformat.FileDataSequenceEntry) int64
    FileSize returns entries' total logical (uncompressed) byte length — the
    file's full size, used both to clip a requested range and (by callers) to
    decide whether a cached fileRecon entry can serve a whole downstream request
    without an upstream call.


TYPES

type ByteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"` // inclusive
}
    ByteRange mirrors xet-core's wire shape for a byte range: both ends
    inclusive.

type ErrChunkIndexOutOfRange struct {
	Hash                      merklehash.Hash
	ChunkIndexStart, ChunkEnd uint32
	FooterChunkCount          uint32
}
    ErrChunkIndexOutOfRange is returned (wrapped) by BuildV1/BuildV2 when
    an entry's ChunkIndexStart/ChunkIndexEnd falls outside its xorb's
    own footer's chunk count. entries can originate from an untrusted
    upstream reconstruction response (internal/proxycas ingests one into
    casserver.Server.IngestFileRecon without independently re-deriving it — see
    this package's doc comment), so this is a real, reachable input validation
    failure, not just defensive programming: without this check, a malformed or
    hostile upstream response would index a slice out of bounds and crash the
    whole process.

func (e *ErrChunkIndexOutOfRange) Error() string

type ErrUnknownXorbFooter struct{ Hash merklehash.Hash }
    ErrUnknownXorbFooter is returned (wrapped) by BuildV1/BuildV2 when entries
    references a xorb this server has no footer for — the server-side fault
    casserver surfaces as a 500 (a shard referenced a xorb whose upload session
    never completed). proxycas never sees this error directly: it only reaches
    BuildV1/BuildV2 indirectly, through its embedded casserver.Server's own
    handlers.

func (e *ErrUnknownXorbFooter) Error() string

type FetchInfoEntry struct {
	URL      string     `json:"url"`
	URLRange ByteRange  `json:"url_range"`
	Range    IndexRange `json:"range"`
}
    FetchInfoEntry is one V1 fetch-info entry: a URL to fetch bytes from, the
    physical byte range within that URL's target, and the logical chunk-index
    range it corresponds to.

type FetchURLBuilder func(hash merklehash.Hash) (string, error)
    FetchURLBuilder returns the URL a client should fetch hash's bytes from
    — casserver may hand out a presigned storage URL or its own byte-serving
    endpoint. The sole caller is casserver.Server; proxycas never constructs
    one of its own (see FooterLookup's doc comment) — it gets casserver's own
    byte-serving endpoint for free by delegating to its embedded Server, which
    is exactly the URL a caching proxy needs anyway (relaying a real presigned
    URL would bypass its cache).

type FooterLookup func(hash merklehash.Hash) (footer xorbformat.FooterV1, ok bool)
    FooterLookup resolves a xorb hash to its known V1 footer, or ok=false
    if this server has no footer for it yet — the one piece of state
    BuildV1/BuildV2 need that isn't already in entries. The sole caller is
    casserver.Server (its own xorbFooters index); proxycas.Server never calls
    BuildV1/BuildV2 directly — it embeds a real casserver.Server and delegates
    reconstruction serving to it (see internal/proxycas's package doc comment).

type IndexRange struct {
	Start uint32 `json:"start"`
	End   uint32 `json:"end"`
}
    IndexRange mirrors xet-core's wire shape for a chunk-index range: Start
    inclusive, End exclusive.

type ResponseV1 struct {
	OffsetIntoFirstRange int64                       `json:"offset_into_first_range"`
	Terms                []Term                      `json:"terms"`
	FetchInfo            map[string][]FetchInfoEntry `json:"fetch_info"`
}
    ResponseV1 is GET /v1/reconstructions/{file_id}'s response body.

func BuildV1(entries []shardformat.FileDataSequenceEntry, rangeStart, rangeEnd int64, lookupFooter FooterLookup, fetchURL FetchURLBuilder) (ResponseV1, error)
    BuildV1 clips entries to [rangeStart, rangeEnd] (inclusive) and builds the
    V1 response — the exact logic casserver.handleReconstructionV1's loop used
    to contain inline.

type ResponseV2 struct {
	OffsetIntoFirstRange int64                            `json:"offset_into_first_range"`
	Terms                []Term                           `json:"terms"`
	Xorbs                map[string][]XorbMultiRangeFetch `json:"xorbs"`
}
    ResponseV2 is GET /v2/reconstructions/{file_id}'s response body: same
    Terms/OffsetIntoFirstRange as V1, but Xorbs groups every chunk/byte range
    touched for a given xorb hash under that hash's key, each range paired with
    a fetch URL — the "multi-range" optimization the V2 endpoint exists for.

func BuildV2(entries []shardformat.FileDataSequenceEntry, rangeStart, rangeEnd int64, lookupFooter FooterLookup, fetchURL FetchURLBuilder) (ResponseV2, error)
    BuildV2 is BuildV1's V2 counterpart: same clipping, grouped by xorb hash
    with multiple ranges per fetch URL instead of V1's one entry per term — the
    exact logic casserver.handleReconstructionV2's loop used to contain inline.

type Term struct {
	Hash           string     `json:"hash"`
	Range          IndexRange `json:"range"`
	UnpackedLength uint32     `json:"unpacked_length"`
}
    Term is one file-reconstruction term: which xorb, which chunk range within
    it, and how many logical bytes it contributes.

type XorbMultiRangeFetch struct {
	URL    string                `json:"url"`
	Ranges []XorbRangeDescriptor `json:"ranges"`
}
    XorbMultiRangeFetch is a single fetch URL covering possibly multiple
    disjoint chunk ranges for one xorb — xet-core's XorbMultiRangeFetch.

type XorbRangeDescriptor struct {
	Chunks IndexRange `json:"chunks"`
	Bytes  ByteRange  `json:"bytes"`
}
    XorbRangeDescriptor is one chunk-range/byte-range pair within a
    XorbMultiRangeFetch — xet-core's XorbRangeDescriptor. Chunks uses exclusive
    end (matching IndexRange elsewhere); Bytes uses inclusive end (matching
    ByteRange elsewhere).
```
