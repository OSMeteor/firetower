<p align="center"><a href="http://chat.ojbk.io" target="_blank" rel="noopener noreferrer"><img width="200" src="http://img.holdno.com/github/holdno/firetowerlogo.png" alt="firetower logo"></a></p>

<p align="center">
  <a href="https://github.com/OSMeteor/beacontower/archive/master.zip"><img src="https://img.shields.io/badge/download-fast-brightgreen.svg" alt="Downloads"></a>
  <a href="https://goreportcard.com/report/github.com/OSMeteor/firetower"><img tag="github.com/OSMeteor/firetower" src="https://goreportcard.com/badge/github.com/OSMeteor/firetower"></a>
  <img src="https://img.shields.io/badge/build-passing-brightgreen.svg" alt="Build Status">
  <img src="https://img.shields.io/badge/package%20utilities-go modules-blue.svg" alt="Package Utilities">
  <img src="https://img.shields.io/badge/golang-1.11.0-%23ff69b4.svg" alt="Version">
  <img src="https://img.shields.io/badge/license-MIT-brightgreen.svg" alt="license">
</p>
<h1 align="center">Firetower</h2>
firetower是一个用golang开发的分布式推送(IM)服务  

完全基于websocket封装，围绕topic进行sub/pub    
自身实现订阅管理服务，无需依赖redis  
聊天室demo体验地址: http://chat.ojbk.io  
### 可用版本
go get github.com/OSMeteor/firetower@v0.5.1  
### 构成

基本服务由两点构成  
- topic管理服务  
> 详见示例 example/topicService  

该服务主要作为集群环境下唯一的topic管理节点  
firetower一定要依赖这个管理节点才能正常工作  
大型项目可以将该服务单独部署在一台独立的服务器上，小项目可以同连接层服务一起部署在一台机器上  
- 连接层服务(websocket服务)  
> 详见示例 example/websocketService  

websocket服务是用户基于firetower自定义开发的业务逻辑  
可以通过firetower提供的回调方法来实现自己的业务逻辑  
（web client 在 example/web 下)  
### 架构图  
![beacontower](http://img.holdno.com/github/holdno/firetower_process.png)  
### 接入姿势  
``` golang 
package main

import (
    "fmt"
    "github.com/gorilla/websocket"
    "github.com/OSMeteor/firetower/gateway"
    "github.com/holdno/snowFlakeByGo" // 这是一个分布式全局唯一id生成器
    "net/http"
    "strconv"
)

var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool {
        return true
    },
} 

var GlobalIdWorker *snowFlakeByGo.Worker

func main() {
    GlobalIdWorker, _ = snowFlakeByGo.NewWorker(1)
    // 如果是集群环境  一定一定要给每个服务设置唯一的id
    // 取值范围 1-1024
    gateway.ClusterId = 1
    http.HandleFunc("/ws", Websocket)
    fmt.Println("websocket service start: 0.0.0.0:9999")
    http.ListenAndServe("0.0.0.0:9999", nil)
}

func Websocket(w http.ResponseWriter, r *http.Request) {
    // 做用户身份验证
    ...
    // 验证成功才升级连接
    ws, _ := upgrader.Upgrade(w, r, nil)
    // 生成一个全局唯一的clientid 正常业务下这个clientid应该由前端传入
    id := GlobalIdWorker.GetId()
    tower := gateway.BuildTower(ws, strconv.FormatInt(id, 10)) // 生成一个烽火台
    tower.Run()
}
```
### 目前支持的回调方法
- ReadHandler 收到客户端发送的消息时触发
``` golang
tower := gateway.BuildTower(ws, strconv.FormatInt(id, 10)) // 创建beacontower实例
tower.SetReadHandler(func(fire *gateway.FireInfo) bool { // 绑定ReadHandler回调方法
    // message.Data 为客户端传来的信息
    // message.Topic 为消息传递的topic
    // 用户可在此做发送验证
    // 判断发送方是否有权限向到达方发送内容
    // 通过 Publish 方法将内容推送到所有订阅 message.Topic 的连接
    tower.Publish(message)
    return true
})
```

- ReadTimeoutHandler 客户端websocket请求超时处理(生产速度高于消费速度)
``` golang 
tower.SetReadTimeoutHandler(func(fire *gateway.FireInfo) {
    fmt.Println("read timeout:", fire.Message.Type, fire.Message.Topic, fire.Message.Data)
})
```

- BeforeSubscribeHandler 客户端订阅某些topic时触发(这个时候topic还没有订阅，是before subscribe)
``` golang
tower.SetBeforeSubscribeHandler(func(context *gateway.FireLife, topic []string) bool {
    // 这里用来判断当前用户是否允许订阅该topic
    return true
})
```

- SubscribeHandler 客户端完成某些topic的订阅时触发(topic已经被topicService收录并管理)
``` golang
tower.SetSubscribeHandler(func(context *gateway.FireLife, topic []string) bool {
    // 我们给出的聊天室示例是需要用到这个回调方法
    // 当某个聊天室(topic)有新的订阅者，则需要通知其他已经在聊天室内的成员当前在线人数+1
    for _, v := range topic {
        num := tower.GetConnectNum(v)
        // 继承订阅消息的context
        var pushmsg = gateway.NewFireInfo(tower, context)
        pushmsg.Message.Topic = v
        pushmsg.Message.Data = []byte(fmt.Sprintf("{\"type\":\"onSubscribe\",\"data\":%d}", num))
        tower.Publish(pushmsg)
    }
    return true
})
```

- UnSubscribeHandler 客户端取消订阅某些topic完成时触发 (这个回调方法没有设置before方法，目前没有想到什么场景会使用到before unsubscribe，如果有请issue联系)
``` golang
tower.SetUnSubscribeHandler(func(context *gateway.FireLife, topic []string) bool {
    for _, v := range topic {
        num := tower.GetConnectNum(v)
        // 继承订阅消息的context
        var pushmsg = gateway.NewFireInfo(tower, context)
        pushmsg.Message.Topic = v
        pushmsg.Message.Data = []byte(fmt.Sprintf("{\"type\":\"onUnsubscribe\",\"data\":%d}", num))
        tower.Publish(pushmsg)
    }
    return true
})
```
注意：当客户端断开websocket连接时firetower会将其在线时订阅的所有topic进行退订 会触发UnSubscirbeHandler  

## 系统架构与无限扩展指南 (System Architecture & Scalability Guide)

Firetower 采用 **Gateway (接入层)** + **TopicManager (逻辑控制层)** 的分离架构设计。这种设计天生具备良好的扩展性。本指南将阐述如何从单机 Docker 部署演进到支撑百万级在线用户的分布式集群。

### 1. 核心组件
*   **Gateway (Websocket Service)**:
    *   **职责**: 维护海量 WebSocket 长连接，处理协议封包/解包，执行消息广播。
    *   **特性**: 仅处理连接逻辑，几乎无状态（订阅关系同步给 TM），**可无限水平扩展**。针对慢连接实现了非阻塞广播和自动丢包保护。
*   **TopicManager (Topic Service)**:
    *   **职责**: 管理 Topic -> Gateway 节点的映射关系，接收 Publish 请求并分发给持有该 Topic 订阅者的所有 Gateway。
    *   **特性**: 目前为单点状态节点 (Stateful)，是扩展的瓶颈所在。

### 2. 演进路线图

#### 阶段一：单机/小规模集群 (Current)
*   **适用场景**: < 50,000 在线用户，业务量适中。
*   **部署**:
    *   1个 TopicManager 实例。
    *   N个 Gateway 实例 (N >= 2)，通过 Nginx/SLB 做 4层或7层负载均衡。
    *   Gateway 启动时通过配置指向唯一的 TopicManager IP。

#### 阶段二：TopicManager 分片 (Sharding)
*   **适用场景**: < 500,000 在线用户，Topic 数量巨大。
*   **改造方案**:
    *   部署 M 个 TopicManager 节点。
    *   **Gateway 改造**: 在连接 TM 时，不再连接单一节点，而是连接 TM 集群。
    *   **路由算法**: 采用一致性哈希 (Consistent Hashing) 或 `Hash(Topic) % M` 算法。
        *   当订阅 `Topic_A` 时，Gateway 计算 Hash 路由到 `TM_Node_1` 进行注册。
        *   当发布 `Topic_A` 时，Gateway 同样路由到 `TM_Node_1` 进行发布。
    *   **效果**: 将订阅关系管理的内存压力和匹配计算的 CPU 压力分散到集群中。

#### 阶段三：无状态化与中间件集成 (Stateless & Middleware)
*   **适用场景**: > 1,000,000 在线用户 (百万级并发)。
*   **核心痛点**: 此时自研的 TopicManager 可能成为维护负担，且有状态服务的扩容迁移复杂。
*   **改造方案**:
    *   **移除 TopicManager**：完全废弃自研的 TopicManager 服务。
    *   **引入 Redis Pub/Sub (或 Kafka/NATS)**：使用成熟的消息中间件作为 Topic 路由中心。
    *   **Gateway 行为**:
        *   用户订阅 `Topic_A` -> Gateway 直接向 Redis 订阅 `Channel_A`。
        *   收到 Redis `Channel_A` 消息 -> Gateway 广播给本机所有订阅了 `Topic_A` 的 WebSocket 连接。
    *   **优势**: 彻底利用云厂商提供的 Redis 集群能力，Gateway 变为完全无状态的纯连接层，实现真正的**无限水平扩展**。

#### 阶段四：千万级超大规模 (Hierarchical Routing)
*   **适用场景**: 各国头部 APP (如 WhatsApp, 微信)。
*   **改造方案**:
    *   **Bucket 预分片**: 引入 "Slot" 概念 (如 16384 个 Slot)。
    *   **二级路由**: 建立 `Slot -> Gateway_IP_List` 的全局映射表 (存储在 Etcd/ZooKeeper)。
    *   **边缘计算**: 消息先推送到 Slot 对应的“分发层”，再由分发层并行推送到具体的 Gateway 节点。

### 3. 稳定性保障 (已实装)
为支撑上述扩展，本项目在代码层面已实装以下企业级特性：
*   **Panic Recovery**: 关键协程全覆盖，单点故障不扩散。
*   **Non-blocking Send**: 防止慢消费者（弱网用户）拖死服务子系统。
*   **Exponential Backoff**: 指数退避重连，防止服务重启时的流量雪崩。
*   **Zero-Copy Logic**: 协议层优化，支撑高吞吐。
 
## TODO
- 运行时web看板  
- 提供推送相关http及grpc接口

## License  
[MIT](https://opensource.org/licenses/MIT)

