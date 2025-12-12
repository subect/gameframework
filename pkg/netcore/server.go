package netcore

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"gameframework/pkg/proto"
	"gameframework/pkg/reliable"
	"net"
	"sync"
	"time"
)

// GameLogic 定义可插拔的游戏逻辑接口。
// 由业务方实现，以便在网络层不变的情况下扩展帧格式/游戏玩法。
type GameLogic interface {
	// OnJoin 在玩家加入时回调，可做初始化。
	OnJoin(pid uint16)
	// OnLeave 在玩家离开时回调（当前未调用，可预留）。
	OnLeave(pid uint16)
	// ApplyInput 在收到输入时调用，业务方可即时更新状态/缓存输入。
	ApplyInput(pid uint16, input uint32)
	// Tick 在每个广播 tick 调用，提供本 tick 聚合的输入。
	Tick(tick uint32, inputs map[uint16]uint32)
	// Snapshot 返回当前状态的二进制快照，服务器会附加到帧末尾。
	// 注意返回的数据会以 uint16 长度前缀写入，长度超出 65535 将被丢弃并跳过本帧附加。
	Snapshot(tick uint32) ([]byte, error)
}

// ClientPeer 保留 peer 状态
type ClientPeer struct {
	id         int
	addr       *net.UDPAddr
	rxReliable *reliable.ReliableReceiver
	txReliable *reliable.ReliableSender
	joined     bool
	lastInput  uint32
}

// Server 主体（可插拔游戏逻辑）
type Server struct {
	conn *net.UDPConn
	room struct {
		players map[int]*ClientPeer
		mu      sync.Mutex
	}
	inputs   map[uint32]map[uint16]uint32
	inputsMu sync.Mutex

	tick   uint32
	tickHz int

	logic GameLogic
}

// NewServer 创建并绑定 UDP，注入游戏逻辑。
func NewServer(listen string, tickHz int, logic GameLogic) (*Server, error) {
	if logic == nil {
		return nil, fmt.Errorf("logic is nil")
	}
	udpAddr, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		conn:   conn,
		inputs: make(map[uint32]map[uint16]uint32),
		tickHz: tickHz,
		logic:  logic,
	}
	s.room.players = make(map[int]*ClientPeer)
	return s, nil
}

// registerPeer 注册新 peer
func (s *Server) registerPeer(id int, addr *net.UDPAddr) *ClientPeer {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	if p, ok := s.room.players[id]; ok {
		p.addr = addr
		return p
	}
	p := &ClientPeer{
		id:         id,
		addr:       addr,
		rxReliable: reliable.NewReliableReceiver(),
		txReliable: reliable.NewReliableSender(),
		joined:     false,
		lastInput:  0,
	}
	s.room.players[id] = p
	fmt.Printf("Server: registered peer id=%d addr=%s\n", id, addr.String())
	return p
}

func (s *Server) findPeerByAddr(addr *net.UDPAddr) *ClientPeer {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	for _, p := range s.room.players {
		if p.addr != nil && p.addr.IP.Equal(addr.IP) && p.addr.Port == addr.Port {
			return p
		}
	}
	return nil
}

func (s *Server) storeInput(tick uint32, pid uint16, input uint32) {
	// 丢弃极端过旧输入
	if s.tick > 1000 && tick+1000 < s.tick {
		fmt.Printf("Server: dropping very old input tick=%d (serverTick=%d) from pid=%d\n", tick, s.tick, pid)
		return
	}
	s.inputsMu.Lock()
	if _, ok := s.inputs[tick]; !ok {
		s.inputs[tick] = make(map[uint16]uint32)
	}
	s.inputs[tick][pid] = input
	s.inputsMu.Unlock()

	s.room.mu.Lock()
	if peer, ok := s.room.players[int(pid)]; ok {
		peer.lastInput = input
	}
	s.room.mu.Unlock()
}

func (s *Server) findMaxReceivedTick() uint32 {
	s.inputsMu.Lock()
	defer s.inputsMu.Unlock()
	var max uint32 = 0
	for t := range s.inputs {
		if t > max {
			max = t
		}
	}
	return max
}

// ListenLoop 启动接收循环（建议以 goroutine 调用）
func (s *Server) ListenLoop() {
	buf := make([]byte, 4096)
	for {
		n, raddr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			fmt.Println("server recv err:", err)
			continue
		}
		_, ack, ackBits, payload, err := proto.ReadUDPHeader(buf[:n])
		if err != nil {
			fmt.Println("server bad header:", err)
			continue
		}
		peer := s.findPeerByAddr(raddr)
		if peer == nil {
			// try reliable join
			if rseq, inner, err := proto.UnpackReliableEnvelope(payload); err == nil {
				if len(inner) >= 2 && inner[0] == proto.MsgJoinRoom {
					playerID := int(inner[1])
					peer = s.registerPeer(playerID, raddr)
					peer.rxReliable.MarkReceived(rseq)
					if !peer.rxReliable.AlreadyProcessed(rseq) {
						peer.rxReliable.MarkProcessed(rseq)
						s.logic.OnJoin(uint16(playerID))
						go s.HandleJoinReliable(peer, rseq, inner)
					}
					continue
				}
			}
			// try input packet
			if p, err := proto.ReadInputPacket(payload); err == nil {
				playerID := int(p.PlayerID)
				peer = s.registerPeer(playerID, raddr)
				s.storeInput(p.Tick, p.PlayerID, p.Input)
				s.logic.ApplyInput(p.PlayerID, p.Input)
				continue
			}
			continue
		}
		// process ack for txReliable
		if peer.txReliable != nil {
			cleared := peer.txReliable.ProcessAckFromRemote(ack, ackBits)
			if len(cleared) > 0 {
				fmt.Printf("Server: cleared pending for peer %d seqs=%v\n", peer.id, cleared)
			}
		}
		// parse input first
		if p, err := proto.ReadInputPacket(payload); err == nil {
			if p.Tick+5 < s.tick {
				fmt.Printf("Server: late input tick=%d (serverTick=%d) from pid=%d\n", p.Tick, s.tick, p.PlayerID)
			}
			if p.Tick > s.tick+500 {
				fmt.Printf("Server: suspicious future input tick=%d (serverTick=%d) from pid=%d\n", p.Tick, s.tick, p.PlayerID)
				continue
			}
			s.storeInput(p.Tick, p.PlayerID, p.Input)
			s.logic.ApplyInput(p.PlayerID, p.Input)
			continue
		}
		// else try reliable
		if rseq, inner, err := proto.UnpackReliableEnvelope(payload); err == nil {
			if len(inner) < 1 {
				continue
			}
			peer.rxReliable.MarkReceived(rseq)
			if !peer.rxReliable.AlreadyProcessed(rseq) {
				peer.rxReliable.MarkProcessed(rseq)
				switch inner[0] {
				case proto.MsgJoinRoom:
					go s.HandleJoinReliable(peer, rseq, inner)
				case proto.MsgPing:
					if len(inner) >= 9 {
						clientTs := int64(binary.LittleEndian.Uint64(inner[1:9]))
						pong := make([]byte, 1+8)
						pong[0] = proto.MsgPong
						binary.LittleEndian.PutUint64(pong[1:], uint64(clientTs))
						seq := peer.txReliable.AddPending(pong)
						ack2, ackbits2 := peer.rxReliable.BuildAckAndBits()
						packetSeq2 := peer.txReliable.NextPacketSeq()
						buf2 := &bytes.Buffer{}
						proto.WriteUDPHeader(buf2, packetSeq2, ack2, ackbits2)
						proto.PackReliableEnvelope(buf2, seq, pong)
						_, _ = s.conn.WriteToUDP(buf2.Bytes(), peer.addr)
						peer.txReliable.UpdatePendingSent(seq)
					}
				default:
					// 其他可靠消息按需扩展
				}
			}
			continue
		}
	}
}

// BroadcastLoop 按 tick 广播 Frame，并用 lastInput 补全缺失
func (s *Server) BroadcastLoop() {
	ticker := time.NewTicker(time.Duration(1000/s.tickHz) * time.Millisecond)
	for range ticker.C {
		maxRecv := s.findMaxReceivedTick()
		if s.tick >= maxRecv+uint32(s.tickHz) {
			time.Sleep(1 * time.Millisecond)
			continue
		}
		s.tick++
		// ensure map
		s.inputsMu.Lock()
		if _, ok := s.inputs[s.tick]; !ok {
			s.inputs[s.tick] = make(map[uint16]uint32)
		}
		s.room.mu.Lock()
		for pid, peer := range s.room.players {
			pid16 := uint16(pid)
			if _, ok := s.inputs[s.tick][pid16]; !ok {
				s.inputs[s.tick][pid16] = peer.lastInput
			}
		}
		s.room.mu.Unlock()
		inputs := s.inputs[s.tick]
		s.inputsMu.Unlock()

		// 业务逻辑 tick
		s.logic.Tick(s.tick, inputs)

		// 构建帧数据：输入帧 + 自定义快照（长度前缀 uint16）
		payloadBuf := &bytes.Buffer{}
		proto.WriteFramePacket(payloadBuf, s.tick, inputs)

		if snap, err := s.logic.Snapshot(s.tick); err == nil && len(snap) > 0 {
			if len(snap) > 65535 {
				fmt.Printf("Server: snapshot too large (%d), skip attach\n", len(snap))
			} else {
				binary.Write(payloadBuf, binary.LittleEndian, uint16(len(snap)))
				payloadBuf.Write(snap)
			}
		} else if err != nil {
			fmt.Printf("Server: snapshot error tick=%d err=%v\n", s.tick, err)
		}

		payload := payloadBuf.Bytes()

		s.room.mu.Lock()
		for _, p := range s.room.players {
			packetSeq := p.txReliable.NextPacketSeq()
			ack, ackbits := p.rxReliable.BuildAckAndBits()
			buf := &bytes.Buffer{}
			proto.WriteUDPHeader(buf, packetSeq, ack, ackbits)
			buf.Write(payload)
			_, _ = s.conn.WriteToUDP(buf.Bytes(), p.addr)
		}
		s.room.mu.Unlock()

		if s.tick > 200 {
			old := s.tick - 200
			s.inputsMu.Lock()
			delete(s.inputs, old)
			s.inputsMu.Unlock()
		}
	}
}

// Reliable retransmit
func (s *Server) ReliableRetransmitLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	for range ticker.C {
		s.room.mu.Lock()
		for _, p := range s.room.players {
			pend := p.txReliable.GetPendingOlderThan(200)
			for _, pm := range pend {
				ack, ackbits := p.rxReliable.BuildAckAndBits()
				packetSeq := p.txReliable.NextPacketSeq()
				buf := &bytes.Buffer{}
				proto.WriteUDPHeader(buf, packetSeq, ack, ackbits)
				proto.PackReliableEnvelope(buf, pm.Seq, pm.Payload)
				_, err := s.conn.WriteToUDP(buf.Bytes(), p.addr)
				if err != nil {
					fmt.Println("Server retransmit err:", err)
				} else {
					p.txReliable.UpdatePendingSent(pm.Seq)
					fmt.Printf("Server retransmit -> peer %d reliableSeq=%d packetSeq=%d\n", p.id, pm.Seq, packetSeq)
				}
			}
		}
		s.room.mu.Unlock()
	}
}

// HandleJoinReliable 处理 join（会发送 JoinAck / PlayerList / Broadcast Joined）
func (s *Server) HandleJoinReliable(peer *ClientPeer, reliableSeq uint32, inner []byte) {
	if len(inner) < 2 {
		return
	}
	peer.joined = true
	// 初始化玩家
	s.logic.OnJoin(uint16(peer.id))

	ackPayload := []byte{proto.MsgJoinAck, byte(peer.id)}
	seq := peer.txReliable.AddPending(ackPayload)
	ack, ackbits := peer.rxReliable.BuildAckAndBits()
	packetSeq := peer.txReliable.NextPacketSeq()
	buf := &bytes.Buffer{}
	proto.WriteUDPHeader(buf, packetSeq, ack, ackbits)
	proto.PackReliableEnvelope(buf, seq, ackPayload)
	_, _ = s.conn.WriteToUDP(buf.Bytes(), peer.addr)
	peer.txReliable.UpdatePendingSent(seq)
	fmt.Printf("Server sent JoinAck reliableSeq=%d packetSeq=%d to %d\n", seq, packetSeq, peer.id)

	// 广播 PlayerJoined
	s.BroadcastPlayerJoined(peer.id)
}

// BroadcastPlayerJoined 广播新玩家加入
func (s *Server) BroadcastPlayerJoined(newPlayerID int) {
	payload := proto.BuildPlayerJoined(newPlayerID)
	s.room.mu.Lock()
	for _, p := range s.room.players {
		seq := p.txReliable.AddPending(payload)
		ack, ackbits := p.rxReliable.BuildAckAndBits()
		packetSeq := p.txReliable.NextPacketSeq()
		buf := &bytes.Buffer{}
		proto.WriteUDPHeader(buf, packetSeq, ack, ackbits)
		proto.PackReliableEnvelope(buf, seq, payload)
		_, _ = s.conn.WriteToUDP(buf.Bytes(), p.addr)
		p.txReliable.UpdatePendingSent(seq)
		fmt.Printf("Server broadcast PlayerJoined -> peer %d reliableSeq=%d packetSeq=%d\n", p.id, seq, packetSeq)
	}
	s.room.mu.Unlock()
}

