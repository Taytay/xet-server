# `github.com/guilt/xet-server/internal/bg4`

```
package bg4 // import "github.com/guilt/xet-server/internal/bg4"

Package bg4 implements the "ByteGrouping4" transform xet-core applies before
LZ4 compression for the ByteGrouping4LZ4 chunk compression scheme: bytes are
regrouped by their position mod 4 (all byte-0's of each 4-byte group first, then
all byte-1's, etc.), which tends to expose more redundancy to LZ4 in structured
binary data (e.g. tensors of same-width floats) than compressing the interleaved
bytes directly.

Ported from the public reference implementation's algorithm and cross-checked
against its own published test vector (github.com/ jedisct1/zig-xet,
src/compression.zig: applyByteGrouping / reverseByteGrouping), since xet-core's
own Rust source was not read for this transform - only its existence and name
(compression_scheme.rs) were confirmed there.

FUNCTIONS

func Apply(data []byte) []byte
    Apply reorders data into four byte-position groups (forward transform,
    applied by the client before LZ4 compression).

func Reverse(data []byte) []byte
    Reverse undoes Apply: given byte-grouped data, reconstructs the original
    interleaved byte order. This is the direction a CAS server needs, since it
    only ever receives already-grouped-and-compressed chunk data from a real
    client and must decompress it back to verify chunk hashes.
```
