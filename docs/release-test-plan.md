# 上线前测试清单

本文列出上线前必须跑的测试、怎么跑、什么算通过。扩容方案见 [scale-plan.md](scale-plan.md)。

整理时间是 2026-11-10，对着当时工作区的代码写的。当时的状态：改动都还没有提交，下面的用例一条都没有在这份代码上实际运行过。用例清单会随代码变化，以 `go test -list . ./...` 的输出为准。

## 怎么用这份清单

按顺序做，前一层没过不要做后一层。每一层的结果填进文末的记录表。

| 层 | 内容 | 大约耗时 | 不通过的后果 |
| :--- | :--- | :--- | :--- |
| 0 | 能编译，代码已提交 | 几分钟 | 后面的结果对不上代码 |
| 1 | 全部单元和审计用例，带竞态检测 | 几分钟 | 有已知缺陷或数据竞争 |
| 2 | 评审里发现的问题各有一条回归用例 | 取决于要补多少 | 这些问题会在线上重现 |
| 3 | 真实进程验收 `cmd/livecheck` | 几分钟 | 进程级的流程不通 |
| 4 | 故障注入 | 半天 | 故障后恢复不了，或恢复时间未知 |
| 5 | 压测和长时间运行 | 一到两天 | 容量未知，内存可能持续上涨 |
| 6 | 接入方式没有变 | 半天 | 现有客户端或接入进程要改 |
| 7 | 上线配置核对 | 一小时 | 测的和上的不是同一份配置 |
| 8 | 灰度和回退 | 数天 | 问题直接影响全部用户 |

## 上线范围先定下来

测试只覆盖要开启的功能。下面这些默认都是关闭的，不打算开就不用测，但要确认配置里确实没开：

| 功能 | 开关 | 默认 |
| :--- | :--- | :--- |
| 多台 Manager | 配置里的 `[[manager]]` 列表 | 只用 `topicServiceAddr` 和 `[grpc].address`，一台 |
| 热点 Topic 独立队列 | `hotTopicMin` | 0，关闭 |
| 缺失提示帧 | `FireTower.EnableGapHints()` | 关闭 |
| 被动订阅 | `FireTower.SetPassive()` | 都是主动 |
| 房间分片 | `gateway.SetTopicShards()` 或配置里的 `group` | 没有任何房间分片 |
| Manager 启动预热 | `managerWarmup` | 0，关闭 |

第一次上线建议只用默认配置：一台 Manager，上面几项全部不开。下文标了「仅多 Manager」「仅开启时」的条目可以跳过。

## 第 0 层：编译和提交

```
go build ./...
go vet ./...
git status --short
```

通过标准：

- 前两条没有任何输出。
- 要上线的代码已经提交，`git status` 里没有未提交的 `.go` 文件。测试记录里写上提交号。
- `go.mod` 里的 Go 版本和构建机器上的一致。

## 第 1 层：单元和审计用例

```
go test -count=1 ./...
go test -race -count=3 ./...
```

`-race` 在 Windows 上需要 CGO 和 gcc。没有 gcc 时这条命令在编译阶段就失败（`cgo: C compiler "gcc" not found`），所有包都显示 `[build failed]`，测试一条都没有执行，这不算跑过。可以装 MinGW-w64，或者在 Linux、WSL、CI 里跑这一条，不能跳过：并发相关的缺陷大多只有它能报出来。第一条不带 `-race` 的命令不需要 gcc，先跑它。

通过标准：

- 两条命令全部 `ok`，没有 `FAIL`，没有 `WARNING: DATA RACE`。
- `-count=3` 三轮结果一致。偶尔失败一次的用例按失败处理，不要重跑到通过为止。
- 没有被 `t.Skip` 跳过的用例；有的话逐条写明原因。

用例按前缀分组，定位问题时可以单独跑某一组：

| 前缀或文件 | 覆盖什么 | 单独跑 |
| :--- | :--- | :--- |
| `TestStage0_` | 空指针、坏帧、并发关闭都不能让进程 panic。三个包里各有一个 `stage0_crash_test.go` | `go test -race -run TestStage0 ./...` |
| `TestAudit_FT` | 功能正确性：排队中的消息不被改写、`ToSelf` 入队、订阅有超时、桶之间互不阻塞 | `go test -race -run 'TestAudit_FT' ./...` |
| `TestAudit_CO` | 并发：单写者、订阅和转发竞争、连接替换 | `go test -race -run 'TestAudit_CO' ./...` |
| `TestAudit_NW` | 网络：静默连接被关闭、重连后重放、慢 Gateway 不拖累其他、写失败不停掉发布方 | `go test -race -run 'TestAudit_NW' ./...` |
| `TestAudit_DL` | 关闭路径不死锁：退订失败、RPC 卡住、重入关闭、踢人 | `go test -race -run 'TestAudit_DL' ./service/gateway` |
| `TestAudit_PR`、`TestAudit_TC` | 协议解包的边界、TCP 客户端的关闭和重连 | `go test -race -run 'TestAudit_(PR|TC)' ./socket` |
| `audit_stability_test.go` | 上限和超时：每帧和每连接的 Topic 数、连接数、RPC 不挂住、日志级别 | `go test -race -run 'TestAudit_(TopicFrame|ConnectionLimit|ManagerRPC|ManagerIP|DropLog|ToSelfNil)' ./service/gateway` |
| `capacity_test.go` | 只进持有 Topic 的桶、热点队列、被动订阅、缺失提示默认关闭、容量告警 | `go test -race -run 'TestAudit_(OnlyBuckets|HotTopic|Passive|GapHint|CapacityWarn)' ./service/gateway` |
| `pool_test.go` | 连接池：单台永远被选中、哈希稳定、只登记不发布、登记全成或全败、旧配置等于一台 | `go test -race -run 'TestAudit_(OneManager|HashStays|RegisterOnly|RegisterIsAll|GetConnectNumUses|OldConfig)' ./service/gateway` |
| `group_test.go` | 房间分片（仅开启时） | `go test -race -run 'TestAudit_Group|TestAudit_Ungrouped' ./service/gateway` |
| `service/manager/audit_test.go` | 绝对人数盖写、对账、握手换键、写队列满不阻塞、登记排队、关闭时删索引 | `go test -race ./service/manager` |
| `grpc/manager/absolute_test.go` | proto 新字段的编解码 | `go test ./grpc/manager` |

## 第 2 层：评审问题的回归用例

下面每一条都是代码评审里发现的问题。上线前要么有一条用例证明它已经不存在，要么书面确认接受这个风险。「现有用例」一列里写的是名字相近的用例，它测的未必是这里要求的行为，要打开看断言。

| 编号 | 场景 | 期望结果 | 现有用例 |
| :--- | :--- | :--- | :--- |
| R1 | 只有一台 Manager。持续有订阅请求在处理的同时，连续发布 30 秒，中间跨过至少三次 Manager 心跳 | 没有一次 `Publish` 因为「没有可用的 Manager」失败 | `HeartbeatWaitsWhileWarmingOrBusy`、`HeartbeatWaitIsNotPublishable` 测的是相反方向，核对它们的断言是否已随代码改掉 |
| R2 | 同一个桶里，连接 A 是某 Topic 的最后一个订阅者。A 退订和连接 B 订阅该 Topic 并发执行，重复十万次 | 每一轮结束后向该 Topic 发布，B 都能收到 | 无 |
| R3a | 整表重放分成多块，让中间一块的 RPC 失败、最后一块成功 | 失败那一块里的 Topic 在 Manager 上的路由还在 | `ReconcileDropsTopicsMissingFromTheRound` 只测了「没出现就删」 |
| R3b | 重放取完快照之后、第一块到达 Manager 之前，新订阅一个 Topic | 本轮对账结束后，这个新 Topic 的路由还在 | 无 |
| R4 | 一次性向同一台 Gateway 上的 Topic 连续发布 5000 条 | 全部送达；或者丢弃的条数能从计数里读出来，且与实际缺少的条数相等 | `FullOutboxDoesNotBlockPublisher` 只测了不阻塞 |
| R5 | 依次订阅再退订 100 万个互不相同的 Topic，中间让 Gateway 与 Manager 的 TCP 重连 10 次 | 结束后 Gateway 和 Manager 的内存回到开始时的水平附近，不随 Topic 总数增长 | 无 |
| R6 | 一个 Topic 有 1 万个订阅者，发布一条 1KB 的消息 | 这一条消息引起的内存分配远小于 10MB（订阅者数 × 正文大小） | 无 |
| R7 | Gateway 到 Manager 的 TCP 断开、gRPC 仍然通的期间，让客户端订阅一个 Topic | 订阅失败；或者成功且 TCP 恢复后 `GetConnectNum` 等于实际人数。Manager 上不留下没有连接对应的条目 | 无 |
| R8 | 客户端一帧订阅 `"a,a"`，然后退订 `a` | `GetConnectNum("a")` 为 0，Manager 上没有 `a` 的路由 | 无 |
| R9 | 两台 Manager 同时断开再同时恢复（仅多 Manager） | 两台都在几秒内收到这台 Gateway 的完整重放，不用等定时对账 | `ReplayIsOneAtATime` 测的是「同一时刻只跑一轮」，和这里的要求有冲突，要一起看 |
| R10 | 热点队列的处理协程发生一次 panic（仅开启时） | 之后该 Topic 的消息仍然能送达 | `HotLoopSurvivesPanic`、`ConsumerSurvivesPanic` |

## 第 3 层：真实进程验收

先编出两个可执行文件，再交给 `cmd/livecheck`。它会自己拉起 Manager、Gateway 和 WebSocket 客户端。

```
go build -o bin/topic ./example/topic_service
go build -o bin/gateway ./example/websocket_service
go run ./cmd/livecheck -mode all -topic <仓库绝对路径>/bin/topic -gateway <仓库绝对路径>/bin/gateway -conns 1000 -hold 60s
```

`-topic` 和 `-gateway` 必须写绝对路径。`livecheck` 会把子进程的工作目录切到临时配置目录，相对路径会被解析到那个临时目录下，报 `executable file not found`。Windows 上例如 `C:\meteor\firetower\bin\topic`。

| 模式 | 做什么 | 通过标准 |
| :--- | :--- | :--- |
| `nw04` | 两个客户端收发，中途停掉 Manager 再拉起 | 输出 `NW04 PASS`。把打印的「恢复耗时」记下来，它是第 4 层的基线 |
| `lg` | 拉起 `-conns` 个连接，保持 `-hold`，再注入 Manager 故障 | 输出 `LG PASS`。记下打印的协程数和内存基线 |
| `crash` | 坏帧打到 Gateway，再把 Manager 连续重启 5 次 | 输出 `CRASH PASS`。Gateway 进程不退出，原连接不重订也继续收到消息 |

通过之后把 `-conns` 提到计划上线规模的同一数量级再跑一次。`livecheck` 只起一台 Gateway。

### 多 Gateway、多 Manager、热点分组：`cmd/stagecheck`

`livecheck` 只有一台 Gateway 和一台 Manager。阶段 2、3、4 用 `cmd/stagecheck` 验证，它自己拉起多台 Gateway 和多台 Manager（同一个可执行文件按角色启动，每台 Gateway 的 `ClusterId` 不同），不需要先编 example。

```bash
go build -race -o bin/stagecheck ./cmd/stagecheck
./bin/stagecheck -stage all        # 也可以 -stage 2、3、4 单独跑
```

本机没有 gcc 时去掉 `-race` 也能跑，但竞态那一项就没有意义了。全部通过时最后一行是「通过 110，失败 0」（项数随版本增加，以 0 失败为准），退出码 0，Linux 约 10 分钟，Windows 约 12 分钟（其中 3 分多钟在等定期对账，近 2 分钟在等内存保护那组的内存自然回落）。失败时各进程的日志目录会保留并打印出来。

| 阶段 | 拓扑 | 查什么 |
| :--- | :--- | :--- |
| 2 | 4 台 Gateway + 1 台 Manager，另有一轮 20 台 Gateway | 跨 Gateway 恰好一次；没订阅的收不到；人数精确；7 种业务回调 panic 后进程还在；硬杀和重新拉起 Gateway；扩一台再 `Drain`；Manager 重启后原连接不重订就恢复，并打印恢复耗时；连接频繁进出时重启 Manager，结束后人数必须立刻精确 |
| 3 | 3 台 Gateway + 2 台 Manager，中途加到 3 台 | 每台 Manager 各自有全量路由；轮流杀、同时杀；一台不在时的新订阅；后端经任意一台 Manager 的 gRPC 发布；两条管理命令；按「先只登记、再打开发布」两步滚动加一台 Manager，滚到一半也不丢；往 Manager 和 Gateway 灌坏帧、坏请求 |
| 4 | 3 台 Gateway + 2 台 Manager，`big` 分 8 片 | 确认真的分了片；任意 Gateway 发布各片恰好一次；人数是各片之和；后端按房间名 gRPC 发布和查人数；按房间名踢人、解散；杀 Manager、杀 Gateway；客户端自己拼片键 |
| `net` | 3 台 Gateway + 1 台 Manager，中间放一段可控的转发 | 网络卡住 3 秒；握手确认超过 500ms 才到；链路卡住时后端连发 2 万条把 Manager 的发送队列写满；断网 40 秒（超过 30 秒读超时）再恢复。连接都不断开，只能靠超时发现 |
| `load` | 3 台 Gateway + 1 台 Manager | 热点房间每秒 900 条、每条约 2KB，10% 的连接不读：安静房间每条恰好一次且不超过 1 秒；不串内容、不重复；正常连接收到 99.5% 以上 |
| `opt` | 2 台 Gateway + 1 台 Manager | 单帧 Topic 数、单连接 Topic 数、连接数三个上限；被动订阅不收状态事件；缺失提示只发给丢了消息且打开了提示的连接 |
| `cfg` | 2 台 Gateway + 2 台 Manager | `register=false`、`publish=false` 的 Manager 等于停用；各 Gateway 分组配置不一致时不崩并告警，改一致后恢复 |
| `mem` | 1 台 Gateway（上限 64MB）+ 1 台 Manager；另一组 2 台 Gateway + 1 台 Manager（上限 200MB） | Gateway：开连接直到被拒；40 条不回心跳的连接在读超时之前被内存保护优先关掉；一直回心跳的连接一条不关、人数正确、恰好一次；断开一部分后内存回落并重新接受新连接。Manager：链路卡住时后端灌 64KB 大消息，存活堆不超过上限，链路恢复后投递和人数正确 |
| `reconcile` | 2 台 Gateway + 1 台 Manager | 在 Manager 上人为造出人数多、多一个空房间、少一条路由三种偏差，不重启不重连，等 3 分钟的定期对账全部纠正 |

阶段 2 里还包括：4KB 到 900KB 的消息跨 Gateway 原样送达；两台 Gateway 误用同一个 `ClusterId`；20 台 Gateway 同时各订 600 个 Topic（越过重放的 256 分块和登记并发上限 8）。阶段 3 里还包括按两步减一台 Manager、往每台 Manager 和 Gateway 灌坏帧坏请求。

每个阶段最后都检查：没被故意杀掉的进程都还活着，日志里没有未恢复的 panic，没有竞态报告。

`stagecheck` 的连接数很小，它查的是流程对不对，不是容量。容量仍然看第 5 层。

另有三个模糊测试，每次发版前各跑一分钟以上：

```bash
go test ./socket -run '^$' -fuzz FuzzDepack -fuzztime 60s
go test ./service/manager -run '^$' -fuzz FuzzRegisterRequests -fuzztime 60s
go test ./service/gateway -run '^$' -fuzz FuzzClientFrame -fuzztime 90s
```

## 第 4 层：故障注入

`livecheck` 只覆盖了「Manager 进程退出」这一种。下面几项要在测试环境里手工做，Gateway 和 Manager 分别放在两台机器或两个容器里。

每一项都要记录两个数：故障发生到系统发现的时间，故障解除到恢复正常的时间。

| 编号 | 做法 | 通过标准 |
| :--- | :--- | :--- |
| F1 | 杀掉 Manager 进程，10 秒后拉起。客户端的 WebSocket 不重连，也不重新订阅 | 拉起后若干秒内，原有订阅者重新收到新发布的消息。恢复时间不超过预先定好的上限 |
| F2 | 用防火墙丢弃 Gateway 与 Manager 之间的全部包，不发 RST，60 秒后恢复。这模拟的是主机断电，不是进程退出 | Gateway 在读超时（30 秒）左右判定断线并开始重连；Manager 在写超时或心跳失败后清掉这台 Gateway。恢复后同 F1 |
| F3 | 两台 Gateway。让其中一台的进程暂停（挂起，不是杀掉）60 秒 | 另一台 Gateway 上的发布和接收不受影响，延迟没有明显变化 |
| F4 | 杀掉一台 Gateway 进程 | Manager 上这台的路由被清掉，`GetConnectNum` 回落。它的客户端重连到另一台后能正常收发 |
| F5 | 让 10% 的客户端停止读取 | 只有这些连接丢消息。其余连接的延迟不变。日志量不随丢弃数线性增长。进程内存稳定 |
| F6 | 客户端发送：非 JSON、`null`、超过 1MB 的帧、带几千个 Topic 的订阅帧 | 进程不退出。超限的请求被拒绝，连接按预期关闭或保留 |
| F7 | 短时间内让全部客户端断开再重连，并各自重新订阅 | Manager 和 Gateway 都不退出。订阅最终全部成功，或者失败的能被客户端感知并重试 |
| F8 | Gateway 运行中，Manager 连续重启 5 次，每次间隔 30 秒 | 每次都能恢复。5 次之后 Gateway 的协程数和内存与开始时接近 |
| F9 | 依次杀掉每一台 Manager（仅多 Manager） | 任何时刻都没有 Topic 完全收不到消息。被杀的那台回来并重放完成后才重新承接发布 |
| F10 | 调用 `gateway.Drain` | 新连接被拒绝，现有连接分批关闭，客户端重连到其他 Gateway |

## 第 5 层：压测和长时间运行

现有工具：

```
go run ./stress -stable 50 -chaos 50 -slow 10 -d 30s -addr 127.0.0.1:9999
go run ./stress/broadcast -addr 127.0.0.1:6666 -topic stable_chat -rate 1000 -d 30s
```

第一条模拟三类客户端：稳定连接、频繁断开重连、慢读。第二条直接连 Manager 的 TCP 端口，按固定速率向一个 Topic 发布。参数的缺省值很小，要按目标规模放大。

压测前先解决一件事：丢弃和失败的计数现在都是进程内的变量（`sendDrops`、`bucketDrops`、`socket.DroppedFrames`、`ManagerFlaps`），没有对外读取的入口。压测需要能读到它们，否则「丢了多少」只能靠客户端自己数。

| 编号 | 场景 | 通过标准 |
| :--- | :--- | :--- |
| P1 | 连接数：只建连接和订阅，不发消息，逐步加到目标在线人数的 1.5 倍 | 全部连上。每条连接的内存与预期一致。达到 `maxConnections` 后新连接被拒绝而不是进程变慢 |
| P2 | 小房间：目标在线人数，每人订阅几个 Topic，每个 Topic 几个人，按预期的业务速率发布 | 没有丢弃。端到端延迟的 99 分位在可接受范围内 |
| P3 | 大房间：一个 Topic 的订阅者数取业务里最大房间的 2 倍，发布速率从低往上加，直到开始丢弃 | 记下开始丢弃时的「房间人数 × 每秒消息数」，这就是单台的投递预算。同时在跑的小房间不受影响 |
| P4 | 订阅风暴：每秒新增连接数取登录高峰的 2 倍，每条连接订阅若干 Topic | 订阅失败率在可接受范围内，Manager 的登记不长时间排队 |
| P5 | 慢消费者：P2 的负载下让 10% 的连接慢读 | 同 F5 |
| P6 | 长时间运行：P2 的负载加上持续的连接断开重连，连续跑 24 小时以上，期间重启 Manager 两次 | 内存和协程数在头一小时之后不再持续上涨。结束时没有累积的丢弃或失败 |

P6 最重要。内存只增不减的问题在几分钟的压测里看不出来。

### 单机压测：`stagecheck -stage bench`

P1、P3 和 Manager 的转发能力可以用 `cmd/stagecheck` 在一台机器上量出来。它按部署手册场景 A 的模板（`chanLens = 1000`、`ConsumerNum = 1`）起真实进程，不需要另外准备环境：

```
go build -o stagecheck ./cmd/stagecheck
./stagecheck -stage bench
```

| 项 | 做法 | 输出 |
| :--- | :--- | :--- |
| B1（P1） | 一台 Gateway 建 5000、10000 条空闲连接，订阅 100 个房间，再全部断开 | 每条连接的存活堆、Go 管理内存、协程数；断开后协程是否回到基线 |
| B2（P3） | 一台 Gateway，一个 2000 人的房间，每条 1KB，发送速率从 25 条/秒逐档翻倍，每档 5 秒 | 不丢且 p99 ≤ 1 秒的最高一档，换算成每秒投递次数；各进程 CPU |
| B3 | 1 台 Manager、8 台 Gateway（每台 10 人订阅同一房间），速率从 250 条/秒逐档翻倍 | 最高一档的帧数/秒和 Manager CPU |

参数：`-bench-conns`、`-bench-subs`、`-bench-gateways`、`-bench-size`、`-bench-step`。Linux 上会自动把文件句柄软上限提到硬上限，不够时自动减少 B1 的连接数并打印出来。所有进程和压测客户端都在同一台机器上，结果是这台机器的下限。Windows 上可以用 `cmd/stagecheck/run-bench.bat` 一次跑完 Windows 和 WSL 两种环境：先编译 `GOOS=windows go build -o bench-windows.exe ./cmd/stagecheck` 和 `GOOS=linux go build -o bench-linux ./cmd/stagecheck`，和 bat 放在同一个目录，双击 bat，结果在旁边的 `logs` 目录。

## 第 6 层：接入方式没有变

用**上一个已上线版本**的客户端和接入进程代码，不做任何修改，对着新版本跑一遍主流程。

| 检查项 | 通过标准 |
| :--- | :--- |
| 客户端发 `{"topic","data","type"}`，`type` 为 `subscribe`、`unSubscribe` 和其他 | 行为和旧版本一致 |
| 客户端收到的帧 | 只有发布方的 `data` 原样、WebSocket Ping、踢人时的 `{}`。没有新出现的帧类型 |
| 接入进程的代码 | 不改一行就能编译。`BuildTower`、`Run`、`Publish`、`ToSelf`、全部 `Set…Handler`、`GetConnectNum`、`CheckTopicExist`、`GetTopicManage`、`GetTopicManageGrpc` 的签名没变 |
| Manager 的启动代码 | `StartGrpcService` 现在有返回值。用 `go m.StartGrpcService(...)` 的旧写法仍能编译，确认一遍 |
| 订阅前后、读消息、下线几个回调 | 触发时机和次数与旧版本一致 |
| `GetConnectNum` | 订阅后立刻读到的人数是对的 |
| 业务服务端通过 gRPC 调 `Publish` | 行为不变 |
| 旧的配置文件 | 不改就能启动，等价于只有一台 Manager |
| 新旧版本混跑 | 新 Manager 配旧 Gateway、旧 Manager 配新 Gateway 都能订阅和收发。升级顺序是先 Manager 后 Gateway |

要注意一处有意的变化：`ToSelf` 从同步写改成了入队，队列满时返回错误。接入进程里如果依赖它的返回值，要看一眼。

## 第 7 层：上线配置核对

用要上线的那份配置文件重跑第 3 层。

| 检查项 | 说明 |
| :--- | :--- |
| 进程实际读的是哪份配置 | Gateway 读工作目录下的 `./fireTower.toml`，除非设了 `gateway.DefaultConfigPath`。仓库里有三份内容不同的副本 |
| `heartbeat` | `config/fireTower.toml` 里是 600，死连接要等约 1200 秒才发现。上线值取几十秒 |
| `chanLens` | 每条连接两条这么长的队列。现在每条消息在队列里是独立副本，这个值乘以消息大小就是单条慢连接能占的内存 |
| `bucket.ConsumerNum` | 大于 1 时同一连接上的消息先后不保证 |
| `maxConnections`、`maxTopicsPerConn`、`maxTopicsPerFrame` | 缺省是 100000、4096、1024。确认比业务的正常值大得多 |
| `topicServiceAddr`、`[grpc].address` | 多台 Gateway 时填 Manager 的内网地址，不是 `0.0.0.0` 或 `localhost` |
| `gateway.ClusterId` | 每个 Gateway 进程不同。它在接入进程的 `main` 里赋值 |
| 日志级别 | `LogLevel` 为 `INFO` 时热路径按采样打印正文。确认日志量和磁盘 |
| 进程守护 | Manager 和 Gateway 退出后能被自动拉起 |

## 第 8 层：灰度和回退

| 步骤 | 内容 |
| :--- | :--- |
| 回退演练 | 上线前先在测试环境做一次：新版本换回旧版本，客户端重连后一切正常。回退顺序与升级相反，先 Gateway 后 Manager |
| 第一步灰度 | 一台 Gateway 承接一小部分流量，其余保持旧版本。观察至少一个业务高峰 |
| 观察的指标 | 发布失败次数、发送队列丢弃数、桶队列丢弃数、解包丢帧数、Gateway 与 Manager 的重连次数、进程内存、协程数、订阅失败次数 |
| 继续放量的条件 | 上面的指标平稳，内存没有持续上涨，没有用户反馈收不到消息 |
| 立即回退的条件 | 进程退出；发布持续失败；内存持续上涨；任一丢弃指标持续不为零且找不到原因 |

## 结果记录

| 层 | 日期 | 提交号 | 配置 | 结果 | 未通过项和处理 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| 0 编译和提交 | 2026-11-10 | 工作区，HEAD `682da3f` | 本机 Go 1.26.1。`go.mod` 写的是 1.24.0，toolchain 1.24.4 | `go build ./...`、`go vet ./...` 无输出 | 要上线的 `.go` 还没提交。模块声明的 Go 版本和本机编译器不一致 |
| 1 单元和审计用例 | 2026-11-10 | 未提交 | `go test -count=1` 四个有测试的包 | 通过。gateway 40.9s（含 R1 的 30 秒），manager 2.4s，socket 5.4s，grpc 0.2s。没有 `t.Skip` | |
| 1 带 `-race` | 2026-11-10 | 未提交 | 本机无 gcc | 没跑成。`cgo: C compiler "gcc" not found` | 不记为通过。`-count=3` 也没跑 |
| 2 回归用例 R1～R10 | 2026-11-10 | 未提交 | 单台 Manager | R1～R8、R10 通过 | R9 是多 Manager 才做的项，默认配置不开，这次跳过 |
| 3 livecheck | 2026-11-10 | 未提交 | `-mode all`，lg `-conns 1000 -hold 60s`，另跑 `-mode crash` | NW04、LG、CRASH 都是 PASS | 二进制要用绝对路径。1000 还不是计划上线规模 |
| 3 stagecheck（全部 10 组） | 2026-11-11 | `2ad677b` 加工作区改动；最终复测为 v0.6.0 | 竞态版，`-stage all`。Linux（Go 1.24.7）、Windows（Go 1.27.3）各连跑 2 次 | 见下面「本轮结果」 | 连接数只有几十到上百，不代表容量。网络故障是在本机用转发模拟的 |
| 4 故障注入 F1～F10 | 2026-11-10 | 未提交 | 本机单进程 | F6 坏帧通过。F1 的立即重启和 F8 的连续 5 次重启通过，原连接都收到了新发布 | F1 没有先停 10 秒。F8 没有隔 30 秒，也没有比内存。F2～F5、F7、F9、F10 没做 |
| 5 压测 P1～P6 | 2026-11-10 | 未提交 | | 没跑 | 没有 1.5 倍连接、投递预算和 24 小时数据 |
| 6 接入方式 | 2026-11-10 | 未提交 | 当前示例能编译 | 示例 `go build` 通过。`/ws` 帧格式没改 | 没有用上一个已上线版本的客户端混跑 |
| 7 配置核对 | 2026-11-10 | 未提交 | 三份 `fireTower.toml` | 见下文 | 第 3 层用的是 livecheck 临时配置，不是 `config/fireTower.toml` |
| 8 灰度 | 2026-11-10 | 未提交 | | 没做 | |

### 本轮结果（2026-11-11，竞态版，含内存保护）

| 项目 | Linux（Go 1.24.7） | Windows（Go 1.27.3） |
| :--- | :--- | :--- |
| 单测 `go test -race -count=3` | 四个包全过，0 竞态 | 四个包全过，0 竞态；原生 `-count=3` 也全过 |
| livecheck `all` + `crash` | NW04、LG、CRASH 全过 | 同左，0 竞态报告 |
| stagecheck 全部 10 组 | 2 轮，每轮 109/109；v0.6.0 最终代码再跑 2 轮，每轮 110/110 | 2 轮，每轮 109/109，0 竞态报告；v0.6.0 最终代码再跑 2 轮，每轮 110/110，0 竞态报告 |
| 模糊测试 | `FuzzDepack` 约 160 万次、`FuzzRegisterRequests` 约 29 万次、`FuzzClientFrame` 约 1.9 万次，都没有 panic | 没跑 |

stagecheck 里打印的几个数（左 Linux，右 Windows）：

| 指标 | Linux | Windows |
| :--- | :--- | :--- |
| 4 台 Gateway，Manager 重启后原连接全部收到 | 2.39s | 3.47s |
| 20 台 Gateway、每台 600 个 Topic，Manager 重启后 80 个连接全部收到 | 3.48s | 8.82s |
| 同上，抽查人数全部对上 | 9.26s | 14.9s |
| 内存保护：Gateway 上限 64MB 时存活堆峰值 | 52～61MB | 45MB |
| 内存保护：Manager 上限 200MB 时存活堆峰值 | 147MB | 150MB |
| 内存保护：40 条不回心跳的连接在读超时前被关掉 | 是 | 是 |
| 内存回落到第一道水位以下（不主动触发 GC，空闲时要等 Go 自己的 2 分钟强制 GC） | 1m44s～1m57s | 1m44s |

修复前的版本在 Windows 上跑这组时，20 台 Gateway 那组两轮都失败（80 个连接里最多 56 个收不到）：Manager 忙时订阅超时被整个丢掉。现在超时的订阅留在本机，由后台重放补登记，两个平台都过了。

### 单机压测结果（2026-11-11，`-stage bench`，v0.6.0）

| 项 | Linux 云主机 2 核 8GB | Windows 8 核 16 线程 32GB（Xeon 8272CL） | 同机 WSL2 16 核 16GB |
| :--- | :--- | :--- | :--- |
| B1 每条空闲连接：Go 管理内存 / 存活堆 / 协程 | 53KB / 33KB / 3 | 67KB / 26KB / 3 | 54KB / 30KB / 3 |
| B1 1 万条连接建连并订阅完 | 0.8 秒 | 1.9 秒 | 1.4 秒 |
| B1 全部断开后 | 协程回到 18，存活堆 27MB | 协程回到 18，存活堆 16MB | 协程回到 18，存活堆 17MB |
| B2 最高通过档（2000 人房间） | 100 条/秒 = 20 万次投递/秒，Gateway 1.0 核 | 100 条/秒 = 20 万次/秒，Gateway 5.2 核 | 50 条/秒 = 10 万次/秒，Gateway 3.2 核 |
| B2 下一档为什么没过 | p99 超过 1 秒，没有丢 | 同左 | 同左 |
| B3 最高通过档（8 台 Gateway） | 2000 条/秒 = 16000 帧/秒，Manager 0.10 核 | 同左，Manager 0.49 核 | 同左，Manager 0.61 核 |
| B3 下一档为什么没过 | 单条发布连接每秒只发出约 2600 条，丢 2%，Manager 没压满 | 实发约 2500 条/秒，丢 0.02% | 实发约 2000 条/秒，p99 超过 1 秒 |
| 进程健康 | 6 项全过，无 panic | 6 项全过 | 6 项全过 |

看这组数时注意：

- 压测客户端和 Gateway 抢同一台机器的 CPU，客户端自己用了 1～5 核。B2 在 16 线程的机器上并没有比 2 核快，CPU 反而用得多。可能的原因是每次投递都要唤醒一个连接的发送协程、做一次系统调用，核数多时线程切换的开销更大；这一点还没有单独验证。生产上客户端不在 Gateway 机器上，数字会更好，但仍应按每秒 10～20 万次投递估单台 Gateway。
- B3 没有测出 Manager 的上限：瓶颈在单条发布连接和同机的 8 个 Gateway。每秒 16000 帧时 Manager 只用了 0.1～0.6 核。
- 没跑 P2、P4～P6，24 小时长跑还没做。

需要单独记下来的数字：

| 数字 | 来源 | 值 |
| :--- | :--- | :--- |
| Manager 重启后的恢复耗时 | 第 3 层 `nw04`、F1 | nw04 新订阅 2.48 秒，原连接不重订也收到新发布。lg 新订阅 2.37 秒。lg：980 连接，协程 29 → 约 3000 → 关掉后 30，内存基线 14.7MB，峰值采样 96.4MB，关掉 3 秒后 56.5MB |
| 断网后发现故障的时间 | F2 | |
| 单台 Gateway 的投递预算 | P3 / bench B2 | 2000 人房间、1KB：每秒 10～20 万次投递（p99 ≤ 1 秒、不丢），见上表 |
| 每条空闲连接的内存 | P1 / bench B1 | `chanLens = 1000` 时 Go 管理内存 53～67KB，存活堆 26～33KB，3 个协程 |
| 24 小时后的内存相对第一小时的变化 | P6 | |

第 7 层核对到的三份配置：

| 文件 | heartbeat | chanLens | ConsumerNum |
| :--- | :--- | :--- | :--- |
| `config/fireTower.toml` | 600 | 1000 | 32 |
| `fireTower.toml` | 30 | 1000 | 1 |
| `example/websocket_service/fireTower.toml` | 30 | 1000 | 1 |

`config/fireTower.toml` 的心跳是 600 秒，静默连接要大约 1200 秒才会被关掉。livecheck 自己写的临时配置是 30 秒，所以第 3 层没有覆盖这份 600 秒的配置。示例里的 `gateway.ClusterId` 仍写死为 1。
