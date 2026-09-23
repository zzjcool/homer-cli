# homer-cli M3 · S0-scout 侦察报告

> 只读侦察，生成于 M3 P0（与 M0-scaffold 并行）。本报告是 **W2-adapters 分类冻结依据**。
> 基线 commit：`c731e51`（m3-plan.md 冻结）；工作树已有 M0 的 `package.json`/`package-lock.json` 改动（含 `age-encryption@^0.3.1`）。

---

## 0. 结论速览（TL;DR）

| 项 | 结论 |
|---|---|
| **age-encryption npm 包** | ✅ **可用**（0.3.1，ESM-only，Node ~G==20.19，X25519 + 多 recipient + identityToRecipient 全部验证通过） |
| **本机 `age` CLI** | ❌ **没有**（`which age` 空、`age --version` command not found）→ W1 互操作用例须 skip |
| **herdr `~/.config/herdr` 分类** | 同步 **1 个文件**（`config.toml`，mirror），其余 7 项全为运行时状态/socket/日志 → 忽略。无额外可同步的用户配置目录 |
| **opencode `~/.config/opencode` 分类** | 同步 **3 个文件**（`opencode.json` merge + `package.json` merge + 锁文件 mirror），`node_modules/`/`.plugins.lock`/`*.log`/`.gitignore` 忽略。另有 `~/.local/share/opencode`（大数据/DB，**不**属配置，忽略） |
| **§2.8 初案裁定** | 与实测**一致**，仅需 Node 版本下限等微调（见 §3.3） |

---

## 1. herdr 侦察

### 1.1 `~/.config/herdr` 全清单（`ls -la ~/.config/herdr`）

```
drwxr-xr-x  ./
-rw-r--r--  .plugins.lock            0 B      插件锁
-rw-r--r--  config.toml            281 B      用户设置（onboarding/theme/ui/experimental）
-rw-r--r--  herdr-client.log    537 KB      客户端日志
srw-------  herdr-client.sock        0 B      Unix socket
-rw-r--r--  herdr-server.log    4.8 MB      服务端日志
srw-------  herdr.sock               0 B      Unix socket（HERDR_SOCKET_PATH 指向它）
-rw-r--r--  release-notes.json   10.9 KB     版本提示缓存
-rw-r--r--  session.json          6.5 KB     运行时布局状态（workspace/pane/cwd/agent session）
```

### 1.2 `config.toml` 实际内容（本机实测）

```toml
onboarding = false
[experimental]
kitty_graphics = true
[theme]
name = "gruvbox"
auto_switch = false
[ui]
status_indicators = "symbols"
```

**主题（theme）与 agent 定义都在 `config.toml` 内联**：
- 内置主题名单（herdr 二进制 strings）：`catppuccin, terminal, tokyo-night, tokyo-night-day, gruvbox, gruvbox-light, one-light, solarized, solarized-light, kanagawa-lotus, rose-pine, rose-pine-dawn`；自定义色走 `[theme.custom*]` 段，仍写在 config.toml。
- Agent 检测定义（agent-detection manifests）来自**远端 catalog**（`HERDR_AGENT_DETECTION_MANIFEST_CATALOG_URL=https://herdr.dev/agent-detection/index.toml`），本地缓存于 **`~/.local/state/herdr/agent-detection/`**（见 1.4），**不是** `~/.config/herdr` 下的用户配置文件。用户自定义 agent manifest 由 `herdr server agent-manifests` 管理，不落在 config 目录。

> **裁定：主题与 agent 定义无独立用户文件，全部内联于 `config.toml` → 同步 config.toml 即覆盖。无需新增分类。**

### 1.3 `~/.herdr/` 其他目录

```
~/.herdr/worktrees/{tdmq-appserver, herdr-subagents, pi-sysmon, ...}   ← herdr 托管的 git worktree checkout（机器本地路径，体积大）
```
- 证据：herdr 二进制含 `directory = "~/.herdr/worktrees"` 默认值，且 `HERDR_HOME` env 未设时默认 `~/.herdr`。
- 性质：**机器本地工作副本**（git checkout），含完整源码，绝对路径与机器绑定 → **必须忽略**，绝不进 store。

### 1.4 其他状态目录（非 config 路径，供忽略参考）

`~/.local/state/herdr/`（XDG state）：
```
agent-detection/{status.toml, remote/*.toml}   ← 远端 catalog 缓存（机器/版本相关）
client/{endpoint-selection.json, endpoints.json} ← client 端点状态
client-shell/local-*.json                       ← shell 会话状态
```
→ 全部**运行时状态，忽略**。

### 1.5 herdr 文件分类裁定表（W2 冻结依据）

| 路径（相对 root=~/.config/herdr） | 类别 | 裁定 | 依据 |
|---|---|---|---|
| `config.toml` | config | ✅ **同步（mirror）** | 唯一用户设置文件；含 onboarding/theme/ui/experimental |
| `session.json` | 运行时 | ❌ 忽略 | workspace/pane 布局 + 机器本地 cwd + agent session 路径（实测含 `/root/code/...`） |
| `*.sock`（herdr.sock / herdr-client.sock） | 运行时 | ❌ 忽略 | Unix socket，机器本地 |
| `*.log` | 运行时 | ❌ 忽略 | 客户端/服务端日志（实测 537KB/4.8MB） |
| `.plugins.lock` | 运行时 | ❌ 忽略 | 插件锁 |
| `release-notes.json` | 运行时 | ❌ 忽略 | 版本提示缓存，机器相关 |
| `~/.herdr/worktrees/**` | 其他 root | ❌ 不纳入 adapter | 机器本地 git checkout，超大 |
| `~/.local/state/herdr/**` | 其他 root | ❌ 不纳入 adapter | XDG 运行时状态 |

**§2.8 初案 vs 实测**：初案 `categories.config = {paths:['config.toml'], mode:'mirror'}` + ignore `['session.json','*.sock','*.log','.plugins.lock','release-notes.json']` —— **完全一致，无需修改**。

---

## 2. opencode 侦察

### 2.1 `~/.config/opencode` 全清单

```
drwxr-xr-x  ./
-rw-r--r--  .gitignore          45 B     （内容：node_modules / package.json / bun.lock / .gitignore）
-rw-r--r--  bun.lock           711 B
drwxr-xr-x  node_modules/       29 个包
-rw-r--r--  opencode.json    11.7 KB     主配置（provider/models/permission）
-rw-r--r--  package-lock.json 14.4 KB
-rw-r--r--  package.json        65 B     （dependencies: @opencode-ai/plugin 1.17.12）
```

### 2.2 `opencode.json` 结构（本机实测）

- 顶层键：`$schema`, `provider`（单 provider `ccrb`，含 `models` 大量模型定义 + `options.apiKey="ccrb"` 短占位 + `options.baseURL="http://0.0.0.0:3457/v1"`）, `permission`, `disabled_providers`。
- `apiKey` 值是短占位 `"ccrb"`，**不命中** M2 密钥扫描闸门（长 key 才会）— 与 §2.8 安全注记一致。
- 是合法 JSON，可走 merge 模式。

### 2.3 同目录旁证 / 其他 opencode 目录

| 路径 | 内容 | 裁定 |
|---|---|---|
| `~/.opencode/` | 与 `~/.config/opencode` 高度重叠（同 `.gitignore`/package.json/bun.lock），但**无 opencode.json**，含独立 `bin/` 与各自 node_modules | ❌ 不纳入（默认 root 是 `~/.config/opencode`；`~/.opencode` 是旧式/备用路径。**风险点：若用户机器用 `~/.opencode` 则配置不在默认 root——列入待验证**） |
| `~/.local/share/opencode/` | `opencode.db`（577 MB）+ `opencode.db-wal/shm/bak`、`auth.json`（2 B，`{}`）、`log/`, `repos/`, `snapshot/`, `storage/`（session diff jsonl）, `tool-output/`, `worktree/`, `bin/`（含 rg） | ❌ 全部忽略：大数据/DB/会话记录/凭据，均运行时；**不含用户设置** |
| `~/.local/state/opencode/` | `model.json`（237 B）、`frecency.jsonl`、`prompt-history.jsonl`、`locks/` | ❌ 忽略：运行时状态（含隐私 prompt 历史） |
| `~/.cache/opencode/` | `models.json`（4.5 MB）、`bin/`, node_modules | ❌ 忽略：模型元数据缓存 |

### 2.4 opencode 文件分类裁定表（W2 冻结依据）

| 路径（root=~/.config/opencode） | 类别 | 裁定 | 依据 |
|---|---|---|---|
| `opencode.json` | config | ✅ **同步（merge）** | 主配置，JSON；短 apiKey 占位不触发闸门 |
| `package.json` | plugins | ✅ **同步（merge）** | 插件依赖声明（@opencode-ai/plugin） |
| `package-lock.json` | locks | ✅ **同步（mirror）** | 锁文件 |
| `bun.lock` | locks | ✅ **同步（mirror）** | 锁文件（opencode 用 bun） |
| `node_modules/` | 运行时 | ❌ 忽略 | 依赖实体，体积大 |
| `.plugins.lock` | 运行时 | ❌ 忽略 | 插件锁（本机实测目录中当前**不存在**，但已在 ignore 列表） |
| `*.log` | 运行时 | ❌ 忽略 | 日志 |
| `.gitignore` | 元数据 | ❌ 忽略 | opencode 自生成的 gitignore（含 package.json/bun.lock——若同步会被 tool 自身忽略，无意义） |

**§2.8 初案 vs 实测**：初案分类 `{config:{opencode.json,merge}, plugins:{package.json,merge}, locks:{package-lock.json,bun.lock,mirror}}` + ignore `['node_modules/','.plugins.lock','*.log','.gitignore']` —— **与实测一致，无需修改**。

> ⚠️ 注意点（写进 W2 任务书）：opencode 自带的 `.gitignore` 把 `package.json`/`bun.lock` 也 ignore，但 homer 的 store 是**独立 git 仓库**（homer home），不受 opencode 的 .gitignore 影响，所以同步这两个文件没问题。

---

## 3. age-encryption 包调研

### 3.1 npm 元数据

| 项 | 值 |
|---|---|
| 包名 / 版本 | `age-encryption` **0.3.1** |
| 描述 / 许可 | FiloSottile/typage（官方 age 作者的纯 JS/TS 实现） / **BSD-3-Clause** |
| 类型 | `"type":"module"`，**ESM-only**（`exports: "./dist/index.js"`，无 CJS 入口） |
| 依赖 | `@noble/ciphers ^2.1.1`, `@noble/curves ^2.0.1`, `@noble/hashes ^2.0.1`, `@noble/post-quantum ^0.5.3`, `@scure/base ^2.0.0` |
| Engines | `@noble/*` 均声明 `node ~G== 20.19.0`（间接约束） |
| 维护 | 由 age 官方作者 Filippo Valsorda 维护；mtime 2026-08-28（活跃） |
| 仓库 | github.com/FiloSottile/typage |

> ⚠️ **homer-cli 是 ESM 还是 CJS？** 需 M0/orchestrator 确认（本侦察未核对 tsconfig module 字段）。若 homer 编译目标是 CJS，`import('age-encryption')` 需走动态 import。**列入待验证**。

### 3.2 API 验证代码片段（/tmp 同环境直接跑，Node v22.23.1）

**验证 1 — X25519 生成 → recipient 派生 → roundtrip：**
```js
import * as age from "age-encryption";
const secret = await age.generateIdentity();          // "AGE-SECRET-KEY-1..."
const recip  = await age.identityToRecipient(secret); // "age1..." (62 字符)
const enc = new age.Encrypter(); enc.addRecipient(recip);
const ct = await enc.encrypt("hello-homer-m3");        // Uint8Array (214 bytes)
const dec = new age.Decrypter(); dec.addIdentity(secret);
const pt = await dec.decrypt(ct, "text");              // "hello-homer-m3"
```
→ 实测输出：`recipient: age1vq2nhdj7sf72x86xc4hfzsjhlvln7llxuh6wxf0rq49eje7mtunqunk36n`，`roundtrip: OK`。

**验证 2 — 多 recipient（3 个）+ 二进制 payload + 非 recipient 拒绝 + 非法 recipient 报错：**
```js
const ids = await Promise.all([...Array(3)].map(()=>age.generateX25519Identity()));
const recips = await Promise.all(ids.map(age.identityToRecipient)); // 均 age1 前缀、62 字符
const enc = new age.Encrypter(); for (const r of recips) enc.addRecipient(r);
const plain = Buffer.from([0,1,2,3,255,254,10,13,0]);
const ct = await enc.encrypt(new Uint8Array(plain));
// 3 个 identity 各自解密 === plain → 全部 OK
// 非 recipient identity：decrypt 抛 Error(no identity matched any of the file recipients)
// 非法 recipient：age1invalid → addRecipient 抛 Error(Unknown letter i...)
```
→ 实测：`multi-recipient binary roundtrip all: true`；outsider throws；bad recip throws。

**验证 3 — armor（ASCII 封装，可选）+ recipient 正则：**
```js
const armored = age.armor.encode(ct);        // "-----BEGIN AGE ENCRYPTED FILE-----"
const back    = age.armor.decode(armored);   // Uint8Array，可解密
/^age1[02-9ac-hj-np-z]{58}$/.test(recip);    // true → 与 §2.2 recipientIsValid 正则完全吻合
```

**关键 API 签名（来自 dist/*.d.ts）：**
- `generateIdentity(): Promise<string>` / `generateX25519Identity(): Promise<string>` / `identityToRecipient(identity): Promise<string>` — **均返回 Promise（异步）**，与 §2.2 冻结的 `generateIdentity(): AgeIdentity`（同步）**不一致 → 见 §3.4**。
- `Encrypter.addRecipient(s: string | Recipient): void` — 可多次调用（多 recipient）。
- `Decrypter.addIdentity(s: string | CryptoKey | Identity): void`；`decrypt(file: Uint8Array, outputFormat?): Promise<Uint8Array|string>`。
- 有 `armor` / `webauthn` 子命名空间（不需要）。

### 3.3 Node ~G== 20 兼容

- `package.json` 无顶层 `engines`，但依赖 `@noble/* ^2.x` 均要求 `node ~G== 20.19.0`。
- 本机 Node **v22.23.1** 实测全部通过。**Node 20.19.0 为实际下限**（非“~G==20”泛化）；install.sh 的版本检查应至少 **~G==20.19**。

### 3.4 结论

> ✅ **可用（use age-encryption）**
> 不需要 fallback（node:crypto 自实现）。**但有一处接口适配需在 W1 落地：**
> §2.2 冻结签名 `generateIdentity(): AgeIdentity`（同步）与包 API `generateIdentity(): Promise<string>`（异步）冲突。`createAgeCryptoPort()` 封装点可吸收此差异（在 W1 内 await 包装即可），但 `AgeCryptoPort.encrypt/decrypt` **已经是 Promise 返回**，与包一致。**建议 orchestrator 在 W1 任务书里注明 `generateIdentity` 为 async 或改为 await 调用**（属实现细节，不改冻结签名也行，只要 W1 内部 await）。

**风险**：ESM-only。若 homer-cli 产出 CJS，需动态 import（`const age = await import('age-encryption')`）；见待验证。

---

## 4. age CLI 确认

```
$ which age        → (空，exit 1)
$ age --version    → bash: age: command not found (exit 127)
PATH 搜索完毕：/root/.pi/agent/bin, node, maven, java, sdkman, cargo, go, /usr/local/bin, /usr/bin ... 无 age
```
> ❌ **本机无 `age` CLI**。→ W1 任务书中「若本机有 age CLI 则加互操作用例」**应 skip**（用 skipIf 检测 which age 为空）。不阻塞 W1。

---

## 5. 风险与坑

1. **ESM-only 依赖**（age-encryption + @noble/*）：homer-cli 若为 CJS 需动态 import。**最高优先级待验证项**。
2. **`generateIdentity` 同步/异步签名不一致**：§2.2 写同步，包是 Promise。W1 需 await 包装。
3. **herdr agent/theme 定义**：无独立用户文件（内联 config.toml / 远端 catalog 缓存）。若未来 herdr 版本改为独立 `agents.toml`，分类需重审——**版本敏感点**。
4. **opencode 双路径**：`~/.config/opencode`（默认，含 opencode.json）vs `~/.opencode`（无 opencode.json）。默认 root 固定 `~/.config/opencode`（符合 XDG，实测主配置在此），可接受，但 README 应提示。
5. **opencode DB 巨大**（577 MB `opencode.db`）：明确不纳入 adapter，避免误同步。
6. **`~/.herdr/worktrees` 是完整 git checkout**：明确不纳入，否则 store 会爆炸。

---

## 6. 待验证问题

1. **homer-cli 编译目标是 ESM 还是 CJS？** → 决定 age-encryption 是否需要动态 import。（未核对 tsconfig module 字段）
2. **Node 版本下限**：install.sh 应检查 ~G==20.19（非 ~G==20）—— 需在 M0/W4 落实。
3. `~/.local/state/opencode/model.json`（最近模型选择）是否值得可选同步？默认裁定忽略；如需，属 M4+。
4. herdr 未来版本是否会新增 `~/.config/herdr` 下独立文件（如 agents.toml / keybindings）？当前 0.9.1 无。
5. `generateIdentity` 的同步/异步签名冲突最终如何处理——交由 orchestrator 裁定。

---

## 附：证据文件路径汇总

| 主张 | 证据 |
|---|---|
| herdr 目录清单 | `ls -la ~/.config/herdr`（§1.1） |
| herdr config.toml 内容 | `cat ~/.config/herdr/config.toml`（§1.2） |
| herdr 主题/agent 内联 | herdr 二进制 `strings`（内置主题名单 + agent-detection catalog URL） |
| herdr worktrees 默认路径 | herdr 二进制 strings `directory = "~/.herdr/worktrees"` |
| opencode 清单 | `ls -la ~/.config/opencode`、`~/.local/share/opencode`（§2.1/§2.3） |
| opencode.json 结构 | `cat ~/.config/opencode/opencode.json`（§2.2） |
| age API | `node_modules/age-encryption/dist/{index,recipients,x25519}.d.ts` + 实测（§3.2） |
| age 无 CLI | `which age` / `age --version` = not found（§4） |
