# Go SDK 接入指南（网络层）

本框架的重点是“网络层 + 协议”。推荐作为依赖使用，不改核心源码；示例游戏逻辑仅供参考，可自行替换。

## 公共 API 面（少量稳定接口）
- `pkg/proto`：`WriteInputPacket` / `ReadInputPacket` / `WriteFramePacket` / `ReadFramePacketWithPos` / `WriteUDPHeader` / `ReadUDPHeader` / `PackReliableEnvelope` / `UnpackReliableEnvelope`
- `pkg/reliable`：`NewReliableSender` / `NewReliableReceiver`；发送端 `AddPending → NextPacketSeq → BuildAckAndBits → UpdatePendingSent`；接收端 `MarkReceived/MarkProcessed → BuildAckAndBits`
- `pkg/server`：`NewServer(listen, tickHz)`，`ListenLoop` / `ReliableRetransmitLoop` / `BroadcastLoop`
- `pkg/game`：示例输入应用，可替换为你自己的游戏逻辑，仅复用网络/协议层

## 快速接入

### 服务器（作为依赖）
```go
import "gameframework/pkg/server"

srv, _ := server.NewServer(":30000", 60) // 监听 + 60Hz
go srv.ListenLoop()                      // 接收输入/可靠消息
go srv.ReliableRetransmitLoop()          // 可靠重传
srv.BroadcastLoop()                      // 帧广播（阻塞）
```

### 客户端协议（如自研客户端）
- 输入上报：使用 `proto.WriteInputPacket` 构造 `Tick, PlayerID, Input, TS`，带上 UDP 头（PacketSeq/Ack/AckBits），发送。
- 帧接收：`proto.ReadFramePacketWithPos(payload)` 解析 `tick + inputs + players positions`，驱动本地回放/渲染。
- 可靠消息：需要可靠通道的控制类消息（JoinRoom/JoinAck/PlayerList/PlayerJoined）用 `proto.PackReliableEnvelope` 包裹，配合 `reliable.ReliableSender/Receiver` 管理 pending/重传/去重。

### 可靠层
- 发送端：`reliable.NewReliableSender()` → `AddPending(payload)` → `NextPacketSeq()` → `BuildAckAndBits()` → 写 UDP 头 → `PackReliableEnvelope()` → 发送 → `UpdatePendingSent(seq)`；收到 ack 用 `ProcessAckFromRemote()`
- 接收端：`reliable.NewReliableReceiver()` → 收包 `MarkReceived()`，业务处理后 `MarkProcessed()`；回包时 `BuildAckAndBits()` 生成 Ack/AckBits

### 可配置项
- 帧率：`server.NewServer(listen, tickHz)` 的 `tickHz`（默认 20，常用 60）
- 速度：`pkg/game/game.go` 中 `ApplyInput` 的 `speed`（默认 0.5，配合 60Hz 等效之前 20Hz/1.5）
- 端口/地址：启动参数或自定义

## 接入示例（开发新游戏时的最小路径）
1) **服务端复用网络层，替换游戏逻辑**
   - 继续使用 `pkg/server`、`pkg/proto`、`pkg/reliable`。
   - 将 `pkg/game` 换成你的游戏逻辑（例如自定义输入枚举与状态更新），确保对外暴露与 `ApplyInput(pid, input)` 类似的接口，被 `BroadcastLoop` 调用即可。
2) **定义输入与状态协议**
   - 参考 `pkg/proto.InputPacket` 与 `WriteInputPacket/ReadInputPacket`，按你的游戏输入枚举扩展 `input` 字段。
   - 如果需要广播更多状态，可在帧 payload 中追加自定义状态块，或仿照 `ReadFramePacketWithPos` 扩展。
3) **客户端（任意语言/引擎，如 Unity/Cocos）**
   - 发送输入：构造 InputPacket（LittleEndian），加 UDP 头（PacketSeq/Ack/AckBits），可选可靠封装后发给服务器。
   - 收帧：解析帧（tick + inputs + 你的状态），驱动本地回放/渲染；按需做插值/预测。
   - 可靠消息：控制类消息（Join/PlayerList 等）用可靠通道；运动帧可走不可靠通道。
4) **配置与运行**
   - `go run ./cmd/server --hz 60`（或自定端口/帧率）。
   - 测试用：`go run ./cmd/testclient --id 1`（仅验证网络流程，实际客户端用你自己的前端）。
5) **依赖方式**
   - 在你的后端/工具 go.mod 中：`require github.com/subect/gameframework vX.Y.Z`
   - 只依赖 `pkg/proto/reliable/server`（游戏逻辑可自带），避免修改核心源码。

## 约定与建议
- 作为依赖使用：通过 Go module 引用，不建议修改核心包（`pkg/reliable`, `pkg/proto`, `pkg/server`, `pkg/game`）。
- 稳定版本：优先依赖已发布的 tag/release 而非 main。
- 游戏逻辑：可自行实现/替换，复用网络与协议层。
- 前端实现：Unity/Cocos/自研客户端按协议自行实现输入上报与帧解析，参考 `pkg/proto` 的格式。
- 贡献方式：如需改动核心，请先提 Issue/PR 讨论。

