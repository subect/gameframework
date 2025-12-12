package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"gameframework/pkg/game"
	"gameframework/pkg/proto"
	"gameframework/pkg/reliable"
	"math"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
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
	// 将终端切换为原始模式以便即时按键读取
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err == nil {
		defer term.Restore(int(os.Stdin.Fd()), oldState)
	}
	// 捕获中断信号，确保恢复终端
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		if oldState != nil {
			term.Restore(int(os.Stdin.Fd()), oldState)
		}
		os.Exit(0)
	}()
	// create conn
	raddr, _ := net.ResolveUDPAddr("udp", *serverAddr)
	conn, _ := net.ListenUDP("udp", nil)

	// lightweight client state (mirrors earlier client struct)
	id16 := uint16(*id)
	serverTick := uint32(0)
	inputDelay := 2
	rxReliable := reliable.NewReliableReceiver()
	txReliable := reliable.NewReliableSender()

	buffer := make(map[uint32]map[uint16]uint32)
	playerPositions := make(map[uint16]*proto.PlayerPos) // 所有玩家位置
	playerPosMu := sync.RWMutex{}                        // 保护 playerPositions
	lastRenderTick := uint32(0)
	lastRenderHash := ""
	renderCycle := 0 // 控制清屏频率，减少闪烁
	rttFiltered := 200.0
	rttAlpha := 0.1
	rttMinDelay := 2
	currentInput := uint32(game.InputNone) // 当前输入
	inputMutex := sync.Mutex{}             // 保护currentInput的锁

	// 渲染配置
	const (
		canvasW = 60
		canvasH = 20
	)
	playerColors := []string{
		"\033[38;5;196m", // 红
		"\033[38;5;39m",  // 蓝
		"\033[38;5;226m", // 黄
		"\033[38;5;46m",  // 绿
		"\033[38;5;201m", // 粉
		"\033[38;5;214m", // 橙
		"\033[38;5;51m",  // 青
	}
	resetColor := "\033[0m"

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
		// 加入房间请求已发送，不再打印调试信息
	}

	// recv loop
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				// 接收错误，不再打印调试信息
				continue
			}
			_, ack, ackbits, payload, err := proto.ReadUDPHeader(buf[:n])
			if err != nil {
				// 包头解析错误，不再打印调试信息
				continue
			}
			if txReliable != nil {
				cleared := txReliable.ProcessAckFromRemote(ack, ackbits)
				_ = cleared // 不再打印调试信息，避免干扰显示
			}
			// 尝试读取包含位置的帧数据
			if tick, inputs, positions, err := proto.ReadFramePacketWithPos(payload); err == nil && len(positions) > 0 {
				serverTick = tick
				buffer[tick] = inputs
				// 更新玩家位置
				playerPosMu.Lock()
				for _, pos := range positions {
					pcopy := pos
					playerPositions[pcopy.PID] = &pcopy
				}
				playerPosMu.Unlock()
				continue
			}
			// 兼容旧格式
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
						// 加入成功，不再打印调试信息
					}
				case proto.MsgPlayerList:
					if !rxReliable.AlreadyProcessed(rseq) {
						rxReliable.MarkProcessed(rseq)
						// 玩家列表更新，不再打印调试信息
					}
				case proto.MsgPlayerJoined:
					if !rxReliable.AlreadyProcessed(rseq) {
						rxReliable.MarkProcessed(rseq)
						// 新玩家加入，不再打印调试信息
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
							// RTT调整，不再打印调试信息
							inputDelay = newDelay
						}
					}
					if !rxReliable.AlreadyProcessed(rseq) {
						rxReliable.MarkProcessed(rseq)
					}
				default:
					if !rxReliable.AlreadyProcessed(rseq) {
						rxReliable.MarkProcessed(rseq)
						// 未知消息类型，不再打印调试信息
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
					// 重传错误，不再打印调试信息
				} else {
					txReliable.UpdatePendingSent(pm.Seq)
					// 重传成功，不再打印调试信息
				}
			}
		}
	}()

	// 键盘输入读取
	go func() {
		reader := bufio.NewReader(os.Stdin)
		for {
			b, err := reader.ReadByte()
			if err != nil {
				break
			}
			// 只处理ASCII字符，忽略转义序列等
			if b >= 32 && b <= 126 {
				char := strings.ToUpper(string(b))[0]
				inputMutex.Lock()
				switch char {
				case 'A':
					currentInput = game.InputLeft
				case 'D':
					currentInput = game.InputRight
				case 'W':
					currentInput = game.InputUp
				case 'S':
					currentInput = game.InputDown
				case 'Q':
					// 退出游戏，恢复终端
					inputMutex.Unlock()
					if oldState != nil {
						term.Restore(int(os.Stdin.Fd()), oldState)
					}
					os.Exit(0)
				default:
					currentInput = game.InputNone
				}
				inputMutex.Unlock()
			} else if b == 3 { // Ctrl+C
				if oldState != nil {
					term.Restore(int(os.Stdin.Fd()), oldState)
				}
				os.Exit(0)
			}
			// 忽略其他控制字符和转义序列
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
			inputMutex.Lock()
			input := currentInput
			// 发送后立即重置为InputNone，这样按一次键只会移动一次
			// 如果需要持续移动，需要持续按键
			currentInput = game.InputNone
			inputMutex.Unlock()

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
		}
	}()

	clampInt := func(v, lo, hi int) int {
		if v < lo {
			return lo
		}
		if v > hi {
			return hi
		}
		return v
	}

	renderHash := func(posMap map[uint16]*proto.PlayerPos) string {
		keys := make([]int, 0, len(posMap))
		for k := range posMap {
			keys = append(keys, int(k))
		}
		sort.Ints(keys)
		sb := strings.Builder{}
		for _, k := range keys {
			p := posMap[uint16(k)]
			if p != nil {
				fmt.Fprintf(&sb, "%d:%.2f,%.2f|", k, p.X, p.Y)
			}
		}
		return sb.String()
	}

	// 显示循环
	ticker := time.NewTicker(200 * time.Millisecond) // 5Hz 显示刷新，减轻闪烁
	for range ticker.C {
		// 拷贝快照，避免读写竞争
		playerPosMu.RLock()
		snapshot := make(map[uint16]*proto.PlayerPos, len(playerPositions))
		for k, v := range playerPositions {
			if v != nil {
				pcopy := *v
				snapshot[k] = &pcopy
			}
		}
		playerPosMu.RUnlock()

		// 仅在状态变化时重绘，减少闪烁
		hash := renderHash(snapshot)
		if serverTick == lastRenderTick && hash == lastRenderHash {
			continue
		}
		lastRenderTick = serverTick
		lastRenderHash = hash

		// 控制清屏频率：每3帧清屏一次，降低闪烁
		if renderCycle%3 == 0 {
			fmt.Print("\033[2J") // 清屏
		}
		renderCycle++
		fmt.Print("\033[H") // 光标回到左上角
		fmt.Println("=== 多人房间 - 彩色小球 ===")
		fmt.Printf("你的ID: %d | Server Tick: %d\n", id16, serverTick)
		fmt.Println("---------------------------")

		if len(snapshot) == 0 {
			fmt.Println("等待玩家加入...")
			continue
		}

		// 按玩家ID排序，确保显示顺序稳定
		playerIDs := make([]uint16, 0, len(snapshot))
		for pid := range snapshot {
			playerIDs = append(playerIDs, pid)
		}
		for i := 0; i < len(playerIDs)-1; i++ {
			for j := i + 1; j < len(playerIDs); j++ {
				if playerIDs[i] > playerIDs[j] {
					playerIDs[i], playerIDs[j] = playerIDs[j], playerIDs[i]
				}
			}
		}

		// 彩色画布
		canvas := make([][]string, canvasH)
		for y := range canvas {
			canvas[y] = make([]string, canvasW)
			for x := range canvas[y] {
				canvas[y][x] = " "
			}
		}

		// 绘制玩家
		for _, pid := range playerIDs {
			pos := snapshot[pid]
			// 将世界坐标映射到画布
			scale := float32(1.5)
			x := int(pos.X*scale) + canvasW/2
			y := canvasH/2 - int(pos.Y*scale)
			x = clampInt(x, 0, canvasW-1)
			y = clampInt(y, 0, canvasH-1)

			color := playerColors[int(pid)%len(playerColors)]
			symbol := "●" // 彩色小球
			if pid == id16 {
				symbol = "⬤" // 自己用更实心的球
			}
			canvas[y][x] = fmt.Sprintf("%s%s%s", color, symbol, resetColor)
		}

		// 绘制边框与画布
		top := "+" + strings.Repeat("-", canvasW) + "+"
		fmt.Println(top)
		for y := 0; y < canvasH; y++ {
			row := strings.Builder{}
			row.WriteByte('|')
			for x := 0; x < canvasW; x++ {
				row.WriteString(canvas[y][x])
			}
			row.WriteByte('|')
			fmt.Println(row.String())
		}
		bottom := "+" + strings.Repeat("-", canvasW) + "+"
		fmt.Println(bottom)

		// HUD
		fmt.Println("\n玩家列表（按ID排序）：")
		for _, pid := range playerIDs {
			pos := snapshot[pid]
			marker := " "
			if pid == id16 {
				marker = "*"
			}
			color := playerColors[int(pid)%len(playerColors)]
			fmt.Printf("%s玩家 %d%s%s: X=%.2f, Y=%.2f\n", color, pid, marker, resetColor, pos.X, pos.Y)
		}

		fmt.Println("\n按 W/A/S/D 移动，Q 退出")
	}
}
