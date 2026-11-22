package antigravity

import (
	"encoding/binary"
	"testing"
)

// pbField is one encoded field for the test-side protobuf builder.
type pbField []byte

func pbTag(num, wire int) []byte {
	return binary.AppendUvarint(nil, uint64(num)<<3|uint64(wire))
}

func pbV(num int, v uint64) pbField {
	return binary.AppendUvarint(pbTag(num, 0), v)
}

func pbB(num int, b []byte) pbField {
	out := binary.AppendUvarint(pbTag(num, 2), uint64(len(b)))
	return append(out, b...)
}

func pbS(num int, s string) pbField { return pbB(num, []byte(s)) }

func pbM(num int, fields ...pbField) pbField { return pbB(num, pbMsg(fields...)) }

func pbFixed64(num int, v uint64) pbField {
	return binary.LittleEndian.AppendUint64(pbTag(num, 1), v)
}

func pbFixed32(num int, v uint32) pbField {
	return binary.LittleEndian.AppendUint32(pbTag(num, 5), v)
}

func pbMsg(fields ...pbField) []byte {
	var out []byte
	for i := 0; i < len(fields); i++ {
		out = append(out, fields[i]...)
	}
	return out
}

func TestPBBytes_NestedPathSkipsOtherWireTypes(t *testing.T) {
	msg := pbMsg(
		pbFixed64(1, 7),
		pbFixed32(2, 7),
		pbV(3, 9),
		pbM(10, pbM(1, pbV(1, 1535), pbS(28, "gemini-x"), pbV(6, 262144))),
	)
	got, ok := pbBytes(msg, 10, 1, 28)
	if !ok || string(got) != "gemini-x" {
		t.Errorf("pbBytes(10,1,28) = %q, %v; want gemini-x, true", got, ok)
	}
	if v, ok := pbVarint(msg, 10, 1, 6); !ok || v != 262144 {
		t.Errorf("pbVarint(10,1,6) = %d, %v; want 262144, true", v, ok)
	}
	if v, ok := pbVarint(msg, 3); !ok || v != 9 {
		t.Errorf("pbVarint(3) = %d, %v; want 9, true", v, ok)
	}
}

func TestPBBytes_FirstOccurrenceWins(t *testing.T) {
	msg := pbMsg(pbS(1, "a"), pbS(1, "b"))
	if got, ok := pbBytes(msg, 1); !ok || string(got) != "a" {
		t.Errorf("pbBytes = %q, %v; want a, true", got, ok)
	}
}

func TestPB_MissingAndMalformed(t *testing.T) {
	cases := map[string][]byte{
		"empty":          nil,
		"truncated len":  {0x0a, 0x05, 'a'},
		"truncated var":  {0x08, 0x80},
		"group wiretype": {0x0b},
		"field zero":     {0x00, 0x01},
	}
	for name, msg := range cases {
		if _, ok := pbBytes(msg, 1); ok {
			t.Errorf("%s: pbBytes ok = true, want false", name)
		}
		if _, ok := pbVarint(msg, 1); ok {
			t.Errorf("%s: pbVarint ok = true, want false", name)
		}
	}
	// A varint where a message is expected is not traversable.
	if _, ok := pbBytes(pbMsg(pbV(1, 5)), 1, 2); ok {
		t.Error("descending into a varint: ok = true, want false")
	}
	if _, ok := pbVarint(pbMsg(pbS(1, "x")), 1); ok {
		t.Error("varint lookup of bytes field: ok = true, want false")
	}
}

func TestPBTimestamp(t *testing.T) {
	msg := pbMsg(pbM(1, pbV(1, 1795377617), pbV(2, 500)))
	ts, ok := pbTimestamp(msg, 1)
	if !ok || ts.Unix() != 1795377617 || ts.Nanosecond() != 500 {
		t.Errorf("pbTimestamp = %v, %v", ts, ok)
	}
	if _, ok := pbTimestamp(msg, 2); ok {
		t.Error("missing timestamp: ok = true")
	}
}
