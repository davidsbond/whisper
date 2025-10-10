package whisper

import (
	"crypto/ecdh"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
)

type (
	Peer struct {
		ID        uint64
		Address   netip.AddrPort
		PublicKey *ecdh.PublicKey
		Delta     int64
		Status    PeerStatus
		Metadata  []byte
	}

	PeerStatus int
)

const (
	PeerStatusUnknown PeerStatus = iota
	PeerStatusJoining
	PeerStatusJoined
	PeerStatusLeft
)

func (p *Peer) Marshal() ([]byte, error) {
	addr := p.Address.Addr()
	port := p.Address.Port()

	addrLen := ipv4Size
	if addr.Is6() {
		addrLen = ipv6Size
	}

	totalSize := 8 + 8 + publicKeySize + 1 + addrLen + 2 + len(p.Metadata)
	data := make([]byte, 0, totalSize)

	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], p.ID)
	data = append(data, tmp[:]...)

	binary.BigEndian.PutUint64(tmp[:], uint64(p.Delta))
	data = append(data, tmp[:]...)
	data = append(data, p.PublicKey.Bytes()...)

	if addr.Is4() {
		data = append(data, 4)
	} else {
		data = append(data, 6)
	}

	data = append(data, addr.AsSlice()...)
	data = append(data, byte(port>>8), byte(port))

	if len(p.Metadata) > 0 {
		data = append(data, p.Metadata...)
	}

	return data, nil
}

func (p *Peer) Unmarshal(curve ecdh.Curve, data []byte) error {
	var err error

	if len(data) < 8+8+publicKeySize+1+2 {
		return io.ErrUnexpectedEOF
	}

	p.ID = binary.BigEndian.Uint64(data[:8])
	p.Delta = int64(binary.BigEndian.Uint64(data[8:16]))
	p.PublicKey, err = curve.NewPublicKey(data[16 : 16+publicKeySize])
	if err != nil {
		return fmt.Errorf("failed to parse public key: %w", err)
	}

	offset := 16 + publicKeySize
	addrKind := data[offset]
	offset++

	if addrKind != 4 && addrKind != 6 {
		return fmt.Errorf("invalid address kind %d, expected 4 or 6", addrKind)
	}

	size := ipv4Size
	if addrKind == 6 {
		size = ipv6Size
	}

	if len(data[offset:]) < size+2 {
		return io.ErrUnexpectedEOF
	}

	addrBytes := data[offset : offset+size]
	offset += size

	port := binary.BigEndian.Uint16(data[offset : offset+2])
	offset += 2

	ip, ok := netip.AddrFromSlice(addrBytes)
	if !ok {
		return errors.New("failed to parse address")
	}

	p.Address = netip.AddrPortFrom(ip, port)
	if offset < len(data) {
		p.Metadata = data[offset:]
	}

	return nil
}
