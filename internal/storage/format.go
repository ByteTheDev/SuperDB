package storage

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	magic          = "SUPERDB1"
	version        = byte(1)
	snapshotKind   = byte('S')
	walKind        = byte('W')
	headerSize     = 20
	maxPayloadSize = 256 << 20
)

func encode(kind byte, payload []byte) ([]byte, error) {
	if len(payload) > maxPayloadSize {
		return nil, errors.New("payload exceeds SUPERDB size limit")
	}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(payload); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if compressed.Len() > maxPayloadSize {
		return nil, errors.New("compressed payload exceeds SUPERDB size limit")
	}
	out := make([]byte, headerSize+compressed.Len())
	copy(out[:8], magic)
	out[8] = kind
	out[9] = version
	binary.BigEndian.PutUint32(out[12:16], uint32(len(payload)))
	binary.BigEndian.PutUint32(out[16:20], uint32(compressed.Len()))
	copy(out[headerSize:], compressed.Bytes())
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
	if h[9] != version {
		return nil, offset, fmt.Errorf("unsupported SUPERDB version %d", h[9])
	}
	rawLen := binary.BigEndian.Uint32(h[12:16])
	compressedLen := binary.BigEndian.Uint32(h[16:20])
	if rawLen > maxPayloadSize || compressedLen > maxPayloadSize {
		return nil, offset, errors.New("SUPERDB payload exceeds size limit")
	}
	end := headerSize + int(compressedLen)
	if end < headerSize || end > len(h) {
		return nil, offset, errors.New("truncated SUPERDB payload")
	}
	zr, err := zlib.NewReader(bytes.NewReader(h[headerSize:end]))
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
