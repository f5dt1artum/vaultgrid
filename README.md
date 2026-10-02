# VaultGrid

这是一个面向存储与数据管理的块/对象存储与卷管理控制面。长期目标是提供存储池与卷生命周期、S3 风格对象接口、纠删码与多副本放置、一致性哈希与再平衡、快照与克隆、完整性自愈和配额，把存储控制面沉淀为可复用服务。

仓库采用 Go。当前提供进程健康检查、存储池目录、池级容量预留与卷生命周期管理；每个能力都定义可观察的公共行为、兼容边界和失败语义，不依赖未公开内部 API。所有状态保存在内存中，进程重启后清空。

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

`DELETE /v1/storage-pools/{id}`：池无预留且无卷时返回 `204` 并释放其设备；否则返回 `409 pool_not_empty`。

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

卷占用直接计入所属池的 `allocatedBytes`，与显式预留共享池容量。卷 id 与节点 id 遵循同样的标识规则。

### 创建卷（幂等）

`POST /v1/volumes`

```json
{"id": "vol-1", "poolId": "pool-a", "sizeBytes": 400}
```

`sizeBytes` 为正整数（JSON 整数）。成功返回 `201`，结果包含 `generation`（初始 `0`）与 `binding`（初始 `null`）：

```json
{"id":"vol-1","poolId":"pool-a","sizeBytes":400,"generation":0,"binding":null}
```

- 同一 `id` 以相同参数重试返回 `200` 及当前结果，不重复计量；参数不同返回 `409 volume_exists`。
- 池不存在返回 `404 pool_not_found`；容量不足返回 `409 insufficient_capacity`。
- 失败不产生卷或占用；并发创建不会使池超配。

### 列出与查询

- `GET /v1/volumes` 返回 `{"items":[...]}`，按卷 id 升序。
- `GET /v1/volumes/{id}` 返回单个卷；不存在返回 `404 volume_not_found`。

### 绑定与解绑

`PUT /v1/volumes/{id}/binding` 建立独占绑定：

```json
{"nodeId": "node-1", "expectedGeneration": 0}
```

`expectedGeneration` 为非负整数且须等于当前 `generation`。未绑定卷成功后返回 `200`，`binding` 记录 `nodeId`，`generation` 加一；同节点且版本匹配的重试返回当前结果而不递增；已绑定其他节点返回 `409 volume_already_bound`；版本不符返回 `409 stale_generation`；同版本并发请求只有一个能改变状态。

`DELETE /v1/volumes/{id}/binding?expectedGeneration=N` 解绑：版本错误返回 `409 stale_generation`；已绑定时返回 `204` 并递增 `generation`；原本未绑定时返回 `204` 且版本不变。缺少、重复、非整数或额外查询参数返回 `400 invalid_request`。

### 删除卷

`DELETE /v1/volumes/{id}` 只删除未绑定卷，成功返回 `204` 并归还容量；绑定中返回 `409 volume_in_use`，不存在返回 `404 volume_not_found`。卷存在时删除所属池仍返回 `409 pool_not_empty`。

## 错误与路由

错误响应统一为：

```json
{"error":{"code":"..."}}
```

| HTTP | code | 场景 |
| --- | --- | --- |
| 400 | `invalid_request` | 未知字段、空设备集、非法 id、容量非正整数或非整数等，且不产生部分状态 |
| 404 | `pool_not_found` | 查询、预留或删除不存在的池；在不存在的池中创建卷 |
| 404 | `volume_not_found` | 查询、绑定、解绑或删除不存在的卷 |
| 404 | `not_found` | 未知路径 |
| 405 | `method_not_allowed` | 不支持的方法，响应带正确的 `Allow` 头 |
| 409 | `pool_exists` | 池 id 已存在 |
| 409 | `device_in_use` | 设备已被其他池占用 |
| 409 | `pool_not_empty` | 删除仍有预留或卷的池 |
| 409 | `idempotency_conflict` | 同 requestId 重试但 bytes 不同 |
| 409 | `insufficient_capacity` | 预留或卷超过池可用容量 |
| 409 | `volume_exists` | 卷 id 已存在但创建参数不同 |
| 409 | `volume_already_bound` | 卷已绑定其他节点 |
| 409 | `stale_generation` | expectedGeneration 与当前版本不符 |
| 409 | `volume_in_use` | 删除仍绑定节点的卷 |

## 验证

```bash
go test ./...
```
