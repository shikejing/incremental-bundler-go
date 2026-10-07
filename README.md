# incremental-bundler-go

面向本地工作区源码的**模块打包引擎**：解析模块依赖图 → 按固定顺序执行内容转换 →
合成为产物，并用一张**可持久化的内容寻址缓存图**让重复构建只重做真正变化的部分。
整条链路只在本进程（以及同一台机器上的其他本进程）内运行，不依赖外部服务，也不
使用真实凭据。

- 纯 Go 标准库实现，无第三方依赖。
- 输入：`MemorySource`（进程内生成内容）或 `DirSource`（本地工作区目录）。
- 输出：`releases/<content-id>/` 下的不可变发布 + 原子切换的 `current/` 符号链接。

## 快速开始

```go
src := bundler.NewMemorySource().
    Add("app/main", "body\n// @import shared/util").
    Add("shared/util", "util body")

b := bundler.New(bundler.Config{
    Source:        src,
    CacheDir:      ".bundler/cache",
    OutputDir:     ".bundler/dist",
    Pipeline:      bundler.NewPipeline(bundler.TrimTransform{}, bundler.BannerTransform{Text: "v1"}),
    Entries:       []string{"app/main"}, // 为空表示所有模块都是入口
    Mode:          bundler.OutputSplit,   // 或 bundler.OutputSingle
    MissingPolicy: "error",               // 或 "stub"
    Workers:       0,                     // 0 = GOMAXPROCS
})
rep, err := b.Build(context.Background())
```

命令行/文件目录场景使用 `bundler.NewDirSource("path/to/workspace")`，目录中所有
`.src` 文件成为模块，ID 为相对路径去掉 `.src` 后缀（POSIX 斜杠），例如
`pkg/a.src` → `pkg/a`。以 `.` 开头的目录和 `node_modules` 会被跳过。

## 1. 依赖解析规则

### 1.1 依赖声明

每个模块是一个字节序列。依赖通过整行匹配的声明给出，支持两种写法（行首允许空白）：

```
// @import other/module
//@import other/module
```

- 以空白分词，取声明中的第一个 token 作为被依赖模块的 ID。
- 同一模块内重复声明只保留第一次（去重，保序）。
- 声明不做传递展开；图在所有模块上统一构建。
- 不是声明行的内容一律原样保留，因此转换步骤看到的仍是完整源码。

### 1.2 重复模块

同一 ID 被多个源文件提供时属于致命错误：解析阶段返回 `BuildErrors`，其中每一项是
可定位到 ID 与全部来源路径的 `DuplicateModuleError`，构建不会开始。

### 1.3 缺失依赖

边 `from -> dep` 指向不存在的模块时：

- `MissingPolicy: "error"`（默认）：返回 `BuildErrors`，每项是
  `MissingDependencyError{From, Dep}`，可定位到引用方与缺失项。
- `MissingPolicy: "stub"`：为缺失 ID 合成一个空模块（`Origin: stub:<id>`），构建
  继续，并在 `Report.Diagnostics` 中给出 `missing-stub` 警告。

### 1.4 循环依赖：可解释的确定性断环

循环既不静默丢弃，也不会造成无限递归或栈耗尽：

1. 使用**迭代式 Tarjan 算法**（显式工作栈）求强连通分量（SCC），模块图再大也不
   依赖 Go 调用栈深度。
2. 对每个成环 SCC（含自环）应用固定策略 `ascending-edges`：
   - SCC 成员按 ID 升序排列；
   - 仅保留 `from < to` 的边用于排序；
   - 切断 `from >= to` 的边（因此自环 `a -> a` 必被切断）。
3. 每个环都记录在 `Graph.Cycles` / `Report.Cycles` 中：SCC 成员、环内全部原始边、
   被切断的边、策略名；同时发出 `cycle-broken` 诊断，消息包含具体切断的边。
4. **模块上声明的原始依赖永不被改写**。切边只影响拓扑排序；闭包计算始终沿原始边
   遍历，所以环上每个模块的内容都会进入产物。
5. 切边后的图使用 Kahn 拓扑排序生成构建顺序，并列时按模块 ID 升序，保证顺序确定。

## 2. 转换流水线

`Pipeline` 是 `Transform` 的有序列表，内置：

- `TrimTransform`（名称 `trim`）：逐行去除首尾空白并丢弃空行；
- `BannerTransform`（名称 `banner:<text>`）：在头部追加固定注释，**不含时间戳**。

语义保证：

- 严格按顺序执行，前一步输出是后一步输入；
- 任一步失败立即中止该模块，返回定位到模块与步骤序号的
  `StepError{Module, Step, Name}`；
- 半加工内容不会进入产物（失败模块不产生输出，整个构建不发布任何产物）；
- 单模块失败不污染其他模块：错误聚合成 `BuildErrors`，互不相关的模块照常完成转换
  （但产物只在零错误时发布）；
- `context.Context` 取消会在步骤边界与提交边界生效。

## 3. 增量语义与缓存键

### 3.1 键的构成

模块缓存键只取决于**自身内容 + 全部上游依赖内容 + 流水线配置**，与路径、时间无关：

1. `contentHash = sha256(模块内容)`；
2. SCC 指纹（`scc1` 前缀的 sha256）：
   - 环外模块：对（自身内容哈希 ∪ 其外部依赖 SCC 指纹集合）做哈希；
   - 环内模块：整个 SCC 共享一个指纹，输入是**全部成员的内容哈希**与外部依赖 SCC
     指纹集合——因此环上任意一个模块变化都会使整环失效；
   - 缩点后的 SCC DAG 无环，外部依赖指纹按拓扑序必然先算好；
3. 模块键：`sha256("mod1" + 模块ID + pipelineID + scc指纹)`。

`pipelineID` 是流水线中各步骤 `Name()` 与顺序的哈希，因此增删改转换步骤会使所有
模块键改变，等价于一次干净的全量重算。

### 3.2 失效与复用

- 上游任意内容变化 → 上游 SCC 指纹变化 → 下游所有模块键变化 → 必然重算；
- 未变化的模块（及其未变化的依赖闭包）键不变 → 直接复用磁盘上的 blob；
- `Report` 报告 `CacheHits` 与 `Transformed` 数量，结果可核对。

### 3.3 全量与增量逐字节一致

- 全量构建（空缓存）与增量构建（暖缓存）对同一输入、同一配置产出**逐字节一致**的
  产物，因为转换是纯函数、排序与命名全部确定、产物头不含任何时间/路径信息；
- 发布目录名是完整产物集合内容的 sha256 摘要（`release1` 前缀），同一结果复用
  同一个 release，`current` 指向不变。

## 4. 持久化与损坏恢复

缓存目录布局：

```
<CacheDir>/
  index.json            # 单一 JSON 索引（临时文件 + rename 原子写入）
  blobs/<ab>/<sha256>   # 内容寻址的不可变转换结果
  lock                  # flock 跨进程咨询锁
```

- 索引首字段为 `format_version`；当前版本 `CacheFormatVersion = 1`。
- 打开缓存时若发现 JSON 损坏、截断、版本不兼容：损坏文件被改名为
  `index.corrupt-<nanos>` 隔离，写入 `RECOVERY.txt` 说明原因，缓存重置为空并**安全
  回退全量重建**。不崩溃，也不需要人工清理。
- blob 先写临时文件再 `rename`，读者永远看不到半写入内容。

## 5. 并发模型

- 模块转换由有界 worker 池并发执行（`Workers`，默认 `GOMAXPROCS`）；慢模块只占用
  自己的 worker，不会阻塞不相关模块。
- 同一进程内对相同缓存键做 `singleflight` 去重：并发构建不会重复计算同一模块。
- 多个 OS 进程共享同一缓存时，每个键的“索引重读 → 合并 → 原子重写”临界区由
  `flock` 串行化（`lock` 文件），任何时刻磁盘索引只有一个自洽版本；等待锁可被
  context 取消中断。
- blob 内容寻址、不可变，天然支持并发写与去重。

## 6. 失败、取消与恢复

- 取消或模块失败时：产物**完全不发布**，旧的 `current/` 继续指向旧 release，外部
  永远看不到半成品；
- 已提交到缓存的模块结果保持完整且自洽（纯函数结果，可任意次复用），重新构建时
  从最近一致位置继续：已提交的命中、未提交的重算，不会重复应用转换，也不会跳过
  变化；
- 发布流程：全部产物先写入 `.staging-*`（每个文件同样临时文件 + rename + fsync，
  目录 fsync），再整体 rename 为 `releases/<id>`，最后用临时符号链接 + rename 原子
  切换 `current`。

## 7. 源码增删改名与产物合成

每次构建都重新扫描工作区：

- 新增：新 ID 进入图，作为入口或被依赖时进入闭包；
- 删除：ID 消失，引用方会触发“缺失依赖”规则；删除的模块及其内容不再出现在任何
  产物中（旧缓存 blob 不再被键引用，也不影响输出）；
- 改名：等价于删除旧 ID + 新增新 ID，产物中只出现新名字。

产物模式：

- `OutputSingle`：一个 `bundle/bundle.out`，包含所有入口闭包；
- `OutputSplit`：每个入口一个 `entries/<encoded>.out`，`encoded` 是入口 ID 的无碰撞
  确定性编码（字母数字与 `._-` 保留，其他字节编码为 `_xx` 两位小写十六进制），
  因此命名稳定、可复现、无碰撞。

产物文本由确定性头部 + 按拓扑顺序排列的模块段组成，段标记形如
`// ===== begin module: <id> =====`。

## 8. 配置项

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `Source` | 必填 | `MemorySource` 或 `DirSource` 等 |
| `CacheDir` | `.bundler/cache` | 持久缓存目录 |
| `OutputDir` | `.bundler/dist` | 产物根目录（`releases/`、`current/`） |
| `Pipeline` | 空流水线 | 有序转换步骤；步骤 Name 与顺序进入缓存键 |
| `Entries` | 空（全部模块为入口） | 入口模块 ID 列表，内部排序后使用，顺序无关 |
| `Mode` | `OutputSingle` | `OutputSingle` / `OutputSplit` |
| `MissingPolicy` | `error` | `error`（构建失败）或 `stub`（空模块 + 警告） |
| `Workers` | `GOMAXPROCS` | 模块转换并发上限 |

## 9. 错误类型

| 类型 | 含义 |
| --- | --- |
| `DuplicateModuleError` | 同一 ID 多个来源（含全部来源路径） |
| `MissingDependencyError` | 引用了不存在的模块（含引用方） |
| `StepError` | 转换在某模块的某一步失败（含步骤序号与名称） |
| `ModuleError` | 缓存读写等模块级阶段错误 |
| `BuildErrors` | 多个相互独立错误的聚合，支持 `errors.Unwrap() []error` |

诊断（非致命，出现在 `Report.Diagnostics`）：`cycle-broken`、`missing-stub`、
`cache-recovered`。

## 10. 测试

`go test ./...`（建议加 `-race`）覆盖：

- 依赖解析：重复、缺失（两种策略）、三模块环与自环、确定性拓扑序；
- 增量：全量/暖缓存逐字节一致、上游叶节点变化级联失效、无关模块复用、删除依赖后
  旧内容消失、改名、拆分命名确定性；
- 缓存：垃圾 JSON、截断 JSON、版本不匹配三类损坏的隔离与回退、flock 获取/释放/
  取消；
- 并发：进程内并发构建（`-race`）与 4 个真实子进程共享同一份缓存；
- 中断恢复：构建取消后无半成品可见，续建精确复用已提交结果且最终逐字节一致；
- 目录源：`.src` 发现规则与磁盘改名的正确反映。
