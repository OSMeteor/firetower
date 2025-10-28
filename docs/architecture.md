# Firetower 架构概览

Firetower 是一个围绕 Topic 发布/订阅模型打造的分布式推送服务，整体由 Topic 管理节点和若干 WebSocket 网关节点协同提供订阅登记、消息投递以及连接管理等能力。

## 核心组件

### WebSocket 网关（Gateway）
- **连接生命周期管理**：`service/gateway/tower.go` 中的 `FireTower` 对象封装单条客户端连接的状态、读写队列以及订阅列表，并在 `Run` 启动时并行执行读取、处理与发送循环。Gateway 在关闭连接时会自动退订全部 Topic 并回收资源。
- **本地订阅索引**：`service/gateway/tower_manager.go` 中的 `TowerManager` 将同一实例的连接划分到多个 `Bucket`，每个桶内部维护 `topic -> connection` 的映射，并在收到中心广播后并发向所有匹配连接推送消息。Bucket 会在 Gateway 启动时按配置一次性初始化并常驻进程，后续通过连接关闭或退订流程调用 `DelSubscribe`/`unbindTopic` 清理条目，因此无需额外的 Bucket 回收逻辑。
- **Topic 管理客户端**：启动阶段通过 `Init` 加载配置、构建 Bucket，并建立到 Manager 的 gRPC 与 TCP 客户端，用于提交订阅关系、查询 Topic 状态和写入推送数据通道。
- **回调扩展点**：Gateway 暴露 `SetReadHandler`、`SetSubscribeHandler` 等回调，允许业务侧在订阅、取消订阅或消息到达时插入自定义逻辑。

### Topic 管理服务（Manager）
- **订阅关系中心**：`service/manager/topic_manage_service.go` 使用 gRPC 暴露 `SubscribeTopic`、`UnSubscribeTopic` 和 `CheckTopicExist` 等接口，将每个 Topic 与发起订阅的 Gateway 节点 IP 建立映射，并记录订阅计数。
- **推送总线**：收到 gRPC `Publish` 调用后，Manager 会封包消息并通过长连 TCP 通道广播给所有注册的 Gateway；`service/manager/connect_bucket.go` 中的 TCP 服务端负责接收来自 Gateway 的推送请求，并可按用户或 Topic 做离线清理。
- **运行时观测**：内置 HTTP Dashboard，可按 Topic 返回订阅量等运行时指标，便于监控与排障。
- **对外端口**：默认监听两个端口——`6666` 用于 Gateway 通过 TCP 建立长连接执行推送与心跳，`6667` 暴露 gRPC 服务给业务或 Gateway 发起订阅、退订与发布请求；两者分别在 `config/fireTower.toml` 的 `topicServiceAddr` 和 `[grpc].address` 中配置。
- **水平扩展策略**：当单个 Manager 无法满足大型业务的吞吐或可用性要求时，可按以下步骤演进：
  - **拆分读写职责**：保留现有 TCP 推送与 `topicRelevance` 维护逻辑（`service/manager/topic_relevance.go`），将 gRPC 面向业务的 `Publish`/`SubscribeTopic` 请求放置在多个前端进程上，通过一致性哈希或 `ClusterId` 划分把操作路由到对应的状态节点。
  - **共享状态存储**：引入诸如 Redis、TiKV 等外部存储保存 Topic→Gateway 映射，Manager 状态节点只缓存热点数据，定期或在连接掉线时回写；需确保订阅登记与 TCP 广播之间具备幂等或事务保障，避免重复推送。
    - **Redis 部署建议**：采用主从 + Sentinel 或者 Cluster 模式，确保写入能力与高可用；为避免热点，可按 `topic:{hash_slot}:members` 切分键空间，`hash_slot` 可基于 Topic 名的 CRC16 取模。
    - **写入流程**：在处理 `SubscribeTopic`/`UnSubscribeTopic` 时先更新 Redis（使用 `HINCRBY`/`HDEL` 维护计数和连接列表），再将结果写入本地 `topicRelevance` 缓存；若 Redis 写入失败则回滚本地缓存并向 Gateway 返回错误，避免状态漂移。
    - **推送读取**：`Publish` 时读取 Redis 中的订阅列表，可配合 Lua 脚本一次性取回连接列表并记录版本号；Manager 缓存最近一次读取的版本，当 Redis 版本更新时刷新本地缓存，降低频繁读 Redis 的开销。
    - **失效清理**：利用 Redis `PEXPIRE` 让订阅键在心跳超时后自动过期，或在 Manager 的 `checkHeartBeat` 中对离线连接执行 `ZREM`/`HDEL`；确保所有 Gateway 在断线时会执行 `UnSubscribeTopic` 以保持 Redis 索引干净。
    - **推送协调**：多 Manager 并发推送时，可通过 Redis Stream/PubSub 将 Publish 事件广播给所有活跃 Manager，或使用分布式锁（例如基于 `SETNX` 的租约）确保同一 Topic 的消息仅由一个 Manager 推送。
  - **推送协调层**：对 TCP 推送通道做多活部署时，可将 `ConnectBucket` 封装为共享的协调层，利用消息队列或内部 RPC 将 `Publish` 请求分发到各个在线 Manager 连接上，保证每个 Gateway 仅接受一份数据。
  - **故障切换**：结合现有心跳与清理逻辑（`service/manager/manager.go` 中的 `checkHeartBeat`），为每个状态节点设置健康检测与自动主备切换策略；当节点不可用时，从共享存储恢复订阅索引并重建与 Gateway 的长连。

#### Bucket 扩容策略（面向 5k～10k 订阅者）
- **配置入口**：`config/fireTower.toml` 中的 `[bucket]` 配置块控制 `Num`（Bucket 个数）、`BuffChanCount`（每个 Bucket 的推送队列容量）与 `ConsumerNum`（每个 Bucket 并发执行 `consumer` 的 goroutine 数量）。`buildBuckets` 会在 Gateway 启动时依据这些参数构建 `TowerManager.bucket`，并为每个 Bucket 预启动 `ConsumerNum` 个消费者。调高 `Num` 会直接增加 Gateway 侧的并发推送通道数，适合大群场景下横向切分连接负载。实现位于 `service/gateway/bucket.go`。 
- **扩容原理**：`TowerManager.GetBucket` 依据连接自增的 `connId` 对 Bucket 数量取模，从而将同一实例的 WebSocket 连接均匀分布到不同 Bucket。每条消息先进入中心通道 `centralChan`，再被广播到全部 Bucket 的 `BuffChan`。增大 Bucket 数量意味着在 fan-out 阶段存在更多 goroutine 并行遍历 `topic -> connection` 映射，单个 Bucket 需要处理的订阅列表缩短，从而降低 5k+ 订阅者聚集时的遍历时间和写 socket 的尾延迟。 
- **配套调优建议**：
  - 随着 Bucket 数量增加，应同步放大 `CentralChanCount` 和每个 Bucket 的 `BuffChanCount`，避免高并发下消息在中心或分桶队列堆积。
  - 若单 Bucket 的 `consumer` goroutine 仍成为瓶颈，可提高 `ConsumerNum`（默认 32）让每个 Bucket 拥有更多并发写出线程；32 这一初始值大致匹配 8 核 CPU。针对 5k～10k 人的大群，可按“`CPU 核心数 * 2`”的上限逐步压测到 48 或 64，并在监控中确认 `consumer` Goroutine 的平均推送耗时与系统负载保持在可控范围。
  - 对于超大 Topic，可结合 Bucket 扩容与业务层的 Topic 拆分（例如基于用户 hash 拆成多个子 Topic），进一步减少单 Bucket 内的订阅数。

#### 大群场景的瓶颈评估（5k～10k 订阅者）
- **Gateway 内部 Fan-out 并发**：单个 Gateway 推送链路核心依赖 `TowerManager.centralChan` 将消息复制到所有 Bucket，随后每个 Bucket 顺序遍历本地 `topic -> connection` 列表并调用连接的 `Send`。在 5k+ 连接聚集于同一 Topic 时，热点 Bucket 的顺序遍历与 `sendLoop` 单 goroutine 写回会放大尾延迟，可通过增加 Bucket 数量、开启 `sendLoop` 批量写入或在业务层拆分 Topic 缓解。实现位置：`service/gateway/tower_manager.go`、`service/gateway/tower_send.go`。
- **Manager → Gateway 广播**：Manager 在 `topic_manage_service.go` 中逐个 TCP 连接写入封包，若该 Topic 订阅扩散到多台 Gateway，则单个 `Publish` 需要串行写多个连接。需要关注 Manager 进程的 CPU 与网络带宽，可通过多进程横向扩容、分片 Topic 或者为热点 Topic 建立专门的推送协程池提升吞吐。
- **网络与系统资源**：5k～10k 订阅者同时在线时，单条消息的出站数据量可达数十 MB，受限于 Gateway 网卡、内核 `sendfile`/缓冲区配置以及云主机带宽上限。需要提前在操作系统层调整 `ulimit`、TCP 缓冲区和心跳超时，避免网络阻塞导致 Manager 误判心跳超时。
- **状态同步压力**：若引入 Redis 共享索引，热点 Topic 会频繁执行 `HINCRBY`/`SMEMBERS` 等操作，Redis 成为新的热点资源。可通过局部缓存、管道化批量写、分桶 Topic Key 及只在订阅变更时同步等手段减轻压力。

### Socket 协议层
Manager 与 Gateway 之间的 TCP 通道使用自定义协议：帧以固定头 `FireHeader` 开始，包含长度字段、指令类型、消息 ID、消息来源、Topic 以及消息体。`socket/protocol/packet.go` 中的 `Enpack` 与 `Depack` 函数分别负责组包与拆包，确保跨进程通信时的完整性与粘包处理。

## 进程间协作流程
1. **Gateway 启动**：`service/gateway/gateway.go` 的 `Init` 函数读取配置、创建 Bucket，并建立到 Manager 的 gRPC 与 TCP 连接。
2. **连接接入**：业务代码通过 `BuildTower` 包装 WebSocket 连接；`Run` 方法启动后即进入读写循环，并可在连接回调中完成鉴权或初始化。
3. **订阅登记**：客户端发送订阅消息时，Gateway 先在本地 Bucket 建立 Topic -> Connection 映射，再通过 gRPC 将订阅同步到 Manager，确保后续推送可路由到该 Gateway。
4. **消息推送**：业务通过 `Publish` 将消息写入 Manager 的 TCP 通道；Manager 解包后，按 Topic 将消息分发至已登记的 Gateway。Gateway 接收后再由 `TowerManager` 将消息广播到所属 Bucket，最终写回各个 WebSocket 连接。

## 部署拓扑
- **单节点调试**：可将 Manager 与 Gateway 同机部署，适用于开发或低并发场景。
- **多实例扩缩容**：生产环境通常会部署多个 Gateway 实例，并为每个实例配置唯一的 `ClusterId`。所有 Gateway 必须连接同一个 Manager，以便集中维护订阅关系。
- **外部依赖**：Firetower 不依赖 Redis 等外部队列，消息路由全部由自建的 Manager 服务负责。

## 参考图示
可复用仓库 README 中的架构图 `firetower_process.png`，展示客户端、Gateway 与 Manager 之间的调用关系与消息流向。
