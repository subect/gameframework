Lightweight Lockstep Game Framework (Go)

轻量级帧同步 + 可靠 UDP 传输的网络层/协议库。可作为游戏后端的网络底座，前端（如 Unity/Cocos）可按协议接入。非网关，聚焦帧同步与可靠传输。

---

目录
- 这是啥 / 提供什么
- 目录结构
- 快速开始（测试）
- Go SDK 接入
- 已具备能力
- 后续规划

## 这是啥 / 提供什么
- 网络层/协议库（Go）：可靠 UDP（PacketSeq/Ack/AckBits）、帧同步服务器骨架、基础协议序列化。
- 轻量 Lockstep：固定 Tick（默认 20Hz，可 60Hz），收集输入、下发 authoritative frame。
- 自适应延迟：基于 RTT 调整 inputDelay。
- 房间与可靠控制消息：JoinRoom/JoinAck/PlayerList/PlayerJoined 走可靠通道；输入帧可不可靠。
- 示例游戏逻辑：2D 位置与输入应用，可替换。

## 目录结构
- `cmd/server`：示例服务器入口（帧同步核心）
- `cmd/testclient`：测试用文本客户端（仅验证网络/帧同步，生产请自研前端）
- `reliable/`：可靠 UDP 实现
- `protocol/`：消息协议与封包定义
- `server/`：房间、玩家管理、帧同步逻辑
- `client/`：客户端帧同步与预测逻辑（参考/可替换）
- `game/`：示例游戏逻辑与 Tick 驱动

## 快速开始（测试验证）
1) 启动服务器（默认 20Hz，可用 `--hz 60` 提升帧率）  
```
go run ./cmd/server
```
2) 启动测试客户端（仅用于验证网络/帧同步）  
```
go run ./cmd/testclient --id 1
go run ./cmd/testclient --id 2
```
3) 观察  
- 客户端本地预测移动，服务器 authoritative 帧下发后校正  
- RTT 变化时，inputDelay 自动调整  

> 测试客户端仅用于验证；生产前端请自研（Unity/Cocos 等），按协议接入。

## Go SDK 接入
- 文档：详见 `docs/INTEGRATION.md`（公共 API、协议/可靠层、服务器骨架与配置）。
- 依赖：在你的 go.mod 中引用发布的 tag/release（避免改核心源码）。
- 服务器最小示例：
  ```go
  import "gameframework/pkg/server"

  func main() {
      srv, _ := server.NewServer(":30000", 60)
      go srv.ListenLoop()
      go srv.ReliableRetransmitLoop()
      srv.BroadcastLoop()
  }
  ```
- 客户端（任意语言/引擎）：按 `pkg/proto` 的 InputPacket/FramePacket 格式发送输入、解析帧；可靠控制消息用 `reliable` 流程（AddPending/BuildAckAndBits/ProcessAck）。

## 使用建议（作为底层网络框架）
- 作为依赖使用：将本仓库作为 Go module 依赖，按协议接入，不建议改动核心源码。
- 配置优先：端口、tick 频率、速度等通过启动参数或配置调整，避免直接修改核心逻辑。
- 稳定版本：建议依赖发布的 tag/release，而非 main 开发分支。
- 贡献方式：如需改动核心，请先提 Issue/PR 讨论；保护核心代码路径（`pkg/reliable/`, `pkg/proto/`, `pkg/server/`, `pkg/game/`），避免直接修改。
- 前端接入：Unity/Cocos 等客户端建议仅复用协议与网络层，测试客户端仅作参考。

## 已具备能力
- 多玩家移动同步与帧广播。
- 帧同步 + 本地预测 + 服务器校正。
- 自动 RTT 延迟适应。
- 可靠消息管道（房间加入、玩家列表、玩家加入广播）。
- 游戏 Tick 控制。
- 适合作为入门 Lockstep 技术研究与示例项目。

## 后续规划
为更接近完整帧同步 / GGPO 级别，建议补充：
- Rollback & Re-simulate（★★★★★）：保存状态快照，authoritative 到达后回滚并重演。
- 丢包 / 乱序处理（★★★）：输入补齐、Frame 重排序。
- Deterministic（★★★★）：固定点数学，避免浮点误差。
- 状态快照系统（★★★）：支撑 rollback。
- 插值 / 平滑处理（★★★）：混合历史帧，画面稳定。