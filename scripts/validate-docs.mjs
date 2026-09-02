import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const docs = path.join(repository, "docs");
const failures = [];
const pages = [];

function walk(directory) {
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    const absolute = path.join(directory, entry.name);
    if (entry.isDirectory()) walk(absolute);
    else if (entry.name.endsWith(".mdx")) pages.push(absolute);
  }
}

walk(docs);
const pageText = pages.map((page) => fs.readFileSync(page, "utf8"));
const allDocs = pageText.join("\n");
const credentialLiteral = /(?:ocwh_|xox[baprs]-|gh[pousr]_|sk-ant-)[A-Za-z0-9_-]{12,}/;
for (let index = 0; index < pages.length; index++) {
  if (credentialLiteral.test(pageText[index])) {
    failures.push(`${path.relative(docs, pages[index])} contains a credential-shaped literal`);
  }
}

const canonicalOpenAPI = fs.readFileSync(path.join(repository, "api", "openapi.yaml"), "utf8");
const publishedOpenAPI = fs.readFileSync(path.join(docs, "api", "openapi.yaml"), "utf8");
if (!/^\s*- url: \/$/m.test(canonicalOpenAPI)) failures.push("canonical OpenAPI must use a relative server");
if (!/^\s*- url: https?:\/\//m.test(publishedOpenAPI)) failures.push("published OpenAPI must use an absolute local server");

for (const match of allDocs.matchAll(/`(GET|POST|PUT|PATCH|DELETE) (\/(?:api|webhooks)\/[^`]+)`/g)) {
  const method = match[1].toLowerCase();
  const route = match[2];
  if (!canonicalOpenAPI.includes(`  ${route}:`) || !canonicalOpenAPI.includes(`    ${method}:`)) {
    failures.push(`documentation names an HTTP operation absent from OpenAPI: ${match[1]} ${route}`);
  }
}

const configSource = fs.readdirSync(path.join(repository, "internal", "config"))
  .filter((name) => name.endsWith(".go") && !name.endsWith("_test.go"))
  .map((name) => fs.readFileSync(path.join(repository, "internal", "config", name), "utf8"))
  .join("\n");
const supportedEnvironment = new Set(configSource.match(/"OC_[A-Z0-9_]+"/g)?.map((key) => key.slice(1, -1)) ?? []);
for (const key of allDocs.match(/\bOC_[A-Z0-9_]+\b/g) ?? []) {
  if (!supportedEnvironment.has(key)) failures.push(`documentation names unsupported environment key ${key}`);
}

if (failures.length) {
  failures.forEach((failure) => console.error(failure));
  process.exit(1);
}
console.log(`validated ${pages.length} documentation pages against configuration and OpenAPI`);
