# Firetower 架构概览

Firetower 是一个围绕 Topic 发布/订阅模型打造的分布式推送服务，整体由 Topic 管理节点和若干 WebSocket 网关节点协同提供订阅登记、消息投递以及连接管理等能力。

## 核心组件

### WebSocket 网关（Gateway）
- **连接生命周期管理**：`FireTower` 对象封装单条客户端连接的状态、读写队列以及订阅列表，并在 `Run` 启动时并行执行读取、处理与发送循环。Gateway 在关闭连接时会自动退订全部 Topic 并回收资源。
- **本地订阅索引**：`TowerManager` 将同一实例的连接划分到多个 `Bucket`，每个桶内部维护 `topic -> connection` 的映射，并在收到中心广播后并发向所有匹配连接推送消息。
- **Topic 管理客户端**：启动阶段通过 `Init` 加载配置、构建 Bucket，并建立到 Manager 的 gRPC 与 TCP 客户端，用于提交订阅关系、查询 Topic 状态和写入推送数据通道。
- **回调扩展点**：Gateway 暴露 `SetReadHandler`、`SetSubscribeHandler` 等回调，允许业务侧在订阅、取消订阅或消息到达时插入自定义逻辑。

### Topic 管理服务（Manager）
- **订阅关系中心**：Manager 使用 gRPC 提供 `SubscribeTopic`、`UnSubscribeTopic` 和 `CheckTopicExist` 等接口，将每个 Topic 与发起订阅的 Gateway 节点 IP 建立映射，并记录订阅计数。
- **推送总线**：收到 gRPC `Publish` 调用后，Manager 会封包消息并通过长连 TCP 通道广播给所有注册的 Gateway；TCP 服务端负责接收来自 Gateway 的推送请求，并可按用户或 Topic 做离线清理。
- **运行时观测**：内置 HTTP Dashboard，可按 Topic 返回订阅量等运行时指标，便于监控与排障。

### Socket 协议层
Manager 与 Gateway 之间的 TCP 通道使用自定义协议：帧以固定头 `FireHeader` 开始，包含长度字段、指令类型、消息 ID、消息来源、Topic 以及消息体。`Enpack` 与 `Depack` 函数分别负责组包与拆包，确保跨进程通信时的完整性与粘包处理。

## 进程间协作流程
1. **Gateway 启动**：调用 `Init` 函数读取配置、创建 Bucket，并建立到 Manager 的 gRPC 与 TCP 连接。
2. **连接接入**：业务代码通过 `BuildTower` 包装 WebSocket 连接；`Run` 方法启动后即进入读写循环，并可在连接回调中完成鉴权或初始化。
3. **订阅登记**：客户端发送订阅消息时，Gateway 先在本地 Bucket 建立 Topic -> Connection 映射，再通过 gRPC 将订阅同步到 Manager，确保后续推送可路由到该 Gateway。
4. **消息推送**：业务通过 `Publish` 将消息写入 Manager 的 TCP 通道；Manager 解包后，按 Topic 将消息分发至已登记的 Gateway。Gateway 接收后再由 `TowerManager` 将消息广播到所属 Bucket，最终写回各个 WebSocket 连接。

## 部署拓扑
- **单节点调试**：可将 Manager 与 Gateway 同机部署，适用于开发或低并发场景。
- **多实例扩缩容**：生产环境通常会部署多个 Gateway 实例，并为每个实例配置唯一的 `ClusterId`。所有 Gateway 必须连接同一个 Manager，以便集中维护订阅关系。
- **外部依赖**：Firetower 不依赖 Redis 等外部队列，消息路由全部由自建的 Manager 服务负责。

## 参考图示
可复用仓库 README 中的架构图 `firetower_process.png`，展示客户端、Gateway 与 Manager 之间的调用关系与消息流向。
