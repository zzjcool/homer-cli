#!/usr/bin/env bash
# homer hub 容器化 E2E 冒烟（plan §5 P2/P3 验收序列）。
# 用法: bash e2e/hub-smoke.sh   （需要 docker compose 与 jq）
set -euo pipefail

cd "$(dirname "$0")"
COMPOSE="docker compose -f docker-compose.yml"

HUB_URL=http://localhost:17760
AUTH="Authorization: Bearer test-token"
need() { command -v "$1" >/dev/null || { echo "缺少依赖: $1"; exit 2; }; }
need curl
need jq

teardown() {
  $COMPOSE down -v >/dev/null 2>&1 || true
}
trap teardown EXIT

echo "==> 构建并启动 compose 拓扑（origin + hub + agent-a + agent-b）"
$COMPOSE up -d --build >/dev/null

wait_for() {
  local desc="$1" url="$2" extra="${3:-}"
  for _ in $(seq 1 30); do
    if curl -fsS $extra "$url" >/dev/null 2>&1; then
      echo "    OK: $desc"
      return 0
    fi
    sleep 2
  done
  echo "    FAIL: $desc ($url)"
  $COMPOSE ps
  return 1
}

echo "==> 1. hub 健康检查"
wait_for "GET /api/health" "$HUB_URL/api/health"

echo "==> 2. 双 agent 注册（listen 的 agent-a + connect 的 agent-b）"
for _ in $(seq 1 30); do
  if curl -fsS -H "$AUTH" "$HUB_URL/api/agents" 2>/dev/null \
      | jq -e '[.agents[].agentId] | contains(["agent-a","agent-b"])' >/dev/null; then
    echo "    OK: agent-a 与 agent-b 均已注册"
    break
  fi
  sleep 2
  if [ "$_" = "30" ]; then
    echo "    FAIL: agents 未注册"
    curl -fsS -H "$AUTH" "$HUB_URL/api/agents" 2>/dev/null | jq . || true
    exit 1
  fi
done

echo "==> 2b. 机器资源快照（agent 自己采集的 CPU / 内存 / 网卡）"
for id in agent-a agent-b; do
  ok=0
  for _ in $(seq 1 15); do
    if curl -fsS -H "$AUTH" "$HUB_URL/api/agents" 2>/dev/null \
        | jq -e --arg id "$id" '
            .agents[] | select(.agentId == $id) | .host
            | (.memory.total > 0)
              and (.cpu.cores > 0)
              and (.cpu.usage != null)
              and ([.nets[]? | select((.addrs // []) | length > 0)] | length >= 1)
          ' >/dev/null; then
      echo "    OK: $id"
      curl -fsS -H "$AUTH" "$HUB_URL/api/agents" \
        | jq -c --arg id "$id" '.agents[] | select(.agentId == $id) | {agentId, host: {os: .host.os, distro: .host.distro, cpu: .host.cpu, memory: .host.memory, nets: .host.nets}}'
      ok=1
      break
    fi
    sleep 2
  done
  if [ "$ok" != 1 ]; then
    echo "    FAIL: $id 没有上报可用的资源快照"
    curl -fsS -H "$AUTH" "$HUB_URL/api/agents" | jq . || true
    exit 1
  fi
done

echo "==> 3. 远程采集（listen 直连）"
curl -fsS -H "$AUTH" -X POST "$HUB_URL/api/agents/agent-a/status" \
  | jq -e '.report.adapters | length >= 1' >/dev/null
echo "    OK: agent-a status"

echo "==> 4. 远程采集（connect 投递）"
curl -fsS -H "$AUTH" -X POST "$HUB_URL/api/agents/agent-b/status" \
  | jq -e '.report.adapters | length >= 1' >/dev/null
echo "    OK: agent-b status"

echo "==> 5. 制造漂移 → agent-a 远程 push"
$COMPOSE exec -T agent-a sh -c \
  'printf "\n// hub-e2e\n" >> /root/toolhome/.pi/agent/settings.json'
curl -fsS -H "$AUTH" -X POST \
  "$HUB_URL/api/agents/agent-a/push?confirm=true" \
  | jq -e '.ok == true' >/dev/null
echo "    OK: agent-a pushed"

echo "==> 6. agent-b 远程 pull → 配置真实流转"
curl -fsS -H "$AUTH" -X POST \
  "$HUB_URL/api/agents/agent-b/pull?confirm=true" \
  | jq -e '.ok == true' >/dev/null
echo "    OK: agent-b pulled"

echo "==> 7. 终态断言：agent-b 工具目录出现同一行"
$COMPOSE exec -T agent-b sh -c \
  'grep -q "hub-e2e" /root/toolhome/.pi/agent/settings.json'
echo "    OK: 配置跨容器流转成功"

echo ""
echo "✅ 容器化 E2E 全部通过"
