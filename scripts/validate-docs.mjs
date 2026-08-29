import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const docs = path.join(repository, "docs");
const site = JSON.parse(fs.readFileSync(path.join(docs, "docs.json"), "utf8"));
const pages = new Map();

function walk(directory) {
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    const absolute = path.join(directory, entry.name);
    if (entry.isDirectory()) {
      walk(absolute);
    } else if (entry.name.endsWith(".mdx")) {
      const name = path.relative(docs, absolute).replaceAll(path.sep, "/").slice(0, -4);
      pages.set(name, fs.readFileSync(absolute, "utf8"));
    }
  }
}

function collectNavigation(node, found = new Set()) {
  if (Array.isArray(node)) {
    for (const child of node) {
      if (typeof child === "string") found.add(child);
      else collectNavigation(child, found);
    }
  } else if (node && typeof node === "object") {
    if (typeof node.root === "string") found.add(node.root);
    for (const [key, child] of Object.entries(node)) {
      if (key !== "global" && key !== "root") collectNavigation(child, found);
    }
  }
  return found;
}

walk(docs);
const navigation = collectNavigation(site.navigation);
const redirects = new Set((site.redirects ?? []).map((redirect) => redirect.source));
const hiddenPages = new Set(["feature-availability"]);
const failures = [];
const internalLink = /\]\((\/[^)#?]*)(?:[?#][^)]*)?\)/g;
const credentialLiteral = /(?:ocwh_|xox[baprs]-|gh[pousr]_|sk-ant-)[A-Za-z0-9_-]{12,}/;
const integrationPages = [
  "integrations/alerting/generic_webhook",
  "integrations/alerting/alertmanager",
  "integrations/infrastructure/kubernetes",
  "integrations/source-control/github",
  "integrations/collaboration/slack",
];

for (const page of navigation) {
  if (!pages.has(page)) failures.push(`navigation names missing page ${page}`);
}
for (const [page, content] of pages) {
  if (!navigation.has(page) && !hiddenPages.has(page)) failures.push(`unreachable page ${page}`);
  if (!/^---\r?\n[\s\S]*?^title:\s*.+$[\s\S]*?^description:\s*.+$[\s\S]*?^---\r?$/m.test(content)) {
    failures.push(`${page} lacks title/description frontmatter`);
  }
  for (const forbidden of ["AGENTS.md", "plans/", "operator surface", "webhook-work", "OperatorHandlers", "Environment record"]) {
    if (content.includes(forbidden)) failures.push(`${page} contains forbidden text ${forbidden}`);
  }
  if (credentialLiteral.test(content)) failures.push(`${page} contains a credential-shaped literal`);
  if (/!\[\]\(/.test(content)) failures.push(`${page} contains an image without alternative text`);
  for (const match of content.matchAll(internalLink)) {
    const target = match[1].replace(/^\/+|\/+$/g, "");
    if (target && !pages.has(target) && !redirects.has(`/${target}`)) {
      failures.push(`${page} links to missing page ${match[1]}`);
    }
  }
}

for (const page of integrationPages) {
  const content = pages.get(page) ?? "";
  for (const marker of ["## Availability", "adapter source", "releases", "changelog", "Edit this page", "report a documentation issue"]) {
    if (!content.includes(marker)) failures.push(`${page} lacks integration contract marker ${marker}`);
  }
}

if (failures.length) {
  for (const failure of failures) console.error(failure);
  process.exit(1);
}
console.log(`validated ${pages.size} documentation pages`);
