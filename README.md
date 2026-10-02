# VaultGrid

这是一个面向存储与数据管理的块/对象存储与卷管理控制面。长期目标是提供存储池与卷生命周期、S3 风格对象接口、纠删码与多副本放置、一致性哈希与再平衡、快照与克隆、完整性自愈和配额，把存储控制面沉淀为可复用服务。

仓库采用 Go，当前冻结基线只提供进程健康检查。后续能力必须通过独立题目逐步实现；每个题目都应定义可观察的公共行为、兼容边界和失败语义，不得依赖未公开内部 API。

## 启动

```bash
go run ./cmd/vaultgrid
```

服务默认监听 `127.0.0.1:8080`。可通过 `VAULTGRID_ADDR` 修改监听地址。`GET /healthz` 返回 JSON 健康状态。

## 存储池与容量预留

- `POST /v1/storage-pools` 创建存储池，请求体 `{"id":"...","devices":[{"id":"...","capacityBytes":正整数,"faultDomain":"..."}]}`。id 限 1–64 个字母、数字、点、下划线或连字符；设备 id 不得重复或跨池占用。成功返回 201 及池表示（含 `rawCapacityBytes`、`allocatedBytes`、`availableBytes`）。
- `GET /v1/storage-pools` 返回按池 id 升序的 `{"items":[]}`；`GET /v1/storage-pools/{id}` 返回单个池。
- `POST /v1/storage-pools/{id}/reservations` 以 `{"requestId":"...","bytes":正整数}` 预留容量。相同 `requestId` 与 `bytes` 重试返回 200 且不重复计量；`bytes` 不同返回 409 `idempotency_conflict`；容量不足返回 409 `insufficient_capacity`。
- `DELETE /v1/storage-pools/{id}/reservations/{requestId}` 释放预留，重复删除仍返回 204。
- `DELETE /v1/storage-pools/{id}` 删除无预留的池（204），否则返回 409 `pool_not_empty`。

错误体统一为 `{"error":{"code":"..."}}`；状态仅存于内存，重启后清空。

## 验证

```bash
go test ./...
```

当前基线刻意不包含卷生命周期、放置策略与完整性自愈的实现，以便后续任务从已冻结事实出发独立设计并验证这些能力。
