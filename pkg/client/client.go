package client

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"gameframework/pkg/proto"
	"gameframework/pkg/reliable"
)

// ClientConfig describes how to connect a game client to a gameframework server.
// This is intentionally minimal and focused on network concerns only; game logic
// / rendering should sit on top of this package.
type ClientConfig struct {
	// ServerAddr is the UDP address of the game server, e.g. "127.0.0.1:30000".
	ServerAddr string

	// PlayerID is the logical player identifier assigned by your game.
	PlayerID uint16

	// TickHz is the expected simulation tick rate. This should normally match
	// the server's tick rate.
	TickHz int

	// Optional dial / resend settings. Zero values mean "use defaults".
	DialTimeout   time.Duration
	ResendTimeout time.Duration
}

// FrameUpdate represents a single server frame as observed by the client.
// It is the main data structure that game / rendering layers should consume.
type FrameUpdate struct {
	Tick   uint32
	Inputs map[uint16]uint32
	Snap   []byte // optional snapshot payload
}

// Client is the main entry point for connecting to a gameframework server
// from a game. It hides UDP / reliable channel details behind a small API.
type Client struct {
	cfg ClientConfig

	conn *net.UDPConn

	rxReliable *reliable.ReliableReceiver
	txReliable *reliable.ReliableSender

	updates chan FrameUpdate

	rttMu     sync.Mutex
	rtt       time.Duration
	lossRate  float64
	lastPing  int64
	pingSeq   uint32
	closing   int32
	closeOnce sync.Once
}

// NewClient creates a new Client instance with the given configuration.
// It does not start any background goroutines; call Start to begin network IO.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.ServerAddr == "" {
		return nil, fmt.Errorf("ServerAddr is empty")
	}
	if cfg.TickHz <= 0 {
		return nil, fmt.Errorf("TickHz must be > 0")
	}

	// Resolve server address but don't dial yet; Start will create the socket.
	if _, err := net.ResolveUDPAddr("udp", cfg.ServerAddr); err != nil {
		return nil, err
	}

	c := &Client{
		cfg:        cfg,
		updates:    make(chan FrameUpdate, 128),
		rxReliable: reliable.NewReliableReceiver(),
		txReliable: reliable.NewReliableSender(),
	}
	return c, nil
}

// Start begins the client's network processing: it creates the UDP socket and
// starts background goroutines for receiving packets and sending periodic ping.
func (c *Client) Start() error {
	if c.conn != nil {
		return nil
	}

	laddr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
	raddr, err := net.ResolveUDPAddr("udp", c.cfg.ServerAddr)
	if err != nil {
		return err
	}

	conn, err := net.DialUDP("udp", laddr, raddr)
	if err != nil {
		return err
	}
	c.conn = conn

	go c.readLoop()
	go c.pingLoop()

	return nil
}

// Close shuts down the client and releases any underlying resources.
func (c *Client) Close() error {
	if !atomic.CompareAndSwapInt32(&c.closing, 0, 1) {
		return nil
	}

	c.closeOnce.Do(func() {
		if c.conn != nil {
			_ = c.conn.Close()
		}
		close(c.updates)
	})

	return nil
}

// SendInput sends a single input for the given simulation tick to the server.
// The concrete wire format should mirror proto.InputPacket.
func (c *Client) SendInput(tick uint32, input uint32) error {
	if c.conn == nil {
		return fmt.Errorf("client not started")
	}

	buf := &bytes.Buffer{}

	// Build UDP header with packet sequence and current acks.
	packetSeq := c.txReliable.NextPacketSeq()
	ack, ackBits := c.rxReliable.BuildAckAndBits()
	proto.WriteUDPHeader(buf, packetSeq, ack, ackBits)

	// Write input packet payload.
	p := &proto.InputPacket{
		Tick:     tick,
		PlayerID: c.cfg.PlayerID,
		Input:    input,
		TS:       time.Now().UnixNano(),
	}
	proto.WriteInputPacket(buf, p)

	_, err := c.conn.Write(buf.Bytes())
	return err
}

// FrameUpdates returns a receive-only channel of FrameUpdate. The client
// implementation should push new frames here when they arrive from the server.
func (c *Client) FrameUpdates() <-chan FrameUpdate {
	return c.updates
}

// RTT returns the most recent round-trip time estimation between client and server.
func (c *Client) RTT() time.Duration {
	c.rttMu.Lock()
	defer c.rttMu.Unlock()
	return c.rtt
}

// LossRate returns an approximate recent packet loss rate as a fraction [0,1].
func (c *Client) LossRate() float64 {
	c.rttMu.Lock()
	defer c.rttMu.Unlock()
	return c.lossRate
}

// internal: background receive loop
func (c *Client) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			if atomic.LoadInt32(&c.closing) == 0 {
				fmt.Println("client recv err:", err)
			}
			return
		}
		packet := buf[:n]

		_, ack, ackBits, payload, err := proto.ReadUDPHeader(packet)
		if err != nil {
			fmt.Println("client bad header:", err)
			continue
		}

		// Process acks for our reliable sender.
		if c.txReliable != nil {
			cleared := c.txReliable.ProcessAckFromRemote(ack, ackBits)
			if len(cleared) > 0 {
				// Could track success stats here if desired.
				_ = cleared
			}
		}

		// Try parse as frame packet first.
		if tick, inputs, err := proto.ReadFramePacket(payload); err == nil {
			var snap []byte
			// Check for optional snapshot tail.
			if len(payload) > 0 {
				// payload layout: [frame_packet][optional: snapshot_len(uint16)+snap]
				r := bytes.NewReader(payload)
				var tmpTick uint32
				var count uint8
				_ = binary.Read(r, binary.LittleEndian, &tmpTick)
				_ = binary.Read(r, binary.LittleEndian, &count)
				for i := 0; i < int(count); i++ {
					var pid uint16
					var in uint32
					_ = binary.Read(r, binary.LittleEndian, &pid)
					_ = binary.Read(r, binary.LittleEndian, &in)
				}
				var snapLen uint16
				if err := binary.Read(r, binary.LittleEndian, &snapLen); err == nil && snapLen > 0 {
					if int(snapLen) <= r.Len() {
						snap = make([]byte, snapLen)
						if _, err := r.Read(snap); err != nil {
							snap = nil
						}
					}
				}
			}

			select {
			case c.updates <- FrameUpdate{Tick: tick, Inputs: inputs, Snap: snap}:
			default:
				// drop if consumer is too slow
			}
			continue
		}

		// Then try reliable envelope (e.g. ping/pong or game messages).
		if rseq, inner, err := proto.UnpackReliableEnvelope(payload); err == nil {
			if len(inner) < 1 {
				continue
			}
			c.rxReliable.MarkReceived(rseq)
			if c.rxReliable.AlreadyProcessed(rseq) {
				continue
			}
			c.rxReliable.MarkProcessed(rseq)

			msgType := inner[0]
			switch msgType {
			case proto.MsgPong:
				// inner layout: [msgType][8 bytes: clientTs]
				if len(inner) >= 9 {
					clientTs := int64(binary.LittleEndian.Uint64(inner[1:9]))
					now := time.Now().UnixNano()
					rtt := time.Duration(now-clientTs) * time.Nanosecond
					c.rttMu.Lock()
					c.rtt = rtt
					c.rttMu.Unlock()
				}
			default:
				// Application-specific reliable messages could be handled here
				// in the future by exposing a channel/callback.
			}
			continue
		}
	}
}

// internal: periodic ping loop to measure RTT.
func (c *Client) pingLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if atomic.LoadInt32(&c.closing) != 0 {
			return
		}
		c.sendPing()
	}
}

func (c *Client) sendPing() {
	if c.conn == nil {
		return
	}
	now := time.Now().UnixNano()
	c.lastPing = now

	payload := make([]byte, 1+8)
	payload[0] = proto.MsgPing
	binary.LittleEndian.PutUint64(payload[1:], uint64(now))

	seq := c.txReliable.AddPending(payload)
	ack, ackBits := c.rxReliable.BuildAckAndBits()
	packetSeq := c.txReliable.NextPacketSeq()

	buf := &bytes.Buffer{}
	proto.WriteUDPHeader(buf, packetSeq, ack, ackBits)
	proto.PackReliableEnvelope(buf, seq, payload)

	if _, err := c.conn.Write(buf.Bytes()); err == nil {
		c.txReliable.UpdatePendingSent(seq)
	}
}
