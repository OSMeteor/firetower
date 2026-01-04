# 更新日志 (Change Log)

## [未发布]

### 稳定性与核心修复 (Stability & Critical Fixes)
- **协议解析重构 (Protocol Parsing)**: 重写了 `socket/protocol.go` 中的 `Depack` 函数。原实现存在 O(N²) 复杂度及索引越界风险。新实现为 O(N) 且对粘包/半包处理更健壮。
- **并发安全 (Concurrency Safety)**: 在 `socket/socket.go` 的 `TcpClient` 中引入了 `sync.Mutex`，彻底修复了连接关闭与重连时的竞态条件 (Race Condition)。
- **崩溃恢复机制 (Panic Recovery)**: 在所有关键后台 Goroutine (`sendLoop`, `readDispose`, `bucket.consumer`, `manager_client`) 中添加了 `defer recover` 保护。**即便单个连接或逻辑处理发生 Panic，主程序不仅不会崩溃，还能安全关闭异常连接并维持服务运行。**
- **空指针防御 (Nil Pointer Protection)**: 在 `service/gateway/tower.go` 中增加了防御性检查，确保从对象池获取的 `FireInfo` 及其成员永不为 nil。
- **防止队头阻塞 (Head-of-Line Blocking)**: 这是一个 **P0** 级修复。修改了 `FireTower.Send` 为非阻塞模式 (`select + default`)。如果客户端接收缓冲慢或满，服务端将主动丢弃消息并记录日志，防止拖死整个 Bucket 的广播分发。
- **指数退避重连 (Exponential Backoff)**: 实现了 TCP 和 gRPC 的指数退避策略 (1s -> 30s)，防止后端服务故障时大量 Gateway 发起连接风暴。
- **Topic Manager 竞态条件修复**: 在 `service/manager/manager.go` 中修复了 `SubscribeTopic` 和 `UnSubscribeTopic` 无法安全并发访问订阅列表导致的 Panic 问题。引入了互斥锁 (`Mutex`) 保护共享资源。
- **构建修复**: 解决了 `socket/socket.go` 和 `service/gateway/tower.go` 中的合并冲突，统一了代码版本。

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
- **性能基准**: `BenchmarkBucketPush` 实测单机广播吞吐量 > 100万 ops/s。

### 生产环境建议 (Production Advice)
1. **监控**: 建议接入 Prometheus 监控连接数、丢包率 (Dropped Messages) 和 Panic 计数。
2. **优雅退出**: 在业务层集成 `Shutdown()`，确保重启时不强制杀进程。
