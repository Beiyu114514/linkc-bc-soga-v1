# 代码审查报告

- **日期**：2026-11-14
- **范围**：全仓库（`/code-review all`）
- **方法**：静态审查 + 逐条对照当前源码核对；涉及 Xray 行为的条目已对照 `xray-core v1.260327.1-0.20261109052709-676eaf66f8f0` 上游实现
- **结果**：15 条，其中高危 5 条、中危 8 条、低危 2 条

## 总览

| # | 严重性 | 位置 | 问题 | 主要影响 |
|---|--------|------|------|----------|
| 1 | 高 | `internal/audit/checker.go:107` | DNS 解析无结果时跳过全部规则匹配 | 审计 fail-open，可绕过封禁 |
| 2 | 高 | `internal/agent/sync.go:210` | 规则更新受 change 标志门控，失败后不再重试 | 新审计规则可能永远不生效 |
| 3 | 高 | `internal/audit/rule.go:154` | 空白/空值规则编译为“匹配一切” | 全量误封或审计队列刷屏 |
| 4 | 高 | `internal/agent/agent.go:763` | 内存积压计入暂停阈值，Push 又拒绝运行 | 流量管道死锁，重启才恢复 |
| 5 | 中 | `cmd/linkc-node/main.go:115` | SIGHUP goroutine 未 join | 关机时与 Close 竞争，可能 panic |
| 6 | 高 | `cmd/linkc-node/main.go:246` | redact 漏掉 ECH/ML-DSA 密钥字段 | `dump-config` 明文输出密钥 |
| 7 | 中 | `internal/audit/rule.go:213` | 白名单 IP 规则不置 `hasIP` | 白名单目标被误封 |
| 8 | 中 | `internal/xray/validate.go:187` | REALITY short_id 校验与 Xray 相反 | 合法配置被拒 / 非法配置放行 |
| 9 | 中 | `internal/panel/client.go:354` | ETag 缓存写失败丢弃已取回的响应 | 面板正常时 agent 停止更新 |
| 10 | 低 | `internal/agent/alive.go:48` | 推送成功前就记录 IP 快照 | 失败后 IP 上报延迟一个周期 |
| 11 | 中 | `internal/panel/client.go:460` | `Retry-After` 无上限且末次仍 sleep | Sync 与 applyMu 被长时间占用 |
| 12 | 中 | `internal/audit/checker.go:67` | 加载失败的非 nil GeoData 被缓存 | geo 规则在补齐数据后仍不生效 |
| 13 | 中 | `internal/xray/runtime.go:1145` | 限速写失败不释放 MultiBuffer | 连接频繁取消时内存泄漏 |
| 14 | 中 | `internal/traffic/manager.go:98` | 单次 Push 最多排空 5000 条 | 大节点日志积压只增不减 |
| 15 | 低 | `internal/xray/runtime.go:935` | 设备统计仅在 enforce 模式生效 | report 模式设备指标恒为 0 |

---

## 高危

### 1. 审计在 DNS 解析失败时 fail-open

**位置**：`internal/audit/checker.go:107`

**问题**：当目标是域名、开启了 `resolve_domain_for_ip_rules` 且存在 IP 规则时，`Check` 只在解析出的 IP 列表上循环调用 `rules.Match`。`resolve` 在超时、NXDOMAIN 或返回空列表时返回 `nil`，循环体一次都不执行，`ok` 保持 `false`，函数直接返回“放行”。此时 keyword/domain/full/port 规则完全没有被咨询过。

**触发路径**：
1. 配置 `audit.resolve_domain_for_ip_rules: true`，规则中同时存在任意 `ip:`/`geoip:` 规则和一条 `keyword:blocked` 规则。
2. 用户访问命中 keyword 的域名，但该域名的 DNS 查询超时或返回 NXDOMAIN。
3. `c.resolve` 返回 `nil` → 跳过 `Match` → 连接被放行。

**影响**：审计策略在最需要它的场景（恶意域名常伴随 DNS 污染/超时）下失效，属于安全绕过。

**修复建议**：解析不到任何 IP 时回退到不含 IP 的匹配：

```go
if ip == nil && domain != "" && c.cfg.ResolveDomain && rules.HasIPRules() {
    matched := false
    for _, candidate := range c.resolve(domain) {
        if ruleID, white, ok = rules.Match(domain, candidate, int(dest.Port)); ok {
            ip = candidate
            matched = true
            break
        }
    }
    if !matched {
        ruleID, white, ok = rules.Match(domain, nil, int(dest.Port))
    }
} else {
    ruleID, white, ok = rules.Match(domain, ip, int(dest.Port))
}
```

**附注**：`resolve` 不缓存失败结果，DNS 故障时每条连接都会付出最多 2 秒超时代价。建议同时增加短 TTL 的负缓存。

---

### 2. 审计规则在 whitelist 拉取失败后不再应用

**位置**：`internal/agent/sync.go:210`

**问题**：`audit.Update` 被 `rulesChanged || whiteChanged || localChanged || !auditLoaded` 门控。面板客户端在 200 响应时已经把新 ETag 写入缓存，因此下一轮拉取会得到 304 + 缓存 body，`changed` 为 `false`。如果本轮因为另一个端点失败导致 `auditOK == false`，更新块被跳过，但 ETag 已经前进；下一轮两个端点都是 304，门控为假，新规则永远不会被编译。`PutLastGood` 和 `a.localWhite` 也在 `Update` 成功之前就推进了。

**触发路径**：
1. 面板发布规则 R2，`audit_rules` 拉取成功（ETag 已缓存），`white_list` 拉取失败 → `auditOK=false`，跳过 `Update`。
2. 下一轮两个端点均返回 304 → `rulesChanged=false`、`whiteChanged=false`、`localChanged=false`、`auditLoaded=true` → 门控为假。
3. R1 持续生效，直到面板发布 R3。

**影响**：规则更新静默丢失；若 R2 是封禁规则，则形成持续放行窗口。

**修复建议**：
- 在 Agent 中维护 `auditPending` 标志：当 `rulesChanged || whiteChanged` 但 `!auditOK`，或 `Update` 返回错误时置位；门控加入 `auditPending`，`Update` 成功后清除。
- 304 时 `rawRules`/`rawWhite` 仍会由缓存 body 填充，因此仅靠内存标志即可在下一轮完成补偿。
- `PutLastGood` 与 `localWhite` 的推进移到 `Update` 成功之后，避免 last_good 领先于实际生效规则。

---

### 3. 空白规则编译为“匹配一切”

**位置**：`internal/audit/rule.go:154`

**问题**：长度校验发生在 `strings.TrimSpace` 之前，`"   "` 能通过 `len(raw.Rule) == 0` 检查；随后 `parseOne` 将其 trim 成空串，auto 模式下 `looksRegex("")` 为假，最终生成 `kindKeyword{value: ""}`。`compiled.match` 对 keyword 的实现是 `domain != "" && strings.Contains(domain, "")`，恒为真。`keyword:`、`regexp:` 等前缀空值同理：`regexp.Compile("")` 也匹配任意字符串。

**触发路径**：面板下发 `rule: "   "`（或 `keyword:`、`regexp:`）→ 编译成功 → `action=block` 时所有域名的连接全部被拒；`action=report` 时每条连接都进审计队列。

**影响**：全节点误封或审计/存储洪泛，属于高危可用性事故。

**修复建议**：在 `parseOne` trim 之后拒绝空规则；对需要值的 prefix 校验非空：

```go
raw = strings.TrimSpace(raw)
if raw == "" {
    return compiled{}, fmt.Errorf("empty rule")
}
```

并在 `parsePrefixed` 的 `regexp`/`keyword`/`domain`/`full` 分支中要求 `value != ""`。白名单侧同样受益（`keyword:` 空值会把所有目标加入白名单）。

---

### 4. 流量管道在 journal 写失败时死锁

**位置**：`internal/agent/agent.go:763`、`internal/traffic/manager.go:43-49,89-91,170-178`

**问题**：`PendingBytes()` 同时统计 journal 和 `m.mem` 中未落盘的内存增量。`collect` 在 `PendingBytes() > max_journal_bytes` 时直接返回，连 `Manager.Collect` 都不会调用；而 `Manager.Collect` 的第一件事正是尝试把 `mem` 刷入 journal。同时 `Push` 在 `mem` 非空时直接返回 `deltas are not journaled yet`，拒绝排空 journal。三者组合成环形等待。

**触发路径**：
1. bbolt 写失败（如磁盘满）数个采集周期，`Collect` 把增量堆在 `m.mem`。
2. `journal + mem` 超过 `storage.max_journal_bytes`。
3. `collect` 每次都在 guard 处返回；`Push` 因 `mem` 非空拒绝运行；journal 永不排空，`mem` 永不冲刷。
4. 只有重启能恢复，且 `mem` 中的数据丢失。

**影响**：流量上报完全停摆，重启丢失未落盘增量。

**修复建议**：
- 把大小 guard 移到 `Manager.Collect` 内部、内存冲刷之后；或让 `agent.collect` 先调用显式的 `FlushMem()`，成功后再判断 journal 大小。
- `Push` 不应因 `mem` 非空而整体拒绝：`mem` 尚未 journal，与 journal 中的记录无顺序冲突，可以正常推送已落盘记录并记录一条 backlog 日志。
- 为 `mem` 积压增加指标/告警，避免只能从日志发现。

---

### 5. SIGHUP goroutine 未 join，关机时与 Close 竞争

**位置**：`cmd/linkc-node/main.go:115`

**问题**：`runAgent` 启动的 SIGHUP goroutine 没有任何等待机制。`a.Run(ctx)` 在 ctx 取消后返回，随后 deferred `a.Close()` 关闭 engine 并把 store 置空/关闭；此时仍在执行的 `ReloadCerts` 可能正处在 `a.collect` 或 `engine.SwapInbound` 中。

**触发路径**：SIGHUP 与 SIGTERM 几乎同时到达 → `ReloadCerts` 进入 `collect`/`SwapInbound` → `Run` 返回 → `Close` 关闭 bbolt 与 Xray → `ReloadCerts` 调用 `store.AppendTraffic`（nil db）或操作已关闭的 Xray 实例 → panic，进程非正常退出。

**影响**：关机路径崩溃，可能影响 systemd 对退出状态的判断，并丢失最后一次流量采集。

**修复建议**：用 `sync.WaitGroup` 等待 goroutine 退出，并注意 defer 的 LIFO 顺序（`wg.Wait()` 要注册在 `defer a.Close()` 之后）：

```go
var wg sync.WaitGroup
wg.Add(1)
go func() {
    defer wg.Done()
    // ... 现有 select 循环
}()
defer wg.Wait()
```

另建议在 `ReloadCerts` 获取 `applyMu` 后检查 `a.closed`，对已关闭的 Agent 直接返回。

---

### 6. `dump-config` 泄露 ECH / ML-DSA 密钥

**位置**：`cmd/linkc-node/main.go:246`

**问题**：`redact` 的小写 key 白名单只覆盖 `privatekey`、`shortids` 等，未覆盖 `echServerKeys` 与 `mldsa65Seed`。这两个字段由 `internal/xray/runtime.go:729,753` 原样写入生成的 Xray JSON。

**触发路径**：节点配置了 `node.tls.ech_server_keys` 或 `node.reality.mldsa65_seed`，运维执行 `linkc-node dump-config`，base64 编码的 ECH 私钥与 ML-DSA-65 seed 被打印到 stdout/日志/工单附件。

**影响**：长期密钥泄露，可被用于冒充服务端或伪造 REALITY 握手。

**修复建议**：把两个字段（含下划线变体）加入 redact 列表：

```go
case "id", "email", "privatekey", "private_key", "certificate", "key",
    "shortids", "short_ids", "headers",
    "echserverkeys", "ech_server_keys", "mldsa65seed", "mldsa65_seed":
```

建议补一个单测：构造包含全部敏感字段的 map，断言 redact 后无明文残留。

---

## 中危

### 7. 白名单 IP 规则不置 `hasIP`

**位置**：`internal/audit/rule.go:213`

**问题**：`Compile` 只在主规则列表（含 geo 展开）中设置 `r.hasIP`，白名单分支直接 `append`，从不更新。若白名单里只有 `ip:`/`geoip:` 条目，`HasIPRules()` 返回 `false`，`Check` 不会为域名目标做 DNS 解析，白名单 IP 匹配器收到 `nil` IP 后返回 `false`。

**触发路径**：规则为 `keyword:example`（block）+ 白名单 `ip:203.0.113.5`，`example.com` 解析到该 IP。`Check` 走 `Match(domain, nil, port)`，keyword 命中 → 本应被白名单放行的目标被封锁。

**影响**：白名单失效，误封合法目标。

**修复建议**：白名单条目编译后同样维护 `hasIP`（geo 展开的条目也要）：

```go
r.white = append(r.white, cs...)
for _, c := range cs {
    if c.kind == kindIP {
        r.hasIP = true
    }
}
```

普通 `parseOne` 结果同理。

---

### 8. REALITY short_id 校验与 Xray 相反

**位置**：`internal/xray/validate.go:187`

**问题**：当前校验拒绝空字符串（`len(sid) == 0`），却允许空列表。Xray 上游行为正好相反：
- `infra/conf/transport_security.go:135`：`len(ShortIds) == 0` 直接报 `empty "shortIds"`；
- 同文件 139-146：空字符串可以 `hex.Decode` 成功（得到全零 8 字节），是禁用 shortId 的常见写法。

**触发路径**：
- 面板下发 `short_ids: [""]` → `FromPanel` 接受 → `ValidateConfig` 报 `invalid short_id`，节点拒绝启动/应用配置。
- 面板下发 `short_ids: []` → `ValidateConfig` 通过 → Xray 在 `AddInboundHandler` 阶段报 `empty shortIds`。

**影响**：合法配置无法上线；非法配置延迟到 Xray 内部才失败，错误信息更晦涩。

**修复建议**：仅在 reality 下要求列表非空，允许空字符串，长度与十六进制合法性交给 `hex.DecodeString`：

```go
if tlsType == "reality" && len(node.ShortIDs) == 0 {
    return fmt.Errorf("reality requires at least one short_id")
}
for _, sid := range node.ShortIDs {
    if len(sid) > 16 {
        return fmt.Errorf("invalid short_id %q: too long", sid)
    }
    if _, err := hex.DecodeString(sid); err != nil {
        return fmt.Errorf("invalid short_id %q: %w", sid, err)
    }
}
```

（`shortIds` 只在 reality 分支写入配置，非 reality 类型无需校验。）

---

### 9. ETag 缓存写失败会丢弃有效响应

**位置**：`internal/panel/client.go:354`

**问题**：200 分支在 `PutETag` 失败时返回错误，丢弃刚读到的 body。304 分支和 `getOnceUnconditional` 都使用 `_ = c.cache.PutETag(...)` 忽略错误，行为不一致。

**触发路径**：`dump-config` 用 `store.OpenReadOnly` 打开状态库，`PutETag` 必然返回 `bolt.ErrDatabaseReadOnly`，`FetchNode`/`FetchUsers` 全部报 `cache write` 错误，`dump-config` 静默回退到 `last_good`（没有则直接失败）。运行中的 agent 在磁盘满/只读挂载时同样会停止应用面板更新。

**影响**：缓存层故障被放大成拉取失败，可用性下降。

**修复建议**：与其它路径保持一致，缓存写失败不致命：

```go
if c.cache != nil {
    _ = c.cache.PutETag(c.cacheKey(path), rec)
}
```

如需可观测性，增加一个失败计数器而非返回错误。

---

### 10. alive 追踪在推送成功前记录快照

**位置**：`internal/agent/alive.go:48`

**问题**：`diff` 在构造批次时就把 `t.last[id] = key` 写入。若后续 `PushAliveIP` 失败，下一轮 diff 认为该用户没有变化而跳过，面板要等到下一次 full 快照才能拿到新 IP。

**触发路径**：用户 42 的 IP 变化 → `diff` 记录新 key → POST 失败（5xx/超时）→ 后续每轮都跳过用户 42 → 直到 `full_every`（默认 20 次，60s 间隔约 20 分钟）才补发。

**影响**：在线 IP 上报最长延迟约 20 分钟，影响风控与在线统计。

**修复建议**：把 `diff` 拆成 `pending()`（只读当前快照并计算批次）和 `commit(batch)`（成功后更新 `last` 并清理离线用户）；`pushAliveIP` 仅在 `PushAliveIP` 成功后调用 `commit`。

---

### 11. `Retry-After` 无上限且末次仍 sleep

**位置**：`internal/panel/client.go:359-364,456-470`

**问题**：429 分支在 `getOnce` 内部直接 `sleep(ctx, delay)`，`delay` 完全由服务端 `Retry-After` 决定，没有上限；而且这个 sleep 发生在返回 `retry=true` 之前，调用方随后即使因 `attempt == attempts` 不再重试，也已经睡完了整个时长。

**触发路径**：面板返回 `429 Retry-After: 3600` → `getOnce` 在请求循环内睡 1 小时并持有 `applyMu` → SIGHUP 证书重载和下一轮 pull 全部阻塞；若已是最后一次尝试，睡满 1 小时后仍返回错误。

**影响**：一次 429 可让配置应用停摆任意时长。

**修复建议**：
- 给 `Retry-After` 设上限（例如 30s~60s，或做成配置项）。
- 429 的重试等待交给 `get()` 的退避循环处理：`getOnce` 只返回 `retry=true` 和期望延迟，由调用方在“还有下一次尝试”时才 sleep。

---

### 12. 加载失败的 GeoData 被永久缓存

**位置**：`internal/audit/checker.go:67`

**问题**：`LoadGeodata` 在文件缺失/解析失败时返回一个非 nil 的空 `GeoData` 加 error；`Update` 无论成败都执行 `c.geo = geo`。后续 `Update` 因 `c.geo != nil` 不再重载，geo 规则永远编译不出来（strict 模式下每次 `Update` 都失败）。

**触发路径**：启动时 `geodata.dir` 已配置但 `geosite.dat`/`geoip.dat` 尚未安装 → 首次 `Update` 缓存空 GeoData → 运维补装文件 → 后续 `Update` 不再重载 → 规则持续不生效，直到重启。

**影响**：geo 封禁规则长期静默失效。

**修复建议**：只在成功时赋值，失败保持 `nil` 以便下轮重试：

```go
if geo, err := LoadGeodata(c.cfg.GeodataDir); err != nil {
    c.log.Warn("load geodata", "dir", c.cfg.GeodataDir, "error", err)
} else {
    c.geo = geo
}
```

若担心每轮 Sync 都读盘，可加一个失败重试间隔（如 5 分钟）。部分文件缺失时也不要用半份数据。

---

### 13. 限速写失败不释放 MultiBuffer

**位置**：`internal/xray/runtime.go:1145`

**问题**：`buf.Writer.WriteMultiBuffer` 的契约是接管 buffer 所有权，出错时由 writer 负责释放。`rateWriter` 在 `waitLimits` 返回错误时直接 `return err`，没有释放 `mb`。Xray 的 `copyInternal`（`common/buf/copy.go`）在 writer 报错时也是直接返回，不会补释放。

**触发路径**：限速连接在 `waitLimits` 等待 token 时被取消 → 返回 context 错误 → `mb` 泄漏。连接频繁建立/取消时内存持续增长。

**影响**：池化 buffer 泄漏，长时间运行后内存上涨。

**修复建议**：

```go
func (w *rateWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
    if !mb.IsEmpty() {
        if err := waitLimits(w.ctx, w.lims, int(mb.Len())); err != nil {
            buf.ReleaseMulti(mb)
            return err
        }
    }
    return w.w.WriteMultiBuffer(mb)
}
```

`buf.ReleaseMulti` 见上游 `common/buf/multi_buffer.go:57`。

**相关观察**：`rateReader.ReadMultiBuffer` 在 `waitLimits` 失败时返回 `(mb, err)`；`copyInternal` 会先把非空 buffer 写出去，再处理读错误，因此取消时仍会转发一帧数据。若语义上希望取消即丢弃，应 `buf.ReleaseMulti(mb)` 后返回 `nil, err`。

---

### 14. 单次 Push 排空上限导致 journal 积压

**位置**：`internal/traffic/manager.go:98`

**问题**：`Push` 每次最多读取 5000 条 journal 记录，而 `Collect` 每 30 秒可为每个活跃用户追加一条记录，`Push` 默认 60 秒一次。用户数超过约 2500 时，写入速率就超过排空速率。

**触发路径**：10,000 个用户有流量 → 每 30 秒约 1 万条记录，每分钟只排空 5000 条 → journal 以约 1.5 万条/分钟增长 → 超过 `storage.max_journal_bytes`（默认 64MiB）后 `collect` 暂停，流量上报延迟数小时。

**影响**：大节点流量延迟、journal 膨胀，最终触发第 4 条的暂停逻辑。

**修复建议**：
- `Push` 改为循环排空，直到无记录或达到时间预算（如 5 秒）：

```go
for {
    records, err := m.store.PendingTraffic(maxRecords)
    if err != nil || len(records) == 0 {
        break
    }
    // ... 聚合并 flush ...
    if time.Since(start) > budget {
        break
    }
}
```

- 或让 `maxRecords` 可配置，并保证其不小于“单次 Collect 可能产生的记录数”。
- 同时把 `max_journal_bytes` 的告警接出来，避免只在暂停时才发现。

---

## 低危

### 15. report 模式下设备统计恒为零

**位置**：`internal/xray/runtime.go:935`

**问题**：设备追踪整块逻辑被 `g.mode == "enforce"` 门控。`report` 模式下 `deviceRefs` 从不填充，`Gate.Metrics().Devices`、`Gate.Stats()` 和 `RejectsDevice` 永远为 0；而 `After` 本身在非 `off` 模式都会执行递减，说明设计上并未要求只在 enforce 下记账。

**触发路径**：运维设置 `node.limits.mode=report` 观察设备限制影响 → 用户多设备连接 → `/status`、`/metrics` 和告警中的设备数/违规数始终为 0，无法评估策略。

**影响**：report 模式失去观测价值。

**修复建议**：非 `off` 模式都维护 `deviceRefs`，仅在 enforce 下拒绝：

```go
if g.mode != "off" && u.DeviceLimit > 0 && device != "" {
    set := g.deviceRefs[id]
    if set == nil {
        set = make(map[string]int)
        g.deviceRefs[id] = set
    }
    if g.mode == "enforce" && set[device] == 0 && len(set) >= u.DeviceLimit {
        g.rejectsDevice++
        g.mu.Unlock()
        return false
    }
    set[device]++
}
```

如果希望 report 模式也能看到“本应拒绝”的次数，可另加一个 `wouldRejectDevice` 计数器。

---

## 建议修复顺序

1. **安全与数据面（建议立即修）**：#1、#3、#6、#2、#4
2. **稳定性与资源（本迭代）**：#5、#9、#11、#13、#12
3. **正确性与可观测性（随后）**：#7、#8、#10、#14、#15

## 回归测试建议

- #1：mock resolver 返回空列表，断言 keyword 规则仍能命中。
- #3：表驱动覆盖 `"   "`、`keyword:`、`regexp:`，断言编译失败。
- #7：仅含白名单 `ip:` 的规则集，断言 `HasIPRules()` 为真且域名解析后放行。
- #8：`short_ids: [""]` 应通过、`short_ids: []` 在 reality 下应失败。
- #10：注入失败的 `PushAliveIP`，断言下一轮仍会重发同一用户。
- #13：构造 `waitLimits` 取消路径，断言 buffer 已释放（可用池计数或 race/泄漏检测）。
- #15：report 模式下多设备连接，断言 `Metrics().Devices` 非零且无拒绝。
