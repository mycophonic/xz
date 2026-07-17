// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lzma

import (
	"errors"
	"fmt"
	"io"
)

// decoder decodes a raw LZMA stream without any header.
type decoder struct {
	// dictionary; the rear pointer of the buffer will be used for
	// reading the data.
	Dict *decoderDict
	// decoder state
	State *state
	// range decoder
	rd *rangeDecoder
	// start stores the head value of the dictionary for the LZMA
	// stream
	start int64
	// size of uncompressed data
	size int64
	// end-of-stream encountered
	eos bool
	// EOS marker found
	eosMarker bool
}

// newDecoder creates a new decoder instance. The parameter size provides
// the expected byte size of the decompressed data. If the size is
// unknown use a negative value. In that case the decoder will look for
// a terminating end-of-stream marker.
func newDecoder(br io.ByteReader, state *state, dict *decoderDict, size int64) (d *decoder, err error) {
	rd, err := newRangeDecoder(br)
	if err != nil {
		return nil, err
	}
	d = &decoder{
		State: state,
		Dict:  dict,
		rd:    rd,
		size:  size,
		start: dict.pos(),
	}
	return d, nil
}

// Reopen restarts the decoder with a new byte reader and a new size. Reopen
// resets the Decompressed counter to zero.
func (d *decoder) Reopen(br io.ByteReader, size int64) error {
	var err error
	if d.rd, err = newRangeDecoder(br); err != nil {
		return err
	}
	d.start = d.Dict.pos()
	d.size = size
	d.eos = false
	return nil
}

// decodeLiteral decodes a single literal from the LZMA stream. The decoder
// state range/code is threaded through in registers (see readOp).
func (d *decoder) decodeLiteral(rng, code uint32) (op operation, nrng, ncode uint32) {
	litState := d.State.litState(d.Dict.byteAt(1), d.Dict.head)
	match := d.Dict.byteAt(int(d.State.rep[0]) + 1)
	s, rng, code := d.State.litCodec.decode(d.rd, d.State.state, match, litState, rng, code)
	return litOp(s), rng, code
}

// errEOS indicates that an EOS marker has been found.
var errEOS = errors.New("EOS marker found")

// readOp decodes the next operation from the compressed stream. It
// returns the operation. If an explicit end of stream marker is
// identified the eos error is returned.
//
// The range decoder state range/code is hoisted into locals here and threaded
// through every codec in registers, so it is loaded from and committed to the
// rangeDecoder struct once per operation instead of at every codec boundary.
// The codecs report no per-call errors; input failures are sticky on the
// rangeDecoder (rd.err) and are checked by the caller (decompress) after each
// operation.
func (d *decoder) readOp() (op operation, err error) {
	// Value of the end of stream (EOS) marker
	const eosDist = 1<<32 - 1

	state, state2, posState := d.State.states(d.Dict.head)

	rd := d.rd
	rng, code := rd.nrange, rd.code
	var b uint32
	b, rng, code = decodeBitArith(&d.State.isMatch[state2], rng, code)
	if rng < rcTop {
		rng <<= 8
		code <<= 8
		if pos := rd.pos; pos < len(rd.buf) {
			code |= uint32(rd.buf[pos])
			rd.pos = pos + 1
		} else {
			code |= uint32(rd.readByteSlow())
		}
	}
	if b == 0 {
		// literal
		op, rng, code = d.decodeLiteral(rng, code)
		rd.nrange, rd.code = rng, code
		d.State.updateStateLiteral()
		return op, nil
	}
	b, rng, code = decodeBitArith(&d.State.isRep[state], rng, code)
	if rng < rcTop {
		rng <<= 8
		code <<= 8
		if pos := rd.pos; pos < len(rd.buf) {
			code |= uint32(rd.buf[pos])
			rd.pos = pos + 1
		} else {
			code |= uint32(rd.readByteSlow())
		}
	}
	if b == 0 {
		// simple match
		d.State.rep[3], d.State.rep[2], d.State.rep[1] =
			d.State.rep[2], d.State.rep[1], d.State.rep[0]

		d.State.updateStateMatch()
		// The length decoder returns the length offset.
		var n uint32
		n, rng, code = d.State.lenCodec.decode(rd, posState, rng, code)
		// The dist decoder returns the distance offset. The actual
		// distance is 1 higher.
		d.State.rep[0], rng, code = d.State.distCodec.decode(rd, n, rng, code)
		rd.nrange, rd.code = rng, code
		if d.State.rep[0] == eosDist {
			d.eosMarker = true
			return operation{}, errEOS
		}
		op = matchOp(int64(d.State.rep[0])+minDistance, int(n)+minMatchLen)
		return op, nil
	}
	b, rng, code = decodeBitArith(&d.State.isRepG0[state], rng, code)
	if rng < rcTop {
		rng <<= 8
		code <<= 8
		if pos := rd.pos; pos < len(rd.buf) {
			code |= uint32(rd.buf[pos])
			rd.pos = pos + 1
		} else {
			code |= uint32(rd.readByteSlow())
		}
	}
	dist := d.State.rep[0]
	if b == 0 {
		// rep match 0
		b, rng, code = decodeBitArith(&d.State.isRepG0Long[state2], rng, code)
		if rng < rcTop {
			rng <<= 8
			code <<= 8
			if pos := rd.pos; pos < len(rd.buf) {
				code |= uint32(rd.buf[pos])
				rd.pos = pos + 1
			} else {
				code |= uint32(rd.readByteSlow())
			}
		}
		if b == 0 {
			rd.nrange, rd.code = rng, code
			d.State.updateStateShortRep()
			op = matchOp(int64(dist)+minDistance, 1)
			return op, nil
		}
	} else {
		b, rng, code = decodeBitArith(&d.State.isRepG1[state], rng, code)
		if rng < rcTop {
			rng <<= 8
			code <<= 8
			if pos := rd.pos; pos < len(rd.buf) {
				code |= uint32(rd.buf[pos])
				rd.pos = pos + 1
			} else {
				code |= uint32(rd.readByteSlow())
			}
		}
		if b == 0 {
			dist = d.State.rep[1]
		} else {
			b, rng, code = decodeBitArith(&d.State.isRepG2[state], rng, code)
			if rng < rcTop {
				rng <<= 8
				code <<= 8
				if pos := rd.pos; pos < len(rd.buf) {
					code |= uint32(rd.buf[pos])
					rd.pos = pos + 1
				} else {
					code |= uint32(rd.readByteSlow())
				}
			}
			if b == 0 {
				dist = d.State.rep[2]
			} else {
				dist = d.State.rep[3]
				d.State.rep[3] = d.State.rep[2]
			}
			d.State.rep[2] = d.State.rep[1]
		}
		d.State.rep[1] = d.State.rep[0]
		d.State.rep[0] = dist
	}
	var n uint32
	n, rng, code = d.State.repLenCodec.decode(rd, posState, rng, code)
	rd.nrange, rd.code = rng, code
	d.State.updateStateRep()
	op = matchOp(int64(dist)+minDistance, int(n)+minMatchLen)
	return op, nil
}

// apply takes the operation and transforms the decoder dictionary accordingly.
func (d *decoder) apply(op operation) error {
	if op.literal {
		return d.Dict.WriteByte(op.b)
	}
	return d.Dict.writeMatch(op.distance, op.n)
}

// decompress fills the dictionary unless no space for new data is
// available. If the end of the LZMA stream has been reached io.EOF will
// be returned.
func (d *decoder) decompress() error {
	if d.eos {
		return io.EOF
	}
	for d.Dict.Available() >= maxMatchLen {
		op, err := d.readOp()
		// The range decoder records input failures as a sticky error
		// instead of reporting them per byte (the decode loops are free
		// of error branches). An op decoded from missing input is
		// garbage, so the read error takes precedence over whatever
		// readOp returned.
		if d.rd.err != nil {
			err = d.rd.err
		}
		switch err {
		case nil:
			// break
		case errEOS:
			d.eos = true
			if !d.rd.possiblyAtEnd() {
				return errDataAfterEOS
			}
			if d.size >= 0 && d.size != d.Decompressed() {
				return errSize
			}
			return io.EOF
		case io.EOF:
			d.eos = true
			return io.ErrUnexpectedEOF
		default:
			return err
		}
		if err = d.apply(op); err != nil {
			return err
		}
		if d.size >= 0 && d.Decompressed() >= d.size {
			d.eos = true
			if d.Decompressed() > d.size {
				return errSize
			}
			if !d.rd.possiblyAtEnd() {
				_, err = d.readOp()
				if d.rd.err != nil {
					err = d.rd.err
				}
				switch err {
				case nil:
					return errSize
				case io.EOF:
					return io.ErrUnexpectedEOF
				case errEOS:
					break
				default:
					return err
				}
			}
			return io.EOF
		}
	}
	return nil
}

// Errors that may be returned while decoding data.
var (
	errDataAfterEOS = errors.New("lzma: data after end of stream marker")
	errSize         = errors.New("lzma: wrong uncompressed data size")
)

// Read reads data from the buffer. If no more data is available io.EOF is
// returned.
func (d *decoder) Read(p []byte) (n int, err error) {
	var k int
	for {
		// Read of decoder dict never returns an error.
		k, err = d.Dict.Read(p[n:])
		if err != nil {
			panic(fmt.Errorf("dictionary read error %s", err))
		}
		if k == 0 && d.eos {
			return n, io.EOF
		}
		n += k
		if n >= len(p) {
			return n, nil
		}
		if err = d.decompress(); err != nil && err != io.EOF {
			return n, err
		}
	}
}

// Decompressed returns the number of bytes decompressed by the decoder.
func (d *decoder) Decompressed() int64 {
	return d.Dict.pos() - d.start
}
