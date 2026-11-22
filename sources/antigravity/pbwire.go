package antigravity

import (
	"encoding/binary"
	"time"
)

// agy stores step and conversation metadata as protobuf blobs with no
// published schema. These helpers read individual fields by number from the
// wire format; anything malformed or unexpected reads as absent.

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

// pbFind returns the wire type and payload of the first occurrence of field
// num in msg. For varints the payload is the encoded varint.
func pbFind(msg []byte, num uint64) (wire uint64, payload []byte, ok bool) {
	for len(msg) > 0 {
		tag, n := binary.Uvarint(msg)
		if n <= 0 || tag>>3 == 0 {
			return 0, nil, false
		}
		msg = msg[n:]
		var size int
		switch tag & 7 {
		case wireVarint:
			if _, n := binary.Uvarint(msg); n > 0 {
				size = n
			} else {
				return 0, nil, false
			}
		case wireFixed64:
			size = 8
		case wireFixed32:
			size = 4
		case wireBytes:
			l, n := binary.Uvarint(msg)
			if n <= 0 {
				return 0, nil, false
			}
			msg = msg[n:]
			if l > uint64(len(msg)) {
				return 0, nil, false
			}
			size = int(l) //nolint:gosec // l <= len(msg), checked above
		default:
			return 0, nil, false
		}
		if size > len(msg) {
			return 0, nil, false
		}
		if tag>>3 == num {
			return tag & 7, msg[:size], true
		}
		msg = msg[size:]
	}
	return 0, nil, false
}

// pbDescend follows path through nested messages. An empty path returns msg.
func pbDescend(msg []byte, path []uint64) ([]byte, bool) {
	for i := 0; i < len(path); i++ {
		w, payload, ok := pbFind(msg, path[i])
		if !ok || w != wireBytes {
			return nil, false
		}
		msg = payload
	}
	return msg, true
}

// pbBytes returns the length-delimited payload at path.
func pbBytes(msg []byte, path ...uint64) ([]byte, bool) {
	if len(path) == 0 {
		return nil, false
	}
	return pbDescend(msg, path)
}

// pbVarint returns the varint at path.
func pbVarint(msg []byte, path ...uint64) (uint64, bool) {
	if len(path) == 0 {
		return 0, false
	}
	parent, ok := pbDescend(msg, path[:len(path)-1])
	if !ok {
		return 0, false
	}
	w, payload, ok := pbFind(parent, path[len(path)-1])
	if !ok || w != wireVarint {
		return 0, false
	}
	v, _ := binary.Uvarint(payload)
	return v, true
}

// pbTimestamp reads a google.protobuf.Timestamp-shaped message (1: seconds,
// 2: nanos) at path.
func pbTimestamp(msg []byte, path ...uint64) (time.Time, bool) {
	ts, ok := pbBytes(msg, path...)
	if !ok {
		return time.Time{}, false
	}
	sec, ok := pbVarint(ts, 1)
	if !ok {
		return time.Time{}, false
	}
	nsec, _ := pbVarint(ts, 2)
	return time.Unix(int64(sec), int64(nsec)), true //nolint:gosec // protobuf int64/int32 fields carried as varints
}
