package proto

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

const (
	MsgPing = 1
	MsgPong = 2
)

type InputPacket struct {
	Tick     uint32
	PlayerID uint16
	Input    uint32
	TS       int64
}

func WriteInputPacket(buf *bytes.Buffer, p *InputPacket) {
	binary.Write(buf, binary.LittleEndian, p.Tick)
	binary.Write(buf, binary.LittleEndian, p.PlayerID)
	binary.Write(buf, binary.LittleEndian, p.Input)
	binary.Write(buf, binary.LittleEndian, p.TS)
}

func ReadInputPacket(b []byte) (*InputPacket, error) {
	r := bytes.NewReader(b)
	p := &InputPacket{}
	if err := binary.Read(r, binary.LittleEndian, &p.Tick); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &p.PlayerID); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &p.Input); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &p.TS); err != nil {
		return nil, err
	}
	return p, nil
}

func WriteFramePacket(buf *bytes.Buffer, tick uint32, inputs map[uint16]uint32) {
	binary.Write(buf, binary.LittleEndian, tick)
	count := uint8(len(inputs))
	binary.Write(buf, binary.LittleEndian, count)
	for pid, in := range inputs {
		binary.Write(buf, binary.LittleEndian, pid)
		binary.Write(buf, binary.LittleEndian, in)
	}
}

func ReadFramePacket(b []byte) (uint32, map[uint16]uint32, error) {
	r := bytes.NewReader(b)
	var tick uint32
	if err := binary.Read(r, binary.LittleEndian, &tick); err != nil {
		return 0, nil, err
	}
	var count uint8
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return 0, nil, err
	}
	inputs := make(map[uint16]uint32)
	for i := 0; i < int(count); i++ {
		var pid uint16
		var in uint32
		if err := binary.Read(r, binary.LittleEndian, &pid); err != nil {
			return 0, nil, err
		}
		if err := binary.Read(r, binary.LittleEndian, &in); err != nil {
			return 0, nil, err
		}
		inputs[pid] = in
	}
	return tick, inputs, nil
}

func WriteUDPHeader(buf *bytes.Buffer, packetSeq, ack, ackBits uint32) {
	binary.Write(buf, binary.LittleEndian, packetSeq)
	binary.Write(buf, binary.LittleEndian, ack)
	binary.Write(buf, binary.LittleEndian, ackBits)
}

func ReadUDPHeader(b []byte) (uint32, uint32, uint32, []byte, error) {
	if len(b) < 12 {
		return 0, 0, 0, nil, fmt.Errorf("header too small")
	}
	packetSeq := binary.LittleEndian.Uint32(b[0:4])
	ack := binary.LittleEndian.Uint32(b[4:8])
	ackBits := binary.LittleEndian.Uint32(b[8:12])
	return packetSeq, ack, ackBits, b[12:], nil
}

func PackReliableEnvelope(buf *bytes.Buffer, seq uint32, payload []byte) {
	binary.Write(buf, binary.LittleEndian, seq)
	buf.Write(payload)
}

func UnpackReliableEnvelope(b []byte) (uint32, []byte, error) {
	if len(b) < 4 {
		return 0, nil, fmt.Errorf("reliable envelope too small")
	}
	seq := binary.LittleEndian.Uint32(b[0:4])
	return seq, b[4:], nil
}
