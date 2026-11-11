# Firetower 部署手册（v0.6.x，给运维）

本文只写怎么部署、怎么检查、出了告警怎么办。为什么这样设计见 [scale-plan.md](scale-plan.md)，消息怎么流转见 [architecture.md](architecture.md)。

按规模分四个场景，每个场景都按同样的五步写：**机器规划 → 配置文件 → 启动步骤 → 检查项 → 告警处理**。从场景 A 开始，告警告诉你该升级时再往下走。

| 场景 | 机器 | 什么时候升到下一个场景 |
| :--- | :--- | :--- |
| [A 单机](#场景-a单机) | 1 台：Manager + Gateway 两个进程 | Gateway 的连接数、CPU 或出口接近上限 |
| [B 多 Gateway](#场景-b多-gateway) | 1 台 Manager + N 台 Gateway | Manager 的 CPU 或出口打满，且不是单个房间造成的 |
| [C 多 Manager](#场景-c多-manager) | M 台 Manager + N 台 Gateway | 单个大房间把一台 Manager 打满 |
| [D 大房间分片](#场景-d大房间分片) | 同 C，再给大房间配分片 | — |

同一套程序覆盖四个场景，区别只在配置和机器数量。

---

## 第 0 部分：所有场景都要知道的事

### 0.1 两个进程

| 进程 | 是什么 | 端口 | 重启的影响 |
| :--- | :--- | :--- | :--- |
| **Topic Manager** | 独立程序（`example/topic_service` 编译出来的那个） | `6666` TCP（发布、推送、心跳）<br>`6667` gRPC（订阅登记） | 路由在内存里，重启后几秒内由各 Gateway 自动补回。**客户端不断线** |
| **Gateway** | 业务的接入进程（例如 beego 服务），firetower 嵌在里面 | 业务自己的 WebSocket 端口 | **这台机器上的用户会断开**，客户端要重连 |

- 两个进程之间只走内网。`6666`、`6667` 没有认证，防火墙只放行 Gateway 所在的机器。
- 不依赖 Redis、Kafka、Etcd。
- Manager 和全部 Gateway 用同一个 v0.6.x 版本，不能和 v0.5.x 混跑。
- 接入进程（Gateway）的 go.mod 里 `google.golang.org/grpc` 要 v1.25 及以上。

### 0.2 配置文件只留一份

- 仓库里有三份 `fireTower.toml`，内容不一样。`config/fireTower.toml` 里 `heartbeat = 600`（死连接要 20 分钟才发现），**不要用**。以本文的模板为准。
- Gateway 读的路径由接入进程的 `gateway.DefaultConfigPath` 决定，缺省是工作目录下的 `./fireTower.toml`。**要用绝对路径**，否则在哪个目录启动就读哪一份。
- Manager 读工作目录下的 `./topicmanage.toml`。启动前先 `cd` 到程序目录（systemd 里写 `WorkingDirectory`）。
- Gateway 不会热加载配置。改配置要重启 Gateway，这台机器上的用户会断开重连。

### 0.3 内存保护（默认开启）

- 上限的取法，优先级从高到低：配置 `[memory] limitMB` > 自动取（容器 cgroup 限制、`GOMEMLIMIT`、物理内存三者中最小的）> `disable = true` 关闭。
- **Gateway 嵌在业务进程里，统计的是整个进程的内存，业务缓存也算在内。** 同机有别的大程序、或者业务缓存很大时，必须写 `limitMB`。
- Gateway：上限的 70% 起拒绝新连接，85% 起断开积压严重或不活跃的连接，95% 加大清理。一直有心跳或消息的连接不会被断开。
- Manager：85% 起丢弃发往慢 Gateway 的积压消息，不会断开 Gateway。
- 启动时日志会打一行 `memory guard: limit ...MB from ...`，来源是 `memory.limitMB`、`cgroup`、`GOMEMLIMIT` 或 `physical` 之一，用它确认取到的值对不对。

### 0.4 进程守护

两个进程都要被守护（systemd、supervisor 或容器的重启策略），异常退出自动拉起。启动顺序不强制：Gateway 先起会每隔几秒自动重试连 Manager，但 Manager 连上之前，客户端的订阅会失败。

Manager 的 systemd 示例：

```ini
# /etc/systemd/system/firetower-manager.service
[Unit]
Description=Firetower Topic Manager
After=network-online.target

[Service]
# topicmanage.toml 放在 WorkingDirectory 下
WorkingDirectory=/opt/firetower-manager
ExecStart=/opt/firetower-manager/topic_service
Restart=always
RestartSec=2
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

Manager 跑在 Windows 上时，用 NSSM（第三方的服务包装工具，nssm.cc）注册成服务，崩溃后自动拉起：

```bat
nssm install firetower-manager C:\firetower-manager\topic_service.exe
nssm set firetower-manager AppDirectory C:\firetower-manager
nssm set firetower-manager AppStdout C:\firetower-manager\logs\manager.log
nssm set firetower-manager AppStderr C:\firetower-manager\logs\manager.log
nssm set firetower-manager AppRotateFiles 1
nssm set firetower-manager AppRotateBytes 104857600
nssm start firetower-manager
```

`AppDirectory` 必须是放 `topicmanage.toml` 的目录。不能装 NSSM 时，用「任务计划程序」建一个「计算机启动时」触发的任务，在「设置」里勾上「如果任务失败，重新启动」。

Gateway 按业务进程原来的方式守护。如果是 Linux，文件句柄上限要大于 `maxConnections`，每条 WebSocket 占一个句柄。

### 0.5 日志在哪里

| 进程 | 输出到 | 怎么收 |
| :--- | :--- | :--- |
| Manager | 标准输出和标准错误 | systemd 下 `journalctl -u firetower-manager`；NSSM 下是上面配置的 `manager.log` |
| Gateway（框架部分） | 业务进程的标准错误（`[FireTower] ...` 开头）和标准输出（`[manager client] ...` 开头） | 业务进程的标准输出、标准错误要被收集，**不会自动进 beego 的日志文件**。想写进业务日志，在接入代码里给 `gateway.TowerLogger` 赋一个转发到业务日志的函数 |

- Gateway 缺省日志级别是 `INFO`，每条连接建立、断开都打一行。在线人数多时在接入代码里设 `gateway.LogLevel = "WARN"`，只留告警和错误（这时启动时那行 `memory guard: limit` 也不打，用 `gateway.MemoryGuardStats()` 确认上限）。
- 第 6 节以后各场景「告警处理」里的关键字都在这些输出里，建议在日志平台上按关键字配告警。

### 0.6 容量参考（单机压测结果）

2026-11-11 用 `stagecheck -stage bench` 在三种环境上测过（方法见 [release-test-plan.md](release-test-plan.md)）。Manager、Gateway 和压测客户端都在同一台机器上，压测客户端自己就占 1～5 核，所以下面是**下限**，不是生产机器的上限。

| 项 | Linux 云主机 2 核 8GB | Windows 8 核 16 线程 32GB | 同机 WSL2 16 核 16GB |
| :--- | :--- | :--- | :--- |
| 每条空闲连接的内存（Go 管理内存，`chanLens = 1000`） | 53KB | 67KB | 54KB |
| 其中存活对象 | 33KB | 26KB | 30KB |
| 每条连接的协程数 | 3 | 3 | 3 |
| 1 万条连接建连并订阅完 | 约 1 秒 | 约 2 秒 | 约 1.4 秒 |
| 一台 Gateway、2000 人的房间、每条 1KB：不丢且 p99 ≤ 1 秒的最高发送速率 | 100 条/秒，即每秒 20 万次投递，Gateway 用 1.0 核 | 100 条/秒，每秒 20 万次投递，Gateway 用 5.2 核 | 50 条/秒，每秒 10 万次投递，Gateway 用 3.2 核 |
| 一台 Manager 转发给 8 台 Gateway、每条 1KB | 每秒 16000 帧时 Manager 用 0.10 核 | 0.49 核 | 0.61 核 |

怎么用这些数：

- **内存**：每条连接按 70KB 估。Gateway 进程的内存上限（`limitMB` 或机器内存）至少要 `连接数 × 70KB ÷ 0.7 + 业务自身占用`，因为到 70% 就开始拒绝新连接。例：5 万连接约需 3.5GB ÷ 0.7 = 5GB，再加业务占用。`chanLens` 每减少 100，每条连接约少 1.6KB。
- **投递**：一台 Gateway 每秒的投递次数（每秒消息条数 × 本机订阅人数，各房间加总）按 **10～20 万次** 估。超过时加 Gateway，或降低大房间的消息频率。投递很吃 CPU：在 16 线程的机器上每秒 20 万次用了约 5 核，Gateway 的机器按这个比例留 CPU。
- **Manager**：每秒 16000 帧只用了 0.1～0.6 核，测试里没能把它压满。Manager 一般不会先成为瓶颈，先看它的网卡出口。
- **单条连接发布的上限**：一个 WebSocket 连接每秒最多发约 2000 条，再快会被这条连接的读写卡住。业务后端要大量推送时，用多条连接，或直接调 Manager 的 gRPC `Publish`。
- **延迟**：没到上限时，p99 在 10～50 毫秒。
- 正式选型前，在目标机器上跑一遍 `stagecheck -stage bench`，按实测数字算。

---

## 场景 A：单机

### 机器规划

| 项 | 要求 |
| :--- | :--- |
| 机器 | 1 台，上面跑 Manager、Gateway 两个进程 |
| 适合 | 开发、测试、在线人数不大的生产 |
| 容量 | 按第 0.6 节估：每条连接约 70KB 内存；一台 Gateway 每秒 10～20 万次投递。框架缺省 `maxConnections = 100000`，按内存算出的连接数比它小时，把它调低 |

### 配置文件

Gateway 的 `fireTower.toml`（完整模板，后面的场景只写和它不同的地方）：

```toml
chanLens = 1000              # 每条连接的收发队列长度
heartbeat = 30               # 秒。WebSocket Ping 间隔，60 秒没响应就断开
maxConnections = 100000      # 本机最多连接数，超过的新连接被拒绝

topicServiceAddr = "127.0.0.1:6666"
[grpc]
address = "127.0.0.1:6667"

[bucket]
Num = 4
CentralChanCount = 100000
BuffChanCount = 1000
ConsumerNum = 1              # 大于 1 时同一连接上的消息先后不保证

[warn]                       # 告警水位（百分比）
connectionPercent = 70
queuePercent = 70

[memory]                     # 内存保护。全部不写 = 按机器自动取上限
# limitMB = 4096             # 同机还有别的大程序、或业务缓存大时写上
# disable = true             # 关闭（不建议）
```

Manager 的 `topicmanage.toml`：

```toml
[grpc]
port = 6667

[socket]
port = 6666

[memory]
# limitMB = 2048
# disable = true
```

`topicmanage.toml` 里如果还有 `heartbeat`、`servicetimeout`，可以删掉，代码不读它们。

接入进程代码：只有一台 Gateway 时 `gateway.ClusterId = 1` 写死没问题。

### 启动步骤

1. 启动 Manager：`systemctl start firetower-manager`。
2. 确认端口在监听：`ss -lntp | grep -E ':666[67]'`，应看到 `6666`、`6667` 两行。
3. 启动 Gateway（业务进程）。
4. 看 Gateway 日志里有 `[manager client] connected:`，说明连上了 Manager。
5. 看 Gateway 和 Manager 日志里各有一行 `memory guard: limit ...`。

### 检查项

- [ ] Gateway 读的配置是绝对路径，内容是上面的模板；`heartbeat` 是 30，不是 600。
- [ ] 接入代码满足三条（找开发确认）：`BuildTower` 返回 `nil` 时调用 `ws.Close()` 并返回（见 README 的接入示例）；go.mod 里 `google.golang.org/grpc` 是 v1.25 及以上；在线人数多时设了 `gateway.LogLevel = "WARN"`。
- [ ] Gateway 进程的标准输出、标准错误有人收（第 0.5 节）。
- [ ] Gateway 日志有 `[manager client] connected:`，没有反复出现 `tcp connect ... error`。
- [ ] 两个进程的 `memory guard: limit` 数值合理。Gateway 和业务共用一台机器时，确认已经写了 `limitMB`。
- [ ] 两个进程都在守护下：分别 `kill` 一次，确认能自动拉起。
- [ ] **演练 Manager 重启**：用测试客户端连上并订阅一个房间，重启 Manager，客户端不重连也能在几秒后继续收到新消息。

### 告警处理

| 日志关键字 | 意思 | 怎么处理 |
| :--- | :--- | :--- |
| `connections value=... action=add gateway` | 连接数超过 `maxConnections` 的 70% | 准备升到场景 B |
| `centralChan value=... action=check one topic...` | 本机中心消息队列过 70% | 先看是不是单个大房间；不要盲目调大队列。多个房间普遍高就升到场景 B |
| `memory guard: rejecting new connections` | 内存到了 70%，新连接被拒 | 加内存，或者升到场景 B；查业务缓存是否过大 |
| `memory guard: closed ... connections, dropped ...` | 内存到了 85%，断开了积压或不活跃的连接 | 同上，并查是否有大量慢客户端 |
| `send buffer full, message dropped` | 某个客户端读得太慢，消息被丢 | 个别出现属正常；大量出现查客户端网络或消息量 |
| `memory guard: could not detect the memory limit` | 自动取上限失败，内存保护没开 | 写 `[memory] limitMB` |
| `[manager client] tcp connect ... error` 持续出现 | Gateway 连不上 Manager | 查 Manager 进程和端口 |

---

## 场景 B：多 Gateway

### 机器规划

| 项 | 要求 |
| :--- | :--- |
| Manager | 1 台，**单独一台机器，不和任何 Gateway 同机**。同机时那台 Gateway 的 CPU 和出口会挤占转发 |
| Manager 地址 | 固定内网 IP、内网 DNS 或 VIP，Gateway 配置里写这个地址 |
| Manager 选型 | 看网卡和 CPU，不看内存。每条消息进来一次，出去的次数等于订了这个房间的 Gateway 台数。例：每秒 5000 条、每条 1KB、平均命中 20 台 Gateway，出口约 100MB/s |
| Gateway | N 台，前面放负载均衡（需支持 WebSocket），把用户分到各台。负载均衡的空闲超时要大于 `heartbeat` |
| 网络 | 每台 Gateway 能直连 Manager 的 `6666`、`6667`；中间不能有 NAT |

### 配置文件

每台 Gateway 的 `fireTower.toml` 在场景 A 模板上改地址：

```toml
topicServiceAddr = "10.0.0.10:6666"   # Manager 的内网地址（示例）
[grpc]
address = "10.0.0.10:6667"
```

Manager 的 `topicmanage.toml` 和场景 A 一样。

接入进程代码：**每台 Gateway 的 `gateway.ClusterId` 必须不同**（1～1024）。不能再写死 1，要改成从配置或环境变量读，例如 beego 的 `app.conf` 里每台写不同的值。

### 启动步骤

1. 启动 Manager，确认 `6666`、`6667` 在监听。
2. 在每台 Gateway 上确认能连到 Manager：`nc -zv 10.0.0.10 6666`、`nc -zv 10.0.0.10 6667`（Windows 用 `Test-NetConnection 10.0.0.10 -Port 6666`）。
3. 逐台启动 Gateway，每台都看到 `[manager client] connected:`。
4. 把 Gateway 加进负载均衡。

### 检查项

- [ ] 场景 A 的检查项全部通过。
- [ ] 每台 Gateway 的 `ClusterId` 不同，且是接入代码从配置或环境变量读的，不是写死的。Manager 日志里没有 `cluster ... reported startup ... and ...`。
- [ ] Manager 单独一台机器。
- [ ] 每台 Gateway 到 Manager 两个端口都通。
- [ ] **演练跨机投递**：两个测试客户端分别连到两台不同的 Gateway，订阅同一个房间，一个发、另一个能收到。
- [ ] **演练 Manager 重启**：重启 Manager，各台 Gateway 上的客户端不重连也能在几秒后继续收消息。实测 4 台 Gateway 约 2.4 秒，20 台 Gateway、每台 600 个房间约 3.5～9 秒。
- [ ] **演练单台 Gateway 重启**：这台上的用户断开、重连到其他 Gateway；其他 Gateway 上的用户不受影响。

### 告警处理

场景 A 的告警照常适用（在每台 Gateway 上看），另外关注：

| 日志关键字 | 出现在 | 意思 | 怎么处理 |
| :--- | :--- | :--- | :--- |
| `connections value=... action=add gateway` | Gateway | 这台连接数过 70% | 加一台 Gateway；或检查负载均衡是否分得不均 |
| `gateway outbox value=... action=check that gateway` | Manager | 发往某台 Gateway 的队列积压 | 查那台 Gateway 的网络和负载。只有它慢就不是 Manager 的问题 |
| `memory guard: dropped ... queued frames to gateways` | Manager | Manager 内存紧张，丢了发往慢 Gateway 的积压 | 同上，找出慢的那台 |
| `register requests are waiting` | Manager | 大量订阅同时涌入 | Manager 重启后的恢复期出现属正常；平时持续出现查 Manager 的 CPU |
| `replay to manager ... keeps failing` | Gateway | 订阅一直补不到 Manager | 查 Manager 是否存活、端口是否通 |
| `cluster ... reported startup ... and ...` | Manager | 两台 Gateway 用了同一个 `ClusterId` | 改成不同的值后重启其中一台 |
| Manager 机器 CPU 或出口持续很高 | 机器监控 | Manager 转发打满 | 先看是不是单个房间造成的。多个房间普遍高 → 升到场景 C；单个房间 → 场景 C + D |

---

## 场景 C：多 Manager

### 机器规划

| 项 | 要求 |
| :--- | :--- |
| Manager | M 台，**每台单独一台机器，放在不同的机架或可用区**。两台同机等于仍是单点 |
| 工作方式 | 每台 Manager 都有完整的路由，订阅向每一台登记，发布按房间名哈希只发给其中一台 |
| 故障切换 | 自动。一台 Manager 挂了，发往它的房间自动改发下一台，不用人工切换 |
| 代价 | 订阅登记量和路由内存都乘以 Manager 台数（路由表只有「房间 → Gateway」，内存很小） |
| Gateway | 同场景 B |

### 配置文件

每台 Gateway 的 `fireTower.toml`：**删掉** `topicServiceAddr` 和 `[grpc] address`，改用 `[[manager]]` 列出全部 Manager（写了 `[[manager]]` 时旧的两项不生效）：

```toml
[[manager]]
name = "m1"
tcp = "10.0.0.10:6666"
grpc = "10.0.0.10:6667"

[[manager]]
name = "m2"
tcp = "10.0.0.11:6666"
grpc = "10.0.0.11:6667"
```

每项还可以写 `register`、`publish`，缺省都是 `true`，只在增减 Manager 时用（见下面的步骤）。

**所有 Gateway 的 `[[manager]]` 列表必须完全一致。**

每台 Manager 的 `topicmanage.toml` 同场景 A。

### 启动步骤

**从场景 B 第一次扩成多 Manager，或以后再加一台**：

1. 起新的 Manager（例如 m2），确认端口在监听，每台 Gateway 都能连通。
2. 第一步，只登记不发布：所有 Gateway 的配置里加上 m2，写 `publish = false`，**逐台滚动重启**全部 Gateway。
3. 全部 Gateway 都重启完以后，第二步：删掉 m2 的 `publish = false`，再逐台滚动重启一遍。

**减少一台 Manager**（顺序反过来）：

1. 所有 Gateway 把它改成 `publish = false`，滚动一遍。
2. 从列表里删掉它，再滚动一遍。
3. 停掉这台 Manager。

为什么不能一步到位：滚动期间，已重启的 Gateway 会往新 Manager 发布，但还没重启的 Gateway 没在它上面登记，那些 Gateway 上的用户会收不到消息。

### 检查项

- [ ] 场景 B 的检查项全部通过。
- [ ] 所有 Gateway 的 `[[manager]]` 列表一致（逐台比对配置文件）。
- [ ] 每台 Gateway 日志里对每台 Manager 都有 `[manager client] connected:`。
- [ ] 各台 Manager 在不同的机器、不同的机架或可用区。
- [ ] **演练停掉一台 Manager**：停 m1，所有房间的消息仍能收到（发往 m1 的房间改发 m2），Gateway 日志出现 `publish for ... moved from m1 to ...`。启动 m1 后恢复。

### 告警处理

场景 B 的告警照常适用，另外关注：

| 日志关键字 | 出现在 | 意思 | 怎么处理 |
| :--- | :--- | :--- | :--- |
| `publish for ... moved from ... to ...` | Gateway | 某台 Manager 掉线或恢复，发布换了目标 | 偶尔出现正常；频繁出现说明有 Manager 不稳定，查它的进程和网络 |
| `replay to manager <名字> keeps failing` | Gateway | 订阅一直补不到这台 Manager | 查这台 Manager；如果是计划下线，按「减少一台 Manager」的步骤把它从列表里删掉 |
| 某一台 Manager 的 CPU 或出口明显高于其他台 | 机器监控 | 有个大房间集中在这台 | 升到场景 D，给这个房间配分片 |
| 所有 Manager 的 CPU 或出口都高 | 机器监控 | 总量打满 | 再加一台 Manager |

---

## 场景 D：大房间分片

### 机器规划

- 同场景 C。分片要和多台 Manager 一起用：**只有一台 Manager 时打开分片没有好处**，一次发布反而变成多帧。
- 分片的效果：一个大房间拆成多片，各片的发布落到不同的 Manager 上，不再由一台 Manager 独自承担整个房间。
- 用户收到的消息数不变，Gateway 的出口带宽不会因为分片下降。

### 配置文件

在**所有** Gateway 的 `fireTower.toml` 里加，每个大房间一段：

```toml
[[group]]
topic = "/live/room-1001"    # 房间名，和客户端订阅的完全一样
shards = 8                   # 2～256。片数是 Manager 台数的几倍即可，例如 2 台 Manager 用 4～8

[[group]]
topic = "/live/room-1002"
shards = 8
```

- **所有 Gateway 的 `[[group]]` 必须完全一致。**
- 客户端和业务后端都不用改，仍然用原来的房间名订阅、发布、查人数。
- Manager 不需要改配置。

### 启动步骤

1. 在所有 Gateway 的配置里加上 `[[group]]`。
2. 逐台滚动重启 Gateway。滚动期间，同一个房间有的 Gateway 是整间登记、有的是分片登记，Manager 日志会出现 `registered both whole and in shards`，全部重启完后消失。
3. 去掉分片：删掉对应的 `[[group]]`，同样逐台滚动重启。

滚动期间这个房间的消息可能有短暂缺失，建议在这个房间人少的时段操作。

### 检查项

- [ ] 场景 C 的检查项全部通过。
- [ ] 所有 Gateway 的 `[[group]]` 一致（逐台比对）。
- [ ] 全部滚动完成后，Manager 日志里不再出现 `registered both whole and in shards`。
- [ ] **演练**：两个测试客户端连到不同的 Gateway，订阅同一个分片房间，一个发、另一个收到且只收到一次；查到的在线人数和实际一致。
- [ ] 观察各台 Manager 的出口，大房间的流量应分散到多台。

### 告警处理

场景 C 的告警照常适用，另外关注：

| 日志关键字 | 出现在 | 意思 | 怎么处理 |
| :--- | :--- | :--- | :--- |
| `topic "..." is registered both whole and in shards` | Manager | 各 Gateway 的 `[[group]]` 不一致 | 滚动期间出现正常；滚动完成后仍出现，逐台比对配置，统一后重启不一致的那台 |
| 分片后某台 Manager 仍然明显高 | 机器监控 | 片数太少，或片的分布不均 | 增大 `shards`，或增加 Manager |

---

## 日常操作（所有场景）

### 重启或升级 Manager

- 直接重启。客户端不断线，各 Gateway 几秒内自动把订阅补回去。
- **恢复期间（几秒）发出的消息会丢**。业务需要可靠消息时，由业务自己补发或让客户端补拉。
- 多台 Manager 时一台一台来，前一台恢复后再操作下一台。

### 重启或升级 Gateway

- 这台机器上的用户会断开，客户端重连后被分到其他 Gateway。
- 一台一台滚动，不要同时重启多台。
- 可选：接入进程收到停止信号时调用 `gateway.Drain(批大小, 间隔)`，先拒绝新连接，再分批断开已有连接，避免全部用户同一秒重连。这需要接入进程的代码配合。

### 从 v0.5.x 升级到 v0.6.x

不支持新旧版本混跑。安排一个窗口，Manager 和所有 Gateway 一起换成新版本。

升级前准备好回退用的东西：v0.5.x 的 Manager 程序、v0.5.x 编译的接入进程、升级前的 `fireTower.toml` 和 `topicmanage.toml`。

### 回退到 v0.5.x

也必须整体回退，不能只退一部分：

1. 停掉所有 Gateway（用户会断开）。
2. 停掉所有 Manager。
3. 换回 v0.5.x 的 Manager 程序和配置，启动。v0.5.x 只支持一台 Manager，多 Manager 的部署要先改回一台。
4. 换回 v0.5.x 编译的接入进程和升级前的配置，逐台启动 Gateway。v0.5.x 不认 `[[manager]]`、`[[group]]`、`[memory]`，配置里要改回 `topicServiceAddr` 和 `[grpc] address`。
5. 按场景 A 或 B 的检查项检查一遍。

回退后内存保护、订阅自动补回等 v0.6 的能力都没有了，Manager 重启后客户端要重连才能恢复收消息。

---

## 附录 1：常见故障怎么判断

| 现象 | 先查 |
| :--- | :--- |
| 所有用户都收不到消息，但连接还在 | Manager 进程是否存活；Gateway 日志里有没有 `replay to manager ... keeps failing` 或 `tcp connect ... error` |
| 只有某一台 Gateway 上的用户收不到 | 这台 Gateway 到 Manager 的 `6666`、`6667` 是否通；Manager 日志里有没有这台的 `gateway outbox` 告警 |
| 新用户连不上，老用户正常 | 是否到了 `maxConnections`，或内存保护在拒绝新连接（日志 `memory guard: rejecting new connections`） |
| 用户频繁掉线重连 | 客户端是否响应 Ping（`heartbeat` 的 2 倍时间内没响应就断）；内存保护是否在断开连接；负载均衡的空闲超时是否小于 `heartbeat` |
| 在线人数和实际对不上 | 各 Gateway 的 `[[group]]`、`[[manager]]` 是否一致。Manager 重启后的几秒内人数短暂偏低属正常 |
| 进程退出 | 正常情况下业务回调出错或收到坏数据不会让进程退出。先看是否被系统 OOM 杀掉（`dmesg \| grep -i oom`），是的话写 `limitMB` 或加内存 |

## 附录 2：监控建议

- 进程存活；Manager 的 `6666`、`6667` 在监听。
- 机器的 CPU、内存、网卡出口。Manager 重点看出口。
- Gateway 可以在接入进程里定时调用 `gateway.MemoryGuardStats()` 上报：上限、当前存活堆、是否在拒绝新连接、累计断开的连接数。Manager 可以用 `manager.MemoryGuardDropped()` 上报丢弃数。这两项需要接入代码配合。
- Manager 自带一个 HTTP 看板（`manager.HttpDashboard()`，缺省 `:8000`，没有认证）。示例程序默认没打开，需要的话在 Manager 的 `main` 里调用，并且只在内网开放。

## 附录 3：配置项速查

**Gateway `fireTower.toml`**

| 键 | 缺省 | 说明 |
| :--- | :--- | :--- |
| `heartbeat` | 30 | 秒。Ping 间隔，读超时是它的 2 倍 |
| `chanLens` | 8 | 每条连接的收发队列长度（模板里用 1000） |
| `maxConnections` | 100000 | 本机连接数上限 |
| `maxTopicsPerConn` | 4096 | 每条连接最多订阅的房间数 |
| `maxTopicsPerFrame` | 1024 | 一次订阅请求里最多的房间数 |
| `topicServiceAddr`、`[grpc] address` | — | 只有一台 Manager 时用 |
| `[[manager]]` | — | `name`、`tcp`、`grpc`、`register`、`publish` |
| `[[group]]` | — | `topic`、`shards` |
| `[bucket]` | — | `Num`、`CentralChanCount`、`BuffChanCount`、`ConsumerNum` |
| `[warn]` | 70 / 70 | `connectionPercent`、`queuePercent` |
| `[memory]` | 自动 | `limitMB`、`softPercent`（70）、`hardPercent`（85）、`criticalPercent`（95）、`shedPercent`（5）、`disable` |

**Manager `topicmanage.toml`**

| 键 | 说明 |
| :--- | :--- |
| `[grpc] port` | 订阅登记端口，通常 6667 |
| `[socket] port` | 发布和推送端口，通常 6666 |
| `[memory]` | `limitMB`、`hardPercent`（85）、`criticalPercent`（95）、`disable` |

**写死在代码里、不能配置的值**（排查时用得到）：Manager 对 Gateway 的心跳 10 秒；Gateway 读 Manager 超时 30 秒；Manager 写 Gateway 超时 5 秒；订阅登记超时 500ms，再试 1s、2s；断线重连从 200ms 起倍增、封顶 30 秒；每 3 分钟全量对账一次。
