// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"bytes"
	"io"
	"math/rand"
	"testing"
)

// parallelTestData generates compressible data with literals, matches
// and runs.
func parallelTestData(n int) []byte {
	rng := rand.New(rand.NewSource(7))
	words := []string{"the ", "quick ", "brown ", "fox ", "jumps ",
		"over ", "lazy ", "dog ", "0000000000000000", "\n"}
	var buf bytes.Buffer
	for buf.Len() < n {
		buf.WriteString(words[rng.Intn(len(words))])
	}
	return buf.Bytes()[:n]
}

// compressMultiBlock compresses data into an xz stream with the given
// block size.
func compressMultiBlock(t *testing.T, data []byte, blockSize int64) []byte {
	var buf bytes.Buffer
	w, err := WriterConfig{BlockSize: blockSize}.NewWriter(&buf)
	if err != nil {
		t.Fatalf("NewWriter error %s", err)
	}
	if _, err = w.Write(data); err != nil {
		t.Fatalf("Write error %s", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("Close error %s", err)
	}
	return buf.Bytes()
}

func testParallelRead(t *testing.T, xz []byte, want []byte, workers int) {
	c := ParallelReaderConfig{Workers: workers}
	r, err := c.NewParallelReader(bytes.NewReader(xz), int64(len(xz)))
	if err != nil {
		t.Fatalf("NewParallelReader error %s", err)
	}
	if r.Size() != int64(len(want)) {
		t.Fatalf("Size() is %d; want %d", r.Size(), len(want))
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll error %s", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded data differs from original")
	}
}

func TestParallelReaderMultiBlock(t *testing.T) {
	data := parallelTestData(1 << 20)
	xz := compressMultiBlock(t, data, 64<<10)
	for _, workers := range []int{0, 1, 4} {
		testParallelRead(t, xz, data, workers)
	}
}

func TestParallelReaderSingleBlock(t *testing.T) {
	data := parallelTestData(1 << 18)
	xz := compressMultiBlock(t, data, 0) // default: one block
	testParallelRead(t, xz, data, 4)
}

func TestParallelReaderMultiStream(t *testing.T) {
	a := parallelTestData(1 << 19)
	b := parallelTestData(1 << 18)
	xza := compressMultiBlock(t, a, 32<<10)
	xzb := compressMultiBlock(t, b, 32<<10)
	// concatenated streams with stream padding in between
	pad := make([]byte, 8)
	file := append(append(append([]byte{}, xza...), pad...), xzb...)
	testParallelRead(t, file, append(append([]byte{}, a...), b...), 3)
}

func TestParallelReaderEmpty(t *testing.T) {
	xz := compressMultiBlock(t, nil, 64<<10)
	testParallelRead(t, xz, nil, 2)
}

func TestParallelReaderWriteTo(t *testing.T) {
	data := parallelTestData(1 << 20)
	xz := compressMultiBlock(t, data, 64<<10)
	r, err := NewParallelReader(bytes.NewReader(xz), int64(len(xz)))
	if err != nil {
		t.Fatalf("NewParallelReader error %s", err)
	}
	var buf bytes.Buffer
	n, err := r.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo error %s", err)
	}
	if n != int64(len(data)) || !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("WriteTo result differs from original")
	}
}

func TestParallelReaderAgainstReader(t *testing.T) {
	data := parallelTestData(1 << 20)
	xz := compressMultiBlock(t, data, 128<<10)
	sr, err := NewReader(bytes.NewReader(xz))
	if err != nil {
		t.Fatalf("NewReader error %s", err)
	}
	want, err := io.ReadAll(sr)
	if err != nil {
		t.Fatalf("streaming ReadAll error %s", err)
	}
	testParallelRead(t, xz, want, 4)
}

func TestParallelReaderTruncated(t *testing.T) {
	data := parallelTestData(1 << 20)
	xz := compressMultiBlock(t, data, 64<<10)
	// missing footer
	if _, err := NewParallelReader(bytes.NewReader(xz[:len(xz)-4]),
		int64(len(xz)-4)); err == nil {
		t.Fatalf("NewParallelReader on truncated file: no error")
	}
	// corrupt a byte in the middle of some block
	bad := append([]byte{}, xz...)
	bad[len(bad)/2] ^= 0xff
	r, err := NewParallelReader(bytes.NewReader(bad), int64(len(bad)))
	if err != nil {
		// corruption may already be detected during parsing
		return
	}
	if _, err = io.ReadAll(r); err == nil {
		t.Fatalf("ReadAll on corrupted file: no error")
	}
	r.Close()
}

func TestParallelReaderClose(t *testing.T) {
	data := parallelTestData(1 << 20)
	xz := compressMultiBlock(t, data, 16<<10)
	r, err := NewParallelReader(bytes.NewReader(xz), int64(len(xz)))
	if err != nil {
		t.Fatalf("NewParallelReader error %s", err)
	}
	p := make([]byte, 100)
	if _, err = io.ReadFull(r, p); err != nil {
		t.Fatalf("Read error %s", err)
	}
	if err = r.Close(); err != nil {
		t.Fatalf("Close error %s", err)
	}
	if _, err = r.Read(p); err != errReaderClosed {
		t.Fatalf("Read after Close returned %v; want %v",
			err, errReaderClosed)
	}
	// closing an unstarted reader must not panic
	r2, err := NewParallelReader(bytes.NewReader(xz), int64(len(xz)))
	if err != nil {
		t.Fatalf("NewParallelReader error %s", err)
	}
	if err = r2.Close(); err != nil {
		t.Fatalf("Close error %s", err)
	}
}
