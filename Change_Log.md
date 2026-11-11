# 更新日志 (Change Log)

## [未发布] 文档

- 新增 `docs/deploy.md` 运维部署手册（单机、多 Gateway、多 Manager、大房间分片四个场景）。README、architecture、network-communication、scale-plan 按 v0.6 的实际行为更新，加了整体架构图。
- 注明依赖要求：`google.golang.org/grpc` 需要 v1.25 及以上。

## [v0.6.0] - 2026-11-11

### 进程不崩 (Crash safety)
- 用户回调（OnConnect、读、订阅、退订、离线）和所有日志回调里的 panic 都会被接住，只关闭出问题的那条连接，不影响主进程。
- 客户端不能订阅或发布内部保留的 topic（握手、纪元、含分组分隔符的名字）。
- 修复 `Depack` 对超过 4KB 的帧截断的问题。

### 内存保护 (Memory guard)
- Gateway 和 Topic Manager 默认开启：配置 `[memory] limitMB` 时以配置为准，不配置则自动取 GOMEMLIMIT、cgroup 和物理内存中的最小值，`disable = true` 可关闭。
- 按存活堆判断：70% 拒绝新连接，85% 清理积压连接，95% 加大清理；持续有心跳或消息的连接不会被清理。不强制 GC、不暂停业务流程。

### 故障恢复 (Recovery)
- 以 Gateway 的订阅统计为准，向 Manager 分批重放；注册幂等；Manager 慢或暂时不可用时订阅保留在本地并持续重试重放。
- Manager 重启后的迟到握手、旧地址 key 的清理。
- 多 Manager（`[[manager]]`，HRW 选发布目标）和 Topic 分片按房间名展开。

### 测试 (Tests)
- 新增 `cmd/stagecheck`：用真实进程测试阶段 2/3/4、网络故障、负载、内存保护、配置和对账。
- 新增 gateway/manager/socket 的守护测试和模糊测试。

## [未发布]

### 稳定性与核心修复 (Stability & Critical Fixes)
- **协议解析重构 (Protocol Parsing)**: 重写了 `socket/protocol.go` 中的 `Depack` 函数。原实现存在 O(N²) 复杂度及索引越界风险。新实现为 O(N) 且对粘包/半包处理更健壮。
- **并发安全 (Concurrency Safety)**: 在 `socket/socket.go` 的 `TcpClient` 中引入了 `sync.Mutex`，彻底修复了连接关闭与重连时的竞态条件 (Race Condition)。
- **崩溃恢复机制 (Panic Recovery)**: 在所有关键后台 Goroutine (`sendLoop`, `readDispose`, `bucket.consumer`, `manager_client`) 中添加了 `defer recover` 保护。**即便单个连接或逻辑处理发生 Panic，主程序不仅不会崩溃，还能安全关闭异常连接并维持服务运行。**
- **空指针防御 (Nil Pointer Protection)**: 在 `service/gateway/tower.go` 中增加了防御性检查，确保从对象池获取的 `FireInfo` 及其成员永不为 nil。
- **防止队头阻塞 (Head-of-Line Blocking)**: 这是一个 **P0** 级修复。修改了 `FireTower.Send` 为非阻塞模式 (`select + default`)。如果客户端接收缓冲慢或满，服务端将主动丢弃消息并记录日志，防止拖死整个 Bucket 的广播分发。
- **指数退避重连 (Exponential Backoff)**: 实现了 TCP 和 gRPC 的指数退避策略 (1s -> 30s)，防止后端服务故障时大量 Gateway 发起连接风暴。
- **Topic Manager 竞态条件修复**: 在 `service/manager/manager.go` 中修复了 `SubscribeTopic` 和 `UnSubscribeTopic` 无法安全并发访问订阅列表导致的 Panic 问题。引入了互斥锁 (`Mutex`) 保护共享资源。
- **Topic Manager 守护机制**: 在 `TopicManager` 的所有后台协程 (`handler`, `sendLoop`, `heartbeat`) 中也增加了 `defer recover`，防止单连接处理异常导致整个节点服务崩溃 (P0级风险修复)。
- **构建修复**: 解决了 `socket/socket.go` 和 `service/gateway/tower.go` 中的合并冲突，统一了代码版本。

### API 变更 (API Breaking Changes)
- **SetBeforeSubscribeHandler 签名升级**:
  - **旧**: `func(ctx, topic) bool`
  - **新**: `func(ctx, topic) ([]string, bool)`
  - **目的**: 支持**中间件模式**，允许在订阅前动态修改 Topic 名称（如实现热点分桶）。旧代码需更新返回值。

### 架构重构 (Refactoring)
- **内存管理简化**: 移除了针对小对象 (`FireInfo`, `FireTower`) 的 `sync.Pool`。利用 Go 1.18+ Runtime 优秀的 GC 能力，直接使用堆/栈分配。这消除了“Use-after-free”和“Double-free”导致的崩溃风险，且在基准测试中性能损耗仅 <6%，极大提升了代码稳定性和安全性。

### 测试与验证 (Testing)
- **单元测试**:
  - 新增 `socket/protocol_test.go` 和 `socket/protocol_more_test.go`，覆盖协议解析边缘情况。
  - 新增 `service/gateway/bucket_test.go`，验证广播性能和非阻塞特性。
  - 验证了 Panic Recovery 机制，确保注入 Panic 后进程依然存活。
- **压力测试 (Stress Testing)**: 开发了 `stress/main_stress.go` (Advanced Stress Client)，模拟企业级场景：
  - **稳定用户**: 长连接并不停发送心跳。
  - **混沌用户**: 频繁连接、断开、重连。
  - **慢消费者**: 模拟网络卡顿或处理慢的客户端，验证服务端的反压保护。
- **性能验证 (Verified Performance)**: 
  - 进行了为期 30 秒的混合压力测试：**110个并发用户** (包含混沌行为) + **2000 msg/s** 的广播风暴。
  - 结果：**0 错误**，**0 崩溃**，客户端总接收消息量超过 **175万条** (单节点吞吐量约 5.8万/秒)。
- **健壮性测试 (Robustness & Jitter)**:
  - 模拟 **网抖动 (Network Jitter)** 场景：45秒内 **4300+ 次连接重连** (Chaos Users) + **50个慢速消费者** (Packet Loss/Lag Simulation)。
  - 结果：服务端成功触发反压保护 (Backpressure)，主动丢弃堆积消息防止 OOM，主进程 **零 Panic**，核心广播服务未受慢速连接拖累，持续吞吐 **360万+** 条消息。

### 生产环境建议 (Production Advice)
1. **监控**: 建议接入 Prometheus 监控连接数、丢包率 (Dropped Messages) 和 Panic 计数。
2. **优雅退出**: 在业务层集成 `Shutdown()`，确保重启时不强制杀进程。
