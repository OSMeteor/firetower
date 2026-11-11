# 修改要求：竞态检测清零

这份文档交给负责改代码的人。改完并满足文末的验收标准后，才能打 `v0.6.0-rc.1`。

## 背景

2026-11-10 17:35，对工作区当时的代码（未提交，最近一次提交是 `682da3f`）做了一次完整运行。环境是 Linux amd64、Go 1.24.7、有 gcc。下文的行号以那一刻的文件为准，代码变动后以函数名为准。

| 项目 | 结果 |
| :--- | :--- |
| `go build ./...`、`go vet ./...` | 通过 |
| `go test -count=1 ./...` | 全部通过 |
| `go test -race -count=3 ./...` | 退出码 1。24 条 `WARNING: DATA RACE`，14 个用例被判失败 |
| 用 `go build -race` 编出的 Manager 和 Gateway 跑 `cmd/livecheck` | `NW04 PASS`、`LG PASS`，但 Gateway 进程报了 2 条数据竞争 |

所有失败用例的失败信息都只有 `race detected during execution of test`，没有断言失败。

## 要改的内容

### A. 正式代码里的 2 处数据竞争（必须改）

这两处是真实进程在竞态检测下报出来的，和测试无关。

**A1. `service/gateway/bucket.go` 的 `getConnId` 和 `getNewBucketId`**

现状：

```go
func getConnId() uint64 {
	atomic.AddUint64(&connId, 1)
	return connId
}
```

第二行是普通读。两个连接同时建立时可能拿到同一个 `connId`。`liveTowers` 以 `connId` 为键，后一个连接会覆盖前一个；其中一个关闭时 `liveTowers.Delete` 还会删掉另一个的记录，`Drain` 因此会漏掉连接。

要求：返回原子操作的结果。`getNewBucketId` 是同样的写法，一起改。

```go
func getConnId() uint64 {
	return atomic.AddUint64(&connId, 1)
}
```

**A2. `service/gateway/tower.go` 的 `readDispose` 里对 `t.isClose` 的无锁读**

现状：`Close` 在 `t.mutex` 内写 `t.isClose`，`readDispose` 里的 `else if t.isClose` 没有加锁。

要求：这一处改成在 `t.mutex` 内读，或者把 `isClose` 改成 `atomic.Bool`。如果选后者，文件里其他读写 `isClose` 的地方要一起改，保持一致。

### B. 测试造成的竞态报告（必须清零）

24 条报告里，每一条都有一方是测试代码。它们的共同原因是：测试修改了包级变量，而上一个用例或本用例启动的协程还在读这些变量。

这些报告不说明正式代码有缺陷，但它们让 `go test -race` 永远失败。只要这一关是红的，以后出现真正的竞态也会被淹没，所以必须清零。

| 次数 | 测试一侧 | 正式代码一侧 | 涉及的变量 |
| :--- | :--- | :--- | :--- |
| 10 | `service/gateway/audit_env_test.go` 的 `saveAuditGlobals` 收尾，恢复 `TowerLogger` | `logInfo`、`readDispose`、`noteSendDrop` | `TowerLogger` |
| 2 | 同上，恢复 `ConfigTree` | `configInt` | `ConfigTree` |
| 1 | 同上，恢复 `TM` | `unbindTopic` | `TM` |
| 2 | 同上，恢复 `topicManageGrpc`、`topicManage` | `activeLinks` | `topicManage`、`topicManageGrpc` |
| 1 | `service/gateway/audit_checklist_test.go` 的 `TestAudit_FT03_QueuedMessageKeepsPayload`，交出消息后改写 `msg.Data` | `dispatch` 里采样打印日志时读同一条消息（`socket.sendLog`） | 消息的 `Data` |
| 1 | `service/gateway/stage0_crash_test.go` 的 `TestAudit_HotLoopSurvivesPanic`，写 `testBeforePush` | `Bucket.consumer` | `testBeforePush` |
| 5 | `service/manager/stage0_crash_test.go` 的 `TestStage0_AcceptLoopSurvivesBadConnection`、`TestStage0_GrpcPanicDoesNotEscape`，替换和恢复 `Logger` | `handler`、`relation`、`serveSocket` | `manager.Logger` |
| 1 | `socket/audit_test.go` 的 `TestAudit_ReadTimeoutReplacesSilentManager`，写 `managerReadTimeout` | `readLoopConn` | `managerReadTimeout` |
| 1 | `socket/audit_test.go` 的 `TestAudit_DialLoopRestartsAfterPanic`，替换 `dialTCP` | `Connect` | `dialTCP` |

改法由实现者决定，下面是可行的方向：

- **测试先等协程退出，再恢复变量。** 用例里启动的连接（`Run`、`readLoop`、`sendLoop`）、Manager 的 `serveSocket` 和连接协程、`TcpClient` 的读写协程，都要在收尾恢复包级变量之前确认已经结束。这是最不影响正式代码的做法。
- **不导出的变量可以改成原子访问。** `testBeforePush`、`hotTopicMin`、`managerReadTimeout`、`dialTCP` 都不是对外接口，改成 `atomic.Pointer`、`atomic.Int64` 之类不影响接入方。
- **整个包只设置一次、不再恢复。** 例如日志函数在 `TestMain` 里统一换成空实现，各用例不再各自替换。
- **`FT03` 那一条**：用例在把消息交给 `dispatch` 之后改写了原对象，用来验证排队的副本不受影响。可以让用例等 `dispatch` 处理完这条消息之后再改写，或者让 `dispatch` 的采样日志在入队之前打印。

## 不能做的事

- **不能改变接入方式。** 客户端协议、导出函数的签名、导出变量的类型都不变。`gateway.TowerLogger`、`gateway.FireLogger`、`gateway.ConfigTree`、`gateway.TM`、`gateway.IdWorker`、`gateway.ClusterId`、`manager.Logger` 是接入进程会直接赋值的导出变量，不能为了消除竞态把它们改成别的类型或改成私有。
- **不能靠削弱测试来过关。** 不删除用例，不加 `t.Skip`，不放宽断言，不用 `//go:build !race` 之类的构建标签把用例排除在竞态检测之外。
- **不能改变主流程的行为。** 订阅、发布、接收、退订、断线恢复的结果和现在一致。

## 验收标准

下面每一条都要满足。命令在仓库根目录执行。

**1. 编译和静态检查**

```
go build ./...
go vet ./...
```

没有输出，退出码 0。

**2. 功能用例**

```
go test -count=3 ./...
```

全部 `ok`。

**3. 竞态检测**

```
go test -race -count=3 ./...
```

退出码 0，输出里没有 `WARNING: DATA RACE`，没有 `FAIL`。Windows 上需要 gcc；没有的话在 Linux、WSL 或 CI 里跑，不能跳过。

**4. 真实进程在竞态检测下验收**

```
go build -race -o bin/topic ./example/topic_service
go build -race -o bin/gateway ./example/websocket_service
```

然后设置环境变量 `GORACE=log_path=<某个空目录>/r halt_on_error=0`，再执行：

```
go run ./cmd/livecheck -mode all -topic <绝对路径>/bin/topic -gateway <绝对路径>/bin/gateway
```

要求输出 `NW04 PASS` 和 `LG PASS`，并且 `GORACE` 指定的目录里**没有生成任何文件**。有文件就说明进程里报了竞态，把文件内容附上。

`-topic` 和 `-gateway` 必须是绝对路径，`livecheck` 会切换子进程的工作目录。

**5. 用例数量**

`go test -list . ./...` 列出的用例数不少于修改前（修改前是 142 个，含 1 个模糊测试、2 个基准测试）。

**6. 提交**

改动已经提交，`git status --short` 里没有未提交的 `.go` 文件。把提交号和上面四条命令的输出一起交回来。

## 交回之后

满足以上六条后，由评审方重新取代码独立跑一遍第 3、4 条。结果一致就可以打 `v0.6.0-rc.1`。正式版 `v0.6.0` 另需完成 [release-test-plan.md](release-test-plan.md) 里的长时间运行和灰度。
