// Development-only Node loader for the existing locked TypeScript compiler.
// No loader or test fixture is included in the browser build.
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import ts from "typescript";

export async function resolve(specifier, context, nextResolve) {
  try {
    return await nextResolve(specifier, context);
  } catch (error) {
    if (!specifier.startsWith(".") || /\.[a-z0-9]+$/i.test(specifier)) throw error;
    for (const suffix of [".ts", ".tsx", ".js"]) {
      try {
        return await nextResolve(specifier + suffix, context);
      } catch {
        // Try only the fixed source extensions; preserve the original error.
      }
    }
    throw error;
  }
}

export async function load(url, context, nextLoad) {
  if (!/\.tsx?$/.test(url)) return nextLoad(url, context);
  const source = await readFile(fileURLToPath(url), "utf8");
  const result = ts.transpileModule(source, {
    fileName: fileURLToPath(url),
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
      jsx: ts.JsxEmit.ReactJSX,
      sourceMap: false,
      verbatimModuleSyntax: true,
    },
  });
  return { format: "module", source: result.outputText, shortCircuit: true };
}
