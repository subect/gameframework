package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"gameframework/pkg/proto"
	"gameframework/pkg/reliable"
	"math"
	"math/rand"
	"net"
	"time"
)

func nowMs() int64 { return time.Now().UnixNano() / int64(time.Millisecond) }

func main() {
	mode := flag.String("mode", "client", "server or client")
	serverAddr := flag.String("server", "127.0.0.1:30000", "server addr")
	id := flag.Int("id", 1, "client id")
	tickHz := flag.Int("hz", 20, "tick hz")
	flag.Parse()

	if *mode != "client" {
		fmt.Println("usage: -mode=client")
		return
	}
	// create conn
	raddr, _ := net.ResolveUDPAddr("udp", *serverAddr)
	conn, _ := net.ListenUDP("udp", nil)

	// lightweight client state (mirrors earlier client struct)
	id16 := uint16(*id)
	localTick := uint32(0)
	serverTick := uint32(0)
	jitter := 3
	inputDelay := 2
	rxReliable := reliable.NewReliableReceiver()
	txReliable := reliable.NewReliableSender()

	buffer := make(map[uint32]map[uint16]uint32)
	pos := float32(0.0)
	rttFiltered := 200.0
	rttAlpha := 0.1
	rttMinDelay := 2

	// send join
	{
		msg := []byte{proto.MsgJoinRoom, byte(id16)}
		seq := txReliable.AddPending(msg)
		ack, ackbits := rxReliable.BuildAckAndBits()
		packetSeq := txReliable.NextPacketSeq()
		buf := &bytes.Buffer{}
		proto.WriteUDPHeader(buf, packetSeq, ack, ackbits)
		proto.PackReliableEnvelope(buf, seq, msg)
		_, _ = conn.WriteToUDP(buf.Bytes(), raddr)
		txReliable.UpdatePendingSent(seq)
		fmt.Printf("Client sent JoinRoom reliableSeq=%d packetSeq=%d\n", seq, packetSeq)
	}

	// recv loop
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				fmt.Println("client recv err:", err)
				continue
			}
			packetSeq, ack, ackbits, payload, err := proto.ReadUDPHeader(buf[:n])
			if err != nil {
				fmt.Println("client bad header:", err)
				continue
			}
			if txReliable != nil {
				cleared := txReliable.ProcessAckFromRemote(ack, ackbits)
				if len(cleared) > 0 {
					fmt.Printf("Client: pending cleared %v\n", cleared)
				}
			}
			if tick, inputs, err := proto.ReadFramePacket(payload); err == nil {
				serverTick = tick
				buffer[tick] = inputs
				continue
			}
			if rseq, inner, err := proto.UnpackReliableEnvelope(payload); err == nil {
				if len(inner) < 1 {
					continue
				}
				rxReliable.MarkReceived(rseq)
				switch inner[0] {
				case proto.MsgJoinAck:
					if !rxReliable.AlreadyProcessed(rseq) {
						rxReliable.MarkProcessed(rseq)
						if len(inner) >= 2 {
							playerID := int(inner[1])
							fmt.Printf("Client: JoinAck received. assigned id=%d (reliableSeq=%d packetSeq=%d)\n", playerID, rseq, packetSeq)
						}
					}
				case proto.MsgPlayerList:
					if !rxReliable.AlreadyProcessed(rseq) {
						rxReliable.MarkProcessed(rseq)
						ids := proto.ParsePlayerList(inner)
						fmt.Printf("Client: PlayerList=%v (reliableSeq=%d)\n", ids, rseq)
					}
				case proto.MsgPlayerJoined:
					if !rxReliable.AlreadyProcessed(rseq) {
						rxReliable.MarkProcessed(rseq)
						id := proto.ParsePlayerJoined(inner)
						fmt.Printf("Client: PlayerJoined=%d (reliableSeq=%d)\n", id, rseq)
					}
				case proto.MsgPong:
					if len(inner) >= 9 {
						clientTs := int64(binary.LittleEndian.Uint64(inner[1:9]))
						now := nowMs()
						sampleRtt := float64(now - clientTs)
						rttFiltered = rttFiltered*(1.0-rttAlpha) + sampleRtt*rttAlpha
						tickMs := float64(1000 / *tickHz)
						estimatedTicks := int(math.Ceil(rttFiltered / tickMs))
						newDelay := estimatedTicks + 1
						if newDelay < rttMinDelay {
							newDelay = rttMinDelay
						}
						if newDelay > 30 {
							newDelay = 30
						}
						if newDelay != inputDelay {
							fmt.Printf("Client: RTT sample=%.1fms filtered=%.1fms -> inputDelay %d->%d ticks\n", sampleRtt, rttFiltered, inputDelay, newDelay)
							inputDelay = newDelay
						}
					}
					if !rxReliable.AlreadyProcessed(rseq) {
						rxReliable.MarkProcessed(rseq)
					}
				default:
					if !rxReliable.AlreadyProcessed(rseq) {
						rxReliable.MarkProcessed(rseq)
						fmt.Printf("Client: unknown reliable msg type=%d\n", inner[0])
					}
				}
				continue
			}
		}
	}()

	// ping loop
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			ts := nowMs()
			payload := make([]byte, 1+8)
			payload[0] = proto.MsgPing
			binary.LittleEndian.PutUint64(payload[1:], uint64(ts))
			seq := txReliable.AddPending(payload)
			ack, ackbits := rxReliable.BuildAckAndBits()
			packetSeq := txReliable.NextPacketSeq()
			buf := &bytes.Buffer{}
			proto.WriteUDPHeader(buf, packetSeq, ack, ackbits)
			proto.PackReliableEnvelope(buf, seq, payload)
			_, _ = conn.WriteToUDP(buf.Bytes(), raddr)
			txReliable.UpdatePendingSent(seq)
		}
	}()

	// retransmit loop
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		for range ticker.C {
			pending := txReliable.GetPendingOlderThan(200)
			for _, pm := range pending {
				ack, ackbits := rxReliable.BuildAckAndBits()
				packetSeq := txReliable.NextPacketSeq()
				buf := &bytes.Buffer{}
				proto.WriteUDPHeader(buf, packetSeq, ack, ackbits)
				proto.PackReliableEnvelope(buf, pm.Seq, pm.Payload)
				_, err := conn.WriteToUDP(buf.Bytes(), raddr)
				if err != nil {
					fmt.Println("client retransmit err:", err)
				} else {
					txReliable.UpdatePendingSent(pm.Seq)
					fmt.Printf("Client retransmit reliableSeq=%d packetSeq=%d\n", pm.Seq, packetSeq)
				}
			}
		}
	}()

	// send input loop
	go func() {
		ticker := time.NewTicker(time.Duration(1000 / *tickHz) * time.Millisecond)
		for range ticker.C {
			if serverTick == 0 {
				continue
			}
			sendTick := serverTick + uint32(inputDelay)
			input := uint32(0)
			r := rand.Intn(3) - 1
			if r < 0 {
				input = 1
			} else if r > 0 {
				input = 2
			}
			p := &proto.InputPacket{Tick: sendTick, PlayerID: uint16(id16), Input: input, TS: nowMs()}
			pl := &bytes.Buffer{}
			proto.WriteInputPacket(pl, p)
			payload := pl.Bytes()
			ack, ackbits := rxReliable.BuildAckAndBits()
			packetSeq := txReliable.NextPacketSeq()
			ubuf := &bytes.Buffer{}
			proto.WriteUDPHeader(ubuf, packetSeq, ack, ackbits)
			ubuf.Write(payload)
			_, _ = conn.WriteToUDP(ubuf.Bytes(), raddr)
			// local prediction
			if input == 1 {
				pos -= 0.1
			} else if input == 2 {
				pos += 0.1
			}
		}
	}()

	// simulate loop (single-thread logging)
	ticker := time.NewTicker(time.Duration(1000 / *tickHz) * time.Millisecond)
	for range ticker.C {
		// allowed = serverTick - inputDelay
		var allowed uint32 = 0
		if serverTick > uint32(inputDelay) {
			allowed = serverTick - uint32(inputDelay)
		} else {
			allowed = 0
		}
		if localTick < allowed {
			localTick++
		} else {
			fmt.Printf("[client %d] tick %d pos=%.2f (waiting for server tick %d)\n", id16, localTick, pos, serverTick)
			continue
		}
		if localTick < uint32(jitter) {
			fmt.Printf("[client %d] tick %d pos=%.2f (warming)\n", id16, localTick, pos)
			continue
		}
		target := localTick - uint32(jitter)
		if inputs, ok := buffer[target]; ok {
			if in, ex := inputs[uint16(id16)]; ex {
				if in == 1 {
					pos -= 0.1
				} else if in == 2 {
					pos += 0.1
				}
			}
			delete(buffer, target)
		} else {
			// rely on prediction
		}
		fmt.Printf("[client %d] tick %d pos=%.2f\n", id16, localTick, pos)
	}
}
