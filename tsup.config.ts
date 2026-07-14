import { defineConfig } from 'tsup';

export default defineConfig({
  entry: ['src/index.ts'],
  format: ['esm'],
  target: 'node22',
  clean: true,
  dts: false,
  sourcemap: false,
  minify: true,
  bundle: true,
});
