# Project2 RaftKV（中文版）

Raft 是一种易于理解的共识算法。你可以在 [Raft 官网](https://raft.github.io/) 阅读相关资料，包括交互式可视化和 [Raft 论文](https://raft.github.io/raft.pdf)。

本项目将基于 Raft 实现高可用的 KV 服务，你不仅需要实现 Raft 算法，还要将其实际应用，并面临如持久化状态管理、快照消息流控等更多挑战。

项目分为三部分：
- 实现基础 Raft 算法
- 基于 Raft 构建容错 KV 服务
- 增加 raftlog GC 和快照支持

## Part A

### 代码说明

本部分你将实现 Raft 算法的基础部分。相关代码在 `raft/` 目录下，包括骨架代码和测试用例。Raft 算法与上层应用有良好接口，并采用逻辑时钟（tick）驱动选举和心跳超时。注意 Raft 模块本身不设置定时器，由上层应用通过 `RawNode.Tick()` 驱动逻辑时钟。消息收发等操作也是异步处理，由上层应用决定何时执行。

实现前请先阅读本部分提示，并粗略了解 `proto/proto/eraftpb.proto`，其中定义了 Raft 消息和相关结构体。与 Raft 论文不同，TinyKV 将 Heartbeat 和 AppendEntries 拆分为不同消息以简化逻辑。

本部分分为三步：
- 领导者选举
- 日志复制
- RawNode 接口

### 实现 Raft 算法

`raft.Raft`（在 `raft/raft.go`）是 Raft 算法核心，包括消息处理、逻辑时钟驱动等。更多实现指南请查阅 `raft/doc.go`，其中有设计概述和各类 `MessageType` 说明。

#### 领导者选举

建议从 `raft.Raft.tick()` 入手，该方法用于推进内部逻辑时钟，驱动选举或心跳超时。消息收发逻辑暂时不用关心，发送消息时只需将其加入 `raft.Raft.msgs`，所有收到的消息通过 `raft.Raft.Step()` 处理。测试代码会从 `msgs` 获取消息，并通过 `Step()` 传递响应。`Step()` 是消息处理入口，需处理如 `MsgRequestVote`、`MsgHeartbeat` 及其响应。还需实现如 `becomeXXX` 等状态切换函数。

可运行 `make project2aa` 测试实现。

#### 日志复制

建议从处理 `MsgAppend` 和 `MsgAppendResponse` 入手，需在发送端和接收端都处理。`raft.RaftLog`（在 `raft/log.go`）是日志管理辅助结构体，需要与上层应用通过 `Storage` 接口（定义在 `raft/storage.go`）交互以获取持久化数据。

可运行 `make project2ab` 测试实现。

### 实现 RawNode 接口

`raft.RawNode`（在 `raft/rawnode.go`）是与上层应用交互的接口，包含 `raft.Raft` 并提供如 `Tick()`、`Step()` 等包装函数，还提供 `Propose()` 用于提交新日志。

另一个重要结构体 `Ready` 也在此定义。处理消息或推进逻辑时钟时，Raft 可能需要与上层应用交互，如：
- 发送消息到其他节点
- 持久化日志条目
- 持久化硬状态（term、commit index、vote）
- 应用已提交日志到状态机

但这些交互不会立即发生，而是封装在 `Ready` 中，由 `RawNode.Ready()` 返回给上层应用。上层应用决定何时调用 `Ready()` 并处理，处理后需调用如 `Advance()` 更新 Raft 内部状态。

可运行 `make project2ac` 测试实现，`make project2a` 测试整个 Part A。

> 提示：
> - 可在 `raft.Raft`、`raft.RaftLog`、`raft.RawNode` 及 `eraftpb.proto` 的消息中添加所需状态
> - 测试假定首次启动 raft 时 term 为 0
> - 新当选 leader 应在其任期追加一个 noop entry
> - leader 提升 commit index 后应通过 `MessageType_MsgAppend` 广播 commit index
> - 测试不会为本地消息设置 term，如 `MsgHup`、`MsgBeat`、`MsgPropose`
> - leader 和非 leader 日志追加逻辑不同，需分别处理
> - 选举超时时间应在不同节点间不同
> - `rawnode.go` 中部分包装函数可用 `raft.Step(local message)` 实现
> - 新建 raft 时应从 `Storage` 获取最后的持久化状态初始化

## Part B

本部分将基于 Part A 的 Raft 模块构建容错 KV 服务。你的 KV 服务将成为一个复制状态机，由多个 KV 服务器组成，使用 Raft 进行数据复制。只要大多数服务器存活并可通信，服务就能持续处理客户端请求。

在 project1 中你已实现单机 KV 服务，应已熟悉 KV 服务 API 和 `Storage` 接口。

在介绍代码前，需理解三个术语：`Store`、`Peer` 和 `Region`，定义在 `proto/proto/metapb.proto`。
- Store：TinyKV 服务器实例
- Peer：运行在 Store 上的 Raft 节点
- Region：Peer 的集合，也称 Raft group

![region](imgs/region.png)

为简化，project2 只有一个 Store 上一个 Peer，一个集群只有一个 Region，无需考虑 Region 范围，多个 Region 在 project3 引入。

### 代码说明

首先看 `RaftStorage`（在 `kv/storage/raft_storage/raft_server.go`），它也实现了 `Storage` 接口。与 `StandaloneStorage` 直接读写底层引擎不同，`RaftStorage` 所有读写请求都先发给 Raft，待 Raft 提交后再实际读写底层引擎，从而保证多个 Store 间一致性。

`RaftStorage` 创建 `Raftstore` 驱动 Raft。调用 `Reader` 或 `Write` 时，实际是通过 channel（`raftCh`）发送 `RaftCmdRequest`（定义在 `proto/proto/raft_cmdpb.proto`，含四种基本命令类型：Get/Put/Delete/Snap）到 raftstore，待 Raft 提交并应用后返回响应。`kvrpc.Context` 参数现在有用，携带客户端视角的 Region 信息，作为 `RaftCmdRequest` 的 header。信息可能不正确或过时，raftstore 需检查并决定是否提交请求。

TinyKV 的核心 raftstore 结构较复杂，建议阅读 TiKV 参考文档：
- <https://pingcap.com/blog-cn/the-design-and-implementation-of-multi-raft/#raftstore>（中文）
- <https://pingcap.com/blog/design-and-implementation-of-multi-raft/#raftstore>（英文）

raftstore 入口为 `Raftstore`（见 `kv/raftstore/raftstore.go`），启动多个 worker 异步处理任务，大部分暂不使用，只需关注 `raftWorker`（见 `kv/raftstore/raft_worker.go`）。

整个流程分两部分：raft worker 轮询 `raftCh` 获取消息（包括 tick 驱动 Raft 和 Raft 命令），处理 ready，包括发送 raft 消息、持久化状态、应用已提交条目到状态机，应用后返回响应给客户端。

### 实现 PeerStorage

PeerStorage 是通过 Part A 的 `Storage` 接口与 Raft 交互的，但除了 raft 日志，还管理其他持久化元数据，保证重启后状态机一致性。三种重要状态定义在 `proto/proto/raft_serverpb.proto`：
- RaftLocalState：存储当前 Raft 的 HardState 和最后日志索引
- RaftApplyState：存储 Raft 已应用的最后日志索引及截断日志信息
- RegionLocalState：存储 Region 信息及对应 Peer 状态。Normal 表示正常，Tombstone 表示已移除

这些状态存储在两个 badger 实例：raftdb 和 kvdb：
- raftdb 存储 raft 日志和 RaftLocalState
- kvdb 存储不同 CF 的键值数据、RegionLocalState 和 RaftApplyState，可视为 Raft 论文中的状态机

格式如下，`kv/raftstore/meta` 提供辅助函数，写入可用 `writebatch.SetMeta()`：

| Key              | KeyFormat                        | Value            | DB   |
| :--------------- | :------------------------------- | :--------------- | :--- |
| raft_log_key     | 0x01 0x02 region_id 0x01 log_idx | Entry            | raft |
| raft_state_key   | 0x01 0x02 region_id 0x02         | RaftLocalState   | raft |
| apply_state_key  | 0x01 0x02 region_id 0x03         | RaftApplyState   | kv   |
| region_state_key | 0x01 0x03 region_id 0x01         | RegionLocalState | kv   |

> TinyKV 用两个 badger 实例只是为了与 TiKV 设计一致，实际上可用一个。

PeerStorage 创建时见 `kv/raftstore/peer_storage.go`，初始化 RaftLocalState、RaftApplyState，或重启时从底层引擎获取。注意 RAFT_INIT_LOG_TERM 和 RAFT_INIT_LOG_INDEX 都为 5（只要大于 1），不是 0。这样可区分被动创建的 Peer，详细见 project3b。

本部分只需实现 `PeerStorage.SaveReadyState`，用于保存 `raft.Ready` 中的数据到 badger，包括追加日志条目和保存 Raft 硬状态。

追加日志条目时，将 `raft.Ready.Entries` 全部保存到 raftdb，并删除不会被提交的旧日志，同时更新并保存 RaftLocalState。

保存硬状态也很简单，只需更新 RaftLocalState.HardState 并保存到 raftdb。

> 提示：
> - 用 `WriteBatch` 一次性保存所有状态
> - 参考 `peer_storage.go` 其他函数了解读写状态方法
> - 设置环境变量 LOG_LEVEL=debug 有助于调试，详见 [log levels](../log/log.go)

### 实现 Raft ready 处理流程

在 Part A 中你已实现 tick 驱动的 Raft 模块，现在需编写外部流程驱动它。大部分代码已在 `kv/raftstore/peer_msg_handler.go` 和 `kv/raftstore/peer.go` 实现。建议先学习代码，完成 `proposeRaftCommand` 和 `HandleRaftReady` 逻辑。以下是框架解释：

Raft `RawNode` 已用 `PeerStorage` 创建并存储在 `peer`。在 raft worker 中，`peer` 被 `peerMsgHandler` 包装，主要有 `HandleMsg` 和 `HandleRaftReady` 两个函数。

`HandleMsg` 处理从 raftCh 收到的所有消息，包括 `MsgTypeTick`（调用 `RawNode.Tick()` 驱动 Raft）、`MsgTypeRaftCmd`（包装客户端请求）、`MsgTypeRaftMessage`（Raft 节点间消息）。所有消息类型定义在 `kv/raftstore/message/msg.go`。

消息处理后，Raft 节点应有状态更新。`HandleRaftReady` 获取 Raft 的 ready 并执行相应操作，如持久化日志条目、应用已提交条目、发送 raft 消息等。

伪代码如下：

```go
for {
  select {
  case <-s.Ticker:
    Node.Tick()
  default:
    if Node.HasReady() {
      rd := Node.Ready()
      saveToStorage(rd.State, rd.Entries, rd.Snapshot)
      send(rd.Messages)
      for _, entry := range rd.CommittedEntries {
        process(entry)
      }
      s.Node.Advance(rd)
    }
}
```

完整读写流程如下：
- 客户端调用 RPC RawGet/RawPut/RawDelete/RawScan
- RPC 处理器调用 `RaftStorage` 相关方法
- `RaftStorage` 发送 Raft 命令请求到 raftstore，等待响应
- `RaftStore` 提交 Raft 命令请求为 Raft 日志
- Raft 模块追加日志并通过 `PeerStorage` 持久化
- Raft 模块提交日志
- Raft worker 处理已提交日志并通过回调返回响应
- `RaftStorage` 接收回调响应并返回给 RPC 处理器
- RPC 处理器返回响应给客户端

可运行 `make project2b` 通过所有测试。测试会运行一个模拟集群，包括多个 TinyKV 实例和模拟网络，执行读写操作并检查返回值。

注意错误处理很重要。你会发现一些错误定义在 `proto/proto/errorpb.proto`，gRPC 响应中有错误字段。实现了 `error` 接口的错误定义在 `kv/raftstore/util/error.go`，可作为函数返回值。

这些错误主要与 Region 相关，也是 `RaftCmdResponse` 的 `RaftResponseHeader` 成员。提交请求或应用命令时可能出错，此时应返回带错误的响应，错误会进一步传递到 gRPC 响应。可用 `BindRespError`（在 `kv/raftstore/cmd_resp.go`）将错误转换为 `errorpb.proto` 定义的错误。

本阶段需考虑如下错误，其他将在 project3 处理：
- ErrNotLeader：在 follower 上提交 raft 命令，提示客户端尝试其他 peer
- ErrStaleCommand：如 leader 变更导致部分日志未提交被新 leader 覆盖，客户端不知情仍等待响应，应返回此错误提示重试

> 提示：
> - `PeerStorage` 实现了 Raft 模块的 `Storage` 接口，持久化 Raft 相关状态用 `SaveReadyState()`
> - 用 `engine_util.WriteBatch` 原子性写入多项，如应用已提交条目和更新已应用索引
> - 用 `Transport` 发送 raft 消息，见 `GlobalContext`
> - 非多数节点或数据不最新时不应完成 get RPC，可将 get 操作也放入 raft 日志，或实现 Raft 论文第 8 节的只读优化
> - 应用日志条目时别忘了更新并持久化 apply state
> - 可异步应用已提交日志条目提升性能，非必须
> - 提交命令时记录回调，应用后返回回调
> - snap 命令响应需显式设置 badger Txn 到回调
> - 2A 后部分测试需多次运行排查 bug

## Part C

目前代码下，服务器长期运行会导致 Raft 日志无限增长。为此，服务器会定期检查 Raft 日志数量，超限时丢弃部分日志条目。

本部分将基于前两部分实现快照处理。快照本质上也是 raft 消息，和 AppendEntries 类似，但包含整个状态机数据，体积大，发送时会消耗大量资源和时间，可能阻塞其他 raft 消息处理。为此，快照消息采用独立连接并分块传输。TinyKV 服务有专门的快照 RPC API，详情可查 `snapRunner` 及参考 <https://pingcap.com/blog-cn/tikv-source-code-reading-10/>

### Raft 层实现

虽然快照消息处理有些不同，但在 Raft 算法视角下应无区别。`eraftpb.Snapshot` 的 `data` 字段并不代表实际状态机数据，而是一些元数据，可暂时忽略。leader 需发送快照消息时，可调用 `Storage.Snapshot()` 获取 `eraftpb.Snapshot`，然后像其他 raft 消息一样发送。状态机数据的生成和发送由 raftstore 实现，下一步介绍。只要 `Storage.Snapshot()` 成功返回，leader 就可安全发送快照消息，follower 收到后调用 `handleSnapshot` 处理，即根据 `eraftpb.SnapshotMetadata` 恢复 raft 内部状态（term、commit index、成员信息等），快照处理流程即告完成。

### raftstore 层实现

本步骤需了解 raftstore 的两个 worker：raftlog-gc worker 和 region worker。

raftstore 会定期根据配置 `RaftLogGcCountLimit` 检查是否需要 GC 日志，见 `onRaftGcLogTick()`。如需 GC，则提交 raft 管理命令 `CompactLogRequest`，包装在 `RaftCmdRequest` 中，和四种基本命令一样。该命令被 raft 提交后，需处理 admin 命令，更新 `RaftTruncatedState`（在 `RaftApplyState`）。随后调度任务给 raftlog-gc worker（用 `ScheduleCompactLog`），由其异步删除日志。

由于日志压缩，Raft 可能需发送快照。`PeerStorage` 实现了 `Storage.Snapshot()`，TinyKV 通过 region worker 生成和应用快照。调用 `Snapshot()` 时，实际是发送 `RegionTaskGen` 任务到 region worker，消息处理在 `kv/raftstore/runner/region_task.go`，扫描底层引擎生成快照，通过 channel 发送快照元数据。下次 Raft 调用 `Snapshot` 时检查快照是否生成完毕，如已完成则发送快照消息，快照的发送和接收由 `kv/storage/raft_storage/snap_runner.go` 处理。无需深入细节，只需知道快照消息最终由 `onRaftMsg` 处理。

快照会在下次 Raft ready 中反映出来，需修改 raft ready 处理流程以支持快照。应用快照时，需更新并持久化 `RaftLocalState`、`RaftApplyState`、`RegionLocalState`，并从 kvdb 和 raftdb 移除过时状态。同时更新 `PeerStorage.snapState` 为 `snap.SnapState_Applying`，通过 `PeerStorage.regionSched` 发送 `runner.RegionTaskApply` 任务到 region worker，并等待其完成。

可运行 `make project2c` 通过所有测试。
