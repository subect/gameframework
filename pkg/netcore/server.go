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
	// HandleReliableMessage 处理可靠消息。返回 true 表示消息已处理，false 表示未处理。
	// peerID: 发送消息的玩家ID（如果为0表示未知peer），addr: 发送消息的地址，msgType: 消息类型（第一个字节），payload: 消息内容（包含 msgType）
	// 如果返回的 playerID > 0，表示需要注册该 playerID 的 peer
	HandleReliableMessage(peerID uint16, addr *net.UDPAddr, msgType byte, payload []byte) (handled bool, playerID int)
}

// ClientPeer 保留 peer 状态
type ClientPeer struct {
	ID          int       // 玩家ID（供业务层访问）
	Addr        *net.UDPAddr
	rxReliable  *reliable.ReliableReceiver
	txReliable  *reliable.ReliableSender
	Joined      bool      // 是否已加入（供业务层访问）
	lastActive  time.Time // 最后活跃时间，用于超时检测
	inputCount  int       // 输入计数（用于限流）
	inputWindow time.Time // 输入窗口开始时间
}

// Server 主体（可插拔游戏逻辑）
type Server struct {
	conn *net.UDPConn
	room struct {
		players       map[int]*ClientPeer    // 按 ID 查找
		playersByAddr map[string]*ClientPeer // 按地址查找（O(1)）
		mu            sync.Mutex
	}
	inputs   map[uint32]map[uint16]uint32
	inputsMu sync.Mutex

	tick            uint32
	tickHz          int
	maxReceivedTick uint32 // 维护最大接收 tick，避免每次遍历
	maxTickMu       sync.Mutex

	logic GameLogic

	// 配置
	playerTimeout  time.Duration // 玩家超时时间
	maxInputPerSec int           // 每玩家每秒最大输入数
	bufferPool     sync.Pool     // bytes.Buffer 复用池
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
		conn:           conn,
		inputs:         make(map[uint32]map[uint16]uint32),
		tickHz:         tickHz,
		logic:          logic,
		playerTimeout:  30 * time.Second, // 默认 30 秒超时
		maxInputPerSec: 100,              // 默认每秒最多 100 个输入
	}
	s.room.players = make(map[int]*ClientPeer)
	s.room.playersByAddr = make(map[string]*ClientPeer)

	// 初始化 buffer pool
	s.bufferPool = sync.Pool{
		New: func() interface{} {
			return &bytes.Buffer{}
		},
	}

	return s, nil
}

// registerPeer 注册新 peer
func (s *Server) registerPeer(id int, addr *net.UDPAddr) *ClientPeer {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	addrKey := addr.String()

	if p, ok := s.room.players[id]; ok {
		// 更新地址映射
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

	// 更新最大接收 tick
	if tick > s.maxReceivedTick {
		s.maxReceivedTick = tick
	}
	s.inputsMu.Unlock()

	s.room.mu.Lock()
	if peer, ok := s.room.players[int(pid)]; ok {
		peer.lastActive = time.Now()

		// 限流检测
		now := time.Now()
		if now.Sub(peer.inputWindow) >= time.Second {
			peer.inputCount = 0
			peer.inputWindow = now
		}
		peer.inputCount++
		if peer.inputCount > s.maxInputPerSec {
			// 超过限制，丢弃输入
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
			// 未知 peer，尝试从可靠消息中获取玩家ID（由业务层处理）
			if rseq, inner, err := proto.UnpackReliableEnvelope(payload); err == nil && len(inner) > 0 {
				// 先尝试让业务层处理（可能会返回需要注册的 playerID）
				handled, playerID := s.logic.HandleReliableMessage(0, raddr, inner[0], inner)
				if handled {
					// 如果返回了 playerID，注册 peer 并再次调用 HandleReliableMessage 处理加入逻辑
					if playerID > 0 {
						peer = s.registerPeer(playerID, raddr)
						peer.rxReliable.MarkReceived(rseq)
						if !peer.rxReliable.AlreadyProcessed(rseq) {
							peer.rxReliable.MarkProcessed(rseq)
							// 注册 peer 后，再次调用 HandleReliableMessage 处理加入逻辑
							s.logic.HandleReliableMessage(uint16(playerID), raddr, inner[0], inner)
						}
					} else {
						// 业务层已处理但不需要注册，重新查找 peer（可能已注册）
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
			// try input packet
			if p, err := proto.ReadInputPacket(payload); err == nil {
				playerID := int(p.PlayerID)
				peer = s.registerPeer(playerID, raddr)
				peer.lastActive = time.Now() // 更新活跃时间
				s.storeInput(p.Tick, p.PlayerID, p.Input)
				continue
			}
			continue
		}
		// 更新活跃时间
		peer.lastActive = time.Now()

		// process ack for txReliable
		if peer.txReliable != nil {
			cleared := peer.txReliable.ProcessAckFromRemote(ack, ackBits)
			if len(cleared) > 0 {
				fmt.Printf("Server: cleared pending for peer %d seqs=%v\n", peer.ID, cleared)
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
			// 存储输入到指定的 tick，在 BroadcastLoop 的 Tick 中统一处理
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
				msgType := inner[0]
				
				// Ping/Pong 是网络层基础功能，框架内部处理
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
					// 其他可靠消息交给业务层处理
					s.logic.HandleReliableMessage(uint16(peer.ID), peer.Addr, msgType, inner)
				}
			}
			continue
		}
	}
}

// BroadcastLoop 按 tick 广播 Frame，缺失输入使用 InputNone (0) 补全
func (s *Server) BroadcastLoop() {
	ticker := time.NewTicker(time.Duration(1000/s.tickHz) * time.Millisecond)
	for range ticker.C {
		// 检查是否有玩家
		s.room.mu.Lock()
		hasPlayers := len(s.room.players) > 0
		s.room.mu.Unlock()

		// 如果没有玩家，跳过
		if !hasPlayers {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		maxRecv := s.findMaxReceivedTick()
		// 如果有玩家但没有输入，也继续发送帧（使用 InputNone）
		if maxRecv == 0 && s.tick == 0 {
			// 第一个 tick，直接开始
		} else if s.tick > 0 && s.tick >= maxRecv+uint32(s.tickHz) {
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
				// 如果玩家没有输入，使用 InputNone (0) 而不是 lastInput
				// 这样玩家会保持不动，而不是继续按上次的方向移动
				s.inputs[s.tick][pid16] = 0 // InputNone
			}
		}
		s.room.mu.Unlock()
		inputs := s.inputs[s.tick]
		s.inputsMu.Unlock()

		// 业务逻辑 tick
		s.logic.Tick(s.tick, inputs)

		// 构建帧数据：输入帧 + 自定义快照（长度前缀 uint16）
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
			// 发送帧数据时更新活跃时间（避免因没有输入而被超时移除）
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

// CheckPlayerTimeout 检测玩家超时并清理（建议以 goroutine 调用）
func (s *Server) CheckPlayerTimeout() {
	ticker := time.NewTicker(5 * time.Second) // 每 5 秒检查一次
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

		// 清理超时玩家
		for _, id := range toRemove {
			s.removePlayer(id)
		}
	}
}

// removePlayer 移除玩家
func (s *Server) removePlayer(id int) {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()

	p, ok := s.room.players[id]
	if !ok {
		return
	}

	// 从地址映射中删除
	if p.Addr != nil {
		delete(s.room.playersByAddr, p.Addr.String())
	}

	// 从玩家列表中删除
	delete(s.room.players, id)

	// 调用游戏逻辑的 OnLeave
	s.logic.OnLeave(uint16(id))

	fmt.Printf("Server: removed timeout player id=%d\n", id)
}

// SendReliableToPeer 向指定 peer 发送可靠消息（供业务层使用）
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

// SendReliableToAll 向所有 peer 广播可靠消息（供业务层使用）
func (s *Server) SendReliableToAll(payload []byte) {
	s.room.mu.Lock()
	for _, p := range s.room.players {
		s.SendReliableToPeer(p, payload)
	}
	s.room.mu.Unlock()
}

// GetPeer 获取指定 ID 的 peer（供业务层使用）
func (s *Server) GetPeer(pid uint16) *ClientPeer {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	return s.room.players[int(pid)]
}

// GetAllPeers 获取所有 peer（供业务层使用）
func (s *Server) GetAllPeers() []*ClientPeer {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	peers := make([]*ClientPeer, 0, len(s.room.players))
	for _, p := range s.room.players {
		peers = append(peers, p)
	}
	return peers
}

// RegisterPeer 注册新 peer（供业务层使用，例如从可靠消息中获取玩家ID后注册）
func (s *Server) RegisterPeer(id int, addr *net.UDPAddr) *ClientPeer {
	return s.registerPeer(id, addr)
}
