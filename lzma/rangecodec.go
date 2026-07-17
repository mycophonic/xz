// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lzma

import (
	"errors"
	"io"
)

// rangeEncoder implements range encoding of single bits. The low value can
// overflow therefore we need uint64. The cache value is used to handle
// overflows.
type rangeEncoder struct {
	lbw      *LimitedByteWriter
	nrange   uint32
	low      uint64
	cacheLen int64
	cache    byte
}

// maxInt64 provides the  maximal value of the int64 type
const maxInt64 = 1<<63 - 1

// newRangeEncoder creates a new range encoder.
func newRangeEncoder(bw io.ByteWriter) (re *rangeEncoder, err error) {
	lbw, ok := bw.(*LimitedByteWriter)
	if !ok {
		lbw = &LimitedByteWriter{BW: bw, N: maxInt64}
	}
	return &rangeEncoder{
		lbw:      lbw,
		nrange:   0xffffffff,
		cacheLen: 1}, nil
}

// Available returns the number of bytes that still can be written. The
// method takes the bytes that will be currently written by Close into
// account.
func (e *rangeEncoder) Available() int64 {
	return e.lbw.N - (e.cacheLen + 4)
}

// writeByte writes a single byte to the underlying writer. An error is
// returned if the limit is reached. The written byte will be counted if
// the underlying writer doesn't return an error.
func (e *rangeEncoder) writeByte(c byte) error {
	if e.Available() < 1 {
		return ErrLimit
	}
	return e.lbw.WriteByte(c)
}

// DirectEncodeBit encodes the least-significant bit of b with probability 1/2.
func (e *rangeEncoder) DirectEncodeBit(b uint32) error {
	e.nrange >>= 1
	e.low += uint64(e.nrange) & (0 - (uint64(b) & 1))

	// normalize
	const top = 1 << 24
	if e.nrange >= top {
		return nil
	}
	e.nrange <<= 8
	return e.shiftLow()
}

// EncodeBit encodes the least significant bit of b. The p value will be
// updated by the function depending on the bit encoded.
func (e *rangeEncoder) EncodeBit(b uint32, p *prob) error {
	bound := p.bound(e.nrange)
	if b&1 == 0 {
		e.nrange = bound
		p.inc()
	} else {
		e.low += uint64(bound)
		e.nrange -= bound
		p.dec()
	}

	// normalize
	const top = 1 << 24
	if e.nrange >= top {
		return nil
	}
	e.nrange <<= 8
	return e.shiftLow()
}

// Close writes a complete copy of the low value.
func (e *rangeEncoder) Close() error {
	for i := 0; i < 5; i++ {
		if err := e.shiftLow(); err != nil {
			return err
		}
	}
	return nil
}

// shiftLow shifts the low value for 8 bit. The shifted byte is written into
// the byte writer. The cache value is used to handle overflows.
func (e *rangeEncoder) shiftLow() error {
	if uint32(e.low) < 0xff000000 || (e.low>>32) != 0 {
		tmp := e.cache
		for {
			err := e.writeByte(tmp + byte(e.low>>32))
			if err != nil {
				return err
			}
			tmp = 0xff
			e.cacheLen--
			if e.cacheLen <= 0 {
				if e.cacheLen < 0 {
					panic("negative cacheLen")
				}
				break
			}
		}
		e.cache = byte(uint32(e.low) >> 24)
	}
	e.cacheLen++
	e.low = uint64(uint32(e.low) << 8)
	return nil
}

// rangeDecoder decodes single bits of the range encoding stream.
type rangeDecoder struct {
	br     io.ByteReader
	nrange uint32
	code   uint32
}

// newRangeDecoder initializes a range decoder. It reads five bytes from the
// reader and therefore may return an error.
func newRangeDecoder(br io.ByteReader) (d *rangeDecoder, err error) {
	d = &rangeDecoder{br: br, nrange: 0xffffffff}

	b, err := d.br.ReadByte()
	if err != nil {
		return nil, err
	}
	if b != 0 {
		return nil, errors.New("newRangeDecoder: first byte not zero")
	}

	for i := 0; i < 4; i++ {
		if err = d.updateCode(); err != nil {
			return nil, err
		}
	}

	if d.code >= d.nrange {
		return nil, errors.New("newRangeDecoder: d.code >= d.nrange")
	}

	return d, nil
}

// possiblyAtEnd checks whether the decoder may be at the end of the stream.
func (d *rangeDecoder) possiblyAtEnd() bool {
	return d.code == 0
}

// rcTop is the range-decoder normalization threshold. When nrange drops
// below it another input byte is shifted into code.
const rcTop = 1 << 24

// decodeBitArith performs the arithmetic half of decoding a single range-coded
// bit: it selects the bit as code >= bound, updates the probability model, and
// returns the decoded bit together with the new range and code. It does NOT
// normalize — the caller renormalizes (see readNorm) whenever nrng < rcTop.
//
// The decoded bit is code >= bound. That comparison is data-dependent and
// essentially unpredictable (near-random compressed bits), so branching on it
// mispredicts ~half the time; instead a full-width mask selects the range,
// code and probability updates with arithmetic (the bare comparison compiles
// to a conditional set, no branch).
//
// Range/code are passed and returned by value rather than read from and
// written to *rangeDecoder. This keeps the function small enough to inline
// into the multi-bit codec decode loops, which then hold range/code in
// registers across a whole symbol and commit them to the struct once, instead
// of storing and reloading them on every bit. The only memory side effect is
// the required *p update.
func decodeBitArith(p *prob, rng, code uint32) (bit, nrng, ncode uint32) {
	pv := uint32(*p)
	bound := (rng >> probbits) * pv
	// mask is 0xffffffff when the bit is 1 (code >= bound), else 0.
	var mask uint32
	if code >= bound {
		bit = 1
		mask = ^mask
	}
	// bit 1: code -= bound, nrange -= bound, p -= p>>movebits
	// bit 0: code stays,    nrange  = bound, p += (max-p)>>movebits
	ncode = code - bound&mask
	nrng = (rng-bound)&mask | bound&^mask
	*p = prob(pv + ((((1<<probbits)-pv)>>movebits)&^mask) - ((pv>>movebits)&mask))
	return bit, nrng, ncode
}

// readNorm performs one renormalization step on the passed range/code: it
// shifts in one input byte. It is only reached ~once per eight bits, so the
// out-of-line call (it reads through the io.ByteReader and may error) is
// amortized; keeping it out of decodeBitArith is what lets that function
// inline.
func (d *rangeDecoder) readNorm(rng, code uint32) (nrng, ncode uint32, err error) {
	rng <<= 8
	c, err := d.br.ReadByte()
	if err != nil {
		return rng, code, err
	}
	return rng, (code << 8) | uint32(c), nil
}

// decodeBit decodes a single bit. The bit will be returned at the
// least-significant position. All other bits will be zero. The probability
// value will be updated.
func (d *rangeDecoder) DecodeBit(p *prob) (b uint32, err error) {
	rng, code := d.nrange, d.code
	b, rng, code = decodeBitArith(p, rng, code)
	if rng < rcTop {
		if rng, code, err = d.readNorm(rng, code); err != nil {
			d.nrange, d.code = rng, code
			return b, err
		}
	}
	d.nrange, d.code = rng, code
	return b, nil
}

// updateCode reads a new byte into the code.
func (d *rangeDecoder) updateCode() error {
	b, err := d.br.ReadByte()
	if err != nil {
		return err
	}
	d.code = (d.code << 8) | uint32(b)
	return nil
}
