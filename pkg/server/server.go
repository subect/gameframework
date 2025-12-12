package server

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"gameframework/pkg/game"
	"gameframework/pkg/proto"
	"gameframework/pkg/reliable"
	"net"
	"sync"
	"time"
)

// ClientPeer 保留 peer 状态
type ClientPeer struct {
	id         int
	addr       *net.UDPAddr
	rxReliable *reliable.ReliableReceiver
	txReliable *reliable.ReliableSender
	joined     bool
	lastInput  uint32
}

// Server 主体
type Server struct {
	conn *net.UDPConn
	room struct {
		players map[int]*ClientPeer
		mu      sync.Mutex
	}
	inputs   map[uint32]map[uint16]uint32
	inputsMu sync.Mutex

	tick      uint32
	tickHz    int
	gameState *game.GameState
}

// NewServer 创建并绑定 UDP
func NewServer(listen string, tickHz int) (*Server, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		conn:      conn,
		inputs:    make(map[uint32]map[uint16]uint32),
		tickHz:    tickHz,
		gameState: game.NewGameState(),
	}
	s.room.players = make(map[int]*ClientPeer)
	return s, nil
}

/* 下面的方法基本上对应你文件里的实现，略作包化整理：
   - registerPeer
   - findPeerByAddr
   - storeInput
   - listenLoop
   - broadcastLoop
   - reliableRetransmitLoop
   - HandleJoinReliable / SendPlayerListTo / BroadcastPlayerJoined
*/

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
					fmt.Printf("Server: reliable JoinRoom from %d seq=%d\n", peer.id, rseq)
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
					fmt.Printf("Server: unknown reliable msg type=%d from %d\n", inner[0], peer.id)
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
		for pid := range s.room.players {
			pid16 := uint16(pid)
			if _, ok := s.inputs[s.tick][pid16]; !ok {
				// 如果没有收到输入，使用InputNone而不是lastInput
				// 这样可以避免持续移动
				s.inputs[s.tick][pid16] = 0 // InputNone
			}
		}
		s.room.mu.Unlock()
		inputs := s.inputs[s.tick]
		s.inputsMu.Unlock()

		// 应用输入到游戏状态
		for pid, input := range inputs {
			s.gameState.ApplyInput(pid, input)
		}

		// 构建帧数据，包含输入和位置
		payloadBuf := &bytes.Buffer{}
		proto.WriteFramePacket(payloadBuf, s.tick, inputs)

		// 添加所有玩家位置
		allPos := s.gameState.GetAllPositions()
		binary.Write(payloadBuf, binary.LittleEndian, uint8(len(allPos)))
		for pid, pos := range allPos {
			binary.Write(payloadBuf, binary.LittleEndian, pid)
			binary.Write(payloadBuf, binary.LittleEndian, pos.X)
			binary.Write(payloadBuf, binary.LittleEndian, pos.Y)
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
	// 初始化玩家位置
	s.gameState.ApplyInput(uint16(peer.id), game.InputNone)
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

	// send list & broadcast
	s.SendPlayerListTo(peer)
	s.BroadcastPlayerJoined(peer.id)
}

func (s *Server) SendPlayerListTo(peer *ClientPeer) {
	ids := []int{}
	s.room.mu.Lock()
	for id := range s.room.players {
		ids = append(ids, id)
	}
	s.room.mu.Unlock()
	pl := proto.BuildPlayerList(ids)
	seq := peer.txReliable.AddPending(pl)
	ack, ackbits := peer.rxReliable.BuildAckAndBits()
	packetSeq := peer.txReliable.NextPacketSeq()
	buf := &bytes.Buffer{}
	proto.WriteUDPHeader(buf, packetSeq, ack, ackbits)
	proto.PackReliableEnvelope(buf, seq, pl)
	_, _ = s.conn.WriteToUDP(buf.Bytes(), peer.addr)
	peer.txReliable.UpdatePendingSent(seq)
	fmt.Printf("Server sent PlayerList reliableSeq=%d packetSeq=%d to %d\n", seq, packetSeq, peer.id)
}

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
