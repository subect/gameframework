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

剩余功能逐步完善。
