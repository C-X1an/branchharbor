package store

import (
	"branchharbor/internal/strictjson"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"io"
	"time"
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

type record struct {
	Schema      int    `json:"schema"`
	Seq         uint64 `json:"seq"`
	Op          string `json:"op"`
	Branch      string `json:"branch"`
	Old         Head   `json:"old"`
	New         Head   `json:"new"`
	Actor       string `json:"actor"`
	RequestID   string `json:"request_id"`
	Fingerprint string `json:"fingerprint"`
	PrevHash    string `json:"prev_hash"`
	Timestamp   string `json:"timestamp"`
}
type retry struct {
	Fingerprint string
	Head        Head
}

func payloadHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func encodeFrame(payload []byte) []byte {
	b := make([]byte, 8+len(payload)+4)
	copy(b, "BHJ1")
	binary.BigEndian.PutUint32(b[4:8], uint32(len(payload)))
	copy(b[8:], payload)
	binary.BigEndian.PutUint32(b[8+len(payload):], crc32.Checksum(payload, crcTable))
	return b
}

// readFrame leaves recovery/truncation to the owner. ErrUnexpectedEOF means
// the bytes could be a writer-produced prefix; detectable corruption is refused.
// Format1 cannot distinguish every damaged complete frame from a genuine prefix.
func readFrame(reader io.Reader) ([]byte, error) {
	header := make([]byte, 8)
	n, err := io.ReadFull(reader, header)
	if err == io.EOF && n == 0 {
		return nil, io.EOF
	}
	if err != nil {
		if err == io.ErrUnexpectedEOF && string(header[:min(n, 4)]) != "BHJ1"[:min(n, 4)] {
			return nil, fail("unavailable", "invalid partial journal magic")
		}
		return nil, err
	}
	if string(header[:4]) != "BHJ1" {
		return nil, fail("unavailable", "invalid journal magic")
	}
	length := binary.BigEndian.Uint32(header[4:])
	if length == 0 || length > MaxFrameBytes {
		return nil, fail("unavailable", "invalid journal frame length")
	}
	tail := make([]byte, int(length)+4)
	n, err = io.ReadFull(reader, tail)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		// Format1 does not checksum its header. A complete JSON value followed
		// by four checksum bytes still identifies a complete frame when a
		// damaged length overstates its size, even if the CRC is also damaged.
		decoder := json.NewDecoder(bytes.NewReader(tail[:n]))
		var value json.RawMessage
		decodeErr := decoder.Decode(&value)
		if decodeErr == nil && int64(n)-decoder.InputOffset() >= 4 {
			return nil, fail("unavailable", "journal length contradicts complete payload")
		}
		if decodeErr != nil && decodeErr != io.EOF && decodeErr != io.ErrUnexpectedEOF {
			return nil, fail("unavailable", "invalid partial journal JSON")
		}
		return nil, io.ErrUnexpectedEOF
	}
	if err != nil {
		return nil, err
	}
	payload := tail[:length]
	if crc32.Checksum(payload, crcTable) != binary.BigEndian.Uint32(tail[length:]) {
		return nil, fail("unavailable", "journal checksum mismatch")
	}
	return payload, nil
}
func (s *Store) validateRecord(r record) error {
	if r.Schema != 1 || r.Seq != s.seq+1 || r.PrevHash != s.prevHash || !ValidBranch(r.Branch) || !ValidID(r.Actor) || !ValidID(r.RequestID) || !ValidDigest(r.Fingerprint) || !ValidDigest(r.New.Commit) || r.New.Version == 0 {
		return fail("unavailable", "journal record invariant failure")
	}
	if _, err := time.Parse(time.RFC3339Nano, r.Timestamp); err != nil {
		return fail("unavailable", "invalid journal timestamp")
	}
	old, exists := s.heads[r.Branch]
	if old != r.Old {
		return fail("unavailable", "journal old head mismatch")
	}
	switch r.Op {
	case "init":
		if s.seq != 0 || r.Branch != "main" || exists || r.New.Version != 1 {
			return fail("unavailable", "invalid genesis")
		}
	case "fork":
		if !exists && r.New.Version == 1 && len(s.heads) < MaxBranches {
		} else {
			return fail("unavailable", "invalid fork record")
		}
	case "commit", "merge", "restore":
		if !exists || r.Old.Version == ^uint64(0) || r.New.Version != r.Old.Version+1 {
			return fail("unavailable", "invalid reference transition")
		}
	default:
		return fail("unavailable", "unknown journal operation")
	}
	if _, exists := s.retries[r.Actor+"\x00"+r.RequestID]; exists {
		return fail("unavailable", "duplicate journal request identity")
	}
	c, err := s.commit(r.New.Commit)
	if err != nil {
		return err
	}
	switch r.Op {
	case "init":
		if len(c.Parents) != 0 {
			return fail("unavailable", "genesis has parents")
		}
	case "fork": // A fork shares an existing, already-reachable commit.
		found := false
		for _, h := range s.heads {
			if h.Commit == r.New.Commit {
				found = true
				break
			}
		}
		if !found {
			return fail("unavailable", "fork references unknown head")
		}
	default:
		parents := 1
		if r.Op == "merge" {
			parents = 2
		}
		if len(c.Parents) != parents || c.Parents[0] != r.Old.Commit || c.Actor != r.Actor || c.RequestID != r.RequestID {
			return fail("unavailable", "commit does not match journal")
		}
	}
	return nil
}
func (s *Store) applyRecord(r record, payload []byte) {
	s.heads[r.Branch] = r.New
	s.retries[r.Actor+"\x00"+r.RequestID] = retry{r.Fingerprint, r.New}
	s.seq = r.Seq
	s.prevHash = payloadHash(payload)
}
func (s *Store) appendRecord(op, branch, actor, id, fp string, old, new Head) error {
	r := record{1, s.seq + 1, op, branch, old, new, actor, id, fp, s.prevHash, time.Now().UTC().Format(time.RFC3339Nano)}
	if err := s.validateRecord(r); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	frame := encodeFrame(b)
	if len(b) > MaxFrameBytes || s.journalBytes+int64(len(frame)) > MaxJournalBytes || s.diskBytes+int64(len(frame)) > MaxStoreBytes {
		return fail("limit", "journal or physical storage limit")
	}
	uncertain := func(err error) error { s.poisoned = true; return err }
	if err = s.step("journal-write"); err != nil {
		return uncertain(err)
	}
	n, err := s.journal.Write(frame)
	if err != nil {
		return uncertain(err)
	}
	if n != len(frame) {
		return uncertain(io.ErrShortWrite)
	}
	if err = s.step("after-journal-write"); err != nil {
		return uncertain(err)
	}
	if err = s.step("journal-sync"); err != nil {
		return uncertain(err)
	}
	if err = s.journal.Sync(); err != nil {
		return uncertain(err)
	}
	if err = s.step("after-journal-sync"); err != nil {
		return uncertain(err)
	}
	s.applyRecord(r, b)
	s.journalBytes += int64(len(frame))
	s.diskBytes += int64(len(frame))
	return nil
}
func (s *Store) recover() error {
	st, err := s.journal.Stat()
	if err != nil {
		return err
	}
	if st.Size() > MaxJournalBytes {
		return fail("limit", "journal size exceeds limit")
	}
	if _, err = s.journal.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var offset int64
	trim := func() error {
		if err := s.journal.Truncate(offset); err != nil {
			return err
		}
		if err := s.journal.Sync(); err != nil {
			return err
		}
		s.journalBytes = offset
		_, err := s.journal.Seek(0, io.SeekEnd)
		return err
	}
	for {
		payload, err := readFrame(s.journal)
		if err == io.EOF {
			break
		}
		if err == io.ErrUnexpectedEOF {
			return trim()
		}
		if err != nil {
			return err
		}
		var r record
		if err = strictjson.Decode(payload, &r); err != nil {
			return fail("unavailable", "invalid journal JSON")
		}
		if err = s.validateRecord(r); err != nil {
			return err
		}
		s.applyRecord(r, payload)
		offset += int64(12 + len(payload))
	}
	s.journalBytes = offset
	_, err = s.journal.Seek(0, io.SeekEnd)
	return err
}
