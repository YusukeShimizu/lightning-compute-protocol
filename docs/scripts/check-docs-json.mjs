import fs from 'node:fs';
import path from 'node:path';

const DOCS_JSON_PATH = 'docs.json';

function isNonEmptyString(value) {
  return typeof value === 'string' && value.trim().length > 0;
}

function normalizePageSlug(slug) {
  const trimmed = slug.trim().replace(/^\/+/, '');
  return trimmed.replace(/\.mdx?$/i, '');
}

function collectPageSlugsFromNavigation(node, out) {
  if (node == null) return;

  if (Array.isArray(node)) {
    for (const item of node) collectPageSlugsFromNavigation(item, out);
    return;
  }

  if (isNonEmptyString(node)) {
    out.add(normalizePageSlug(node));
    return;
  }

  if (typeof node !== 'object') return;

  // Only traverse properties that can contain pages or nested navigation nodes.
  // This avoids mistakenly collecting labels like `group`, `label`, etc.
  if ('pages' in node) collectPageSlugsFromNavigation(node.pages, out);
  if ('root' in node) collectPageSlugsFromNavigation(node.root, out);
  if ('groups' in node) collectPageSlugsFromNavigation(node.groups, out);
  if ('anchors' in node) collectPageSlugsFromNavigation(node.anchors, out);
  if ('dropdowns' in node) collectPageSlugsFromNavigation(node.dropdowns, out);
  if ('tabs' in node) collectPageSlugsFromNavigation(node.tabs, out);
  if ('versions' in node) collectPageSlugsFromNavigation(node.versions, out);
  if ('languages' in node) collectPageSlugsFromNavigation(node.languages, out);
  if ('products' in node) collectPageSlugsFromNavigation(node.products, out);
}

function resolveExistingPageFile(slug) {
  const candidates = [`${slug}.mdx`, `${slug}.md`];
  for (const rel of candidates) {
    if (fs.existsSync(rel) && fs.statSync(rel).isFile()) return rel;
  }
  return null;
}

function main() {
  const repoRoot = process.cwd();
  const docsJsonAbsPath = path.join(repoRoot, DOCS_JSON_PATH);

  if (!fs.existsSync(docsJsonAbsPath)) {
    console.error(`ERROR: Missing ${DOCS_JSON_PATH} at repo root.`);
    process.exitCode = 1;
    return;
  }

  let config;
  try {
    config = JSON.parse(fs.readFileSync(docsJsonAbsPath, 'utf8'));
  } catch (err) {
    console.error(`ERROR: Failed to parse ${DOCS_JSON_PATH} as JSON.`);
    console.error(err);
    process.exitCode = 1;
    return;
  }

  if (!config?.navigation) {
    console.error(`ERROR: ${DOCS_JSON_PATH} is missing required "navigation".`);
    process.exitCode = 1;
    return;
  }

  const slugs = new Set();
  collectPageSlugsFromNavigation(config.navigation, slugs);

  const missing = [];
  for (const slug of [...slugs].sort()) {
    const file = resolveExistingPageFile(slug);
    if (!file) missing.push(slug);
  }

  if (missing.length > 0) {
    console.error('ERROR: docs.json references pages that do not exist:');
    for (const slug of missing) console.error(`- ${slug} (.mdx/.md not found)`);
    process.exitCode = 1;
    return;
  }

  console.log(`OK: ${slugs.size} navigation page(s) exist on disk.`);
}

main();

