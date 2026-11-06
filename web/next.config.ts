import type { NextConfig } from "next";

const config: NextConfig = {
  // The image ships this output and no node_modules.
  output: "standalone",
  // Strict mode double-mounts effects in dev, and a double-mounted WebGL context is
  // a slow way to find out how many of those a tab gets.
  reactStrictMode: false,
};

export default config;
