import { defineConfig } from "vitest/config";

// Unit tests run under jsdom so browser globals the app relies on — localStorage,
// CacheStorage, fetch — exist to be exercised or stubbed. We test pure logic and
// the API/error layer, not React rendering, so no component-testing stack is
// pulled in; that keeps the suite fast and the dependency surface small.
export default defineConfig({
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.ts"],
  },
});
