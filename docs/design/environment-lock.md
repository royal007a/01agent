# 环境锁定（Environment Lock）

> 坏味道：**环境漂移**。Agent 运行所依赖的条件（运行时、依赖、外部服务、模型）没有被显式声明和锁定，却在悄悄变化，于是行为不可复现、错误无法定位。
> 重构手法：**环境锁定**。锁定可控项，隔离不可控项；把环境也纳入版本控制，让 Harness 从"这次能跑"变成"随时可复现"。

## 1. 漂移的两个方向

| 方向 | 例子 | 影响 |
|---|---|---|
| 时间上 | 模型同名换代（deepseek-v4-flash → 4.1），API 改版，依赖升级 | 同一台机器，昨天能跑今天不行 |
| 空间上 | shell（macOS bash 3.2 vs Linux bash 5）、BSD/GNU 工具差异、PATH 不同 | 换台机器、换个人就失效；影响部署与分发 |

go-tiny-claw 里现成的空间漂移：launchd 服务的 PATH 和交互 shell 不同，同一台机器上两边看到的 `go`/`rg` 可能就不是同一个。

## 2. 四类环境在 go-tiny-claw 中的现状与锁定方式

| 类别 | 锁定前 | 锁定后 |
|---|---|---|
| 运行时 | 只有 `go.mod` 的 `go 1.26`（下限） | `toolchain go1.26.4` + `GOTOOLCHAIN` 显式固定；锁文件记录 OS/arch/Go 与 Agent 调用的外部命令（bash/git/rg/go/grep/sed）的路径与版本；`scripts/runtime-env.sh` 固定 PATH/locale |
| 依赖 | `go.sum` 已有，但从不校验 | `init.sh` 跑 `go mod verify`；锁文件记录二进制内嵌的依赖 `version+sum`（BuildInfo，编译期即确定） |
| 外部服务 | 只有 LLM API | Provider 抽象就是间接层（对应材料中的 MCP 间接层）：厂商协议变化只改 `internal/provider`，引擎不动。当前没有接 MCP |
| 模型 | 只锁了请求名 | 锁文件同时记录**请求名**与**服务端回报名**（`resp.model`）；`ModelGuard` 在每次响应上核对 |
| Skill | 已本地化在 `<workdir>/.claw/skills/` | 第三方 Skill 放 `.claw/skills/third-party/`，每个文件锁 sha256；**不跟随符号链接**，链接到工作区外的 Skill 视为偷偷引入全局 Skill，即使锁里记录过也判错 |

## 3. 组件

- `internal/envlock`
  - `Capture` 拍快照 → `Lock`；`Save/Load` 读写 `<workdir>/.claw/env.lock.json`（损坏/版本不符/mode 非法一律报错，**绝不当作"未锁定"放行**）。
  - `Diff(locked, current)` 按类别给出漂移与严重级：
    - error：OS/arch、工具消失或版本变化、依赖增删改、Skill 增删改、Provider/BaseURL/模型名变化、实际模型版本变化；
    - warn：Harness 的 Go 运行时变化、新出现的工具、同版本换位置。
    - 只比较锁里**声明过**的工具（声明即契约）；实际模型版本任一方未知时不比较（未探测 ≠ 漂移）。
  - `Enforce(mode)`：warn 只报告；strict 下有错误级漂移即拒绝。
  - `ModelGuard`：装饰 Provider。有锁定的实际版本就按它核对；没有就以本进程首次观测为准——长驻服务运行途中模型滚动也能发现。同一漂移只报一次、次数累计；strict 下直接返回错误，不在不同的模型上悄悄继续跑。
- `app.Build`：工作区有锁 → 启动前比对（strict 拒绝启动）；无锁 → 只挂哨兵。
- CLI：`claw env lock [-mode warn|strict] [-probe] [-tools ...]`、`claw env check`（有错误级漂移退出码 1，未锁定退出码 2）、`claw env show`。`-probe` 发一条极短请求拿到服务端回报的实际版本；重新 lock 时若模型配置未变且未重新探测，保留旧的实际版本。
- Benchmark：报告带 `env` 快照与逐用例 `observed_model`。与基线环境不一致时，报告顶部标注"以下回归/修复不能直接归因到代码改动"；同一次跑分出现多个模型版本也会标出。旧报告没有快照视为"未测"，不报差异。

## 4. 四步锁定

1. **环境初始化** `init.sh`：固定 PATH → 固定工具链（`GOTOOLCHAIN=$(toolchain 行)`，本机版本不同自动下载精确版本）→ `go mod verify` → `-trimpath` 构建 → 工作区无锁则锁定、有锁则比对（错误级漂移失败退出）。`make deploy` 在重启服务前跑它，部署即门禁。
2. **本地化 Skill**：只从工作区 `.claw/skills/` 加载（loader 本来就不读全局目录），第三方放 `third-party/` 并锁哈希。
3. **间接层**：Provider 抽象吸收外部 API 变化；模型滚动由 `ModelGuard` 暴露。
4. **Docker 镜像**：基础镜像按 **digest** 锁定（tag 会漂移），构建期 `GOTOOLCHAIN` 固定 + `go mod verify`，运行时镜像带齐 Agent 要调用的工具与 Go 工具链；构建期在 `/opt/claw` 留一份镜像环境快照；入口脚本对挂载的工作区"首次锁定、之后比对"，镜像升级换了工具也会在启动时暴露。

## 5. 何时重构 / 日常操作

- 从项目开始就配置，不等故障出现；换机器、换使用者、对外分发之前必须完成。
- 有意升级（改依赖、换模型、升级 Skill）后：`make env-lock`（可加 `MODE=strict PROBE=1`），锁文件的 diff 就是这次环境变更的 review 记录。
- 无法复现上次成功时，先 `make env-check`：漂移已经发生的话它会直接告诉你是哪一项。

## 6. 已知局限

- 锁文件放在 Agent 工作区（Agent 的环境），不是 Harness 仓库；工作区若不在版本控制下，锁的历史需要自行保存。
- 工具版本来自 `--version` 首行，BSD `sed` 等拿不到版本只记 `unknown`（仍能发现消失/换位置）。
- 实际模型版本依赖厂商在响应里回报 `model` 字段；有的厂商只回报别名，此时哨兵只能发现别名变化。
- 没有接 MCP；外部服务目前只有 LLM，由 Provider 层承担间接层职责。
- Docker 镜像内 `apk add` 未钉包版本（Alpine 仓库会下架旧版本，钉死反而构建不可重复）；镜像本身按 digest 分发才是锁定单位。
