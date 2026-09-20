package wire

import (
	"bytes"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	payload := []byte(`{"sql":"SELECT 1"}`)
	frame := EncodeFrame(TypeQuery, 42, payload)
	hdr, body, err := ReadFrame(bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Type != TypeQuery || hdr.RequestID != 42 || !bytes.Equal(body, payload) {
		t.Fatalf("mismatch: %+v %q", hdr, body)
	}
	if hdr.Version != Version {
		t.Fatalf("version %d", hdr.Version)
	}
}

func TestBadMagicRejected(t *testing.T) {
	frame := EncodeFrame(TypePing, 1, []byte(`{}`))
	frame[0] = 'X'
	if _, _, err := ReadFrame(bytes.NewReader(frame)); err == nil {
		t.Fatal("expected bad magic error")
	}
}

func TestBadVersionRejected(t *testing.T) {
	frame := EncodeFrame(TypePing, 1, []byte(`{}`))
	frame[4] = 0
	frame[5] = 99
	if _, _, err := ReadFrame(bytes.NewReader(frame)); err == nil {
		t.Fatal("expected version error")
	} else {
		if _, ok := err.(*VersionError); ok {
			// expected typed error
		} else if got := err.Error(); got == "" {
			t.Fatal("empty error")
		}
	}
}

func TestOversizedFrameRejected(t *testing.T) {
	var hdr [HeaderLen]byte
	copy(hdr[0:4], Magic[:])
	hdr[4] = 0
	hdr[5] = 1
	hdr[6] = TypeQuery
	// length = MaxFrameSize+1
	n := MaxFrameSize + 1
	hdr[16] = byte(n >> 24)
	hdr[17] = byte(n >> 16)
	hdr[18] = byte(n >> 8)
	hdr[19] = byte(n)
	if _, _, err := ReadFrame(bytes.NewReader(hdr[:])); err == nil {
		t.Fatal("expected oversized error")
	}
}

func TestWriteFrameRejectsHugePayload(t *testing.T) {
	huge := make([]byte, MaxFrameSize+1)
	if err := WriteFrame(bytes.NewBuffer(nil), TypeQuery, 1, huge); err == nil {
		t.Fatal("expected payload-too-large error")
	}
}
