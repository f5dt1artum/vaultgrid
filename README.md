# VaultGrid

这是一个面向存储与数据管理的块/对象存储与卷管理控制面。长期目标是提供存储池与卷生命周期、S3 风格对象接口、纠删码与多副本放置、一致性哈希与再平衡、快照与克隆、完整性自愈和配额，把存储控制面沉淀为可复用服务。

仓库采用 Go。当前提供进程健康检查、存储池目录、池级容量预留、卷生命周期与单节点独占绑定、内存态卷快照与基于快照的跨池克隆、S3 风格的桶与对象接口，以及池级容量报表导出；每个能力都定义可观察的公共行为、兼容边界和失败语义，不依赖未公开内部 API。所有状态保存在内存中，进程重启后清空。

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

`DELETE /v1/storage-pools/{id}`：池无预留时返回 `204` 并释放其设备；池仍有预留时返回 `409 pool_not_empty`。

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

桶是绑定到某个池的对象命名空间，桶 id 遵循前述 id 规则。对象字节与预留、卷、快照共享同一池容量预算，计入 `allocatedBytes`。

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
- 池不存在返回 `404 pool_not_found`。

### 列出、查询与删除

- `GET /v1/buckets` 返回 `{"items":[...]}`，按桶 id 升序。
- `GET /v1/buckets/{id}` 返回单个桶；不存在返回 `404 bucket_not_found`。
- `DELETE /v1/buckets/{id}`：空桶返回 `204`；仍有对象返回 `409 bucket_not_empty`；不存在返回 `404 bucket_not_found`。

只要池中仍存在桶（即使为空），删除池仍返回 `409 pool_not_empty`。

### 写入对象

`PUT /v1/buckets/{bucketId}/objects/{key}` 以原始请求正文为内容。key 为 `objects/` 后余下路径一次 URL 解码的结果，允许斜杠，须为 1 至 1024 个 UTF-8 字节且无控制字符。内容可为空。`X-Vaultgrid-Meta-*` 请求头作为元数据保存，覆盖写入时整体替换。

成功响应带 `ETag` 头，正文为：

```json
{"key": "a/b.txt", "sizeBytes": 3, "metadata": {"Author": "me"}, "etag": "\"...\""}
```

`etag` 为内容 SHA-256 的带双引号小写十六进制。首次写入返回 `201`，覆盖返回 `200`；覆盖只按大小差额调整池占用。容量不足返回 `409 insufficient_capacity`，旧对象及元数据不变。

支持条件写入：`If-Match: <当前ETag>` 或 `If-None-Match: *`（仅当对象不存在时写入）。条件不满足返回 `412 precondition_failed`；两者并用或其他形式（如 `If-Match: *`、`If-None-Match: <etag>`）返回 `400 invalid_request`。

### 读取、列举与删除对象

- `GET /v1/buckets/{bucketId}/objects/{key}` 返回原始字节，带 `ETag` 与 `X-Vaultgrid-Meta-*` 头；对象不存在返回 `404 object_not_found`。
- `HEAD` 同上但无正文。
- `GET /v1/buckets/{bucketId}/objects` 返回 `{"items":[...]}`，按 key 升序的对象摘要；只接受一个 `prefix` 查询参数，其他或重复参数返回 `400 invalid_request`。
- `DELETE /v1/buckets/{bucketId}/objects/{key}` 返回 `204` 并释放容量；删除不存在（含从未存在）的对象仍为 `204`；桶不存在返回 `404 bucket_not_found`。

非法 key、元数据、查询参数、未知字段或格式错误返回 `400 invalid_request`。并发写删不会超配、重扣或残留部分状态。

## 容量报表

`GET /v1/capacity-report` 导出池级容量报表，只接受两个可选查询参数：

- `format`：`json`（缺省）或 `csv`。
- `poolId`：缺省时统计全部池，指定时只统计该池。

JSON 响应为 `{"summary":{...},"items":[...]}`，`items` 按池 id 升序。每个池项包含：

```json
{"poolId":"pool-a","rawCapacityBytes":1500,"reservationBytes":100,"volumeBytes":200,"snapshotBytes":200,"objectBytes":50,"allocatedBytes":550,"availableBytes":950}
```

`summary` 包含除 `poolId` 外的同名字段，为各池汇总；无池时汇总均为零且 `items` 为空，筛选单池时汇总等于该池数值。四类明细沿用现有归属：克隆按卷计入目标池，快照计入源池，空桶不计量。每个池及汇总都满足 `allocatedBytes` 等于四类占用之和、`availableBytes` 等于 `rawCapacityBytes` 减去 `allocatedBytes`；并发变更时整份报表对应单一状态。

CSV 响应为 UTF-8、`text/csv`，表头固定为：

```
poolId,rawCapacityBytes,reservationBytes,volumeBytes,snapshotBytes,objectBytes,allocatedBytes,availableBytes
```

只输出同序池行（不含汇总行），无池时仅输出表头，数字为十进制整数，与 JSON 口径一致。

`poolId` 不符合 id 规则、`format` 不是 `json` 或 `csv`、参数重复、出现未知参数或参数无值时返回 `400 invalid_request`；合法但不存在的 `poolId` 返回 `404 pool_not_found`。该路径仅允许 `GET`，其他方法返回 `405 method_not_allowed` 且带 `Allow: GET`；额外子路径返回 `404 not_found`。报表只读内存、无副作用，重启后遵循现有清空语义。

## 错误与路由

错误响应统一为：

```json
{"error":{"code":"..."}}
```

| HTTP | code | 场景 |
| --- | --- | --- |
| 400 | `invalid_request` | 未知字段、空设备集、非法 id、容量或版本非正/非整数、非法 key/元数据/查询参数等，且不产生部分状态 |
| 404 | `pool_not_found` | 查询、预留或删除不存在的池，向不存在的目标池克隆、建桶，或按不存在的池导出报表 |
| 404 | `volume_not_found` | 查询、绑定、解绑或删除不存在的卷，或为不存在的卷创建快照 |
| 404 | `snapshot_not_found` | 查询、删除不存在的快照，或基于不存在的快照克隆 |
| 404 | `bucket_not_found` | 查询、删除不存在的桶，或向不存在的桶读写对象 |
| 404 | `object_not_found` | 读取不存在的对象 |
| 404 | `not_found` | 未知路径 |
| 405 | `method_not_allowed` | 不支持的方法，响应带正确的 `Allow` 头 |
| 409 | `pool_exists` | 池 id 已存在 |
| 409 | `device_in_use` | 设备已被其他池占用 |
| 409 | `pool_not_empty` | 删除仍有预留、卷、快照或桶的池 |
| 409 | `idempotency_conflict` | 同 requestId 重试但 bytes 不同 |
| 409 | `insufficient_capacity` | 预留、建卷、快照、克隆或写对象超过池可用容量 |
| 409 | `volume_exists` | 同卷 id 重试但参数不同，或克隆 id 已被其他创建/快照占用 |
| 409 | `volume_already_bound` | 已绑定卷试图改绑其他节点 |
| 409 | `volume_in_use` | 删除或快照仍绑定在节点上的卷 |
| 409 | `stale_generation` | 绑定/解绑或创建快照时 expectedGeneration 与当前版本不符 |
| 409 | `snapshot_exists` | 同快照 id 重试但源卷或源版本不同 |
| 409 | `bucket_exists` | 同桶 id 重试但 poolId 不同 |
| 409 | `bucket_not_empty` | 删除仍有对象的桶 |
| 412 | `precondition_failed` | 条件写入的 If-Match / If-None-Match 不满足 |

## 验证

```bash
go test ./...
```
