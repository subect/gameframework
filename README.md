Lightweight Lockstep Game Framework (Go)

轻量级帧同步 + 可靠 UDP 传输的游戏网络基础框架，适合多人动作、小型竞技与实时同步 Demo。

---

目录
- 核心特性
- 架构概览
- 目录结构
- 快速开始
- 已具备能力
- 后续规划

## 核心特性
- Reliable UDP 信道：PacketSeq / Ack / AckBits，自动重传、去重、Pending 管理，`ReliableEnvelope` 封装可靠消息（如 JoinRoom / PlayerList）。
- 轻量 Lockstep 帧同步：服务器固定 Tick（默认 20Hz）收集所有玩家输入并下发 authoritative frame；客户端 localTick 受 serverTick 限速，inputDelay 保障输入按时到达，本地预测 + 服务器校正，适合基础动作同步。
- RTT 自适应输入延迟：自动估算 RTT，`inputDelay = ceil(RTT / tick_ms) + 1`，高延迟环境仍保持同步稳定。
- 多人房间：可靠的 JoinRoom / JoinAck / PlayerList，可靠广播 PlayerJoined，输入聚合广播。
- 本地预测与补偿：输入即时应用，服务器 authoritative 输入到达后进行补偿修正。

## 架构概览
- Server
  - 固定 Tick 驱动，聚合各玩家输入，生成 authoritative frame 广播。
  - 维护房间、玩家列表，可靠消息通道保证关键事件到达。
- Client
  - localTick 受 serverTick 节流，按 inputDelay 提交输入。
  - 预测当前位置，收到 authoritative 后回放修正。
- 网络
  - UDP + 自定义可靠层，分离可靠与不可靠消息，降低时延同时确保关键状态一致。

## 目录结构
- `cmd/server`：示例服务器入口（帧同步核心）。
- `cmd/testclient`：**测试用文本客户端**（仅用于验证网络/帧同步，生产请自研前端）。
- `reliable/`：可靠 UDP 协议实现。
- `protocol/`：消息协议与封包定义。
- `server/`：房间、玩家管理与帧同步逻辑。
- `client/`：客户端帧同步与预测逻辑（供参考接入，可替换）。
- `game/`：示例游戏逻辑与 Tick 驱动。

## 快速开始（测试验证）
1) 启动服务器（默认 20Hz，可用 `--hz 60` 提升帧率）
```
go run ./cmd/server
```
2) 启动测试客户端（示例开启两个客户端）
```
go run ./cmd/testclient --id 1
go run ./cmd/testclient --id 2
```
3) 观察
- 客户端本地预测移动，服务器 authoritative 帧下发后校正。
- RTT 变化时，inputDelay 会自动调整以保持同步。

> 说明：测试客户端仅用于验证协议和帧同步流程；实际游戏前端请用 Unity/Cocos 等接入本框架的网络与协议层。

## Go SDK 接入
- 文档：详见 `docs/INTEGRATION.md`，包含协议格式、可靠层用法、服务器骨架与可配置项。
- 使用方式：通过 Go module 依赖本仓库，建议依赖发布的 tag/release，避免直接修改核心源码。

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