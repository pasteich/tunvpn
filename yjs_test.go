package main

import (
	"bytes"
	"testing"
)

func TestBinaryAppendsRoundTrip(t *testing.T) {
	chunks := [][]byte{
		[]byte("hello"),
		{0x00, 0x01, 0x02, 0xff, 0xfe}, // raw bytes incl. NUL and high bytes
		bytes.Repeat([]byte{0xAB}, 700),
	}
	// first update ever: startClock 0, carries the root parent
	up := encodeBinaryAppends(42, 0, "tun", chunks)
	items, ok := decodeYjsUpdate(up)
	if !ok {
		t.Fatalf("decode failed")
	}
	if len(items) != len(chunks) {
		t.Fatalf("want %d items, got %d", len(chunks), len(items))
	}
	for i, it := range items {
		if it.client != 42 {
			t.Errorf("item %d client=%d", i, it.client)
		}
		if it.clock != uint64(i) {
			t.Errorf("item %d clock=%d want %d", i, it.clock, i)
		}
		if it.ref != yContentBinary {
			t.Errorf("item %d ref=%d", i, it.ref)
		}
		if !bytes.Equal(it.data, chunks[i]) {
			t.Errorf("item %d data mismatch", i)
		}
	}
}

func TestBinaryAppendsContinuation(t *testing.T) {
	// a later update: startClock 5, all items chain off previous (origin set)
	chunks := [][]byte{[]byte("a"), []byte("bb"), []byte("ccc")}
	up := encodeBinaryAppends(7, 5, "tun", chunks)
	items, ok := decodeYjsUpdate(up)
	if !ok {
		t.Fatalf("decode failed")
	}
	if len(items) != 3 {
		t.Fatalf("want 3 items, got %d", len(items))
	}
	for i, it := range items {
		if it.clock != uint64(5+i) {
			t.Errorf("item %d clock=%d want %d", i, it.clock, 5+i)
		}
		if !bytes.Equal(it.data, chunks[i]) {
			t.Errorf("item %d data mismatch: %q vs %q", i, it.data, chunks[i])
		}
	}
}
