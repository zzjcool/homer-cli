# M3 · P1-W1-age 报告（age 密钥层：AgeCryptoPort + keys + vault）

> 分支 `m3-p1-w1-age`，commit `f170e55`（base `4251323`，worktree `/root/code/homer-cli-w1`）
> 依据：`docs/m3-plan.md` §2.2 / §3-P1-W1 + `docs/m3-scout-report.md` §3（age 包 API 实测结论）

---

## 1. 做了什么

### 1.1 `src/core/age/cipher.ts`（新，309 行）

`AgeCryptoPort` 的**唯一**实现点（§2.2 冻结接口，签名逐字未改）。

**关键决策：X25519 用 `node:crypto` 而不是包的 async API。**
§2.2 冻结签名 `generateIdentity(): AgeIdentity` 是**同步**的，而 `age-encryption@0.3.1` 的
`generateIdentity()` / `identityToRecipient()` 都返回 Promise（S0 §3.4 实测）。做法：

- 私钥 = `randomBytes(32)`；公钥 = `createPublicKey(createPrivateKey(PKCS#8(prefix‖scalar)))`
  的 SPKI 末 32 字节（包内部 `importX25519Key` 同款做法，且不做 clamp，与包一致）；
- 两种字符串用自实现的 BIP-173 bech32（`AGE-SECRET-KEY-` 大写 / `age` 小写）编码。

**正确性由测试锁定，不靠推理**：派生的 recipient 与包 `identityToRecipient()` **逐字节**
一致，且双向互操作（包加密→我们解密、我们加密→包解密）均通过。

**安全加固（相对直接用包）**：

| 项 | 包的行为 | 我们的行为 |
|---|---|---|
| 零 recipient 加密 | **静默成功**，产出无 recipient stanza 的 103 字节密文（实测）→ 密钥永久不可解 | `CliError` 拒绝 |
| 非法 recipient | 抛 `Error(Unknown letter i...)` 等（部分路径回显入参） | `CliError`，消息**不回显入参**（入参可能是误配的私钥） |
| 解密失败 / identity 非法 | 错误消息细节不可控 | 固定文案 `CliError`，不含密文/私钥细节 |
| bech32 解码错误 | `Invalid checksum in <整串>`（**把私钥写进异常**） | 自实现解码，错误消息不含入参（测试锁定） |

**惰性加载（一处被测试兜住的性能/架构修正）**：`types.ts` 从 `cipher.ts` 转出
`createAgeCryptoPort` 后，产生静态链 `config.ts → age/types.ts → age/cipher.ts`。
若 `cipher.ts` 顶层 `import 'age-encryption'`，「读 homer.json」（`homer status`/`init`/`doctor`
的必经第一步）就会连带加载整包密码学实现（实测 ~90ms + `@noble` 全家桶），
违反 `types.ts` 文件头写下的不变量。改为**调用期 `await import()`**（`encrypt`/`decrypt`
本就是 async），静态图中只剩内建的 `node:crypto`。

> 该问题我用 Node 加载钩子实测确认（`config.js` 确实拖入了 `age-encryption`），修好后
> 加了回归测试，并**用 mutation 验证过测试会红**（临时改成顶层 import → 用例失败）。

### 1.2 `src/core/age/keys.ts`（新，149 行）

`generateIdentity`（同步、纯内存、不落盘）/ `identityFilePath`（`<keysDir>/age.txt`）/
`writeIdentityFile` / `loadIdentity` / `parseIdentityFile`。

- **拒绝覆盖**：见已存在文件即 `CliError`（含路径 + hint）。覆盖 = 永久销毁旧机可解的历史
  密文，故零容忍；测试断言原文件**逐字节未变**。
- **0600 + 0700 目录 + 原子写**：`<file>.tmp-<pid>` + `rename`；写入前 `chmod 0600` 再设一次
  （对抗异常 umask——测试用 `umask(0)` 验证不被放宽）；失败路径 `rmSync(tmp)` 不留半个私钥。
- **`loadIdentity` 不 throw**：缺失 / 不可读 / 内容非法 / 多密钥 / EISDIR → `undefined`，
  与 `state.ts::loadState` 同款容错（读路径的职责是给出可判定的「有没有」）。
- **`parseIdentityFile`**：取首个非空非 `#` 注释行（兼容 age CLI 的 key 文件格式）；
  第二个私钥行 → `CliError`（单密钥模型，静默丢弃一个会让用户以为生效了）。
  所有错误消息**不含内容本体**（只报行数等元信息）。

### 1.3 `src/core/age/vault.ts`（新，202 行）

`secretFilePath` / `secretNameValid` / `encryptSecretToFile` / `decryptSecretFromFile` /
`listSecrets` / `VaultEntryStatus`。

- **一密钥一 `<secretsDir>/<name>.age`**，name 逃逸闸门（`secretNameValid` + ≤128 字节），
  路径必落在 `secretsDir` 内（测试断言）。
- **密文自检（§1-D4 冻结）**：写盘**前**断言密文不含明文的任何 >16 字节片段
  （取首/中/尾三处采样）。测试用**注入的假 crypto**（返回明文的「加密」）验证：
  失败时**不写盘**、不留 tmp。采样覆盖首/中/尾（有专门用例验证「只查首段会漏」的尾段泄漏）。
- **原子写**：tmp + rename，成功后目录内只有 `.age` 文件。
- **`decryptSecretFromFile`**：§2.2 签名只注入 `crypto`，故 identity 由函数内部
  `loadIdentity(paths)` 取得（§2.6 pull / §2.5 doctor `checkAge` 都按此签名调用）。
  三类失败（vault 缺失 / identity 缺失 / 解不开）都在**写任何目标文件之前**抛 `CliError`，
  满足 §2.6 的「全有或全无」。
- **`listSecrets`**：纯读（`existsSync`，不解密、**不需要 identity**、不碰目标路径），
  按 name 字典序排序（报告稳定可断言）。

### 1.4 `src/core/age/index.ts`（新，45 行）

门面。§2.2 把 `recipientIsValid` / `secretNameValid` 列在**多个文件**名下（types 是权威实现，
keys/vault 各 re-export 一次以对齐计划字面归属），故门面**逐项显式导出**而非 `export *`
（避免撞名歧义），同名符号只保留一份实现。

### 1.5 `src/core/age/types.ts`（改，+12/−13）

**删除 P0 stub**（任务书授权：「你实现真 port，替换 stub 但不改接口签名」）。改为
`export { createAgeCryptoPort } from './cipher.js'`（**值转出**，非包装函数）→
`types.createAgeCryptoPort === cipher.createAgeCryptoPort` 同一函数对象，
P0 契约（`types.ts` 导出该符号）保持，实现不漂移。`AgeIdentity`/`AgeCryptoPort`
两个 interface 与两个校验函数**一字未动**。

### 1.6 `tests/core/m3-p0-scaffold.test.ts`（改，+19/−4）— ⚠️ 越界说明

该文件**不在任务书声明的文件范围内**，但其第 143-147 行断言
「`createAgeCryptoPort()` 抛 `CliError('尚未实现')`」，即**专门锁定 P0 stub**，
且用例标题自带「（实现由 W1 落地）」。替换 stub 必然使其失败——这是计划设计的
P0 阶段标记而非计划错误，故做最小更新：改为断言「拿到真 port + `types`/`cipher`
同源 + 可 roundtrip」。**若 orchestrator 认为该文件应归 W9 统一处理，可单独 revert
该文件的改动**（其余改动自洽，仅该 1 个用例会红）。

---

## 2. 测试覆盖（新增 117 例）

`cipher 32+1skip / keys 34 / vault 42 / index 11`（`tests/core/age/`，4 文件）

| 验收项（§3-P1-W1 原文） | 覆盖 |
|---|---|
| keygen→write→load roundtrip（recipient 一致、0600） | `keys.test.ts`：roundtrip + mode 0600 + 目录 0700 + umask(0) 不放宽 + 内容 = 一行私钥+换行 |
| 重复写入拒绝覆盖 | 3 用例：拒绝 + 原文件逐字节未变 + 幂等 + 空文件占位也拒 |
| `parseIdentityFile` 非法→CliError 无私钥泄漏 | 7 种非法形态 + 大小写规范化 + 多密钥 + checksum 篡改；断言消息不含密钥材料（含 12 字符滑窗抽查） |
| 多 recipient（2 identity 各自可解）roundtrip | `cipher`：2/3 个 recipient + 重复 recipient + **与参考实现交叉**；`vault`：换设备侧 |
| 非法 recipient→throw | 8 种非法（含「把私钥填进 recipients」）+ 空数组 + 断言消息不回显 |
| vault 原子写 + 密文断言 | 无 tmp 残留 + 假 crypto 泄漏被拒（不写盘）+ 尾段采样 + 空密文 + 短明文不误报 + 真实现多形态 |
| `listSecrets` present/missing | 混存 / 无需 identity / 缺省段 / 字典序 / 目录占位 / 纯读不建目录 |
| 互操作用例（有 age CLI 则加） | **skip**（S0 结论：本机无 CLI）；改由 `age-encryption` 官方实现充当互操作基准（双向 + 多 recipient 交叉），并以 `skipIf` 保留 CLI 用例占位 |

**额外**：与包 `identityToRecipient` 逐字节一致（12 轮）、bech32 与 `@scure/base` 一致、
1 MiB / 256 KiB 大 payload、二进制含 `0x00/0xFF/CRLF`、惰性加载不变量（mutation 验证）。

**硬约束遵守**：所有 identity 一律 `generateIdentity()` 临时生成 + `mkdtemp`
（+`getHomerPaths({HOMER_HOME})` 注入），**绝不落真实 HOME**。已实测验证：
`~/.homer` 与 `~/keys` 不存在（无 `age.txt` 落到 HOME 外任何位置），tmp 无残留。

---

## 3. 验证输出（原样）

```
$ npm run typecheck
> homer-cli@0.0.0 typecheck
> tsc --noEmit
（无输出 = 绿）
```

```
$ npm test
> homer-cli@0.0.0 test
> vitest run

 RUN  v5.0.1 /root/code/homer-cli-w1

 Test Files  42 passed (42)
      Tests  914 passed | 1 skipped (915)
   Start at  06:56:45
   Duration  25.41s (tests 70%, transform 25%, import 5%)
```

```
$ npx vitest run tests/core/age

 RUN  v5.0.1 /root/code/homer-cli-w1

 Test Files  4 passed (4)
      Tests  119 passed | 1 skipped (120)
   Start at  06:57:11
   Duration  4.63s (tests 91%, transform 7%, import 2%)
```

逐文件：`cipher 32 passed | 1 skipped (33)` / `keys 34 passed` / `vault 42 passed` /
`index 11 passed`。

基线 794 → 914 passed（+120 计入口径），1 skipped = age CLI 互操作用例。

---

## 4. MR / 分支

分支 `m3-p1-w1-age` **已推送**（`origin/m3-p1-w1-age`，commit `f170e55`）。

**PR 未能自动创建**：本机无 `gh` / `glab` CLI，无 `GH_TOKEN`/`GITHUB_TOKEN`，
匿名 GitHub API 被限流（HTTP 403 `API rate limit exceeded`）——与 `docs/m3-p0-report.md`
§6-1 记录的限制相同。请由 orchestrator 侧创建：

- https://github.com/zzjcool/homer-cli/pull/new/m3-p1-w1-age
- 或 `gh pr create --base master --head m3-p1-w1-age`

---

## 5. 未决问题 / 需 orchestrator 知晓

1. **⚠️ 越界改动了 `tests/core/m3-p0-scaffold.test.ts`**（见 §1.6）。原因：该文件锁定了
   被任务书明确要求替换的 P0 stub。改动最小（1 个用例），可单独 revert。
   `src/core/age/types.ts` 的改动在任务书授权范围内（「替换 stub 但不改接口签名」）。
2. **§2.2 与实现的签名归属偏差（已按阻塞最小原则处理）**：§2.2 冻结签名里
   `createAgeCryptoPort()` 是**同步**、`generateIdentity()` 是**同步**，而包 API 全 async。
   我的处理：两者都对上层保持同步（`generateIdentity` 用 `node:crypto` 真同步；
   `createAgeCryptoPort` 同步返回 port 对象、包在首次加解密时动态加载）。
   S0 §3.4 建议「W1 内 await 包装」，即把 `generateIdentity` 变 async——**我没有这么做**，
   因为那会改冻结签名（违反「不改接口签名」），且异步 keygen 无收益。
   如 orchestrator 更倾向 async 版，需同步修订 §2.2 文本再改。
3. **`decryptSecretFromFile` 自行加载 identity**：§2.2 签名只注入 `crypto`，而 §2.5/§2.6
   都按此签名调用，故「取本机 identity」被实现为该函数的内在职责。若 W6/W7 希望
   由命令层注入 identity（例如为了产出更精确的 `no-identity` 状态），需在 §2.2 加一个
   可选 identity 参数——**属接口变更，我未擅自添加**。
4. **`secretFilePath` 非法 name 抛 `CliError` 而非裸 `Error`**：§2.2 原文写「非法 name → throw」
   未指定类型。选 `CliError` 以便 CLI 命令层统一渲染 + exit 1（与仓库既有约定一致）。
5. **`VaultEntryStatus` 双份声明**：`src/cli/commands/secret.ts` 有一份 P0 的逐字副本 +
   `TODO(P1-W1)` 说落地后改为 import。**该文件不在我的文件范围内，未改动**；
   W6 落地时按 TODO 换成 `import type { VaultEntryStatus } from '../../core/age/vault.js'`
   （形状逐字相同，非接口变更）。
6. **超长 name 上限 128 字节**：§2.2 正则无长度限制，但 `<name>.age` 必须能作为单个文件名
   （POSIX 上限 255）。我加了 128 字节闸门（超出 → `CliError`）。属实现细节，
   若认为不必要可放宽。
