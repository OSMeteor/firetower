# Firetower 系统扩展方案与架构演进 (Scalability Guide)

本文档详细阐述了 Firetower 系统如何从**单机 Demo** 演进到 **亿级并发 (Uber-Scale)** 的全链路扩展方案。

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

### 2. 阶段二：哈希分片集群 (Sharding Cluster) -> *推荐生产环境起步配置*
*   **适用场景**: 5万 ~ 100万 在线用户。
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

### 4. 阶段四：超级热点分桶 (Bucketing for Hot-Spots)
*   **适用场景**: 单个 Topic（如春晚红包、顶流直播）在线人数 > 200万。
*   **痛点解决**: 某个 Topic 过于火爆，导致其归属的那台 TM 网卡被打爆。即使有 Hash 分片，单点压力依然过大。
*   **架构改造 (业务层介入)**:
    *   **Topic 裂变 (Sharding)**: 将逻辑上的大 Topic `Live_Super` 拆分为 `Live_Super_0` ... `Live_Super_99`。
    *   **用户侧 (Gateway)**: 用户订阅时，根据 `Hash(UID) % 100` 自动分流到不同的子 Topic。
    *   **发布侧 (Admin)**: 后台发消息时，循环向所有子 Topic 广播同一条消息 (Fan-out)。
*   **效果**: 千万人同一个房间聊天，流量被均匀打散到几十台 TM 上。

---

## 组件通信与职责 (Component Roles)

| 组件 | 角色 | 存储数据 | 关键职责 | 扩容方式 |
| :--- | :--- | :--- | :--- | :--- |
| **Gateway** | **接入层** (数据面) | `Connection Map`: 本机所有 WebSocket 连接<br>`TM List`: 下游 TM 集群列表 | 维持海量长连接、协议转换、**Hash 路由** | **无限水平扩展**<br>(加机器即可) |
| **TopicManager** | **逻辑层** (控制面) | `Subscription Map`: 全局 Topic -> Gateway IP 映射表 | 维护订阅关系、消息分发路由 | **Hash 分片扩容**<br>(需 Gateway 配合路由) |
| **Etcd** | **注册中心** | `MetaData`: TM 节点列表及状态 | 服务发现、故障检测 | 集群化部署 |

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

## 开发者调用指南

*   **我要给用户发消息 (Push)**: 
    *   请连接 **Gateway** (WebSocket/TCP)。
    *   Gateway 会自动帮你路由到正确的 TM，性能最高。
*   **我要踢人/查状态 (Control)**: 
    *   请连接 **TM** (gRPC :6667)。
    *   这是修改全局状态的唯一入口。

---

## 总结

Firetower 的架构设计摒弃了对 Redis Pub/Sub 的重度依赖，通过 **Client-side Sharding (Gateway端路由)** 实现了**纯 Go 原生**的无限扩展能力。

*   **小规模**: 单机跑，也是高性能。
*   **大规模**: 加机器，配置 Hash，性能线性增长。
*   **超大规模**: 上 Etcd，上分桶策略，支撑亿级并发。
