// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"bufio"
	"errors"
	"hash"
	"io"
	"runtime"
	"sync"
)

// ParallelReaderConfig defines the parameters for the parallel xz
// reader. Workers is the number of blocks decoded concurrently; values
// below 1 select runtime.GOMAXPROCS(0).
type ParallelReaderConfig struct {
	DictCap int
	Workers int
}

// Verify checks the configuration for errors. Zero values will be
// replaced by default values.
func (c *ParallelReaderConfig) Verify() error {
	if c == nil {
		return errors.New("xz: parallel reader parameters are nil")
	}
	rc := ReaderConfig{DictCap: c.DictCap}
	if err := rc.Verify(); err != nil {
		return err
	}
	if c.Workers < 1 {
		c.Workers = runtime.GOMAXPROCS(0)
	}
	return nil
}

// blockDesc describes the location of a single block inside the xz
// file, as derived from the stream indexes.
type blockDesc struct {
	// file offset of the block header
	offset int64
	// size of header, compressed data and check value, without padding
	unpaddedSize int64
	// size of the uncompressed block data
	uncompressedSize int64
	// constructor for the check of the containing stream
	newHash func() hash.Hash
}

// paddedSize returns the total size of the block in the file.
func (d *blockDesc) paddedSize() int64 {
	return d.unpaddedSize + int64(padLen(d.unpaddedSize))
}

// ParallelReader decodes the blocks of an xz file concurrently. It
// requires random access to the input (io.ReaderAt) and its total size,
// because the block locations are read from the stream indexes at the
// end of each stream before any data is decoded. The decoded stream is
// presented in order through the io.Reader (or io.WriterTo) interface.
//
// Only files consisting of multiple blocks — as produced for example by
// xz with a block size limit or in multi-threaded mode, or by this
// package's writer with WriterConfig.BlockSize — decode with real
// concurrency; a single-block file decodes on one worker. Memory usage
// is proportional to Workers times the uncompressed block size.
//
// The ParallelReader verifies the block checks, the block sizes against
// the index, and the header, footer and index checksums of every
// stream. It is not safe for concurrent use.
type ParallelReader struct {
	ParallelReaderConfig

	xz     io.ReaderAt
	blocks []blockDesc
	size   int64

	started   bool
	queue     chan *blockWork
	jobs      chan *blockWork
	done      chan struct{}
	closeOnce sync.Once
	bufPool   chan []byte

	cur    []byte
	curPos int
	err    error
}

// blockResult is the outcome of decoding one block.
type blockResult struct {
	data []byte
	err  error
}

// blockWork is a unit of work handed to a decode worker. The result
// channel has capacity one, so a worker never blocks delivering.
type blockWork struct {
	d      blockDesc
	result chan blockResult
}

// NewParallelReader creates a reader that decodes the blocks of an xz
// file concurrently using the default parameters. See ParallelReader
// for the conditions under which this actually parallelizes.
func NewParallelReader(xz io.ReaderAt, size int64) (r *ParallelReader, err error) {
	return ParallelReaderConfig{}.NewParallelReader(xz, size)
}

// NewParallelReader creates a new parallel reader using the given
// configuration. It reads and verifies the stream headers, footers and
// indexes, but does not decode any block data yet.
func (c ParallelReaderConfig) NewParallelReader(xz io.ReaderAt, size int64) (r *ParallelReader, err error) {
	if err = c.Verify(); err != nil {
		return nil, err
	}
	blocks, total, err := parseBlocks(xz, size)
	if err != nil {
		return nil, err
	}
	return &ParallelReader{
		ParallelReaderConfig: c,
		xz:                   xz,
		blocks:               blocks,
		size:                 total,
	}, nil
}

// Size returns the total number of uncompressed bytes in the file, as
// recorded in the stream indexes.
func (r *ParallelReader) Size() int64 { return r.size }

// parseBlocks locates all blocks of the xz file by walking the streams
// backwards from the end of the file: footer, index, stream header.
// The header, footer and index checksums of every stream are verified.
func parseBlocks(xz io.ReaderAt, size int64) (blocks []blockDesc, total int64, err error) {
	streams := make([][]blockDesc, 0, 1)
	pos := size
	for pos > 0 {
		// skip stream padding (groups of four zero bytes)
		var p [4]byte
		if pos%4 != 0 {
			return nil, 0, errors.New("xz: file size not a multiple of four bytes")
		}
		if _, err = xz.ReadAt(p[:], pos-4); err != nil {
			return nil, 0, err
		}
		if allZeros(p[:]) {
			pos -= 4
			continue
		}

		// footer
		if pos < HeaderLen+footerLen {
			return nil, 0, errors.New("xz: stream truncated")
		}
		fdata := make([]byte, footerLen)
		if _, err = xz.ReadAt(fdata, pos-footerLen); err != nil {
			return nil, 0, err
		}
		var f footer
		if err = f.UnmarshalBinary(fdata); err != nil {
			return nil, 0, err
		}

		// index
		indexStart := pos - footerLen - f.indexSize
		if indexStart < HeaderLen {
			return nil, 0, errors.New("xz: index size exceeds stream")
		}
		ir := bufio.NewReader(io.NewSectionReader(xz, indexStart, f.indexSize))
		c, err := ir.ReadByte()
		if err != nil {
			return nil, 0, err
		}
		if c != 0 {
			return nil, 0, errors.New("xz: index indicator missing")
		}
		records, n, err := readIndexBody(ir, -1)
		if err != nil {
			return nil, 0, err
		}
		if n+1 != f.indexSize {
			return nil, 0, errors.New("xz: index size does not match footer")
		}

		// stream header
		var blocksLen int64
		for _, rec := range records {
			if rec.unpaddedSize <= 0 {
				return nil, 0, errors.New("xz: invalid unpadded size in index")
			}
			blocksLen += rec.unpaddedSize + int64(padLen(rec.unpaddedSize))
		}
		headerPos := indexStart - blocksLen - HeaderLen
		if headerPos < 0 {
			return nil, 0, errors.New("xz: blocks exceed stream size")
		}
		hdata := make([]byte, HeaderLen)
		if _, err = xz.ReadAt(hdata, headerPos); err != nil {
			return nil, 0, err
		}
		var h header
		if err = h.UnmarshalBinary(hdata); err != nil {
			return nil, 0, err
		}
		if h.flags != f.flags {
			return nil, 0, errors.New("xz: stream header and footer flags differ")
		}
		newHash, err := newHashFunc(h.flags)
		if err != nil {
			return nil, 0, err
		}

		descs := make([]blockDesc, len(records))
		off := headerPos + HeaderLen
		for i, rec := range records {
			descs[i] = blockDesc{
				offset:           off,
				unpaddedSize:     rec.unpaddedSize,
				uncompressedSize: rec.uncompressedSize,
				newHash:          newHash,
			}
			off += descs[i].paddedSize()
		}
		streams = append(streams, descs)
		pos = headerPos
	}
	if len(streams) == 0 {
		return nil, 0, errors.New("xz: no streams found")
	}
	// streams were found back to front
	for i := len(streams) - 1; i >= 0; i-- {
		for _, d := range streams[i] {
			total += d.uncompressedSize
			blocks = append(blocks, d)
		}
	}
	return blocks, total, nil
}

// errReaderClosed is returned by Read after Close has been called.
var errReaderClosed = errors.New("xz: parallel reader is closed")

// start launches the dispatcher and the decode workers. The queue
// capacity bounds the number of blocks in flight (decoding or decoded
// but not yet consumed) and thereby the memory use.
func (r *ParallelReader) start() {
	r.started = true
	r.queue = make(chan *blockWork, r.Workers+2)
	r.jobs = make(chan *blockWork)
	r.done = make(chan struct{})
	r.bufPool = make(chan []byte, cap(r.queue)+1)
	for i := 0; i < r.Workers; i++ {
		go r.worker()
	}
	go r.dispatch()
}

// dispatch feeds the blocks to the workers in file order. Admission to
// the ordered queue (bounded capacity) throttles how many blocks are in
// flight.
func (r *ParallelReader) dispatch() {
	defer close(r.jobs)
	for i := range r.blocks {
		w := &blockWork{
			d:      r.blocks[i],
			result: make(chan blockResult, 1),
		}
		select {
		case r.queue <- w:
		case <-r.done:
			return
		}
		select {
		case r.jobs <- w:
		case <-r.done:
			return
		}
	}
	close(r.queue)
}

// worker decodes blocks until the job channel is closed.
func (r *ParallelReader) worker() {
	for w := range r.jobs {
		buf := r.getBuf(int(w.d.uncompressedSize))
		data, err := r.decodeBlock(&w.d, buf)
		if err != nil {
			r.putBuf(buf)
			data = nil
		}
		w.result <- blockResult{data: data, err: err}
	}
}

// getBuf returns a decode buffer of length n, reusing a pooled buffer
// if one of sufficient capacity is available.
func (r *ParallelReader) getBuf(n int) []byte {
	select {
	case b := <-r.bufPool:
		if cap(b) >= n {
			return b[:n]
		}
	default:
	}
	return make([]byte, n)
}

// putBuf returns a buffer to the pool, dropping it if the pool is full.
func (r *ParallelReader) putBuf(b []byte) {
	if b == nil {
		return
	}
	select {
	case r.bufPool <- b:
	default:
	}
}

// blockReadBufSize is the size of the buffered reader each worker
// places over its section of the file to batch the small reads of the
// block and chunk headers.
const blockReadBufSize = 256 << 10

// decodeBlock decodes a single block into buf, which must have the
// uncompressed size of the block recorded in the index. It verifies the
// block check and that header, compressed size and uncompressed size
// agree with the index record.
func (r *ParallelReader) decodeBlock(d *blockDesc, buf []byte) ([]byte, error) {
	sr := io.NewSectionReader(r.xz, d.offset, d.paddedSize())
	xr := bufio.NewReaderSize(sr, blockReadBufSize)

	h, hlen, err := readBlockHeader(xr)
	if err != nil {
		return nil, err
	}
	c := ReaderConfig{DictCap: r.DictCap}
	br, err := c.newBlockReader(xr, h, hlen, d.newHash())
	if err != nil {
		return nil, err
	}
	if _, err = io.ReadFull(br, buf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	// The block must end exactly here; the final Read triggers the
	// padding and check verification in the block reader.
	var tmp [1]byte
	n, err := br.Read(tmp[:])
	if n != 0 || err == nil {
		return nil, errors.New("xz: block longer than index record")
	}
	if err != io.EOF {
		return nil, err
	}
	if br.record() != (record{d.unpaddedSize, d.uncompressedSize}) {
		return nil, errors.New("xz: block sizes do not match index record")
	}
	return buf, nil
}

// nextBlock retires the current buffer and blocks until the next
// decoded block is available. It returns io.EOF after the last block.
func (r *ParallelReader) nextBlock() error {
	r.putBuf(r.cur)
	r.cur = nil
	r.curPos = 0
	w, ok := <-r.queue
	if !ok {
		return io.EOF
	}
	res := <-w.result
	if res.err != nil {
		return res.err
	}
	r.cur = res.data
	return nil
}

// Read reads the uncompressed data stream. The blocks are decoded
// concurrently but delivered in order.
func (r *ParallelReader) Read(p []byte) (n int, err error) {
	if r.err != nil {
		return 0, r.err
	}
	if !r.started {
		r.start()
	}
	for n < len(p) {
		if r.curPos == len(r.cur) {
			if err = r.nextBlock(); err != nil {
				r.err = err
				if err != io.EOF {
					r.stop()
				}
				return n, err
			}
			continue
		}
		k := copy(p[n:], r.cur[r.curPos:])
		n += k
		r.curPos += k
	}
	return n, nil
}

// WriteTo writes the whole remaining uncompressed data stream to w. It
// avoids the intermediate copy of the Read interface by handing the
// decoded block buffers directly to the writer.
func (r *ParallelReader) WriteTo(w io.Writer) (n int64, err error) {
	if r.err != nil {
		return 0, r.err
	}
	if !r.started {
		r.start()
	}
	for {
		if r.curPos == len(r.cur) {
			if err = r.nextBlock(); err != nil {
				r.err = err
				if err == io.EOF {
					return n, nil
				}
				r.stop()
				return n, err
			}
			continue
		}
		k, err := w.Write(r.cur[r.curPos:])
		n += int64(k)
		r.curPos += k
		if err != nil {
			r.err = err
			r.stop()
			return n, err
		}
	}
}

// stop terminates the dispatcher and lets the workers drain.
func (r *ParallelReader) stop() {
	r.closeOnce.Do(func() {
		if r.done != nil {
			close(r.done)
		}
	})
}

// Close stops the background workers. It must be called when the
// reader is abandoned before io.EOF was reached; it is a no-op
// otherwise. The error is always nil.
func (r *ParallelReader) Close() error {
	r.stop()
	if r.err == nil {
		r.err = errReaderClosed
	}
	return nil
}
