# Project4 事务（中文版）

在前面的项目中，你已经构建了一个使用 Raft 保证多节点一致性的键值数据库。要实现真正的可扩展性，数据库必须能处理多个客户端。多个客户端同时写入同一个键时会出现问题：如果一个客户端写入后立即读取该键，是否应该期望读到刚写入的值？在 project4 中，你将通过为数据库构建事务系统来解决这些问题。

事务系统是客户端（TinySQL）与服务端（TinyKV）之间的协作协议。双方都需正确实现，才能保证事务属性。我们将有一套完整的事务 API，与之前实现的原始 API 独立（如果客户端同时使用原始和事务 API，则无法保证事务属性）。

事务承诺 [*快照隔离*（SI）](https://en.wikipedia.org/wiki/Snapshot_isolation)。即在一个事务内，客户端读取到的数据库内容如同在事务开始时被冻结（事务看到的是一致性视图）。事务要么全部写入数据库，要么全部回滚（如与其他事务冲突）。

为实现 SI，需要改变底层存储的数据方式。不是每个键只存一个值，而是为每个键和时间戳（timestamp）存储一个值，这就是多版本并发控制（MVCC），即每个键存储多个不同版本的值。

你将在 A 部分实现 MVCC，B 和 C 部分实现事务 API。

## TinyKV 的事务

TinyKV 的事务设计参考 [Percolator](https://storage.googleapis.com/pub-tools-public-publication-data/pdf/36726.pdf)，采用两阶段提交协议（2PC）。

一个事务包含一系列读写操作，拥有开始时间戳和提交时间戳（提交时间戳必须大于开始时间戳）。整个事务读取的都是开始时间戳时有效的版本。提交后，所有写入都以提交时间戳出现。若有其他事务在开始和提交时间戳之间写入同一键，则整个事务冲突并回滚。

协议流程如下：客户端先从 TinyScheduler 获取开始时间戳，在本地构建事务（读取数据库，但写入只在内存记录）。事务构建好后，客户端选一个键作为主键（primary key，与 SQL 主键无关），发送 `KvPrewrite` 消息到 TinyKV。`KvPrewrite` 包含事务的所有写入，TinyKV 尝试锁定所有相关键，若有任何键锁定失败，则事务失败，客户端可稍后重试（即用不同开始时间戳）。全部锁定成功则预写成功，每个锁记录事务主键和 TTL。

由于事务的键可能分布在多个 region、多个 Raft group，客户端会分别向各 region leader 发送 `KvPrewrite`，每次只包含该 region 的修改。全部预写成功后，客户端向主键所在 region 发送提交请求，包含提交时间戳（也从 TinyScheduler 获取）。

如有预写失败，客户端会向所有 region 发送 `KvBatchRollback`，解锁所有键并删除预写值。

TinyKV 不会自动检查 TTL，需客户端通过 `KvCheckTxnStatus` 请求发起超时检查。请求包含主键和开始时间戳，TinyKV 检查锁是否存在或已提交，若锁超时则回滚。无论如何，TinyKV 都会返回锁状态，客户端可据此发送 `KvResolveLock` 请求。客户端通常在预写因其他事务锁冲突时检查事务状态。

主键提交成功后，客户端会向其他 region 提交剩余键，这些请求应总是成功，因为预写成功即承诺后续提交必定成功。主键提交失败则回滚事务。

## Part A

你之前实现的原始 API 是直接将用户键值映射到底层存储（Badger）。由于 Badger 不支持分布式事务，需在 TinyKV 层实现事务，并对用户键值进行编码。即实现 MVCC。

MVCC 要求每个键存储所有版本的值。例如，键的值从 `10` 变为 `20`，则需存储 `10` 和 `20` 及其有效时间戳。

TinyKV 用三个列族（CF）：`default` 存用户值，`lock` 存锁，`write` 记录变更。`lock` CF 用用户键访问，存储序列化的 `Lock` 结构（见 [lock.go](/kv/transaction/mvcc/lock.go)）。`default` CF 用用户键和事务开始时间戳访问，存储用户值。`write` CF 用用户键和事务提交时间戳访问，存储 `Write` 结构（见 [write.go](/kv/transaction/mvcc/write.go)）。

用户键和时间戳组合成编码键，编码方式保证先按用户键升序，再按时间戳降序，这样迭代编码键时能优先得到最新版本。编码/解码辅助函数见 [transaction.go](/kv/transaction/mvcc/transaction.go)。

本部分需实现 `MvccTxn` 结构体。B、C 部分将用其实现事务 API。`MvccTxn` 提供基于用户键的读写操作，逻辑上处理锁、写、值。所有修改收集到 `MvccTxn`，最后一次性写入底层数据库，保证命令原子性。注意 MVCC 事务不是 TinySQL 事务，只包含单条命令的修改。

`MvccTxn` 定义在 [transaction.go](/kv/transaction/mvcc/transaction.go)，有框架和辅助函数，测试见 [transaction_test.go](/kv/transaction/mvcc/transaction_test.go)。需实现每个方法，保证所有测试通过。

> 提示：
> - `MvccTxn` 需知道请求的开始时间戳
> - 最难实现的方法可能是 `GetValue` 和获取写的方法，需用 `StorageReader` 在 CF 上迭代。注意编码键的顺序，判断值是否有效要看提交时间戳。

## Part B

本部分用 A 部分的 `MvccTxn` 实现 `KvGet`、`KvPrewrite`、`KvCommit` 请求处理。`KvGet` 在指定时间戳读取值，若被其他事务锁定则返回错误，否则查找最新有效版本。

`KvPrewrite` 和 `KvCommit` 分两阶段写入数据库，均可对多键操作，但实现时可逐键处理。

`KvPrewrite` 实际写入值，需锁定键并存储值，检查是否有其他事务锁定或写入。

`KvCommit` 不改变值，只记录已提交。若键未锁定或被其他事务锁定则失败。

需实现 [server.go](/kv/server/server.go) 中的 `KvGet`、`KvPrewrite`、`KvCommit` 方法，每个方法接收请求对象并返回响应对象，具体内容见 [kvrpcpb.proto](/proto/kvrpcpb.proto)（无需修改协议定义）。

TinyKV 可并发处理多个请求，可能出现本地竞争，如同时提交和回滚同一键。为避免竞争，可对每个键加锁（latch），类似于每键互斥锁，覆盖所有 CF。[latches.go](/kv/transaction/latches/latches.go) 定义了 `Latches` 对象及其 API。

> 提示：
> - 所有命令都属于某个事务，事务由开始时间戳标识
> - 任何请求都可能导致 region 错误，需像原始请求一样处理。大多数响应有非致命错误字段，如键被锁定，客户端可据此重试事务。

## Part C

本部分需实现 `KvScan`、`KvCheckTxnStatus`、`KvBatchRollback`、`KvResolveLock`。总体与 B 部分类似，用 `MvccTxn` 实现 gRPC 请求处理。

`KvScan` 是事务版的 `RawScan`，在单一时间点读取多个值。由于 MVCC，扫描比原始版复杂，不能直接用底层存储迭代。

`KvCheckTxnStatus`、`KvBatchRollback`、`KvResolveLock` 用于客户端遇到事务冲突时处理锁。每个都涉及锁状态变更。

`KvCheckTxnStatus` 检查超时，移除过期锁并返回锁状态。

`KvBatchRollback` 检查键是否被当前事务锁定，如是则移除锁、删除值并写入回滚标记。

`KvResolveLock` 批量检查锁，全部回滚或全部提交。

> 提示：
> - 扫描时建议实现自己的扫描器（迭代器），按逻辑值迭代而非底层原始值。`kv/transaction/mvcc/scanner.go` 提供框架。
> - 扫描时部分错误可记录在单个键上，不影响整体扫描。其他命令如遇单键错误则整体失败。
> - `KvResolveLock` 实际就是批量回滚或提交，可复用 `KvBatchRollback` 和 `KvCommit` 的实现。
> - 时间戳由物理和逻辑部分组成，通常比较整个时间戳，但计算超时时只用物理部分。可用 [transaction.go](/kv/transaction/mvcc/transaction.go) 的 `PhysicalTime` 辅助函数。
