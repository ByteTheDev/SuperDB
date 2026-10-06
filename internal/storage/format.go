package storage

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sync"
)

// zlibPool recycles BestSpeed compressors: snapshot-heavy modes (snapshot
// durability, backups, compact) otherwise pay a fresh compressor
// allocation for every frame.
var zlibPool = sync.Pool{New: func() any {
	w, err := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
	if err != nil {
		panic(err)
	}
	return w
}}

// crcTable is the shared CRC-32C (Castagnoli) table used by every v3
// frame checksum. Table lookups are read-only, so concurrent writers
// share one table.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

const (
	magic           = "SUPERDB1"
	version         = byte(1)
	walVersion      = byte(2)
	checksumVersion = byte(3)
	snapshotKind    = byte('S')
	walKind         = byte('W')
	headerSize      = 20
	checksumSize    = 4
	// v3 frames append a CRC-32C of the stored payload to the base
	// header, raising the header from 20 to 24 bytes.
	checksumHeaderSize = headerSize + checksumSize
	maxPayloadSize     = 256 << 20
)

// checksumStored returns the CRC-32C of the stored (possibly compressed)
// frame payload.
func checksumStored(stored []byte) uint32 {
	return crc32.Checksum(stored, crcTable)
}

// validFrameVersion reports whether a frame version is readable for the
// expected record kind. Snapshot frames are zlib-compressed at v1 and
// v3; WAL frames are zlib at v1, raw at v2, and raw plus checksum at v3.
func validFrameVersion(kind, frameVersion byte) bool {
	switch frameVersion {
	case version:
		return true
	case walVersion:
		return kind == walKind
	case checksumVersion:
		return true
	default:
		return false
	}
}

func encode(kind byte, payload []byte) ([]byte, error) {
	compress := kind != walKind
	return encodeFrame(kind, checksumVersion, payload, compress)
}

func encodeFrame(kind, frameVersion byte, payload []byte, compress bool) ([]byte, error) {
	if kind == snapshotKind && frameVersion == walVersion {
		return nil, errors.New("invalid snapshot version")
	}
	if kind == snapshotKind && !compress {
		return nil, errors.New("snapshot frames must be compressed")
	}
	if kind == walKind {
		if frameVersion == version && !compress {
			return nil, errors.New("v1 WAL frames must be compressed")
		}
		if frameVersion >= walVersion && compress {
			return nil, errors.New("v2/v3 WAL frames must be raw")
		}
	}
	if len(payload) > maxPayloadSize {
		return nil, errors.New("payload exceeds SUPERDB size limit")
	}
	encoded := payload
	if compress {
		var compressed bytes.Buffer
		zw := zlibPool.Get().(*zlib.Writer)
		zw.Reset(&compressed)
		if _, err := zw.Write(payload); err != nil {
			zlibPool.Put(zw)
			return nil, err
		}
		if err := zw.Close(); err != nil {
			zlibPool.Put(zw)
			return nil, err
		}
		zlibPool.Put(zw)
		encoded = compressed.Bytes()
	}
	if len(encoded) > maxPayloadSize {
		return nil, errors.New("compressed payload exceeds SUPERDB size limit")
	}
	size := headerSize
	if frameVersion == checksumVersion {
		size = checksumHeaderSize
	}
	out := make([]byte, size+len(encoded))
	copy(out[:8], magic)
	out[8] = kind
	out[9] = frameVersion
	out[10] = 0
	out[11] = 0
	binary.BigEndian.PutUint32(out[12:16], uint32(len(payload)))
	binary.BigEndian.PutUint32(out[16:20], uint32(len(encoded)))
	if frameVersion == checksumVersion {
		binary.BigEndian.PutUint32(out[20:24], checksumStored(encoded))
	}
	copy(out[size:], encoded)
	return out, nil
}

func decodeFrame(b []byte, offset int, expectedKind byte) ([]byte, int, error) {
	if len(b)-offset < headerSize {
		return nil, offset, errors.New("truncated SUPERDB header")
	}
	h := b[offset:]
	if string(h[:8]) != magic {
		return nil, offset, errors.New("invalid SUPERDB magic")
	}
	if h[8] != expectedKind {
		return nil, offset, fmt.Errorf("unexpected SUPERDB record kind %q", h[8])
	}
	if !validFrameVersion(expectedKind, h[9]) {
		return nil, offset, fmt.Errorf("unsupported SUPERDB version %d", h[9])
	}
	size := headerSize
	if h[9] == checksumVersion {
		if len(h) < checksumHeaderSize {
			return nil, offset, errors.New("truncated SUPERDB checksum header")
		}
		size = checksumHeaderSize
	}
	rawLen := binary.BigEndian.Uint32(h[12:16])
	storedLen := binary.BigEndian.Uint32(h[16:20])
	if rawLen > maxPayloadSize || storedLen > maxPayloadSize {
		return nil, offset, errors.New("SUPERDB payload exceeds size limit")
	}
	end := size + int(storedLen)
	if end < size || end > len(h) {
		return nil, offset, errors.New("truncated SUPERDB payload")
	}
	stored := h[size:end]
	if h[9] == checksumVersion && binary.BigEndian.Uint32(h[20:24]) != checksumStored(stored) {
		return nil, offset, errors.New("SUPERDB checksum mismatch")
	}
	if expectedKind == walKind && h[9] >= walVersion {
		if len(stored) != int(rawLen) {
			return nil, offset, fmt.Errorf("payload length mismatch: got %d, want %d", len(stored), rawLen)
		}
		payload := append([]byte(nil), stored...)
		return payload, offset + end, nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(stored))
	if err != nil {
		return nil, offset, fmt.Errorf("open compressed payload: %w", err)
	}
	payload, readErr := io.ReadAll(io.LimitReader(zr, int64(rawLen)+1))
	closeErr := zr.Close()
	if readErr != nil {
		return nil, offset, fmt.Errorf("read compressed payload: %w", readErr)
	}
	if closeErr != nil {
		return nil, offset, fmt.Errorf("close compressed payload: %w", closeErr)
	}
	if len(payload) != int(rawLen) {
		return nil, offset, fmt.Errorf("payload length mismatch: got %d, want %d", len(payload), rawLen)
	}
	return payload, offset + end, nil
}
