# Firetower 网络通讯流程

本文从客户端接入、订阅管理到消息推送的角度，描述一次消息在 Firetower 内部的流转路径。扩容阶段和完整时序见 [scale-plan.md](scale-plan.md)。实现都在 `service/gateway/tower.go`、`service/gateway/bucket.go`、`service/gateway/manager_client.go`、`service/manager/manager.go`。

## 1. 客户端连接与读写循环
1. **WebSocket 升级**：业务层使用 `gateway.BuildTower` 包装客户端 WebSocket 连接，并调用 `Run` 启动连接生命周期。
2. **读循环 (`readLoop`)**：`service/gateway/tower.go` 的 `readLoop` 读取 WebSocket 帧，反序列化为 `TopicMessage`，写入 `readIn`。3 秒内写不进队列会触发 `readTimeoutHandler`。
3. **写循环 (`sendLoop`)**：同文件的 `sendLoop` 从 `sendOut` 取出消息并写回 WebSocket，按 `heartbeat` 发送 Ping。`ToSelf` 把同样的正文放进 `sendOut`，仍由这一条循环写出。客户端收到的字节不变。
4. **业务回调 (`readDispose`)**：同文件的 `readDispose` 按 `Type` 分发：
   - `subscribe`：执行可选的 `beforeSubscribeHandler`，随后调用 `bindTopic`；成功后触发 `subscribeHandler`。
   - `unSubscribe`：调用 `unbindTopic` 并在成功后触发 `unSubscribeHandler`。
   - 其他类型：交由业务自定义的 `readHandler` 处理，常用于向 Topic 发布消息。

## 2. 订阅关系同步
1. **本地索引**：`bindTopic` 先把连接写入所属 Bucket 的 `topic -> connection`。
2. **Manager 登记**：Gateway 通过 gRPC `SubscribeTopic`（`[grpc].address`，默认 `6667`）上报 Topic 和当前 TCP 的 `LocalAddr()`。退订调用 `UnSubscribeTopic`。
3. **异常处理**：登记 RPC 依次用 500ms、1s、2s 的超时重试。超时或 Manager 暂时不可用时，订阅留在本机，并安排一次后台重放补登记；Manager 明确拒绝时才回滚本地订阅并返回错误。两种情况都不主动关闭这条 WebSocket。登记带绝对人数和版本号，超时后迟到的写入不会让计数多算。
4. **保留 Topic**：`_firetower_handshake`、`_firetower_epoch` 和带分片分隔符的名字只供框架内部使用，客户端订阅或发布这些名字会被拒绝。

## 3. 消息发布与分发
1. **业务发布**：业务层在 `readHandler` 中调用 `Publish`，由 Gateway 通过 TCP（默认 `6666` 端口）发送给 Manager。只有一台 Manager 时就是它；多台时按 Topic 做最高随机权重哈希选一台。分片的房间对每一片各发一帧，正文不变。发送内容包含消息 ID、来源、Topic 以及数据正文。
2. **Manager 广播**：Gateway 的发布走 TCP，不走 gRPC。Manager 在 `connectBucket.sendLoop` 里按 Topic 找到 Gateway 地址，把同一帧写回这些已经建立的连接。gRPC `Publish` 是另一条入口，写的时候持有同一把锁。
3. **Gateway 推送**：`OnPush` 把消息送入 `centralChan`。`dispatch` 再尝试送进每一个 Bucket 的 `BuffChan`，队列满则丢掉。Bucket 拷贝出本机订阅者后调用 `Send`，`sendOut` 满同样丢掉。
4. **客户端接收**：WebSocket 连接通过 `sendLoop` 将消息发送给客户端；若连接在过程中断开，Gateway 会触发清理逻辑并通知 Manager 取消订阅。

## 4. 服务端主动退订
- Manager 可以通过协议指令下发 `OfflineTopicByUserId`、`OfflineTopic` 或 `OfflineUser`，由 Bucket 消费线程执行对应的退订与下线流程，实现运营或风控场景下的强制退订。

## 5. 心跳与故障恢复
- **Gateway 心跳**：`sendLoop` 按配置 `heartbeat` 发送 WebSocket Ping。读超时是心跳间隔的 2 倍，超时后关闭连接并退订。收到 Pong 或客户端任何消息都会刷新这条连接的活跃时间，内存保护按它判断连接是否还活着。
- **Manager 心跳**：TCP 服务端在 `connectBucket.heartbeat` 中每 10 秒向 Gateway 写入心跳包；写入失败即关闭连接并移除订阅关系，确保拓扑内的僵尸连接被及时回收。该 TCP 通道与 `topicServiceAddr` 配置的 `6666` 端口对应。
- **Gateway 发现 Manager 故障**：Gateway 读 Manager 的 TCP 有 30 秒读超时（3 个心跳），超时或断开后带抖动重连（200ms 起倍增，封顶 30 秒）。
- **重连后的恢复**：连上后先握手，再把本机订阅分批（每批 256 个 Topic）重放给 Manager；重放失败会一直重试，直到成功；期间连接断开的话，重连后会重新走一遍握手和重放。每 3 分钟还会全量对账一次。客户端不用重新订阅。

通过以上协作流程，Firetower 在无需外部消息队列的情况下完成了从客户端消息生产、订阅登记到多实例广播的完整链路。
