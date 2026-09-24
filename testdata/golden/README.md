# Go 重写黄金向量（P0）

这些向量全部由一次性导出脚本从 `legacy/ts`（`ts-v1.0.0`）的 TypeScript 引擎生成，输入数据均为本项目自造的测试数据，不含任何真实用户配置或真实密钥。

目录与数量：

- `merge/`：32 组 `mergeJson`（8 个语义情形 × 3 种键序变体，另加边界情形）
- `mirror/`：15 组 `compareFile` / `compareCategory`（包含 3×3 九格矩阵）
- `planpull/`：20 组 `planPull` 及 `excludeKeys` 三重语义（push 占位符、判定剥离、local 植回）
- `firstcontact/`：8 组 `planFirstContact`（`pull` / `merge` / `skip` 三模式）
- `age/`：4 组 TS `AgeCryptoPort` 密文互操作向量（单 recipient、多 recipient、空明文、Unicode 明文）

`age/` 中的 `identities` 是**临时生成的测试密钥，仅用于互操作向量**；它们只加密本目录中同时给出的自造测试明文，设计上公开在 testdata 中，绝不代表真实密钥。密文采用 age v1 二进制格式，以 `ciphertextBase64` 保存；`recipients` 与 `identities` 一一对应。除 `age/` 向量明确需要的测试 identity 外，黄金数据中不出现任何完整 AGE identity 或其他真实密钥。

每个 JSON 文件都包含 `input` 与 `output`，并标明生成它的 TypeScript 操作。JSON 内容由 Go 版后续 worker 逐字节/逐字段回放校验；不要手工格式化或替换向量。
