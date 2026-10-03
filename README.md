# VaultGrid

这是一个面向存储与数据管理的块/对象存储与卷管理控制面。长期目标是提供存储池与卷生命周期、S3 风格对象接口、纠删码与多副本放置、一致性哈希与再平衡、快照与克隆、完整性自愈和配额，把存储控制面沉淀为可复用服务。

仓库采用 Go。当前提供进程健康检查、存储池目录、池级容量预留、卷生命周期与单节点独占绑定、内存态卷快照与基于快照的跨池克隆，以及 S3 风格的桶与对象 HTTP 接口；每个能力都定义可观察的公共行为、兼容边界和失败语义，不依赖未公开内部 API。所有状态保存在内存中，进程重启后清空。

## 启动

```bash
go run ./cmd/vaultgrid
```

服务默认监听 `127.0.0.1:8080`。可通过 `VAULTGRID_ADDR` 修改监听地址。

## 健康检查

`GET /healthz` 返回 JSON 健康状态。

## 存储池

所有 id（池 id、设备 id、requestId）限 1 至 64 个字母、数字、点、下划线或连字符（`A-Za-z0-9._-`）。设备 id 在所有池之间全局唯一。

### 创建池

`POST /v1/storage-pools`

```json
{
  "id": "pool-a",
  "devices": [
    {"id": "dev1", "capacityBytes": 1000, "faultDomain": "rack1"},
    {"id": "dev2", "capacityBytes": 500, "faultDomain": "rack2"}
  ]
}
```

`devices` 必须非空；`capacityBytes` 为正整数（JSON 整数），`faultDomain` 非空，同一请求内设备不得重复。成功返回 `201`：

```json
{
  "id": "pool-a",
  "devices": [ ... ],
  "rawCapacityBytes": 1500,
  "allocatedBytes": 0,
  "availableBytes": 1500
}
```

### 列出与查询

- `GET /v1/storage-pools` 返回 `{"items":[...]}`，按池 id 升序。
- `GET /v1/storage-pools/{id}` 返回单个池。

### 删除池

`DELETE /v1/storage-pools/{id}`：池无预留、卷、快照与桶时返回 `204` 并释放其设备；否则返回 `409 pool_not_empty`。

## 容量预留

### 创建预留（幂等）

`POST /v1/storage-pools/{id}/reservations`

```json
{"requestId": "req-1", "bytes": 600}
```

`bytes` 为正整数。同池内按 `requestId` 幂等：

- 首次成功返回 `201` 及预留结果，计入 `allocatedBytes`。
- 相同 `requestId` 与 `bytes` 重试返回 `200` 及原结果，不重复计量。
- 相同 `requestId` 但 `bytes` 不同返回 `409 idempotency_conflict`。
- 容量不足返回 `409 insufficient_capacity`；并发成功预留的总量不会超过 `rawCapacityBytes`。

### 删除预留

`DELETE /v1/storage-pools/{id}/reservations/{requestId}` 返回 `204` 并立即释放容量；重复删除（含从未存在的预留）仍返回 `204`。

## 卷

卷是池内的容量占用，卷 id、池 id、节点 id 均遵循前述 id 规则。卷的 `sizeBytes` 直接计入所属池的 `allocatedBytes`（与显式预留共享同一容量预算）。

### 创建卷（幂等）

`POST /v1/volumes`

```json
{"id": "vol-1", "poolId": "pool-a", "sizeBytes": 600}
```

`sizeBytes` 为正 JSON 整数。成功返回 `201`：

```json
{"id": "vol-1", "poolId": "pool-a", "sizeBytes": 600, "generation": 0, "binding": null}
```

- 相同 `id`、`poolId`、`sizeBytes` 重试返回 `200` 及当前卷结果，不重复计量。
- 相同 `id` 但参数不同返回 `409 volume_exists`。
- 池不存在返回 `404 pool_not_found`。
- 容量不足返回 `409 insufficient_capacity`；并发创建绝不会使池超配，失败不遗留卷或占用。

### 列出与查询

- `GET /v1/volumes` 返回 `{"items":[...]}`，按卷 id 升序。
- `GET /v1/volumes/{id}` 返回单个卷；不存在返回 `404 volume_not_found`。

### 绑定与解绑

`PUT /v1/volumes/{id}/binding`

```json
{"nodeId": "node-1", "expectedGeneration": 0}
```

`expectedGeneration` 为非负 JSON 整数且必须等于卷当前的 `generation`：

- 未绑定卷：返回 `200`，`binding` 记录 `nodeId`，`generation` 加一。
- 已绑定到同一节点且版本匹配：返回 `200` 当前结果，`generation` 不变。
- 试图改绑其他节点：`409 volume_already_bound`。
- 版本不符：`409 stale_generation`；同版本并发请求只有一个能改变状态。

`DELETE /v1/volumes/{id}/binding?expectedGeneration=<n>`：

- 版本错误返回 `409 stale_generation`。
- 已绑定时返回 `204` 并把 `generation` 加一。
- 原本未绑定时返回 `204`，`generation` 不变。
- 缺少、重复、非整数或额外查询参数返回 `400 invalid_request`。

### 删除卷

`DELETE /v1/volumes/{id}`：

- 未绑定卷返回 `204` 并归还容量。
- 绑定中的卷返回 `409 volume_in_use`。
- 不存在返回 `404 volume_not_found`。

卷存在时删除所属池仍返回 `409 pool_not_empty`。

## 快照

快照是卷在某一代的内存态副本：创建后即与源卷解耦，源卷后续变化或删除都不影响快照。快照按 `sizeBytes` 计入**源池**的 `allocatedBytes`（与卷、预留共享同一容量预算），直到快照被删除。快照 id、卷 id 均遵循前述 id 规则。

### 创建快照（幂等）

`POST /v1/volumes/{volumeId}/snapshots`

```json
{"id": "snap-1", "expectedGeneration": 0}
```

`expectedGeneration` 为非负 JSON 整数且必须等于源卷当前的 `generation`，且源卷必须未绑定。成功返回 `201`：

```json
{"id":"snap-1","sourceVolumeId":"vol-1","poolId":"pool-a","sizeBytes":600,"sourceGeneration":0}
```

- 相同快照 `id`、相同源卷、相同源版本的重试返回 `200` 及原结果，不重复计量；源卷在此之后被删除，重试仍返回 `200`。
- 相同快照 `id` 但源卷或源版本不同返回 `409 snapshot_exists`。
- 源卷不存在返回 `404 volume_not_found`；源卷已绑定返回 `409 volume_in_use`。
- 版本不符返回 `409 stale_generation`。
- 容量不足返回 `409 insufficient_capacity`，且不遗留快照或占用。
- 非法 id、缺失/非整数/负数的 `expectedGeneration`、未知字段等返回 `400 invalid_request`。

### 列出、查询与删除

- `GET /v1/snapshots` 返回 `{"items":[...]}`，按快照 id 升序。
- `GET /v1/snapshots/{id}` 返回单个快照；不存在返回 `404 snapshot_not_found`。
- `DELETE /v1/snapshots/{id}` 返回 `204` 并立即向源池归还容量；不存在返回 `404 snapshot_not_found`。

只要池中仍存在快照（即使源卷已删除），删除池仍返回 `409 pool_not_empty`。

## 克隆

`POST /v1/snapshots/{id}/clones`

```json
{"id": "vol-copy", "poolId": "pool-b"}
```

基于快照创建一个独立卷：`sizeBytes` 与快照相同，`generation` 为 `0`，`binding` 为 `null`。克隆卷就是普通卷，可正常绑定、解绑、列卷与删除；容量计入**目标池**，允许跨池克隆。

- 同一快照、相同卷 `id`、相同目标池的重试返回 `200` 及当前卷结果，不重复占用。
- 卷 `id` 已被其他创建（直接建卷或来自其他快照的克隆）占用，或重试指向不同目标池时返回 `409 volume_exists`。
- 快照不存在返回 `404 snapshot_not_found`；目标池不存在返回 `404 pool_not_found`。
- 目标池容量不足返回 `409 insufficient_capacity`，失败不改变状态。
- 非法 id、缺失字段、未知字段等返回 `400 invalid_request`。

并发创建快照或克隆都不会重复计量或使池超配。

## 桶与对象

桶是对象的扁平命名空间，挂在某个池下；桶 id 沿用前述 id 规则。对象字节与预留、卷、快照共享同一池容量预算，计入 `allocatedBytes`。

### 创建桶（幂等）

`POST /v1/buckets`

```json
{"id": "bucket-1", "poolId": "pool-a"}
```

成功返回 `201`：

```json
{"id": "bucket-1", "poolId": "pool-a", "objectCount": 0, "bytesUsed": 0}
```

- 相同 `id`、`poolId` 重试返回 `200` 及当前桶结果。
- 相同 `id` 但 `poolId` 不同返回 `409 bucket_exists`。
- 池不存在返回 `404 pool_not_found`；非法 id、未知字段等返回 `400 invalid_request`。

### 列出、查询与删除

- `GET /v1/buckets` 返回 `{"items":[...]}`，按桶 id 升序。
- `GET /v1/buckets/{id}` 返回单个桶；不存在返回 `404 bucket_not_found`。
- `DELETE /v1/buckets/{id}`：空桶返回 `204`；非空返回 `409 bucket_not_empty`；不存在返回 `404 bucket_not_found`。

只要池下仍存在桶（即使为空），删除池返回 `409 pool_not_empty`。

### 写入对象

`PUT /v1/buckets/{bucketId}/objects/{key}`，请求正文即为对象原始内容（可为空）。

key 是 `objects/` 之后余下路径做一次 URL 解码的结果，允许包含斜杠；须为 1 至 1024 个 UTF-8 字节且不含控制字符，否则返回 `400 invalid_request`。

`X-Vaultgrid-Meta-*` 请求头作为对象元数据保存（键为头部后缀的小写形式）；覆盖写入时元数据整体替换。成功响应：

- 首次写入返回 `201`，覆盖返回 `200`。
- 响应体为 `{"key","sizeBytes","metadata","etag"}`，响应头同时带 `ETag`；`etag` 为内容 SHA-256 的小写十六进制并带双引号。
- 覆盖只按大小差额调整池占用；容量不足返回 `409 insufficient_capacity`，旧对象及其元数据保持不变。
- 桶不存在返回 `404 bucket_not_found`；非法 key、元数据或查询参数返回 `400 invalid_request`。

条件写入：

- `If-Match: "<etag>"`：仅当对象存在且当前 ETag 相同才写入。
- `If-None-Match: *`：仅当对象不存在才写入。
- 条件不满足返回 `412 precondition_failed`；两个头同时使用或任何其他形式返回 `400 invalid_request`。

### 读取、列举与删除对象

- `GET /v1/buckets/{bucketId}/objects/{key}` 返回原始字节，带 `ETag` 与 `X-Vaultgrid-Meta-*` 头；`HEAD` 返回相同头部但无正文。对象不存在返回 `404 object_not_found`。
- `GET /v1/buckets/{bucketId}/objects` 返回 `{"items":[...]}`，按 key 升序的对象摘要；唯一接受的查询参数为单个 `prefix`，其他或重复参数返回 `400 invalid_request`。
- `DELETE /v1/buckets/{bucketId}/objects/{key}` 返回 `204` 并立即释放容量；桶存在时幂等（删除不存在的对象仍为 `204`），桶不存在返回 `404 bucket_not_found`。

并发写入与删除不会使池超配、重复计量或残留部分状态。

## 错误与路由

错误响应统一为：

```json
{"error":{"code":"..."}}
```

| HTTP | code | 场景 |
| --- | --- | --- |
| 400 | `invalid_request` | 未知字段、空设备集、非法 id、容量或版本非正/非整数、非法 key/元数据/查询参数、非法条件写头等，且不产生部分状态 |
| 404 | `pool_not_found` | 查询、预留或删除不存在的池，向不存在的目标池克隆，或在不存在的池中建桶 |
| 404 | `volume_not_found` | 查询、绑定、解绑或删除不存在的卷，或为不存在的卷创建快照 |
| 404 | `snapshot_not_found` | 查询、删除不存在的快照，或基于不存在的快照克隆 |
| 404 | `bucket_not_found` | 查询、删除不存在的桶，或在不存在的桶中读写删对象 |
| 404 | `object_not_found` | 读取不存在的对象 |
| 404 | `not_found` | 未知路径 |
| 405 | `method_not_allowed` | 不支持的方法，响应带正确的 `Allow` 头 |
| 409 | `pool_exists` | 池 id 已存在 |
| 409 | `device_in_use` | 设备已被其他池占用 |
| 409 | `pool_not_empty` | 删除仍有预留、卷、快照或桶的池 |
| 409 | `idempotency_conflict` | 同 requestId 重试但 bytes 不同 |
| 409 | `insufficient_capacity` | 预留、建卷、快照、克隆或对象写入超过池可用容量 |
| 409 | `volume_exists` | 同卷 id 重试但参数不同，或克隆 id 已被其他创建/快照占用 |
| 409 | `volume_already_bound` | 已绑定卷试图改绑其他节点 |
| 409 | `volume_in_use` | 删除或快照仍绑定在节点上的卷 |
| 409 | `stale_generation` | 绑定/解绑或创建快照时 expectedGeneration 与当前版本不符 |
| 409 | `snapshot_exists` | 同快照 id 重试但源卷或源版本不同 |
| 409 | `bucket_exists` | 同桶 id 重试但 poolId 不同 |
| 409 | `bucket_not_empty` | 删除仍含对象的桶 |
| 412 | `precondition_failed` | 条件写入的 If-Match / If-None-Match 不满足 |

## 验证

```bash
go test ./...
```
