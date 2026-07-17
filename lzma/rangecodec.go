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
//
// The decoder reads its input either from an in-memory byte slice (buf/pos,
// the fast path used for LZMA2 chunks, which are fully buffered) or from a
// streaming io.ByteReader (br, used for LZMA1 streams of unknown size). Read
// failures — including running off the end of buf — are not reported per
// byte; instead err is set once (sticky) and further reads return zero bytes.
// The decode loops therefore contain no error branches; callers check err
// once per decoded operation. Decoding garbage zero bytes until that check is
// harmless: every symbol decode terminates after a bounded number of bits.
type rangeDecoder struct {
	buf    []byte
	pos    int
	br     io.ByteReader
	err    error
	nrange uint32
	code   uint32
}

// byteSliceReader is an io.ByteReader over a byte slice. newRangeDecoder
// recognizes it and adopts the slice directly, enabling the call-free
// in-memory read path.
type byteSliceReader struct {
	buf []byte
	pos int
}

// ReadByte reads a single byte from the slice.
func (b *byteSliceReader) ReadByte() (c byte, err error) {
	if b.pos >= len(b.buf) {
		return 0, io.EOF
	}
	c = b.buf[b.pos]
	b.pos++
	return c, nil
}

// newRangeDecoder initializes a range decoder. It reads five bytes from the
// reader and therefore may return an error.
func newRangeDecoder(br io.ByteReader) (d *rangeDecoder, err error) {
	if bsr, ok := br.(*byteSliceReader); ok {
		d = &rangeDecoder{buf: bsr.buf, pos: bsr.pos, nrange: 0xffffffff}
	} else {
		d = &rangeDecoder{br: br, nrange: 0xffffffff}
	}

	b := d.readByte()
	if d.err != nil {
		return nil, d.err
	}
	if b != 0 {
		return nil, errors.New("newRangeDecoder: first byte not zero")
	}

	for i := 0; i < 4; i++ {
		d.code = (d.code << 8) | uint32(d.readByte())
	}
	if d.err != nil {
		return nil, d.err
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
// normalize — the caller shifts in a byte (readByte) whenever nrng < rcTop.
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
	*p = prob(pv + ((((1 << probbits) - pv) >> movebits) &^ mask) - ((pv >> movebits) & mask))
	return bit, nrng, ncode
}

// readByte returns the next input byte. On exhaustion (or a read error on the
// streaming fallback) it records a sticky error on the decoder and returns
// zero bytes; see the rangeDecoder documentation.
//
// The decode loops do not call readByte: its cost model price (the cold
// readByteSlow call alone counts 57 of the 80 budget) keeps it from inlining,
// so the loops hand-inline the in-memory fast path and call readByteSlow
// directly on the cold branch. It is kept for the non-hot readers
// (initialization).
func (d *rangeDecoder) readByte() byte {
	pos := d.pos
	if pos >= len(d.buf) {
		return d.readByteSlow()
	}
	d.pos = pos + 1
	return d.buf[pos]
}

// readByteSlow serves the byte reads once the in-memory buffer is exhausted:
// it reads from the streaming fallback if present and records the sticky
// error otherwise. Kept out of the hot decode loops, which only call it on
// the cold branch of their hand-inlined fast path.
func (d *rangeDecoder) readByteSlow() byte {
	if d.err != nil {
		return 0
	}
	if d.br == nil {
		d.err = io.EOF
		return 0
	}
	c, err := d.br.ReadByte()
	if err != nil {
		d.err = err
		return 0
	}
	return c
}

// decodeBit decodes a single bit with threaded decoder state: range/code are
// passed in and returned in registers instead of being reloaded from and
// committed to the struct around every bit, so the call itself carries no
// memory traffic. The multi-bit decode loops hand-inline the same sequence;
// this function serves the isolated per-operation bits (readOp, the
// lengthCodec choice bits). The bit will be returned at the least-significant
// position; all other bits will be zero. The probability value will be
// updated.
func (d *rangeDecoder) decodeBit(p *prob, rng, code uint32) (b, nrng, ncode uint32) {
	b, rng, code = decodeBitArith(p, rng, code)
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
	return b, rng, code
}
