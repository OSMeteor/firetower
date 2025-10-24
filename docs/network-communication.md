# Firetower 网络通讯流程

本文从客户端接入、订阅管理到消息推送的角度，描述一次消息在 Firetower 内部的流转路径。

## 1. 客户端连接与读写循环
1. **WebSocket 升级**：业务层使用 `gateway.BuildTower` 包装客户端 WebSocket 连接，并调用 `Run` 启动连接生命周期。
2. **读循环 (`readLoop`)**：Gateway 持续读取 WebSocket 帧，将 JSON 反序列化为 `TopicMessage`，并写入内部 `readIn` 队列；若在 3 秒超时时间内写入失败，会触发 `readTimeoutHandler` 并输出超时日志。
3. **写循环 (`sendLoop`)**：Gateway 从 `sendOut` 队列取出 `SendMessage`，按需补全消息类型并写回 WebSocket，同时以定时器发送心跳帧来检测连接存活。
4. **业务回调 (`readDispose`)**：读取线程通过 `read` 拿到消息后，根据 `Type` 进行分发：
   - `subscribe`：执行可选的 `beforeSubscribeHandler`，随后调用 `bindTopic`；成功后触发 `subscribeHandler`。
   - `unSubscribe`：调用 `unbindTopic` 并在成功后触发 `unSubscribeHandler`。
   - 其他类型：交由业务自定义的 `readHandler` 处理，常用于向 Topic 发布消息。

## 2. 订阅关系同步
1. **本地索引**：`bindTopic` 会在连接所属的 `Bucket` 中新增 `topic -> connection` 关系，确保实例内部的推送可以快速定位目标连接。
2. **Manager 登记**：Gateway 通过 gRPC `SubscribeTopic` 接口，将待订阅 Topic 与当前 Gateway 的 TCP 地址（`topicManage.Conn.LocalAddr()`）一起上报给 Manager；退订流程调用 `UnSubscribeTopic`。
3. **异常处理**：若 gRPC 调用失败，则关闭当前连接，避免出现 Manager 与 Gateway 订阅状态不一致的问题。

## 3. 消息发布与分发
1. **业务发布**：业务层在 `readHandler` 中调用 `Publish`，由 Gateway 通过 TCP 客户端 `topicManage.Publish` 将消息发送给 Manager。发送内容包含消息 ID、来源、Topic 以及数据正文。
2. **Manager 广播**：Manager 的 gRPC `Publish` 接口收到请求后，查找 Topic 订阅列表，将消息封包为自定义协议，通过 TCP 连接写回到所有登记的 Gateway。
3. **Gateway 推送**：Gateway 侧 `TowerManager` 的中心通道 (`centralChan`) 接到 Manager 推送的 `SendMessage` 后，会将消息复制到每个 `Bucket`。Bucket 遍历本地订阅表，调用连接的 `Send` 将消息写入 WebSocket。
4. **客户端接收**：WebSocket 连接通过 `sendLoop` 将消息发送给客户端；若连接在过程中断开，Gateway 会触发清理逻辑并通知 Manager 取消订阅。

## 4. 服务端主动退订
- Manager 可以通过协议指令下发 `OfflineTopicByUserId`、`OfflineTopic` 或 `OfflineUser`，由 Bucket 消费线程执行对应的退订与下线流程，实现运营或风控场景下的强制退订。

## 5. 心跳与故障恢复
- **Gateway 心跳**：`sendLoop` 定期发送 `heartbeat` 文本帧；异常时会触发连接关闭并清理资源。
- **Manager 心跳**：TCP 服务端在 `connectBucket.heartbeat` 中每 10 秒向 Gateway 写入心跳包；写入失败即关闭连接并移除订阅关系，确保拓扑内的僵尸连接被及时回收。

通过以上协作流程，Firetower 在无需外部消息队列的情况下完成了从客户端消息生产、订阅登记到多实例广播的完整链路。
