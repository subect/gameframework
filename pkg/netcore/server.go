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

type GameLogic interface {
	OnJoin(pid uint16)
	OnLeave(pid uint16)
	ApplyInput(pid uint16, input uint32)
	Tick(tick uint32, inputs map[uint16]uint32)
	Snapshot(tick uint32) ([]byte, error)
	HandleReliableMessage(peerID uint16, addr *net.UDPAddr, msgType byte, payload []byte) (handled bool, playerID int)
}

type ClientPeer struct {
	ID          int
	Addr        *net.UDPAddr
	rxReliable  *reliable.ReliableReceiver
	txReliable  *reliable.ReliableSender
	Joined      bool
	lastActive  time.Time
	inputCount  int
	inputWindow time.Time
}

type Server struct {
	conn *net.UDPConn
	room struct {
		players       map[int]*ClientPeer
		playersByAddr map[string]*ClientPeer
		mu            sync.Mutex
	}
	inputs   map[uint32]map[uint16]uint32
	inputsMu sync.Mutex

	tick            uint32
	tickHz          int
	maxReceivedTick uint32
	maxTickMu       sync.Mutex

	logic GameLogic

	playerTimeout  time.Duration
	maxInputPerSec int
	bufferPool     sync.Pool
}

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
		conn:           conn,
		inputs:         make(map[uint32]map[uint16]uint32),
		tickHz:         tickHz,
		logic:          logic,
		playerTimeout:  30 * time.Second,
		maxInputPerSec: 100,
	}
	s.room.players = make(map[int]*ClientPeer)
	s.room.playersByAddr = make(map[string]*ClientPeer)

	s.bufferPool = sync.Pool{
		New: func() interface{} {
			return &bytes.Buffer{}
		},
	}

	return s, nil
}

func (s *Server) registerPeer(id int, addr *net.UDPAddr) *ClientPeer {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	addrKey := addr.String()

	if p, ok := s.room.players[id]; ok {
		if p.Addr != nil {
			delete(s.room.playersByAddr, p.Addr.String())
		}
		p.Addr = addr
		s.room.playersByAddr[addrKey] = p
		p.lastActive = time.Now()
		return p
	}
	p := &ClientPeer{
		ID:          id,
		Addr:        addr,
		rxReliable:  reliable.NewReliableReceiver(),
		txReliable:  reliable.NewReliableSender(),
		Joined:      false,
		lastActive:  time.Now(),
		inputWindow: time.Now(),
	}
	s.room.players[id] = p
	s.room.playersByAddr[addrKey] = p
	fmt.Printf("Server: registered peer id=%d addr=%s\n", id, addr.String())
	return p
}

func (s *Server) findPeerByAddr(addr *net.UDPAddr) *ClientPeer {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	return s.room.playersByAddr[addr.String()]
}

func (s *Server) storeInput(tick uint32, pid uint16, input uint32) {
	if s.tick > 1000 && tick+1000 < s.tick {
		fmt.Printf("Server: dropping very old input tick=%d (serverTick=%d) from pid=%d\n", tick, s.tick, pid)
		return
	}
	s.inputsMu.Lock()
	if _, ok := s.inputs[tick]; !ok {
		s.inputs[tick] = make(map[uint16]uint32)
	}
	s.inputs[tick][pid] = input

	if tick > s.maxReceivedTick {
		s.maxReceivedTick = tick
	}
	s.inputsMu.Unlock()

	s.room.mu.Lock()
	if peer, ok := s.room.players[int(pid)]; ok {
		peer.lastActive = time.Now()

		now := time.Now()
		if now.Sub(peer.inputWindow) >= time.Second {
			peer.inputCount = 0
			peer.inputWindow = now
		}
		peer.inputCount++
		if peer.inputCount > s.maxInputPerSec {
			s.room.mu.Unlock()
			return
		}
	}
	s.room.mu.Unlock()
}

func (s *Server) findMaxReceivedTick() uint32 {
	s.maxTickMu.Lock()
	defer s.maxTickMu.Unlock()
	return s.maxReceivedTick
}

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
			if rseq, inner, err := proto.UnpackReliableEnvelope(payload); err == nil && len(inner) > 0 {
				handled, playerID := s.logic.HandleReliableMessage(0, raddr, inner[0], inner)
				if handled {
					if playerID > 0 {
						peer = s.registerPeer(playerID, raddr)
						peer.rxReliable.MarkReceived(rseq)
						if !peer.rxReliable.AlreadyProcessed(rseq) {
							peer.rxReliable.MarkProcessed(rseq)
							s.logic.HandleReliableMessage(uint16(playerID), raddr, inner[0], inner)
						}
					} else {
						peer = s.findPeerByAddr(raddr)
						if peer != nil {
							peer.rxReliable.MarkReceived(rseq)
							if !peer.rxReliable.AlreadyProcessed(rseq) {
								peer.rxReliable.MarkProcessed(rseq)
							}
						}
					}
					continue
				}
			}
			if p, err := proto.ReadInputPacket(payload); err == nil {
				playerID := int(p.PlayerID)
				peer = s.registerPeer(playerID, raddr)
				peer.lastActive = time.Now()
				s.storeInput(p.Tick, p.PlayerID, p.Input)
				continue
			}
			continue
		}
		peer.lastActive = time.Now()

		if peer.txReliable != nil {
			cleared := peer.txReliable.ProcessAckFromRemote(ack, ackBits)
			if len(cleared) > 0 {
				fmt.Printf("Server: cleared pending for peer %d seqs=%v\n", peer.ID, cleared)
			}
		}
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
		if rseq, inner, err := proto.UnpackReliableEnvelope(payload); err == nil {
			if len(inner) < 1 {
				continue
			}
			peer.rxReliable.MarkReceived(rseq)
			if !peer.rxReliable.AlreadyProcessed(rseq) {
				peer.rxReliable.MarkProcessed(rseq)
				msgType := inner[0]

				if msgType == proto.MsgPing {
					if len(inner) >= 9 {
						clientTs := int64(binary.LittleEndian.Uint64(inner[1:9]))
						peer.lastActive = time.Now()

						pong := make([]byte, 1+8)
						pong[0] = proto.MsgPong
						binary.LittleEndian.PutUint64(pong[1:], uint64(clientTs))
						s.SendReliableToPeer(peer, pong)
					}
				} else {
					s.logic.HandleReliableMessage(uint16(peer.ID), peer.Addr, msgType, inner)
				}
			}
			continue
		}
	}
}

func (s *Server) BroadcastLoop() {
	ticker := time.NewTicker(time.Duration(1000/s.tickHz) * time.Millisecond)
	for range ticker.C {
		s.room.mu.Lock()
		hasPlayers := len(s.room.players) > 0
		s.room.mu.Unlock()

		if !hasPlayers {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		maxRecv := s.findMaxReceivedTick()
		if maxRecv == 0 && s.tick == 0 {
		} else if s.tick > 0 && s.tick >= maxRecv+uint32(s.tickHz) {
			time.Sleep(1 * time.Millisecond)
			continue
		}
		s.tick++
		s.inputsMu.Lock()
		if _, ok := s.inputs[s.tick]; !ok {
			s.inputs[s.tick] = make(map[uint16]uint32)
		}
		s.room.mu.Lock()
		for pid := range s.room.players {
			pid16 := uint16(pid)
			if _, ok := s.inputs[s.tick][pid16]; !ok {
				s.inputs[s.tick][pid16] = 0
			}
		}
		s.room.mu.Unlock()
		inputs := s.inputs[s.tick]
		s.inputsMu.Unlock()

		s.logic.Tick(s.tick, inputs)

		payloadBuf := s.bufferPool.Get().(*bytes.Buffer)
		payloadBuf.Reset()
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
		payloadCopy := make([]byte, len(payload))
		copy(payloadCopy, payload)
		s.bufferPool.Put(payloadBuf)

		s.room.mu.Lock()
		for _, p := range s.room.players {
			p.lastActive = time.Now()

			packetSeq := p.txReliable.NextPacketSeq()
			ack, ackbits := p.rxReliable.BuildAckAndBits()
			buf := s.bufferPool.Get().(*bytes.Buffer)
			buf.Reset()
			proto.WriteUDPHeader(buf, packetSeq, ack, ackbits)
			buf.Write(payloadCopy)
			_, _ = s.conn.WriteToUDP(buf.Bytes(), p.Addr)
			s.bufferPool.Put(buf)
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

func (s *Server) ReliableRetransmitLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	for range ticker.C {
		s.room.mu.Lock()
		for _, p := range s.room.players {
			pend := p.txReliable.GetPendingOlderThan(200)
			for _, pm := range pend {
				ack, ackbits := p.rxReliable.BuildAckAndBits()
				packetSeq := p.txReliable.NextPacketSeq()
				buf := s.bufferPool.Get().(*bytes.Buffer)
				buf.Reset()
				proto.WriteUDPHeader(buf, packetSeq, ack, ackbits)
				proto.PackReliableEnvelope(buf, pm.Seq, pm.Payload)
				_, err := s.conn.WriteToUDP(buf.Bytes(), p.Addr)
				if err != nil {
					fmt.Println("Server retransmit err:", err)
				} else {
					p.txReliable.UpdatePendingSent(pm.Seq)
					fmt.Printf("Server retransmit -> peer %d reliableSeq=%d packetSeq=%d\n", p.ID, pm.Seq, packetSeq)
				}
				s.bufferPool.Put(buf)
			}
		}
		s.room.mu.Unlock()
	}
}

func (s *Server) CheckPlayerTimeout() {
	ticker := time.NewTicker(5 * time.Second)
	for range ticker.C {
		now := time.Now()
		var toRemove []int

		s.room.mu.Lock()
		for id, p := range s.room.players {
			if now.Sub(p.lastActive) > s.playerTimeout {
				toRemove = append(toRemove, id)
			}
		}
		s.room.mu.Unlock()

		for _, id := range toRemove {
			s.removePlayer(id)
		}
	}
}

func (s *Server) removePlayer(id int) {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()

	p, ok := s.room.players[id]
	if !ok {
		return
	}

	if p.Addr != nil {
		delete(s.room.playersByAddr, p.Addr.String())
	}

	delete(s.room.players, id)

	s.logic.OnLeave(uint16(id))

	fmt.Printf("Server: removed timeout player id=%d\n", id)
}

func (s *Server) SendReliableToPeer(peer *ClientPeer, payload []byte) {
	seq := peer.txReliable.AddPending(payload)
	ack, ackbits := peer.rxReliable.BuildAckAndBits()
	packetSeq := peer.txReliable.NextPacketSeq()
	buf := s.bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	proto.WriteUDPHeader(buf, packetSeq, ack, ackbits)
	proto.PackReliableEnvelope(buf, seq, payload)
	_, _ = s.conn.WriteToUDP(buf.Bytes(), peer.Addr)
	peer.txReliable.UpdatePendingSent(seq)
	s.bufferPool.Put(buf)
}

func (s *Server) SendReliableToAll(payload []byte) {
	s.room.mu.Lock()
	for _, p := range s.room.players {
		s.SendReliableToPeer(p, payload)
	}
	s.room.mu.Unlock()
}

func (s *Server) GetPeer(pid uint16) *ClientPeer {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	return s.room.players[int(pid)]
}

func (s *Server) GetAllPeers() []*ClientPeer {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	peers := make([]*ClientPeer, 0, len(s.room.players))
	for _, p := range s.room.players {
		peers = append(peers, p)
	}
	return peers
}

func (s *Server) RegisterPeer(id int, addr *net.UDPAddr) *ClientPeer {
	return s.registerPeer(id, addr)
}
