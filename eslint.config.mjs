import tseslint from "@typescript-eslint/eslint-plugin";
import tsparser from "@typescript-eslint/parser";

export default [
  {
    // Test files (*.test.ts) are excluded from tsconfig's
    // browser-typed project so they can pull in bun-types via
    // a triple-slash reference without contaminating production
    // code. Keeping them out of eslint here too avoids a
    // "file not in project" parser error while still running
    // the real linter on everything that ends up in the bundle.
    ignores: ["static/ts/**/*.test.ts"],
  },
  {
    files: ["static/ts/**/*.ts"],
    languageOptions: {
      parser: tsparser,
      parserOptions: {
        project: "./tsconfig.json",
      },
    },
    plugins: {
      "@typescript-eslint": tseslint,
    },
    rules: {
      ...tseslint.configs.recommended.rules,
      "@typescript-eslint/no-unused-vars": ["error", { argsIgnorePattern: "^_" }],
      "@typescript-eslint/explicit-function-return-type": "error",
      "@typescript-eslint/no-explicit-any": "warn",
      "no-console": "warn",
    },
  },
];
