package store

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func journalFixture(t testing.TB) []byte {
	t.Helper()
	r := record{Schema: 1, Seq: 1, Op: "init", Branch: "main",
		New:   Head{Commit: strings.Repeat("a", 64), Version: 1},
		Actor: "system", RequestID: "genesis", Fingerprint: strings.Repeat("b", 64),
		Timestamp: "2026-10-04T00:00:00Z"}
	payload, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return encodeFrame(payload)
}

func TestJournalFramePrefixes(t *testing.T) {
	frame := journalFixture(t)
	for cut := 0; cut < len(frame); cut++ {
		_, err := readFrame(bytes.NewReader(frame[:cut]))
		want := io.ErrUnexpectedEOF
		if cut == 0 {
			want = io.EOF
		}
		if err != want {
			t.Fatalf("valid frame prefix %d: got %v, want %v", cut, err, want)
		}
	}
}

func TestJournalInvalidJSONPrefix(t *testing.T) {
	frame := journalFixture(t)
	binary.BigEndian.PutUint32(frame[4:8], binary.BigEndian.Uint32(frame[4:8])+1)
	frame[8] = '!'
	if _, err := readFrame(bytes.NewReader(frame)); Code(err) != "unavailable" {
		t.Fatalf("invalid JSON with inflated length treated as torn tail: %v", err)
	}
}

func FuzzJournalFrame(f *testing.F) {
	frame := journalFixture(f)
	f.Add([]byte{})
	f.Add(frame)
	f.Add(frame[:7])
	f.Add([]byte("BHJ1\xff\xff\xff\xff"))
	bad := bytes.Clone(frame)
	bad[len(bad)-1] ^= 1
	f.Add(bad)
	f.Fuzz(func(t *testing.T, input []byte) {
		// Keep disk-free parser and record generation bounded independently of
		// the fuzzer's input size. Header allocation remains capped by the core.
		if len(input) > 4096 {
			input = input[:4096]
		}
		payload, err := readFrame(bytes.NewReader(input))
		if err == nil {
			if len(input) < 12 || string(input[:4]) != "BHJ1" {
				t.Fatal("accepted invalid frame header")
			}
			length := int(binary.BigEndian.Uint32(input[4:8]))
			if length < 1 || length > MaxFrameBytes || length+12 > len(input) {
				t.Fatal("accepted incomplete or out-of-bounds frame")
			}
			if !bytes.Equal(payload, input[8:8+length]) ||
				crc32.Checksum(payload, crcTable) != binary.BigEndian.Uint32(input[8+length:12+length]) {
				t.Fatal("accepted payload or checksum mismatch")
			}
		}
		// Generate known valid records so rejecting all inputs cannot pass.
		idBytes := input
		if len(idBytes) > 32 {
			idBytes = idBytes[:32]
		}
		r := record{Schema: 1, Seq: 1, Op: "init", Branch: "main",
			New: Head{strings.Repeat("a", 64), 1}, Actor: "fixture",
			RequestID: "id-" + hex.EncodeToString(idBytes), Fingerprint: strings.Repeat("b", 64),
			Timestamp: "2026-10-04T00:00:00Z"}
		known, marshalErr := json.Marshal(r)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		generated := encodeFrame(known)
		decoded, decodeErr := readFrame(bytes.NewReader(generated))
		if decodeErr != nil || !bytes.Equal(decoded, known) {
			t.Fatalf("valid frame round trip failed: %v", decodeErr)
		}
		cut := 0
		if len(input) > 0 {
			cut = int(input[len(input)-1]) * (len(generated) - 1) / 255
		}
		_, decodeErr = readFrame(bytes.NewReader(generated[:cut]))
		want := io.ErrUnexpectedEOF
		if cut == 0 {
			want = io.EOF
		}
		if decodeErr != want {
			t.Fatalf("incomplete generated frame: %v", decodeErr)
		}
		generated[len(generated)-1] ^= 1
		if _, decodeErr = readFrame(bytes.NewReader(generated)); Code(decodeErr) != "unavailable" {
			t.Fatal("complete checksum corruption accepted")
		}
	})
}

// This isolates framing recovery using a regular file. It does not claim
// Linux store ownership or durable acknowledged-state reproduction.
func TestJournalLengthCorruption(t *testing.T) {
	for _, extra := range []uint32{1, 512} {
		for _, badCRC := range []bool{false, true} {
			frame := journalFixture(t)
			binary.BigEndian.PutUint32(frame[4:8], binary.BigEndian.Uint32(frame[4:8])+extra)
			if badCRC {
				frame[len(frame)-1] ^= 1
			}
			path := filepath.Join(t.TempDir(), "journal")
			if err := os.WriteFile(path, frame, 0600); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(path, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			s := &Store{journal: file, heads: map[string]Head{}, retries: map[string]retry{}}
			err = s.recover()
			if closeErr := file.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err == nil || Code(err) != "unavailable" {
				t.Fatalf("complete frame with inflated length accepted: extra=%d badCRC=%v err=%v", extra, badCRC, err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, frame) {
				t.Fatal("corruption silently rewritten")
			}
		}
	}
}
