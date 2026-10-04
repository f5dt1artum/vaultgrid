# VaultGrid

这是一个面向存储与数据管理的块/对象存储与卷管理控制面。长期目标是提供存储池与卷生命周期、S3 风格对象接口、纠删码与多副本放置、一致性哈希与再平衡、快照与克隆、完整性自愈和配额，把存储控制面沉淀为可复用服务。

仓库采用 Go。当前提供进程健康检查、存储池目录、池级容量预留、卷生命周期与单节点独占绑定、内存态卷快照、同池多卷一致性快照组、快照原地恢复与基于快照的跨池克隆、S3 风格的桶与对象接口、池级容量报表导出以及内存态审计事件；每个能力都定义可观察的公共行为、兼容边界和失败语义，不依赖未公开内部 API。所有状态保存在内存中，进程重启后清空。

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

- 相同 `id`、`poolId`、`sizeBytes` 重试返回 `200` 及当前卷结果，不重复计量（比对的是原始创建参数：卷即使经调整改变了当前大小，原始请求重放仍返回 `200` 及当前卷表示）。
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

### 调整卷大小

`PUT /v1/volumes/{id}/size`

```json
{"sizeBytes": 900, "expectedGeneration": 2}
```

该子资源只允许 `PUT`，且不接受任何查询参数。`sizeBytes` 为正 JSON 整数，`expectedGeneration` 为非负 JSON 整数且必须等于卷当前的 `generation`，卷必须未绑定：

- 新大小与当前不同：返回 `200` 及当前卷表示，`sizeBytes` 更新，`generation` 加一，`binding` 保持原值（未绑定时仍为 `null`）。
- 新大小与当前相同且版本匹配：返回 `200` 及当前卷表示，`generation`、容量计量与审计日志均不变。
- 卷已绑定：返回 `409 volume_in_use`，该判断优先于版本判断。
- 版本不符：返回 `409 stale_generation`；使用同一当前 generation 的并发请求至多一个能改变大小。
- 卷不存在返回 `404 volume_not_found`。
- 扩容先检查租户配额、再检查池容量：分别返回 `409 tenant_quota_exceeded` 与 `409 insufficient_capacity`；缩容不受这两项限制。
- 正文缺字段、未知字段、数值非法、卷 id 非法或携带查询参数返回 `400 invalid_request`。
- 任何失败都不改变卷、池容量、租户用量或审计日志。

检查与提交在同一临界区内完成，因此与绑定、删除、快照创建或其他容量分配并发时不会超配或重扣。大小改变后，池的 `volumeBytes`、`allocatedBytes`、`availableBytes`、租户配额用量以及 JSON、CSV 容量报表立即按有符号差额更新；审计日志原子追加一条 `volume.resized` 事件，`resource` 为 `/v1/volumes/{id}/size`，`bytesDelta` 为有符号差额（扩容为正、缩容为负），同大小请求与失败不记录事件。

已有快照保留创建时的大小、源代数与源池占用，不受源卷调整影响；后续克隆继续使用快照大小。克隆卷与普通卷一样支持调整。卷被调整后，原创建参数的幂等重放仍返回 `200` 及当前卷表示（创建幂等只比对 `id`、`poolId` 与租户），其他参数仍返回 `409 volume_exists`。

### 恢复快照到卷（原地）

`POST /v1/volumes/{id}/restore`

```json
{"snapshotId": "snap-1", "expectedGeneration": 2}
```

把卷原地恢复到其自身某个快照记录的大小：不创建新卷，也不删除快照。该子资源只允许 `POST`，且不接受任何查询参数。`expectedGeneration` 为非负 JSON 整数且必须等于卷当前的 `generation`，卷必须未绑定：

- 成功返回 `200` 及当前卷表示：`sizeBytes` 变为快照大小，`binding` 保持 `null`，`generation` 无论大小是否改变都加一（恢复本身产生了新状态，与同大小调整的幂等空操作不同）。
- 只允许使用目标卷**当前实例**产生的快照：其他卷的快照，或卷删除后以同一 id 重建前的旧快照，都返回 `409 snapshot_source_mismatch`。
- 卷不存在返回 `404 volume_not_found`；快照不存在（含已删除）返回 `404 snapshot_not_found`。
- 卷已绑定返回 `409 volume_in_use`，该判断优先于版本判断；版本不符返回 `409 stale_generation`。
- 恢复导致扩容时，按卷的既有租户先检查租户配额、再检查池容量，分别返回 `409 tenant_quota_exceeded` 与 `409 insufficient_capacity`；缩容立即释放容量与租户用量，不受这两项限制。
- 正文缺字段、未知字段、标识或数值非法，或携带查询参数返回 `400 invalid_request`。
- 任何失败都不改变卷、快照、容量计量、租户用量或审计日志。

检查与提交在同一临界区内完成，与绑定、调整大小、删除卷、删除快照并发时等价于某个串行顺序。成功提交后，池容量、容量报表与租户用量按新旧大小的有符号差额一致更新，并原子追加一条 `volume.restored` 审计事件：`resource` 为 `/v1/volumes/{id}/restore`，`poolId` 为目标池，`bytesDelta` 为有符号差额（差额为零也记录）。快照本身保持不动，继续计入源池占用。

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
- `DELETE /v1/snapshots/{id}` 返回 `204` 并立即向源池归还容量；不存在返回 `404 snapshot_not_found`；快照属于某个快照组时返回 `409 snapshot_in_group`（只能随整组删除）。

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

## 快照组

快照组把同一池中多个卷的快照作为一个原子单元创建和删除：整组要么全部提交，要么不留任何组、快照、计量或审计变化。组内快照就是普通快照，照常支持列出、查询、恢复与克隆，并按 `sizeBytes` 计入源池。

### 创建快照组（幂等）

`POST /v1/snapshot-groups`

```json
{
  "id": "grp-1",
  "members": [
    {"volumeId": "vol-1", "snapshotId": "snap-1", "expectedGeneration": 0},
    {"volumeId": "vol-2", "snapshotId": "snap-2", "expectedGeneration": 1}
  ]
}
```

`members` 含 2 至 64 个成员；`volumeId` 与 `snapshotId` 在请求内分别唯一，所有卷须属于同一池。成功返回 `201`，组表示包含 `id`、`poolId` 和按 `snapshotId` 升序的快照项：

```json
{"id":"grp-1","poolId":"pool-a","snapshots":[
  {"id":"snap-1","sourceVolumeId":"vol-1","poolId":"pool-a","sizeBytes":600,"sourceGeneration":0},
  {"id":"snap-2","sourceVolumeId":"vol-2","poolId":"pool-a","sizeBytes":400,"sourceGeneration":1}
]}
```

- 相同组 `id`、相同成员映射（卷→快照，与顺序无关）与相同版本的重试返回 `200` 及原结果，不重复计量或审计；源卷在此之后被删除，重试仍返回 `200`。
- 相同组 `id` 但成员映射或版本不同返回 `409 snapshot_group_exists`。
- 按成员顺序检查：卷不存在返回 `404 volume_not_found`；卷已绑定返回 `409 volume_in_use`；版本不符返回 `409 stale_generation`。
- 成员卷不属于同一池返回 `409 cross_pool_snapshot_group`；任一 `snapshotId` 已存在返回 `409 snapshot_exists`。
- 成员大小按租户聚合后先检查租户配额、再检查池容量，分别返回 `409 tenant_quota_exceeded` 与 `409 insufficient_capacity`。
- 非法 id、成员数越界、成员重复、缺失/非法字段或携带查询参数返回 `400 invalid_request`。
- 任何失败都不留下组、快照、计量或审计变化；整组提交与其他卷和快照写请求等价于某个串行顺序。

### 删除快照组

`DELETE /v1/snapshot-groups/{id}` 原子删除全部成员快照，并立即向共同池归还容量、释放各租户用量，返回 `204`；组不存在返回 `404 snapshot_group_not_found`。已从组内快照克隆出的卷不受影响。直接 `DELETE /v1/snapshots/{id}` 删除组内快照返回 `409 snapshot_in_group`。

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

### 桶版本控制（不可撤销）

版本控制按桶开启，一旦启用不能关闭；未启用的桶保持下列全部原有行为。

- `GET /v1/buckets/{bucketId}/versioning` 返回 `{"enabled":false}`（未启用）或 `{"enabled":true}`（已启用）；桶不存在返回 `404 bucket_not_found`。
- `PUT /v1/buckets/{bucketId}/versioning` 的正文**仅**接受 `{"enabled":true}`，不接受查询参数：
  - 首次启用返回 `200`，把桶内每个现有对象转成一个数据版（容量仍只计一次），并审计一次 `bucket.versioning-enabled`。
  - 重复启用幂等返回 `200`，不改变状态、不再审计。
  - 其他正文（如 `{"enabled":false}`、`{}`、未知字段）返回 `400 invalid_request`；桶不存在返回 `404 bucket_not_found`。
- 该路径仅允许 `GET`、`PUT`，其他方法返回 `405 method_not_allowed` 且带 `Allow: GET, PUT`。

### 写入对象

`PUT /v1/buckets/{bucketId}/objects/{key}` 以原始请求正文为内容。key 为 `objects/` 后余下路径一次 URL 解码的结果，允许斜杠，须为 1 至 1024 个 UTF-8 字节且无控制字符。内容可为空。`X-Vaultgrid-Meta-*` 请求头作为元数据保存，覆盖写入时整体替换。

成功响应带 `ETag` 头，正文为：

```json
{"key": "a/b.txt", "sizeBytes": 3, "metadata": {"Author": "me"}, "etag": "\"...\""}
```

`etag` 为内容 SHA-256 的带双引号小写十六进制。首次写入返回 `201`，覆盖返回 `200`；覆盖只按大小差额调整池占用。容量不足返回 `409 insufficient_capacity`，旧对象及元数据不变。

支持条件写入：`If-Match: <当前ETag>` 或 `If-None-Match: *`（仅当对象不存在时写入）。条件不满足返回 `412 precondition_failed`；两者并用或其他形式（如 `If-Match: *`、`If-None-Match: <etag>`）返回 `400 invalid_request`。

#### 已启用版本控制的桶

成功写入不覆盖旧版，而是生成一个唯一非空的 `versionId`：响应带 `X-Vaultgrid-Version-Id` 头，正文在原字段外增加 `versionId`：

```json
{"key":"a/b.txt","versionId":"9f86...","sizeBytes":3,"metadata":{},"etag":"\"...\""}
```

- 每个数据版按其**完整大小**计入池容量与桶租户配额；覆盖保留旧版，不释放旧版字节，因此新版本按完整大小而非差额计费。容量或配额不足沿用 `409 insufficient_capacity` / `409 tenant_quota_exceeded`，失败不产生版本、不留计量或审计。
- 当前可见对象首次出现（key 不存在或最新为删除标记）返回 `201`；覆盖当前可见数据版返回 `200`。
- 条件写入只判断**当前可见版**：最新为删除标记时按“不存在”处理（`If-None-Match: *` 可再次成功，`If-Match` 须匹配可见版 etag）。
- 写入不接受 `versionId` 或其他查询参数（`400 invalid_request`）。

### 读取、列举与删除对象

- `GET /v1/buckets/{bucketId}/objects/{key}` 返回原始字节，带 `ETag` 与 `X-Vaultgrid-Meta-*` 头；对象不存在返回 `404 object_not_found`。
- `HEAD` 同上但无正文。
- 读取（含版本桶的 `GET`/`HEAD`）支持单区间字节读取，规则见下文[区间读取](#区间读取)。
- `GET /v1/buckets/{bucketId}/objects` 返回 `{"items":[...]}`，按 key 升序的对象摘要；只接受一个 `prefix` 查询参数，其他或重复参数返回 `400 invalid_request`。
- `DELETE /v1/buckets/{bucketId}/objects/{key}` 返回 `204` 并释放容量；删除不存在（含从未存在）的对象仍为 `204`；桶不存在返回 `404 bucket_not_found`。

非法 key、元数据、查询参数、未知字段或格式错误返回 `400 invalid_request`。并发写删不会超配、重扣或残留部分状态。

#### 区间读取

`GET`/`HEAD` 可携带 `Range` 请求头，仅接受单个 bytes 区间的三种形式：`bytes=start-end`、`bytes=start-`、`bytes=-suffixLength`（首尾均包含，相对所选版本的完整字节长度计算）。

- 不带 `Range`：返回 `200` 及完整对象；成功的 200 与 206 均带 `Accept-Ranges: bytes`，且原有 `Content-Length`、`Content-Type`、`ETag`、`X-Vaultgrid-Meta-*` 及版本桶中的 `X-Vaultgrid-Version-Id` 语义不变。
- 命中区间：返回 `206 Partial Content`，正文仅含选中的连续字节，`Content-Length` 为实际区间长度，`Content-Range: bytes 实际起点-实际终点/完整长度`。结束位置越界截到对象末尾；后缀长度达到或超过对象长度时返回整个对象（仍为 206）。
- 不可满足：起始位置等于或超过对象长度、显式起点大于终点、以及对空对象的任何区间请求，返回 `416 range_not_satisfiable`，并带 `Content-Range: bytes */完整长度`。
- 格式错误：单位不精确为 `bytes`、逗号分隔的多区间、两端皆空、负数或非十进制数、后缀长度为零、额外空白等任何其他形式，返回 `400 invalid_request`，不降级为完整读取。
- 校验顺序：先校验桶、key、查询参数与 `versionId` 并选出目标版本，再处理 `Range`，故桶、对象或版本不存在时仍返回原有的 `404`。
- `HEAD` 的区间响应与同一 `GET` 状态码和响应头一致但无正文；`400`/`416` 使用统一错误 JSON，HEAD 错误响应沿用既有处理。区间读取不改变对象、版本顺序、容量、租户用量或审计事件。

#### 已启用版本控制的桶

读取：

- `GET`/`HEAD` 默认取当前可见版（最新数据版），并带 `X-Vaultgrid-Version-Id` 头；最新版本是删除标记或 key 不存在时返回 `404 object_not_found`。
- 指定 `versionId` 时返回对应**数据版**及其版本头、`ETag`、元数据头；版本不存在或该版本是删除标记时返回 `404 object_version_not_found`。
- 只接受一个非空 `versionId` 参数；空值、重复或与其他参数（含 `prefix`）并用为 `400 invalid_request`。

删除：

- 不带 `versionId`：新增一个删除标记（删除标记也有唯一 `versionId`、不计字节），返回 `204` 并审计 `object.delete-marker-created`；最新版本**已是**删除标记时仅返回 `204`，不新增标记、不审计。
- 带 `versionId`：永久删除该版本，返回 `204` 并审计 `object.version-deleted`（`bytesDelta` 为释放字节，删除标记为 `0`）。只有数据版释放池容量与租户用量；被删版本不存在（含其他 key 的版本）返回 `404 object_version_not_found`。删除最后一个版本后该 key 消失。
- `versionId` 空值、重复或与其他参数并用为 `400 invalid_request`。

列举：

- 普通对象列表（`GET .../objects`）只显示当前可见对象（最新为删除标记的 key 被隐藏）。
- `GET /v1/buckets/{bucketId}/object-versions` 只接受一个 `prefix` 参数，返回 `{"items":[...]}`，包含匹配前缀的**所有数据版和删除标记**，按 key 升序、同一 key 内从新到旧排列。每项字段为 `key`、`versionId`、`isLatest`、`deleteMarker`、`sizeBytes`、`etag`、`metadata`；删除标记的 `sizeBytes` 为 `0`、`etag` 为空串、`metadata` 为 `{}`。其他或重复参数为 `400 invalid_request`，桶不存在为 `404 bucket_not_found`，仅允许 `GET`（其他方法 `405`，带 `Allow: GET`）。

计量与删桶：

- 桶的 `objectCount` 只计当前可见 key；桶的 `bytesUsed`、池的 `objectBytes`/`allocatedBytes`、容量报表以及租户 `usedBytes` 都统计**全部数据版**（删除标记不计字节）。
- 桶内仍存在任何数据版或删除标记时，删桶返回 `409 bucket_not_empty`；此类桶同样阻止删除其所属池。

## 租户配额

每个池可按租户设置字节配额。创建预留、卷、桶时可携带至多一个 `X-Vaultgrid-Tenant` 头指定归属租户（沿用 id 规则）；缺省归属 `default`，非法值或重复头返回 `400 invalid_request`。快照继承源卷的租户，克隆继承快照的租户并计入目标池，对象继承所属桶的租户；删除资源、缩小对象或永久删除对象数据版即释放原租户用量。未配置配额的租户不限额。租户的 `usedBytes` 是其在池内的预留、卷、快照与对象字节总和（空桶不计量；版本控制桶中对象字节按**全部数据版**求和，删除标记不计）。

### 设置配额（幂等）

`PUT /v1/storage-pools/{poolId}/tenant-quotas/{tenantId}`

```json
{"limitBytes": 600}
```

`limitBytes` 为正 JSON 整数。成功返回：

```json
{"poolId":"pool-a","tenantId":"team-a","limitBytes":600,"usedBytes":0,"availableBytes":600}
```

- 首次创建返回 `201`；修改限额或同值重放返回 `200`（同值重放不改变状态）。
- 限额低于该租户当前用量返回 `409 quota_below_usage`。
- 池不存在返回 `404 pool_not_found`；非法 id 或正文返回 `400 invalid_request`。

### 查询、列出与删除

- `GET /v1/storage-pools/{poolId}/tenant-quotas` 返回 `{"items":[...]}`，按 tenantId 升序。
- `GET /v1/storage-pools/{poolId}/tenant-quotas/{tenantId}` 返回单个配额；配额不存在返回 `404 quota_not_found`。
- `DELETE /v1/storage-pools/{poolId}/tenant-quotas/{tenantId}` 始终返回 `204`（含从未存在的配额），只解除限制；配额不阻止删除空池。

### 配额执行

容量增加（建预留、建卷、卷扩容、建快照、克隆、写对象）先判定幂等，再在同一临界区内原子检查租户额度与池容量：超出租户配额返回 `409 tenant_quota_exceeded`，池容量不足仍返回 `409 insufficient_capacity`；失败不留资源、计量或审计残留，并发不会突破任一上限。卷缩容释放租户用量且不受这两项限制。同标识同租户的重试保留原幂等结果；租户不同则按既有规则返回 `idempotency_conflict`、`volume_exists` 或 `bucket_exists`。旧客户端不传头时一切行为不变（归属 `default`）。

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

## 审计事件

所有真正改变状态的成功请求都会向内存态审计日志追加一条事件；失败、只读或成功但未改变状态的请求（含各类幂等重试、已绑定卷的同节点重复绑定、未绑定卷的解绑、删除从未存在的预留或对象）不记录。日志与业务状态及容量计数在同一临界区原子提交：响应成功后立即可查，请求失败绝不遗留事件。事件仅存于内存，进程重启后清空。

每条事件为：

```json
{"sequence":1,"action":"pool.created","resource":"/v1/storage-pools/pool-a","poolId":"pool-a","bytesDelta":0}
```

- `sequence` 从 1 起连续递增；并发请求按实际提交状态的先后编号。
- `action` 取值：`pool.created`、`pool.deleted`、`reservation.created`、`reservation.deleted`、`volume.created`、`volume.deleted`、`volume.bound`、`volume.unbound`、`volume.resized`、`volume.restored`、`snapshot.created`、`snapshot.deleted`、`snapshot-group.created`、`snapshot-group.deleted`、`clone.created`、`bucket.created`、`bucket.deleted`、`object.created`、`object.overwritten`、`object.deleted`、`bucket.versioning-enabled`、`object.version-created`、`object.delete-marker-created`、`object.version-deleted`、`tenant-quota.created`、`tenant-quota.updated`、`tenant-quota.deleted`。
- `resource` 是事件对应资源的公开路径（预留为 `/v1/storage-pools/{id}/reservations/{requestId}`，绑定为 `/v1/volumes/{id}/binding`，卷大小调整为 `/v1/volumes/{id}/size`，对象为 `/v1/buckets/{id}/objects/{key}`，桶版本控制为 `/v1/buckets/{id}/versioning`，克隆卷为 `/v1/volumes/{id}`，快照组为 `/v1/snapshot-groups/{id}`，租户配额为 `/v1/storage-pools/{id}/tenant-quotas/{tenantId}`）。
- `poolId` 是事件所属池：卷、快照、桶、预留、对象、租户配额归属其所在池；快照组归属成员的共同池；克隆计入目标池。
- `bytesDelta` 是该次提交对池 `allocatedBytes` 的有符号变化：创建为正、删除为负、容量无关的动作（建删池、建删桶、绑定解绑、租户配额增删改）为 `0`。快照组创建为成员总字节的正值，删除为其负值。对象覆盖为 `新大小-旧大小`；**即使容量差为零也记录 `object.overwritten`**。卷调整为 `新大小-旧大小`，同大小请求不记录 `volume.resized`。版本控制桶中：每个新建数据版记录 `object.version-created`，`bytesDelta` 为该版完整大小（旧版仍计费）；删除标记记录 `object.delete-marker-created`，`bytesDelta` 为 `0`；永久删除版本记录 `object.version-deleted`，仅数据版 `bytesDelta` 为负、删除标记为 `0`。启用版本控制只在首次记录一次 `bucket.versioning-enabled`（`bytesDelta` 为 `0`），幂等重放不记录。

### 查询事件

`GET /v1/audit-events` 只接受三个可选查询参数：

- `after`：非负十进制整数，缺省 `0`；只返回序号严格大于它的事件。
- `limit`：`1` 至 `1000` 的十进制整数，缺省 `100`；每页至多返回该数量。
- `poolId`：沿用既有 id 规则，缺省不过滤；只返回属于该池的事件。

响应为：

```json
{"items":[...],"nextAfter":0,"hasMore":false}
```

`items` 按 `sequence` 升序，取序号大于 `after` 且匹配池的至多 `limit` 条。`nextAfter` 取本页末项序号，空页取请求中的 `after`。`hasMore` 表示在同一读取状态下其后仍有匹配项；整页来自单一一致状态。`after` 超过当前最大序号时返回空页（`items` 为空、`nextAfter` 为该 `after`、`hasMore` 为 `false`）。翻页时把上一页的 `nextAfter` 作为下一页的 `after`。

`after` 非十进制非负整数、`limit` 不在 `1..1000`、`poolId` 不符合 id 规则，以及未知、重复或缺值参数均返回 `400 invalid_request`；`poolId` 合法但池不存在（含已删除）返回 `404 pool_not_found`。该路径仅允许 `GET`，其他方法返回 `405 method_not_allowed` 且带 `Allow: GET`；额外子路径返回 `404 not_found`。

## 错误与路由

错误响应统一为：

```json
{"error":{"code":"..."}}
```

| HTTP | code | 场景 |
| --- | --- | --- |
| 400 | `invalid_request` | 未知字段、空设备集、非法 id、容量或版本非正/非整数、非法 key/元数据/查询参数等，且不产生部分状态 |
| 404 | `pool_not_found` | 查询、预留或删除不存在的池，向不存在的目标池克隆、建桶，或按不存在的池导出报表 |
| 404 | `volume_not_found` | 查询、绑定、解绑、删除或调整大小不存在的卷，或为不存在的卷创建快照 |
| 404 | `snapshot_not_found` | 查询、删除不存在的快照，或基于不存在的快照克隆 |
| 404 | `bucket_not_found` | 查询、删除不存在的桶，或向不存在的桶读写对象、配置版本控制 |
| 404 | `object_not_found` | 读取不存在（或最新为删除标记）的对象 |
| 404 | `object_version_not_found` | 在版本控制桶中按 `versionId` 读取或删除不存在的版本，或该版本是删除标记（读取时） |
| 404 | `quota_not_found` | 查询不存在的租户配额 |
| 404 | `not_found` | 未知路径 |
| 405 | `method_not_allowed` | 不支持的方法，响应带正确的 `Allow` 头 |
| 409 | `pool_exists` | 池 id 已存在 |
| 409 | `device_in_use` | 设备已被其他池占用 |
| 409 | `pool_not_empty` | 删除仍有预留、卷、快照或桶的池 |
| 409 | `idempotency_conflict` | 同 requestId 重试但 bytes 不同 |
| 409 | `insufficient_capacity` | 预留、建卷、卷扩容、快照、克隆或写对象超过池可用容量（缩容不受限） |
| 409 | `volume_exists` | 同卷 id 重试但参数不同，或克隆 id 已被其他创建/快照占用 |
| 409 | `volume_already_bound` | 已绑定卷试图改绑其他节点 |
| 409 | `volume_in_use` | 删除、创建快照或调整大小仍绑定在节点上的卷（调整大小时优先于版本判断） |
| 409 | `stale_generation` | 绑定/解绑、创建快照或调整大小时 expectedGeneration 与当前版本不符 |
| 409 | `snapshot_exists` | 同快照 id 重试但源卷或源版本不同，或快照组的成员 snapshotId 已存在 |
| 409 | `snapshot_group_exists` | 同快照组 id 重试但成员映射或版本不同 |
| 409 | `cross_pool_snapshot_group` | 快照组成员卷不属于同一池 |
| 409 | `snapshot_in_group` | 直接删除仍属于某个快照组的快照 |
| 404 | `snapshot_group_not_found` | 删除不存在的快照组 |
| 409 | `snapshot_source_mismatch` | 恢复快照不属于目标卷的当前实例（其他卷的快照，或卷删除重建前的旧快照） |
| 409 | `bucket_exists` | 同桶 id 重试但 poolId 不同 |
| 409 | `bucket_not_empty` | 删除仍有可见对象、历史数据版或删除标记的桶 |
| 409 | `quota_below_usage` | 租户配额限额低于该租户当前用量 |
| 409 | `tenant_quota_exceeded` | 容量增加（建预留、建卷、卷扩容、建快照、克隆、写对象）超出该租户在池内的配额（缩容不受限） |
| 412 | `precondition_failed` | 条件写入的 If-Match / If-None-Match 不满足 |

## 验证

```bash
go test ./...
```
