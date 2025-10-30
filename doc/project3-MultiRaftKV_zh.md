# Project3 MultiRaftKV（中文版）

在 project2 中，你已经实现了基于 Raft 的高可用 KV 服务，做得很棒！但还不够，这样的 KV 服务只由一个 raft group 支撑，扩展性有限，每次写入都要等到提交后再逐条写入 badger，虽然保证了一致性，但也牺牲了并发性。

![multiraft](imgs/multiraft.png)

本项目将实现一个带有调度器的多 raft group KV 服务，每个 raft group 负责一段 key 范围（称为 region），如上图所示。单个 region 的请求处理方式与之前相同，但多个 region 可并发处理请求，提升性能，同时也带来如请求均衡等新挑战。

项目分为三部分：
1. 实现 Raft 算法的成员变更和领导权转移
2. 在 raftstore 上实现配置变更和 region 分裂
3. 引入调度器

## Part A

本部分将实现 Raft 算法的成员变更（conf change）和领导权转移（leader transfer），为后续两部分做准备。成员变更用于添加或移除 raft group 的 peer，会改变 quorum，需谨慎处理。领导权转移用于将 leader 转移到其他 peer，有助于负载均衡。

### 代码说明

需修改的代码主要在 `raft/raft.go` 和 `raft/rawnode.go`，新消息类型定义在 `proto/proto/eraft.proto`。成员变更和领导权转移均由上层应用触发，建议从 `raft/rawnode.go` 入手。

### 实现领导权转移

需引入两种新消息类型：`MsgTransferLeader` 和 `MsgTimeoutNow`。领导权转移时，先在当前 leader 上调用 `raft.Raft.Step` 处理 `MsgTransferLeader` 消息，leader 需检查目标 peer（transferee）是否合格（如日志是否最新）。如不合格，leader 可选择中止或帮助目标 peer（建议帮助）。如目标 peer 日志不最新，leader 应发送 `MsgAppend` 消息并暂停新提案，避免循环。目标 peer 合格后，leader 立即发送 `MsgTimeoutNow` 消息，目标 peer 收到后应立即发起新选举（term 更高且日志最新），有很大概率成为新 leader。

### 实现成员变更

本项目实现的成员变更算法不是 Raft 论文中的联合共识算法，只能逐个添加或移除 peer，更简单易懂。成员变更由 leader 调用 `raft.RawNode.ProposeConfChange` 提交一个类型为 `EntryConfChange` 的 entry，`Data` 字段为 `pb.ConfChange`。当该 entry 被提交后，需通过 `RawNode.ApplyConfChange` 应用，实际调用 `raft.Raft.addNode` 或 `raft.Raft.removeNode`。

> 提示：
> - `MsgTransferLeader` 是本地消息，不来自网络
> - `MsgTransferLeader` 的 `Message.from` 设置为目标 peer
> - 立即发起新选举可调用 `Raft.Step` 处理 `MsgHup` 消息
> - 用 `pb.ConfChange.Marshal` 获取字节数据，放入 `pb.Entry.Data`

## Part B

Raft 层支持成员变更和领导权转移后，本部分需让 TinyKV 支持这些管理命令。`proto/proto/raft_cmdpb.proto` 中有四种管理命令：
- CompactLog（已在 project2 part C 实现）
- TransferLeader
- ChangePeer
- Split

`TransferLeader` 和 `ChangePeer` 基于 Raft 层的支持，作为调度器的基本操作。`Split` 用于将一个 Region 分裂为两个，是多 raft 的基础。你将逐步实现这些功能。

### 提交领导权转移

作为 raft 命令，`TransferLeader` 以 Raft entry 提交。但实际只需调用 `RawNode` 的 `TransferLeader()` 方法，无需像普通命令那样复制到其他 peer。

### 在 raftstore 实现成员变更

成员变更有两种类型：`AddNode` 和 `RemoveNode`。执行时需了解 `RegionEpoch`，它是 `metapb.Region` 的元信息，添加/移除 peer 或分裂时会改变 epoch。`conf_ver` 在成员变更时增加，`version` 在分裂时增加，用于保证网络隔离下 region 信息的最新性。

需让 raftstore 支持成员变更命令，流程如下：
1. 通过 `ProposeConfChange` 提交管理命令
2. 日志提交后，修改 `RegionLocalState`，包括 `RegionEpoch` 和 `Peers`
3. 调用 `ApplyConfChange()`

> 提示：
> - 执行 `AddNode` 时，新 peer 由 leader 的心跳创建，见 `maybeCreatePeer()`，此时 peer 未初始化，region 信息未知，用 0 初始化其日志 term 和 index。leader 会发现该 follower 没有数据（日志从 0 到 5 有缺口），会直接发送快照。
> - 执行 `RemoveNode` 时需显式调用 `destroyPeer()` 停止 Raft 模块，销毁逻辑已提供。
> - 别忘了更新 `GlobalContext` 的 `storeMeta` 中的 region 状态
> - 测试会多次调度同一成员变更命令，需考虑如何忽略重复命令

### 在 raftstore 实现 region 分裂

![raft_group](imgs/keyspace.png)

为支持多 raft，系统需对数据分片，每个 raft group 只存储部分数据。常见分片方式有 Hash 和 Range，TinyKV 采用 Range，便于聚合同前缀的键，方便 scan 操作，分裂时也更高效。初始只有一个 Region，范围为 ["", "")，可视为整个空间。随着数据写入，分裂检查器会定期检查 region 大小，生成分裂键，将 region 一分为二，分裂键会包装为 `MsgSplitRegion`，由 `onPrepareSplitRegion()` 处理。

为保证新 Region 和 Peer 的 id 唯一，id 由调度器分配，已实现，无需你处理。`onPrepareSplitRegion()` 实际会调度 pd worker 任务向调度器申请 id，收到响应后在 `onAskSplit()`（见 `kv/raftstore/runner/scheduler_task.go`）生成分裂管理命令。

你的任务是实现分裂管理命令的处理流程，类似成员变更。多 raft 支持已在 `kv/raftstore/router.go` 实现。Region 分裂后，一个 Region 继承分裂前的元数据，只需修改 Range 和 RegionEpoch，另一个则需创建相关元信息。

> 提示：
> - 新 Region 的 Peer 需用 `createPeer()` 创建并注册到 router.regions，region 信息插入到 ctx.StoreMeta 的 regionRanges
> - 网络隔离下 region 分裂，快照可能与现有 region 范围重叠，检查逻辑在 `checkSnapshot()`（见 `kv/raftstore/peer_msg_handler.go`），实现时需注意
> - 用 `engine_util.ExceedEndKey()` 比较 region 的 end key，end key 为 "" 时，任何 key 都大于等于 ""
> - 需考虑更多错误：`ErrRegionNotFound`、`ErrKeyNotInRegion`、`ErrEpochNotMatch`

## Part C

如上所述，KV 存储中的所有数据都被分成多个 region，每个 region 有多个副本。问题来了：每个副本该放在哪里？如何选择最佳位置？谁发送 AddPeer 和 RemovePeer 命令？调度器负责这些。

调度器需了解整个集群信息，如 region 分布、键数量、大小等。为此，每个 region 需定期向调度器发送心跳请求，结构为 `RegionHeartbeatRequest`（见 `/proto/proto/schedulerpb.proto`）。调度器收到心跳后更新本地 region 信息。

同时，调度器定期检查 region 信息，发现不均衡时进行迁移。例如某 store region 太多，则将 region 移到其他 store。迁移命令作为下次心跳响应返回。

本部分需实现上述两项功能，按指导和框架操作即可。

### 收集 region 心跳

`processRegionHeartbeat` 的唯一参数是 regionInfo，包含心跳发送者的 region 信息。调度器需更新本地 region 记录，但不是每次心跳都更新。

原因有二：一是无变化可跳过，二是调度器不能信任所有心跳，尤其是集群某部分网络隔离时，部分节点信息可能错误。

例如，某些 region 分裂后重新选举，但隔离节点仍发送过时信息。对于同一 region，可能有多个节点自称 leader，调度器不能都信。

哪个更可信？调度器应比较 `conf_ver` 和 `version`（即 RegionEpoch），先比 version，再比 conf_ver，较大的为新信息。

检查流程如下：
1. 本地存储有同 id region，若心跳的 `conf_ver` 或 `version` 小于本地，则为过时 region
2. 若无，则扫描所有与其重叠的 region，心跳的 `conf_ver` 和 `version` 应大于等于所有重叠 region，否则为过时 region

调度器如何判断可跳过更新？可列举一些简单条件：
* 新的 version 或 conf_ver 大于原有则不可跳过
* leader 变化不可跳过
* 新或原有有 pending peer 不可跳过
* ApproximateSize 变化不可跳过
* ...

无需严格穷举，冗余更新不影响正确性。

如需更新本地存储，需更新 region tree 和 store 状态。用 `RaftCluster.core.PutRegion` 更新 region tree，用 `RaftCluster.core.UpdateStoreStatus` 更新 store 状态（如 leader 数、region 数、pending peer 数等）。

### 实现 region balance scheduler

调度器可运行多种类型，如 balance-region、balance-leader。本项目关注 balance-region。

每个调度器需实现 Scheduler 接口（见 `/scheduler/server/schedule/scheduler.go`）。Scheduler 用 `GetMinInterval` 返回默认运行间隔，若多次返回 null，则用 `GetNextInterval` 增加间隔。`Schedule` 方法返回 operator，调度器会在下次相关 region 心跳响应时分发。

Scheduler 的核心是 `Schedule` 方法，返回值为 Operator，包含多步操作如 AddPeer、RemovePeer。例如，调度器尝试将第一个 raft group 的 peer 从第三个 store 移到第四个，先 AddPeer，再判断是否需 transferLeader，最后 RemovePeer。

可用 `CreateMovePeerOperator`（在 `scheduler/server/schedule/operator`）创建 MovePeer operator。

![balance](imgs/balance1.png)

![balance](imgs/balance2.png)

本部分只需实现 `scheduler/server/schedulers/balance_region.go` 的 `Schedule` 方法。该调度器避免某 store region 过多，先选出所有合适的 store，按 region size 排序，尝试从 region size 最大的 store 迁移 region。

调度器会优先选 pending region（可能磁盘负载过高），无则选 follower region，再无则选 leader region，最终选出要迁移的 region，否则尝试下一个 store，直到所有 store 都尝试过。

选出要迁移的 region 后，调度器会选 region size 最小的 store 作为目标。判断迁移是否有价值，需比较原 store 和目标 store 的 region size 差值，若足够大则分配新 peer 并创建迁移 operator。

上述流程只是大致过程，实际还有很多细节：
* 哪些 store 合适迁移？需 up 且 down 时间不超过 `MaxStoreDownTime`（见 `cluster.GetMaxStoreDownTime()`）
* 如何选 region？Scheduler 框架提供 `GetPendingRegionsWithLock`、`GetFollowersWithLock`、`GetLeadersWithLock` 三种方法
* 如何判断迁移有价值？需保证迁移后目标 store 的 region size 仍小于原 store，差值需大于两倍 region 的 approximate size
