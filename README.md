This here is a friendly fork of https://github.com/ulikunitz/xz with
several performance improvements, mostly focused on decoding, roughly
doubling decoding speed for serial encoding, and about 20x with
parallel block decoding for multiblock archives.

Upstream seems inactive. However, if you have time and interest in doing that,
feel free to carry these changes over there.

## Upstream as of 2026-07-16

| Benchmark          | Throughput   | Time/op   | Bytes/op  | Allocs/op   |
| ------------------ | ------------ | --------- | --------- | ----------- |
| Reader (decompress) | 48.63 MiB/s | 196.1 ms  | 31.94 MiB | 1,213,036   |
| Writer (compress)   | 14.09 MiB/s | 678.3 ms  | 69.35 MiB | 1,217,288   |

## This fork

| Benchmark           | Throughput             | Time/op            | Bytes/op            | Allocs/op                |
| ------------------- | ---------------------- | ------------------ | ------------------- | ------------------------ |
| Reader (decompress) | 106.32 MiB/s (+118.6%) | 89.7 ms (-54.3%)   | 13.56 MiB (-57.6%)  | 284 (-99.98%)            |
| Writer (compress)   | 18.27 MiB/s (+29.7%)   | 522.1 ms (-23.0%)  | 50.85 MiB (-26.7%)  | 4,622 (-99.6%)           |

## LLVM xz release tarball decoding (1.6G, multiblock)

| Decoder                    | Time     | Throughput   |
| -------------------------- | -------- | ------------ |
| Reader (serial, Patch 9)   | 79.7 s   | 141 MiB/s    |
| ParallelReader (18 workers)| 7.4 s    | 1,514 MiB/s  |

------------------------

# Package xz

This Go language package supports the reading and writing of xz
compressed streams. It includes also a gxz command for compressing and
decompressing data. The package is completely written in Go and doesn't
have any dependency on any C code.

The package is currently under development. There might be bugs and APIs
are not considered stable. At this time the package cannot compete with
the xz tool regarding compression speed and size. The algorithms there
have been developed over a long time and are highly optimized. However
there are a number of improvements planned and I'm very optimistic about
parallel compression and decompression. Stay tuned!

## Using the API

The following example program shows how to use the API.

```go
package main

import (
    "bytes"
    "io"
    "log"
    "os"

    "github.com/mycophonic/xz"
)

func main() {
    const text = "The quick brown fox jumps over the lazy dog.\n"
    var buf bytes.Buffer
    // compress text
    w, err := xz.NewWriter(&buf)
    if err != nil {
        log.Fatalf("xz.NewWriter error %s", err)
    }
    if _, err := io.WriteString(w, text); err != nil {
        log.Fatalf("WriteString error %s", err)
    }
    if err := w.Close(); err != nil {
        log.Fatalf("w.Close error %s", err)
    }
    // decompress buffer and write output to stdout
    r, err := xz.NewReader(&buf)
    if err != nil {
        log.Fatalf("NewReader error %s", err)
    }
    if _, err = io.Copy(os.Stdout, r); err != nil {
        log.Fatalf("io.Copy error %s", err)
    }
}
```

## Documentation

You can find the full documentation at [pkg.go.dev](https://pkg.go.dev/github.com/mycophonic/xz).

## Using the gxz compression tool

The package includes a gxz command line utility for compression and
decompression.

Use following command for installation:

    $ go get github.com/mycophonic/xz/cmd/gxz

To test it call the following command.

    $ gxz bigfile

After some time a much smaller file bigfile.xz will replace bigfile.
To decompress it use the following command.

    $ gxz -d bigfile.xz

## Security & Vulnerabilities

The security policy is documented in [SECURITY.md](SECURITY.md). 

The software is not affected by the supply chain attack on the original xz
implementation, [CVE-2024-3094](https://nvd.nist.gov/vuln/detail/CVE-2024-3094).
This implementation doesn't share any files with the original xz implementation
and no patches or pull requests are accepted without a review.

All security advisories for this project are published under
[github.com/mycophonic/xz/security/advisories](https://github.com/mycophonic/xz/security/advisories?state=published).
