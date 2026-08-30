import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const docs = path.join(repository, "docs");
const site = JSON.parse(fs.readFileSync(path.join(docs, "docs.json"), "utf8"));
const pages = new Map();
const failures = [];

function walk(directory) {
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    const absolute = path.join(directory, entry.name);
    if (entry.isDirectory()) walk(absolute);
    else if (entry.name.endsWith(".mdx")) {
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
      if (!["global", "root", "openapi"].includes(key)) collectNavigation(child, found);
    }
  }
  return found;
}

function headingSlug(heading) {
  return heading
    .toLowerCase()
    .replace(/`([^`]+)`/g, "$1")
    .replace(/\[([^\]]+)\]\([^)]*\)/g, "$1")
    .replace(/<[^>]+>/g, "")
    .replace(/[^\p{L}\p{N}\s-]/gu, "")
    .trim()
    .replace(/\s+/g, "-")
    .replace(/-+/g, "-");
}

function anchors(content) {
  const found = new Set();
  for (const match of content.matchAll(/^#{1,6}\s+(.+)$/gm)) found.add(headingSlug(match[1]));
  return found;
}

function checkInternalTarget(source, rawTarget, redirects) {
  if (!rawTarget.startsWith("/") && !rawTarget.startsWith("#")) return;
  const [rawPage, fragment] = rawTarget.split("#", 2);
  const target = rawPage.startsWith("/")
    ? rawPage.replace(/^\/+|\/+$/g, "")
    : source;
  const resolved = redirects.get(`/${target}`) ?? target;
  if (target && !pages.has(target) && !redirects.has(`/${target}`)) {
    failures.push(`${source} links to missing page ${rawTarget}`);
    return;
  }
  if (fragment && pages.has(resolved) && !anchors(pages.get(resolved)).has(fragment)) {
    failures.push(`${source} links to missing heading #${fragment} in ${resolved}`);
  }
}

function collectHrefs(node, found = []) {
  if (Array.isArray(node)) {
    for (const child of node) collectHrefs(child, found);
  } else if (node && typeof node === "object") {
    for (const [key, child] of Object.entries(node)) {
      if (key === "href" && typeof child === "string") found.push(child);
      else collectHrefs(child, found);
    }
  }
  return found;
}

function parseOpenAPIOperations(content) {
  const operations = new Map();
  let currentPath = "";
  let currentMethod = "";
  for (const line of content.split(/\r?\n/)) {
    const pathMatch = line.match(/^  (\/[^:]+):$/);
    if (pathMatch) {
      currentPath = pathMatch[1];
      currentMethod = "";
      continue;
    }
    const methodMatch = line.match(/^    (get|post|put|patch|delete):$/);
    if (currentPath && methodMatch) {
      currentMethod = methodMatch[1].toUpperCase();
      operations.set(`${currentMethod} ${currentPath}`, {
        path: currentPath,
        method: currentMethod,
        operationId: "",
        summary: "",
      });
      continue;
    }
    const operation = operations.get(`${currentMethod} ${currentPath}`);
    const operationIDMatch = line.match(/^      operationId: (.+)$/);
    if (operation && operationIDMatch) {
      operation.operationId = operationIDMatch[1].trim();
      continue;
    }
    const summaryMatch = line.match(/^      summary: (.+)$/);
    if (operation && summaryMatch) {
      operation.summary = summaryMatch[1].trim();
    }
  }
  return operations;
}

walk(docs);
const navigation = collectNavigation(site.navigation);
const redirects = new Map();
for (const redirect of site.redirects ?? []) {
  if (redirects.has(redirect.source)) failures.push(`duplicate redirect ${redirect.source}`);
  redirects.set(redirect.source, redirect.destination.replace(/^\/+|\/+$/g, ""));
}

if (pages.size !== 22) failures.push(`documentation has ${pages.size} authored pages, want 22`);
for (const page of navigation) {
  if (!pages.has(page)) failures.push(`navigation names missing page ${page}`);
}
for (const page of pages.keys()) {
  if (!navigation.has(page)) failures.push(`unreachable page ${page}`);
}
for (const [source, destination] of redirects) {
  if (!source.startsWith("/")) failures.push(`redirect source must be absolute: ${source}`);
  if (!pages.has(destination)) failures.push(`redirect ${source} names missing destination /${destination}`);
}

const credentialLiteral = /(?:ocwh_|xox[baprs]-|gh[pousr]_|sk-ant-)[A-Za-z0-9_-]{12,}/;
const markdownLink = /\]\(([^)\s]+)(?:\s+"[^"]*")?\)/g;
const mdxHref = /\bhref=["']([^"']+)["']/g;
for (const [page, content] of pages) {
  if (!/^---\r?\n[\s\S]*?^title:\s*.+$[\s\S]*?^description:\s*.+$[\s\S]*?^---\r?$/m.test(content)) {
    failures.push(`${page} lacks title/description frontmatter`);
  }
  for (const forbidden of ["AGENTS.md", "plans/", "operator surface", "webhook-work", "OperatorHandlers", "Environment record"]) {
    if (content.includes(forbidden)) failures.push(`${page} contains forbidden text ${forbidden}`);
  }
  if (credentialLiteral.test(content)) failures.push(`${page} contains a credential-shaped literal`);
  if (/!\[\]\(/.test(content)) failures.push(`${page} contains an image without alternative text`);
  if (content.includes("structured-investigation.png")) failures.push(`${page} publishes the stale structured-investigation image`);
  if (/X-OpenCluster-Organization:\s*local\b/.test(content)) failures.push(`${page} uses placeholder Organization local`);
  for (const match of content.matchAll(markdownLink)) checkInternalTarget(page, match[1], redirects);
  for (const match of content.matchAll(mdxHref)) checkInternalTarget(page, match[1], redirects);
}

for (const href of collectHrefs(site)) {
  if (href.startsWith("/")) checkInternalTarget("docs.json", href, redirects);
  else {
    try { new URL(href); } catch { failures.push(`docs.json contains invalid URL ${href}`); }
  }
}

const expectedNavbar = JSON.stringify({
  links: [{ type: "github", href: "https://github.com/open-cluster/oc-control-plane" }],
  primary: { type: "button", label: "Quickstart", href: "/getting-started/quickstart" },
});
if (JSON.stringify(site.navbar) !== expectedNavbar) failures.push("navbar must contain only GitHub and the Quickstart action");

const requiredSocials = {
  github: "https://github.com/open-cluster",
  discord: "https://discord.gg/gj7v8TJCu",
  x: "https://x.com/openclusterio",
};
for (const [name, href] of Object.entries(requiredSocials)) {
  if (site.footer?.socials?.[name] !== href) failures.push(`footer ${name} social must be ${href}`);
}
if (site.footer?.socials?.linkedin && site.footer.socials.linkedin !== "https://www.linkedin.com/company/openclusterio") {
  failures.push("footer LinkedIn social uses an unapproved URL");
}
if ((site.footer?.links ?? []).length !== 2 || site.footer.links[0]?.header !== "Resources" || site.footer.links[1]?.header !== "Community") {
  failures.push("footer must contain only Resources and Community groups");
}
if (site.feedback) failures.push("feedback controls duplicate the approved contextual actions");
const contextual = site.contextual?.options ?? [];
if (contextual.length !== 3 || contextual[0] !== "copy" || contextual[1] !== "view" || typeof contextual[2] !== "object" || contextual[2].title !== "Report a documentation issue") {
  failures.push("contextual actions must be Copy page, View as Markdown, and Report a documentation issue");
}
for (const forbidden of ["assistant", "chatgpt", "claude", "perplexity", "mcp", "add-mcp"]) {
  if (contextual.includes(forbidden)) failures.push(`contextual actions include forbidden AI action ${forbidden}`);
}

const integrationPages = [
  "integrations/alerting/generic_webhook",
  "integrations/alerting/alertmanager",
  "integrations/infrastructure/kubernetes",
  "integrations/source-control/github",
  "integrations/collaboration/slack",
];
const integrationSections = [
  ["prerequisites", /^## Prerequisites$/m],
  ["least-privilege permissions", /^## (?:Least-privilege access|Access|Scopes|Tools and required permissions|GitHub App permissions)$/m],
  ["setup", /^## (?:Connect|Setup)$/m],
  ["verification", /^## Verify$/m],
  ["evidence", /^## (?:During investigations|Evidence available|What OpenCluster uses it for)$/m],
  ["limitations", /^## Limitations$/m],
  ["troubleshooting", /^## Troubleshooting$/m],
  ["rotation/removal", /^## (?:Rotate|Replace|Disconnect)/m],
  ["next step", /^## Next step$/m],
];
for (const page of integrationPages) {
  const content = pages.get(page) ?? "";
  for (const [section, pattern] of integrationSections) {
    if (!pattern.test(content)) failures.push(`${page} lacks ${section} guidance`);
  }
}

const canonicalOpenAPI = fs.readFileSync(path.join(repository, "api", "openapi.yaml"), "utf8");
const publishedOpenAPIPath = path.join(docs, "api", "openapi.yaml");
if (!fs.existsSync(publishedOpenAPIPath)) failures.push("docs/api/openapi.yaml is missing");
if (!/^\s*- url: \/$/m.test(canonicalOpenAPI)) {
  failures.push("canonical OpenAPI must retain its deployment-neutral relative server");
}
const publishedOpenAPI = fs.existsSync(publishedOpenAPIPath)
  ? fs.readFileSync(publishedOpenAPIPath, "utf8")
  : "";
if (!/^\s*- url: https?:\/\//m.test(publishedOpenAPI)) {
  failures.push("published OpenAPI needs an absolute local server for generated request examples");
}
const operations = parseOpenAPIOperations(canonicalOpenAPI);
if (operations.size === 0) failures.push("canonical OpenAPI contains no operations");
const apiOverview = pages.get("api-reference/overview") ?? "";
if (
  canonicalOpenAPI.includes("required-for-unsafe-cookie-request") &&
  !canonicalOpenAPI.includes("name: Origin") &&
  !apiOverview.includes("Generated cURL examples currently omit this header")
) {
  failures.push("API overview must disclose the generated unsafe-request Origin limitation");
}
const apiGroup = site.navigation?.groups?.find((group) => group.group === "API reference");
const openapiNode = apiGroup?.pages?.find((page) => typeof page === "object" && page.openapi);
if (openapiNode?.group !== "Endpoints" || openapiNode?.openapi !== "api/openapi.yaml") {
  failures.push("API reference does not attach the generated Endpoints group to api/openapi.yaml");
}

const surface = pages.get("api-reference/surface") ?? "";
const surfaceCounts = new Map();
for (const match of surface.matchAll(/`(GET|POST|PUT|PATCH|DELETE) (\/(?:api|webhooks)\/[^`]+)`/g)) {
  const operation = `${match[1]} ${match[2]}`;
  surfaceCounts.set(operation, (surfaceCounts.get(operation) ?? 0) + 1);
}
for (const operation of operations.keys()) {
  if (surfaceCounts.get(operation) !== 1) failures.push(`API surface lists ${operation} ${surfaceCounts.get(operation) ?? 0} times; want once`);
}
for (const [name, operation] of operations) {
  if (!operation.operationId) failures.push(`${name} lacks an OpenAPI operationId`);
  if (!operation.summary) failures.push(`${name} lacks a short OpenAPI summary for its generated page`);
}
for (const operation of surfaceCounts.keys()) {
  if (!operations.has(operation)) failures.push(`API surface documents unknown operation ${operation}`);
}
for (const [page, content] of pages) {
  for (const match of content.matchAll(/`(GET|POST|PUT|PATCH|DELETE) (\/(?:api|webhooks)\/[^`]+)`/g)) {
    const operation = `${match[1]} ${match[2]}`;
    if (!operations.has(operation)) failures.push(`${page} documents route absent from OpenAPI: ${operation}`);
  }
}

const configSource = fs.readdirSync(path.join(repository, "internal", "config"))
  .filter((name) => name.endsWith(".go") && !name.endsWith("_test.go"))
  .map((name) => fs.readFileSync(path.join(repository, "internal", "config", name), "utf8"))
  .join("\n");
const supportedOCKeys = new Set(configSource.match(/"OC_[A-Z0-9_]+"/g)?.map((key) => key.slice(1, -1)) ?? []);
const configPage = pages.get("self-hosting/configuration") ?? "";
for (const key of configPage.match(/\bOC_[A-Z0-9_]+\b/g) ?? []) {
  if (!supportedOCKeys.has(key)) failures.push(`configuration documents unsupported environment key ${key}`);
}
const composeSource = fs.readFileSync(path.join(repository, "deploy", "compose", "compose.yaml"), "utf8");
const supportedComposeKeys = new Set(composeSource.match(/OPENCLUSTER_[A-Z0-9_]+/g) ?? []);
const quickstart = pages.get("getting-started/quickstart") ?? "";
for (const key of quickstart.match(/\bOPENCLUSTER_[A-Z0-9_]+\b/g) ?? []) {
  if (!supportedComposeKeys.has(key)) failures.push(`Quickstart documents unsupported Compose input ${key}`);
}
const yamlSource = fs.readFileSync(path.join(repository, "internal", "config", "file.go"), "utf8");
const supportedYAMLKeys = new Set(yamlSource.match(/yaml:"([a-z0-9_]+)"/g)?.map((tag) => tag.slice(6, -1)) ?? []);
for (const key of configPage.match(/\b[a-z][a-z0-9_]*_file\b/g) ?? []) {
  if (!supportedYAMLKeys.has(key)) failures.push(`configuration documents unsupported YAML key ${key}`);
}

if (failures.length) {
  for (const failure of failures) console.error(failure);
  process.exit(1);
}
console.log(`validated ${pages.size} documentation pages and ${operations.size} OpenAPI operations`);
