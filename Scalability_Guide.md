# Firetower 系统扩展方案与架构演进 (Scalability Guide)

本文档详细阐述了 Firetower 系统如何从**单机 Demo** 演进到 **亿级并发 (Uber-Scale)** 的全链路扩展方案。重点包含**Topic分片 (Topic Sharding)** 和 **用户分桶 (User Bucketing)** 两大核心策略的具体实施指南。

---

## 核心设计理念

Firetower 采用 **控制面 (Control Plane)** 与 **数据面 (Data Plane)** 分离的经典分布式架构：

*   **数据面 (Gateway + Socket)**: 负责高频、低延迟的消息收发与推送。
*   **控制面 (TopicManager + gRPC)**: 负责全局状态管理、路由表维护及管理指令下发。

---

## 架构演进路线图

### 1. 阶段一：单机/小规模 (Current Status)
*   **适用场景**: < 50,000 在线用户，企业内部 IM，小型直播间。
*   **架构**:
    *   **1** 个 TopicManager (TM) 节点。
    *   **N** 个 Gateway 节点 (N >= 2)。
    *   **配置**: Gateway 配置文件中硬编码指向唯一的 TM IP。
*   **特性**: 部署简单，成本最低。但存在 TM 单点故障风险。

### 2. 阶段二：哈希分片集群 (Topic Sharding) -> *推荐生产环境起步配置*
*   **适用场景**: 5万 ~ 100万 在线用户，Topic 数量庞大（如百万个不同的聊天群）。
*   **痛点解决**: 单台 TM 的 CPU 和锁竞争成为瓶颈。
*   **架构改造**:
    *   **TM 集群**: 部署 3~5 台 TM。
    *   **Gateway 改造**: 
        *   维护一个 TM 连接池 (Connection Pool)。
        *   **一致性哈希 (Consistent Hashing)**: 订阅/发布时，计算 `Hash(Topic) % TM_Count`，将不同 Topic 的流量分流到不同的 TM 节点。
*   **效果**: 
    *   **线性扩展**: 增加 TM 机器即可线性提升系统承载能力。
    *   **故障隔离**: 一台 TM 挂掉，只影响 1/N 的 Topic，不会全网瘫痪。

### 3. 阶段三：服务发现与自动伸缩 (Service Discovery)
*   **适用场景**: > 100万 在线用户，需要频繁扩缩容。
*   **痛点解决**: 每次扩容 TM 都需要修改所有 Gateway 配置并重启。
*   **架构改造**:
    *   引入 **Etcd / Consul / Zookeeper** 作为注册中心。
    *   **TM**: 启动时自动注册 IP 到 Etcd。
    *   **Gateway**: 启动时 Watch Etcd，动态更新本地 TM 列表和 Hash 环，实现**无感扩容**。

### 4. 阶段四：超级热点分桶 (User Bucketing)
*   **适用场景**: 单个 Topic（如春晚红包、顶流直播）在线人数 > 200万。
*   **痛点解决**: 某个 Topic 过于火爆，导致其归属的那台 TM 网卡被打爆。即使有 Hash 分片，单点压力依然过大。
*   **架构改造 (业务层介入)**:
    *   **Topic 裂变 (Sharding)**: 将逻辑上的大 Topic `Live_Super` 拆分为 `Live_Super_0` ... `Live_Super_99`。
    *   **用户侧 (Gateway)**: 用户订阅时，根据 `Hash(UID) % 100` 自动分流到不同的子 Topic。
    *   **发布侧 (Admin)**: 后台发消息时，循环向所有子 Topic 广播同一条消息 (Fan-out)。
*   **效果**: 千万人同一个房间聊天，流量被均匀打散到几十台 TM 上。

---

## 代码落地指南 (Implementation Guide)

本节指导开发者如何修改现有的 Firetower 代码以实现上述扩展。

### 1. 实现 Topic 分片 (Sharding)

**目标**: 让 Gateway 根据 Topic 名称自动选择对应的 TopicManager。

**修改文件**: `service/gateway/tower.go`

1.  **修改数据结构**:
    *   将 `FireTower` 结构体中的 `topicManage` 和 `topicManageGrpc` 字段，从单个 Client 修改为 `Map` 或 `HashRing`。
    ```go
    type FireTower struct {
        // ...
        // topicManage     *socket.TcpClient  // OLD
        // topicManageGrpc pb.TopicServiceClient // OLD
        
        tmRing    *一致性哈希环           // NEW: 用于计算 Hash
        tmClients map[string]*socket.TcpClient // NEW: IP -> TcpClient 映射
        // ...
    }
    ```

2.  **修改 `bindTopic` / `unbindTopic` 方法**:
    *   在订阅前，计算 Topic 的哈希值，找到对应的 TM 节点。
    ```go
    func (t *FireTower) bindTopic(topics []string) {
        // Group topics by TM node to reduce network calls
        topicsByNode := make(map[string][]string)
        for _, topic := range topics {
            nodeIP := t.tmRing.GetNode(topic)
            topicsByNode[nodeIP] = append(topicsByNode[nodeIP], topic)
        }
        
        // Batch subscribe to each node
        for ip, subTopics := range topicsByNode {
            client := t.tmClients[ip]
            client.Subscribe(subTopics)
            // ... grpc call ...
        }
    }
    ```

### 2. 实现热点分桶 (Bucketing)

**目标**: 将超大 Topic 拆分为多个小 Topic。由于我们已经实现了 Middleware 机制，**这一步可以在业务层完成，无需修改 Firetower 核心代码！**

**实现位置**: 业务层初始化 Gateway 时 (如 `main.go`)。

**代码示例**:
利用 `SetBeforeSubscribeHandler` 钩子函数动态重写 Topic 名称。

```go
tower := gateway.BuildTower(wsConn, nil)

// 设置订阅前钩子
tower.SetBeforeSubscribeHandler(func(ctx *gateway.FireLife, topics []string) bool {
    var newTopics []string
    for _, topic := range topics {
        // 假设 "live_stream" 是超级热点
        if topic == "live_stream" {
            // 根据用户ID计算 Hash 桶 (0-99)
            bucketID := hash(ctx.UserId) % 100
            // 订阅到具体的子桶: live_stream_42
            newTopics = append(newTopics, fmt.Sprintf("%s_%d", topic, bucketID))
        } else {
            newTopics = append(newTopics, topic)
        }
    }
    // 修改待订阅列表 (注意：这里需要 tower 源码支持修改入参，或者返回新的列表)
    // 建议修改 SetBeforeSubscribeHandler 签名使其能返回 []string
    return true
})
```
*注：发布端(Producer)也需要做相应改造，向所有 Bucket (`live_stream_0` 到 `live_stream_99`) 群发消息。*

---

## 协议端口说明

系统设计了两个端口，对应两种不同的业务形态：

1.  **Port 6666 (TCP / Socket)** —— **数据动脉**
    *   **协议**: 自定义二进制 TCP 协议。
    *   **特点**: 全双工、长连接、极低 Overhead。
    *   **用途**: **Gateway <-> TM 的高频消息通信**。包括用户上下线注册、消息推送。这是系统的流量主力。
    
2.  **Port 6667 (gRPC)** —— **管理控制台**
    *   **协议**: gRPC (HTTP/2)。
    *   **特点**: 强类型接口、Request-Response 模式。
    *   **用途**: **后台管理系统调用**。
        *   **踢人**: `Unsubscribe(User)`
        *   **监控**: `GetTopicStatus(Topic)`
        *   **健康检查**: `Ping()`

---

## 总结

Firetower 的架构设计摒弃了对 Redis Pub/Sub 的重度依赖，通过 **Client-side Sharding (Gateway端路由)** 实现了**纯 Go 原生**的无限扩展能力。

*   **小规模**: 单机跑，也是高性能。
*   **大规模**: 加机器，配置 Hash，性能线性增长。
*   **超大规模**: 上 Etcd，上分桶策略，支撑亿级并发。
