# VaultGrid

这是一个面向存储与数据管理的块/对象存储与卷管理控制面。长期目标是提供存储池与卷生命周期、S3 风格对象接口、纠删码与多副本放置、一致性哈希与再平衡、快照与克隆、完整性自愈和配额，把存储控制面沉淀为可复用服务。

仓库采用 Go。当前提供进程健康检查、存储池目录、池级容量预留、卷生命周期与单节点独占绑定，以及内存态卷快照与克隆；每个能力都定义可观察的公共行为、兼容边界和失败语义，不依赖未公开内部 API。所有状态保存在内存中，进程重启后清空。

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

快照是卷在某一代的不可变内存副本，快照 id 遵循前述 id 规则且全局唯一。快照独立占用**源池**中与卷 `sizeBytes` 相等的容量（与卷、预留共享同一容量预算），且不受源卷后续绑定变化或删除影响。

### 创建快照（幂等）

`POST /v1/volumes/{volumeId}/snapshots`

```json
{"id": "snap-1", "expectedGeneration": 2}
```

`expectedGeneration` 为非负 JSON 整数且必须等于卷当前的 `generation`，且卷必须未绑定。成功返回 `201`：

```json
{"id":"snap-1","sourceVolumeId":"vol-1","poolId":"pool-a","sizeBytes":600,"sourceGeneration":2}
```

- 相同 `id`、源卷与源版本的重试返回 `200` 及原结果，不重复计量。
- 相同 `id` 的其他请求（不同源卷或源版本）返回 `409 snapshot_exists`。
- 源卷不存在返回 `404 volume_not_found`。
- 卷已绑定返回 `409 volume_in_use`。
- 版本不符返回 `409 stale_generation`。
- 容量不足返回 `409 insufficient_capacity`；并发快照绝不会使源池超配，失败不遗留快照或占用。
- 非法请求返回 `400 invalid_request`，且不产生部分状态。

### 列出与查询

- `GET /v1/snapshots` 返回 `{"items":[...]}`，按快照 id 升序。
- `GET /v1/snapshots/{id}` 返回单个快照；不存在返回 `404 snapshot_not_found`。

### 删除快照

`DELETE /v1/snapshots/{id}` 返回 `204` 并立即向源池归还容量；不存在返回 `404 snapshot_not_found`。仍有快照占用容量的池删除时返回 `409 pool_not_empty`。

## 克隆

`POST /v1/snapshots/{id}/clones`

```json
{"id": "vol-copy", "poolId": "pool-b"}
```

克隆卷与快照 `sizeBytes` 相同，`generation` 为 `0`、`binding` 为 `null`，是独立的新卷；容量计入**目标池**，允许跨池（也允许克隆回源池）。成功返回 `201` 及卷表示。

- 相同快照、卷 `id` 和目标池的重试返回 `200` 及当前卷结果，不重复占用。
- 卷 `id` 已被普通建卷或其他克隆占用时返回 `409 volume_exists`。
- 快照不存在返回 `404 snapshot_not_found`；目标池不存在返回 `404 pool_not_found`。
- 容量不足返回 `409 insufficient_capacity`；并发克隆绝不会使目标池超配，失败不遗留卷或占用。
- 非法请求返回 `400 invalid_request`。

## 错误与路由

错误响应统一为：

```json
{"error":{"code":"..."}}
```

| HTTP | code | 场景 |
| --- | --- | --- |
| 400 | `invalid_request` | 未知字段、空设备集、非法 id、容量或版本非正/非整数、非法查询参数等，且不产生部分状态 |
| 404 | `pool_not_found` | 查询、预留或删除不存在的池；克隆目标池不存在 |
| 404 | `volume_not_found` | 查询、绑定、解绑、删除卷或为不存在的卷建快照 |
| 404 | `snapshot_not_found` | 查询、删除不存在的快照，或从不存在的快照克隆 |
| 404 | `not_found` | 未知路径 |
| 405 | `method_not_allowed` | 不支持的方法，响应带正确的 `Allow` 头 |
| 409 | `pool_exists` | 池 id 已存在 |
| 409 | `device_in_use` | 设备已被其他池占用 |
| 409 | `pool_not_empty` | 删除仍有预留、卷或快照的池 |
| 409 | `idempotency_conflict` | 同 requestId 重试但 bytes 不同 |
| 409 | `insufficient_capacity` | 预留、建卷、快照或克隆超过池可用容量 |
| 409 | `volume_exists` | 同卷 id 重试但参数不同（含克隆 id 冲突） |
| 409 | `volume_already_bound` | 已绑定卷试图改绑其他节点 |
| 409 | `volume_in_use` | 删除或快照仍绑定在节点上的卷 |
| 409 | `snapshot_exists` | 同快照 id 重试但源卷或源版本不同 |
| 409 | `stale_generation` | 绑定/解绑或快照时 expectedGeneration 与当前版本不符 |

## 验证

```bash
go test ./...
```
