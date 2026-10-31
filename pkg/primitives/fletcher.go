package primitives

import "encoding/binary"

// fletcher4 is the running checksum of a replication stream's header
// (fletcher_4_incremental_native / _byteswap): four 64 bit sums over the
// 32 bit words of the data.
type fletcher4 [4]uint64

// write adds b, whose length must be a multiple of 4, to the sums. order is
// the byte order of the stream: the host's for the native function, the other
// one for the byte swapping function.
func (z *fletcher4) write(order binary.ByteOrder, b []byte) {
	a, bb, c, d := z[0], z[1], z[2], z[3]
	for i := 0; i+4 <= len(b); i += 4 {
		a += uint64(order.Uint32(b[i:]))
		bb += a
		c += bb
		d += c
	}
	z[0], z[1], z[2], z[3] = a, bb, c, d
}

// words reads four 64 bit words, as a zio_cksum_t sits in a record.
func readCksum(order binary.ByteOrder, b []byte) fletcher4 {
	var z fletcher4
	for i := range z {
		z[i] = order.Uint64(b[8*i:])
	}
	return z
}

func (z fletcher4) put(order binary.ByteOrder, b []byte) {
	for i := range z {
		order.PutUint64(b[8*i:], z[i])
	}
}
