import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    // P0 scaffold 阶段还没有测试文件；P1 各 worker 会补齐 tests/**
    passWithNoTests: true,
    include: ['tests/**/*.test.ts'],
    environment: 'node',
  },
});
