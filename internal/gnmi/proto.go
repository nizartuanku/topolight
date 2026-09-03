package gnmi

// A minimal protobuf wire codec — enough for gNMI Capabilities/Get. The
// messages are hand-built from the gnmi.proto field numbers so the binary
// carries no gRPC or protobuf runtime.

import (
	"encoding/binary"
	"errors"
	"math"
)

type wireType int

const (
	wireVarint  wireType = 0
	wireFixed64 wireType = 1
	wireBytes   wireType = 2
	wireFixed32 wireType = 5
)

type field struct {
	num  int
	wt   wireType
	u    uint64 // varint / fixed
	data []byte // bytes
}

// ---- encoding ----

type enc struct{ b []byte }

func (e *enc) tag(num int, wt wireType) { e.varint(uint64(num)<<3 | uint64(wt)) }

func (e *enc) varint(v uint64) {
	var tmp [10]byte
	n := binary.PutUvarint(tmp[:], v)
	e.b = append(e.b, tmp[:n]...)
}

func (e *enc) uint(num int, v uint64) {
	if v == 0 {
		return
	}
	e.tag(num, wireVarint)
	e.varint(v)
}

func (e *enc) bytes(num int, b []byte) {
	if len(b) == 0 {
		return
	}
	e.tag(num, wireBytes)
	e.varint(uint64(len(b)))
	e.b = append(e.b, b...)
}

func (e *enc) str(num int, s string) { e.bytes(num, []byte(s)) }

func (e *enc) msg(num int, sub *enc) {
	e.tag(num, wireBytes)
	e.varint(uint64(len(sub.b)))
	e.b = append(e.b, sub.b...)
}

// ---- decoding ----

func decode(b []byte) ([]field, error) {
	var out []field
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errors.New("protobuf: bad tag")
		}
		b = b[n:]
		f := field{num: int(key >> 3), wt: wireType(key & 7)}
		switch f.wt {
		case wireVarint:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, errors.New("protobuf: bad varint")
			}
			f.u = v
			b = b[n:]
		case wireFixed64:
			if len(b) < 8 {
				return nil, errors.New("protobuf: short fixed64")
			}
			f.u = binary.LittleEndian.Uint64(b)
			b = b[8:]
		case wireFixed32:
			if len(b) < 4 {
				return nil, errors.New("protobuf: short fixed32")
			}
			f.u = uint64(binary.LittleEndian.Uint32(b))
			b = b[4:]
		case wireBytes:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return nil, errors.New("protobuf: bad length")
			}
			f.data = b[n : n+int(l)]
			b = b[n+int(l):]
		default:
			return nil, errors.New("protobuf: unsupported wire type")
		}
		out = append(out, f)
	}
	return out, nil
}

func f64(u uint64) float64 { return math.Float64frombits(u) }
func f32(u uint64) float64 { return float64(math.Float32frombits(uint32(u))) }
