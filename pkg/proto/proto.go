package proto

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// 公共消息类型（可扩展）
const (
	MsgJoinRoom     = 1
	MsgJoinAck      = 2
	MsgPlayerList   = 3
	MsgPlayerJoined = 4

	MsgPing = 10
	MsgPong = 11
)

// InputPacket 表示客户端上报的输入（序列化格式）
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

// Frame packet: server -> clients，包含 tick 与所有玩家输入
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

// PlayerPos 玩家位置
type PlayerPos struct {
	PID  uint16
	X, Y float32
}

// ReadFramePacketWithPos 读取包含位置的帧数据
func ReadFramePacketWithPos(b []byte) (uint32, map[uint16]uint32, []PlayerPos, error) {
	tick, inputs, err := ReadFramePacket(b)
	if err != nil {
		return tick, inputs, nil, err
	}

	// 尝试读取位置数据
	r := bytes.NewReader(b)
	// 跳过已读的数据
	var tempTick uint32
	var tempCount uint8
	binary.Read(r, binary.LittleEndian, &tempTick)
	binary.Read(r, binary.LittleEndian, &tempCount)
	for i := 0; i < int(tempCount); i++ {
		var pid uint16
		var in uint32
		binary.Read(r, binary.LittleEndian, &pid)
		binary.Read(r, binary.LittleEndian, &in)
	}

	// 读取位置
	var posCount uint8
	if err := binary.Read(r, binary.LittleEndian, &posCount); err != nil {
		// 没有位置数据，返回空
		return tick, inputs, nil, nil
	}

	positions := make([]PlayerPos, 0, posCount)
	for i := 0; i < int(posCount); i++ {
		var pos PlayerPos
		if err := binary.Read(r, binary.LittleEndian, &pos.PID); err != nil {
			return tick, inputs, positions, err
		}
		if err := binary.Read(r, binary.LittleEndian, &pos.X); err != nil {
			return tick, inputs, positions, err
		}
		if err := binary.Read(r, binary.LittleEndian, &pos.Y); err != nil {
			return tick, inputs, positions, err
		}
		positions = append(positions, pos)
	}

	return tick, inputs, positions, nil
}

// UDP header: packetSeq:uint32 | ack:uint32 | ackBits:uint32 | payload...
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

// reliable envelope helpers (reliableSeq:uint32 | inner...)
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

// helper message builders/parsers (Join/PlayerList)
func BuildPlayerList(ids []int) []byte {
	buf := []byte{MsgPlayerList, byte(len(ids))}
	for _, id := range ids {
		buf = append(buf, byte(id))
	}
	return buf
}
func ParsePlayerList(b []byte) []int {
	if len(b) < 2 {
		return []int{}
	}
	n := int(b[1])
	ids := make([]int, 0, n)
	for i := 0; i < n && 2+i < len(b); i++ {
		ids = append(ids, int(b[2+i]))
	}
	return ids
}
func BuildPlayerJoined(id int) []byte { return []byte{MsgPlayerJoined, byte(id)} }
func ParsePlayerJoined(b []byte) int {
	if len(b) < 2 {
		return -1
	}
	return int(b[1])
}
