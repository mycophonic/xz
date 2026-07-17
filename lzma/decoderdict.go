// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lzma

import (
	"errors"
	"fmt"
)

// decoderDict provides the dictionary for the decoder. The whole
// dictionary is used as reader buffer.
type decoderDict struct {
	buf  buffer
	head int64
}

// newDecoderDict creates a new decoder dictionary. The whole dictionary
// will be used as reader buffer.
func newDecoderDict(dictCap int) (d *decoderDict, err error) {
	// lower limit supports easy test cases
	if !(1 <= dictCap && int64(dictCap) <= MaxDictCap) {
		return nil, errors.New("lzma: dictCap out of range")
	}
	d = &decoderDict{buf: *newBuffer(dictCap)}
	return d, nil
}

// Reset clears the dictionary. The read buffer is not changed, so the
// buffered data can still be read.
func (d *decoderDict) Reset() {
	d.head = 0
}

// WriteByte writes a single byte into the dictionary. It is used to
// write literals into the dictionary.
func (d *decoderDict) WriteByte(c byte) error {
	if err := d.buf.WriteByte(c); err != nil {
		return err
	}
	d.head++
	return nil
}

// pos returns the position of the dictionary head.
func (d *decoderDict) pos() int64 { return d.head }

// dictLen returns the actual length of the dictionary.
func (d *decoderDict) dictLen() int {
	capacity := d.buf.Cap()
	if d.head >= int64(capacity) {
		return capacity
	}
	return int(d.head)
}

// byteAt returns a byte stored in the dictionary. If the distance is
// non-positive or exceeds the current length of the dictionary the zero
// byte is returned.
func (d *decoderDict) byteAt(dist int) byte {
	if !(0 < dist && dist <= d.dictLen()) {
		return 0
	}
	i := d.buf.front - dist
	if i < 0 {
		i += len(d.buf.data)
	}
	return d.buf.data[i]
}

// writeMatch writes the match at the top of the dictionary. The given
// distance must point in the current dictionary and the length must not
// exceed the maximum length 273 supported in LZMA.
//
// The error value ErrNoSpace indicates that no space is available in
// the dictionary for writing. You need to read from the dictionary
// first.
func (d *decoderDict) writeMatch(dist int64, length int) error {
	if !(0 < dist && dist <= int64(d.dictLen())) {
		return errors.New("writeMatch: distance out of range")
	}
	if !(0 < length && length <= maxMatchLen) {
		return errors.New("writeMatch: length out of range")
	}
	if length > d.buf.Available() {
		return ErrNoSpace
	}
	d.head += int64(length)

	data := d.buf.data
	front := d.buf.front
	i := front - int(dist)
	// Fast path: neither the source run nor the destination run crosses
	// the physical end of the circular buffer, so the copy happens
	// directly on the underlying array without going through buf.Write
	// (which would recheck space and redo the wrap logic per segment).
	if i >= 0 && front+length <= len(data) {
		if length <= int(dist) {
			// no overlap
			copy(data[front:front+length], data[i:i+length])
		} else {
			// The match overlaps the write head: the stream repeats
			// the last dist bytes. Copy the dist-sized pattern once,
			// then double it from the freshly written destination
			// (source and destination of each copy never overlap).
			end := front + length
			k := copy(data[front:end], data[i:front])
			for k < length {
				k += copy(data[front+k:end], data[front:front+k])
			}
		}
		front += length
		if front == len(data) {
			front = 0
		}
		d.buf.front = front
		return nil
	}
	// Slow path: the source or destination wraps around the end of the
	// circular buffer. Rare (at most once per trip through the
	// dictionary).
	for length > 0 {
		var p []byte
		if i < 0 {
			i += len(data)
		}
		if i >= d.buf.front {
			p = data[i:]
			i = 0
		} else {
			p = data[i:d.buf.front]
			i = d.buf.front
		}
		if len(p) > length {
			p = p[:length]
		}
		if _, err := d.buf.Write(p); err != nil {
			panic(fmt.Errorf("d.buf.Write returned error %s", err))
		}
		length -= len(p)
	}
	return nil
}

// Write writes the given bytes into the dictionary and advances the
// head.
func (d *decoderDict) Write(p []byte) (n int, err error) {
	n, err = d.buf.Write(p)
	d.head += int64(n)
	return n, err
}

// Available returns the number of available bytes for writing into the
// decoder dictionary.
func (d *decoderDict) Available() int { return d.buf.Available() }

// Read reads data from the buffer contained in the decoder dictionary.
func (d *decoderDict) Read(p []byte) (n int, err error) { return d.buf.Read(p) }
