# Changelog

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

## [Unreleased]
### 修复 / Fixed
- **避免读协程阻塞**：`FireTower.read` 现在同时监听 `readIn` 与 `closeChan`，在关闭流程开始时立即返回 `ErrorClose`，彻底解决 WebSocket 断开后读循环长时间挂起的问题。
- **完善关闭路径清理**：在塔关闭时会先记录并回收仍在处理的 `FireInfo`，再执行 `Close`，从而消除生产环境里出现的 goroutine 泄漏风险。
- **保持离线可构建性**：通过在 `go.mod` 中替换所需的第三方依赖为本地 stub，实现完全离线的编译与测试体验。
- **增强发送容错能力**：`FireTower.Send` 与 `sendLoop` 新增超时与关闭检测、心跳失败回收以及写入截止时间设置，能够在网络异常时快速返回错误并收敛资源，避免 `sendLoop` 再次触发崩溃或阻塞。
