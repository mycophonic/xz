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

// Decode uses the range decoder to decode a value with the given number of
// given bits. The most-significant bit is decoded first.
func (dc directCodec) Decode(d *rangeDecoder) (v uint32, err error) {
	// Direct bits are equiprobable, so there is no probability model to
	// touch — the per-bit cost is purely call overhead and reloading
	// range/code from the struct. Hoist range/code into locals and inline
	// DirectDecodeBit (and its byte read) into the loop; write back once.
	const top = 1 << 24
	rng, code := d.nrange, d.code
	for i := int(dc); i > 0; i-- {
		rng >>= 1
		code -= rng
		t := 0 - (code >> 31)
		code += rng & t
		v = (v << 1) | ((t + 1) & 1)
		if rng < top {
			rng <<= 8
			var b byte
			if b, err = d.br.ReadByte(); err != nil {
				d.nrange, d.code = rng, code
				return 0, err
			}
			code = (code << 8) | uint32(b)
		}
	}
	d.nrange, d.code = rng, code
	return v, nil
}
