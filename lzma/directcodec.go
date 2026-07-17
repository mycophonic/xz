// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lzma

// directCodec allows the encoding and decoding of values with a fixed number
// of bits. The number of bits must be in the range [1,32].
type directCodec byte

// Bits returns the number of bits supported by this codec.
func (dc directCodec) Bits() int {
	return int(dc)
}

// Encode uses the range encoder to encode a value with the fixed number of
// bits. The most-significant bit is encoded first.
func (dc directCodec) Encode(e *rangeEncoder, v uint32) error {
	for i := int(dc) - 1; i >= 0; i-- {
		if err := e.DirectEncodeBit(v >> uint(i)); err != nil {
			return err
		}
	}
	return nil
}

// decode uses the range decoder to decode a value with the given number of
// given bits. The most-significant bit is decoded first. The decoder state
// range/code is threaded through in registers (see readOp).
func (dc directCodec) decode(d *rangeDecoder, rng, code uint32) (v, nrng, ncode uint32) {
	// Direct bits are equiprobable, so there is no probability model to
	// touch — the per-bit cost is purely the range arithmetic. The
	// renormalization byte read is hand-inlined (readByteSlow only on the
	// cold branch), so the common loop body is free of calls and error
	// branches; read errors are sticky on the decoder and checked once per
	// operation.
	for i := int(dc); i > 0; i-- {
		rng >>= 1
		code -= rng
		t := 0 - (code >> 31)
		code += rng & t
		v = (v << 1) | ((t + 1) & 1)
		if rng < rcTop {
			rng <<= 8
			code <<= 8
			if pos := d.pos; pos < len(d.buf) {
				code |= uint32(d.buf[pos])
				d.pos = pos + 1
			} else {
				code |= uint32(d.readByteSlow())
			}
		}
	}
	return v, rng, code
}
