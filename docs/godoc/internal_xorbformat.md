# `github.com/guilt/xet-server/internal/xorbformat`

```
package xorbformat // import "github.com/guilt/xet-server/internal/xorbformat"

WriteFooterV1 writes footer f's wire representation to w, matching
xet_object_format.rs's XorbObjectInfoV1::serialize. Used by this package's
own tests to build round-trip fixtures; a real CAS server only ever needs
ParseFooterV1 for xorbs uploaded by real clients.

Package xorbformat implements the on-wire binary layout of a xorb -
the aggregated-chunk storage unit real Xet clients (hf_xet/xet-core)
upload via POST /v1/xorbs/{prefix}/{hash} - ported from
xet_core_structures/src/xorb_object/{xorb_chunk_format,xorb_object_format}.rs.

A CAS server's job is to store a xorb's serialized bytes as an opaque blob and
later hand back raw byte ranges for reconstruction; the (possibly compressed)
chunk payloads are never decompressed server-side - only the client does that
after fetching. Accordingly, this package parses chunk headers and the V1 footer
(hashes, boundary offsets) without needing to implement any compression codec.

CONSTANTS

const ChunkHeaderSize = 8
    ChunkHeaderSize is the fixed 8-byte size of a XorbChunkHeader: version(1) +
    compressed_length(3) + compression_scheme(1) + uncompressed_length(3).


FUNCTIONS

func DecompressChunkPayload(scheme CompressionScheme, payload []byte, uncompressedLen uint32) ([]byte, error)
    DecompressChunkPayload returns the uncompressed bytes of one chunk's payload
    per its declared compression scheme. Real hf_xet clients upload xorbs
    without a footer (chunk metadata is reconstructed by the server from the
    raw chunk stream - see casserver.IngestXorb), so this is the only way to
    obtain a chunk's true content and independently verify its claimed hash.
    Exported (rather than kept package-internal to casserver) since it's a pure
    codec-dispatch function with no casserver-specific state - a natural fit for
    this package alongside the rest of the wire-format logic it already owns.

func WriteChunkHeader(w io.Writer, h ChunkHeader) error
    WriteChunkHeader writes h's 8-byte wire representation to w.

func WriteFooterV1(w io.Writer, f FooterV1) error
    WriteFooterV1 serializes f to w in the same section order and layout
    XorbObjectInfoV1::serialize uses.


TYPES

type ChunkEntry struct {
	Header       ChunkHeader
	HeaderOffset int64
	DataOffset   int64 // HeaderOffset + ChunkHeaderSize
}
    ChunkEntry describes one chunk as found while scanning a xorb's chunk
    section: its header plus the physical byte offset (from the start of the
    xorb) where its header begins.

func ScanChunks(r io.ReadSeeker) ([]ChunkEntry, error)
    ScanChunks reads consecutive chunk headers from r (which must be positioned
    at the start of the chunk section), skipping over each chunk's compressed
    payload via Seek, until it encounters the xorb footer's ident ("XETBLOB") or
    EOF. Returns the chunk entries found.

    r must also implement io.Seeker; ScanChunks does not decompress or otherwise
    inspect payload bytes.

type ChunkHeader struct {
	Version            uint8
	CompressedLength   uint32 // 24-bit on the wire
	CompressionScheme  CompressionScheme
	UncompressedLength uint32 // 24-bit on the wire
}
    ChunkHeader mirrors xorb_chunk_format.rs's XorbChunkHeader.

func ReadChunkHeader(r io.Reader) (ChunkHeader, error)
    ReadChunkHeader reads and validates one 8-byte chunk header from r.

type CompressionScheme uint8
    CompressionScheme mirrors xet_core_structures::CompressionScheme's wire
    discriminants (compression_scheme.rs). Never decoded/encoded here beyond
    recording which scheme a chunk claims - the server treats chunk payload
    bytes as opaque regardless of scheme.

const (
	CompressionNone             CompressionScheme = 0
	CompressionLZ4              CompressionScheme = 1
	CompressionByteGrouping4LZ4 CompressionScheme = 2
	CompressionAuto             CompressionScheme = 99 // never valid on the wire
)
type FooterV1 struct {
	XorbHash             merklehash.Hash
	ChunkHashes          []merklehash.Hash
	ChunkBoundaryOffsets []uint32 // physical (compressed+header) byte offsets, one per chunk
	UnpackedChunkOffsets []uint32 // logical (uncompressed) byte offsets, one per chunk
	NumChunks            uint32
}
    FooterV1 mirrors xet_object_format.rs's XorbObjectInfoV1: the trailer
    written after a xorb's chunk section, carrying the xorb's content hash,
    per-chunk hashes, and both physical (compressed) and logical (uncompressed)
    chunk boundary offsets.

func DeriveFooter(r interface {
	io.ReadSeeker
	io.ReaderAt
}) (footer FooterV1, computedHash merklehash.Hash, err error)
    DeriveFooter independently reconstructs a xorb's V1 footer and content
    hash by scanning r's chunk headers (via ScanChunks) and decompressing each
    chunk's payload (via DecompressChunkPayload) - the same reconstruction real
    hf_xet clients rely on the server side to perform, since a real upload never
    includes a footer at all ("XORBs are sent without footer - the server/client
    reconstructs it from chunk data", per xet-core's file_upload_session.rs).

    Used by casserver.IngestXorb to independently verify a freshly -uploaded
    xorb's claimed hash against its actual chunk contents.

    r must implement io.ReaderAt in addition to io.ReadSeeker, to read each
    chunk's payload independently of ScanChunks' own sequential Seek position.

func ParseFooterV1(r io.Reader) (FooterV1, error)
    ParseFooterV1 reads a V1 footer from r, which must be positioned at the
    start of the footer (immediately after the last chunk's payload bytes).
```
