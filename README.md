# gameframework

轻量级帧同步游戏网络框架（Go），提供可靠 UDP 传输和帧同步的网络层/协议库。

## 核心包

- **`pkg/netcore`** - 网络核心，定义 `GameLogic` 接口
- **`pkg/proto`** - 协议层（序列化/反序列化）
- **`pkg/reliable`** - 可靠传输层（UDP 可靠传输）

## 特性

- 可靠 UDP 传输
- 帧同步机制
- 自定义游戏逻辑接口
- 高性能优化

## 功能实现情况

| 功能 | 实现情况 | 备注 |
|------|---------|------|
| Delay-based Lockstep | ✅ | 目前项目实现 |
| Reliable UDP | ✅ | 工程级别基础 |
| RTT adaptive delay | ✅ | 自动延迟计算 |
| Prediction + Correction | ✅ | 客户端 |
| Rollback + Re-simulate | ❌ | 高级特性 |
| Deterministic simulation | ❌ | 高级特性 |
| Frame interpolation | ❌ | 可选特性 |
| Snapshot / delta sync | ❌ | 高级状态优化 |

剩余功能逐步完善。
