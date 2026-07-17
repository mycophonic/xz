// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lzma

import (
	"errors"
	"io"

	"github.com/mycophonic/xz/internal/xlog"
)

// Reader2Config stores the parameters for the LZMA2 reader.
// format.
type Reader2Config struct {
	DictCap int
}

// fill converts the zero values of the configuration to the default values.
func (c *Reader2Config) fill() {
	if c.DictCap == 0 {
		c.DictCap = 8 * 1024 * 1024
	}
}

// Verify checks the reader configuration for errors. Zero configuration values
// will be replaced by default values.
func (c *Reader2Config) Verify() error {
	c.fill()
	if !(MinDictCap <= c.DictCap && int64(c.DictCap) <= MaxDictCap) {
		return errors.New("lzma: dictionary capacity is out of range")
	}
	return nil
}

// Reader2 supports the reading of LZMA2 chunk sequences. Note that the
// first chunk should have a dictionary reset and the first compressed
// chunk a properties reset. The chunk sequence may not be terminated by
// an end-of-stream chunk.
type Reader2 struct {
	r   io.Reader
	err error

	dict        *decoderDict
	ur          *uncompressedReader
	decoder     *decoder
	chunkReader io.Reader

	// reusable buffer and reader that hold one compressed chunk in memory
	// so the range decoder reads bytes by index instead of paying a
	// per-byte interface Read plus memmove through a streaming breader.
	// newRangeDecoder recognizes the byteSliceReader and adopts the slice
	// directly for its call-free read path.
	compBuf []byte
	compRd  byteSliceReader

	cstate chunkState
}

// NewReader2 creates a reader for an LZMA2 chunk sequence.
func NewReader2(lzma2 io.Reader) (r *Reader2, err error) {
	return Reader2Config{}.NewReader2(lzma2)
}

// NewReader2 creates an LZMA2 reader using the given configuration.
func (c Reader2Config) NewReader2(lzma2 io.Reader) (r *Reader2, err error) {
	if err = c.Verify(); err != nil {
		return nil, err
	}
	r = &Reader2{r: lzma2, cstate: start}
	r.dict, err = newDecoderDict(c.DictCap)
	if err != nil {
		return nil, err
	}
	if err = r.startChunk(); err != nil {
		r.err = err
	}
	return r, nil
}

// uncompressed tests whether the chunk type specifies an uncompressed
// chunk.
func uncompressed(ctype chunkType) bool {
	return ctype == cU || ctype == cUD
}

// startChunk parses a new chunk.
func (r *Reader2) startChunk() error {
	r.chunkReader = nil
	header, err := readChunkHeader(r.r)
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	xlog.Debugf("chunk header %v", header)
	if err = r.cstate.next(header.ctype); err != nil {
		return err
	}
	if r.cstate == stop {
		return io.EOF
	}
	if header.ctype == cUD || header.ctype == cLRND {
		r.dict.Reset()
	}
	size := int64(header.uncompressed) + 1
	if uncompressed(header.ctype) {
		if r.ur != nil {
			r.ur.Reopen(r.r, size)
		} else {
			r.ur = newUncompressedReader(r.r, r.dict, size)
		}
		r.chunkReader = r.ur
		return nil
	}
	// Buffer the whole compressed chunk. The stored size field is the
	// actual length minus one, so the chunk is header.compressed+1 bytes
	// (matching the previous LimitReader bound). io.ReadFull advances r.r
	// by exactly that many bytes, keeping the next chunk header aligned,
	// and serving the range decoder from an in-memory bytes.Reader avoids
	// the per-byte Read plus memmove that the streaming breader incurred.
	n := int(header.compressed) + 1
	if cap(r.compBuf) < n {
		r.compBuf = make([]byte, n)
	}
	r.compBuf = r.compBuf[:n]
	if _, err = io.ReadFull(r.r, r.compBuf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	r.compRd = byteSliceReader{buf: r.compBuf}
	br := &r.compRd
	if r.decoder == nil {
		state := newState(header.props)
		r.decoder, err = newDecoder(br, state, r.dict, size)
		if err != nil {
			return err
		}
		r.chunkReader = r.decoder
		return nil
	}
	switch header.ctype {
	case cLR:
		r.decoder.State.Reset()
	case cLRN, cLRND:
		r.decoder.State = newState(header.props)
	}
	err = r.decoder.Reopen(br, size)
	if err != nil {
		return err
	}
	r.chunkReader = r.decoder
	return nil
}

// Read reads data from the LZMA2 chunk sequence.
func (r *Reader2) Read(p []byte) (n int, err error) {
	if r.err != nil {
		return 0, r.err
	}
	for n < len(p) {
		var k int
		k, err = r.chunkReader.Read(p[n:])
		n += k
		if err != nil {
			if err == io.EOF {
				err = r.startChunk()
				if err == nil {
					continue
				}
			}
			r.err = err
			return n, err
		}
		if k == 0 {
			r.err = errors.New("lzma: Reader2 doesn't get data")
			return n, r.err
		}
	}
	return n, nil
}

// EOS returns whether the LZMA2 stream has been terminated by an
// end-of-stream chunk.
func (r *Reader2) EOS() bool {
	return r.cstate == stop
}

// uncompressedReader is used to read uncompressed chunks.
type uncompressedReader struct {
	lr   io.LimitedReader
	Dict *decoderDict
	eof  bool
	err  error
}

// newUncompressedReader initializes a new uncompressedReader.
func newUncompressedReader(r io.Reader, dict *decoderDict, size int64) *uncompressedReader {
	ur := &uncompressedReader{
		lr:   io.LimitedReader{R: r, N: size},
		Dict: dict,
	}
	return ur
}

// Reopen reinitializes an uncompressed reader.
func (ur *uncompressedReader) Reopen(r io.Reader, size int64) {
	ur.err = nil
	ur.eof = false
	ur.lr = io.LimitedReader{R: r, N: size}
}

// fill reads uncompressed data into the dictionary.
func (ur *uncompressedReader) fill() error {
	if !ur.eof {
		n, err := io.CopyN(ur.Dict, &ur.lr, int64(ur.Dict.Available()))
		if err != io.EOF {
			return err
		}
		ur.eof = true
		if n > 0 {
			return nil
		}
	}
	if ur.lr.N != 0 {
		return io.ErrUnexpectedEOF
	}
	return io.EOF
}

// Read reads uncompressed data from the limited reader.
func (ur *uncompressedReader) Read(p []byte) (n int, err error) {
	if ur.err != nil {
		return 0, ur.err
	}
	for {
		var k int
		k, err = ur.Dict.Read(p[n:])
		n += k
		if n >= len(p) {
			return n, nil
		}
		if err != nil {
			break
		}
		err = ur.fill()
		if err != nil {
			break
		}
	}
	ur.err = err
	return n, err
}
