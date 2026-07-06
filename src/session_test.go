package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestSnapshotRing_WrappedDropsLeadingPartialLine(t *testing.T) {
	s := newSession("agent1", "sid1", "/tmp", "")

	// Write more bytes than the ring can hold so eviction occurs.
	// Use a repeating pattern with newlines so we can reason about
	// the retained window.
	line := strings.Repeat("a", 63) + "\n" // 64 bytes per line
	total := ringSize + ringSize/2         // 1.5x the ring
	buf := make([]byte, 0, total)
	for len(buf) < total {
		buf = append(buf, line...)
	}
	buf = buf[:total]
	s.writeRing(buf)

	got := s.snapshotRing()
	if len(got) == 0 {
		t.Fatalf("expected non-empty snapshot after wrap with newlines present")
	}

	// Reconstruct the retained window (the last ringSize bytes written)
	// and find the first newline in it — the snapshot should start with
	// the byte immediately after that newline.
	window := buf[len(buf)-ringSize:]
	firstNL := bytes.IndexByte(window, '\n')
	if firstNL < 0 {
		t.Fatalf("test setup: expected a newline in retained window")
	}
	if len(got) != len(window)-(firstNL+1) {
		t.Fatalf("length mismatch: got %d, want %d", len(got), len(window)-(firstNL+1))
	}
	if got[0] != window[firstNL+1] {
		t.Fatalf("first byte mismatch: got %q, want %q (byte after first newline in retained window)", got[0], window[firstNL+1])
	}
}

func TestSnapshotRing_NoWrapReturnsFullBuffer(t *testing.T) {
	s := newSession("agent1", "sid1", "/tmp", "")
	data := []byte("no newline here, buffer has not wrapped")
	s.writeRing(data)

	got := s.snapshotRing()
	if !bytes.Equal(got, data) {
		t.Fatalf("expected full buffer unchanged when not wrapped, got %q", got)
	}
}

func TestSnapshotRing_WrappedNoNewlineReturnsEmpty(t *testing.T) {
	s := newSession("agent1", "sid1", "/tmp", "")
	// Fill and overflow ring with bytes that contain no newline.
	blob := bytes.Repeat([]byte{'x'}, ringSize+128)
	s.writeRing(blob)

	got := s.snapshotRing()
	if len(got) != 0 {
		t.Fatalf("expected empty snapshot when wrapped buffer has no newline, got %d bytes", len(got))
	}
}
