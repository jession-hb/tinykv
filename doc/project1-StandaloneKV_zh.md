# Project1 StandaloneKV（中文版）

在本项目中，你将构建一个支持列族（Column Family）的单机版键值存储 [gRPC](https://grpc.io/docs/guides/) 服务。单机版意味着只有一个节点，不是分布式系统。列族（CF）类似于键的命名空间，即同一个键在不同列族下的值可以不同。你可以简单地把多个列族看作是独立的小型数据库。列族的支持主要用于后续的事务模型（见 project4），届时你会明白 TinyKV 为什么需要 CF。

该服务支持四个基本操作：Put/Delete/Get/Scan。它维护一个简单的键值对数据库，键和值均为字符串。`Put` 用于替换指定 CF 下某个键的值，`Delete` 用于删除指定 CF 下某个键的值，`Get` 用于获取指定 CF 下某个键的当前值，`Scan` 用于批量获取指定 CF 下一系列键的当前值。

项目分为两个步骤：

1. 实现单机存储引擎。
2. 实现原始键值服务处理器。

### 代码说明

`gRPC` 服务器在 `kv/main.go` 初始化，包含一个 `tinykv.Server`，它提供名为 `TinyKv` 的 gRPC 服务。服务接口由 [protocol-buffer](https://developers.google.com/protocol-buffers) 在 `proto/proto/tinykvpb.proto` 定义，具体的 rpc 请求和响应定义在 `proto/proto/kvrpcpb.proto`。

通常你无需修改 proto 文件，因为所有必要字段都已定义。如果确实需要修改，可以编辑 proto 文件并运行 `make proto`，以更新相关的 go 代码（在 `proto/pkg/xxx/xxx.pb.go`）。

此外，`Server` 依赖于一个 `Storage`，这是你需要在 `kv/storage/standalone_storage/standalone_storage.go` 实现的单机存储引擎接口。实现好 `Storage` 接口后，就可以用它来实现 `Server` 的原始键值服务。

#### 实现单机存储引擎

第一步是实现 [badger](https://github.com/dgraph-io/badger) 键值 API 的封装。gRPC 服务依赖于 `Storage` 接口（定义在 `kv/storage/storage.go`），本质上就是对 badger 的封装，主要有两个方法：

```go
type Storage interface {
    // 其他内容
    Write(ctx *kvrpcpb.Context, batch []Modify) error
    Reader(ctx *kvrpcpb.Context) (StorageReader, error)
}
```

`Write` 用于批量修改内部状态（即 badger 实例）。

`Reader` 返回一个 `StorageReader`，支持快照上的点查和扫描操作。

你暂时不用关心 `kvrpcpb.Context`，它在后续项目中才会用到。

> 提示：
> - 建议用 [badger.Txn](https://godoc.org/github.com/dgraph-io/badger#Txn) 实现 `Reader`，因为 badger 的事务可以提供一致性快照。
> - badger 不原生支持列族，`engine_util` 包（`kv/util/engine_util`）通过给键加前缀模拟列族。例如，属于某个 CF 的键 `key` 实际存储为 `${cf}_${key}`。所有读写操作都建议通过 `engine_util` 提供的方法完成，详细请阅读 `util/engine_util/doc.go`。
> - TinyKV 使用的是 `badger` 的分支版本，请用 `github.com/Connor1996/badger`。
> - 别忘了在使用完 `badger.Txn` 后调用 `Discard()`，并在丢弃前关闭所有迭代器。

#### 实现服务处理器

最后一步是用你实现的存储引擎完成原始键值服务处理器，包括 RawGet/RawScan/RawPut/RawDelete。处理器已为你定义好，只需在 `kv/server/raw_api.go` 补全实现。完成后，运行 `make project1` 以通过测试。
