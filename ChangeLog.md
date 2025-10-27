# Changelog

## [Unreleased]
### 修复 / Fixed
- **避免读协程阻塞**：`FireTower.read` 现在同时监听 `readIn` 与 `closeChan`，在关闭流程开始时立即返回 `ErrorClose`，彻底解决 WebSocket 断开后读循环长时间挂起的问题。
- **完善关闭路径清理**：在塔关闭时会先记录并回收仍在处理的 `FireInfo`，再执行 `Close`，从而消除生产环境里出现的 goroutine 泄漏风险。
- **保持离线可构建性**：通过在 `go.mod` 中替换所需的第三方依赖为本地 stub，实现完全离线的编译与测试体验。
